//go:build e2e

// End-to-end tests against the real container (Apache + mod_dav_svn + SvnGo).
//
//	docker compose -p svngo-e2e -f docker-compose.yml -f e2e/compose.e2e.yml up -d --build
//	go test -tags e2e -count=1 -v ./e2e
//
// Every SVN step uses the real svn client over HTTP; nothing is accepted
// just because the web UI reported success.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var (
	baseURL   = strings.TrimRight(env("SVNGO_URL", "http://127.0.0.1:18080"), "/")
	adminUser = env("SVNGO_ADMIN_USER", "admin")
	adminPass = env("SVNGO_ADMIN_PASSWORD", "Adm1n-Test-Pass!")
	// docker compose arguments used for the restart test (run from the repo root)
	composeArgs = strings.Fields(env("SVNGO_COMPOSE_ARGS", "-p svngo-e2e -f docker-compose.yml -f e2e/compose.e2e.yml"))
)

// ---- web client ----

type web struct {
	t    *testing.T
	c    *http.Client
	csrf string
}

var reCSRF = regexp.MustCompile(`name="_csrf" value="([^"]+)"`)
var reAlertErr = regexp.MustCompile(`(?s)class="alert err"[^>]*>(.*?)</div>`)

// koTransport asks for the Korean UI, which the page assertions use.
type koTransport struct{}

func (koTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Accept-Language", "ko")
	return http.DefaultTransport.RoundTrip(r)
}

