package passwd

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
		if !Verify(c.want, c.pw) {
			t.Errorf("Verify failed for %s", c.want)
		}
		if Verify(c.want, c.pw+"x") {
			t.Errorf("Verify accepted wrong password for %s", c.want)
		}
		if !ValidStoredHash(c.want) || HashType(c.want) != "apr1" {
			t.Errorf("hash %s not recognised", c.want)
		}
	}
}

func TestBcryptHtpasswdFormat(t *testing.T) {
	h, err := Hash("Correct-Horse-1", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$2y$05$") || len(h) != 60 {
		t.Fatalf("unexpected hash format %q", h)
	}
	if !ValidStoredHash(h) || !Verify(h, "Correct-Horse-1") || Verify(h, "wrong") {
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
	ok, rejected, err := ParseHtpasswd(strings.NewReader(in))
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
