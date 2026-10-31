// Package server is the SvnGo admin web application: HTTP handlers, sessions,
// background jobs and the serialized DB → htpasswd/authz synchronization.
package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"svngo/internal/apache"
	"svngo/internal/authz"
	"svngo/internal/config"
	"svngo/internal/i18n"
	"svngo/internal/logring"
	"svngo/internal/passwd"
	"svngo/internal/store"
	"svngo/internal/svn"
	"svngo/internal/validate"
)

const AppName = "SvnGo"

// Version is set at build time with -ldflags "-X svngo/internal/server.Version=…".
var Version = "1.0.0"

// App holds shared state for the admin server.
type App struct {
	cfg    *config.Config
	db     *sql.DB
	svn    *svn.Tools
	apache *apache.Supervisor // nil when Apache is not managed by SvnGo

	syncMu sync.Mutex // serializes DB + auth file changes and repo structure changes
	policy atomic.Pointer[authz.Policy]

	limiter  *loginLimiter
	svnGuard *apache.Guard
	allow    allowState
	appLogs  *logring.Ring
	apLogs   *logring.Ring
	tmpl     *templates
	i18n     *i18n.Registry

	ctx    context.Context
	jobSem chan struct{}
	jobsWG sync.WaitGroup

	sizeMu  sync.Mutex
	sizing  map[string]bool
	started time.Time
}

