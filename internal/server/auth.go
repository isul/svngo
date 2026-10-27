package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"svngo/internal/passwd"
	"svngo/internal/store"
	"svngo/internal/validate"
)

const sessionCookie = "svngo_session"

type Session struct {
	TokenHash   string
	Kind        string // admin | user
	PrincipalID int64
	Username    string
	CSRF        string
	ExpiresAt   int64
}

func (s *Session) IsAdmin() bool { return s != nil && s.Kind == "admin" }

type handler func(w http.ResponseWriter, r *http.Request, s *Session)

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func tokenHash(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func (a *App) loadSession(r *http.Request) *Session {
	c, err := r.Cookie(sessionCookie)
	if err != nil || len(c.Value) < 20 || len(c.Value) > 100 {
		return nil
	}
	s := &Session{TokenHash: tokenHash(c.Value)}
	err = a.db.QueryRowContext(r.Context(), `SELECT kind, principal_id, username, csrf, expires_at FROM sessions
		WHERE token_hash=? AND expires_at>?`, s.TokenHash, store.Now()).
		Scan(&s.Kind, &s.PrincipalID, &s.Username, &s.CSRF, &s.ExpiresAt)
	if err != nil {
		return nil
	}
	// the principal must still exist (and be enabled for SVN users)
	var ok int
	if s.Kind == "admin" {
		err = a.db.QueryRowContext(r.Context(), "SELECT 1 FROM admins WHERE id=? AND username=?", s.PrincipalID, s.Username).Scan(&ok)
	} else {
		err = a.db.QueryRowContext(r.Context(), "SELECT 1 FROM users WHERE id=? AND username=? AND enabled=1", s.PrincipalID, s.Username).Scan(&ok)
	}
	if err != nil {
		return nil
	}
	return s
}

func (a *App) requireUser(h handler) http.HandlerFunc {
	return a.auth(h, false, true)
}

func (a *App) requireAdmin(h handler) http.HandlerFunc {
	return a.auth(h, true, true)
}

// requireAdminNoCSRF is for streaming multipart uploads which verify the
// CSRF token themselves from the first form part.
func (a *App) requireAdminNoCSRF(h handler) http.HandlerFunc {
	return a.auth(h, true, false)
}

func (a *App) auth(h handler, admin, csrf bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := a.loadSession(r)
		if s == nil {
			if r.Method == http.MethodGet && r.Header.Get("HX-Request") != "true" {
				http.Redirect(w, r, "/admin/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
				return
			}
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", "/admin/login")
			}
			http.Error(w, "로그인이 필요합니다", http.StatusUnauthorized)
			return
		}
		if admin && !s.IsAdmin() {
			a.errorPage(w, r, s, http.StatusForbidden, "관리자 권한이 필요합니다.")
			return
		}
		if r.Method == http.MethodPost {
			if !a.sameOrigin(r) {
				a.errorPage(w, r, s, http.StatusForbidden, "잘못된 요청 출처입니다 (Origin 불일치).")
				return
			}
			if csrf && !validCSRF(r, s) {
				a.errorPage(w, r, s, http.StatusForbidden, "보안 토큰(CSRF)이 유효하지 않습니다. 페이지를 새로고침한 뒤 다시 시도하세요.")
				return
			}
		}
		h(w, r, s)
	}
}

func validCSRF(r *http.Request, s *Session) bool {
	tok := r.Header.Get("X-CSRF-Token")
	if tok == "" {
		r.Body = http.MaxBytesReader(nil, r.Body, 2<<20)
		tok = r.PostFormValue("_csrf")
	}
	return tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.CSRF)) == 1
}

// sameOrigin rejects cross-site form posts when the browser sends Origin/Referer.
func (a *App) sameOrigin(r *http.Request) bool {
	src := r.Header.Get("Origin")
	if src == "" || src == "null" {
		src = r.Header.Get("Referer")
	}
	if src == "" {
		return true
	}
	u, err := url.Parse(src)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// clientIP returns the client address. Requests come through Apache on
// loopback, which appends the real peer to X-Forwarded-For.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if c := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(c) != nil {
				return c
			}
		}
	}
	return host
}

