// Package validate checks user supplied names, passwords and authz values and
// sanitizes free text.
package validate

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"svngo/internal/i18n"
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
		return i18n.E("valid.repo_name")
	}
	return nil
}

// reservedLocations are URL paths served by SvnGo itself.
var reservedLocations = []string{"admin", "svn"}

// LocationName checks an SVN location name; it becomes both the URL path
// (/<name>/<repo>) and the folder name under the repository folder.
func LocationName(s string) error {
	if !reRepoName.MatchString(s) || strings.HasSuffix(s, ".") {
		return i18n.E("valid.location_name")
	}
	for _, r := range reservedLocations {
		if strings.EqualFold(s, r) {
			return i18n.E("valid.location_reserved", s)
		}
	}
	return nil
}

func Username(s string) error {
	if !reUsername.MatchString(s) {
		return i18n.E("valid.username")
	}
	return nil
}

func GroupName(s string) error {
	if !reGroupName.MatchString(s) {
		return i18n.E("valid.group_name")
	}
	return nil
}

func BackupName(s string) error {
	if !reBackup.MatchString(s) || strings.Contains(s, "..") {
		return i18n.E("valid.backup_name")
	}
	return nil
}

func Password(pw string) error {
	if utf8.RuneCountInString(pw) < 8 {
		return i18n.E("valid.password_short")
	}
	if len(pw) > 72 {
		return i18n.E("valid.password_long")
	}
	if !utf8.ValidString(pw) {
		return i18n.E("valid.password_invalid")
	}
	for _, r := range pw {
		if unicode.IsControl(r) {
			return i18n.E("valid.password_control")
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
		return "", i18n.E("valid.rule_path_chars")
	}
	for _, seg := range strings.Split(c, "/") {
		if seg != strings.TrimSpace(seg) {
			return "", i18n.E("valid.rule_path_space")
		}
	}
	return c, nil
}

func Access(a string) error {
	switch a {
	case "r", "rw", "none":
		return nil
	}
	return i18n.E("valid.access")
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
