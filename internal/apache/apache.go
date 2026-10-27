// Package apache supervises the httpd child process and blocks SVN clients
// that repeatedly fail HTTP Basic authentication.
package apache

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"svngo/internal/config"
	"svngo/internal/logring"
)

// Supervisor runs httpd in the foreground and restarts it when it exits.
type Supervisor struct {
	cfg  *config.Config
	logs *logring.Ring

	mu        sync.Mutex
	cmd       *exec.Cmd
	pid       int
	startedAt time.Time
	restarts  int
	lastExit  string
	running   bool
	tls       bool
	done      chan struct{}

	// OnAuthFail receives the client IP of every failed SVN Basic login.
	OnAuthFail func(ip string)
}

// authFailPrefix marks the dedicated access-log line Apache writes for a
// failed SVN login (httpd.conf: LogFormat svngo_authfail). Only lines from
// httpd's stdout are trusted; every normal access line starts with an IP.
const authFailPrefix = "svngo-authfail "

type Status struct {
	Managed   bool
	Running   bool
	PID       int
	StartedAt time.Time
	Restarts  int
	LastExit  string
	TLS       bool
}

func New(cfg *config.Config, logs *logring.Ring) *Supervisor {
	return &Supervisor{cfg: cfg, logs: logs, done: make(chan struct{})}
}

func (ap *Supervisor) env() []string {
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"SVNGO_HTTP_PORT=" + ap.cfg.HTTPPort,
		"SVNGO_HTTPS_PORT=" + ap.cfg.HTTPSPort,
		"SVNGO_REPOS_DIR=" + ap.cfg.ReposDir,
		"SVNGO_CONFIG_DIR=" + ap.cfg.ConfigDir,
		"SVNGO_RUN_DIR=" + ap.cfg.RunDir,
		"SVNGO_ADMIN_BACKEND=" + ap.cfg.Listen,
		"SVNGO_SERVER_NAME=" + ap.cfg.ServerName,
		"SVNGO_TLS_CERT=" + ap.cfg.TLSCert,
		"SVNGO_TLS_KEY=" + ap.cfg.TLSKey,
		"SVNGO_SVN_MAX_BODY=" + strconv.FormatInt(ap.cfg.SvnMaxBody, 10),
		"SVNGO_SVN_MAX_XML=" + strconv.FormatInt(ap.cfg.SvnMaxXML, 10),
		"SVNGO_SVN_BLOCK_MAP=" + filepath.Join(ap.cfg.RunDir, blockFile),
	}
	if tz, ok := os.LookupEnv("TZ"); ok {
		env = append(env, "TZ="+tz)
	}
	return env
}

func (ap *Supervisor) args(extra ...string) []string {
	args := []string{"-f", ap.cfg.ApacheConf}
	if ap.tls {
		args = append(args, "-DSVNGO_TLS")
	}
	return append(args, extra...)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// Check validates the Apache configuration (httpd -t).
func (ap *Supervisor) Check(ctx context.Context) error {
	ap.tls = fileExists(ap.cfg.TLSCert) && fileExists(ap.cfg.TLSKey)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, ap.cfg.ApacheBin, ap.args("-t")...)
	c.Env = ap.env()
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Apache 설정 검사 실패: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (ap *Supervisor) pump(r io.Reader, w io.Writer, access bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if access && strings.HasPrefix(line, authFailPrefix) {
			if ip := strings.TrimSpace(line[len(authFailPrefix):]); net.ParseIP(ip) != nil && ap.OnAuthFail != nil {
				ap.OnAuthFail(ip)
			}
			continue
		}
		ap.logs.Add(line)
		fmt.Fprintln(w, "[apache] "+line)
	}
}

// Run starts httpd and restarts it with backoff until ctx is cancelled.
func (ap *Supervisor) Run(ctx context.Context) {
	defer close(ap.done)
	backoff := time.Second
	for {
		started := time.Now()
		err := ap.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		msg := "정상 종료"
		if err != nil {
			msg = err.Error()
		}
		ap.mu.Lock()
		ap.lastExit = time.Now().Format("2006-01-02 15:04:05") + " " + msg
		ap.restarts++
		ap.mu.Unlock()
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		slog.Error("Apache 가 종료되어 재시작합니다", "err", msg, "backoff", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (ap *Supervisor) runOnce(ctx context.Context) error {
	os.Remove(ap.cfg.RunDir + "/httpd.pid")
	c := exec.Command(ap.cfg.ApacheBin, ap.args("-DFOREGROUND")...)
	c.Env = ap.env()
	stdout, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := c.StderrPipe()
	if err != nil {
		return err
	}
	if err := c.Start(); err != nil {
		return fmt.Errorf("Apache 시작 실패: %w", err)
	}
	ap.mu.Lock()
	ap.cmd, ap.pid, ap.startedAt, ap.running = c, c.Process.Pid, time.Now(), true
	ap.mu.Unlock()
	slog.Info("Apache 시작", "pid", c.Process.Pid, "http_port", ap.cfg.HTTPPort, "tls", ap.tls)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); ap.pump(stdout, os.Stdout, true) }()
	go func() { defer wg.Done(); ap.pump(stderr, os.Stderr, false) }()

	waitErr := make(chan error, 1)
	go func() {
		wg.Wait()
		waitErr <- c.Wait()
	}()

	var res error
	select {
	case res = <-waitErr:
	case <-ctx.Done():
		// graceful-stop lets in-flight SVN requests finish (GracefulShutdownTimeout)
		c.Process.Signal(syscall.SIGWINCH)
		select {
		case res = <-waitErr:
		case <-time.After(15 * time.Second):
			c.Process.Signal(syscall.SIGTERM)
			select {
			case res = <-waitErr:
			case <-time.After(5 * time.Second):
				c.Process.Kill()
				res = <-waitErr
			}
		}
	}
	ap.mu.Lock()
	ap.running, ap.pid = false, 0
	ap.mu.Unlock()
	return res
}

// Wait blocks until the supervisor loop has exited.
func (ap *Supervisor) Wait() { <-ap.done }

func (ap *Supervisor) Status() Status {
	ap.mu.Lock()
	defer ap.mu.Unlock()
	return Status{Managed: true, Running: ap.running, PID: ap.pid, StartedAt: ap.startedAt,
		Restarts: ap.restarts, LastExit: ap.lastExit, TLS: ap.tls}
}

func (ap *Supervisor) Version(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, ap.cfg.ApacheBin, "-v").Output()
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(strings.TrimPrefix(first, "Server version:"))
}
