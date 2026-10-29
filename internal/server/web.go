package server

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"svngo/internal/i18n"
	"svngo/internal/passwd"
	"svngo/internal/repopath"
	"svngo/internal/store"
	"svngo/internal/validate"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// templates keeps the parsed pages as never-executed prototypes and clones
// them per language with that language's translation functions bound, so
// {{t "key"}} works everywhere, including partials invoked with other data.
type templates struct {
	proto map[string]*template.Template
	reg   *i18n.Registry

	mu    sync.RWMutex
	cache map[string]*template.Template // lang + "/" + page
}

var tmplFuncs = template.FuncMap{
	"size":       humanSize,
	"dt":         fmtTime,
	"unix":       func(n int64) time.Time { return time.Unix(n, 0) },
	"rpath":      escapeRepoPath,
	"add":        func(a, b int) int { return a + b },
	"sub":        func(a, b int) int { return a - b },
	"add64":      func(a, b int64) int64 { return a + b },
	"sub64":      func(a, b int64) int64 { return a - b },
	"parent":     repopath.Parent,
	"activeJobs": hasActiveJobs,
	"crumbs":     breadcrumbs,
	"firstLine": func(s string) string {
		l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
		return validate.Clip(l, 120)
	},
	"basename": repopath.Base,
	"join":     strings.Join,
	"dict": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[fmt.Sprint(kv[i])] = kv[i+1]
		}
		return m
	},
	"code": func(s string) template.HTML {
		return template.HTML("<code>" + template.HTMLEscapeString(s) + "</code>")
	},
	"strong": func(s string) template.HTML {
		return template.HTML("<strong>" + template.HTMLEscapeString(s) + "</strong>")
	},
	"link": func(href, text string) template.HTML {
		return template.HTML(`<a href="` + template.HTMLEscapeString(href) + `">` + template.HTMLEscapeString(text) + "</a>")
	},
}

// langFuncs returns the translation functions bound to one language.
func langFuncs(l *i18n.Localizer) template.FuncMap {
	return template.FuncMap{
		"lang": func() string { return l.Lang },
		"t":    l.T,
		"th":   l.HTML,
		"msg":  l.Any,
		"auditLabel": func(a string) string {
			return l.TOr("audit."+a, a)
		},
		"jobKind": func(k string) string {
			return l.TOr("job.kind."+k, k)
		},
		"jobMsg": func(s string) string {
			return l.Msg(i18n.Decode(s))
		},
		"accessLabel": func(a string) string {
			switch a {
			case "rw", "r":
				return l.T("access." + a)
			}
			return l.T("access.none")
		},
		"actionLabel": func(a string) string {
			switch a {
			case "A", "D", "R":
				return l.T("change." + a)
			}
			return l.T("change.M")
		},
		"hashLabel": func(h string) string {
			if t := passwd.HashType(h); t != "apr1" {
				return t
			}
			return l.T("hash.apr1")
		},
	}
}

func loadTemplates(reg *i18n.Registry) (*templates, error) {
	funcs := template.FuncMap{}
	maps.Copy(funcs, tmplFuncs)
	maps.Copy(funcs, langFuncs(reg.For(i18n.Base)))
	base, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/_*.html")
	if err != nil {
		return nil, err
	}
	files, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	t := &templates{proto: map[string]*template.Template{}, reg: reg, cache: map[string]*template.Template{}}
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
			return nil, fmt.Errorf("template %s: %w", name, err)
		}
		t.proto[strings.TrimSuffix(name, ".html")] = c
	}
	return t, nil
}

// get returns the page bound to lang, cloning the prototype on first use.
// Translations are looked up at execution time, so reloading language files
// needs no rebuild.
func (t *templates) get(lang, page string) (*template.Template, error) {
	key := lang + "/" + page
	t.mu.RLock()
	c := t.cache[key]
	t.mu.RUnlock()
	if c != nil {
		return c, nil
	}
	p, ok := t.proto[page]
	if !ok {
		return nil, fmt.Errorf("template %s not found", page)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if c := t.cache[key]; c != nil {
		return c, nil
	}
	c, err := p.Clone()
	if err != nil {
		return nil, err
	}
	c.Funcs(langFuncs(t.reg.For(lang)))
	t.cache[key] = c
	return c, nil
}

// fragment executes a named sub-template of page in the request language.
func (a *App) fragment(w http.ResponseWriter, r *http.Request, page, name string, data any) {
	t, err := a.tmpl.get(a.lang(r), page)
	if err == nil {
		var buf bytes.Buffer
		if err = t.ExecuteTemplate(&buf, name, data); err == nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Write(buf.Bytes())
			return
		}
	}
	slog.Error("template rendering failed", "page", page, "name", name, "err", err)
	http.Error(w, a.loc(r).T("err.internal"), http.StatusInternalServerError)
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
	Title    i18n.Msg
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

// ---- language ----

const langCookie = "svngo_lang"

type langKey struct{}

// resolveLang picks the UI language: the signed-in user's choice, else (when
// signed out) the language cookie, else the browser's Accept-Language, else
// the system default.
func (a *App) resolveLang(r *http.Request, s *Session) string {
	if s != nil {
		if s.Lang != "" && a.i18n.Has(s.Lang) {
			return s.Lang
		}
	} else if c, err := r.Cookie(langCookie); err == nil && a.i18n.Has(c.Value) {
		return c.Value
	}
	if m := a.i18n.Match(r.Header.Get("Accept-Language")); m != "" {
		return m
	}
	return a.defaultLang(r.Context())
}

// defaultLang is the system default language setting (if still available).
func (a *App) defaultLang(ctx context.Context) string {
	if d := store.Setting(ctx, a.db, "default_lang"); a.i18n.Has(d) {
		return d
	}
	return i18n.Base
}

// withLang stores the resolved language in the request context.
func (a *App) withLang(r *http.Request, s *Session) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), langKey{}, a.resolveLang(r, s)))
}

