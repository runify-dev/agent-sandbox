package main

import (
	"io"
	"net"
	"testing"
	"time"
)

func TestParseTCPForwards(t *testing.T) {
	got, err := parseTCPForwards(" 5432=192.168.11.153:5432, 13306=db.internal:3306 ", ":8090", ":3128")
	if err != nil {
		t.Fatal(err)
	}
	want := []tcpForward{{"5432", "192.168.11.153:5432"}, {"13306", "db.internal:3306"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}

	for _, bad := range []string{
		"5432",               // 缺少目标
		"abc=1.2.3.4:5432",   // 监听端口不是数字
		"0=1.2.3.4:5432",     // 端口越界
		"5432=1.2.3.4",       // 目标缺少端口
		"5432=:5432",         // 目标缺少主机
		"5432=1.2.3.4:99999", // 目标端口越界
		"8090=1.2.3.4:5432",  // 和 API 端口冲突
		"3128=1.2.3.4:5432",  // 和代理端口冲突
		"5432=a:1,5432=b:2",  // 重复的监听端口
	} {
		if _, err := parseTCPForwards(bad, ":8090", ":3128"); err == nil {
			t.Errorf("parseTCPForwards(%q) succeeded, want error", bad)
		}
	}

	if got, err := parseTCPForwards("", ":8090"); err != nil || len(got) != 0 {
		t.Errorf("empty config: got %v, %v", got, err)
	}
}

// startEcho 启动一个回显服务，返回它的地址
func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

// freePort 返回一个当前空闲的本地端口
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

func TestTCPForwarderRelays(t *testing.T) {
	port := freePort(t)
	fw := newTCPForwarder([]tcpForward{{port, startEcho(t)}}, nil)
	if err := fw.Start(); err != nil {
		t.Fatal(err)
	}
	defer fw.Close()

	c, err := net.Dial("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("select 1")); err != nil {
		t.Fatal(err)
	}
	// 半关闭后回显服务读到 EOF 会关闭连接，ReadAll 能完整读到数据
	_ = c.(*net.TCPConn).CloseWrite()
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "select 1" {
		t.Fatalf("got %q, want %q", got, "select 1")
	}
}

func TestTCPForwarderRejectsOtherNetworks(t *testing.T) {
	port := freePort(t)
	_, sandboxNet, _ := net.ParseCIDR("172.30.0.0/16")
	fw := newTCPForwarder([]tcpForward{{port, startEcho(t)}}, []*net.IPNet{sandboxNet})
	if err := fw.Start(); err != nil {
		t.Fatal(err)
	}
	defer fw.Close()

	// 来自 127.0.0.1 的连接不在沙箱网段内，会被直接关闭
	c, err := net.Dial("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = c.Write([]byte("x"))
	if n, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatalf("read %d bytes from rejected connection, want error", n)
	}
}

func TestTCPForwarderPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	if err := newTCPForwarder([]tcpForward{{port, "127.0.0.1:1"}}, nil).Start(); err == nil {
		t.Fatal("Start succeeded on a port that is in use")
	}
}
