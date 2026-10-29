// Package authz builds the access policy from the database and renders the
// htpasswd and Subversion authz files Apache uses.
package authz

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"

	"svngo/internal/repopath"
	"svngo/internal/store"
)

// Rule is one access entry: [repo:path] subject = access
type Rule struct {
	ID          int64
	RepoID      int64
	Repo        string
	Path        string
	SubjectType string // user | group | all | authenticated
	SubjectID   int64
	Subject     string // user or group name
	Access      string // r | rw | none
}

func (r Rule) SubjectLabel() string {
	switch r.SubjectType {
	case "group":
		return "@" + r.Subject
	case "all":
		return "*"
	case "authenticated":
		return "$authenticated"
	}
	return r.Subject
}

func (r Rule) authzValue() string {
	switch r.Access {
	case "rw":
		return "rw"
	case "r":
		return "r"
	}
	return ""
}

// Policy is an in-memory snapshot of everything that goes into htpasswd and authz.
// It is rebuilt after every successful change and used for web UI access checks.
type Policy struct {
	Users      map[string]string   // enabled user -> password hash
	Groups     map[string][]string // group -> enabled members (sorted)
	userGroups map[string]map[string]bool
	Repos      map[string]bool              // repo -> enabled
	Rules      map[string]map[string][]Rule // repo -> path -> effective rules
}

func Load(ctx context.Context, q store.DBTX) (*Policy, error) {
	p := &Policy{
		Users:      map[string]string{},
		Groups:     map[string][]string{},
		userGroups: map[string]map[string]bool{},
		Repos:      map[string]bool{},
		Rules:      map[string]map[string][]Rule{},
	}
	rows, err := q.QueryContext(ctx, "SELECT username, password_hash FROM users WHERE enabled=1")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var u, h string
		if err := rows.Scan(&u, &h); err != nil {
			rows.Close()
			return nil, err
		}
		p.Users[u] = h
	}
	rows.Close()

	rows, err = q.QueryContext(ctx, `SELECT g.name, u.username FROM group_members m
		JOIN groups g ON g.id=m.group_id JOIN users u ON u.id=m.user_id
		WHERE u.enabled=1 ORDER BY g.name, u.username`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var g, u string
		if err := rows.Scan(&g, &u); err != nil {
			rows.Close()
			return nil, err
		}
		p.Groups[g] = append(p.Groups[g], u)
		if p.userGroups[u] == nil {
			p.userGroups[u] = map[string]bool{}
		}
		p.userGroups[u][g] = true
	}
	rows.Close()

	rows, err = q.QueryContext(ctx, "SELECT name, enabled FROM repos")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n string
		var en bool
		if err := rows.Scan(&n, &en); err != nil {
			rows.Close()
			return nil, err
		}
		p.Repos[n] = en
	}
	rows.Close()

	rules, err := ListRules(ctx, q, "", "")
	if err != nil {
		return nil, err
	}
	for _, r := range rules {
		if !p.Repos[r.Repo] {
			continue // disabled repositories get a deny-all section only
		}
		// Rules for disabled users or empty groups are omitted: an authz rule
		// referencing an undefined group makes the whole file invalid.
		switch r.SubjectType {
		case "user":
			if _, ok := p.Users[r.Subject]; !ok {
				continue
			}
		case "group":
			if len(p.Groups[r.Subject]) == 0 {
				continue
			}
		}
		if p.Rules[r.Repo] == nil {
			p.Rules[r.Repo] = map[string][]Rule{}
		}
		p.Rules[r.Repo][r.Path] = append(p.Rules[r.Repo][r.Path], r)
	}
	return p, nil
}

// ListRules returns rules joined with subject names; filters are optional.
func ListRules(ctx context.Context, q store.DBTX, repo, subject string) ([]Rule, error) {
	return queryRules(ctx, q, repo, subject, 0)
}

func GetRule(ctx context.Context, q store.DBTX, id int64) (*Rule, error) {
	rules, err := queryRules(ctx, q, "", "", id)
	if err != nil {
		return nil, err
	}
	if len(rules) == 0 {
		return nil, sql.ErrNoRows
	}
	return &rules[0], nil
}

