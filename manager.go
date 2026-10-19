package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

const (
	labelManaged = "agent.sandbox.managed"
	labelScope   = "agent.sandbox.scope"
	labelSession = "agent.sandbox.session"
)

var (
	ErrTooManySandboxes = errors.New("too many active sandbox containers")
	ErrSandboxBusy      = errors.New("sandbox is busy")
)

// 每种语言的解释器，代码统一通过 stdin 传入，避免命令行参数长度限制和注入问题
var interpreters = map[string][]string{
	"python": {"python3", "-"},
	"node":   {"node", "-"},
	"bash":   {"bash", "-s"},
}

type ExecRequest struct {
	Scope    string            `json:"scope"`
	Session  string            `json:"session"`
	Language string            `json:"language"`
	Code     string            `json:"code"`
	Timeout  int               `json:"timeout"`
	Env      map[string]string `json:"env"`
}

type ExecResult struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	TimedOut   bool   `json:"timed_out"`
	Truncated  bool   `json:"truncated"`
	DurationMs int64  `json:"duration_ms"`
}

// sandboxKey 标识一个容器：scope 是数据归属（如用户），session 是其中一次对话，可以为空
type sandboxKey struct {
	Scope   string
	Session string
}

// sandbox 对应一个常驻容器
type sandbox struct {
	mu          sync.Mutex // 保护 containerID
	containerID string
	inflight    int       // 由 Manager.mu 保护
	lastUsed    time.Time // 由 Manager.mu 保护
}

type Manager struct {
	cfg       *Config
	cli       *client.Client
	mu        sync.Mutex
	sandboxes map[sandboxKey]*sandbox

	egressNets []*net.IPNet // proxy 模式下沙箱所在 internal 网络的网段
}

func NewManager(cfg *Config) (*Manager, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Manager{cfg: cfg, cli: cli, sandboxes: map[sandboxKey]*sandbox{}}, nil
}

// Init 检查镜像是否存在，并清理上次进程遗留的沙箱容器
// EnsureImage 确保运行时镜像在本地，不存在时自动拉取（镜像较大，可能需要几分钟）
func (m *Manager) EnsureImage(ctx context.Context) error {
	if _, err := m.cli.ImageInspect(ctx, m.cfg.Image); err == nil {
		return nil
	}
	slog.Info("sandbox image not found locally, pulling", "image", m.cfg.Image)
	rc, err := m.cli.ImagePull(ctx, m.cfg.Image, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull sandbox image %q: %w", m.cfg.Image, err)
	}
	defer rc.Close()
	// 拉取进度以 JSON 流返回，必须读完才算拉取结束
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return fmt.Errorf("pull sandbox image %q: %w", m.cfg.Image, err)
	}
	if _, err := m.cli.ImageInspect(ctx, m.cfg.Image); err != nil {
		return fmt.Errorf("sandbox image %q not available after pull: %w", m.cfg.Image, err)
	}
	slog.Info("sandbox image pulled", "image", m.cfg.Image)
	return nil
}

func (m *Manager) Init(ctx context.Context) error {
	if m.cfg.Network == "proxy" {
		if err := m.initEgressNetwork(ctx); err != nil {
			return err
		}
	}
	list, err := m.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", labelManaged+"=true")),
	})
	if err != nil {
		return err
	}
	for _, c := range list {
		m.removeContainer(c.ID)
	}
	return nil
}

// initEgressNetwork 校验沙箱网络存在且是 internal 的（没有直接出网的路由），并记录网段
func (m *Manager) initEgressNetwork(ctx context.Context) error {
	nw, err := m.cli.NetworkInspect(ctx, m.cfg.ProxyNetwork, network.InspectOptions{})
	if err != nil {
		return fmt.Errorf("egress network %q not available, create it as an internal network: %w", m.cfg.ProxyNetwork, err)
	}
	if !nw.Internal {
		return fmt.Errorf("egress network %q must be internal, otherwise sandboxes can bypass the proxy", m.cfg.ProxyNetwork)
	}
	for _, c := range nw.IPAM.Config {
		if _, n, err := net.ParseCIDR(c.Subnet); err == nil {
			m.egressNets = append(m.egressNets, n)
		}
	}
	return nil
}

// containerNetwork 返回容器的网络模式和需要注入的环境变量
func (m *Manager) containerNetwork() (string, []string) {
	var env []string
	if m.cfg.PipIndexURL != "" {
		env = append(env, "PIP_INDEX_URL="+m.cfg.PipIndexURL)
	}
	if m.cfg.NpmRegistry != "" {
		env = append(env, "NPM_CONFIG_REGISTRY="+m.cfg.NpmRegistry)
	}
	if m.cfg.Network != "proxy" {
		return m.cfg.Network, env
	}
	u := m.cfg.ProxyURL
	env = append(env,
		"HTTP_PROXY="+u, "HTTPS_PROXY="+u, "http_proxy="+u, "https_proxy="+u,
		"NO_PROXY=localhost,127.0.0.1", "no_proxy=localhost,127.0.0.1",
		"NODE_USE_ENV_PROXY=1", // 让 Node 内置 fetch 也走代理
	)
	return m.cfg.ProxyNetwork, env
}

