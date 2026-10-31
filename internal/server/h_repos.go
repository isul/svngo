package server

import (
	"context"
	"database/sql"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"svngo/internal/apache"
	"svngo/internal/i18n"
	"svngo/internal/store"
	"svngo/internal/svn"
	"svngo/internal/validate"
)

type RepoRow struct {
	ID          int64
	Name        string
	Location    string // "" = default (/svn)
	Description string
	Enabled     bool
	CreatedAt   int64
	SizeBytes   int64
	SizeAt      int64
	Youngest    int64
	Rules       int
	Dir         string
	SvnURL      string
}

func (a *App) getRepo(ctx context.Context, q store.DBTX, name string) (*RepoRow, error) {
	if err := validate.RepoName(name); err != nil {
		return nil, sql.ErrNoRows
	}
	r := &RepoRow{}
	err := q.QueryRowContext(ctx, `SELECT id, name, location, description, enabled, created_at, size_bytes, size_at,
		(SELECT COUNT(*) FROM rules WHERE repo_id=repos.id) FROM repos WHERE name=?`, name).
		Scan(&r.ID, &r.Name, &r.Location, &r.Description, &r.Enabled, &r.CreatedAt, &r.SizeBytes, &r.SizeAt, &r.Rules)
	if err != nil {
		return nil, err
	}
	r.Dir = a.repoPath(r.Location, r.Name)
	r.Youngest = svn.FSFSYoungest(r.Dir)
	return r, nil
}

// repoFor loads the repository from the URL and enforces access: admins see
// everything, SVN users only enabled repositories they can read something in.
func (a *App) repoFor(w http.ResponseWriter, r *http.Request, s *Session) (*RepoRow, bool) {
	repo, err := a.getRepo(r.Context(), a.db, r.PathValue("repo"))
	if err != nil {
		a.notFound(w, r, s)
		return nil, false
	}
	if !s.IsAdmin() && (!repo.Enabled || !a.policy.Load().Traversable(repo.Name, "/", s.Username)) {
		a.notFound(w, r, s)
		return nil, false
	}
	return repo, true
}

func (a *App) handleHome(w http.ResponseWriter, r *http.Request, s *Session) {
	if !s.IsAdmin() {
		http.Redirect(w, r, "/admin/repos", http.StatusSeeOther)
		return
	}
	ctx := r.Context()
	counts := map[string]int{}
	for k, q := range map[string]string{
		"repos": "SELECT COUNT(*) FROM repos", "users": "SELECT COUNT(*) FROM users",
		"disabledUsers": "SELECT COUNT(*) FROM users WHERE enabled=0", "groups": "SELECT COUNT(*) FROM groups",
		"rules": "SELECT COUNT(*) FROM rules", "legacy": "SELECT COUNT(*) FROM users WHERE password_hash LIKE '$apr1$%'",
	} {
		var n int
		a.db.QueryRowContext(ctx, q).Scan(&n)
		counts[k] = n
	}
	var total int64
	a.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(CASE WHEN size_bytes>0 THEN size_bytes ELSE 0 END),0) FROM repos").Scan(&total)
	audits, _ := a.listAudit(ctx, 8, 0)
	jobs, _ := a.listJobs(ctx, "", 5)
	v := &View{Title: i18n.M("nav.dashboard"), Nav: "dashboard", D: map[string]any{
		"Counts": counts, "TotalSize": total, "Disk": diskUsage(a.cfg.ReposDir), "Audits": audits, "Jobs": jobs,
		"Locations": a.locationRows(r), "Apache": a.apacheStatus(), "Uptime": time.Since(a.started).Round(time.Second).String(),
	}}
	a.render(w, r, s, http.StatusOK, "dashboard", v)
}

