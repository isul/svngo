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
	case "version":
		fmt.Println(server.AppName, server.Version)
	default:
		fmt.Fprintf(os.Stderr, "사용법: svngo [serve|healthcheck|admin-passwd <user>|version]\n")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "오류:", err)
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
	if err := validate.Username(user); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	fmt.Fprint(os.Stderr, "새 비밀번호: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return errors.New("비밀번호를 읽을 수 없습니다")
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
	fmt.Fprintln(os.Stderr, "\n관리자 비밀번호가 설정되었습니다:", user)
	return nil
}
