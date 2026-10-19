package main

import (
	"errors"
	"net"
	"testing"
)

func TestMatchAllowlist(t *testing.T) {
	allowlist := []string{"pypi.org", "*.pythonhosted.org", "10.0.0.5"}
	cases := map[string]bool{
		"pypi.org":               true,
		"files.pythonhosted.org": true,
		"a.b.pythonhosted.org":   true,
		"pythonhosted.org":       false,
		"evilpythonhosted.org":   false,
		"pypi.org.evil.com":      false,
		"evil-pypi.org":          false,
		"10.0.0.5":               true,
		"example.com":            false,
	}
	for host, want := range cases {
		if got := matchAllowlist(host, allowlist); got != want {
			t.Errorf("matchAllowlist(%q) = %v, want %v", host, got, want)
		}
	}
	for _, host := range []string{"example.com", "a.b.c", "1.2.3.4"} {
		if !matchAllowlist(host, []string{"*"}) {
			t.Errorf("matchAllowlist(%q, [*]) = false, want true", host)
		}
	}
}

func TestWildcardKeepsPrivateAndPortChecks(t *testing.T) {
	p := &egressProxy{allowlist: []string{"*"}, ports: map[string]bool{"443": true}}
	if err := p.checkTarget("example.com:443", "443"); err != nil {
		t.Errorf("checkTarget with * unexpected error: %v", err)
	}
	if err := p.checkTarget("example.com:22", "443"); err == nil {
		t.Error("checkTarget with * must still enforce ports")
	}
	if err := p.checkDialAddr("tcp", "169.254.169.254:443", nil); !errors.Is(err, errBlockedAddr) {
		t.Errorf("checkDialAddr with * must still block private addresses, got %v", err)
	}
}

func TestCheckTarget(t *testing.T) {
	p := &egressProxy{allowlist: []string{"pypi.org"}, ports: map[string]bool{"443": true, "80": true}}
	ok := []string{"pypi.org:443", "PyPI.org.:443", "pypi.org"}
	for _, target := range ok {
		if err := p.checkTarget(target, "443"); err != nil {
			t.Errorf("checkTarget(%q) unexpected error: %v", target, err)
		}
	}
	bad := []string{"pypi.org:22", "example.com:443", "127.0.0.1:443"}
	for _, target := range bad {
		if err := p.checkTarget(target, "443"); err == nil {
			t.Errorf("checkTarget(%q) expected error", target)
		}
	}
}

func TestCheckDialAddr(t *testing.T) {
	_, internalMirror, _ := net.ParseCIDR("10.0.0.5/32")
	p := &egressProxy{privateNets: []*net.IPNet{internalMirror}}
	allowed := []string{"151.101.0.223:443", "[2a04:4e42::223]:443", "10.0.0.5:443"}
	for _, addr := range allowed {
		if err := p.checkDialAddr("tcp", addr, nil); err != nil {
			t.Errorf("checkDialAddr(%q) unexpected error: %v", addr, err)
		}
	}
	blocked := []string{
		"127.0.0.1:443", "10.0.0.6:443", "172.17.0.2:443", "192.168.1.1:80", "169.254.169.254:80",
		"100.64.0.1:443", "0.0.0.0:443", "[::1]:443", "[fd00::1]:443", "[::ffff:127.0.0.1]:443",
	}
	for _, addr := range blocked {
		if err := p.checkDialAddr("tcp", addr, nil); err == nil {
			t.Errorf("checkDialAddr(%q) expected error", addr)
		}
	}
}
