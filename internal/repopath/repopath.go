// Package repopath handles paths inside a Subversion repository ("/a/b" form).
package repopath

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// Clean normalizes a path inside a repository to "/a/b" form.
// It rejects control characters and "." / ".." segments.
func Clean(p string) (string, error) {
	if !utf8.ValidString(p) {
		return "", errors.New("경로가 올바른 UTF-8 이 아닙니다")
	}
	if len(p) > 4096 {
		return "", errors.New("경로가 너무 깁니다")
	}
	var segs []string
	for _, s := range strings.Split(p, "/") {
		if s == "" {
			continue
		}
		if s == "." || s == ".." {
			return "", errors.New("경로에 '.' 또는 '..' 를 사용할 수 없습니다")
		}
		for _, r := range s {
			if r < 0x20 || r == 0x7f {
				return "", errors.New("경로에 제어 문자를 사용할 수 없습니다")
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
