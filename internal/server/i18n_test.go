package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"svngo/internal/i18n"
	"svngo/internal/store"
)

var (
	reTmplKey    = regexp.MustCompile(`[{(]-?\s*(?:t|th)\s+"([^"]+)"`)
	reGoKey      = regexp.MustCompile(`i18n\.(?:M|E)\("([^"]+)"|i18n\.Wrap\([^,]+, "([^"]+)"|\.(?:T|TOr)\("([^"]+)"|Key: "([^"]+)"`)
	reI18nPkgKey = regexp.MustCompile(`\b(?:M|E)\("([^"]+)"|Wrap\([^,]+, "([^"]+)"`)
	reAction     = regexp.MustCompile(`"((?:login|admin|repo|user|group|rule|settings|sync|backup|job|svn|locale|allowip|location)\.[a-z]+(?:\.[a-z]+)?)"`)
)

func collect(t *testing.T, glob string, re *regexp.Regexp, line func(string) bool, keys map[string]string) {
	t.Helper()
	files, err := filepath.Glob(glob)
	if err != nil || len(files) == 0 {
		t.Fatalf("no files for %s", glob)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range strings.Split(string(b), "\n") {
			if line != nil && !line(l) {
				continue
			}
			for _, m := range re.FindAllStringSubmatch(l, -1) {
				for _, k := range m[1:] {
					if k != "" {
						keys[k] = f
					}
				}
			}
		}
	}
}

// Every key used by templates and Go code (including keys built at runtime)
// must exist in the base language, so no screen shows a raw key.
func TestTranslationKeysDefined(t *testing.T) {
	keys := map[string]string{}
	collect(t, "templates/*.html", reTmplKey, nil, keys)
	collect(t, "*.go", reGoKey, nil, keys)
	for _, dir := range []string{"../i18n", "../validate", "../repopath", "../passwd", "../ipacl"} {
		re := reGoKey
		if dir == "../i18n" {
			re = reI18nPkgKey
		}
		collect(t, dir+"/*.go", re, nil, keys)
	}
	actions := map[string]string{}
	collect(t, "*.go", reAction, func(l string) bool {
		return strings.Contains(l, "Audit(") || strings.Contains(l, "mutate(") || strings.Contains(l, "action")
	}, actions)
	for _, a := range []string{"login.admin", "login.user", "job.backup", "job.verify", "job.restore"} {
		actions[a] = "dynamic"
	}
	if len(actions) < 30 {
		t.Fatalf("found only %d audit actions", len(actions))
	}
	for a, f := range actions {
		keys["audit."+a] = f
	}
	for _, k := range []string{"access.rw", "access.r", "access.none", "change.A", "change.D", "change.R", "change.M",
		"hash.apr1", "job.kind.backup", "job.kind.verify", "job.kind.restore"} {
		keys[k] = "dynamic"
	}
	if len(keys) < 400 {
		t.Fatalf("found only %d keys; the scanner is broken", len(keys))
	}
	reg := i18n.New("")
	for k, f := range keys {
		if strings.HasSuffix(k, ".") {
			continue
		}
		if _, ok := reg.Lookup(i18n.Base, k); !ok {
			t.Errorf("key %q (used in %s) is not defined in %s.json", k, f, i18n.Base)
		}
	}
}

// ---- scenarios ----

