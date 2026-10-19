package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	APIKey          string // 调用方必须携带的密钥
	Listen          string // 服务监听地址
	Image           string // 沙箱运行时镜像
	DataRoot        string // 宿主机数据根目录，见 Manager.workspaceDir
	MountTarget     string // 当前对话工作目录在容器内的挂载点
	UserMountTarget string // scope 长期空间在容器内的挂载点
	Runtime         string // 容器运行时，留空使用默认 runc，可设为 runsc(gVisor)
	Network         string // none：断网（默认）；proxy：只能通过白名单代理访问外网；其他值作为 Docker 网络名直接使用

	EgressAllowlist   []string        // proxy 模式下允许访问的域名，支持 *.example.com
	EgressPorts       map[string]bool // proxy 模式下允许访问的端口
	EgressPrivateNets []*net.IPNet    // proxy 模式下额外允许访问的内网地址段（如内部镜像源）
	ProxyListen       string          // 白名单代理监听地址
	ProxyNetwork      string          // 沙箱容器接入的 internal 网络
	ProxyURL          string          // 沙箱容器内访问代理的地址
	PipIndexURL       string          // 注入容器的 PIP_INDEX_URL，可指向国内镜像
	NpmRegistry       string          // 注入容器的 npm registry

	MemoryMB  int64
	CPUs      float64
	PidsLimit int64
	TmpMB     int
	UID       int
	GID       int

	IdleTTL        time.Duration // 用户容器空闲多久后回收
	MaxSessions    int           // 同时存在的沙箱容器上限
	DefaultTimeout int           // 单次执行默认超时（秒）
	MaxTimeout     int           // 单次执行最大超时（秒）
	MaxOutputBytes int           // stdout / stderr 各自的最大返回字节数
	MaxUploadBytes int64         // 单个上传文件的最大字节数
}

func loadConfig() (*Config, error) {
	var errs []error
	cfg := &Config{
		APIKey:          os.Getenv("SANDBOX_API_KEY"),
		Listen:          envStr("SANDBOX_LISTEN", ":8090"),
		Image:           envStr("SANDBOX_IMAGE", "agent-sandbox-runtime:latest"),
		DataRoot:        envStr("SANDBOX_DATA_ROOT", "/opt"),
		MountTarget:     envStr("SANDBOX_MOUNT_TARGET", "/opt/workspace"),
		UserMountTarget: envStr("SANDBOX_USER_MOUNT_TARGET", "/opt/user"),
		Runtime:         os.Getenv("SANDBOX_RUNTIME"),
		Network:         envStr("SANDBOX_NETWORK", "none"),
		MemoryMB:        int64(envInt("SANDBOX_MEMORY_MB", 1024, &errs)),
		CPUs:            envFloat("SANDBOX_CPUS", 1.0, &errs),
		PidsLimit:       int64(envInt("SANDBOX_PIDS_LIMIT", 256, &errs)),
		TmpMB:           envInt("SANDBOX_TMP_MB", 512, &errs),
		UID:             envInt("SANDBOX_UID", 1000, &errs),
		GID:             envInt("SANDBOX_GID", 1000, &errs),
		IdleTTL:         envDuration("SANDBOX_IDLE_TTL", 10*time.Minute, &errs),
		MaxSessions:     envInt("SANDBOX_MAX_SESSIONS", 100, &errs),
		DefaultTimeout:  envInt("SANDBOX_DEFAULT_TIMEOUT", 60, &errs),
		MaxTimeout:      envInt("SANDBOX_MAX_TIMEOUT", 600, &errs),
		MaxOutputBytes:  envInt("SANDBOX_MAX_OUTPUT_KB", 1024, &errs) * 1024,
		MaxUploadBytes:  int64(envInt("SANDBOX_MAX_UPLOAD_MB", 2048, &errs)) << 20,

		EgressAllowlist: splitList(envStr("SANDBOX_EGRESS_ALLOWLIST", "pypi.org,files.pythonhosted.org,registry.npmjs.org")),
		EgressPorts:     map[string]bool{},
		ProxyListen:     envStr("SANDBOX_PROXY_LISTEN", ":3128"),
		ProxyNetwork:    envStr("SANDBOX_PROXY_NETWORK", "agent-sandbox-egress"),
		ProxyURL:        envStr("SANDBOX_PROXY_URL", "http://sandbox-egress-proxy:3128"),
		PipIndexURL:     os.Getenv("SANDBOX_PIP_INDEX_URL"),
		NpmRegistry:     os.Getenv("SANDBOX_NPM_REGISTRY"),
	}
	for _, p := range splitList(envStr("SANDBOX_EGRESS_PORTS", "80,443")) {
		cfg.EgressPorts[p] = true
	}
	for _, c := range splitList(os.Getenv("SANDBOX_EGRESS_PRIVATE_CIDRS")) {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			errs = append(errs, fmt.Errorf("SANDBOX_EGRESS_PRIVATE_CIDRS: %w", err))
			continue
		}
		cfg.EgressPrivateNets = append(cfg.EgressPrivateNets, n)
	}
	if len(cfg.APIKey) < 16 {
		errs = append(errs, errors.New("SANDBOX_API_KEY is required and must be at least 16 characters"))
	}
	if cfg.DefaultTimeout <= 0 || cfg.MaxTimeout < cfg.DefaultTimeout {
		errs = append(errs, errors.New("SANDBOX_DEFAULT_TIMEOUT must be > 0 and <= SANDBOX_MAX_TIMEOUT"))
	}
	return cfg, errors.Join(errs...)
}

// splitList 解析逗号分隔的配置，去掉空白并统一小写
func splitList(s string) []string {
	var out []string
	for _, item := range strings.Split(s, ",") {
		if item = strings.ToLower(strings.TrimSpace(item)); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int, errs *[]error) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return n
}

func envFloat(key string, def float64, errs *[]error) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return f
}

func envDuration(key string, def time.Duration, errs *[]error) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return d
}