func newWeb(t *testing.T) *web {
	jar, _ := cookiejar.New(nil)
	return &web{t: t, c: &http.Client{Jar: jar, Timeout: 60 * time.Second, Transport: koTransport{},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (w *web) get(path string) (int, string) {
	w.t.Helper()
	resp, err := w.c.Get(baseURL + path)
	if err != nil {
		w.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if m := reCSRF.FindSubmatch(b); m != nil {
		w.csrf = string(m[1])
	}
	return resp.StatusCode, string(b)
}

func (w *web) post(path string, form url.Values) (int, string) {
	w.t.Helper()
	st, loc, _ := w.send(path, form)
	return st, loc
}

func (w *web) postBody(path string, form url.Values) (int, string) {
	w.t.Helper()
	st, _, body := w.send(path, form)
	return st, body
}

func (w *web) send(path string, form url.Values) (int, string, string) {
	w.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	if w.csrf != "" {
		form.Set("_csrf", w.csrf)
	}
	req, _ := http.NewRequest("POST", baseURL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", baseURL)
	resp, err := w.c.Do(req)
	if err != nil {
		w.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), string(b)
}

// ok posts a form and fails unless the app redirected without an error flash.
func (w *web) ok(path string, form url.Values) {
	w.t.Helper()
	st, loc := w.post(path, form)
	if st != http.StatusSeeOther {
		w.t.Fatalf("POST %s: status %d", path, st)
	}
	if _, page := w.get(loc); reAlertErr.MatchString(page) {
		w.t.Fatalf("POST %s failed: %s", path, strings.TrimSpace(reAlertErr.FindStringSubmatch(page)[1]))
	}
}

func (w *web) login(user, pass string) int {
	w.t.Helper()
	w.get("/admin/login")
	st, _ := w.post("/admin/login", url.Values{"username": {user}, "password": {pass}, "next": {"/admin/"}})
	if st == http.StatusSeeOther {
		w.get("/admin/account")
	}
	return st
}

type apacheInfo struct {
	PID      int  `json:"pid"`
	Restarts int  `json:"restarts"`
	Running  bool `json:"running"`
}

func (w *web) apache() apacheInfo {
	w.t.Helper()
	_, body := w.get("/admin/api/status")
	var st struct {
		Apache apacheInfo `json:"apache"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil || !st.Apache.Running || st.Apache.PID == 0 {
		w.t.Fatalf("bad status %q: %v", body, err)
	}
	return st.Apache
}

// ---- svn client ----

type svnClient struct {
	t      *testing.T
	cfgDir string
}

func newSVN(t *testing.T) *svnClient {
	if _, err := exec.LookPath("svn"); err != nil {
		t.Skip("svn client not installed")
	}
	return &svnClient{t: t, cfgDir: t.TempDir()}
}

// run executes svn as user (empty = anonymous); the password goes through stdin.
func (s *svnClient) run(dir, user, pass string, args ...string) (string, error) {
	full := []string{"--non-interactive", "--no-auth-cache", "--config-dir", s.cfgDir}
	if user != "" {
		full = append(full, "--username", user, "--password-from-stdin")
	}
	cmd := exec.Command("svn", append(full, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LC_ALL=C.UTF-8", "LANGUAGE=C")
	cmd.Stdin = strings.NewReader(pass + "\n")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func (s *svnClient) must(dir, user, pass string, args ...string) string {
	s.t.Helper()
	out, err := s.run(dir, user, pass, args...)
	if err != nil {
		s.t.Fatalf("svn %v as %q failed: %v\n%s", args, user, err, out)
	}
	return out
}

// denied asserts the command fails with an authentication/authorization error.
func (s *svnClient) denied(dir, user, pass string, args ...string) {
	s.t.Helper()
	out, err := s.run(dir, user, pass, args...)
	if err == nil {
		s.t.Fatalf("svn %v as %q unexpectedly succeeded:\n%s", args, user, out)
	}
	if !regexp.MustCompile(`E170001|E215004|E175013|E170013|403|401|[Aa]uthori[sz]ation failed|Access denied|forbidden`).MatchString(out) {
		s.t.Fatalf("svn %v as %q failed for an unexpected reason:\n%s", args, user, out)
	}
}

func waitHealthy(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(baseURL + "/admin/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("SvnGo at %s not healthy after %s", baseURL, timeout)
}

var reRuleDelete = regexp.MustCompile(`action="/admin/rules/(\d+)/delete"`)

func TestHTTPSVN(t *testing.T) {
	waitHealthy(t, 60*time.Second)
	svn := newSVN(t)
	tmp := t.TempDir()
	stamp := time.Now().Format("150405")
	repo := "e2e-" + stamp
	alice, bob := "alice"+stamp, "bob"+stamp
	alicePw, aliceNewPw, bobPw := "Alice-Pass-1", "Alice-New-Pass-2", "Bob-Pass-123"
	repoURL := baseURL + "/svn/" + repo

	step := func(name string) { t.Logf("✔ %s", name) }

	// admin login
	admin := newWeb(t)
	if st := admin.login(adminUser, "wrong-password"); st != http.StatusUnauthorized {
		t.Fatalf("wrong admin password: %d", st)
	}
	if st := admin.login(adminUser, adminPass); st != http.StatusSeeOther {
		t.Fatalf("admin login: %d", st)
	}
	before := admin.apache()
	step("관리자 로그인")

	// 1. repository creation
	admin.ok("/admin/repos", url.Values{"name": {repo}, "description": {"e2e"}, "layout": {"1"}})
	step("1. 저장소 생성")

	// 2. user creation
	admin.ok("/admin/users", url.Values{"username": {alice}, "password": {alicePw}, "password2": {alicePw}})
	admin.ok("/admin/users", url.Values{"username": {bob}, "password": {bobPw}, "password2": {bobPw}})
	step("2. 사용자 생성")

	// unauthenticated and not-yet-authorized access is blocked
	svn.denied(tmp, "", "", "ls", repoURL)
	svn.denied(tmp, alice, alicePw, "ls", repoURL)
	admin.ok("/admin/repos/"+repo+"/perms", url.Values{"path": {"/"}, "subject": {"user:" + alice}, "access": {"rw"}, "back": {"/admin/repos/" + repo + "/perms"}})

	// 3. HTTP checkout
	wc := filepath.Join(tmp, "wc")
	svn.must(tmp, alice, alicePw, "checkout", "-q", repoURL+"/trunk", wc)
	step("3. HTTP checkout")

	// 10. a repository added at runtime is served without restarting Apache
	if now := admin.apache(); now.PID != before.PID || now.Restarts != before.Restarts {
		t.Fatalf("Apache restarted: before=%+v now=%+v", before, now)
	}
	step("10. Apache 재시작 없이 신규 저장소 접근")

	// 4. HTTP commit (incl. a unicode file name)
	os.WriteFile(filepath.Join(wc, "readme.txt"), []byte("hello over http\n"), 0o644)
	os.WriteFile(filepath.Join(wc, "한글.txt"), []byte("유니코드\n"), 0o644)
	svn.must(wc, "", "", "add", "-q", "readme.txt", "한글.txt")
	out := svn.must(wc, alice, alicePw, "commit", "-m", "e2e 첫 커밋")
	if !strings.Contains(out, "Committed revision 2") {
		t.Fatalf("unexpected commit output: %s", out)
	}
	step("4. HTTP commit")

	// 5. HTTP update in a second working copy
	wc2 := filepath.Join(tmp, "wc2")
	svn.must(tmp, alice, alicePw, "checkout", "-q", repoURL+"/trunk", wc2)
	os.WriteFile(filepath.Join(wc, "readme.txt"), []byte("hello over http\nsecond line\n"), 0o644)
	svn.must(wc, alice, alicePw, "commit", "-q", "-m", "e2e 두번째 커밋")
	svn.must(wc2, alice, alicePw, "update", "-q")
	if b, _ := os.ReadFile(filepath.Join(wc2, "readme.txt")); string(b) != "hello over http\nsecond line\n" {
		t.Fatalf("update did not bring new content: %q", b)
	}
	step("5. HTTP update")

	// 11. web UI shows files and commit history
	if _, page := admin.get("/admin/repos/" + repo + "/commits"); !strings.Contains(page, "e2e 첫 커밋") || !strings.Contains(page, "e2e 두번째 커밋") || !strings.Contains(page, alice) {
		t.Fatal("commit history not shown in web UI")
	}
	if _, page := admin.get("/admin/repos/" + repo + "/files/trunk"); !strings.Contains(page, "readme.txt") || !strings.Contains(page, "한글.txt") {
		t.Fatal("files not shown in web UI")
	}
	if _, page := admin.get("/admin/repos/" + repo + "/commits/3"); !strings.Contains(page, "&#43;second line") {
		t.Fatal("diff not shown in web UI")
	}
	step("11. 웹 UI 커밋 이력/파일 표시")

	// 9. unauthorized user is blocked (read and write)
	svn.denied(tmp, bob, bobPw, "ls", repoURL+"/trunk")
	svn.denied(tmp, bob, bobPw, "cat", repoURL+"/trunk/readme.txt")
	svn.denied(tmp, alice, "wrong-password", "ls", repoURL)
	step("9. 권한 없는 사용자 차단")

	// 6/7. password change takes effect immediately; the old password is rejected
	admin.ok("/admin/users/"+alice+"/password", url.Values{"password": {aliceNewPw}, "password2": {aliceNewPw}})
	svn.denied(tmp, alice, alicePw, "ls", repoURL)
	svn.must(tmp, alice, aliceNewPw, "ls", repoURL)
	step("6. 비밀번호 변경")
	step("7. 기존 비밀번호 거부")

	// 8. permission change: rw -> r blocks commits, removal blocks reads
	admin.ok("/admin/repos/"+repo+"/perms", url.Values{"path": {"/"}, "subject": {"user:" + alice}, "access": {"r"}, "back": {"/admin/repos/" + repo + "/perms"}})
	os.WriteFile(filepath.Join(wc, "readme.txt"), []byte("blocked\n"), 0o644)
	svn.denied(wc, alice, aliceNewPw, "commit", "-m", "should fail")
	svn.must(wc, "", "", "revert", "-q", "readme.txt")
	svn.must(tmp, alice, aliceNewPw, "cat", repoURL+"/trunk/readme.txt")

	// directory-level deny
	admin.ok("/admin/repos/"+repo+"/perms", url.Values{"path": {"/trunk"}, "subject": {"user:" + alice}, "access": {"none"}, "back": {"/admin/repos/" + repo + "/perms"}})
	svn.denied(tmp, alice, aliceNewPw, "cat", repoURL+"/trunk/readme.txt")
	svn.must(tmp, alice, aliceNewPw, "ls", repoURL+"/branches")

	// remove all of alice's rules
	_, page := admin.get("/admin/repos/" + repo + "/perms")
	ids := reRuleDelete.FindAllStringSubmatch(page, -1)
	if len(ids) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(ids))
	}
	for _, m := range ids {
		admin.ok("/admin/rules/"+m[1]+"/delete", url.Values{"back": {"/admin/repos/" + repo + "/perms"}})
	}
	svn.denied(tmp, alice, aliceNewPw, "ls", repoURL)

	// group grant for bob (read-only)
	group := "g" + stamp
	admin.ok("/admin/groups", url.Values{"name": {group}})
	admin.ok("/admin/groups/"+group+"/members", url.Values{"users": {bob}})
	admin.ok("/admin/repos/"+repo+"/perms", url.Values{"path": {"/trunk"}, "subject": {"group:" + group}, "access": {"r"}, "back": {"/admin/repos/" + repo + "/perms"}})
	if got := svn.must(tmp, bob, bobPw, "cat", repoURL+"/trunk/readme.txt"); got != "hello over http\nsecond line\n" {
		t.Fatalf("bob cat = %q", got)
	}
	step("8. 권한 변경 (rw→r, 디렉터리 거부, 삭제, 그룹 부여)")

	// disabled user / disabled repository
	admin.ok("/admin/users/"+bob+"/toggle", nil)
	svn.denied(tmp, bob, bobPw, "ls", repoURL+"/trunk")
	admin.ok("/admin/users/"+bob+"/toggle", nil)
	admin.ok("/admin/repos/"+repo+"/toggle", nil)
	svn.denied(tmp, bob, bobPw, "ls", repoURL+"/trunk")
	admin.ok("/admin/repos/"+repo+"/toggle", nil)
	svn.must(tmp, bob, bobPw, "ls", repoURL+"/trunk")

	if now := admin.apache(); now.PID != before.PID || now.Restarts != before.Restarts {
		t.Fatalf("Apache restarted during changes: before=%+v now=%+v", before, now)
	}
	if st, body := admin.postBody("/admin/system/sync", nil); st != http.StatusOK || !strings.Contains(body, "모든 검증을 통과했습니다") {
		t.Fatalf("sync check failed: status %d", st)
	}
	step("사용자/저장소 비활성화, 동기화 검증, Apache 무재시작 확인")

	// 12. container restart keeps repositories, users and permissions
	if os.Getenv("SVNGO_E2E_SKIP_RESTART") == "" {
		cmd := exec.Command("docker", append(append([]string{"compose"}, composeArgs...), "restart")...)
		cmd.Dir = ".."
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("docker compose restart: %v\n%s", err, out)
		}
		time.Sleep(2 * time.Second)
		waitHealthy(t, 90*time.Second)

		if got := svn.must(tmp, bob, bobPw, "cat", repoURL+"/trunk/readme.txt"); got != "hello over http\nsecond line\n" {
			t.Fatalf("content after restart = %q", got)
		}
		svn.denied(tmp, alice, aliceNewPw, "ls", repoURL)
		svn.must(wc2, bob, bobPw, "log", "-q", "-l", "1")
		admin2 := newWeb(t)
		if st := admin2.login(adminUser, adminPass); st != http.StatusSeeOther {
			t.Fatalf("admin login after restart: %d", st)
		}
		if _, page := admin2.get("/admin/repos/" + repo + "/commits"); !strings.Contains(page, "e2e 두번째 커밋") {
			t.Fatal("commit history missing after restart")
		}
		u := newWeb(t)
		if st := u.login(alice, alicePw); st != http.StatusUnauthorized {
			t.Fatalf("old password accepted after restart: %d", st)
		}
		if st := u.login(alice, aliceNewPw); st != http.StatusSeeOther {
			t.Fatalf("new password rejected after restart: %d", st)
		}
		step("12. Docker 재시작 후 데이터 유지")
	}

	admin3 := newWeb(t)
	admin3.login(adminUser, adminPass)

	// SVN brute-force guard: repeated wrong passwords block the client IP before
	// authentication (even the correct password gets 429), without restarting Apache.
	settings := func(attempts string) url.Values {
		return url.Values{"svn_base_url": {""}, "page_size": {"30"}, "login_attempts": {"10"}, "default_layout": {"1"},
			"svn_block_attempts": {attempts}, "svn_block_minutes": {"15"}}
	}
	t.Cleanup(func() {
		w := newWeb(t)
		if w.login(adminUser, adminPass) == http.StatusSeeOther {
			w.post("/admin/system/settings", settings("20"))
		}
	})
	admin3.ok("/admin/system/settings", settings("3"))
	apBefore := admin3.apache()
	blocked := false
	for i := 0; i < 10 && !blocked; i++ {
		out, err := svn.run(tmp, bob, "wrong-password-"+stamp, "ls", repoURL+"/trunk")
		if err == nil {
			t.Fatal("wrong password accepted")
		}
		blocked = strings.Contains(out, "429")
	}
	if !blocked {
		t.Fatal("client was not blocked after repeated failures")
	}
	if out, err := svn.run(tmp, bob, bobPw, "ls", repoURL+"/trunk"); err == nil || !strings.Contains(out, "429") {
		t.Fatalf("blocked client must get 429 even with the right password: %v\n%s", err, out)
	}
	_, page = admin3.get("/admin/system")
	m := regexp.MustCompile(`name="ip" value="([^"]+)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("blocked IP not listed in system page")
	}
	admin3.ok("/admin/system/svn-unblock", url.Values{"ip": {m[1]}})
	svn.must(tmp, bob, bobPw, "ls", repoURL+"/trunk")
	if now := admin3.apache(); now.PID != apBefore.PID || now.Restarts != apBefore.Restarts {
		t.Fatalf("Apache restarted by block/unblock: %+v -> %+v", apBefore, now)
	}
	admin3.ok("/admin/system/settings", settings("20"))
	step("SVN 무차별 대입 차단(429) 및 차단 해제, Apache 무재시작")

	// cleanup: delete the test repository, users and group
	admin3.ok("/admin/repos/"+repo+"/delete", url.Values{"confirm": {repo}})
	svn.denied(tmp, bob, bobPw, "ls", repoURL)
	admin3.ok("/admin/groups/"+group+"/delete", nil)
	admin3.ok("/admin/users/"+alice+"/delete", nil)
	admin3.ok("/admin/users/"+bob+"/delete", nil)
	if st := newWeb(t).login(bob, bobPw); st != http.StatusUnauthorized {
		t.Fatalf("deleted user can still log in: %d", st)
	}
	fmt.Println("e2e data removed:", repo, alice, bob, group)
}

// TestHTTPLocations covers extra URL paths (/<location>/<repo>) served by
// generated <Location> blocks, applied with a graceful restart.
func TestHTTPLocations(t *testing.T) {
	waitHealthy(t, 60*time.Second)
	svn := newSVN(t)
	tmp := t.TempDir()
	stamp := time.Now().Format("150405")
	loc, repo, user, pw := "loc"+stamp, "lr"+stamp, "carol"+stamp, "Carol-Pass-1"
	locURL, svnURL := baseURL+"/"+loc+"/"+repo, baseURL+"/svn/"+repo
	step := func(name string) { t.Logf("✔ %s", name) }

	admin := newWeb(t)
	if st := admin.login(adminUser, adminPass); st != http.StatusSeeOther {
		t.Fatalf("admin login: %d", st)
	}
	before := admin.apache()
	t.Cleanup(func() {
		w := newWeb(t)
		if w.login(adminUser, adminPass) != http.StatusSeeOther {
			return
		}
		w.post("/admin/system/access", url.Values{"admin_allow_ips": {""}, "svn_allow_ips": {""}})
		w.post("/admin/repos/"+repo+"/delete", url.Values{"confirm": {repo}})
		w.post("/admin/system/locations/"+loc+"/delete", nil)
		w.post("/admin/users/"+user+"/delete", nil)
	})

	admin.ok("/admin/system/locations", url.Values{"name": {loc}})
	admin.ok("/admin/repos", url.Values{"name": {repo}, "location": {loc}, "layout": {"1"}})
	admin.ok("/admin/users", url.Values{"username": {user}, "password": {pw}, "password2": {pw}})
	svn.denied(tmp, "", "", "ls", locURL)
	svn.denied(tmp, user, pw, "ls", locURL)
	admin.ok("/admin/repos/"+repo+"/perms", url.Values{"path": {"/"}, "subject": {"user:" + user}, "access": {"rw"}, "back": {"/admin/repos/" + repo + "/perms"}})
	step("위치 생성, 위치 저장소 생성, 권한 부여")

	wc := filepath.Join(tmp, "wc")
	svn.must(tmp, user, pw, "checkout", "-q", locURL+"/trunk", wc)
	os.WriteFile(filepath.Join(wc, "a.txt"), []byte("location\n"), 0o644)
	svn.must(wc, "", "", "add", "-q", "a.txt")
	if out := svn.must(wc, user, pw, "commit", "-m", "loc commit"); !strings.Contains(out, "Committed revision 2") {
		t.Fatalf("commit: %s", out)
	}
	svn.must(tmp, user, pw, "copy", "-m", "branch", locURL+"/trunk", locURL+"/branches/b1")
	if got := svn.must(tmp, user, pw, "cat", locURL+"/branches/b1/a.txt"); got != "location\n" {
		t.Fatalf("branch content = %q", got)
	}
	if out, err := svn.run(tmp, user, pw, "ls", svnURL); err == nil {
		t.Fatalf("location repository also served under /svn:\n%s", out)
	}
	if _, page := admin.get("/admin/repos/" + repo); !strings.Contains(page, "svn checkout "+locURL+"/trunk") {
		t.Fatal("overview does not show the location URL")
	}
	step("위치 URL checkout/commit/서버측 copy")

	// the SVN IP allow list covers location paths but not the admin UI
	admin.ok("/admin/system/access", url.Values{"admin_allow_ips": {""}, "svn_allow_ips": {"192.0.2.1"}})
	svn.denied(tmp, user, pw, "ls", locURL)
	admin.ok("/admin/system/access", url.Values{"admin_allow_ips": {""}, "svn_allow_ips": {""}})
	svn.must(tmp, user, pw, "ls", locURL)
	step("SVN IP 허용 목록이 위치 경로에도 적용")

	// moving between /svn and the location switches the served URL
	admin.ok("/admin/repos/"+repo+"/rename", url.Values{"new_name": {repo}, "location": {""}})
	svn.must(tmp, user, pw, "ls", svnURL+"/branches/b1")
	if _, err := svn.run(tmp, user, pw, "ls", locURL); err == nil {
		t.Fatal("old location URL still served after the move")
	}
	admin.ok("/admin/repos/"+repo+"/rename", url.Values{"new_name": {repo}, "location": {loc}})
	svn.must(wc, user, pw, "update", "-q")
	step("저장소 위치 이동")

	// a location with repositories cannot be removed; an empty one can
	if st, page := admin.post("/admin/system/locations/"+loc+"/delete", nil); st != http.StatusSeeOther {
		t.Fatalf("delete non-empty location: %d", st)
	} else if _, body := admin.get(page); !reAlertErr.MatchString(body) {
		t.Fatal("non-empty location removed")
	}
	admin.ok("/admin/repos/"+repo+"/delete", url.Values{"confirm": {repo}})
	admin.ok("/admin/system/locations/"+loc+"/delete", nil)
	if out, err := svn.run(tmp, user, pw, "ls", baseURL+"/"+loc+"/"+repo); err == nil {
		t.Fatalf("removed location still served:\n%s", out)
	}
	admin.ok("/admin/users/"+user+"/delete", nil)

	if now := admin.apache(); now.PID != before.PID || now.Restarts != before.Restarts {
		t.Fatalf("Apache process replaced: before=%+v now=%+v", before, now)
	}
	if st, body := admin.postBody("/admin/system/sync", nil); st != http.StatusOK || !strings.Contains(body, "모든 검증을 통과했습니다") {
		t.Fatalf("sync check failed: status %d", st)
	}
	step("위치 삭제, Apache 무중단(graceful) 확인")
}
