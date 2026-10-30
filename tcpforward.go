package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 数据库等非 HTTP 协议不会走 HTTP 代理，proxy 模式下沙箱连不出去。
// TCP 转发由管理员显式配置「监听端口 -> 目标地址」，沙箱连 <代理主机>:<监听端口>，
// 服务端把字节流原样转发到目标。沙箱不能自己指定目标，所以不受白名单和内网地址限制。

// tcpForward 是一条转发规则
type tcpForward struct {
	ListenPort string // 服务端在沙箱网络上监听的端口
	Target     string // 转发目标 host:port
}

func (f tcpForward) String() string { return f.ListenPort + "=" + f.Target }

// parseTCPForwards 解析 SANDBOX_TCP_FORWARDS，格式：监听端口=目标主机:端口，多条用逗号分隔
func parseTCPForwards(s string, reserved ...string) ([]tcpForward, error) {
	var out []tcpForward
	var errs []error
	seen := map[string]bool{}
	for _, r := range reserved {
		if _, p, err := net.SplitHostPort(r); err == nil {
			seen[p] = true
		}
	}
	for _, item := range splitList(s) {
		port, target, ok := strings.Cut(item, "=")
		port, target = strings.TrimSpace(port), strings.TrimSpace(target)
		if !ok || !validPort(port) {
			errs = append(errs, fmt.Errorf("SANDBOX_TCP_FORWARDS: %q: want <listen port>=<host>:<port>", item))
			continue
		}
		if host, tport, err := net.SplitHostPort(target); err != nil || host == "" || !validPort(tport) {
			errs = append(errs, fmt.Errorf("SANDBOX_TCP_FORWARDS: %q: invalid target %q", item, target))
			continue
		}
		if seen[port] {
			errs = append(errs, fmt.Errorf("SANDBOX_TCP_FORWARDS: listen port %s is already in use", port))
			continue
		}
		seen[port] = true
		out = append(out, tcpForward{ListenPort: port, Target: target})
	}
	return out, errors.Join(errs...)
}

func validPort(p string) bool {
	n, err := strconv.Atoi(p)
	return err == nil && n > 0 && n < 65536
}

// tcpForwarder 监听所有转发端口，只接受来自沙箱网络的连接
type tcpForwarder struct {
	forwards  []tcpForward
	allowFrom []*net.IPNet // 为空时不限制来源
	listeners []net.Listener
	wg        sync.WaitGroup
}

func newTCPForwarder(forwards []tcpForward, allowFrom []*net.IPNet) *tcpForwarder {
	return &tcpForwarder{forwards: forwards, allowFrom: allowFrom}
}

// Start 打开所有监听端口，任何一个失败都会关闭已打开的端口并返回错误
func (t *tcpForwarder) Start() error {
	for _, f := range t.forwards {
		ln, err := net.Listen("tcp", ":"+f.ListenPort)
		if err != nil {
			t.Close()
			return fmt.Errorf("tcp forward %s: %w", f, err)
		}
		t.listeners = append(t.listeners, ln)
		t.wg.Add(1)
		go t.serve(ln, f)
	}
	return nil
}

func (t *tcpForwarder) Close() {
	for _, ln := range t.listeners {
		_ = ln.Close()
	}
	t.wg.Wait()
}

func (t *tcpForwarder) serve(ln net.Listener, f tcpForward) {
	defer t.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				slog.Error("tcp forward accept failed", "forward", f.String(), "error", err)
			}
			return
		}
		if !t.allowed(conn.RemoteAddr()) {
			slog.Warn("tcp forward denied", "client", conn.RemoteAddr().String(), "forward", f.String())
			_ = conn.Close()
			continue
		}
		go t.handle(conn, f)
	}
}

func (t *tcpForwarder) allowed(addr net.Addr) bool {
	if len(t.allowFrom) == 0 {
		return true
	}
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return false
	}
	for _, n := range t.allowFrom {
		if n.Contains(tcp.IP) {
			return true
		}
	}
	return false
}

func (t *tcpForwarder) handle(client net.Conn, f tcpForward) {
	defer client.Close()
	upstream, err := net.DialTimeout("tcp", f.Target, 10*time.Second)
	if err != nil {
		slog.Warn("tcp forward dial failed", "client", client.RemoteAddr().String(), "target", f.Target, "error", err)
		return
	}
	defer upstream.Close()
	slog.Info("tcp forward", "client", client.RemoteAddr().String(), "target", f.Target)

	done := make(chan struct{}, 2)
	go func() { copyHalf(upstream, client); done <- struct{}{} }()
	go func() { copyHalf(client, upstream); done <- struct{}{} }()
	<-done
	<-done
}
