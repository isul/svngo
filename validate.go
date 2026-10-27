package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	reRepoName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	reUsername  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)
	reGroupName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	reBackup    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,150}\.svndump\.gz$`)
)

func validRepoName(s string) error {
	if !reRepoName.MatchString(s) || strings.HasSuffix(s, ".") {
		return errors.New("저장소 이름은 영문/숫자로 시작하고 영문, 숫자, '.', '_', '-' 만 사용할 수 있습니다 (최대 64자)")
	}
	return nil
}

func validUsername(s string) error {
	if !reUsername.MatchString(s) {
		return errors.New("사용자 이름은 영문/숫자로 시작하고 영문, 숫자, '.', '_', '-', '@' 만 사용할 수 있습니다 (최대 64자)")
	}
	return nil
}

func validGroupName(s string) error {
	if !reGroupName.MatchString(s) {
		return errors.New("그룹 이름은 영문/숫자로 시작하고 영문, 숫자, '.', '_', '-' 만 사용할 수 있습니다 (최대 64자)")
	}
	return nil
}

func validBackupName(s string) error {
	if !reBackup.MatchString(s) || strings.Contains(s, "..") {
		return errors.New("잘못된 백업 파일 이름입니다")
	}
	return nil
}

func validPassword(pw string) error {
	if utf8.RuneCountInString(pw) < 8 {
		return errors.New("비밀번호는 8자 이상이어야 합니다")
	}
	if len(pw) > 72 {
		return errors.New("비밀번호는 72바이트 이하여야 합니다")
	}
	if !utf8.ValidString(pw) {
		return errors.New("비밀번호에 잘못된 문자가 있습니다")
	}
	for _, r := range pw {
		if unicode.IsControl(r) {
			return errors.New("비밀번호에 제어 문자를 사용할 수 없습니다")
		}
	}
	return nil
}

// cleanRepoPath normalizes a path inside a repository to "/a/b" form.
// It rejects control characters and "." / ".." segments.
func cleanRepoPath(p string) (string, error) {
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

// validRulePath validates a path used in an authz section header.
func validRulePath(p string) (string, error) {
	c, err := cleanRepoPath(strings.TrimSpace(p))
	if err != nil {
		return "", err
	}
	if strings.ContainsAny(c, "[]%\\") {
		return "", fmt.Errorf("권한 경로에 '[', ']', '%%', '\\' 문자를 사용할 수 없습니다")
	}
	for _, seg := range strings.Split(c, "/") {
		if seg != strings.TrimSpace(seg) {
			return "", errors.New("권한 경로 구성요소의 앞뒤에 공백을 둘 수 없습니다")
		}
	}
	return c, nil
}

func validAccess(a string) error {
	switch a {
	case "r", "rw", "none":
		return nil
	}
	return errors.New("권한은 읽기(r), 읽기/쓰기(rw), 접근 금지(none) 중 하나여야 합니다")
}

func parentPath(p string) string {
	if p == "/" || p == "" {
		return "/"
	}
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

func baseName(p string) string {
	if p == "/" {
		return "/"
	}
	return p[strings.LastIndex(p, "/")+1:]
}

func joinRepoPath(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// singleLine removes control characters (newlines etc.) from free text fields.
func singleLine(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return clip(s, n)
}
