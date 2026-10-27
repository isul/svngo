package main

import (
	"bytes"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type templates struct {
	pages map[string]*template.Template
}

var auditLabels = map[string]string{
	"login.admin": "관리자 로그인", "login.user": "사용자 로그인",
	"admin.create": "관리자 추가", "admin.delete": "관리자 삭제", "admin.passwd": "관리자 비밀번호 변경",
	"repo.create": "저장소 생성", "repo.update": "저장소 수정", "repo.rename": "저장소 이름 변경",
	"repo.delete": "저장소 삭제", "repo.enable": "저장소 활성화", "repo.disable": "저장소 비활성화",
	"repo.restore": "저장소 복원", "repo.scan": "저장소 스캔",
	"user.create": "사용자 생성", "user.update": "사용자 수정", "user.delete": "사용자 삭제",
	"user.enable": "사용자 활성화", "user.disable": "사용자 비활성화", "user.passwd": "비밀번호 변경",
	"user.rehash": "해시 업그레이드", "user.import": "사용자 가져오기", "user.groups": "사용자 그룹 변경",
	"group.create": "그룹 생성", "group.update": "그룹 수정", "group.delete": "그룹 삭제",
	"group.members.add": "그룹 멤버 추가", "group.members.remove": "그룹 멤버 제거",
	"rule.set": "권한 설정", "rule.update": "권한 수정", "rule.delete": "권한 삭제",
	"settings.update": "설정 변경", "sync.repair": "인증 파일 재생성",
	"backup.delete": "백업 삭제", "backup.upload": "백업 업로드",
	"job.backup": "백업 시작", "job.verify": "검증 시작", "job.restore": "복원 시작",
	"svn.block": "SVN 접속 차단", "svn.unblock": "SVN 차단 해제",
}

var tmplFuncs = template.FuncMap{
	"size":        humanSize,
	"dt":          fmtTime,
	"unix":        func(n int64) time.Time { return time.Unix(n, 0) },
	"rpath":       escapeRepoPath,
	"add":         func(a, b int) int { return a + b },
	"sub":         func(a, b int) int { return a - b },
	"add64":       func(a, b int64) int64 { return a + b },
	"sub64":       func(a, b int64) int64 { return a - b },
	"parent":      parentPath,
	"activeJobs":  hasActiveJobs,
	"accessLabel": accessLabel,
	"crumbs":      breadcrumbs,
	"firstLine": func(s string) string {
		l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
		return clip(l, 120)
	},
	"basename": baseName,
	"join":     strings.Join,
	"dict": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[fmt.Sprint(kv[i])] = kv[i+1]
		}
		return m
	},
	"auditLabel": func(a string) string {
		if l, ok := auditLabels[a]; ok {
			return l
		}
		return a
	},
	"actionLabel": func(a string) string {
		switch a {
		case "A":
			return "추가"
		case "D":
			return "삭제"
		case "R":
			return "교체"
		}
		return "수정"
	},
	"hashLabel": func(h string) string {
		if hashType(h) == "apr1" {
			return "APR1-MD5 (레거시)"
		}
		return hashType(h)
	},
}

func loadTemplates() (*templates, error) {
	base, err := template.New("").Funcs(tmplFuncs).ParseFS(templateFS, "templates/_*.html")
	if err != nil {
		return nil, err
	}
	files, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	t := &templates{pages: map[string]*template.Template{}}
	for _, f := range files {
		name := path.Base(f)
		if strings.HasPrefix(name, "_") {
			continue
		}
		c, err := base.Clone()
		if err != nil {
			return nil, err
		}
		if _, err := c.ParseFS(templateFS, f); err != nil {
			return nil, fmt.Errorf("템플릿 %s: %w", name, err)
		}
		t.pages[strings.TrimSuffix(name, ".html")] = c
	}
	return t, nil
}