func (m *Manager) Exec(ctx context.Context, req *ExecRequest) (*ExecResult, error) {
	key := sandboxKey{Scope: req.Scope, Session: req.Session}
	s, err := m.acquire(key)
	if err != nil {
		return nil, err
	}
	defer m.release(s)

	containerID, err := m.ensureContainer(ctx, key, s)
	if err != nil {
		return nil, fmt.Errorf("prepare sandbox: %w", err)
	}

	// timeout 会杀掉整个进程组，ffmpeg 之类的子进程也会一起结束
	cmd := append([]string{"timeout", "-s", "KILL", strconv.Itoa(req.Timeout)}, interpreters[req.Language]...)
	env := make([]string, 0, len(req.Env))
	for k, v := range req.Env {
		env = append(env, k+"="+v)
	}
	created, err := m.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		User:         fmt.Sprintf("%d:%d", m.cfg.UID, m.cfg.GID),
		WorkingDir:   m.cfg.MountTarget,
		Env:          env,
		Cmd:          cmd,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("create exec: %w", err)
	}

	start := time.Now()
	hj, err := m.cli.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("attach exec: %w", err)
	}
	defer hj.Close()
	// 兜底：容器内 timeout 失效时，读取也会在超时后中断
	_ = hj.Conn.SetReadDeadline(start.Add(time.Duration(req.Timeout)*time.Second + 10*time.Second))
	go func() {
		_, _ = io.Copy(hj.Conn, strings.NewReader(req.Code))
		_ = hj.CloseWrite()
	}()

	stdout := &cappedBuffer{limit: m.cfg.MaxOutputBytes}
	stderr := &cappedBuffer{limit: m.cfg.MaxOutputBytes}
	_, copyErr := stdcopy.StdCopy(stdout, stderr, hj.Reader)
	elapsed := time.Since(start)
	res := &ExecResult{
		Stdout:     stdout.buf.String(),
		Stderr:     stderr.buf.String(),
		Truncated:  stdout.truncated || stderr.truncated,
		DurationMs: elapsed.Milliseconds(),
	}
	if copyErr != nil {
		var ne net.Error
		if errors.As(copyErr, &ne) && ne.Timeout() {
			slog.Warn("exec did not finish after timeout, resetting sandbox", "scope", key.Scope, "session", key.Session)
			m.resetContainer(s, containerID)
			res.ExitCode = -1
			res.TimedOut = true
			return res, nil
		}
		return nil, fmt.Errorf("read exec output: %w", copyErr)
	}

	for i := 0; i < 20; i++ {
		ins, err := m.cli.ContainerExecInspect(ctx, created.ID)
		if err != nil {
			return nil, fmt.Errorf("inspect exec: %w", err)
		}
		if !ins.Running {
			res.ExitCode = ins.ExitCode
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	res.TimedOut = res.ExitCode == 137 && elapsed >= time.Duration(req.Timeout)*time.Second
	return res, nil
}

// Remove 销毁 scope 下的容器：session 非空时只销毁这个对话的，为空时销毁整个 scope 的。
// purge 为 true 时同时删除对应的数据目录
func (m *Manager) Remove(scope, session string, purge bool) error {
	match := func(k sandboxKey) bool { return k.Scope == scope && (session == "" || k.Session == session) }
	var ids []string
	m.mu.Lock()
	for k, s := range m.sandboxes {
		if match(k) && s.inflight > 0 {
			m.mu.Unlock()
			return ErrSandboxBusy
		}
	}
	for k, s := range m.sandboxes {
		if match(k) {
			delete(m.sandboxes, k)
			if s.containerID != "" {
				ids = append(ids, s.containerID)
			}
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.removeContainer(id)
	}
	if !purge {
		return nil
	}
	dir := filepath.Join(m.cfg.DataRoot, scope)
	if session != "" {
		dir = filepath.Join(dir, "sessions", session)
	}
	// RemoveAll 不跟随符号链接，只删除链接本身
	return os.RemoveAll(dir)
}

// RunReaper 定期回收空闲容器
func (m *Manager) RunReaper(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.reapIdle()
		}
	}
}

func (m *Manager) reapIdle() {
	var ids []string
	m.mu.Lock()
	for k, s := range m.sandboxes {
		if s.inflight == 0 && time.Since(s.lastUsed) > m.cfg.IdleTTL {
			delete(m.sandboxes, k)
			if s.containerID != "" {
				ids = append(ids, s.containerID)
			}
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.removeContainer(id)
	}
}

func (m *Manager) Close() {
	var ids []string
	m.mu.Lock()
	for _, s := range m.sandboxes {
		if s.containerID != "" {
			ids = append(ids, s.containerID)
		}
	}
	m.sandboxes = map[sandboxKey]*sandbox{}
	m.mu.Unlock()
	for _, id := range ids {
		m.removeContainer(id)
	}
	_ = m.cli.Close()
}

func (m *Manager) acquire(key sandboxKey) (*sandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sandboxes[key]
	if !ok {
		if len(m.sandboxes) >= m.cfg.MaxSessions {
			return nil, ErrTooManySandboxes
		}
		s = &sandbox{}
		m.sandboxes[key] = s
	}
	s.inflight++
	s.lastUsed = time.Now()
	return s, nil
}

func (m *Manager) release(s *sandbox) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.inflight--
	s.lastUsed = time.Now()
}

