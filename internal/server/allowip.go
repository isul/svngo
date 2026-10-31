package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"svngo/internal/apache"
	"svngo/internal/i18n"
	"svngo/internal/ipacl"
	"svngo/internal/store"
)

// Client IP allow lists. Each list is the union of a fixed list from the
// environment and one edited in the web UI (settings table); both empty means
// no restriction. Loopback is always allowed. The admin UI list is enforced
// here, the SVN list by Apache (internal/apache/access.go).

const (
	settingAdminAllow = "admin_allow_ips"
	settingSvnAllow   = "svn_allow_ips"
	allowSyncInterval = 20 * time.Second
)

type parsedList struct {
	raw  string
	list ipacl.List
}

// allowState caches parsed settings and tracks what Apache has applied.
type allowState struct {
	admin atomic.Pointer[parsedList]
	svn   atomic.Pointer[parsedList]

	svnMu      sync.Mutex
	svnApplied *string // effective SVN list given to Apache, nil before the first apply
	svnErr     string  // last periodic apply error (logged once)
}

// settingList returns the parsed list stored under key, re-parsing only when
// the stored text changes. A value that does not parse (edited outside SvnGo)
// fails closed to loopback only.
func (a *App) settingList(ctx context.Context, key string, cache *atomic.Pointer[parsedList]) ipacl.List {
	raw := store.Setting(ctx, a.db, key)
	if p := cache.Load(); p != nil && p.raw == raw {
		return p.list
	}
	l, err := ipacl.Parse(raw)
	if err != nil {
		slog.Error("stored IP allow list is invalid; allowing loopback only", "setting", key, "err", err)
		l = ipacl.Loopback
	}
	cache.Store(&parsedList{raw: raw, list: l})
	return l
}

// adminAllowList is the effective admin UI list (empty = unrestricted).
func (a *App) adminAllowList(ctx context.Context) ipacl.List {
	return ipacl.Merge(a.cfg.AdminAllowEnv, a.settingList(ctx, settingAdminAllow, &a.allow.admin))
}

// svnAllowList is the effective SVN list (empty = unrestricted).
func (a *App) svnAllowList(ctx context.Context) ipacl.List {
	return ipacl.Merge(a.cfg.SvnAllowEnv, a.settingList(ctx, settingSvnAllow, &a.allow.svn))
}

func ipAllowed(allow ipacl.List, ip string) bool {
	return allow.Empty() || allow.Contains(ip) || ipacl.Loopback.Contains(ip)
}

// adminACL rejects admin UI requests from clients outside the allow list.
// Static assets stay public so the rejection page keeps its styling.
func (a *App) adminACL(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/admin/static/") {
			if ip := clientIP(r); !ipAllowed(a.adminAllowList(r.Context()), ip) {
				slog.Warn("admin UI request from a disallowed IP rejected", "ip", ip, "method", r.Method, "path", r.URL.Path)
				a.errorPage(w, r, nil, http.StatusForbidden, i18n.M("allowip.denied", ip))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// initSvnAllow writes the SVN rules file before Apache starts.
func (a *App) initSvnAllow(ctx context.Context) error {
	a.allow.svnMu.Lock()
	defer a.allow.svnMu.Unlock()
	list := a.svnAllowList(ctx)
	if err := apache.WriteSvnAccess(a.cfg.RunDir, list); err != nil {
		return fmt.Errorf("writing the SVN allow list failed: %w", err)
	}
	s := list.String()
	a.allow.svnApplied = &s
	return nil
}

// syncSvnAllow gives Apache the current SVN list when it has changed.
func (a *App) syncSvnAllow(ctx context.Context) error {
	if a.apache == nil {
		return nil
	}
	a.allow.svnMu.Lock()
	defer a.allow.svnMu.Unlock()
	list := a.svnAllowList(ctx)
	s := list.String()
	if a.allow.svnApplied != nil && *a.allow.svnApplied == s {
		return nil
	}
	if err := a.apache.ApplySvnAccess(ctx, list); err != nil {
		return err
	}
	a.allow.svnApplied = &s
	slog.Info("SVN IP allow list applied", "entries", list.Len())
	return nil
}

// svnAllowLoop picks up changes made outside the web UI (svngo access-reset)
// and retries failed applies of the IP allow list and the SVN locations.
func (a *App) svnAllowLoop(ctx context.Context) {
	t := time.NewTicker(allowSyncInterval)
	defer t.Stop()
	locErr := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			err := a.syncSvnAllow(ctx)
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			if msg != a.allow.svnErr && msg != "" {
				slog.Error("applying the SVN IP allow list failed", "err", err)
			}
			a.allow.svnErr = msg

			msg = ""
			if err := a.applyLocations(ctx, a.db); err != nil {
				msg = err.Error()
			}
			if msg != locErr && msg != "" {
				slog.Error("applying the SVN locations failed", "err", msg)
			}
			locErr = msg
		}
	}
}