func humanSize(n int64) string {
	if n < 0 {
		return "-"
	}
	if n < 1024 {
		return strconv.FormatInt(n, 10) + " B"
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	f := float64(n)
	i := -1
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if f >= 100 {
		return fmt.Sprintf("%.0f %s", math.Round(f), units[i])
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

func fmtTime(v any) string {
	var t time.Time
	switch x := v.(type) {
	case time.Time:
		t = x
	case int64:
		if x == 0 {
			return "-"
		}
		t = time.Unix(x, 0)
	default:
		return ""
	}
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// escapeRepoPath escapes each path segment for use in a URL.
func escapeRepoPath(p string) string {
	var b strings.Builder
	for _, s := range strings.Split(p, "/") {
		if s == "" {
			continue
		}
		b.WriteString("/" + url.PathEscape(s))
	}
	return b.String()
}

type crumb struct{ Name, Path string }

func breadcrumbs(p string) []crumb {
	var res []crumb
	cur := ""
	for _, s := range strings.Split(p, "/") {
		if s == "" {
			continue
		}
		cur += "/" + s
		res = append(res, crumb{s, cur})
	}
	return res
}

// View is the data passed to every page template.
type View struct {
	Title    string
	Nav      string
	Tab      string
	S        *Session
	CSRF     string
	Flash    string
	FlashErr string
	App      string
	Version  string
	Repo     *RepoRow
	D        map[string]any
}

func (a *App) render(w http.ResponseWriter, r *http.Request, s *Session, status int, page string, v *View) {
	t, ok := a.tmpl.pages[page]
	if !ok {
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}
	v.S, v.App, v.Version = s, appName, version
	if s != nil {
		v.CSRF = s.CSRF
	}
	if v.D == nil {
		v.D = map[string]any{}
	}
	if msg, isErr, ok := readFlash(w, r); ok {
		if isErr {
			v.FlashErr = msg
		} else {
			v.Flash = msg
		}
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", v); err != nil {
		slog.Error("템플릿 렌더링 실패", "page", page, "err", err)
		http.Error(w, "내부 오류", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (a *App) errorPage(w http.ResponseWriter, r *http.Request, s *Session, status int, msg string) {
	a.render(w, r, s, status, "error", &View{Title: "오류", D: map[string]any{"Status": status, "Message": msg}})
}

func (a *App) notFound(w http.ResponseWriter, r *http.Request, s *Session) {
	a.errorPage(w, r, s, http.StatusNotFound, "요청한 항목을 찾을 수 없거나 접근 권한이 없습니다.")
}

// ---- flash messages & redirects ----

const flashCookie = "svngo_flash"

func (a *App) setFlash(w http.ResponseWriter, r *http.Request, msg string, isErr bool) {
	kind := "o"
	if isErr {
		kind = "e"
	}
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Value: kind + base64.RawURLEncoding.EncodeToString([]byte(clip(msg, 1500))),
		Path: "/admin", MaxAge: 60, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.isSecure(r),
	})
}

func readFlash(w http.ResponseWriter, r *http.Request) (string, bool, bool) {
	c, err := r.Cookie(flashCookie)
	if err != nil || len(c.Value) < 2 {
		return "", false, false
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: "/admin", MaxAge: -1, HttpOnly: true})
	b, err := base64.RawURLEncoding.DecodeString(c.Value[1:])
	if err != nil {
		return "", false, false
	}
	return string(b), c.Value[0] == 'e', true
}

// done sets a flash message and redirects (HTMX aware).
func (a *App) done(w http.ResponseWriter, r *http.Request, to string, msg string, err error) {
	if err != nil {
		a.setFlash(w, r, err.Error(), true)
	} else if msg != "" {
		a.setFlash(w, r, msg, false)
	}
	a.redirect(w, r, to)
}

func (a *App) redirect(w http.ResponseWriter, r *http.Request, to string) {
	if r.Header.Get("HX-Request") == "true" {
		b, _ := json.Marshal(map[string]string{"path": to, "target": "body"})
		w.Header().Set("HX-Location", string(b))
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// ---- pagination ----

type Pager struct {
	Page, Pages, Total, Size int
	Base                     string // URL with query (without page)
}

func newPager(r *http.Request, total, size int) Pager {
	p, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pages := (total + size - 1) / size
	if pages < 1 {
		pages = 1
	}
	if p < 1 {
		p = 1
	}
	if p > pages {
		p = pages
	}
	q := r.URL.Query()
	q.Del("page")
	base := r.URL.Path + "?"
	if enc := q.Encode(); enc != "" {
		base += enc + "&"
	}
	return Pager{Page: p, Pages: pages, Total: total, Size: size, Base: base}
}

func (p Pager) Offset() int     { return (p.Page - 1) * p.Size }
func (p Pager) HasPrev() bool   { return p.Page > 1 }
func (p Pager) HasNext() bool   { return p.Page < p.Pages }
func (p Pager) PrevURL() string { return p.Base + "page=" + strconv.Itoa(p.Page-1) }
func (p Pager) NextURL() string { return p.Base + "page=" + strconv.Itoa(p.Page+1) }

// ---- routing ----

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	fileServer := http.StripPrefix("/admin/static/", http.FileServer(http.FS(static)))
	mux.Handle("GET /admin/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		fileServer.ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /admin/healthz", a.handleHealth)
	mux.HandleFunc("GET /admin/login", a.handleLoginPage)
	mux.HandleFunc("POST /admin/login", a.handleLogin)
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})

	U, A := a.requireUser, a.requireAdmin
	mux.HandleFunc("POST /admin/logout", U(a.handleLogout))
	mux.HandleFunc("GET /admin/{$}", U(a.handleHome))
	mux.HandleFunc("GET /admin/account", U(a.handleAccount))
	mux.HandleFunc("POST /admin/account/password", U(a.handleAccountPassword))

	// repositories
	mux.HandleFunc("GET /admin/repos", U(a.handleRepos))
	mux.HandleFunc("POST /admin/repos", A(a.handleRepoCreate))
	mux.HandleFunc("GET /admin/repos/{repo}", U(a.handleRepoOverview))
	mux.HandleFunc("GET /admin/repos/{repo}/files", U(a.handleBrowse))
	mux.HandleFunc("GET /admin/repos/{repo}/files/{path...}", U(a.handleBrowse))
	mux.HandleFunc("GET /admin/repos/{repo}/raw/{path...}", U(a.handleRaw))
	mux.HandleFunc("GET /admin/repos/{repo}/commits", U(a.handleCommits))
	mux.HandleFunc("GET /admin/repos/{repo}/commits/{rev}", U(a.handleCommit))
	mux.HandleFunc("GET /admin/repos/{repo}/diff/{rev}", U(a.handleRawDiff))
	mux.HandleFunc("GET /admin/repos/{repo}/perms", A(a.handleRepoPerms))
	mux.HandleFunc("POST /admin/repos/{repo}/perms", A(a.handleRuleAdd))
	mux.HandleFunc("GET /admin/repos/{repo}/access", A(a.handleAccessCheck))
	mux.HandleFunc("GET /admin/repos/{repo}/settings", A(a.handleRepoSettings))
	mux.HandleFunc("POST /admin/repos/{repo}/update", A(a.handleRepoUpdate))
	mux.HandleFunc("POST /admin/repos/{repo}/toggle", A(a.handleRepoToggle))
	mux.HandleFunc("POST /admin/repos/{repo}/rename", A(a.handleRepoRename))
	mux.HandleFunc("POST /admin/repos/{repo}/delete", A(a.handleRepoDelete))
	mux.HandleFunc("POST /admin/repos/{repo}/verify", A(a.handleRepoVerify))
	mux.HandleFunc("POST /admin/repos/{repo}/backup", A(a.handleRepoBackup))
	mux.HandleFunc("POST /admin/repos/{repo}/size", A(a.handleRepoSize))

	// permissions
	mux.HandleFunc("GET /admin/perms", A(a.handlePerms))
	mux.HandleFunc("POST /admin/perms", A(a.handleRuleAdd))
	mux.HandleFunc("POST /admin/rules/{id}/update", A(a.handleRuleUpdate))
	mux.HandleFunc("POST /admin/rules/{id}/delete", A(a.handleRuleDelete))

	// users
	mux.HandleFunc("GET /admin/users", A(a.handleUsers))
	mux.HandleFunc("POST /admin/users", A(a.handleUserCreate))
	mux.HandleFunc("POST /admin/users/import", A(a.handleUserImport))
	mux.HandleFunc("GET /admin/users/{user}", A(a.handleUserEdit))
	mux.HandleFunc("POST /admin/users/{user}", A(a.handleUserUpdate))
	mux.HandleFunc("POST /admin/users/{user}/password", A(a.handleUserPassword))
	mux.HandleFunc("POST /admin/users/{user}/reset", A(a.handleUserReset))
	mux.HandleFunc("POST /admin/users/{user}/toggle", A(a.handleUserToggle))
	mux.HandleFunc("POST /admin/users/{user}/groups", A(a.handleUserGroups))
	mux.HandleFunc("POST /admin/users/{user}/delete", A(a.handleUserDelete))

	// groups
	mux.HandleFunc("GET /admin/groups", A(a.handleGroups))
	mux.HandleFunc("POST /admin/groups", A(a.handleGroupCreate))
	mux.HandleFunc("GET /admin/groups/{group}", A(a.handleGroupEdit))
	mux.HandleFunc("POST /admin/groups/{group}", A(a.handleGroupUpdate))
	mux.HandleFunc("POST /admin/groups/{group}/members", A(a.handleGroupAddMembers))
	mux.HandleFunc("POST /admin/groups/{group}/members/{user}/remove", A(a.handleGroupRemoveMember))
	mux.HandleFunc("POST /admin/groups/{group}/delete", A(a.handleGroupDelete))

	// system
	mux.HandleFunc("GET /admin/system", A(a.handleSystem))
	mux.HandleFunc("POST /admin/system/settings", A(a.handleSettingsSave))
	mux.HandleFunc("POST /admin/system/sync", A(a.handleSyncCheck))
	mux.HandleFunc("POST /admin/system/repair", A(a.handleSyncRepair))
	mux.HandleFunc("POST /admin/system/scan", A(a.handleRepoScan))
	mux.HandleFunc("POST /admin/system/admins", A(a.handleAdminCreate))
	mux.HandleFunc("POST /admin/system/admins/{id}/delete", A(a.handleAdminDelete))
	mux.HandleFunc("GET /admin/system/backups", A(a.handleBackups))
	mux.HandleFunc("GET /admin/system/backups/{file}", A(a.handleBackupDownload))
	mux.HandleFunc("POST /admin/system/backups/{file}/delete", A(a.handleBackupDelete))
	mux.HandleFunc("POST /admin/system/restore", A(a.handleRestore))
	mux.HandleFunc("POST /admin/system/upload", a.requireAdminNoCSRF(a.handleBackupUpload))
	mux.HandleFunc("GET /admin/system/jobs", A(a.handleJobsFragment))
	mux.HandleFunc("GET /admin/system/logs", A(a.handleLogs))
	mux.HandleFunc("GET /admin/api/status", A(a.handleAPIStatus))
	mux.HandleFunc("POST /admin/system/svn-unblock", A(a.handleSvnUnblock))

	// Rejects cross-site unsafe requests via Sec-Fetch-Site / Origin before any
	// handler runs, in addition to the per-session CSRF tokens.
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slog.Warn("교차 출처 요청 차단", "method", r.Method, "path", r.URL.Path, "ip", clientIP(r),
			"origin", clip(r.Header.Get("Origin"), 200), "sec_fetch_site", r.Header.Get("Sec-Fetch-Site"))
		a.errorPage(w, r, nil, http.StatusForbidden, "잘못된 요청 출처입니다 (교차 출처 요청 차단).")
	}))
	return a.securityHeaders(cop.Handler(mux))
}

func (a *App) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
			"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), interest-cohort=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if a.isSecure(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) isSecure(r *http.Request) bool {
	switch a.cfg.CookieSecure {
	case "always":
		return true
	case "never":
		return false
	}
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// svnBaseURL returns the base URL shown to users for checkout.
func (a *App) svnBaseURL(r *http.Request) string {
	if v := strings.TrimRight(getSetting(r.Context(), a.db, "svn_base_url"), "/"); v != "" {
		return v
	}
	scheme := "http"
	if a.isSecure(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/svn"
}

func (a *App) pageSize(r *http.Request) int {
	return getSettingInt(r.Context(), a.db, "page_size", 10, 200)
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := a.db.PingContext(r.Context()); err != nil {
		http.Error(w, "db error", http.StatusServiceUnavailable)
		return
	}
	if a.apache != nil && !a.apache.Status().Running {
		http.Error(w, "apache not running", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("ok\n"))
}
