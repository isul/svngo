package server

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"

	"svngo/internal/authz"
	"svngo/internal/i18n"
	"svngo/internal/passwd"
	"svngo/internal/store"
	"svngo/internal/validate"
)

type UserRow struct {
	ID        int64
	Username  string
	FullName  string
	Email     string
	Hash      string
	Enabled   bool
	CreatedAt int64
	UpdatedAt int64
	Groups    string
}

func (a *App) getUser(ctx context.Context, q store.DBTX, name string) (*UserRow, error) {
	if validate.Username(name) != nil {
		return nil, sql.ErrNoRows
	}
	u := &UserRow{}
	err := q.QueryRowContext(ctx, `SELECT id, username, full_name, email, password_hash, enabled, created_at, updated_at,
		COALESCE((SELECT group_concat(g.name, ', ') FROM group_members m JOIN groups g ON g.id=m.group_id WHERE m.user_id=users.id),'')
		FROM users WHERE username=?`, name).
		Scan(&u.ID, &u.Username, &u.FullName, &u.Email, &u.Hash, &u.Enabled, &u.CreatedAt, &u.UpdatedAt, &u.Groups)
	return u, err
}

func likePattern(q string) string {
	return "%" + strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(q) + "%"
}

func cleanEmail(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if len(s) > 254 || !strings.Contains(s, "@") || strings.ContainsAny(s, " <>\"'\r\n\t") {
		return "", i18n.E("user.err.email")
	}
	return s, nil
}

func (a *App) handleUsers(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	status := r.URL.Query().Get("status")
	like := likePattern(q)
	where := `(username LIKE ? ESCAPE '\' OR full_name LIKE ? ESCAPE '\' OR email LIKE ? ESCAPE '\')`
	switch status {
	case "enabled":
		where += " AND enabled=1"
	case "disabled":
		where += " AND enabled=0"
	case "legacy":
		where += " AND password_hash LIKE '$apr1$%'"
	}
	var total int
	a.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE "+where, like, like, like).Scan(&total)
	pg := newPager(r, total, a.pageSize(r))
	rows, err := a.db.QueryContext(ctx, `SELECT id, username, full_name, email, password_hash, enabled, created_at, updated_at,
		COALESCE((SELECT group_concat(g.name, ', ') FROM group_members m JOIN groups g ON g.id=m.group_id WHERE m.user_id=users.id),'')
		FROM users WHERE `+where+` ORDER BY username LIMIT ? OFFSET ?`, like, like, like, pg.Size, pg.Offset())
	if err != nil {
		a.errorPage(w, r, s, 500, err)
		return
	}
	defer rows.Close()
	var users []UserRow
	for rows.Next() {
		var u UserRow
		if err := rows.Scan(&u.ID, &u.Username, &u.FullName, &u.Email, &u.Hash, &u.Enabled, &u.CreatedAt, &u.UpdatedAt, &u.Groups); err == nil {
			users = append(users, u)
		}
	}
	groups, _ := a.groupNames(ctx)
	a.render(w, r, s, http.StatusOK, "users", &View{Title: i18n.M("nav.users"), Nav: "users", D: map[string]any{
		"Users": users, "Pager": pg, "Q": q, "Status": status, "Groups": groups,
	}})
}

func (a *App) groupNames(ctx context.Context) ([]string, error) {
	rows, err := a.db.QueryContext(ctx, "SELECT name FROM groups ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		res = append(res, n)
	}
	return res, nil
}

func setUserGroups(ctx context.Context, tx *sql.Tx, userID int64, groups []string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM group_members WHERE user_id=?", userID); err != nil {
		return err
	}
	for _, g := range groups {
		if g == "" {
			continue
		}
		res, err := tx.ExecContext(ctx, "INSERT INTO group_members(group_id, user_id) SELECT id, ? FROM groups WHERE name=?", userID, g)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return i18n.E("group.err.not_found", g)
		}
	}
	return nil
}

func (a *App) handleUserCreate(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	name := strings.TrimSpace(r.PostFormValue("username"))
	full := validate.SingleLine(r.PostFormValue("full_name"), 100)
	pw, pw2 := r.PostFormValue("password"), r.PostFormValue("password2")
	groups := r.PostForm["groups"]
	err := func() error {
		if err := validate.Username(name); err != nil {
			return err
		}
		email, err := cleanEmail(r.PostFormValue("email"))
		if err != nil {
			return err
		}
		if pw != pw2 {
			return i18n.E("err.password_mismatch")
		}
		if err := validate.Password(pw); err != nil {
			return err
		}
		hash, err := passwd.Hash(pw, a.cfg.BcryptCost)
		if err != nil {
			return err
		}
		err = a.mutate(ctx, s.Username, "user.create", name, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, `INSERT INTO users(username, full_name, email, password_hash, enabled, created_at, updated_at)
				VALUES(?,?,?,?,1,?,?)`, name, full, email, hash, store.Now(), store.Now())
			if err != nil {
				if strings.Contains(err.Error(), "UNIQUE") {
					return i18n.E("user.err.exists")
				}
				return err
			}
			id, _ := res.LastInsertId()
			return setUserGroups(ctx, tx, id, groups)
		})
		if err != nil {
			return err
		}
		return a.verifyHtpasswdEntry(name, pw, true)
	}()
	if err != nil {
		a.done(w, r, "/admin/users", i18n.Msg{}, err)
		return
	}
	a.done(w, r, "/admin/users/"+name, i18n.M("user.created_msg", name), nil)
}

