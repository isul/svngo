package main

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSVNGuardBlocksAndExpires(t *testing.T) {
	dir := t.TempDir()
	window := time.Hour
	var mu sync.Mutex
	var blockedIPs []string
	g, err := newSVNGuard(dir, func() (int, time.Duration) { return 3, window }, func(ip string, n int) {
		mu.Lock()
		blockedIPs = append(blockedIPs, ip)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	mapFile := filepath.Join(dir, svnBlockFile)
	read := func() string { b, _ := os.ReadFile(mapFile); return string(b) }
	if strings.Contains(read(), "203.0.113.7") {
		t.Fatal("map not empty at start")
	}

	g.Fail("203.0.113.7")
	g.Fail("203.0.113.7")
	if len(g.List()) != 0 || strings.Contains(read(), "203.0.113.7") {
		t.Fatal("blocked before threshold")
	}
	g.Fail("203.0.113.7")
	if l := g.List(); len(l) != 1 || l[0].IP != "203.0.113.7" || l[0].Fails != 3 {
		t.Fatalf("not blocked after threshold: %+v", l)
	}
	if !strings.Contains(read(), "\n203.0.113.7 ") || len(blockedIPs) != 1 {
		t.Fatalf("map/onBlock not updated: %q %v", read(), blockedIPs)
	}

	// other IPs, loopback and garbage are unaffected / ignored
	for i := 0; i < 10; i++ {
		g.Fail("127.0.0.1")
		g.Fail("::1")
		g.Fail("not-an-ip")
		g.Fail("x\n1.2.3.4 1")
	}
	g.Fail("2001:db8::5")
	if l := g.List(); len(l) != 1 {
		t.Fatalf("unexpected blocks: %+v", l)
	}
	if strings.Count(read(), "\n") != 2 { // header + one entry
		t.Fatalf("map content: %q", read())
	}

	// manual unblock
	if ok, err := g.Unblock("203.0.113.7"); !ok || err != nil {
		t.Fatalf("unblock: %v %v", ok, err)
	}
	if len(g.List()) != 0 || strings.Contains(read(), "203.0.113.7") {
		t.Fatal("still blocked after unblock")
	}
	if ok, _ := g.Unblock("203.0.113.7"); ok {
		t.Fatal("double unblock reported success")
	}

	// expiry via Prune
	window = 20 * time.Millisecond
	for i := 0; i < 3; i++ {
		g.Fail("198.51.100.9")
	}
	if len(g.List()) != 1 {
		t.Fatal("not blocked")
	}
	time.Sleep(40 * time.Millisecond)
	if err := g.Prune(); err != nil {
		t.Fatal(err)
	}
	if len(g.List()) != 0 || strings.Contains(read(), "198.51.100.9") {
		t.Fatal("expired block not pruned")
	}
}

func TestSVNGuardDisabled(t *testing.T) {
	g, err := newSVNGuard(t.TempDir(), func() (int, time.Duration) { return 0, time.Hour }, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		g.Fail("203.0.113.7")
	}
	if len(g.List()) != 0 {
		t.Fatal("blocked while disabled")
	}
}

func TestApachePumpAuthFail(t *testing.T) {
	ap := newApache(&Config{}, newRing(10))
	var got []string
	ap.onAuthFail = func(ip string) { got = append(got, ip) }
	in := strings.Join([]string{
		"svngo-authfail 203.0.113.7",
		"203.0.113.7 alice [2026-10-27 10:00:00] \"PROPFIND /svn/x HTTP/1.1\" 401 0 1ms",
		"svngo-authfail not-an-ip",
		"svngo-authfail 2001:db8::1",
	}, "\n") + "\n"
	ap.pump(strings.NewReader(in), &strings.Builder{}, true)
	if strings.Join(got, ",") != "203.0.113.7,2001:db8::1" {
		t.Fatalf("auth failures = %v", got)
	}
	if lines := ap.logs.Lines(100); len(lines) != 1 {
		t.Fatalf("marker lines must not reach the log ring: %v", lines)
	}
	// error log stream is never trusted
	got = nil
	ap.pump(strings.NewReader("svngo-authfail 198.51.100.1\n"), &strings.Builder{}, false)
	if len(got) != 0 {
		t.Fatal("stderr line treated as auth failure")
	}
}

func TestAuthzSelfTestRequired(t *testing.T) {
	requireSVN(t)
	for _, bin := range []string{"", "/bin/true"} {
		cfg := testConfig(t, t.TempDir())
		cfg.SvnAuthz = bin
		ctx, cancel := context.WithCancel(context.Background())
		a, err := newApp(ctx, cfg, newRing(10))
		cancel()
		if err == nil {
			a.db.Close()
			t.Fatalf("newApp started with authz validator %q", bin)
		}
		if !strings.Contains(err.Error(), "authz 검증 도구") {
			t.Fatalf("unexpected error for %q: %v", bin, err)
		}
	}
}

func TestSVNBlockAdminUI(t *testing.T) {
	requireSVN(t)
	e := startEnv(t, testConfig(t, t.TempDir()))
	admin := e.client()
	admin.login("admin", "Admin-Pass-1")

	// settings validation
	base := url.Values{"page_size": {"30"}, "login_attempts": {"10"}, "svn_block_minutes": {"15"}}
	bad := url.Values{}
	for k, v := range base {
		bad[k] = v
	}
	bad.Set("svn_block_attempts", "2")
	if msg := admin.postErr("/admin/system/settings", bad); !strings.Contains(msg, "SVN 로그인 실패") {
		t.Fatalf("invalid svn_block_attempts accepted: %q", msg)
	}
	good := url.Values{}
	for k, v := range base {
		good[k] = v
	}
	good.Set("svn_block_attempts", "3")
	admin.postOK("/admin/system/settings", good)
	if n, _ := e.app.svnBlockLimits(); n != 3 {
		t.Fatalf("svn_block_attempts = %d", n)
	}

	for i := 0; i < 3; i++ {
		e.app.svnGuard.Fail("203.0.113.50")
	}
	if _, page := admin.get("/admin/system"); !strings.Contains(page, "203.0.113.50") {
		t.Fatal("blocked IP not listed")
	}
	if msg := admin.postErr("/admin/system/svn-unblock", url.Values{"ip": {"bad ip"}}); !strings.Contains(msg, "IP") {
		t.Fatalf("invalid ip accepted: %q", msg)
	}
	admin.postOK("/admin/system/svn-unblock", url.Values{"ip": {"203.0.113.50"}})
	if len(e.app.svnGuard.List()) != 0 {
		t.Fatal("unblock via UI failed")
	}
	if _, page := admin.get("/admin/system/logs"); !strings.Contains(page, "SVN 접속 차단") || !strings.Contains(page, "SVN 차단 해제") {
		t.Fatal("block/unblock not audited")
	}

	// SVN users cannot unblock
	if st, _ := e.client().get("/admin/system"); st != http.StatusSeeOther {
		t.Fatalf("anonymous system page: %d", st)
	}
}