func (m *Manager) ensureContainer(ctx context.Context, key sandboxKey, s *sandbox) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.containerID != "" {
		info, err := m.cli.ContainerInspect(ctx, s.containerID)
		if err == nil && info.State != nil && info.State.Running {
			return s.containerID, nil
		}
		slog.Warn("sandbox container not running, recreating", "scope", key.Scope, "session", key.Session,
			"container_id", s.containerID)
		m.removeContainer(s.containerID)
		s.containerID = ""
	}
	id, err := m.createContainer(ctx, key)
	if err != nil {
		return "", err
	}
	s.containerID = id
	return id, nil
}

func (m *Manager) resetContainer(s *sandbox, containerID string) {
	s.mu.Lock()
	if s.containerID == containerID {
		s.containerID = ""
	}
	s.mu.Unlock()
	m.removeContainer(containerID)
}

// workspaceDir 创建并返回宿主机上的工作目录，scope 和 session 已在入口处校验过字符集：
//
//	<DataRoot>/<scope>/workspace            scope 的长期空间（session 为空时）
//	<DataRoot>/<scope>/sessions/<session>   某次对话私有的工作目录
func (m *Manager) workspaceDir(scope, session string) (string, error) {
	parts := []string{m.cfg.DataRoot, scope, "workspace"}
	if session != "" {
		parts = []string{m.cfg.DataRoot, scope, "sessions", session}
	}
	dir := filepath.Join(parts...)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// 数据根目录以下的每一级都必须是真实目录，不能是符号链接
	for i := 2; i <= len(parts); i++ {
		p := filepath.Join(parts[:i]...)
		fi, err := os.Lstat(p)
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return "", fmt.Errorf("%s is not a regular directory", p)
		}
	}
	if err := os.Chown(dir, m.cfg.UID, m.cfg.GID); err != nil {
		slog.Warn("chown workspace failed", "path", dir, "error", err)
	}
	return dir, nil
}

func (m *Manager) createContainer(ctx context.Context, key sandboxKey) (string, error) {
	// 对话目录挂到 /opt/workspace，scope 的长期空间挂到 /opt/user；没有 session 时两者是同一个目录
	userDir, err := m.workspaceDir(key.Scope, "")
	if err != nil {
		return "", err
	}
	workDir := userDir
	if key.Session != "" {
		if workDir, err = m.workspaceDir(key.Scope, key.Session); err != nil {
			return "", err
		}
	}
	initProc := true
	pids := m.cfg.PidsLimit
	memory := m.cfg.MemoryMB << 20
	networkMode, env := m.containerNetwork()
	resp, err := m.cli.ContainerCreate(ctx,
		&container.Config{
			Image:      m.cfg.Image,
			Cmd:        []string{"sleep", "infinity"},
			User:       fmt.Sprintf("%d:%d", m.cfg.UID, m.cfg.GID),
			WorkingDir: m.cfg.MountTarget,
			Env:        env,
			Labels:     map[string]string{labelManaged: "true", labelScope: key.Scope, labelSession: key.Session},
		},
		&container.HostConfig{
			Init:           &initProc,
			Runtime:        m.cfg.Runtime,
			NetworkMode:    container.NetworkMode(networkMode),
			ReadonlyRootfs: true,
			Tmpfs: map[string]string{
				"/tmp": fmt.Sprintf("rw,exec,nosuid,nodev,size=%dm,mode=1777", m.cfg.TmpMB),
			},
			Mounts: []mount.Mount{
				{Type: mount.TypeBind, Source: workDir, Target: m.cfg.MountTarget},
				{Type: mount.TypeBind, Source: userDir, Target: m.cfg.UserMountTarget},
			},
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
			Resources: container.Resources{
				Memory:     memory,
				MemorySwap: memory, // 禁用 swap
				NanoCPUs:   int64(m.cfg.CPUs * 1e9),
				PidsLimit:  &pids,
			},
		},
		nil, nil, "")
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	if err := m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		m.removeContainer(resp.ID)
		return "", fmt.Errorf("start container: %w", err)
	}
	slog.Info("sandbox container started", "scope", key.Scope, "session", key.Session, "container_id", resp.ID[:12])
	return resp.ID, nil
}

func (m *Manager) removeContainer(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
		slog.Warn("remove container failed", "container_id", id, "error", err)
	}
}

// cappedBuffer 只保留前 limit 字节，超出部分丢弃并标记 truncated
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	remain := b.limit - b.buf.Len()
	switch {
	case remain <= 0:
		b.truncated = b.truncated || len(p) > 0
	case len(p) > remain:
		b.buf.Write(p[:remain])
		b.truncated = true
	default:
		b.buf.Write(p)
	}
	return len(p), nil
}