func (a *App) handleRepos(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	like := "%" + strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(q) + "%"
	rows, err := a.db.QueryContext(ctx, `SELECT id, name, location, description, enabled, created_at, size_bytes, size_at,
		(SELECT COUNT(*) FROM rules WHERE repo_id=repos.id) FROM repos
		WHERE name LIKE ? ESCAPE '\' OR description LIKE ? ESCAPE '\' OR location LIKE ? ESCAPE '\' ORDER BY name`, like, like, like)
	if err != nil {
		a.errorPage(w, r, s, 500, err)
		return
	}
	pol := a.policy.Load()
	var all []*RepoRow
	for rows.Next() {
		rr := &RepoRow{}
		if err := rows.Scan(&rr.ID, &rr.Name, &rr.Location, &rr.Description, &rr.Enabled, &rr.CreatedAt, &rr.SizeBytes, &rr.SizeAt, &rr.Rules); err != nil {
			continue
		}
		if !s.IsAdmin() && (!rr.Enabled || !pol.Traversable(rr.Name, "/", s.Username)) {
			continue
		}
		all = append(all, rr)
	}
	rows.Close()
	pg := newPager(r, len(all), a.pageSize(r))
	page := all[pg.Offset():min(pg.Offset()+pg.Size, len(all))]
	for _, rr := range page {
		rr.Dir = a.repoPath(rr.Location, rr.Name)
		rr.SvnURL = a.svnRepoURL(r, rr.Location, rr.Name)
		rr.Youngest = svn.FSFSYoungest(rr.Dir)
		if s.IsAdmin() && (rr.SizeBytes < 0 || time.Since(time.Unix(rr.SizeAt, 0)) > time.Hour) {
			a.refreshSizeAsync(rr.Name)
		}
	}
	v := &View{Title: i18n.M("nav.repos"), Nav: "repos", D: map[string]any{
		"Repos": page, "Pager": pg, "Q": q,
		"DefaultLayout": store.Setting(ctx, a.db, "default_layout") == "1",
	}}
	if s.IsAdmin() {
		v.D["Subjects"] = a.subjectOptions(ctx)
		v.D["Locations"] = a.locationRows(r)
	}
	a.render(w, r, s, http.StatusOK, "repos", v)
}

func (a *App) handleRepoCreate(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	name := strings.TrimSpace(r.PostFormValue("name"))
	desc := validate.SingleLine(r.PostFormValue("description"), 200)
	layout := r.PostFormValue("layout") == "1"
	subject := r.PostFormValue("subject")
	access := r.PostFormValue("access")
	loc := r.PostFormValue("location")
	err := func() error {
		if err := validate.RepoName(name); err != nil {
			return err
		}
		if subject != "" {
			if err := validate.Access(access); err != nil {
				return err
			}
		}
		if err := checkRepoPlacement(ctx, a.db, loc, name); err != nil {
			return err
		}
		final := a.repoPath(loc, name)
		if _, err := os.Lstat(final); err == nil {
			return i18n.E("repo.err.dir_exists_name", name)
		}
		tmp, err := a.tmpPath(loc, "create-")
		if err != nil {
			return i18n.Wrap(err, "repo.err.create")
		}
		defer os.RemoveAll(tmp)
		if err := a.svn.Create(ctx, tmp); err != nil {
			return i18n.Wrap(err, "repo.err.create")
		}
		if layout {
			if err := a.svn.MkLayout(ctx, tmp, s.Username); err != nil {
				return i18n.Wrap(err, "repo.err.layout")
			}
		}
		renamed := false
		err = a.mutate(ctx, s.Username, "repo.create", repoLabel(loc, name), func(tx *sql.Tx) error {
			if err := checkRepoPlacement(ctx, tx, loc, name); err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, "INSERT INTO repos(name, location, description, created_at) VALUES(?,?,?,?)", name, loc, desc, store.Now())
			if err != nil {
				if strings.Contains(err.Error(), "UNIQUE") {
					return i18n.E("repo.err.name_registered")
				}
				return err
			}
			if subject != "" {
				id, _ := res.LastInsertId()
				if err := insertRule(ctx, tx, id, "/", subject, access); err != nil {
					return err
				}
			}
			if _, err := os.Stat(final); err == nil {
				return i18n.E("repo.err.dir_exists")
			}
			if err := os.Rename(tmp, final); err != nil {
				return err
			}
			renamed = true
			return nil
		})
		if err != nil {
			if renamed {
				os.Rename(final, tmp)
			}
			return err
		}
		if _, err := a.svn.Youngest(ctx, final); err != nil {
			return i18n.Wrap(err, "repo.err.created_unverified")
		}
		a.refreshSizeAsync(name)
		return nil
	}()
	if err != nil {
		a.done(w, r, "/admin/repos", i18n.Msg{}, err)
		return
	}
	a.done(w, r, "/admin/repos/"+name, i18n.M("repo.created", name), nil)
}

