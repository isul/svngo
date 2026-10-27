package validate

import (
	"strings"
	"testing"
)

func TestNames(t *testing.T) {
	for _, n := range []string{"project", "a", "My.Repo-1_x"} {
		if RepoName(n) != nil {
			t.Errorf("repo name %q should be valid", n)
		}
	}
	for _, n := range []string{"", ".", "..", "../etc", "a/b", "-x", ".hidden", "name.", "a b", "a\x00", strings.Repeat("a", 65), "repo:1"} {
		if RepoName(n) == nil {
			t.Errorf("repo name %q should be invalid", n)
		}
	}
	for _, n := range []string{"alice", "john.doe@corp", "u_1-2"} {
		if Username(n) != nil {
			t.Errorf("user %q should be valid", n)
		}
	}
	for _, n := range []string{"", "@grp", "$authenticated", "*", "a:b", "a b", "a=b", "~x", "-n", "a\nb"} {
		if Username(n) == nil {
			t.Errorf("user %q should be invalid", n)
		}
	}
	for _, n := range []string{"../x.svndump.gz", "a/b.svndump.gz", "x.tar.gz", ".x.svndump.gz"} {
		if BackupName(n) == nil {
			t.Errorf("backup name %q should be invalid", n)
		}
	}
	if BackupName("proj-20260101-120000-r5.svndump.gz") != nil {
		t.Error("valid backup name rejected")
	}
}

func TestRulePath(t *testing.T) {
	for _, p := range []string{"/a]b", "/[x", "/a%b", "/ a", "/a/../b"} {
		if _, err := RulePath(p); err == nil {
			t.Errorf("RulePath(%q) should fail", p)
		}
	}
	if got, err := RulePath(" trunk//docs/ "); err != nil || got != "/trunk/docs" {
		t.Errorf("RulePath = %q, %v", got, err)
	}
}

func TestPassword(t *testing.T) {
	for _, pw := range []string{"short", strings.Repeat("가", 25), "pass\nword1"} {
		if Password(pw) == nil {
			t.Errorf("password %q should be invalid", pw)
		}
	}
	if Password("Correct-Horse-1") != nil {
		t.Error("valid password rejected")
	}
}
