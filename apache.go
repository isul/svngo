package main

import (
	"bufio"
	"bytes"
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
)

// Ring keeps the last N log lines in memory for the web UI.
type Ring struct {
	mu    sync.Mutex
	lines []string
	next  int
	full  bool
	part  []byte
}

func newRing(n int) *Ring { return &Ring{lines: make([]string, n)} }

func (r *Ring) Add(line string) {
	r.mu.Lock()
	r.lines[r.next] = line
	r.next = (r.next + 1) % len(r.lines)
	if r.next == 0 {
		r.full = true
	}
	r.mu.Unlock()
}

// Write implements io.Writer (used as slog output).
func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.part = append(r.part, p...)
	var done []string
	for {
		i := bytes.IndexByte(r.part, '\n')
		if i < 0 {
			break
		}
		done = append(done, string(r.part[:i]))
		r.part = r.part[i+1:]
	}
	r.mu.Unlock()
	for _, l := range done {
		r.Add(l)
	}
	return len(p), nil
}

// Lines returns lines newest first.
func (r *Ring) Lines(max int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.next
	if r.full {
		n = len(r.lines)
	}
	if max > n {
		max = n
	}
	out := make([]string, 0, max)
	for i := 1; i <= max; i++ {
		idx := (r.next - i + len(r.lines)) % len(r.lines)
		out = append(out, r.lines[idx])
	}
	return out
}

// Apache supervises the httpd child process.
type Apache struct {
	cfg  *Config
	logs *Ring

	mu        sync.Mutex
	cmd       *exec.Cmd
	pid       int
	startedAt time.Time
	restarts  int
	lastExit  string
	running   bool
	tls       bool
	done      chan struct{}

	// onAuthFail receives the client IP of every failed SVN Basic login.
	onAuthFail func(ip string)
}

// authFailPrefix marks the dedicated access-log line Apache writes for a
// failed SVN login (httpd.conf: LogFormat svngo_authfail). Only lines from
// httpd's stdout are trusted; every normal access line starts with an IP.
const authFailPrefix = "svngo-authfail "

type ApacheStatus struct {
	Managed   bool
	Running   bool
	PID       int
	StartedAt time.Time
	Restarts  int
	LastExit  string
	TLS       bool
}

func newApache(cfg *Config, logs *Ring) *Apache {
	return &Apache{cfg: cfg, logs: logs, done: make(chan struct{})}
}

func (ap *Apache) env() []string {
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
		"SVNGO_SVN_BLOCK_MAP=" + filepath.Join(ap.cfg.RunDir, svnBlockFile),
	}
	if tz, ok := os.LookupEnv("TZ"); ok {
		env = append(env, "TZ="+tz)
	}
	return env
}

func (ap *Apache) args(extra ...string) []string {
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
func (ap *Apache) Check(ctx context.Context) error {
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

func (ap *Apache) pump(r io.Reader, w io.Writer, access bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if access && strings.HasPrefix(line, authFailPrefix) {
			if ip := strings.TrimSpace(line[len(authFailPrefix):]); net.ParseIP(ip) != nil && ap.onAuthFail != nil {
				ap.onAuthFail(ip)
			}
			continue
		}
		ap.logs.Add(line)
		fmt.Fprintln(w, "[apache] "+line)
	}
}

// Run starts httpd and restarts it with backoff until ctx is cancelled.
func (ap *Apache) Run(ctx context.Context) {
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

func (ap *Apache) runOnce(ctx context.Context) error {
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
func (ap *Apache) Wait() { <-ap.done }

func (ap *Apache) Status() ApacheStatus {
	ap.mu.Lock()
	defer ap.mu.Unlock()
	return ApacheStatus{Managed: true, Running: ap.running, PID: ap.pid, StartedAt: ap.startedAt,
		Restarts: ap.restarts, LastExit: ap.lastExit, TLS: ap.tls}
}

func (ap *Apache) Version(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, ap.cfg.ApacheBin, "-v").Output()
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(strings.TrimPrefix(first, "Server version:"))
}
