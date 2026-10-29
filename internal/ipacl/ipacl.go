// Package ipacl parses and evaluates client IP allow lists made of addresses
// and CIDR networks.
package ipacl

import (
	"errors"
	"net/netip"
	"slices"
	"strings"

	"svngo/internal/i18n"
)

var errInvalid = errors.New("invalid address")

// MaxEntries limits one list (it ends up in an Apache expression).
const MaxEntries = 200

// List is a normalized, de-duplicated allow list. The zero value is empty.
type List struct {
	prefixes []netip.Prefix
}

// Parse reads entries separated by newlines, commas or spaces. Text after
// "#" on a line is a comment. Single addresses become /32 or /128 networks
// and host bits of networks are cleared.
func Parse(text string) (List, error) {
	var l List
	seen := map[netip.Prefix]bool{}
	for n, line := range strings.Split(text, "\n") {
		line, _, _ = strings.Cut(line, "#")
		for _, f := range strings.FieldsFunc(line, func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\r'
		}) {
			p, err := parseEntry(f)
			if err != nil {
				return List{}, i18n.E("allowip.err.entry", n+1, f)
			}
			if seen[p] {
				continue
			}
			seen[p] = true
			l.prefixes = append(l.prefixes, p)
			if len(l.prefixes) > MaxEntries {
				return List{}, i18n.E("allowip.err.too_many", MaxEntries)
			}
		}
	}
	return l, nil
}

func parseEntry(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		if p.Addr().Zone() != "" {
			return netip.Prefix{}, errInvalid
		}
		if p.Addr().Is4In6() {
			bits := p.Bits() - 96
			if bits < 0 {
				return netip.Prefix{}, errInvalid
			}
			p = netip.PrefixFrom(p.Addr().Unmap(), bits)
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	if a.Zone() != "" {
		return netip.Prefix{}, errInvalid
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// MustParse is Parse for trusted constants; it panics on error.
func MustParse(text string) List {
	l, err := Parse(text)
	if err != nil {
		panic(err)
	}
	return l
}

// Empty reports whether the list has no entries.
func (l List) Empty() bool { return len(l.prefixes) == 0 }

// Len is the number of entries.
func (l List) Len() int { return len(l.prefixes) }

// Contains reports whether ip (textual form, IPv4-mapped IPv6 allowed) falls
// into one of the entries.
func (l List) Contains(ip string) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	return l.ContainsAddr(a)
}

// ContainsAddr is Contains for a parsed address.
func (l List) ContainsAddr(a netip.Addr) bool {
	a = a.WithZone("").Unmap()
	for _, p := range l.prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Entries returns the entries in canonical text form (addresses without /32
// or /128).
func (l List) Entries() []string {
	res := make([]string, len(l.prefixes))
	for i, p := range l.prefixes {
		res[i] = entryString(p)
	}
	return res
}

// CIDRs returns every entry as a network ("10.0.0.1/32"), the form used in
// generated Apache configuration.
func (l List) CIDRs() []string {
	res := make([]string, len(l.prefixes))
	for i, p := range l.prefixes {
		res[i] = p.String()
	}
	return res
}

func entryString(p netip.Prefix) string {
	if p.IsSingleIP() {
		return p.Addr().String()
	}
	return p.String()
}

// String is the canonical storage form: one entry per line.
func (l List) String() string { return strings.Join(l.Entries(), "\n") }

// Merge returns the union of the lists, keeping the order of first appearance.
func Merge(lists ...List) List {
	var res List
	for _, l := range lists {
		for _, p := range l.prefixes {
			if !slices.Contains(res.prefixes, p) {
				res.prefixes = append(res.prefixes, p)
			}
		}
	}
	return res
}

// Loopback is always allowed: container health checks and local tools reach
// the server over the loopback interface.
var Loopback = MustParse("127.0.0.0/8\n::1")
