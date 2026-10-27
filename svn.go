package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// SVN runs Subversion command line tools. Commands are always executed
// directly (never through a shell) with fixed argument lists.
type SVN struct {
	Admin, Look, Client, Authz string
	configDir                  string
	env                        []string
}

func newSVN(c *Config) (*SVN, error) {
	cfgDir := filepath.Join(c.RunDir, "svn-client")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		return nil, err
	}
	return &SVN{
		Admin: c.SvnAdmin, Look: c.SvnLook, Client: c.Svn, Authz: c.SvnAuthz,
		configDir: cfgDir,
		env: []string{
			"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
			"HOME=" + cfgDir,
			"LC_ALL=C.UTF-8",
			"LANG=C.UTF-8",
		},
	}, nil
}

var errOutputTooLarge = errors.New("명령 출력이 너무 큽니다")

type limitWriter struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.buf.Len()+len(p) > l.limit {
		l.over = true
		return 0, errOutputTooLarge
	}
	return l.buf.Write(p)
}

type cmdError struct {
	Tool   string
	Stderr string
	Err    error
}

func (e *cmdError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}
	if len(msg) > 600 {
		msg = msg[:600] + "..."
	}
	return e.Tool + ": " + msg
}

func (e *cmdError) Unwrap() error { return e.Err }

// isNotFound reports whether an svn error means the path/revision does not exist.
func isNotFound(err error) bool {
	var ce *cmdError
	if !errors.As(err, &ce) {
		return false
	}
	for _, code := range []string{"E160013", "E160006", "E195012", "E170000", "E200009", "E160016", "E160017"} {
		if strings.Contains(ce.Stderr, code) {
			return true
		}
	}
	return false
}

func (s *SVN) command(ctx context.Context, bin string, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, bin, args...)
	c.Env = s.env
	c.WaitDelay = 5 * time.Second
	return c
}

// output runs a command and returns stdout (bounded by maxOut bytes).
func (s *SVN) output(ctx context.Context, timeout time.Duration, maxOut int, bin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out := &limitWriter{limit: maxOut}
	errb := &limitWriter{limit: 64 * 1024}
	c := s.command(ctx, bin, args...)
	c.Stdout, c.Stderr = out, errb
	if err := c.Run(); err != nil {
		if out.over {
			return nil, errOutputTooLarge
		}
		if ctx.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("시간 초과: %w", ctx.Err())
		}
		return nil, &cmdError{Tool: filepath.Base(bin) + " " + args[0], Stderr: errb.buf.String(), Err: err}
	}
	return out.buf.Bytes(), nil
}

// stream runs a command writing stdout to w, with optional stdin.
func (s *SVN) stream(ctx context.Context, w io.Writer, stdin io.Reader, bin string, args ...string) error {
	errb := &limitWriter{limit: 64 * 1024}
	c := s.command(ctx, bin, args...)
	c.Stdout, c.Stderr, c.Stdin = w, errb, stdin
	if err := c.Run(); err != nil {
		return &cmdError{Tool: filepath.Base(bin) + " " + args[0], Stderr: errb.buf.String(), Err: err}
	}
	return nil
}

// fileURL builds a file:// URL with a peg revision for the svn client.
func fileURL(repoDir, path string, rev int64) string {
	var b strings.Builder
	b.WriteString("file://")
	for _, seg := range strings.Split(filepath.ToSlash(repoDir), "/") {
		if seg == "" {
			continue
		}
		b.WriteString("/" + url.PathEscape(seg))
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" {
			continue
		}
		b.WriteString("/" + url.PathEscape(seg))
	}
	if rev >= 0 {
		b.WriteString("@" + strconv.FormatInt(rev, 10))
	}
	return b.String()
}

func (s *SVN) clientArgs(args ...string) []string {
	return append([]string{args[0], "--non-interactive", "--config-dir", s.configDir}, args[1:]...)
}

// ---- repository administration ----

func (s *SVN) Create(ctx context.Context, dir string) error {
	_, err := s.output(ctx, 2*time.Minute, 1<<20, s.Admin, "create", "--fs-type", "fsfs", dir)
	return err
}