func (a *App) handleRepoOverview(w http.ResponseWriter, r *http.Request, s *Session) {
	repo, ok := a.repoFor(w, r, s)
	if !ok {
		return
	}
	ctx := r.Context()
	dir := repo.Dir
	uuid, uuidErr := a.svn.UUID(ctx, dir)
	if y, err := a.svn.Youngest(ctx, dir); err == nil {
		repo.Youngest = y
	}
	entries, _ := a.svn.Log(ctx, dir, "/", repo.Youngest, 8)
	entries = a.filterLog(s, repo.Name, entries)
	if s.IsAdmin() && time.Since(time.Unix(repo.SizeAt, 0)) > 10*time.Minute {
		a.refreshSizeAsync(repo.Name)
	}
	v := &View{Title: i18n.Raw(repo.Name), Nav: "repos", Tab: "overview", Repo: repo, D: map[string]any{
		"UUID": uuid, "UUIDErr": uuidErr, "Log": entries, "SvnURL": a.svnRepoURL(r, repo.Location, repo.Name),
	}}
	if s.IsAdmin() {
		v.D["Jobs"], _ = a.listJobs(ctx, repo.Name, 5)
	} else {
		v.D["Access"] = a.policy.Load().Access(repo.Name, "/", s.Username)
	}
	a.render(w, r, s, http.StatusOK, "repo_overview", v)
}

func (a *App) handleRepoSettings(w http.ResponseWriter, r *http.Request, s *Session) {
	repo, ok := a.repoFor(w, r, s)
	if !ok {
		return
	}
	backups, _ := a.listBackups(repo.Name)
	jobs, _ := a.listJobs(r.Context(), repo.Name, 10)
	repo.SvnURL = a.svnRepoURL(r, repo.Location, repo.Name)
	a.render(w, r, s, http.StatusOK, "repo_settings", &View{Title: i18n.M("title.repo_settings", repo.Name), Nav: "repos", Tab: "settings", Repo: repo,
		D: map[string]any{"Backups": backups, "Jobs": jobs, "Locations": a.locationRows(r)}})
}

func (a *App) handleRepoUpdate(w http.ResponseWriter, r *http.Request, s *Session) {
	repo, ok := a.repoFor(w, r, s)
	if !ok {
		return
	}
	desc := validate.SingleLine(r.PostFormValue("description"), 200)
	err := a.mutate(r.Context(), s.Username, "repo.update", repo.Name, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.Context(), "UPDATE repos SET description=? WHERE id=?", desc, repo.ID)
		return err
	})
	a.done(w, r, "/admin/repos/"+repo.Name+"/settings", i18n.M("repo.desc_saved"), err)
}

func (a *App) handleRepoToggle(w http.ResponseWriter, r *http.Request, s *Session) {
	repo, ok := a.repoFor(w, r, s)
	if !ok {
		return
	}
	action, msg := "repo.disable", i18n.M("repo.disabled_msg")
	if !repo.Enabled {
		action, msg = "repo.enable", i18n.M("repo.enabled_msg")
	}
	err := a.mutate(r.Context(), s.Username, action, repo.Name, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.Context(), "UPDATE repos SET enabled=1-enabled WHERE id=?", repo.ID)
		return err
	})
	if err == nil && repo.Enabled {
		err = a.verifyAccess(r.Context(), repo.Name, "/", a.policy.Load().UsersOf("all", "", 20))
	}
	a.done(w, r, "/admin/repos/"+repo.Name+"/settings", msg, err)
}

func (a *App) handleRepoRename(w http.ResponseWriter, r *http.Request, s *Session) {
	repo, ok := a.repoFor(w, r, s)
	if !ok {
		return
	}
	ctx := r.Context()
	newName := strings.TrimSpace(r.PostFormValue("new_name"))
	// forms without a location field keep the current one
	newLoc := repo.Location
	if v, ok := r.PostForm["location"]; ok && len(v) > 0 {
		newLoc = v[0]
	}
	err := func() error {
		if err := validate.RepoName(newName); err != nil {
			return err
		}
		if newName == repo.Name && newLoc == repo.Location {
			return i18n.E("repo.err.same_name")
		}
		if err := checkRepoPlacement(ctx, a.db, newLoc, newName); err != nil {
			return err
		}
		oldDir, newDir := repo.Dir, a.repoPath(newLoc, newName)
		renamed := false
		err := a.mutate(ctx, s.Username, "repo.rename", repoLabel(repo.Location, repo.Name)+" -> "+repoLabel(newLoc, newName), func(tx *sql.Tx) error {
			if err := checkRepoPlacement(ctx, tx, newLoc, newName); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE repos SET name=?, location=? WHERE id=?", newName, newLoc, repo.ID); err != nil {
				if strings.Contains(err.Error(), "UNIQUE") {
					return i18n.E("repo.err.name_in_use")
				}
				return err
			}
			if _, err := os.Lstat(newDir); err == nil {
				return i18n.E("repo.err.dir_exists")
			}
			if err := os.Rename(oldDir, newDir); err != nil {
				return i18n.Wrap(err, "repo.err.rename_dir")
			}
			renamed = true
			return nil
		})
		if err != nil && renamed {
			os.Rename(newDir, oldDir)
		}
		return err
	}()
	if err != nil {
		a.done(w, r, "/admin/repos/"+repo.Name+"/settings", i18n.Msg{}, err)
		return
	}
	msg := i18n.M("repo.renamed", newName)
	if newLoc != repo.Location {
		msg = i18n.M("repo.moved", "/"+locPrefix(newLoc)+"/"+newName)
	}
	a.done(w, r, "/admin/repos/"+newName+"/settings", msg, nil)
}