// lang returns the request language (resolved by the auth middleware or from
// cookie / Accept-Language for anonymous requests).
func (a *App) lang(r *http.Request) string {
	if l, ok := r.Context().Value(langKey{}).(string); ok {
		return l
	}
	return a.resolveLang(r, nil)
}

func (a *App) loc(r *http.Request) *i18n.Localizer { return a.i18n.For(a.lang(r)) }

func (a *App) render(w http.ResponseWriter, r *http.Request, s *Session, status int, page string, v *View) {
	lang := a.lang(r)
	t, err := a.tmpl.get(lang, page)
	if err != nil {
		slog.Error("template not found", "page", page, "err", err)
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}
	v.S, v.App, v.Version = s, AppName, Version
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
		slog.Error("template rendering failed", "page", page, "err", err)
		http.Error(w, a.i18n.T(lang, "err.internal"), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Language", lang)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

// errorPage renders an error; msg may be an i18n.Msg, an error or a string.
func (a *App) errorPage(w http.ResponseWriter, r *http.Request, s *Session, status int, msg any) {
	a.render(w, r, s, status, "error", &View{Title: i18n.M("title.error"), D: map[string]any{"Status": status, "Message": msg}})
}

func (a *App) notFound(w http.ResponseWriter, r *http.Request, s *Session) {
	a.errorPage(w, r, s, http.StatusNotFound, i18n.M("err.not_found"))
}

// ---- flash messages & redirects ----

const flashCookie = "svngo_flash"

func (a *App) setFlash(w http.ResponseWriter, r *http.Request, msg string, isErr bool) {
	kind := "o"
	if isErr {
		kind = "e"
	}
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Value: kind + base64.RawURLEncoding.EncodeToString([]byte(validate.Clip(msg, 1500))),
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

// done sets a flash message in the request language and redirects (HTMX aware).
func (a *App) done(w http.ResponseWriter, r *http.Request, to string, msg i18n.Msg, err error) {
	l := a.loc(r)
	if err != nil {
		a.setFlash(w, r, l.Err(err), true)
	} else if !msg.IsZero() {
		a.setFlash(w, r, l.Msg(msg), false)
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
	mux.HandleFunc("POST /admin/account/lang", U(a.handleAccountLang))

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
	mux.HandleFunc("POST /admin/system/access", A(a.handleAllowIPSave))
	mux.HandleFunc("POST /admin/system/locales", A(a.handleLocaleUpload))
	mux.HandleFunc("POST /admin/system/locales/reload", A(a.handleLocaleReload))
	mux.HandleFunc("POST /admin/system/locales/{code}/delete", A(a.handleLocaleDelete))
	mux.HandleFunc("GET /admin/system/locales/{file}", A(a.handleLocaleDownload))

	// Rejects cross-site unsafe requests via Sec-Fetch-Site / Origin before any
	// handler runs, in addition to the per-session CSRF tokens.
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slog.Warn("cross-origin request blocked", "method", r.Method, "path", r.URL.Path, "ip", clientIP(r),
			"origin", validate.Clip(r.Header.Get("Origin"), 200), "sec_fetch_site", r.Header.Get("Sec-Fetch-Site"))
		a.errorPage(w, r, nil, http.StatusForbidden, i18n.M("err.cross_origin"))
	}))
	return a.securityHeaders(a.adminACL(cop.Handler(mux)))
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
	if v := strings.TrimRight(store.Setting(r.Context(), a.db, "svn_base_url"), "/"); v != "" {
		return v
	}
	scheme := "http"
	if a.isSecure(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/svn"
}

func (a *App) pageSize(r *http.Request) int {
	return store.SettingInt(r.Context(), a.db, "page_size", 10, 200)
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