// ---- login rate limiting (in memory, no background goroutine) ----

type attempt struct {
	count        int
	first        time.Time
	blockedUntil time.Time
}

type loginLimiter struct {
	mu sync.Mutex
	m  map[string]*attempt
}

func newLoginLimiter() *loginLimiter { return &loginLimiter{m: map[string]*attempt{}} }

const limitWindow = 15 * time.Minute

func (l *loginLimiter) blocked(keys ...string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	var wait time.Duration
	for _, k := range keys {
		if at := l.m[k]; at != nil {
			if d := time.Until(at.blockedUntil); d > wait {
				wait = d
			}
		}
	}
	return wait
}

func (l *loginLimiter) fail(max int, keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := time.Now()
	if len(l.m) > 10000 {
		for k, at := range l.m {
			if t.Sub(at.first) > limitWindow && t.After(at.blockedUntil) {
				delete(l.m, k)
			}
		}
	}
	for _, k := range keys {
		at := l.m[k]
		if at == nil || t.Sub(at.first) > limitWindow {
			at = &attempt{first: t}
			l.m[k] = at
		}
		at.count++
		if at.count >= max {
			at.blockedUntil = t.Add(limitWindow)
		}
	}
}

func (l *loginLimiter) reset(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		delete(l.m, k)
	}
}

// ---- handlers ----

func safeNext(next string) string {
	if strings.HasPrefix(next, "/admin/") && !strings.HasPrefix(next, "//") && !strings.ContainsAny(next, "\\\r\n") {
		return next
	}
	return "/admin/"
}

// Login CSRF uses a double-submit token: a random value in an HttpOnly cookie
// that must match the hidden form field. On HTTPS the __Host- prefix keeps
// sibling subdomains from planting the cookie.
const loginCSRFTTL = 2 * time.Hour

func (a *App) loginCSRFCookie(r *http.Request) (name, path string) {
	if a.isSecure(r) {
		return "__Host-svngo_login_csrf", "/"
	}
	return "svngo_login_csrf", "/admin/login"
}

// loginCSRFToken returns the current login token, issuing a new cookie when
// missing. An existing token is reused so several open login tabs keep working.
func (a *App) loginCSRFToken(w http.ResponseWriter, r *http.Request) string {
	name, path := a.loginCSRFCookie(r)
	if c, err := r.Cookie(name); err == nil && len(c.Value) == 43 {
		return c.Value
	}
	tok := randomToken()
	http.SetCookie(w, &http.Cookie{Name: name, Value: tok, Path: path, MaxAge: int(loginCSRFTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: a.isSecure(r)})
	return tok
}

func (a *App) validLoginCSRF(r *http.Request) bool {
	name, _ := a.loginCSRFCookie(r)
	c, err := r.Cookie(name)
	tok := r.PostFormValue("_csrf")
	return err == nil && len(c.Value) == 43 && subtle.ConstantTimeCompare([]byte(tok), []byte(c.Value)) == 1
}

func (a *App) clearLoginCSRF(w http.ResponseWriter, r *http.Request) {
	name, path := a.loginCSRFCookie(r)
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: path, MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: a.isSecure(r)})
}

func (a *App) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s := a.loadSession(r); s != nil {
		http.Redirect(w, r, "/admin/", http.StatusSeeOther)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	a.render(w, r, nil, http.StatusOK, "login", &View{Title: "로그인",
		D: map[string]any{"Next": safeNext(r.URL.Query().Get("next")), "CSRF": a.loginCSRFToken(w, r)}})
}

