package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// 沙箱容器只接入一个 internal 网络，唯一的出口是这个代理：
// 只放行白名单里的域名和端口，并在真正建立连接时校验目标 IP，
// 即使白名单域名被解析到内网地址（DNS rebinding）也无法访问内网。

// 默认禁止访问的地址段：内网、回环、链路本地（含云厂商元数据 169.254.169.254）、组播、保留地址
var blockedNets = mustParseCIDRs(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8",
)

// errBlockedAddr 表示目标 IP 落在禁止访问的地址段
var errBlockedAddr = errors.New("private or reserved address")

// hop-by-hop 头只在一跳内有效，转发 HTTP 请求时要去掉
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

type egressProxy struct {
	allowlist   []string
	ports       map[string]bool
	privateNets []*net.IPNet // 白名单里允许访问的内网地址段，比如内部 PyPI 镜像
	dialer      *net.Dialer
	transport   *http.Transport
}

func newEgressProxy(cfg *Config) *egressProxy {
	p := &egressProxy{allowlist: cfg.EgressAllowlist, ports: cfg.EgressPorts, privateNets: cfg.EgressPrivateNets}
	p.dialer = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: p.checkDialAddr}
	p.transport = &http.Transport{
		DialContext:           p.dialer.DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	return p
}

func (p *egressProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodConnect:
		p.handleConnect(w, r)
	case r.URL.IsAbs() && r.URL.Scheme == "http":
		p.handleHTTP(w, r)
	default:
		http.Error(w, "sandbox egress proxy: only proxy requests are supported", http.StatusBadRequest)
	}
}

// checkTarget 校验 host:port 是否在白名单内
func (p *egressProxy) checkTarget(hostport, defaultPort string) error {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		host, port = hostport, defaultPort
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if !p.ports[port] {
		return fmt.Errorf("port %s is not allowed (SANDBOX_EGRESS_PORTS)", port)
	}
	if !matchAllowlist(host, p.allowlist) {
		return fmt.Errorf("host %s is not in allowlist (SANDBOX_EGRESS_ALLOWLIST)", host)
	}
	return nil
}

// checkDialAddr 在 connect 之前校验实际要连接的 IP
func (p *egressProxy) checkDialAddr(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("invalid address %s", address)
	}
	for _, n := range p.privateNets {
		if n.Contains(ip) {
			return nil
		}
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return fmt.Errorf("destination %s is a %w", ip, errBlockedAddr)
		}
	}
	return nil
}

func (p *egressProxy) deny(w http.ResponseWriter, r *http.Request, target string, err error) {
	slog.Warn("egress denied", "client", r.RemoteAddr, "target", target, "reason", err.Error())
	http.Error(w, "sandbox egress denied: "+err.Error(), http.StatusForbidden)
}

func (p *egressProxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	target := r.Host
	if err := p.checkTarget(target, "443"); err != nil {
		p.deny(w, r, target, err)
		return
	}
	upstream, err := p.dialer.DialContext(r.Context(), "tcp", target)
	if err != nil {
		p.deny(w, r, target, err)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		client.Close()
		upstream.Close()
		return
	}
	// 客户端可能已经提前发出了 TLS ClientHello，先把缓冲里的数据转给上游
	if n := buf.Reader.Buffered(); n > 0 {
		pending, _ := buf.Reader.Peek(n)
		if _, err := upstream.Write(pending); err != nil {
			client.Close()
			upstream.Close()
			return
		}
	}
	done := make(chan struct{}, 2)
	go func() { copyHalf(upstream, client); done <- struct{}{} }()
	go func() { copyHalf(client, upstream); done <- struct{}{} }()
	<-done
	<-done
	client.Close()
	upstream.Close()
}

// copyHalf 把 src 拷到 dst，src 读完后只关闭 dst 的写方向，另一个方向继续传输
func copyHalf(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	if tc, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = tc.CloseWrite()
	} else {
		_ = dst.Close()
	}
}

func (p *egressProxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Host
	if err := p.checkTarget(target, "80"); err != nil {
		p.deny(w, r, target, err)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	resp, err := p.transport.RoundTrip(out)
	if err != nil {
		if errors.Is(err, errBlockedAddr) {
			p.deny(w, r, target, err)
			return
		}
		http.Error(w, "sandbox egress proxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// matchAllowlist：精确匹配域名或 IP；"*.example.com" 匹配所有子域名（不含 example.com 本身）
// 单独一个 "*" 放行所有域名，但内网地址和端口限制仍然生效（见 checkDialAddr、checkTarget）
func matchAllowlist(host string, allowlist []string) bool {
	for _, entry := range allowlist {
		if entry == "*" {
			return true
		}
		if suffix, ok := strings.CutPrefix(entry, "*"); ok && strings.HasPrefix(suffix, ".") {
			if strings.HasSuffix(host, suffix) {
				return true
			}
		} else if host == entry {
			return true
		}
	}
	return false
}

// rejectNetworks 拒绝来自沙箱网络的 API 请求：沙箱容器能连到服务所在的 internal 网络，不能让它调用管理接口
func rejectNetworks(nets []*net.IPNet, next http.Handler) http.Handler {
	if len(nets) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(host); ip != nil {
			for _, n := range nets {
				if n.Contains(ip) {
					writeError(w, http.StatusForbidden, "forbidden")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		nets = append(nets, n)
	}
	return nets
}
