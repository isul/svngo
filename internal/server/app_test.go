package server

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"svngo/internal/authz"
	"svngo/internal/config"
	"svngo/internal/logring"
	"svngo/internal/passwd"
	"svngo/internal/svn"
	"svngo/internal/validate"
)

// ---- test harness ----

type testEnv struct {
	t      *testing.T
	cfg    *config.Config
	app    *App
	srv    *httptest.Server
	cancel context.CancelFunc
}

func requireSVN(t *testing.T) {
	for _, b := range []string{"svnadmin", "svnlook", "svn"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skip("Subversion tools not installed")
		}
	}
	if firstBin("svngo-authz", "svnauthz") == "" {
		t.Skip("svnauthz not installed")
	}
}

func firstBin(names ...string) string {
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

func testConfig(t *testing.T, dir string) *config.Config {
	return &config.Config{
		ReposDir: filepath.Join(dir, "repos"), ConfigDir: filepath.Join(dir, "config"),
		DBDir: filepath.Join(dir, "db"), BackupDir: filepath.Join(dir, "backups"), RunDir: filepath.Join(dir, "run"),
		AdminUser: "admin", AdminPassword: "Admin-Pass-1", BcryptCost: 5, CookieSecure: "never", SessionTTL: time.Hour,
		SvnAdmin: firstBin("svnadmin"), SvnLook: firstBin("svnlook"), Svn: firstBin("svn"),
		SvnAuthz: firstBin("svngo-authz", "svnauthz"),
	}
}

func startEnv(t *testing.T, cfg *config.Config) *testEnv {
	ctx, cancel := context.WithCancel(context.Background())
	a, err := New(ctx, cfg, logring.New(100))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.routes())
	e := &testEnv{t: t, cfg: cfg, app: a, srv: srv, cancel: cancel}
	t.Cleanup(e.stop)
	return e
}

func (e *testEnv) stop() {
	if e.srv == nil {
		return
	}
	e.srv.Close()
	e.cancel()
	e.app.jobsWG.Wait()
	e.app.db.Close()
	e.srv = nil
}

type client struct {
	e    *testEnv
	http *http.Client
	csrf string
}

// langTransport sends a fixed Accept-Language header unless the request sets one.
type langTransport struct{ lang string }

