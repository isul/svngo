package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"svngo/internal/authz"
	"svngo/internal/store"
	"svngo/internal/validate"
)

type SubjectOption struct {
	Value, Label string
}

// subjectOptions lists rule subjects for <select> elements.
func (a *App) subjectOptions(ctx context.Context) []SubjectOption {
	opts := []SubjectOption{
		{"authenticated", "$authenticated (인증된 모든 사용자)"},
		{"all", "* (모든 사용자)"},
	}
	if rows, err := a.db.QueryContext(ctx, "SELECT name FROM groups ORDER BY name"); err == nil {
		for rows.Next() {
			var n string
			rows.Scan(&n)
			opts = append(opts, SubjectOption{"group:" + n, "@" + n + " (그룹)"})
		}
		rows.Close()
	}
	if rows, err := a.db.QueryContext(ctx, "SELECT username, enabled FROM users ORDER BY username"); err == nil {
		for rows.Next() {
			var n string
			var en bool
			rows.Scan(&n, &en)
			label := n
			if !en {
				label += " (비활성)"
			}
			opts = append(opts, SubjectOption{"user:" + n, label})
		}
		rows.Close()
	}
	return opts
}

// parseSubject converts a form value (user:x, group:x, all, authenticated).
func parseSubject(v string) (typ, name string, err error) {
	switch {
	case v == "all" || v == "authenticated":
		return v, "", nil
	case strings.HasPrefix(v, "user:"):
		name = strings.TrimPrefix(v, "user:")
		return "user", name, validate.Username(name)
	case strings.HasPrefix(v, "group:"):
		name = strings.TrimPrefix(v, "group:")
		return "group", name, validate.GroupName(name)
	}
	return "", "", errors.New("권한 대상을 선택하세요")
}

