package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
)

type GroupRow struct {
	ID          int64
	Name        string
	Description string
	Members     int
	Rules       int
}

func (a *App) getGroup(ctx context.Context, name string) (*GroupRow, error) {
	if validGroupName(name) != nil {
		return nil, sql.ErrNoRows
	}
	g := &GroupRow{}
	err := a.db.QueryRowContext(ctx, `SELECT id, name, description,
		(SELECT COUNT(*) FROM group_members WHERE group_id=groups.id),
		(SELECT COUNT(*) FROM rules WHERE subject_type='group' AND subject_id=groups.id)
		FROM groups WHERE name=?`, name).Scan(&g.ID, &g.Name, &g.Description, &g.Members, &g.Rules)
	return g, err
}

func (a *App) handleGroups(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	like := likePattern(q)
	var total int
	a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM groups WHERE name LIKE ? ESCAPE '\' OR description LIKE ? ESCAPE '\'`, like, like).Scan(&total)
	pg := newPager(r, total, a.pageSize(r))
	rows, err := a.db.QueryContext(ctx, `SELECT id, name, description,
		(SELECT COUNT(*) FROM group_members WHERE group_id=groups.id),
		(SELECT COUNT(*) FROM rules WHERE subject_type='group' AND subject_id=groups.id)
		FROM groups WHERE name LIKE ? ESCAPE '\' OR description LIKE ? ESCAPE '\' ORDER BY name LIMIT ? OFFSET ?`,
		like, like, pg.Size, pg.Offset())
	if err != nil {
		a.errorPage(w, r, s, 500, err.Error())
		return
	}
	defer rows.Close()
	var groups []GroupRow
	for rows.Next() {
		var g GroupRow
		if rows.Scan(&g.ID, &g.Name, &g.Description, &g.Members, &g.Rules) == nil {
			groups = append(groups, g)
		}
	}
	a.render(w, r, s, http.StatusOK, "groups", &View{Title: "그룹", Nav: "groups", D: map[string]any{
		"Groups": groups, "Pager": pg, "Q": q,
	}})
}

func (a *App) handleGroupCreate(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	name := strings.TrimSpace(r.PostFormValue("name"))
	desc := singleLine(r.PostFormValue("description"), 200)
	err := validGroupName(name)
	if err == nil {
		err = a.mutate(ctx, s.Username, "group.create", name, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO groups(name, description, created_at) VALUES(?,?,?)", name, desc, now())
			if err != nil && strings.Contains(err.Error(), "UNIQUE") {
				return errors.New("이미 존재하는 그룹입니다")
			}
			return err
		})
	}
	if err != nil {
		a.done(w, r, "/admin/groups", "", err)
		return
	}
	a.done(w, r, "/admin/groups/"+name, "그룹 '"+name+"' 를 생성했습니다.", nil)
}

func (a *App) handleGroupEdit(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	g, err := a.getGroup(ctx, r.PathValue("group"))
	if err != nil {
		a.notFound(w, r, s)
		return
	}
	rows, err := a.db.QueryContext(ctx, `SELECT u.username, u.full_name, u.enabled, (m.user_id IS NOT NULL)
		FROM users u LEFT JOIN group_members m ON m.user_id=u.id AND m.group_id=? ORDER BY u.username`, g.ID)
	if err != nil {
		a.errorPage(w, r, s, 500, err.Error())
		return
	}
	defer rows.Close()
	type mem struct {
		Username, FullName string
		Enabled            bool
	}
	var members, others []mem
	for rows.Next() {
		var m mem
		var in bool
		if rows.Scan(&m.Username, &m.FullName, &m.Enabled, &in) != nil {
			continue
		}
		if in {
			members = append(members, m)
		} else {
			others = append(others, m)
		}
	}
	rules, _ := listRules(ctx, a.db, "", "@"+g.Name)
	a.render(w, r, s, http.StatusOK, "group_edit", &View{Title: "@" + g.Name, Nav: "groups", D: map[string]any{
		"Group": g, "Members": members, "Others": others, "Rules": rules,
	}})
}