// dummyHash equalizes timing for unknown users.
var dummyHash, _ = passwd.Hash("svngo-dummy-password", 10)

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	ctx := r.Context()
	user := strings.TrimSpace(r.PostFormValue("username"))
	pw := r.PostFormValue("password")
	next := safeNext(r.PostFormValue("next"))
	fail := func(status int, msg string) {
		w.Header().Set("Cache-Control", "no-store")
		a.render(w, r, nil, status, "login", &View{Title: "로그인",
			D: map[string]any{"Next": next, "Error": msg, "Username": validate.Clip(user, 64), "CSRF": a.loginCSRFToken(w, r)}})
	}
	if !a.sameOrigin(r) {
		fail(http.StatusForbidden, "잘못된 요청 출처입니다.")
		return
	}
	if !a.validLoginCSRF(r) {
		fail(http.StatusForbidden, "보안 토큰(CSRF)이 없거나 만료되었습니다. 다시 로그인하세요.")
		return
	}
	ip := clientIP(r)
	keys := []string{"ip:" + ip, "user:" + strings.ToLower(validate.Clip(user, 64))}
	if wait := a.limiter.blocked(keys...); wait > 0 {
		fail(http.StatusTooManyRequests, fmt.Sprintf("로그인 시도가 너무 많습니다. %d분 후 다시 시도하세요.", int(wait.Minutes())+1))
		return
	}
	if validate.Username(user) != nil || pw == "" || len(pw) > 72 {
		a.limiter.fail(store.SettingInt(ctx, a.db, "login_attempts", 3, 100), keys...)
		fail(http.StatusUnauthorized, "아이디 또는 비밀번호가 올바르지 않습니다.")
		return
	}

	kind, id, ok := a.checkCredentials(ctx, user, pw)
	if !ok {
		a.limiter.fail(store.SettingInt(ctx, a.db, "login_attempts", 3, 100), keys...)
		slog.Warn("로그인 실패", "user", user, "ip", ip)
		fail(http.StatusUnauthorized, "아이디 또는 비밀번호가 올바르지 않습니다.")
		return
	}
	a.limiter.reset(keys...)

	if old := a.loadSession(r); old != nil {
		a.db.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash=?", old.TokenHash)
	}
	token := randomToken()
	exp := time.Now().Add(a.cfg.SessionTTL)
	a.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at<?", store.Now())
	if _, err := a.db.ExecContext(ctx, `INSERT INTO sessions(token_hash, kind, principal_id, username, csrf, created_at, expires_at)
		VALUES(?,?,?,?,?,?,?)`, tokenHash(token), kind, id, user, randomToken(), store.Now(), exp.Unix()); err != nil {
		fail(http.StatusInternalServerError, "세션 생성 실패")
		return
	}
	if kind == "admin" {
		a.db.ExecContext(ctx, "UPDATE admins SET last_login=? WHERE id=?", store.Now(), id)
	}
	store.Audit(ctx, a.db, user, "login."+kind, ip)
	a.clearLoginCSRF(w, r)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/admin", Expires: exp,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.isSecure(r)})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// checkCredentials tries web admins first, then SVN users. Legacy APR1 hashes
// of SVN users are transparently upgraded to bcrypt on successful login.
func (a *App) checkCredentials(ctx context.Context, user, pw string) (string, int64, bool) {
	var id int64
	var hash string
	if err := a.db.QueryRowContext(ctx, "SELECT id, password_hash FROM admins WHERE username=?", user).Scan(&id, &hash); err == nil {
		if passwd.Verify(hash, pw) {
			return "admin", id, true
		}
	}
	err := a.db.QueryRowContext(ctx, "SELECT id, password_hash FROM users WHERE username=? AND enabled=1", user).Scan(&id, &hash)
	if err != nil {
		passwd.Verify(dummyHash, pw)
		return "", 0, false
	}
	if !passwd.Verify(hash, pw) {
		return "", 0, false
	}
	if passwd.HashType(hash) == "apr1" {
		if nh, err := passwd.Hash(pw, a.cfg.BcryptCost); err == nil {
			err = a.mutate(ctx, user, "user.rehash", user+" APR1 -> bcrypt", func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, "UPDATE users SET password_hash=?, updated_at=? WHERE id=? AND password_hash=?", nh, store.Now(), id, hash)
				return err
			})
			if err != nil {
				slog.Error("레거시 해시 갱신 실패", "user", user, "err", err)
			}
		}
	}
	return "user", id, true
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request, s *Session) {
	a.db.ExecContext(r.Context(), "DELETE FROM sessions WHERE token_hash=?", s.TokenHash)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/admin", MaxAge: -1, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: a.isSecure(r)})
	// full page load: the boosted <body> carries the old session's CSRF header
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/admin/login")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (a *App) handleAccount(w http.ResponseWriter, r *http.Request, s *Session) {
	a.render(w, r, s, http.StatusOK, "account", &View{Title: "내 계정", Nav: "account"})
}

