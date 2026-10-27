// Package config loads runtime settings from environment variables.
package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"svngo/internal/validate"
)

// Config holds runtime settings loaded from environment variables.
type Config struct {
	ReposDir  string
	ConfigDir string
	DBDir     string
	BackupDir string
	RunDir    string

	Listen string // Go admin server listen address (behind Apache)

	ManageApache bool
	ApacheBin    string
	ApacheConf   string
	HTTPPort     string
	HTTPSPort    string
	TLSCert      string
	TLSKey       string
	ServerName   string

	// Apache request limits for /svn (bytes, 0 = unlimited)
	SvnMaxBody int64
	SvnMaxXML  int64

	AdminUser     string
	AdminPassword string

	BcryptCost   int
	CookieSecure string // auto | always | never
	SessionTTL   time.Duration

	SvnAdmin string
	SvnLook  string
	Svn      string
	SvnAuthz string // svnauthz compatible validator (validate / accessof)
}

func Env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func EnvBool(key string, def bool) bool {
	v := strings.ToLower(Env(key, ""))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func findBin(envKey string, names ...string) string {
	if v := Env(envKey, ""); v != "" {
		return v
	}
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

func Load() (*Config, error) {
	c := &Config{
		ReposDir:     Env("SVNGO_REPOS_DIR", "/data/repos"),
		ConfigDir:    Env("SVNGO_CONFIG_DIR", "/data/config"),
		DBDir:        Env("SVNGO_DB_DIR", "/data/db"),
		BackupDir:    Env("SVNGO_BACKUP_DIR", "/data/backups"),
		RunDir:       Env("SVNGO_RUN_DIR", filepath.Join(os.TempDir(), "svngo-run")),
		Listen:       Env("SVNGO_LISTEN", "127.0.0.1:9000"),
		ManageApache: EnvBool("SVNGO_MANAGE_APACHE", false),
		ApacheBin:    Env("SVNGO_APACHE_BIN", "/usr/sbin/httpd"),
		ApacheConf:   Env("SVNGO_APACHE_CONF", "/etc/svngo/httpd.conf"),
		HTTPPort:     Env("SVNGO_HTTP_PORT", "8080"),
		HTTPSPort:    Env("SVNGO_HTTPS_PORT", "8443"),
		ServerName:   Env("SVNGO_SERVER_NAME", "localhost"),
		AdminUser:    Env("ADMIN_USERNAME", "admin"),
		CookieSecure: strings.ToLower(Env("SVNGO_COOKIE_SECURE", "auto")),
		SvnAdmin:     findBin("SVNGO_SVNADMIN", "svnadmin"),
		SvnLook:      findBin("SVNGO_SVNLOOK", "svnlook"),
		Svn:          findBin("SVNGO_SVN", "svn"),
		SvnAuthz:     findBin("SVNGO_SVNAUTHZ", "svngo-authz", "svnauthz"),
	}
	c.TLSCert = Env("SVNGO_TLS_CERT", filepath.Join(c.ConfigDir, "tls", "server.crt"))
	c.TLSKey = Env("SVNGO_TLS_KEY", filepath.Join(c.ConfigDir, "tls", "server.key"))

	c.AdminPassword = os.Getenv("ADMIN_PASSWORD")
	if f := Env("ADMIN_PASSWORD_FILE", ""); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("ADMIN_PASSWORD_FILE 읽기 실패: %w", err)
		}
		c.AdminPassword = strings.TrimRight(string(b), "\r\n")
	}

	// Apache verifies the hash on every SVN HTTP request, so the cost directly
	// affects commit/checkout latency (htpasswd -B uses 5 by default).
	cost, err := strconv.Atoi(Env("SVNGO_BCRYPT_COST", "8"))
	if err != nil || cost < 5 || cost > 14 {
		return nil, fmt.Errorf("SVNGO_BCRYPT_COST 는 5~14 사이여야 합니다")
	}
	c.BcryptCost = cost

	// Apache accepts at most 2 GiB - 1 for these directives.
	for _, l := range []struct {
		key, def string
		dst      *int64
	}{
		{"SVNGO_SVN_MAX_BODY_MB", "1024", &c.SvnMaxBody},
		{"SVNGO_SVN_MAX_XML_MB", "64", &c.SvnMaxXML},
	} {
		mb, err := strconv.Atoi(Env(l.key, l.def))
		if err != nil || mb < 0 || mb > 2047 {
			return nil, fmt.Errorf("%s 는 0(무제한)~2047 사이여야 합니다", l.key)
		}
		*l.dst = int64(mb) << 20
	}

	hours, err := strconv.Atoi(Env("SVNGO_SESSION_HOURS", "12"))
	if err != nil || hours < 1 || hours > 720 {
		return nil, fmt.Errorf("SVNGO_SESSION_HOURS 는 1~720 사이여야 합니다")
	}
	c.SessionTTL = time.Duration(hours) * time.Hour

	switch c.CookieSecure {
	case "auto", "always", "never":
	default:
		return nil, fmt.Errorf("SVNGO_COOKIE_SECURE 는 auto|always|never 중 하나여야 합니다")
	}
	if err := validate.Username(c.AdminUser); err != nil {
		return nil, fmt.Errorf("ADMIN_USERNAME: %w", err)
	}
	for _, d := range []*string{&c.ReposDir, &c.ConfigDir, &c.DBDir, &c.BackupDir, &c.RunDir} {
		abs, err := filepath.Abs(*d)
		if err != nil {
			return nil, err
		}
		*d = abs
	}
	if c.SvnAdmin == "" || c.SvnLook == "" || c.Svn == "" {
		return nil, fmt.Errorf("svnadmin / svnlook / svn 실행 파일을 찾을 수 없습니다")
	}
	if c.SvnAuthz == "" {
		return nil, fmt.Errorf("authz 검증 도구(svngo-authz 또는 svnauthz)를 찾을 수 없습니다. SVNGO_SVNAUTHZ 로 경로를 지정하세요")
	}
	return c, nil
}

func (c *Config) HtpasswdFile() string { return filepath.Join(c.ConfigDir, "htpasswd") }
func (c *Config) AuthzFile() string    { return filepath.Join(c.ConfigDir, "authz") }
func (c *Config) DBFile() string       { return filepath.Join(c.DBDir, "svngo.db") }
func (c *Config) TmpDir() string       { return filepath.Join(c.ReposDir, ".svngo-tmp") }

func (c *Config) EnsureDirs() error {
	for _, d := range []string{c.ReposDir, c.ConfigDir, c.DBDir, c.BackupDir, c.RunDir, c.TmpDir()} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return fmt.Errorf("디렉터리 생성 실패 %s: %w", d, err)
		}
		probe := filepath.Join(d, ".svngo-write-test")
		if err := os.WriteFile(probe, nil, 0o600); err != nil {
			return fmt.Errorf("디렉터리 쓰기 권한 없음 %s (볼륨 소유권을 확인하세요): %w", d, err)
		}
		os.Remove(probe)
	}
	return nil
}