func queryRules(ctx context.Context, q store.DBTX, repo, subject string, id int64) ([]Rule, error) {
	query := `SELECT r.id, r.repo_id, p.name, r.path, r.subject_type, r.subject_id,
		COALESCE(CASE r.subject_type WHEN 'user' THEN u.username WHEN 'group' THEN g.name END, ''), r.access
		FROM rules r JOIN repos p ON p.id=r.repo_id
		LEFT JOIN users u ON r.subject_type='user' AND u.id=r.subject_id
		LEFT JOIN groups g ON r.subject_type='group' AND g.id=r.subject_id
		WHERE (?='' OR p.name=?) AND (?=0 OR r.id=?)`
	args := []any{repo, repo, id, id}
	if subject != "" {
		query += ` AND (CASE r.subject_type WHEN 'user' THEN u.username WHEN 'group' THEN '@'||g.name
			WHEN 'all' THEN '*' ELSE '$authenticated' END) = ?`
		args = append(args, subject)
	}
	query += " ORDER BY p.name, r.path, r.subject_type, r.subject_id"
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []Rule
	for rows.Next() {
		var r Rule
		if err := rows.Scan(&r.ID, &r.RepoID, &r.Repo, &r.Path, &r.SubjectType, &r.SubjectID, &r.Subject, &r.Access); err != nil {
			return nil, err
		}
		res = append(res, r)
	}
	return res, rows.Err()
}

// Htpasswd renders the Apache htpasswd file (enabled users only).
func (p *Policy) Htpasswd() []byte {
	var b bytes.Buffer
	for _, u := range slices.Sorted(maps.Keys(p.Users)) {
		fmt.Fprintf(&b, "%s:%s\n", u, p.Users[u])
	}
	return b.Bytes()
}

// Authz renders the Subversion authz file.
func (p *Policy) Authz() []byte {
	var b bytes.Buffer
	b.WriteString("# Generated by SvnGo. Do not edit: manual changes are overwritten.\n\n[groups]\n")
	for _, g := range slices.Sorted(maps.Keys(p.Groups)) {
		fmt.Fprintf(&b, "%s = %s\n", g, strings.Join(p.Groups[g], ", "))
	}
	for _, repo := range slices.Sorted(maps.Keys(p.Repos)) {
		if !p.Repos[repo] {
			fmt.Fprintf(&b, "\n[%s:/]\n* =\n", repo)
			continue
		}
		paths := p.Rules[repo]
		for _, path := range slices.Sorted(maps.Keys(paths)) {
			fmt.Fprintf(&b, "\n[%s:%s]\n", repo, path)
			for _, r := range paths[path] {
				fmt.Fprintf(&b, "%s = %s\n", r.SubjectLabel(), r.authzValue())
			}
		}
	}
	return b.Bytes()
}

func (p *Policy) matches(r Rule, user string) bool {
	switch r.SubjectType {
	case "all":
		return true
	case "authenticated":
		return user != ""
	case "user":
		return r.Subject == user
	case "group":
		return p.userGroups[user][r.Subject]
	}
	return false
}

// Access evaluates Subversion authz semantics: the most specific path section
// containing an entry matching the user decides; matching entries within that
// section are combined.
func (p *Policy) Access(repo, path, user string) string {
	if !p.Repos[repo] {
		return "no"
	}
	if _, ok := p.Users[user]; !ok {
		return "no"
	}
	rs := p.Rules[repo]
	for cur := path; ; cur = repopath.Parent(cur) {
		matched, read, write := false, false, false
		for _, r := range rs[cur] {
			if !p.matches(r, user) {
				continue
			}
			matched = true
			switch r.Access {
			case "rw":
				read, write = true, true
			case "r":
				read = true
			}
		}
		if matched {
			switch {
			case write:
				return "rw"
			case read:
				return "r"
			}
			return "no"
		}
		if cur == "/" {
			return "no"
		}
	}
}

func (p *Policy) CanRead(repo, path, user string) bool {
	return p.Access(repo, path, user) != "no"
}

// Traversable reports whether the user can read path or anything below it
// (needed to navigate down to a readable sub directory).
func (p *Policy) Traversable(repo, path, user string) bool {
	if p.CanRead(repo, path, user) {
		return true
	}
	prefix := path + "/"
	if path == "/" {
		prefix = "/"
	}
	for rp := range p.Rules[repo] {
		if rp != path && strings.HasPrefix(rp, prefix) && p.CanRead(repo, rp, user) {
			return true
		}
	}
	return false
}

// UsersOf returns enabled users affected by a rule subject (bounded).
func (p *Policy) UsersOf(subjectType, subject string, limit int) []string {
	var users []string
	switch subjectType {
	case "user":
		if _, ok := p.Users[subject]; ok {
			users = []string{subject}
		}
	case "group":
		users = append(users, p.Groups[subject]...)
	default:
		users = slices.Sorted(maps.Keys(p.Users))
	}
	if len(users) > limit {
		users = users[:limit]
	}
	return users
}
