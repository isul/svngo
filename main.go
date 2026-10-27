package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const appName = "SvnGo"

var version = "1.0.0"

// App holds shared state for the admin server.
type App struct {
	cfg    *Config
	db     *sql.DB
	svn    *SVN
	apache *Apache // nil when Apache is not managed by SvnGo

	syncMu sync.Mutex // serializes DB + auth file changes and repo structure changes
	policy atomic.Pointer[Policy]

	limiter  *loginLimiter
	svnGuard *svnGuard
	appLogs  *Ring
	apLogs   *Ring
	tmpl     *templates

	ctx    context.Context
	jobSem chan struct{}
	jobsWG sync.WaitGroup

	sizeMu  sync.Mutex
	sizing  map[string]bool
	started time.Time
}

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "healthcheck":
		err = healthcheck()
	case "admin-passwd":
		err = adminPasswd(os.Args[2:])
	case "version":
		fmt.Println(appName, version)
	default:
		fmt.Fprintf(os.Stderr, "사용법: svngo [serve|healthcheck|admin-passwd <user>|version]\n")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "오류:", err)
		os.Exit(1)
	}
}

func newApp(ctx context.Context, cfg *Config, logs *Ring) (*App, error) {
	if err := cfg.ensureDirs(); err != nil {
		return nil, err
	}
	db, err := openDB(cfg.DBFile())
	if err != nil {
		return nil, err
	}
	svn, err := newSVN(cfg)
	if err != nil {
		db.Close()
		return nil, err
	}
	tm, err := loadTemplates()
	if err != nil {
		db.Close()
		return nil, err
	}
	a := &App{
		cfg: cfg, db: db, svn: svn, limiter: newLoginLimiter(),
		appLogs: logs, apLogs: newRing(1000), tmpl: tm,
		ctx: ctx, jobSem: make(chan struct{}, 2), sizing: map[string]bool{}, started: time.Now(),
	}
	a.svnGuard, err = newSVNGuard(cfg.RunDir, a.svnBlockLimits, func(ip string, fails int) {
		audit(context.Background(), a.db, "system", "svn.block", fmt.Sprintf("%s (실패 %d회)", ip, fails))
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("SVN 차단 목록 초기화 실패: %w", err)
	}
	if err := a.init(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return a, nil
}

// svnBlockLimits returns the SVN brute-force threshold (0 = off) and window.
func (a *App) svnBlockLimits() (int, time.Duration) {
	ctx := context.Background()
	return getSettingInt(ctx, a.db, "svn_block_attempts", 0, 1000),
		time.Duration(getSettingInt(ctx, a.db, "svn_block_minutes", 1, 1440)) * time.Minute
}

// init prepares state at startup: initial admin, repository scan, file sync.
func (a *App) init(ctx context.Context) error {
	if err := a.svn.SelfTestAuthz(ctx, a.cfg.RunDir); err != nil {
		return err
	}
	if err := a.ensureAdmin(ctx); err != nil {
		return err
	}
	if _, err := a.db.ExecContext(ctx, `UPDATE jobs SET status='failed', message='서버 재시작으로 중단됨', finished_at=?
		WHERE status IN ('queued','running')`, now()); err != nil {
		return err
	}
	a.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at < ?", now())
	os.RemoveAll(a.cfg.TmpDir())
	os.MkdirAll(a.cfg.TmpDir(), 0o750)
	if n, err := a.registerRepos(ctx, "system"); err != nil {
		return err
	} else if n > 0 {
		slog.Info("기존 저장소 자동 등록", "count", n)
	}
	// Regenerate files from the DB so a crash between steps can never leave
	// Apache with stale credentials.
	if err := a.mutate(ctx, "system", "", "", nil); err != nil {
		return fmt.Errorf("인증 파일 동기화 실패: %w", err)
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
		pw = randomPassword()
		generated = true
	}
	if err := validPassword(pw); err != nil {
		return fmt.Errorf("ADMIN_PASSWORD: %w", err)
	}
	hash, err := hashPassword(pw, 12)
	if err != nil {
		return err
	}
	if _, err := a.db.ExecContext(ctx, "INSERT INTO admins(username, password_hash, created_at) VALUES(?,?,?)",
		a.cfg.AdminUser, hash, now()); err != nil {
		return err
	}
	if generated {
		f := filepath.Join(a.cfg.ConfigDir, "initial-admin-password")
		if err := os.WriteFile(f, []byte(pw+"\n"), 0o600); err != nil {
			return err
		}
		slog.Warn("ADMIN_PASSWORD 가 설정되지 않아 임의 비밀번호를 생성했습니다. 파일에서 확인 후 삭제하세요",
			"user", a.cfg.AdminUser, "file", f)
	} else {
		slog.Info("초기 관리자 계정 생성", "user", a.cfg.AdminUser)
	}
	return nil
}

func (a *App) unregisteredRepos(ctx context.Context) []string {
	ents, err := os.ReadDir(a.cfg.ReposDir)
	if err != nil {
		return nil
	}
	known := map[string]bool{}
	rows, err := a.db.QueryContext(ctx, "SELECT name FROM repos")
	if err != nil {
		return nil
	}
	for rows.Next() {
		var n string
		rows.Scan(&n)
		known[n] = true
	}
	rows.Close()
	var res []string
	for _, e := range ents {
		if e.IsDir() && !known[e.Name()] && validRepoName(e.Name()) == nil && isRepoDir(filepath.Join(a.cfg.ReposDir, e.Name())) {
			res = append(res, e.Name())
		}
	}
	return res
}

// registerRepos adds repositories found on disk but missing in the DB.
func (a *App) registerRepos(ctx context.Context, actor string) (int, error) {
	names := a.unregisteredRepos(ctx)
	if len(names) == 0 {
		return 0, nil
	}
	err := a.mutate(ctx, actor, "repo.scan", strings.Join(names, ", "), func(tx *sql.Tx) error {
		for _, n := range names {
			if _, err := tx.ExecContext(ctx, "INSERT INTO repos(name, created_at) VALUES(?,?)", n, now()); err != nil {
				return err
			}
		}
		return nil
	})
	return len(names), err
}

func serve() error {
	appLogs := newRing(1000)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, appLogs), &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	a, err := newApp(ctx, cfg, appLogs)
	if err != nil {
		return err
	}
	defer a.db.Close()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           a.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	errc := make(chan error, 1)
	go func() {
		slog.Info(appName+" 관리 서버 시작", "version", version, "listen", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	if cfg.ManageApache {
		a.apache = newApache(cfg, a.apLogs)
		a.apache.onAuthFail = a.svnGuard.Fail
		if err := a.apache.Check(ctx); err != nil {
			return err
		}
		go a.apache.Run(ctx)
	}
	go a.svnGuard.Run(ctx)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	slog.Info("종료 신호 수신, 종료 중...")
	shCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if a.apache != nil {
		a.apache.Wait()
	}
	srv.Shutdown(shCtx)
	a.jobsWG.Wait()
	slog.Info("종료 완료")
	return nil
}

func healthcheck() error {
	port := env("SVNGO_HTTP_PORT", "8080")
	target := "http://127.0.0.1:" + port + "/admin/healthz"
	if !envBool("SVNGO_MANAGE_APACHE", false) {
		target = "http://" + env("SVNGO_LISTEN", "127.0.0.1:9000") + "/admin/healthz"
	}
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Get(target)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("상태 점검 실패: %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// adminPasswd sets/creates a web admin password; the password is read from stdin.
func adminPasswd(args []string) error {
	if len(args) != 1 {
		return errors.New("사용법: svngo admin-passwd <관리자ID>  (새 비밀번호는 표준입력으로 전달)")
	}
	user := args[0]
	if err := validUsername(user); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	fmt.Fprint(os.Stderr, "새 비밀번호: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return errors.New("비밀번호를 읽을 수 없습니다")
	}
	pw := strings.TrimRight(line, "\r\n")
	if err := validPassword(pw); err != nil {
		return err
	}
	db, err := openDB(cfg.DBFile())
	if err != nil {
		return err
	}
	defer db.Close()
	hash, err := hashPassword(pw, 12)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO admins(username, password_hash, created_at) VALUES(?,?,?)
		ON CONFLICT(username) DO UPDATE SET password_hash=excluded.password_hash`, user, hash, now()); err != nil {
		return err
	}
	db.ExecContext(ctx, "DELETE FROM sessions WHERE kind='admin' AND username=?", user)
	audit(ctx, db, "cli", "admin.passwd", user)
	fmt.Fprintln(os.Stderr, "\n관리자 비밀번호가 설정되었습니다:", user)
	return nil
}
