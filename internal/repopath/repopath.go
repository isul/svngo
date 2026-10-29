// Package repopath handles paths inside a Subversion repository ("/a/b" form).
package repopath

import (
	"strings"
	"unicode/utf8"

	"svngo/internal/i18n"
)

// Clean normalizes a path inside a repository to "/a/b" form.
// It rejects control characters and "." / ".." segments.
func Clean(p string) (string, error) {
	if !utf8.ValidString(p) {
		return "", i18n.E("path.err.utf8")
	}
	if len(p) > 4096 {
		return "", i18n.E("path.err.too_long")
	}
	var segs []string
	for _, s := range strings.Split(p, "/") {
		if s == "" {
			continue
		}
		if s == "." || s == ".." {
			return "", i18n.E("path.err.dots")
		}
		for _, r := range s {
			if r < 0x20 || r == 0x7f {
				return "", i18n.E("path.err.control")
			}
		}
		segs = append(segs, s)
	}
	return "/" + strings.Join(segs, "/"), nil
}

func Parent(p string) string {
	if p == "/" || p == "" {
		return "/"
	}
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

func Base(p string) string {
	if p == "/" {
		return "/"
	}
	return p[strings.LastIndex(p, "/")+1:]
}

func Join(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}