func (s *SVN) MkLayout(ctx context.Context, dir, author string) error {
	args := s.clientArgs("mkdir", "--username", author, "-m", "Initial repository layout",
		fileURL(dir, "/trunk", -1), fileURL(dir, "/branches", -1), fileURL(dir, "/tags", -1))
	_, err := s.output(ctx, 2*time.Minute, 1<<20, s.Client, args...)
	return err
}

func (s *SVN) Youngest(ctx context.Context, dir string) (int64, error) {
	out, err := s.output(ctx, 30*time.Second, 4096, s.Look, "youngest", dir)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

func (s *SVN) UUID(ctx context.Context, dir string) (string, error) {
	out, err := s.output(ctx, 30*time.Second, 4096, s.Look, "uuid", dir)
	return strings.TrimSpace(string(out)), err
}

func (s *SVN) Verify(ctx context.Context, dir string) error {
	return s.stream(ctx, io.Discard, nil, s.Admin, "verify", "--quiet", dir)
}

func (s *SVN) Dump(ctx context.Context, dir string, w io.Writer) error {
	return s.stream(ctx, w, nil, s.Admin, "dump", "--quiet", "--deltas", dir)
}

func (s *SVN) Load(ctx context.Context, dir string, r io.Reader) error {
	return s.stream(ctx, io.Discard, r, s.Admin, "load", "--quiet", dir)
}

func (s *SVN) Versions(ctx context.Context) map[string]string {
	v := map[string]string{}
	if out, err := s.output(ctx, 10*time.Second, 4096, s.Admin, "--version", "--quiet"); err == nil {
		v["subversion"] = strings.TrimSpace(string(out))
	}
	return v
}

// ---- authz validation (svnauthz compatible CLI) ----

// SelfTestAuthz proves the validator is a working libsvn authz engine: it must
// accept a valid file, reject a malformed one and evaluate access correctly.
// SvnGo refuses to start otherwise, because every auth file change relies on it.
func (s *SVN) SelfTestAuthz(ctx context.Context, dir string) error {
	if s.Authz == "" {
		return errors.New("authz 검증 도구(svngo-authz 또는 svnauthz)를 찾을 수 없습니다. SVNGO_SVNAUTHZ 로 경로를 지정하세요")
	}
	write := func(content string) (string, error) {
		f, err := os.CreateTemp(dir, ".authz-selftest-*")
		if err != nil {
			return "", err
		}
		defer f.Close()
		if _, err := f.WriteString(content); err != nil {
			os.Remove(f.Name())
			return "", err
		}
		return f.Name(), nil
	}
	good, err := write("[groups]\ndev = alice\n\n[selftest:/]\n@dev = rw\n\n[selftest:/secret]\n@dev =\n")
	if err != nil {
		return err
	}
	defer os.Remove(good)
	bad, err := write("[groups]\ndev = alice\n\n[selftest:/\n@dev = rw\n")
	if err != nil {
		return err
	}
	defer os.Remove(bad)

	if err := s.ValidateAuthz(ctx, good); err != nil {
		return fmt.Errorf("authz 검증 도구 자체 점검 실패 (정상 파일 거부): %w", err)
	}
	if err := s.ValidateAuthz(ctx, bad); err == nil {
		return fmt.Errorf("authz 검증 도구 자체 점검 실패: 잘못된 authz 파일을 통과시켰습니다 (%s)", s.Authz)
	}
	for _, c := range []struct{ path, user, want string }{
		{"/", "alice", "rw"}, {"/secret", "alice", "no"}, {"/", "bob", "no"},
	} {
		got, err := s.AccessOf(ctx, good, "selftest", c.path, c.user)
		if err != nil || got != c.want {
			return fmt.Errorf("authz 검증 도구 자체 점검 실패: %s %s 권한 예상=%s 결과=%q (%v)", c.user, c.path, c.want, got, err)
		}
	}
	return nil
}

func (s *SVN) ValidateAuthz(ctx context.Context, file string) error {
	_, err := s.output(ctx, 30*time.Second, 64*1024, s.Authz, "validate", file)
	return err
}

// AccessOf returns "rw", "r" or "no" as computed by libsvn_repos.
func (s *SVN) AccessOf(ctx context.Context, file, repo, path, user string) (string, error) {
	out, err := s.output(ctx, 30*time.Second, 4096, s.Authz, "accessof", "--username", user,
		"--repository", repo, "--path", path, file)
	return strings.TrimSpace(string(out)), err
}

// ---- read operations ----

type RevInfo struct {
	Rev    int64
	Author string
	Date   time.Time
	Msg    string
}

func (s *SVN) Info(ctx context.Context, dir string, rev int64) (RevInfo, error) {
	out, err := s.output(ctx, 30*time.Second, 4<<20, s.Look, "info", "-r", strconv.FormatInt(rev, 10), dir)
	if err != nil {
		return RevInfo{}, err
	}
	lines := strings.SplitN(string(out), "\n", 4)
	ri := RevInfo{Rev: rev}
	if len(lines) > 0 {
		ri.Author = lines[0]
	}
	if len(lines) > 1 && len(lines[1]) >= 25 {
		ri.Date, _ = time.Parse("2006-01-02 15:04:05 -0700", lines[1][:25])
	}
	if len(lines) > 3 {
		ri.Msg = strings.TrimRight(lines[3], "\n")
	}
	return ri, nil
}

type Change struct {
	Action string // A, D, U, _U, UU
	Path   string
	IsDir  bool
}

func (s *SVN) Changed(ctx context.Context, dir string, rev int64) ([]Change, error) {
	out, err := s.output(ctx, 60*time.Second, 32<<20, s.Look, "changed", "-r", strconv.FormatInt(rev, 10), dir)
	if err != nil {
		return nil, err
	}
	var res []Change
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 5 {
			continue
		}
		p := line[4:]
		ch := Change{Action: strings.TrimSpace(line[:3]), Path: "/" + strings.TrimSuffix(p, "/"), IsDir: strings.HasSuffix(p, "/")}
		res = append(res, ch)
	}
	return res, nil
}

