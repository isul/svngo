package apache

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"svngo/internal/config"
	"svngo/internal/logring"
)

func TestSVNGuardBlocksAndExpires(t *testing.T) {
	dir := t.TempDir()
	window := time.Hour
	var mu sync.Mutex
	var blockedIPs []string
	g, err := NewGuard(dir, func() (int, time.Duration) { return 3, window }, func(ip string, n int) {
		mu.Lock()
		blockedIPs = append(blockedIPs, ip)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	mapFile := filepath.Join(dir, blockFile)
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
	g, err := NewGuard(t.TempDir(), func() (int, time.Duration) { return 0, time.Hour }, nil)
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
	ap := New(&config.Config{}, logring.New(10))
	var got []string
	ap.OnAuthFail = func(ip string) { got = append(got, ip) }
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