func (a *App) handleUserImport(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	overwrite := r.PostFormValue("overwrite") == "1"
	var src io.Reader = strings.NewReader(r.PostFormValue("data"))
	if f, _, err := r.FormFile("file"); err == nil {
		defer f.Close()
		src = io.LimitReader(f, 1<<20)
	}
	entries, rejected, err := passwd.ParseHtpasswd(src)
	if err == nil && len(entries) == 0 {
		err = i18n.E("user.err.import_empty")
	}
	added, updated, skipped := 0, 0, 0
	if err == nil {
		err = a.mutate(ctx, s.Username, "user.import", fmt.Sprintf("%d entries", len(entries)), func(tx *sql.Tx) error {
			for _, e := range entries {
				var id int64
				qerr := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE username=?", e.User).Scan(&id)
				switch {
				case qerr == sql.ErrNoRows:
					if _, err := tx.ExecContext(ctx, `INSERT INTO users(username, password_hash, enabled, created_at, updated_at)
						VALUES(?,?,1,?,?)`, e.User, e.Hash, store.Now(), store.Now()); err != nil {
						return err
					}
					added++
				case qerr != nil:
					return qerr
				case overwrite:
					if _, err := tx.ExecContext(ctx, "UPDATE users SET password_hash=?, updated_at=? WHERE id=?", e.Hash, store.Now(), id); err != nil {
						return err
					}
					updated++
				default:
					skipped++
				}
			}
			return nil
		})
	}
	l := a.loc(r)
	msg := l.T("user.import_done", added, updated, skipped, len(rejected))
	if len(rejected) > 0 {
		parts := make([]string, 0, 10)
		for _, m := range rejected[:min(len(rejected), 10)] {
			parts = append(parts, l.Msg(m))
		}
		msg += " " + l.T("user.import_rejected", strings.Join(parts, "; "))
	}
	if err == nil {
		msg += " " + l.T("user.import_apr1")
	}
	a.done(w, r, "/admin/users", i18n.Raw(msg), err)
}

func (a *App) handleUserEdit(w http.ResponseWriter, r *http.Request, s *Session) {
	a.renderUserEdit(w, r, s, "")
}

func (a *App) renderUserEdit(w http.ResponseWriter, r *http.Request, s *Session, tempPassword string) {
	ctx := r.Context()
	u, err := a.getUser(ctx, a.db, r.PathValue("user"))
	if err != nil {
		a.notFound(w, r, s)
		return
	}
	groups, _ := a.groupNames(ctx)
	member := map[string]bool{}
	for _, g := range strings.Split(u.Groups, ", ") {
		member[g] = true
	}
	direct, _ := authz.ListRules(ctx, a.db, "", u.Username)
	pol := a.policy.Load()
	type eff struct{ Repo, Access string }
	var effective []eff
	for _, repo := range slices.Sorted(maps.Keys(pol.Repos)) {
		if acc := pol.Access(repo, "/", u.Username); acc != "no" {
			effective = append(effective, eff{repo, acc})
		}
	}
	a.render(w, r, s, http.StatusOK, "user_edit", &View{Title: i18n.Raw(u.Username), Nav: "users", D: map[string]any{
		"User": u, "Groups": groups, "Member": member, "Rules": direct, "Effective": effective,
		"TempPassword": tempPassword, "SvnBase": a.svnBaseURL(r),
	}})
}

func (a *App) handleUserUpdate(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	u, err := a.getUser(ctx, a.db, r.PathValue("user"))
	if err != nil {
		a.notFound(w, r, s)
		return
	}
	full := validate.SingleLine(r.PostFormValue("full_name"), 100)
	email, err := cleanEmail(r.PostFormValue("email"))
	if err == nil {
		err = a.mutate(ctx, s.Username, "user.update", u.Username, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE users SET full_name=?, email=?, updated_at=? WHERE id=?", full, email, store.Now(), u.ID)
			return err
		})
	}
	a.done(w, r, "/admin/users/"+u.Username, i18n.M("user.saved"), err)
}