func (a *App) handleAccountPassword(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	cur, pw, pw2 := r.PostFormValue("current"), r.PostFormValue("password"), r.PostFormValue("password2")
	err := func() error {
		if pw != pw2 {
			return errors.New("새 비밀번호 확인이 일치하지 않습니다")
		}
		if err := validate.Password(pw); err != nil {
			return err
		}
		table := "users"
		if s.IsAdmin() {
			table = "admins"
		}
		var hash string
		if err := a.db.QueryRowContext(ctx, "SELECT password_hash FROM "+table+" WHERE id=?", s.PrincipalID).Scan(&hash); err != nil {
			return err
		}
		if !passwd.Verify(hash, cur) {
			return errors.New("현재 비밀번호가 올바르지 않습니다")
		}
		if s.IsAdmin() {
			nh, err := passwd.Hash(pw, 12)
			if err != nil {
				return err
			}
			if _, err := a.db.ExecContext(ctx, "UPDATE admins SET password_hash=? WHERE id=?", nh, s.PrincipalID); err != nil {
				return err
			}
			store.Audit(ctx, a.db, s.Username, "admin.passwd", s.Username)
		} else if err := a.setUserPassword(ctx, s.Username, s.PrincipalID, pw); err != nil {
			return err
		}
		_, err := a.db.ExecContext(ctx, "DELETE FROM sessions WHERE kind=? AND principal_id=? AND token_hash<>?", s.Kind, s.PrincipalID, s.TokenHash)
		return err
	}()
	a.done(w, r, "/admin/account", "비밀번호가 변경되었습니다. SVN 클라이언트에도 즉시 적용됩니다.", err)
}

// setUserPassword updates the SVN user's hash and htpasswd atomically.
func (a *App) setUserPassword(ctx context.Context, actor string, id int64, pw string) error {
	nh, err := passwd.Hash(pw, a.cfg.BcryptCost)
	if err != nil {
		return err
	}
	var name string
	if err := a.db.QueryRowContext(ctx, "SELECT username FROM users WHERE id=?", id).Scan(&name); err != nil {
		return err
	}
	err = a.mutate(ctx, actor, "user.passwd", name, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE users SET password_hash=?, updated_at=? WHERE id=?", nh, store.Now(), id)
		return err
	})
	if err != nil {
		return err
	}
	return a.verifyHtpasswdEntry(name, pw, true)
}

// verifyHtpasswdEntry re-reads the htpasswd file Apache uses and checks the
// entry for user matches the expected state.
func (a *App) verifyHtpasswdEntry(user, pw string, wantPresent bool) error {
	entries, _, err := readHtpasswdFile(a.cfg.HtpasswdFile())
	if err != nil {
		return fmt.Errorf("htpasswd 확인 실패: %w", err)
	}
	hash, ok := entries[user]
	if !wantPresent {
		if ok {
			return fmt.Errorf("htpasswd 에 %s 항목이 남아 있습니다", user)
		}
		return nil
	}
	if !ok {
		return fmt.Errorf("htpasswd 에 %s 항목이 없습니다", user)
	}
	if pw != "" && !passwd.Verify(hash, pw) {
		return fmt.Errorf("htpasswd 의 %s 비밀번호 검증 실패", user)
	}
	return nil
}