type LogPath struct {
	Action   string `xml:"action,attr"`
	Kind     string `xml:"kind,attr"`
	CopyFrom string `xml:"copyfrom-path,attr"`
	CopyRev  string `xml:"copyfrom-rev,attr"`
	Path     string `xml:",chardata"`
}

type LogEntry struct {
	Rev    int64     `xml:"revision,attr"`
	Author string    `xml:"author"`
	Date   time.Time `xml:"date"`
	Msg    string    `xml:"msg"`
	Paths  []LogPath `xml:"paths>path"`
	Hidden bool      `xml:"-"` // message hidden because of partial read access
}

// Log returns up to limit entries for path starting at startRev going backwards.
func (s *SVN) Log(ctx context.Context, dir, path string, startRev int64, limit int) ([]LogEntry, error) {
	if startRev < 1 {
		return nil, nil
	}
	args := s.clientArgs("log", "--xml", "-v", "-r", strconv.FormatInt(startRev, 10)+":1",
		"-l", strconv.Itoa(limit), fileURL(dir, path, startRev))
	out, err := s.output(ctx, 60*time.Second, 32<<20, s.Client, args...)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Entries []LogEntry `xml:"logentry"`
	}
	if err := xml.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("svn log 출력 해석 실패: %w", err)
	}
	return doc.Entries, nil
}

type xmlCommit struct {
	Rev    int64     `xml:"revision,attr"`
	Author string    `xml:"author"`
	Date   time.Time `xml:"date"`
}

type Entry struct {
	Kind   string    `xml:"kind,attr"`
	Name   string    `xml:"name"`
	Size   int64     `xml:"size"`
	Commit xmlCommit `xml:"commit"`
	Path   string    `xml:"-"`
}

func (e Entry) Rev() int64      { return e.Commit.Rev }
func (e Entry) Author() string  { return e.Commit.Author }
func (e Entry) Date() time.Time { return e.Commit.Date }