func (a *App) handleUserPassword(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	u, err := a.getUser(ctx, a.db, r.PathValue("user"))
	if err != nil {
		a.notFound(w, r, s)
		return
	}
	pw, pw2 := r.PostFormValue("password"), r.PostFormValue("password2")
	err = func() error {
		if pw != pw2 {
			return i18n.E("err.password_mismatch")
		}
		if err := validate.Password(pw); err != nil {
			return err
		}
		if err := a.setUserPassword(ctx, s.Username, u.ID, pw); err != nil {
			return err
		}
		_, err := a.db.ExecContext(ctx, "DELETE FROM sessions WHERE kind='user' AND principal_id=?", u.ID)
		return err
	}()
	msg := i18n.M("user.password_changed")
	if !u.Enabled {
		msg = i18n.M("user.password_changed_disabled")
	}
	a.done(w, r, "/admin/users/"+u.Username, msg, err)
}

func (a *App) handleUserReset(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	u, err := a.getUser(ctx, a.db, r.PathValue("user"))
	if err != nil {
		a.notFound(w, r, s)
		return
	}
	pw := passwd.Generate()
	if err := a.setUserPassword(ctx, s.Username, u.ID, pw); err != nil {
		a.done(w, r, "/admin/users/"+u.Username, i18n.Msg{}, err)
		return
	}
	a.db.ExecContext(ctx, "DELETE FROM sessions WHERE kind='user' AND principal_id=?", u.ID)
	// shown once in this response only; never stored or logged
	w.Header().Set("HX-Replace-Url", "/admin/users/"+u.Username)
	a.renderUserEdit(w, r, s, pw)
}

func (a *App) handleUserToggle(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	u, err := a.getUser(ctx, a.db, r.PathValue("user"))
	if err != nil {
		a.notFound(w, r, s)
		return
	}
	action, msg := "user.disable", i18n.M("user.disabled_msg")
	if !u.Enabled {
		action, msg = "user.enable", i18n.M("user.enabled_msg")
	}
	err = a.mutate(ctx, s.Username, action, u.Username, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE users SET enabled=1-enabled, updated_at=? WHERE id=?", store.Now(), u.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE kind='user' AND principal_id=?", u.ID)
		return err
	})
	if err == nil {
		err = a.verifyHtpasswdEntry(u.Username, "", !u.Enabled)
	}
	a.done(w, r, "/admin/users/"+u.Username, msg, err)
}

func (a *App) handleUserGroups(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	u, err := a.getUser(ctx, a.db, r.PathValue("user"))
	if err != nil {
		a.notFound(w, r, s)
		return
	}
	r.ParseForm()
	groups := r.PostForm["groups"]
	err = a.mutate(ctx, s.Username, "user.groups", u.Username+": "+strings.Join(groups, ","), func(tx *sql.Tx) error {
		return setUserGroups(ctx, tx, u.ID, groups)
	})
	if err == nil {
		err = a.verifyUserRules(ctx, u.Username)
	}
	a.done(w, r, "/admin/users/"+u.Username, i18n.M("user.groups_saved"), err)
}

// verifyUserRules cross-checks every rule path that may affect user with libsvn.
func (a *App) verifyUserRules(ctx context.Context, user string) error {
	pol := a.policy.Load()
	n := 0
	for _, repo := range slices.Sorted(maps.Keys(pol.Rules)) {
		for _, path := range slices.Sorted(maps.Keys(pol.Rules[repo])) {
			if n++; n > 100 {
				return nil
			}
			if err := a.verifyAccess(ctx, repo, path, []string{user}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *App) handleUserDelete(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	u, err := a.getUser(ctx, a.db, r.PathValue("user"))
	if err != nil {
		a.notFound(w, r, s)
		return
	}
	err = a.mutate(ctx, s.Username, "user.delete", u.Username, func(tx *sql.Tx) error {
		for _, q := range []string{
			"DELETE FROM rules WHERE subject_type='user' AND subject_id=?",
			"DELETE FROM sessions WHERE kind='user' AND principal_id=?",
			"DELETE FROM users WHERE id=?",
		} {
			if _, err := tx.ExecContext(ctx, q, u.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		err = a.verifyHtpasswdEntry(u.Username, "", false)
	}
	if err != nil {
		a.done(w, r, "/admin/users/"+u.Username, i18n.Msg{}, err)
		return
	}
	a.done(w, r, "/admin/users", i18n.M("user.deleted", u.Username), nil)
}

func readHtpasswdFile(path string) (map[string]string, []i18n.Msg, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	entries, rejected, err := passwd.ParseHtpasswd(f)
	m := map[string]string{}
	for _, e := range entries {
		m[e.User] = e.Hash
	}
	return m, rejected, err
}