func (t langTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.lang != "" && r.Header.Get("Accept-Language") == "" {
		r = r.Clone(r.Context())
		r.Header.Set("Accept-Language", t.lang)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// client returns a browser whose language is Korean, which page assertions use.
func (e *testEnv) client() *client { return e.clientLang("ko") }

// clientLang returns a browser sending the given Accept-Language ("" = none).
func (e *testEnv) clientLang(lang string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{e: e, http: &http.Client{Jar: jar, Transport: langTransport{lang}, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}}
}

var reCSRF = regexp.MustCompile(`name="_csrf" value="([^"]+)"`)

func (c *client) get(path string) (int, string) {
	c.e.t.Helper()
	resp, err := c.http.Get(c.e.srv.URL + path)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if m := reCSRF.FindSubmatch(b); m != nil {
		c.csrf = string(m[1])
	}
	return resp.StatusCode, string(b)
}

// post submits a form with the CSRF token; returns status, Location and body.
func (c *client) post(path string, form url.Values) (int, string, string) {
	c.e.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	if c.csrf != "" && form.Get("_csrf") == "" {
		form.Set("_csrf", c.csrf)
	}
	req, _ := http.NewRequest("POST", c.e.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", c.e.srv.URL)
	resp, err := c.http.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), string(b)
}

// flash follows the redirect after a POST and returns the rendered page.
func (c *client) postOK(path string, form url.Values) string {
	c.e.t.Helper()
	st, loc, body := c.post(path, form)
	if st != http.StatusSeeOther {
		c.e.t.Fatalf("POST %s: status %d body=%s", path, st, validate.Clip(body, 400))
	}
	_, page := c.get(loc)
	if strings.Contains(page, `class="alert err"`) {
		c.e.t.Fatalf("POST %s reported error: %s", path, extractAlert(page))
	}
	return page
}

func (c *client) postErr(path string, form url.Values) string {
	c.e.t.Helper()
	st, loc, body := c.post(path, form)
	if st != http.StatusSeeOther {
		return body
	}
	_, page := c.get(loc)
	if !strings.Contains(page, `class="alert err"`) {
		c.e.t.Fatalf("POST %s expected error, got success", path)
	}
	return extractAlert(page)
}

var reAlert = regexp.MustCompile(`(?s)class="alert (?:err|ok)"[^>]*>(.*?)</div>`)

func extractAlert(page string) string {
	if m := reAlert.FindStringSubmatch(page); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func (c *client) login(user, pw string) int {
	c.e.t.Helper()
	c.csrf = ""
	c.get("/admin/login") // login CSRF cookie + token
	st, _, _ := c.post("/admin/login", url.Values{"username": {user}, "password": {pw}, "next": {"/admin/"}})
	if st == http.StatusSeeOther {
		c.get("/admin/account") // load CSRF token
	}
	return st
}

func (e *testEnv) accessOf(repo, path, user string) string {
	e.t.Helper()
	got, err := e.app.svn.AccessOf(context.Background(), e.cfg.AuthzFile(), repo, path, user)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

func (e *testEnv) htpasswd() map[string]string {
	e.t.Helper()
	m, _, err := readHtpasswdFile(e.cfg.HtpasswdFile())
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func run(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LC_ALL=C.UTF-8")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}

func waitJobs(t *testing.T, e *testEnv) []JobRow {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		jobs, _ := e.app.listJobs(context.Background(), "", 50)
		if !hasActiveJobs(jobs) {
			return jobs
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("jobs did not finish")
	return nil
}

// ---- the end-to-end flow (without Apache) ----

func TestWebFlowWithRealSVN(t *testing.T) {
	requireSVN(t)
	if firstBin("svngo-authz", "svnauthz") == "" {
		t.Skip("svnauthz not installed")
	}
	dir := t.TempDir()
	cfg := testConfig(t, dir)
	e := startEnv(t, cfg)
	admin := e.client()

	// security headers and auth redirect
	st, _ := admin.get("/admin/")
	if st != http.StatusSeeOther {
		t.Fatalf("anonymous dashboard: %d", st)
	}
	resp, _ := http.Get(e.srv.URL + "/admin/login")
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("missing security headers: %v", resp.Header)
	}
	resp.Body.Close()

	// 1. admin login (wrong password rejected)
	if st := admin.login("admin", "wrong-password"); st != http.StatusUnauthorized {
		t.Fatalf("wrong admin password: %d", st)
	}
	if st := admin.login("admin", "Admin-Pass-1"); st != http.StatusSeeOther {
		t.Fatalf("admin login: %d", st)
	}
	if _, page := admin.get("/admin/"); !strings.Contains(page, "대시보드") {
		t.Fatal("dashboard not rendered")
	}

	// CSRF: missing token rejected
	req, _ := http.NewRequest("POST", e.srv.URL+"/admin/repos", strings.NewReader("name=evil"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if resp, _ := admin.http.Do(req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("CSRF not enforced: %d", resp.StatusCode)
	}
	// cross-origin post rejected even with token
	req, _ = http.NewRequest("POST", e.srv.URL+"/admin/repos", strings.NewReader("name=evil&_csrf="+admin.csrf))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	if resp, _ := admin.http.Do(req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("Origin not enforced: %d", resp.StatusCode)
	}

	// 2. create repository (with standard layout)
	admin.postOK("/admin/repos", url.Values{"name": {"proj"}, "description": {"테스트 저장소"}, "layout": {"1"}})
	repoDir := filepath.Join(cfg.ReposDir, "proj")
	if y := svn.FSFSYoungest(repoDir); y != 1 {
		t.Fatalf("youngest after layout = %d", y)
	}
	if msg := admin.postErr("/admin/repos", url.Values{"name": {"../etc"}}); !strings.Contains(msg, "저장소 이름") {
		t.Fatalf("invalid repo name accepted: %s", msg)
	}
	if msg := admin.postErr("/admin/repos", url.Values{"name": {"proj"}}); msg == "" {
		t.Fatal("duplicate repo accepted")
	}

	// 3. create users
	admin.postOK("/admin/users", url.Values{"username": {"alice"}, "full_name": {"앨리스"}, "password": {"Alice-Pass-1"}, "password2": {"Alice-Pass-1"}})
	admin.postOK("/admin/users", url.Values{"username": {"bob"}, "password": {"Bob-Pass-12"}, "password2": {"Bob-Pass-12"}})
	ht := e.htpasswd()
	if !passwd.Verify(ht["alice"], "Alice-Pass-1") || !strings.HasPrefix(ht["alice"], "$2y$") {
		t.Fatalf("alice not in htpasswd with bcrypt: %q", ht["alice"])
	}

	// 4. grant write permission to alice (verified with libsvn)
	if got := e.accessOf("proj", "/", "alice"); got != "no" {
		t.Fatalf("alice access before grant = %s", got)
	}
	admin.postOK("/admin/repos/proj/perms", url.Values{"path": {"/"}, "subject": {"user:alice"}, "access": {"rw"}, "back": {"/admin/repos/proj/perms"}})
	if got := e.accessOf("proj", "/", "alice"); got != "rw" {
		t.Fatalf("alice access after grant = %s", got)
	}
	if got := e.accessOf("proj", "/", "bob"); got != "no" {
		t.Fatalf("bob access = %s", got)
	}

	// 5/6. commit files (file:// - the HTTP path is covered by the Docker e2e test)
	wc := filepath.Join(dir, "wc")
	run(t, dir, "svn", "checkout", "-q", svn.FileURL(repoDir, "/trunk", -1), wc)
	os.WriteFile(filepath.Join(wc, "hello.txt"), []byte("hello svngo\n안녕하세요\n"), 0o644)
	os.MkdirAll(filepath.Join(wc, "secret"), 0o755)
	os.WriteFile(filepath.Join(wc, "secret", "key.txt"), []byte("top secret\n"), 0o644)
	os.WriteFile(filepath.Join(wc, "한글 파일.txt"), []byte("유니코드 경로\n"), 0o644)
	run(t, wc, "svn", "add", "-q", "hello.txt", "secret", "한글 파일.txt")
	run(t, wc, "svn", "commit", "-q", "--username", "alice", "-m", "첫 커밋: hello")
	os.WriteFile(filepath.Join(wc, "hello.txt"), []byte("hello svngo\n안녕하세요\nsecond line\n"), 0o644)
	run(t, wc, "svn", "commit", "-q", "--username", "alice", "-m", "두번째 커밋")

	// 8. web UI shows files and commits
	if _, page := admin.get("/admin/repos/proj/files/trunk"); !strings.Contains(page, "hello.txt") || !strings.Contains(page, "한글 파일.txt") {
		i := strings.Index(page, `class="box error-box"`)
		t.Fatalf("file list missing entries: %s", page[max(i, 0):min(len(page), max(i, 0)+800)])
	}
	if _, page := admin.get("/admin/repos/proj/files/trunk/hello.txt"); !strings.Contains(page, "second line") {
		t.Fatal("file content not shown")
	}
	if _, page := admin.get("/admin/repos/proj/files/trunk/" + url.PathEscape("한글 파일.txt")); !strings.Contains(page, "유니코드 경로") {
		t.Fatal("unicode file not shown")
	}
	if _, page := admin.get("/admin/repos/proj/files/trunk/hello.txt?rev=2"); strings.Contains(page, "second line") {
		t.Fatal("rev=2 should show old content")
	}
	resp, _ = admin.http.Get(e.srv.URL + "/admin/repos/proj/raw/trunk/hello.txt")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(raw) != "hello svngo\n안녕하세요\nsecond line\n" || resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("raw download mismatch: %q %s", raw, resp.Header.Get("Content-Type"))
	}
	if _, page := admin.get("/admin/repos/proj/commits"); !strings.Contains(page, "첫 커밋: hello") || !strings.Contains(page, "두번째 커밋") || !strings.Contains(page, "alice") {
		t.Fatal("commit log missing")
	}
	if _, page := admin.get("/admin/repos/proj/commits/3"); !strings.Contains(page, "&#43;second line") || !strings.Contains(page, "/trunk/hello.txt") {
		i := strings.Index(page, "변경 파일")
		t.Fatalf("commit diff missing: %s", page[max(i, 0):min(len(page), max(i, 0)+2500)])
	}
	if st, _ := admin.get("/admin/repos/proj/files/trunk/nothing"); st != http.StatusNotFound {
		t.Fatalf("missing file status %d", st)
	}
	if st, _ := admin.get("/admin/repos/proj/files/trunk/../../etc/passwd"); st == http.StatusOK {
		t.Fatal("path traversal returned 200")
	}

	// directory level permission: alice cannot read /trunk/secret
	admin.postOK("/admin/perms", url.Values{"repo": {"proj"}, "path": {"/trunk/secret"}, "subject": {"user:alice"}, "access": {"none"}, "back": {"/admin/perms"}})
	if got := e.accessOf("proj", "/trunk/secret/key.txt", "alice"); got != "no" {
		t.Fatalf("alice secret access = %s", got)
	}

	// SVN user login: web browsing is filtered by SVN permissions
	alice := e.client()
	if st := alice.login("alice", "Alice-Pass-1"); st != http.StatusSeeOther {
		t.Fatalf("alice login: %d", st)
	}
	if _, page := alice.get("/admin/repos"); !strings.Contains(page, "proj") {
		t.Fatal("alice cannot see proj")
	}
	if _, page := alice.get("/admin/repos/proj/files/trunk"); strings.Contains(page, "secret") || !strings.Contains(page, "hello.txt") {
		t.Fatal("alice listing not filtered")
	}
	if st, _ := alice.get("/admin/repos/proj/files/trunk/secret/key.txt"); st != http.StatusNotFound {
		t.Fatalf("alice reads secret file: %d", st)
	}
	if st, _ := alice.get("/admin/repos/proj/raw/trunk/secret/key.txt"); st != http.StatusNotFound {
		t.Fatalf("alice downloads secret file: %d", st)
	}
	if _, page := alice.get("/admin/repos/proj/commits/2"); strings.Contains(page, "top secret") || strings.Contains(page, "첫 커밋") {
		t.Fatal("secret diff or message leaked to alice")
	}
	if st, _ := alice.get("/admin/users"); st != http.StatusForbidden {
		t.Fatalf("svn user reached admin page: %d", st)
	}
	bob := e.client()
	bob.login("bob", "Bob-Pass-12")
	if _, page := bob.get("/admin/repos"); strings.Contains(page, "/admin/repos/proj") {
		t.Fatal("bob sees proj without permission")
	}
	if st, _ := bob.get("/admin/repos/proj/files/trunk/hello.txt"); st != http.StatusNotFound {
		t.Fatalf("bob reads file: %d", st)
	}

	// 9/10. remove alice's permission -> denied
	rules, _ := authz.ListRules(context.Background(), e.app.db, "proj", "alice")
	for _, rl := range rules {
		admin.postOK("/admin/rules/"+itoa(rl.ID)+"/delete", url.Values{"back": {"/admin/perms"}})
	}
	if got := e.accessOf("proj", "/", "alice"); got != "no" {
		t.Fatalf("alice access after removal = %s", got)
	}
	if st, _ := alice.get("/admin/repos/proj"); st != http.StatusNotFound {
		t.Fatalf("alice still sees repo: %d", st)
	}

	// 11. password change -> old password rejected
	admin.postOK("/admin/users/alice/password", url.Values{"password": {"Alice-New-2"}, "password2": {"Alice-New-2"}})
	ht = e.htpasswd()
	if passwd.Verify(ht["alice"], "Alice-Pass-1") || !passwd.Verify(ht["alice"], "Alice-New-2") {
		t.Fatal("htpasswd not updated after password change")
	}
	if st := e.client().login("alice", "Alice-Pass-1"); st != http.StatusUnauthorized {
		t.Fatalf("old password accepted: %d", st)
	}
	if st, _ := alice.get("/admin/repos"); st != http.StatusSeeOther {
		t.Fatalf("alice session should be revoked after password reset: %d", st)
	}

	// disable user -> removed from htpasswd
	admin.postOK("/admin/users/bob/toggle", nil)
	if _, ok := e.htpasswd()["bob"]; ok {
		t.Fatal("disabled user still in htpasswd")
	}
	admin.postOK("/admin/users/bob/toggle", nil)

	// groups
	admin.postOK("/admin/groups", url.Values{"name": {"dev"}})
	admin.postOK("/admin/groups/dev/members", url.Values{"users": {"alice", "bob"}})
	admin.postOK("/admin/perms", url.Values{"repo": {"proj"}, "path": {"/trunk"}, "subject": {"group:dev"}, "access": {"r"}, "back": {"/admin/perms"}})
	if e.accessOf("proj", "/trunk", "bob") != "r" || e.accessOf("proj", "/", "bob") != "no" {
		t.Fatal("group rule not applied")
	}
	admin.postOK("/admin/groups/dev/members/bob/remove", nil)
	if e.accessOf("proj", "/trunk", "bob") != "no" {
		t.Fatal("group member removal not applied")
	}

	// APR1 import + transparent upgrade on web login
	admin.postOK("/admin/users/import", url.Values{"data": {"legacy:$apr1$Ab3dEfGh$o.h.BiUJ8Lr.RdMYeOtOA/\nbad:{SHA}xxxx"}})
	if e.htpasswd()["legacy"] != "$apr1$Ab3dEfGh$o.h.BiUJ8Lr.RdMYeOtOA/" {
		t.Fatal("legacy hash not imported")
	}
	if st := e.client().login("legacy", "secret-pass"); st != http.StatusSeeOther {
		t.Fatalf("legacy login: %d", st)
	}
	if h := e.htpasswd()["legacy"]; !strings.HasPrefix(h, "$2y$") || !passwd.Verify(h, "secret-pass") {
		t.Fatalf("legacy hash not upgraded: %s", h)
	}

	// repository disable / enable
	admin.postOK("/admin/repos/proj/toggle", nil)
	if e.accessOf("proj", "/trunk", "alice") != "no" {
		t.Fatal("disabled repo still accessible")
	}
	admin.postOK("/admin/repos/proj/toggle", nil)

	// backup -> restore into a new repository
	admin.postOK("/admin/repos/proj/backup", nil)
	jobs := waitJobs(t, e)
	if jobs[0].Status != "ok" {
		t.Fatalf("backup failed: %+v", jobs[0])
	}
	backups, _ := e.app.listBackups("proj")
	if len(backups) != 1 {
		t.Fatalf("backups = %v", backups)
	}
	admin.postOK("/admin/system/restore", url.Values{"file": {backups[0].Name}, "target": {"proj-copy"}})
	jobs = waitJobs(t, e)
	if jobs[0].Status != "ok" {
		t.Fatalf("restore failed: %+v", jobs[0])
	}
	if svn.FSFSYoungest(filepath.Join(cfg.ReposDir, "proj-copy")) != 3 {
		t.Fatal("restored repository has wrong revision")
	}
	admin.postOK("/admin/repos/proj/verify", nil)
	if jobs = waitJobs(t, e); jobs[0].Status != "ok" {
		t.Fatalf("verify failed: %+v", jobs[0])
	}

	// rename and delete
	admin.postOK("/admin/repos/proj-copy/rename", url.Values{"new_name": {"proj-renamed"}})
	if !svn.IsRepoDir(filepath.Join(cfg.ReposDir, "proj-renamed")) {
		t.Fatal("rename failed")
	}
	if msg := admin.postErr("/admin/repos/proj-renamed/delete", url.Values{"confirm": {"wrong"}}); msg == "" {
		t.Fatal("delete without confirmation accepted")
	}
	admin.postOK("/admin/repos/proj-renamed/delete", url.Values{"confirm": {"proj-renamed"}})
	if _, err := os.Stat(filepath.Join(cfg.ReposDir, "proj-renamed")); !os.IsNotExist(err) {
		t.Fatal("repository directory not removed")
	}

	// sync check reports everything consistent
	st, _, page := admin.post("/admin/system/sync", nil)
	if st != http.StatusOK || !strings.Contains(page, "모든 검증을 통과했습니다") {
		t.Fatalf("sync check failed: %s", extractAlert(page))
	}

	// 12. restart: data survives, files regenerated identically
	beforeH, _ := os.ReadFile(cfg.HtpasswdFile())
	beforeA, _ := os.ReadFile(cfg.AuthzFile())
	e.stop()
	e2 := startEnv(t, cfg)
	afterH, _ := os.ReadFile(cfg.HtpasswdFile())
	afterA, _ := os.ReadFile(cfg.AuthzFile())
	if string(beforeH) != string(afterH) || string(beforeA) != string(afterA) {
		t.Fatal("auth files changed after restart")
	}
	c := e2.client()
	if st := c.login("alice", "Alice-New-2"); st != http.StatusSeeOther {
		t.Fatalf("alice login after restart: %d", st)
	}
	admin2 := e2.client()
	admin2.login("admin", "Admin-Pass-1")
	if _, page := admin2.get("/admin/repos/proj/commits"); !strings.Contains(page, "두번째 커밋") {
		t.Fatal("commits missing after restart")
	}
}

func TestLoginRateLimit(t *testing.T) {
	requireSVN(t)
	e := startEnv(t, testConfig(t, t.TempDir()))
	c := e.client()
	for i := 0; i < 10; i++ {
		c.login("admin", "bad-password")
	}
	if st := c.login("admin", "Admin-Pass-1"); st != http.StatusTooManyRequests {
		t.Fatalf("rate limit not applied: %d", st)
	}
}

// rawPost sends a form with explicit headers and returns the response.
func (c *client) rawPost(path string, form url.Values, headers map[string]string) *http.Response {
	c.e.t.Helper()
	req, _ := http.NewRequest("POST", c.e.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

func hasSessionCookie(resp *http.Response) bool {
	for _, ck := range resp.Cookies() {
		if ck.Name == sessionCookie && ck.Value != "" {
			return true
		}
	}
	return false
}

func TestLoginCSRF(t *testing.T) {
	requireSVN(t)
	e := startEnv(t, testConfig(t, t.TempDir()))
	creds := url.Values{"username": {"admin"}, "password": {"Admin-Pass-1"}, "next": {"/admin/"}}
	same := map[string]string{"Origin": e.srv.URL}
	withToken := func(c *client, tok string) url.Values {
		f := url.Values{}
		for k, v := range creds {
			f[k] = v
		}
		f.Set("_csrf", tok)
		return f
	}

	// no login cookie / token at all
	c := e.client()
	if resp := c.rawPost("/admin/login", creds, same); resp.StatusCode != http.StatusForbidden || hasSessionCookie(resp) {
		t.Fatalf("login without CSRF token: %d", resp.StatusCode)
	}

	// cookie present but the form token is forged
	c = e.client()
	c.get("/admin/login")
	if c.csrf == "" {
		t.Fatal("login page has no CSRF token")
	}
	if resp := c.rawPost("/admin/login", withToken(c, "forged-"+c.csrf[7:]), same); resp.StatusCode != http.StatusForbidden || hasSessionCookie(resp) {
		t.Fatalf("login with forged token: %d", resp.StatusCode)
	}

	// token from another browser (attacker fetched their own pair)
	attacker := e.client()
	attacker.get("/admin/login")
	victim := e.client()
	victim.get("/admin/login")
	if resp := victim.rawPost("/admin/login", withToken(victim, attacker.csrf), same); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("login with another client's token: %d", resp.StatusCode)
	}

	// valid token but cross-site browser request
	c = e.client()
	c.get("/admin/login")
	if resp := c.rawPost("/admin/login", withToken(c, c.csrf), map[string]string{"Sec-Fetch-Site": "cross-site"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site login (Sec-Fetch-Site): %d", resp.StatusCode)
	}
	if resp := c.rawPost("/admin/login", withToken(c, c.csrf), map[string]string{"Origin": "https://evil.example"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site login (Origin): %d", resp.StatusCode)
	}

	// valid same-origin login succeeds and the one-time login cookie is cleared
	resp := c.rawPost("/admin/login", withToken(c, c.csrf), map[string]string{"Origin": e.srv.URL, "Sec-Fetch-Site": "same-origin"})
	if resp.StatusCode != http.StatusSeeOther || !hasSessionCookie(resp) {
		t.Fatalf("valid login: %d", resp.StatusCode)
	}
	cleared := false
	for _, ck := range resp.Cookies() {
		if ck.Name == "svngo_login_csrf" && ck.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("login CSRF cookie not cleared after login")
	}

	// authenticated POST: valid session token is still refused for cross-site requests
	c.get("/admin/account")
	form := url.Values{"name": {"csrf-test"}, "_csrf": {c.csrf}}
	if resp := c.rawPost("/admin/groups", form, map[string]string{"Sec-Fetch-Site": "cross-site"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site POST with session token: %d", resp.StatusCode)
	}
	if resp := c.rawPost("/admin/groups", form, map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "http://sub.example"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("same-site (subdomain) POST: %d", resp.StatusCode)
	}
	if resp := c.rawPost("/admin/groups", form, map[string]string{"Sec-Fetch-Site": "same-origin"}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("same-origin POST rejected: %d", resp.StatusCode)
	}
	var n int
	e.app.db.QueryRow("SELECT COUNT(*) FROM groups WHERE name='csrf-test'").Scan(&n)
	if n != 1 {
		t.Fatalf("groups created = %d, want exactly 1", n)
	}

	// HTMX logout forces a full page load so the next session gets a fresh CSRF header
	resp = c.rawPost("/admin/logout", url.Values{"_csrf": {c.csrf}}, map[string]string{"HX-Request": "true", "Origin": e.srv.URL})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("HX-Redirect") != "/admin/login" {
		t.Fatalf("htmx logout: %d HX-Redirect=%q", resp.StatusCode, resp.Header.Get("HX-Redirect"))
	}
	if st, _ := c.get("/admin/account"); st != http.StatusSeeOther {
		t.Fatalf("session still valid after logout: %d", st)
	}
}

func TestAuthFileRollbackOnInvalidAuthz(t *testing.T) {
	requireSVN(t)
	if firstBin("svngo-authz", "svnauthz") == "" {
		t.Skip("svnauthz not installed")
	}
	e := startEnv(t, testConfig(t, t.TempDir()))
	before, _ := os.ReadFile(e.cfg.AuthzFile())
	// a validator that always fails must leave files and DB untouched
	e.app.svn.Authz = "/bin/false"
	err := e.app.mutate(context.Background(), "test", "group.create", "x", func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO groups(name, created_at) VALUES('x', 0)")
		return err
	})
	if err == nil {
		t.Fatal("expected validation failure")
	}
	after, _ := os.ReadFile(e.cfg.AuthzFile())
	if string(before) != string(after) {
		t.Fatal("authz changed despite validation failure")
	}
	var n int
	e.app.db.QueryRow("SELECT COUNT(*) FROM groups").Scan(&n)
	if n != 0 {
		t.Fatal("DB change committed despite validation failure")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
