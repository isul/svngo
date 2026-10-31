package config

import (
	"strings"
	"testing"
)

func TestParseTrustedProxies(t *testing.T) {
	l, err := ParseTrustedProxies("")
	if err != nil || !l.Empty() {
		t.Fatalf("empty: %v %v", l, err)
	}
	l, err = ParseTrustedProxies("172.22.0.1, 10.8.0.0/24 fd00::/8")
	if err != nil || strings.Join(l.CIDRs(), " ") != "172.22.0.1/32 10.8.0.0/24 fd00::/8" {
		t.Fatalf("list: %v %v", l.CIDRs(), err)
	}
	for _, bad := range []string{"0.0.0.0/0", "10.0.0.1, ::/0", "proxy.local"} {
		if _, err := ParseTrustedProxies(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
