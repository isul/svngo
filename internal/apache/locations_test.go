package apache

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"svngo/internal/config"
	"svngo/internal/logring"
)

func TestLocationsConf(t *testing.T) {
	got, err := LocationsConf(nil)
	if err != nil || strings.Contains(got, "<Location") {
		t.Fatalf("no locations: %q %v", got, err)
	}
	got, err = LocationsConf([]string{"android", "hw"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<Location "/android">`, `SVNParentPath "${SVNGO_REPOS_DIR}/android"`,
		`<Location "/hw">`, `SVNParentPath "${SVNGO_REPOS_DIR}/hw"`,
		`AuthzSVNAccessFile "${SVNGO_CONFIG_DIR}/authz"`, `AuthUserFile "${SVNGO_CONFIG_DIR}/htpasswd"`,
		"SVNPathAuthz short_circuit", "Require valid-user", "LimitRequestBody ${SVNGO_SVN_MAX_BODY}",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "</Location>"); n != 2 {
		t.Fatalf("%d blocks", n)
	}
	for _, bad := range []string{"admin", "svn", "a b", `x"y`, "../x", "x\nRequire all granted", ""} {
		if _, err := LocationsConf([]string{bad}); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestApplyLocations(t *testing.T) {
	run := t.TempDir()
	cfg := &config.Config{RunDir: run, ApacheConf: filepath.Join(run, "httpd.conf")}
	if err := WriteLocations(run, nil); err != nil {
		t.Fatal(err)
	}
	live := LocationsFile(run)

	out := filepath.Join(t.TempDir(), "seen")
	cfg.ApacheBin = fakeHttpd(t, `cat "$SVNGO_SVN_LOCATIONS_CONF" > `+out)
	ap := New(cfg, logring.New(10))
	names := []string{"android"}
	if err := ap.ApplyLocations(context.Background(), names); err != nil {
		t.Fatal(err)
	}
	want, _ := LocationsConf(names)
	seen, _ := os.ReadFile(out)
	now, _ := os.ReadFile(live)
	if string(now) != want || string(seen) != want {
		t.Fatalf("live=%q tested=%q", now, seen)
	}
	if _, err := os.Stat(live + ".new"); !os.IsNotExist(err) {
		t.Fatal("temporary locations file left behind")
	}

	cfg.ApacheBin = fakeHttpd(t, "echo broken; exit 1")
	ap = New(cfg, logring.New(10))
	if err := ap.ApplyLocations(context.Background(), []string{"android", "hw"}); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("expected config test failure, got %v", err)
	}
	if b, _ := os.ReadFile(live); string(b) != want {
		t.Fatal("live locations changed after a failed test")
	}
	if err := ap.ApplyLocations(context.Background(), names); err != nil {
		t.Fatalf("unchanged locations re-tested: %v", err)
	}
	if err := ap.ApplyLocations(context.Background(), []string{"bad name"}); err == nil {
		t.Fatal("invalid name accepted")
	}
}