func (s *SVN) List(ctx context.Context, dir, path string, rev int64) ([]Entry, error) {
	args := s.clientArgs("list", "--xml", fileURL(dir, path, rev))
	out, err := s.output(ctx, 60*time.Second, 32<<20, s.Client, args...)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Entries []Entry `xml:"list>entry"`
	}
	if err := xml.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("svn list 출력 해석 실패: %w", err)
	}
	for i := range doc.Entries {
		doc.Entries[i].Path = joinRepoPath(path, doc.Entries[i].Name)
	}
	return doc.Entries, nil
}

type NodeInfo struct {
	Kind   string    `xml:"kind,attr"`
	Commit xmlCommit `xml:"commit"`
}

func (n NodeInfo) Rev() int64      { return n.Commit.Rev }
func (n NodeInfo) Author() string  { return n.Commit.Author }
func (n NodeInfo) Date() time.Time { return n.Commit.Date }

func (s *SVN) NodeInfo(ctx context.Context, dir, path string, rev int64) (NodeInfo, error) {
	args := s.clientArgs("info", "--xml", fileURL(dir, path, rev))
	out, err := s.output(ctx, 30*time.Second, 1<<20, s.Client, args...)
	if err != nil {
		return NodeInfo{}, err
	}
	var doc struct {
		Entry NodeInfo `xml:"entry"`
	}
	if err := xml.Unmarshal(out, &doc); err != nil {
		return NodeInfo{}, fmt.Errorf("svn info 출력 해석 실패: %w", err)
	}
	return doc.Entry, nil
}

func (s *SVN) FileSize(ctx context.Context, dir, path string, rev int64) (int64, error) {
	out, err := s.output(ctx, 30*time.Second, 4096, s.Look, "filesize", "-r", strconv.FormatInt(rev, 10), dir, path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

// Cat streams file contents (never buffered fully in memory).
func (s *SVN) Cat(ctx context.Context, w io.Writer, dir, path string, rev int64) error {
	return s.stream(ctx, w, nil, s.Look, "cat", "-r", strconv.FormatInt(rev, 10), dir, path)
}

// DiffLines streams `svnlook diff` output line by line to fn. fn returning
// false stops the command early.
func (s *SVN) DiffLines(ctx context.Context, dir string, rev int64, fn func(line string) bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c := s.command(ctx, s.Look, "diff", "--diff-copy-from", "-r", strconv.FormatInt(rev, 10), dir)
	errb := &limitWriter{limit: 64 * 1024}
	c.Stderr = errb
	pipe, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	if err := c.Start(); err != nil {
		return err
	}
	br := bufio.NewReaderSize(pipe, 64*1024)
	stopped := false
	for {
		line, rerr := br.ReadString('\n')
		if len(line) > 0 {
			if !fn(strings.TrimRight(line, "\r\n")) {
				stopped = true
				cancel()
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	io.Copy(io.Discard, pipe)
	werr := c.Wait()
	if werr != nil && !stopped {
		return &cmdError{Tool: "svnlook diff", Stderr: errb.buf.String(), Err: werr}
	}
	return nil
}

// diffSectionPath returns the repository path if line starts a new file section.
func diffSectionPath(line string) (string, bool) {
	for _, p := range []string{"Modified: ", "Added: ", "Deleted: ", "Copied: ", "Property changes on: "} {
		if strings.HasPrefix(line, p) {
			path := strings.TrimPrefix(line, p)
			if i := strings.Index(path, " (from rev "); i > 0 && p == "Copied: " {
				path = path[:i]
			}
			return "/" + strings.TrimPrefix(path, "/"), true
		}
	}
	return "", false
}

// ---- cheap metadata read directly from FSFS files (used for list pages) ----

func fsfsYoungest(dir string) int64 {
	b, err := os.ReadFile(filepath.Join(dir, "db", "current"))
	if err != nil {
		return -1
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return -1
	}
	n, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil {
		return -1
	}
	return n
}

func isRepoDir(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "format"))
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	st, err = os.Stat(filepath.Join(dir, "db"))
	return err == nil && st.IsDir()
}