// insertRule upserts a rule inside a transaction.
func insertRule(ctx context.Context, tx *sql.Tx, repoID int64, path, subject, access string) error {
	typ, name, err := parseSubject(subject)
	if err != nil {
		return err
	}
	if err := validate.Access(access); err != nil {
		return err
	}
	var sid int64
	switch typ {
	case "user":
		if err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE username=?", name).Scan(&sid); err != nil {
			return fmt.Errorf("사용자를 찾을 수 없습니다: %s", name)
		}
	case "group":
		if err := tx.QueryRowContext(ctx, "SELECT id FROM groups WHERE name=?", name).Scan(&sid); err != nil {
			return fmt.Errorf("그룹을 찾을 수 없습니다: %s", name)
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO rules(repo_id, path, subject_type, subject_id, access, created_at)
		VALUES(?,?,?,?,?,?) ON CONFLICT(repo_id, path, subject_type, subject_id) DO UPDATE SET access=excluded.access`,
		repoID, path, typ, sid, access, store.Now())
	return err
}

func (a *App) repoNames(ctx context.Context) []string {
	var res []string
	rows, err := a.db.QueryContext(ctx, "SELECT name FROM repos ORDER BY name")
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		rows.Scan(&n)
		res = append(res, n)
	}
	return res
}

func (a *App) handlePerms(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	repo := r.URL.Query().Get("repo")
	subject := strings.TrimSpace(r.URL.Query().Get("subject"))
	if repo != "" && validate.RepoName(repo) != nil {
		repo = ""
	}
	rules, err := authz.ListRules(ctx, a.db, repo, subject)
	if err != nil {
		a.errorPage(w, r, s, 500, err.Error())
		return
	}
	pg := newPager(r, len(rules), a.pageSize(r)*2)
	page := rules[pg.Offset():min(pg.Offset()+pg.Size, len(rules))]
	a.render(w, r, s, http.StatusOK, "perms", &View{Title: "권한", Nav: "perms", D: map[string]any{
		"Rules": page, "Pager": pg, "Repo": repo, "Subject": subject,
		"Repos": a.repoNames(ctx), "Subjects": a.subjectOptions(ctx), "Back": r.URL.RequestURI(),
	}})
}

func (a *App) handleRepoPerms(w http.ResponseWriter, r *http.Request, s *Session) {
	repo, ok := a.repoFor(w, r, s)
	if !ok {
		return
	}
	ctx := r.Context()
	rules, err := authz.ListRules(ctx, a.db, repo.Name, "")
	if err != nil {
		a.errorPage(w, r, s, 500, err.Error())
		return
	}
	a.render(w, r, s, http.StatusOK, "repo_perms", &View{Title: repo.Name + " 권한", Nav: "repos", Tab: "perms", Repo: repo,
		D: map[string]any{"Rules": rules, "Subjects": a.subjectOptions(ctx), "Back": "/admin/repos/" + repo.Name + "/perms",
			"Path": r.URL.Query().Get("path")}})
}

func (a *App) handleRuleAdd(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	repoName := r.PathValue("repo")
	if repoName == "" {
		repoName = r.PostFormValue("repo")
	}
	back := safeNext(r.PostFormValue("back"))
	subject, access := r.PostFormValue("subject"), r.PostFormValue("access")
	err := func() error {
		repo, err := a.getRepo(ctx, a.db, repoName)
		if err != nil {
			return errors.New("저장소를 선택하세요")
		}
		path, err := validate.RulePath(r.PostFormValue("path"))
		if err != nil {
			return err
		}
		typ, name, err := parseSubject(subject)
		if err != nil {
			return err
		}
		detail := fmt.Sprintf("[%s:%s] %s = %s", repo.Name, path, subject, access)
		if err := a.mutate(ctx, s.Username, "rule.set", detail, func(tx *sql.Tx) error {
			return insertRule(ctx, tx, repo.ID, path, subject, access)
		}); err != nil {
			return err
		}
		return a.verifyAccess(ctx, repo.Name, path, a.policy.Load().UsersOf(typ, name, 50))
	}()
	a.done(w, r, back, "권한을 저장했고 SVN 권한 엔진(libsvn)으로 적용 결과를 검증했습니다.", err)
}

func (a *App) ruleByID(ctx context.Context, idStr string) (*authz.Rule, error) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		return nil, sql.ErrNoRows
	}
	return authz.GetRule(ctx, a.db, id)
}

func (a *App) handleRuleUpdate(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	back := safeNext(r.PostFormValue("back"))
	access := r.PostFormValue("access")
	err := func() error {
		rl, err := a.ruleByID(ctx, r.PathValue("id"))
		if err != nil {
			return errors.New("권한 규칙을 찾을 수 없습니다")
		}
		if err := validate.Access(access); err != nil {
			return err
		}
		detail := fmt.Sprintf("[%s:%s] %s = %s", rl.Repo, rl.Path, rl.SubjectLabel(), access)
		if err := a.mutate(ctx, s.Username, "rule.update", detail, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE rules SET access=? WHERE id=?", access, rl.ID)
			return err
		}); err != nil {
			return err
		}
		return a.verifyAccess(ctx, rl.Repo, rl.Path, a.policy.Load().UsersOf(rl.SubjectType, rl.Subject, 50))
	}()
	a.done(w, r, back, "권한을 변경했고 SVN 권한 엔진(libsvn)으로 검증했습니다.", err)
}

func (a *App) handleRuleDelete(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	back := safeNext(r.PostFormValue("back"))
	err := func() error {
		rl, err := a.ruleByID(ctx, r.PathValue("id"))
		if err != nil {
			return errors.New("권한 규칙을 찾을 수 없습니다")
		}
		affected := a.policy.Load().UsersOf(rl.SubjectType, rl.Subject, 50)
		detail := fmt.Sprintf("[%s:%s] %s", rl.Repo, rl.Path, rl.SubjectLabel())
		if err := a.mutate(ctx, s.Username, "rule.delete", detail, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "DELETE FROM rules WHERE id=?", rl.ID)
			return err
		}); err != nil {
			return err
		}
		return a.verifyAccess(ctx, rl.Repo, rl.Path, affected)
	}()
	a.done(w, r, back, "권한 규칙을 삭제했고 SVN 권한 엔진(libsvn)으로 검증했습니다.", err)
}

// handleAccessCheck returns an HTML fragment comparing SvnGo's evaluation
// with libsvn's result for a user/path.
func (a *App) handleAccessCheck(w http.ResponseWriter, r *http.Request, s *Session) {
	repo, ok := a.repoFor(w, r, s)
	if !ok {
		return
	}
	user := strings.TrimSpace(r.URL.Query().Get("user"))
	data := map[string]any{"User": user}
	path, err := validate.RulePath(r.URL.Query().Get("path"))
	switch {
	case validate.Username(user) != nil:
		data["Error"] = "사용자 이름을 입력하세요"
	case err != nil:
		data["Error"] = err.Error()
	default:
		data["Path"] = path
		data["Want"] = a.policy.Load().Access(repo.Name, path, user)
		if a.svn.Authz != "" {
			got, err := a.svn.AccessOf(r.Context(), a.cfg.AuthzFile(), repo.Name, path, user)
			if err != nil {
				data["Error"] = err.Error()
			}
			data["Got"] = got
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl.pages["repo_perms"].ExecuteTemplate(w, "access_result", data); err != nil {
		http.Error(w, template.HTMLEscapeString(err.Error()), 500)
	}
}
