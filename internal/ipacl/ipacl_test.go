package ipacl

import (
	"strconv"
	"strings"
	"testing"
)

func TestParseNormalizes(t *testing.T) {
	l, err := Parse("10.1.2.3, 192.168.0.77/24 # office\n\n2001:DB8::1/32;::ffff:172.16.0.5 10.1.2.3\r\n  # only a comment\n::ffff:10.9.0.0/112")
	if err != nil {
		t.Fatal(err)
	}
	want := "10.1.2.3\n192.168.0.0/24\n2001:db8::/32\n172.16.0.5\n10.9.0.0/16"
	if got := l.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	if got := strings.Join(l.CIDRs(), " "); got != "10.1.2.3/32 192.168.0.0/24 2001:db8::/32 172.16.0.5/32 10.9.0.0/16" {
		t.Fatalf("CIDRs() = %q", got)
	}
	if l.Len() != 5 || l.Empty() {
		t.Fatalf("Len() = %d", l.Len())
	}
	again, err := Parse(l.String())
	if err != nil || again.String() != l.String() {
		t.Fatalf("round trip: %v %q", err, again.String())
	}
}

func TestParseEmpty(t *testing.T) {
	for _, s := range []string{"", "  \n\t", "# nothing\n#"} {
		l, err := Parse(s)
		if err != nil || !l.Empty() || l.String() != "" {
			t.Fatalf("Parse(%q) = %v, %v", s, l, err)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, s := range []string{"10.0.0", "10.0.0.1/33", "example.com", "fe80::1%eth0", "1.2.3.4/abc", "::ffff:1.2.3.4/64", "'10.0.0.1'", "10.0.0.1)"} {
		if _, err := Parse("127.0.0.1\n" + s); err == nil {
			t.Errorf("Parse(%q) accepted", s)
		} else if !strings.Contains(err.Error(), "2") {
			t.Errorf("Parse(%q) error lacks line number: %v", s, err)
		}
	}
	var b strings.Builder
	for i := 0; i <= MaxEntries; i++ {
		b.WriteString("10.0." + strconv.Itoa(i/250) + "." + strconv.Itoa(i%250) + "\n")
	}
	if _, err := Parse(b.String()); err == nil {
		t.Fatal("more than MaxEntries accepted")
	}
}

func TestContains(t *testing.T) {
	l := MustParse("192.168.1.0/24\n10.0.0.5\n2001:db8::/48")
	cases := map[string]bool{
		"192.168.1.1":              true,
		"192.168.1.255":            true,
		"192.168.2.1":              false,
		"10.0.0.5":                 true,
		"10.0.0.6":                 false,
		"::ffff:192.168.1.9":       true,
		"2001:db8:0:1::5":          true,
		"2001:db8:1::5":            false,
		"fe80::1%eth0":             false,
		"not-an-ip":                false,
		"":                         false,
		"::ffff:10.0.0.5":          true,
		"2001:db8::1%en0":          true,
		"0000:0000::ffff:10.0.0.5": true,
	}
	for ip, want := range cases {
		if got := l.Contains(ip); got != want {
			t.Errorf("Contains(%q) = %v, want %v", ip, got, want)
		}
	}
	var empty List
	if empty.Contains("10.0.0.5") {
		t.Fatal("empty list contains an address")
	}
}

func TestMergeAndLoopback(t *testing.T) {
	m := Merge(MustParse("10.0.0.1\n10.0.0.2"), MustParse("10.0.0.2\n10.0.0.3"), List{})
	if m.String() != "10.0.0.1\n10.0.0.2\n10.0.0.3" {
		t.Fatalf("Merge = %q", m.String())
	}
	for _, ip := range []string{"127.0.0.1", "127.1.2.3", "::1"} {
		if !Loopback.Contains(ip) {
			t.Errorf("Loopback does not contain %s", ip)
		}
	}
	if Loopback.Contains("10.0.0.1") {
		t.Fatal("Loopback contains 10.0.0.1")
	}
}
