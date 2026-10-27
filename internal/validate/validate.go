// Package validate checks user supplied names, passwords and authz values and
// sanitizes free text.
package validate

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"svngo/internal/repopath"
)

var (
	reRepoName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	reUsername  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)
	reGroupName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	reBackup    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,150}\.svndump\.gz$`)
)

func RepoName(s string) error {
	if !reRepoName.MatchString(s) || strings.HasSuffix(s, ".") {
		return errors.New("저장소 이름은 영문/숫자로 시작하고 영문, 숫자, '.', '_', '-' 만 사용할 수 있습니다 (최대 64자)")
	}
	return nil
}

func Username(s string) error {
	if !reUsername.MatchString(s) {
		return errors.New("사용자 이름은 영문/숫자로 시작하고 영문, 숫자, '.', '_', '-', '@' 만 사용할 수 있습니다 (최대 64자)")
	}
	return nil
}

func GroupName(s string) error {
	if !reGroupName.MatchString(s) {
		return errors.New("그룹 이름은 영문/숫자로 시작하고 영문, 숫자, '.', '_', '-' 만 사용할 수 있습니다 (최대 64자)")
	}
	return nil
}

func BackupName(s string) error {
	if !reBackup.MatchString(s) || strings.Contains(s, "..") {
		return errors.New("잘못된 백업 파일 이름입니다")
	}
	return nil
}

func Password(pw string) error {
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

// RulePath validates and normalizes a path used in an authz section header.
func RulePath(p string) (string, error) {
	c, err := repopath.Clean(strings.TrimSpace(p))
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

func Access(a string) error {
	switch a {
	case "r", "rw", "none":
		return nil
	}
	return errors.New("권한은 읽기(r), 읽기/쓰기(rw), 접근 금지(none) 중 하나여야 합니다")
}

// Clip trims s and limits it to n runes.
func Clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// SingleLine removes control characters (newlines etc.) from free text fields.
func SingleLine(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return Clip(s, n)
}