// New opens the database, verifies the authz validator and brings the auth
// files in line with the database. Call Close when done.
func New(ctx context.Context, cfg *config.Config, logs *logring.Ring) (*App, error) {
	if err := cfg.EnsureDirs(); err != nil {
		return nil, err
	}
	db, err := store.Open(cfg.DBFile())
	if err != nil {
		return nil, err
	}
	tools, err := svn.New(cfg)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err := os.MkdirAll(cfg.LocalesDir(), 0o750); err != nil {
		slog.Warn("creating the language file folder failed", "dir", cfg.LocalesDir(), "err", err)
	}
	reg := i18n.New(cfg.LocalesDir())
	for _, e := range reg.LoadErrors() {
		slog.Warn("language file skipped", "dir", cfg.LocalesDir(), "err", e)
	}
	tm, err := loadTemplates(reg)
	if err != nil {
		db.Close()
		return nil, err
	}
	a := &App{
		cfg: cfg, db: db, svn: tools, limiter: newLoginLimiter(),
		appLogs: logs, apLogs: logring.New(1000), tmpl: tm, i18n: reg,
		ctx: ctx, jobSem: make(chan struct{}, 2), sizing: map[string]bool{}, started: time.Now(),
	}
	a.svnGuard, err = apache.NewGuard(cfg.RunDir, a.svnBlockLimits, func(ip string, fails int) {
		store.Audit(context.Background(), a.db, "system", "svn.block", fmt.Sprintf("%s (%d failures)", ip, fails))
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("initializing the SVN block list failed: %w", err)
	}
	if err := a.init(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return a, nil
}

// Close releases the database. Call it after Run has returned.
func (a *App) Close() error { return a.db.Close() }

// Run serves the admin UI, supervises Apache when configured and blocks until
// ctx is cancelled; it then shuts down gracefully and waits for running jobs.
func (a *App) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              a.cfg.Listen,
		Handler:           a.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	errc := make(chan error, 1)
	go func() {
		slog.Info(AppName+" admin server started", "version", Version, "listen", a.cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	if a.cfg.ManageApache {
		if err := a.initSvnAllow(ctx); err != nil {
			return err
		}
		if err := a.initLocations(ctx); err != nil {
			return err
		}
		a.apache = apache.New(a.cfg, a.apLogs)
		a.apache.OnAuthFail = a.svnGuard.Fail
		if err := a.apache.Check(ctx); err != nil {
			return err
		}
		go a.apache.Run(ctx)
		go a.svnAllowLoop(ctx)
	}
	go a.svnGuard.Run(ctx)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutdown signal received, stopping...")
	shCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if a.apache != nil {
		a.apache.Wait()
	}
	srv.Shutdown(shCtx)
	a.jobsWG.Wait()
	slog.Info("shutdown complete")
	return nil
}

// svnBlockLimits returns the SVN brute-force threshold (0 = off) and window.
func (a *App) svnBlockLimits() (int, time.Duration) {
	ctx := context.Background()
	return store.SettingInt(ctx, a.db, "svn_block_attempts", 0, 1000),
		time.Duration(store.SettingInt(ctx, a.db, "svn_block_minutes", 1, 1440)) * time.Minute
}

// init prepares state at startup: initial admin, repository scan, file sync.
func (a *App) init(ctx context.Context) error {
	if err := a.svn.SelfTestAuthz(ctx, a.cfg.RunDir); err != nil {
		return err
	}
	if err := a.ensureAdmin(ctx); err != nil {
		return err
	}
	if _, err := a.db.ExecContext(ctx, `UPDATE jobs SET status='failed', message=?, finished_at=?
		WHERE status IN ('queued','running')`, i18n.Encode(i18n.M("job.interrupted_restart")), store.Now()); err != nil {
		return err
	}
	a.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at < ?", store.Now())
	os.RemoveAll(a.cfg.TmpDir())
	os.MkdirAll(a.cfg.TmpDir(), 0o750)
	locs, err := locationNames(ctx, a.db)
	if err != nil {
		return err
	}
	for _, l := range locs {
		os.RemoveAll(a.tmpDir(l))
	}
	if n, err := a.registerRepos(ctx, "system"); err != nil {
		return err
	} else if n > 0 {
		slog.Info("existing repositories registered", "count", n)
	}
	if _, conflicts := a.scanRepos(ctx); len(conflicts) > 0 {
		slog.Warn("repositories not registered because the name is already used in another location",
			"repos", strings.Join(repoLabels(conflicts), ", "))
	}
	// Regenerate files from the DB so a crash between steps can never leave
	// Apache with stale credentials.
	if err := a.mutate(ctx, "system", "", "", nil); err != nil {
		return fmt.Errorf("auth file synchronization failed: %w", err)
	}
	return nil
}

func (a *App) ensureAdmin(ctx context.Context) error {
	var n int
	if err := a.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM admins").Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	pw := a.cfg.AdminPassword
	generated := false
	if pw == "" {
		pw = passwd.Generate()
		generated = true
	}
	if err := validate.Password(pw); err != nil {
		return fmt.Errorf("ADMIN_PASSWORD: %w", err)
	}
	hash, err := passwd.Hash(pw, 12)
	if err != nil {
		return err
	}
	if _, err := a.db.ExecContext(ctx, "INSERT INTO admins(username, password_hash, created_at) VALUES(?,?,?)",
		a.cfg.AdminUser, hash, store.Now()); err != nil {
		return err
	}
	if generated {
		f := filepath.Join(a.cfg.ConfigDir, "initial-admin-password")
		if err := os.WriteFile(f, []byte(pw+"\n"), 0o600); err != nil {
			return err
		}
		slog.Warn("ADMIN_PASSWORD is not set; a random password was generated. Read it from the file, then delete the file",
			"user", a.cfg.AdminUser, "file", f)
	} else {
		slog.Info("initial admin account created", "user", a.cfg.AdminUser)
	}
	return nil
}

// registerRepos adds repositories found on disk (in every location) but
// missing in the DB.
func (a *App) registerRepos(ctx context.Context, actor string) (int, error) {
	found, _ := a.scanRepos(ctx)
	if len(found) == 0 {
		return 0, nil
	}
	err := a.mutate(ctx, actor, "repo.scan", strings.Join(repoLabels(found), ", "), func(tx *sql.Tx) error {
		for _, f := range found {
			if _, err := tx.ExecContext(ctx, "INSERT INTO repos(name, location, created_at) VALUES(?,?,?)", f.Name, f.Location, store.Now()); err != nil {
				return err
			}
		}
		return nil
	})
	return len(found), err
}
