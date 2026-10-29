// Command svngo runs the SvnGo admin server and supervises Apache httpd.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"svngo/internal/config"
	"svngo/internal/logring"
	"svngo/internal/passwd"
	"svngo/internal/server"
	"svngo/internal/store"
	"svngo/internal/validate"
)

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
	case "access-reset":
		err = accessReset(os.Args[2:])
	case "version":
		fmt.Println(server.AppName, server.Version)
	default:
		fmt.Fprintf(os.Stderr, "usage: svngo [serve|healthcheck|admin-passwd <user>|access-reset [admin|svn|all]|version]\n")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func serve() error {
	appLogs := logring.New(1000)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, appLogs), &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	a, err := server.New(ctx, cfg, appLogs)
	if err != nil {
		return err
	}
	defer a.Close()
	return a.Run(ctx)
}

func healthcheck() error {
	port := config.Env("SVNGO_HTTP_PORT", "8080")
	target := "http://127.0.0.1:" + port + "/admin/healthz"
	if !config.EnvBool("SVNGO_MANAGE_APACHE", false) {
		target = "http://" + config.Env("SVNGO_LISTEN", "127.0.0.1:9000") + "/admin/healthz"
	}
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Get(target)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("health check failed: %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// adminPasswd sets/creates a web admin password; the password is read from stdin.
func adminPasswd(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: svngo admin-passwd <admin-id>  (the new password is read from standard input)")
	}
	user := args[0]
	if err := validate.Username(user); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	fmt.Fprint(os.Stderr, "New password: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return errors.New("cannot read the password")
	}
	pw := strings.TrimRight(line, "\r\n")
	if err := validate.Password(pw); err != nil {
		return err
	}
	db, err := store.Open(cfg.DBFile())
	if err != nil {
		return err
	}
	defer db.Close()
	hash, err := passwd.Hash(pw, 12)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO admins(username, password_hash, created_at) VALUES(?,?,?)
		ON CONFLICT(username) DO UPDATE SET password_hash=excluded.password_hash`, user, hash, store.Now()); err != nil {
		return err
	}
	db.ExecContext(ctx, "DELETE FROM sessions WHERE kind='admin' AND username=?", user)
	store.Audit(ctx, db, "cli", "admin.passwd", user)
	fmt.Fprintln(os.Stderr, "\nAdmin password set:", user)
	return nil
}

// accessReset clears the IP allow lists edited in the web UI (recovery after
// a lockout). Lists fixed by environment variables stay in effect.
func accessReset(args []string) error {
	which := "admin"
	if len(args) > 1 {
		return errors.New("usage: svngo access-reset [admin|svn|all]")
	}
	if len(args) == 1 {
		which = args[0]
	}
	var keys []string
	switch which {
	case "admin":
		keys = []string{"admin_allow_ips"}
	case "svn":
		keys = []string{"svn_allow_ips"}
	case "all":
		keys = []string{"admin_allow_ips", "svn_allow_ips"}
	default:
		return errors.New("usage: svngo access-reset [admin|svn|all]")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.DBFile())
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	for _, k := range keys {
		if err := store.SetSetting(ctx, db, k, ""); err != nil {
			return err
		}
	}
	store.Audit(ctx, db, "cli", "allowip.reset", which)
	fmt.Fprintf(os.Stderr, "IP allow lists cleared (%s). The admin UI applies this immediately, SVN within 20 seconds.\n", which)
	if !cfg.AdminAllowEnv.Empty() || !cfg.SvnAllowEnv.Empty() {
		fmt.Fprintln(os.Stderr, "SVNGO_ADMIN_ALLOW_IPS / SVNGO_SVN_ALLOW_IPS still apply; change them in the environment and restart to remove them.")
	}
	return nil
}
