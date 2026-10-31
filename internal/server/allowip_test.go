package server

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"testing"

	"svngo/internal/apache"
	"svngo/internal/ipacl"
	"svngo/internal/logring"
	"svngo/internal/store"
)

// ipTransport makes requests appear to come from ip: the test server sees a
// loopback peer and trusts the last X-Forwarded-For entry, as behind Apache.
type ipTransport struct {
	ip   string
	base http.RoundTripper
}

func (t ipTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Forwarded-For", "10.255.255.1, "+t.ip)
	return t.base.RoundTrip(r)
}

// clientFrom returns a Korean browser whose requests come from ip.
func (e *testEnv) clientFrom(ip string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{e: e, http: &http.Client{Jar: jar, Transport: ipTransport{ip, langTransport{"ko"}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func TestAdminAllowList(t *testing.T) {
	requireSVN(t)
	e := startEnv(t, testConfig(t, t.TempDir()))
	admin := e.clientFrom("203.0.113.10")
	if admin.login("admin", "Admin-Pass-1") != http.StatusSeeOther {
		t.Fatal("admin login failed")
	}
	_, page := admin.get("/admin/system")
	if !strings.Contains(page, "접근 제한 (허용 IP)") || !strings.Contains(page, "203.0.113.10") || !strings.Contains(page, "제한 없음") {
		t.Fatal("access restriction section or current IP missing")
	}
	if !strings.Contains(page, "SVN 목록은 저장만 되고") {
		t.Fatal("unmanaged Apache warning missing")
	}

	// validation: bad entry with line number, self lockout
	if msg := admin.postErr("/admin/system/access", url.Values{"admin_allow_ips": {"203.0.113.10\nexample.com"}}); !strings.Contains(msg, "2번째 줄") || !strings.Contains(msg, "example.com") {
		t.Fatalf("invalid entry accepted: %q", msg)
	}
	if msg := admin.postErr("/admin/system/access", url.Values{"admin_allow_ips": {"198.51.100.0/24"}}); !strings.Contains(msg, "203.0.113.10") {
		t.Fatalf("self lockout not prevented: %q", msg)
	}
	if v := store.Setting(context.Background(), e.app.db, settingAdminAllow); v != "" {
		t.Fatalf("rejected list stored: %q", v)
	}

	page = admin.postOK("/admin/system/access", url.Values{
		"admin_allow_ips": {"203.0.113.0/24 # office\n198.51.100.7"},
		"svn_allow_ips":   {"2001:DB8::/32, 192.0.2.1"},
	})
	if !strings.Contains(page, "접근 제한을 저장했습니다") {
		t.Fatal("save message missing")
	}
	ctx := context.Background()
	if v := store.Setting(ctx, e.app.db, settingAdminAllow); v != "203.0.113.0/24\n198.51.100.7" {
		t.Fatalf("admin list stored as %q", v)
	}
	if v := store.Setting(ctx, e.app.db, settingSvnAllow); v != "2001:db8::/32\n192.0.2.1" {
		t.Fatalf("svn list stored as %q", v)
	}
	if !strings.Contains(page, "제한 중 (2개)") {
		t.Fatal("restricted badge missing")
	}

	// enforcement
	outsider := e.clientFrom("198.51.100.8")
	st, body := outsider.get("/admin/login")
	if st != http.StatusForbidden || !strings.Contains(body, "198.51.100.8") {
		t.Fatalf("disallowed IP: status %d", st)
	}
	if st, _, _ := outsider.post("/admin/login", url.Values{"username": {"admin"}, "password": {"Admin-Pass-1"}}); st != http.StatusForbidden {
		t.Fatalf("disallowed IP login POST: status %d", st)
	}
	if st, _ := outsider.get("/admin/static/app.css"); st != http.StatusOK {
		t.Fatalf("static assets blocked: %d", st)
	}
	if st, _ := e.clientFrom("198.51.100.7").get("/admin/login"); st != http.StatusOK {
		t.Fatalf("allowed single IP rejected: %d", st)
	}
	if st, _ := e.clientFrom("::ffff:203.0.113.99").get("/admin/login"); st != http.StatusOK {
		t.Fatalf("allowed IPv4-mapped address rejected: %d", st)
	}
	if st, _ := e.client().get("/admin/healthz"); st != http.StatusOK {
		t.Fatalf("loopback health check blocked: %d", st)
	}
	if st, _ := admin.get("/admin/repos"); st != http.StatusOK {
		t.Fatalf("allowed admin blocked: %d", st)
	}

	// reset outside the UI (svngo access-reset) applies without restart
	if err := store.SetSetting(ctx, e.app.db, settingAdminAllow, ""); err != nil {
		t.Fatal(err)
	}
	if st, _ := outsider.get("/admin/login"); st != http.StatusOK {
		t.Fatalf("cleared list still enforced: %d", st)
	}

	// a corrupted stored value fails closed (loopback only)
	store.SetSetting(ctx, e.app.db, settingAdminAllow, "not an ip")
	if st, _ := admin.get("/admin/login"); st != http.StatusForbidden {
		t.Fatalf("invalid stored list did not fail closed: %d", st)
	}
	if st, _ := e.client().get("/admin/login"); st != http.StatusOK {
		t.Fatalf("loopback blocked by invalid stored list: %d", st)
	}
	store.SetSetting(ctx, e.app.db, settingAdminAllow, "")

	if _, page := admin.get("/admin/system/logs"); !strings.Contains(page, "접근 제한 변경") || !strings.Contains(page, "admin 0 -&gt; 2, svn 0 -&gt; 2") {
		t.Fatal("access restriction change not audited")
	}
}

func TestAdminAllowListFromEnv(t *testing.T) {
	requireSVN(t)
	cfg := testConfig(t, t.TempDir())
	cfg.AdminAllowEnv = ipacl.MustParse("203.0.113.10")
	cfg.SvnAllowEnv = ipacl.MustParse("192.0.2.0/24")
	e := startEnv(t, cfg)

	if st, _ := e.clientFrom("198.51.100.8").get("/admin/login"); st != http.StatusForbidden {
		t.Fatalf("env list not enforced: %d", st)
	}
	admin := e.clientFrom("203.0.113.10")
	if admin.login("admin", "Admin-Pass-1") != http.StatusSeeOther {
		t.Fatal("admin from env-allowed IP cannot log in")
	}
	_, page := admin.get("/admin/system")
	if !strings.Contains(page, "SVNGO_ADMIN_ALLOW_IPS") || !strings.Contains(page, "SVNGO_SVN_ALLOW_IPS") || !strings.Contains(page, "192.0.2.0/24") {
		t.Fatal("fixed env entries not shown")
	}
	// the env list keeps the admin reachable, so an unrelated web list is accepted
	admin.postOK("/admin/system/access", url.Values{"admin_allow_ips": {"198.51.100.0/24"}})
	if st, _ := e.clientFrom("198.51.100.8").get("/admin/login"); st != http.StatusOK {
		t.Fatalf("web list not merged with env list: %d", st)
	}
	if st, _ := admin.get("/admin/system"); st != http.StatusOK {
		t.Fatalf("env-allowed admin blocked: %d", st)
	}
}

// writeFakeHttpd creates a stand-in for httpd -t that succeeds or fails.
func writeFakeHttpd(t *testing.T, dir string, ok bool) string {
	t.Helper()
	p := dir + "/fake-httpd"
	script := "#!/bin/sh\nexit 0\n"
	if !ok {
		script = "#!/bin/sh\necho 'Syntax error in svn-access.conf'\nexit 1\n"
	}
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSvnAllowListApply(t *testing.T) {
	requireSVN(t)
	dir := t.TempDir()
	cfg := testConfig(t, dir)
	cfg.ManageApache = true
	cfg.ApacheBin = writeFakeHttpd(t, dir, true)
	cfg.ApacheConf = dir + "/httpd.conf"
	cfg.TrustedProxies = ipacl.MustParse("172.22.0.1")
	e := startEnv(t, cfg)
	ctx := context.Background()
	if err := e.app.initSvnAllow(ctx); err != nil {
		t.Fatal(err)
	}
	e.app.apache = apache.New(cfg, logring.New(10))
	live := apache.AccessFile(cfg.RunDir)
	if b, _ := os.ReadFile(live); !strings.Contains(string(b), "no restriction") {
		t.Fatalf("initial SVN rules: %q", b)
	}

	admin := e.clientFrom("203.0.113.10")
	admin.login("admin", "Admin-Pass-1")
	_, page := admin.get("/admin/system")
	if strings.Contains(page, "SVN 목록은 저장만 되고") {
		t.Fatal("unmanaged warning shown although Apache is managed")
	}
	if !strings.Contains(page, "신뢰하는 프록시") || !strings.Contains(page, "172.22.0.1") || !strings.Contains(page, "SVNGO_TRUSTED_PROXIES") {
		t.Fatal("trusted proxies not shown")
	}
	admin.postOK("/admin/system/access", url.Values{"svn_allow_ips": {"192.0.2.0/24"}})
	b, _ := os.ReadFile(live)
	if !strings.Contains(string(b), "RewriteCond expr \"! -R '192.0.2.0/24'\"") || !strings.Contains(string(b), "[F,L]") {
		t.Fatalf("SVN rules not applied: %q", b)
	}

	// a failing configuration test keeps the old rules and the old settings
	cfg.ApacheBin = writeFakeHttpd(t, t.TempDir(), false)
	e.app.apache = apache.New(cfg, logring.New(10))
	msg := admin.postErr("/admin/system/access", url.Values{"svn_allow_ips": {"198.51.100.0/24"}})
	if !strings.Contains(msg, "적용하지 못했습니다") || !strings.Contains(msg, "Syntax error") {
		t.Fatalf("apply failure not reported: %q", msg)
	}
	if v := store.Setting(ctx, e.app.db, settingSvnAllow); v != "192.0.2.0/24" {
		t.Fatalf("settings not restored after failed apply: %q", v)
	}
	if b2, _ := os.ReadFile(live); string(b2) != string(b) {
		t.Fatal("live SVN rules changed after failed apply")
	}

	// changes made outside the UI are picked up by the periodic sync
	cfg.ApacheBin = writeFakeHttpd(t, t.TempDir(), true)
	e.app.apache = apache.New(cfg, logring.New(10))
	store.SetSetting(ctx, e.app.db, settingSvnAllow, "")
	if err := e.app.syncSvnAllow(ctx); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(live); !strings.Contains(string(b), "no restriction") {
		t.Fatalf("cleared SVN list not applied: %q", b)
	}
}
