package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"svngo/internal/logring"
)

func TestAuthzSelfTestRequired(t *testing.T) {
	requireSVN(t)
	for _, bin := range []string{"", "/bin/true"} {
		cfg := testConfig(t, t.TempDir())
		cfg.SvnAuthz = bin
		ctx, cancel := context.WithCancel(context.Background())
		a, err := New(ctx, cfg, logring.New(10))
		cancel()
		if err == nil {
			a.db.Close()
			t.Fatalf("New started with authz validator %q", bin)
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
