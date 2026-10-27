package main

import (
	"strings"
	"testing"
)

func TestAPR1Vectors(t *testing.T) {
	// generated with: openssl passwd -apr1 -salt <salt> <password>
	cases := []struct{ pw, salt, want string }{
		{"secret-pass", "Ab3dEfGh", "$apr1$Ab3dEfGh$o.h.BiUJ8Lr.RdMYeOtOA/"},
		{"p", "xy", "$apr1$xy$q/TPHhf1FfQqXeFUwZC/b/"},
		{"한글비밀번호123", "12345678", "$apr1$12345678$tWatTV0BHwaUBgV1T3nVG1"},
	}
	for _, c := range cases {
		if got := apr1(c.pw, c.salt); got != c.want {
			t.Errorf("apr1(%q,%q) = %s, want %s", c.pw, c.salt, got, c.want)
		}
		if !verifyPassword(c.want, c.pw) {
			t.Errorf("verifyPassword failed for %s", c.want)
		}
		if verifyPassword(c.want, c.pw+"x") {
			t.Errorf("verifyPassword accepted wrong password for %s", c.want)
		}
		if !validStoredHash(c.want) || hashType(c.want) != "apr1" {
			t.Errorf("hash %s not recognised", c.want)
		}
	}
}

func TestBcryptHtpasswdFormat(t *testing.T) {
	h, err := hashPassword("Correct-Horse-1", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$2y$05$") || len(h) != 60 {
		t.Fatalf("unexpected hash format %q", h)
	}
	if !validStoredHash(h) || !verifyPassword(h, "Correct-Horse-1") || verifyPassword(h, "wrong") {
		t.Fatal("bcrypt verify failed")
	}
}

func TestParseHtpasswd(t *testing.T) {
	in := `# comment
alice:$apr1$Ab3dEfGh$o.h.BiUJ8Lr.RdMYeOtOA/
bob:$2y$05$abcdefghijklmnopqrstuuJ9rH6g2V0OB9n6vYdRAEbSxHN0cFJ1y
carol:{SHA}W6ph5Mm5Pz8GgiULbPgzG37mj9g=
bad user:$apr1$Ab3dEfGh$o.h.BiUJ8Lr.RdMYeOtOA/
alice:$apr1$Ab3dEfGh$o.h.BiUJ8Lr.RdMYeOtOA/
noseparator
`
	ok, rejected, err := parseHtpasswd(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(ok) != 2 || ok[0].User != "alice" || ok[1].User != "bob" {
		t.Fatalf("unexpected entries: %+v", ok)
	}
	if len(rejected) != 4 {
		t.Fatalf("expected 4 rejected, got %v", rejected)
	}
}

func TestValidation(t *testing.T) {
	for _, n := range []string{"project", "a", "My.Repo-1_x"} {
		if validRepoName(n) != nil {
			t.Errorf("repo name %q should be valid", n)
		}
	}
	for _, n := range []string{"", ".", "..", "../etc", "a/b", "-x", ".hidden", "name.", "a b", "a\x00", strings.Repeat("a", 65), "repo:1"} {
		if validRepoName(n) == nil {
			t.Errorf("repo name %q should be invalid", n)
		}
	}
	for _, n := range []string{"alice", "john.doe@corp", "u_1-2"} {
		if validUsername(n) != nil {
			t.Errorf("user %q should be valid", n)
		}
	}
	for _, n := range []string{"", "@grp", "$authenticated", "*", "a:b", "a b", "a=b", "~x", "-n", "a\nb"} {
		if validUsername(n) == nil {
			t.Errorf("user %q should be invalid", n)
		}
	}
	paths := map[string]string{"": "/", "/": "/", "trunk": "/trunk", "/trunk//a/": "/trunk/a", "/한글 폴더/x": "/한글 폴더/x"}
	for in, want := range paths {
		got, err := cleanRepoPath(in)
		if err != nil || got != want {
			t.Errorf("cleanRepoPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, p := range []string{"/a/../b", "..", "/./x", "/a\nb", "/a\x00"} {
		if _, err := cleanRepoPath(p); err == nil {
			t.Errorf("cleanRepoPath(%q) should fail", p)
		}
	}
	for _, p := range []string{"/a]b", "/[x", "/a%b", "/ a"} {
		if _, err := validRulePath(p); err == nil {
			t.Errorf("validRulePath(%q) should fail", p)
		}
	}
	for _, n := range []string{"../x.svndump.gz", "a/b.svndump.gz", "x.tar.gz", ".x.svndump.gz"} {
		if validBackupName(n) == nil {
			t.Errorf("backup name %q should be invalid", n)
		}
	}
	if validBackupName("proj-20260101-120000-r5.svndump.gz") != nil {
		t.Error("valid backup name rejected")
	}
}

func TestDiffParser(t *testing.T) {
	lines := []string{
		"Modified: trunk/a.txt",
		"===================================================================",
		"--- trunk/a.txt\t(rev 1)",
		"+++ trunk/a.txt\t(rev 2)",
		"@@ -1 +1,2 @@",
		" hello",
		"+world",
		"",
		"Property changes on: trunk/dir",
		"___________________________________________________________________",
		"Added: svn:ignore",
		"## -0,0 +1 ##",
		"+*.o",
		"Added: secret/key.txt",
		"===================================================================",
		"+top secret",
	}
	var headers []string
	content := map[string][]string{}
	cur := ""
	dp := &diffParser{
		onHeader: func(h, p string) { headers = append(headers, p); cur = p },
		onLine:   func(l string) { content[cur] = append(content[cur], l) },
	}
	for _, l := range lines {
		dp.feed(l)
	}
	dp.flush()
	want := []string{"/trunk/a.txt", "/trunk/dir", "/secret/key.txt"}
	if strings.Join(headers, ",") != strings.Join(want, ",") {
		t.Fatalf("headers = %v, want %v", headers, want)
	}
	found := false
	for _, l := range content["/trunk/dir"] {
		if l == "Added: svn:ignore" {
			found = true
		}
	}
	if !found {
		t.Fatal("property line inside section was treated as a header")
	}
}