func (c *client) cookie(name string) string {
	u, _ := url.Parse(c.e.srv.URL + "/admin/")
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

// upload posts a multipart form with the CSRF token and one file.
func (c *client) upload(path, filename, data string) (int, string, string) {
	c.e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("_csrf", c.csrf)
	fw, _ := mw.CreateFormFile("file", filename)
	fw.Write([]byte(data))
	mw.Close()
	req, _ := http.NewRequest("POST", c.e.srv.URL+path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", c.e.srv.URL)
	resp, err := c.http.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), string(b)
}

// follow loads the redirect target of a form post and returns the page and
// its alert text; it fails when the outcome is not the expected one.
func (c *client) follow(path string, st int, loc, body string, wantErr bool) (string, string) {
	c.e.t.Helper()
	if st != http.StatusSeeOther {
		c.e.t.Fatalf("POST %s: status %d body=%.300s", path, st, body)
	}
	_, page := c.get(loc)
	if isErr := strings.Contains(page, `class="alert err"`); isErr != wantErr {
		c.e.t.Fatalf("POST %s: error=%v, want %v (%s)", path, isErr, wantErr, extractAlert(page))
	}
	return page, extractAlert(page)
}

var settingsForm = url.Values{"page_size": {"30"}, "login_attempts": {"10"}, "svn_block_attempts": {"20"}, "svn_block_minutes": {"15"}}

func withValues(base url.Values, kv ...string) url.Values {
	v := url.Values{}
	for k, vs := range base {
		v[k] = append([]string(nil), vs...)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

func pageLang(page string) string {
	if m := regexp.MustCompile(`<html lang="([^"]+)"`).FindStringSubmatch(page); m != nil {
		return m[1]
	}
	return ""
}

func TestLanguageResolution(t *testing.T) {
	requireSVN(t)
	e := startEnv(t, testConfig(t, t.TempDir()))

	resp, err := e.clientLang("en-US,en;q=0.9").http.Get(e.srv.URL + "/admin/login")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Header.Get("Content-Language") != "en" || pageLang(string(b)) != "en" || !strings.Contains(string(b), "Subversion server administration") {
		t.Fatalf("English browser: Content-Language=%q lang=%q", resp.Header.Get("Content-Language"), pageLang(string(b)))
	}
	if _, page := e.clientLang("ko-KR,ko;q=0.9,en;q=0.8").get("/admin/login"); pageLang(page) != "ko" || !strings.Contains(page, "Subversion 서버 관리") {
		t.Fatalf("Korean browser got %q", pageLang(page))
	}
	for _, h := range []string{"ja", "", "fr-CA,fr;q=0.9"} {
		if _, page := e.clientLang(h).get("/admin/login"); pageLang(page) != "en" {
			t.Fatalf("Accept-Language %q should use the default language en, got %q", h, pageLang(page))
		}
	}

	// errors and flash messages follow the browser language
	en := e.clientLang("en")
	en.login("admin", "Admin-Pass-1")
	if st, page := en.get("/admin/repos/nope"); st != http.StatusNotFound || !strings.Contains(page, "The requested item was not found") {
		t.Fatalf("English 404 page: %d", st)
	}
	if msg := en.postErr("/admin/repos", url.Values{"name": {"../etc"}}); !strings.Contains(msg, "Repository names must start") {
		t.Fatalf("English validation error: %q", msg)
	}
	admin := e.client()
	admin.login("admin", "Admin-Pass-1")
	if st, page := admin.get("/admin/repos/nope"); st != http.StatusNotFound || !strings.Contains(page, "요청한 항목을 찾을 수 없거나") {
		t.Fatalf("Korean 404 page: %d", st)
	}

	// system default language
	admin.postOK("/admin/system/settings", withValues(settingsForm, "default_lang", "ko"))
	if _, page := e.clientLang("ja").get("/admin/login"); pageLang(page) != "ko" {
		t.Fatalf("unsupported browser language should use the new default ko, got %q", pageLang(page))
	}
	if msg := admin.postErr("/admin/system/settings", withValues(settingsForm, "default_lang", "zz")); !strings.Contains(msg, "알 수 없는 언어") {
		t.Fatalf("unknown default language accepted: %q", msg)
	}
	admin.postOK("/admin/system/settings", withValues(settingsForm)) // a form without default_lang keeps it
	if got := store.Setting(t.Context(), e.app.db, "default_lang"); got != "ko" {
		t.Fatalf("default_lang = %q", got)
	}
	if _, page := admin.get("/admin/system"); !strings.Contains(page, `<option value="ko" selected>`) {
		t.Fatal("default language not selected on the system page")
	}
}

func TestAccountLanguage(t *testing.T) {
	requireSVN(t)
	e := startEnv(t, testConfig(t, t.TempDir()))

	c := e.clientLang("en")
	c.login("admin", "Admin-Pass-1")
	if _, page := c.get("/admin/account"); pageLang(page) != "en" || !strings.Contains(page, "My account") {
		t.Fatal("account page not in the browser language")
	}
	page := c.postOK("/admin/account/lang", url.Values{"lang": {"ko"}})
	if pageLang(page) != "ko" || !strings.Contains(page, "언어를 저장했습니다") {
		t.Fatalf("saved language not applied: %q / %q", pageLang(page), extractAlert(page))
	}
	var saved string
	e.app.db.QueryRow("SELECT lang FROM admins WHERE username='admin'").Scan(&saved)
	if saved != "ko" || c.cookie(langCookie) != "ko" {
		t.Fatalf("lang column=%q cookie=%q", saved, c.cookie(langCookie))
	}
	if _, page := c.get("/admin/repos"); pageLang(page) != "ko" {
		t.Fatal("saved language should win over the browser language")
	}
	if _, page := c.get("/admin/account"); !strings.Contains(page, `<option value="ko" selected>`) {
		t.Fatal("saved language not selected on the account page")
	}

	// the cookie keeps the login page in the chosen language after logout,
	// and logging in from another browser restores it
	c.post("/admin/logout", nil)
	if _, page := c.get("/admin/login"); pageLang(page) != "ko" {
		t.Fatalf("login page after logout: %q", pageLang(page))
	}
	other := e.clientLang("en")
	other.login("admin", "Admin-Pass-1")
	if other.cookie(langCookie) != "ko" {
		t.Fatal("language cookie not set on login")
	}

	// back to automatic
	page = other.postOK("/admin/account/lang", url.Values{"lang": {""}})
	if pageLang(page) != "en" || other.cookie(langCookie) != "" {
		t.Fatalf("automatic language: lang=%q cookie=%q", pageLang(page), other.cookie(langCookie))
	}
	if msg := other.postErr("/admin/account/lang", url.Values{"lang": {"zz"}}); !strings.Contains(msg, "Unknown language") {
		t.Fatalf("unknown language accepted: %q", msg)
	}

	// SVN users choose their language too
	admin := e.client()
	admin.login("admin", "Admin-Pass-1")
	admin.postOK("/admin/users", url.Values{"username": {"carol"}, "password": {"Carol-Pass-1"}, "password2": {"Carol-Pass-1"}})
	carol := e.client()
	carol.login("carol", "Carol-Pass-1")
	if page := carol.postOK("/admin/account/lang", url.Values{"lang": {"en"}}); pageLang(page) != "en" {
		t.Fatal("SVN user language not applied")
	}
	e.app.db.QueryRow("SELECT lang FROM users WHERE username='carol'").Scan(&saved)
	if saved != "en" {
		t.Fatalf("users.lang = %q", saved)
	}
}

func TestLocaleFiles(t *testing.T) {
	requireSVN(t)
	cfg := testConfig(t, t.TempDir())
	e := startEnv(t, cfg)
	admin := e.client()
	admin.login("admin", "Admin-Pass-1")
	ja := e.clientLang("ja,en;q=0.5")

	// upload a partial translation; the uploaded text must stay escaped
	file := `{"_meta":{"code":"ja","name":"日本語"},"login.submit":"ログイン","login.subtitle":"<script>alert(1)</script>"}`
	st, loc, body := admin.upload("/admin/system/locales", "ja.json", file)
	if _, msg := admin.follow("/admin/system/locales", st, loc, body, false); !strings.Contains(msg, "언어 파일을 등록했습니다") {
		t.Fatalf("upload flash: %q", msg)
	}
	if _, err := os.Stat(filepath.Join(cfg.LocalesDir(), "ja.json")); err != nil {
		t.Fatal("uploaded file not stored:", err)
	}
	_, page := ja.get("/admin/login")
	if pageLang(page) != "ja" || !strings.Contains(page, "ログイン") || !strings.Contains(page, "Username") {
		t.Fatalf("uploaded language not used (lang=%q)", pageLang(page))
	}
	if strings.Contains(page, "<script>alert(1)") || !strings.Contains(page, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatal("translation text not escaped")
	}
	if _, page := admin.get("/admin/system"); !strings.Contains(page, "日本語") || !strings.Contains(page, `/admin/system/locales/ja/delete`) {
		t.Fatal("uploaded language not listed")
	}
	if _, page := admin.get("/admin/system/logs"); !strings.Contains(page, "언어 파일 등록") {
		t.Fatal("upload not audited")
	}

	// download a complete file for translators
	st, raw := admin.get("/admin/system/locales/ja.json")
	var exp map[string]any
	if st != http.StatusOK || json.Unmarshal([]byte(raw), &exp) != nil {
		t.Fatalf("download: %d", st)
	}
	if exp["login.submit"] != "ログイン" || exp["nav.users"] != "Users" || len(exp) < 400 {
		t.Fatalf("download incomplete: %d keys", len(exp))
	}
	if st, _ := admin.get("/admin/system/locales/zz.json"); st != http.StatusNotFound {
		t.Fatalf("unknown language download: %d", st)
	}
	if st, _ := e.clientLang("en").get("/admin/system/locales/ja.json"); st != http.StatusSeeOther {
		t.Fatalf("anonymous download: %d", st)
	}

	// invalid files are rejected with a translated reason
	for name, c := range map[string]struct{ file, data, want string }{
		"json": {"bad.json", `{oops`, "JSON"},
		"code": {"x.json", `{"_meta":{"code":"../x","name":"X"}}`, "언어 코드"},
		"name": {"xx.json", `{"_meta":{"code":"xx"}}`, "_meta.name"},
	} {
		st, loc, body := admin.upload("/admin/system/locales", c.file, c.data)
		if _, msg := admin.follow("/admin/system/locales", st, loc, body, true); !strings.Contains(msg, c.want) {
			t.Errorf("%s: %q", name, msg)
		}
	}

	// delete
	admin.postOK("/admin/system/locales/ja/delete", nil)
	if _, page := ja.get("/admin/login"); pageLang(page) != "en" {
		t.Fatalf("deleted language still used: %q", pageLang(page))
	}
	if msg := admin.postErr("/admin/system/locales/ja/delete", nil); !strings.Contains(msg, "등록된 언어 파일이 아닙니다") {
		t.Fatalf("second delete: %q", msg)
	}

	// files copied into the folder are picked up by "reload"
	os.WriteFile(filepath.Join(cfg.LocalesDir(), "fr.json"), []byte(`{"_meta":{"code":"fr","name":"Français"},"login.submit":"Connexion"}`), 0o644)
	os.WriteFile(filepath.Join(cfg.LocalesDir(), "broken.json"), []byte(`{`), 0o644)
	fr := e.clientLang("fr-FR,fr;q=0.9")
	if _, page := fr.get("/admin/login"); pageLang(page) != "en" {
		t.Fatal("folder file used before reload")
	}
	admin.postOK("/admin/system/locales/reload", nil)
	if _, page := fr.get("/admin/login"); pageLang(page) != "fr" || !strings.Contains(page, "Connexion") {
		t.Fatalf("reloaded language not used: %q", pageLang(page))
	}
	if _, page := admin.get("/admin/system"); !strings.Contains(page, "broken.json") {
		t.Fatal("broken language file not reported")
	}
}

func TestJobMessagesLocalized(t *testing.T) {
	requireSVN(t)
	e := startEnv(t, testConfig(t, t.TempDir()))
	for _, msg := range []string{i18n.Encode(i18n.M("job.verify_ok", 5)), "legacy text"} {
		if _, err := e.app.db.Exec(`INSERT INTO jobs(kind, repo, actor, status, message, started_at, finished_at)
			VALUES('verify','proj','admin','ok',?,?,?)`, msg, store.Now(), store.Now()); err != nil {
			t.Fatal(err)
		}
	}
	ko := e.client()
	ko.login("admin", "Admin-Pass-1")
	if _, page := ko.get("/admin/system/backups"); !strings.Contains(page, "r0 ~ r5 검증 성공") || !strings.Contains(page, "legacy text") || !strings.Contains(page, "검증") {
		t.Fatal("Korean job messages")
	}
	en := e.clientLang("en")
	en.login("admin", "Admin-Pass-1")
	if _, page := en.get("/admin/system/jobs"); !strings.Contains(page, "r0 – r5 verified") || !strings.Contains(page, "Verify") {
		t.Fatal("English job fragment")
	}
}
