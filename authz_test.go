package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func testPolicy() *Policy {
	p := &Policy{
		Users:      map[string]string{"alice": "x", "bob": "x", "carol": "x", "dave": "x"},
		Groups:     map[string][]string{"dev": {"alice", "bob"}, "qa": {"carol"}},
		userGroups: map[string]map[string]bool{"alice": {"dev": true}, "bob": {"dev": true}, "carol": {"qa": true}},
		Repos:      map[string]bool{"proj": true, "off": false, "pub": true},
		Rules:      map[string]map[string][]Rule{},
	}
	add := func(repo, path, typ, subj, acc string) {
		if p.Rules[repo] == nil {
			p.Rules[repo] = map[string][]Rule{}
		}
		p.Rules[repo][path] = append(p.Rules[repo][path], Rule{Repo: repo, Path: path, SubjectType: typ, Subject: subj, Access: acc})
	}
	add("proj", "/", "group", "dev", "rw")
	add("proj", "/", "group", "qa", "r")
	add("proj", "/trunk/secret", "all", "", "none")
	add("proj", "/trunk/secret", "user", "alice", "r")
	add("proj", "/trunk/docs", "user", "carol", "rw")
	add("proj", "/trunk/docs", "user", "bob", "none")
	add("proj", "/branches/rel", "user", "dave", "r")
	add("pub", "/", "authenticated", "", "r")
	add("pub", "/", "user", "dave", "rw")
	return p
}

func TestPolicyAccess(t *testing.T) {
	p := testPolicy()
	cases := []struct{ repo, path, user, want string }{
		{"proj", "/", "alice", "rw"},
		{"proj", "/trunk/a.c", "bob", "rw"},
		{"proj", "/", "carol", "r"},
		{"proj", "/", "dave", "no"},
		{"proj", "/trunk/secret", "bob", "no"},
		{"proj", "/trunk/secret/x", "alice", "r"},
		{"proj", "/trunk/docs/readme", "carol", "rw"},
		{"proj", "/trunk/docs", "bob", "no"},
		{"proj", "/trunk/docs", "alice", "rw"},
		{"proj", "/branches/rel/x", "dave", "r"},
		{"off", "/", "alice", "no"},
		{"pub", "/x", "carol", "r"},
		{"pub", "/x", "dave", "rw"},
		{"pub", "/x", "nobody", "no"},
	}
	for _, c := range cases {
		if got := p.Access(c.repo, c.path, c.user); got != c.want {
			t.Errorf("Access(%s,%s,%s) = %s, want %s", c.repo, c.path, c.user, got, c.want)
		}
	}
	if !p.Traversable("proj", "/", "dave") || !p.Traversable("proj", "/branches", "dave") {
		t.Error("dave should be able to traverse to /branches/rel")
	}
	if p.Traversable("proj", "/trunk", "dave") {
		t.Error("dave must not traverse /trunk")
	}
}

// TestPolicyMatchesLibsvn compares SvnGo's evaluator with libsvn (svnauthz)
// on the generated authz file.
func TestPolicyMatchesLibsvn(t *testing.T) {
	tool := firstBin("svngo-authz", "svnauthz")
	if tool == "" {
		t.Skip("svnauthz not installed")
	}
	p := testPolicy()
	file := filepath.Join(t.TempDir(), "authz")
	if err := os.WriteFile(file, p.Authz(), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &SVN{Authz: tool, env: os.Environ()}
	ctx := context.Background()
	if err := s.ValidateAuthz(ctx, file); err != nil {
		t.Fatalf("generated authz invalid: %v\n%s", err, p.Authz())
	}
	for _, repo := range []string{"proj", "off", "pub"} {
		for _, path := range []string{"/", "/trunk", "/trunk/secret", "/trunk/secret/x", "/trunk/docs", "/trunk/docs/a", "/branches", "/branches/rel", "/x"} {
			for _, u := range []string{"alice", "bob", "carol", "dave"} {
				want := p.Access(repo, path, u)
				got, err := s.AccessOf(ctx, file, repo, path, u)
				if err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Errorf("%s:%s %s: svngo=%s libsvn=%s", repo, path, u, want, got)
				}
			}
		}
	}
}

func firstBin(names ...string) string {
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}