func (a *App) handleRepoDelete(w http.ResponseWriter, r *http.Request, s *Session) {
	repo, ok := a.repoFor(w, r, s)
	if !ok {
		return
	}
	ctx := r.Context()
	if r.PostFormValue("confirm") != repo.Name {
		a.done(w, r, "/admin/repos/"+repo.Name+"/settings", i18n.Msg{}, i18n.E("repo.err.confirm_name"))
		return
	}
	dir := repo.Dir
	trash, err := a.tmpPath(repo.Location, "trash-"+repo.Name+"-")
	if err != nil {
		a.done(w, r, "/admin/repos/"+repo.Name+"/settings", i18n.Msg{}, i18n.Wrap(err, "repo.err.move_dir"))
		return
	}
	moved := false
	err = a.mutate(ctx, s.Username, "repo.delete", repoLabel(repo.Location, repo.Name), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM repos WHERE id=?", repo.ID); err != nil {
			return err
		}
		if _, err := os.Lstat(dir); err == nil {
			if err := os.Rename(dir, trash); err != nil {
				return i18n.Wrap(err, "repo.err.move_dir")
			}
			moved = true
		}
		return nil
	})
	if err != nil {
		if moved {
			os.Rename(trash, dir)
		}
		a.done(w, r, "/admin/repos/"+repo.Name+"/settings", i18n.Msg{}, err)
		return
	}
	if moved {
		a.jobsWG.Add(1)
		go func() {
			defer a.jobsWG.Done()
			if err := os.RemoveAll(trash); err != nil {
				slog.Error("cleaning up the deleted repository failed", "dir", trash, "err", err)
			}
		}()
	}
	a.done(w, r, "/admin/repos", i18n.M("repo.deleted", repo.Name), nil)
}

func (a *App) handleRepoVerify(w http.ResponseWriter, r *http.Request, s *Session) {
	repo, ok := a.repoFor(w, r, s)
	if !ok {
		return
	}
	_, err := a.startJob(s.Username, "verify", repo.Name, a.jobVerify(repo.Name))
	a.done(w, r, "/admin/repos/"+repo.Name+"/settings", i18n.M("repo.verify_started"), err)
}

func (a *App) handleRepoBackup(w http.ResponseWriter, r *http.Request, s *Session) {
	repo, ok := a.repoFor(w, r, s)
	if !ok {
		return
	}
	_, err := a.startJob(s.Username, "backup", repo.Name, a.jobBackup(repo.Name))
	a.done(w, r, "/admin/repos/"+repo.Name+"/settings", i18n.M("repo.backup_started"), err)
}

func (a *App) handleRepoSize(w http.ResponseWriter, r *http.Request, s *Session) {
	repo, ok := a.repoFor(w, r, s)
	if !ok {
		return
	}
	n, err := a.refreshSize(r.Context(), repo.Name)
	a.done(w, r, "/admin/repos/"+repo.Name+"/settings", i18n.M("repo.size_msg", humanSize(n)), err)
}

// ---- size cache ----

func dirSize(ctx context.Context, dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total, err
}

func (a *App) refreshSize(ctx context.Context, name string) (int64, error) {
	dir, err := a.repoDirByName(ctx, name)
	if err != nil {
		return -1, err
	}
	n, err := dirSize(ctx, dir)
	if err != nil {
		return -1, err
	}
	_, err = a.db.ExecContext(ctx, "UPDATE repos SET size_bytes=?, size_at=? WHERE name=?", n, store.Now(), name)
	return n, err
}

// refreshSizeAsync recalculates a repository size at most once concurrently.
func (a *App) refreshSizeAsync(name string) {
	a.sizeMu.Lock()
	if a.sizing[name] || len(a.sizing) >= 4 {
		a.sizeMu.Unlock()
		return
	}
	a.sizing[name] = true
	a.sizeMu.Unlock()
	a.jobsWG.Add(1)
	go func() {
		defer a.jobsWG.Done()
		defer func() {
			a.sizeMu.Lock()
			delete(a.sizing, name)
			a.sizeMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(a.ctx, 10*time.Minute)
		defer cancel()
		a.refreshSize(ctx, name)
	}()
}

func (a *App) apacheStatus() apache.Status {
	if a.apache == nil {
		return apache.Status{}
	}
	return a.apache.Status()
}