func (a *App) handleGroupUpdate(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	g, err := a.getGroup(ctx, r.PathValue("group"))
	if err != nil {
		a.notFound(w, r, s)
		return
	}
	desc := singleLine(r.PostFormValue("description"), 200)
	err = a.mutate(ctx, s.Username, "group.update", g.Name, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE groups SET description=? WHERE id=?", desc, g.ID)
		return err
	})
	a.done(w, r, "/admin/groups/"+g.Name, "설명을 저장했습니다.", err)
}

func (a *App) handleGroupAddMembers(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	g, err := a.getGroup(ctx, r.PathValue("group"))
	if err != nil {
		a.notFound(w, r, s)
		return
	}
	r.ParseForm()
	var names []string
	for _, v := range r.PostForm["users"] {
		for _, n := range strings.FieldsFunc(v, func(c rune) bool { return c == ',' || c == ' ' || c == '\n' || c == '\r' }) {
			names = append(names, n)
		}
	}
	err = func() error {
		if len(names) == 0 {
			return errors.New("추가할 사용자를 선택하세요")
		}
		for _, n := range names {
			if err := validUsername(n); err != nil {
				return err
			}
		}
		err := a.mutate(ctx, s.Username, "group.members.add", g.Name+": "+strings.Join(names, ","), func(tx *sql.Tx) error {
			for _, n := range names {
				res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO group_members(group_id, user_id) SELECT ?, id FROM users WHERE username=?`, g.ID, n)
				if err != nil {
					return err
				}
				if c, _ := res.RowsAffected(); c == 0 {
					var x int
					if tx.QueryRowContext(ctx, "SELECT 1 FROM users WHERE username=?", n).Scan(&x) != nil {
						return errors.New("사용자를 찾을 수 없습니다: " + n)
					}
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		return a.verifyGroupRules(ctx, g.Name, names)
	}()
	a.done(w, r, "/admin/groups/"+g.Name, "구성원을 추가했습니다.", err)
}

func (a *App) handleGroupRemoveMember(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	g, err := a.getGroup(ctx, r.PathValue("group"))
	if err != nil {
		a.notFound(w, r, s)
		return
	}
	user := r.PathValue("user")
	err = validUsername(user)
	if err == nil {
		err = a.mutate(ctx, s.Username, "group.members.remove", g.Name+": "+user, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "DELETE FROM group_members WHERE group_id=? AND user_id=(SELECT id FROM users WHERE username=?)", g.ID, user)
			return err
		})
	}
	if err == nil {
		err = a.verifyGroupRules(ctx, g.Name, []string{user})
	}
	a.done(w, r, "/admin/groups/"+g.Name, "구성원 '"+user+"' 를 제외했습니다.", err)
}

// verifyGroupRules checks the group's rules for the given users with libsvn.
func (a *App) verifyGroupRules(ctx context.Context, group string, users []string) error {
	rules, err := listRules(ctx, a.db, "", "@"+group)
	if err != nil {
		return err
	}
	if len(users) > 20 {
		users = users[:20]
	}
	for i, rl := range rules {
		if i >= 20 {
			break
		}
		if err := a.verifyAccess(ctx, rl.Repo, rl.Path, users); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) handleGroupDelete(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	g, err := a.getGroup(ctx, r.PathValue("group"))
	if err != nil {
		a.notFound(w, r, s)
		return
	}
	err = a.mutate(ctx, s.Username, "group.delete", g.Name, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM rules WHERE subject_type='group' AND subject_id=?", g.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM groups WHERE id=?", g.ID)
		return err
	})
	if err != nil {
		a.done(w, r, "/admin/groups/"+g.Name, "", err)
		return
	}
	a.done(w, r, "/admin/groups", "그룹 '"+g.Name+"' 와 관련 권한 규칙을 삭제했습니다.", nil)
}
