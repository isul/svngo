package svn

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestSelfTestAuthz(t *testing.T) {
	ctx := context.Background()
	for _, bin := range []string{"", "/bin/true", "/nonexistent/svngo-authz"} {
		err := (&Tools{Authz: bin}).SelfTestAuthz(ctx, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "authz 검증 도구") {
			t.Errorf("validator %q: err = %v", bin, err)
		}
	}
	for _, name := range []string{"svngo-authz", "svnauthz"} {
		if p, err := exec.LookPath(name); err == nil {
			if err := (&Tools{Authz: p}).SelfTestAuthz(ctx, t.TempDir()); err != nil {
				t.Fatalf("%s: %v", p, err)
			}
			return
		}
	}
	t.Log("svnauthz not installed: positive case skipped")
}

func TestFileURL(t *testing.T) {
	if got := FileURL("/data/repos/a b", "/trunk/한 글", 3); got != "file:///data/repos/a%20b/trunk/%ED%95%9C%20%EA%B8%80@3" {
		t.Fatalf("FileURL = %s", got)
	}
	if got := FileURL("/r", "/", -1); got != "file:///r" {
		t.Fatalf("FileURL = %s", got)
	}
}