// allowView is the data for the access restriction section.
func (a *App) allowView(r *http.Request) map[string]any {
	ctx := r.Context()
	return map[string]any{
		"ClientIP":  clientIP(r),
		"Admin":     store.Setting(ctx, a.db, settingAdminAllow),
		"Svn":       store.Setting(ctx, a.db, settingSvnAllow),
		"AdminEnv":  a.cfg.AdminAllowEnv.Entries(),
		"SvnEnv":    a.cfg.SvnAllowEnv.Entries(),
		"Trusted":   a.cfg.TrustedProxies.Entries(),
		"AdminLen":  a.adminAllowList(ctx).Len(),
		"SvnLen":    a.svnAllowList(ctx).Len(),
		"SvnManage": a.cfg.ManageApache,
		"Max":       ipacl.MaxEntries,
	}
}

func (a *App) handleAllowIPSave(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	err := func() error {
		admin, err := ipacl.Parse(r.PostFormValue("admin_allow_ips"))
		if err != nil {
			return i18n.Wrap(err, "allowip.admin")
		}
		svn, err := ipacl.Parse(r.PostFormValue("svn_allow_ips"))
		if err != nil {
			return i18n.Wrap(err, "allowip.svn")
		}
		if ip := clientIP(r); !ipAllowed(ipacl.Merge(a.cfg.AdminAllowEnv, admin), ip) {
			return i18n.E("allowip.err.self_lockout", ip)
		}
		oldAdmin := store.Setting(ctx, a.db, settingAdminAllow)
		oldSvn := store.Setting(ctx, a.db, settingSvnAllow)
		if err := a.setAllowSettings(ctx, admin.String(), svn.String()); err != nil {
			return err
		}
		if err := a.syncSvnAllow(ctx); err != nil {
			slog.Error("applying the SVN IP allow list failed; settings restored", "err", err)
			if rerr := a.setAllowSettings(ctx, oldAdmin, oldSvn); rerr != nil {
				slog.Error("restoring the IP allow lists failed", "err", rerr)
			}
			return i18n.Wrap(err, "allowip.err.apply")
		}
		return store.Audit(ctx, a.db, s.Username, "allowip.update", fmt.Sprintf("admin %d -> %d, svn %d -> %d",
			countEntries(oldAdmin), admin.Len(), countEntries(oldSvn), svn.Len()))
	}()
	a.done(w, r, "/admin/system", i18n.M("allowip.saved"), err)
}

func (a *App) setAllowSettings(ctx context.Context, admin, svn string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := store.SetSetting(ctx, tx, settingAdminAllow, admin); err != nil {
		return err
	}
	if err := store.SetSetting(ctx, tx, settingSvnAllow, svn); err != nil {
		return err
	}
	return tx.Commit()
}

func countEntries(raw string) int {
	l, err := ipacl.Parse(raw)
	if err != nil {
		return -1
	}
	return l.Len()
}
