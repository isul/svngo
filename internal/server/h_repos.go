package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"svngo/internal/apache"
	"svngo/internal/passwd"
	"svngo/internal/store"
	"svngo/internal/svn"
	"svngo/internal/validate"
)

type RepoRow struct {
	ID          int64
	Name        string
	Description string
	Enabled     bool
	CreatedAt   int64
	SizeBytes   int64
	SizeAt      int64
	Youngest    int64
	Rules       int
}

func (a *App) repoDir(name string) string { return filepath.Join(a.cfg.ReposDir, name) }

func (a *App) getRepo(ctx context.Context, q store.DBTX, name string) (*RepoRow, error) {
	if err := validate.RepoName(name); err != nil {
		return nil, sql.ErrNoRows
	}
	r := &RepoRow{}
	err := q.QueryRowContext(ctx, `SELECT id, name, description, enabled, created_at, size_bytes, size_at,
		(SELECT COUNT(*) FROM rules WHERE repo_id=repos.id) FROM repos WHERE name=?`, name).
		Scan(&r.ID, &r.Name, &r.Description, &r.Enabled, &r.CreatedAt, &r.SizeBytes, &r.SizeAt, &r.Rules)
	if err != nil {
		return nil, err
	}
	r.Youngest = svn.FSFSYoungest(a.repoDir(r.Name))
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
	v := &View{Title: "대시보드", Nav: "dashboard", D: map[string]any{
		"Counts": counts, "TotalSize": total, "Disk": diskUsage(a.cfg.ReposDir), "Audits": audits, "Jobs": jobs,
		"SvnBase": a.svnBaseURL(r), "Apache": a.apacheStatus(), "Uptime": time.Since(a.started).Round(time.Second).String(),
	}}
	a.render(w, r, s, http.StatusOK, "dashboard", v)
}

func (a *App) handleRepos(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	like := "%" + strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(q) + "%"
	rows, err := a.db.QueryContext(ctx, `SELECT id, name, description, enabled, created_at, size_bytes, size_at,
		(SELECT COUNT(*) FROM rules WHERE repo_id=repos.id) FROM repos
		WHERE name LIKE ? ESCAPE '\' OR description LIKE ? ESCAPE '\' ORDER BY name`, like, like)
	if err != nil {
		a.errorPage(w, r, s, 500, err.Error())
		return
	}
	pol := a.policy.Load()
	var all []*RepoRow
	for rows.Next() {
		rr := &RepoRow{}
		if err := rows.Scan(&rr.ID, &rr.Name, &rr.Description, &rr.Enabled, &rr.CreatedAt, &rr.SizeBytes, &rr.SizeAt, &rr.Rules); err != nil {
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
		rr.Youngest = svn.FSFSYoungest(a.repoDir(rr.Name))
		if s.IsAdmin() && (rr.SizeBytes < 0 || time.Since(time.Unix(rr.SizeAt, 0)) > time.Hour) {
			a.refreshSizeAsync(rr.Name)
		}
	}
	v := &View{Title: "저장소", Nav: "repos", D: map[string]any{
		"Repos": page, "Pager": pg, "Q": q, "SvnBase": a.svnBaseURL(r),
		"DefaultLayout": store.Setting(ctx, a.db, "default_layout") == "1",
	}}
	if s.IsAdmin() {
		v.D["Subjects"] = a.subjectOptions(ctx)
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
	err := func() error {
		if err := validate.RepoName(name); err != nil {
			return err
		}
		if subject != "" {
			if err := validate.Access(access); err != nil {
				return err
			}
		}
		final := a.repoDir(name)
		if _, err := os.Stat(final); err == nil {
			return fmt.Errorf("이미 같은 이름의 저장소 디렉터리가 있습니다: %s", name)
		}
		tmp := filepath.Join(a.cfg.TmpDir(), "create-"+passwd.RandomString(10, "abcdefghijklmnopqrstuvwxyz0123456789"))
		defer os.RemoveAll(tmp)
		if err := a.svn.Create(ctx, tmp); err != nil {
			return fmt.Errorf("svnadmin create 실패: %w", err)
		}
		if layout {
			if err := a.svn.MkLayout(ctx, tmp, s.Username); err != nil {
				return fmt.Errorf("기본 디렉터리 생성 실패: %w", err)
			}
		}
		renamed := false
		err := a.mutate(ctx, s.Username, "repo.create", name, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, "INSERT INTO repos(name, description, created_at) VALUES(?,?,?)", name, desc, store.Now())
			if err != nil {
				if strings.Contains(err.Error(), "UNIQUE") {
					return errors.New("이미 등록된 저장소 이름입니다")
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
				return errors.New("이미 같은 이름의 저장소 디렉터리가 있습니다")
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
			return fmt.Errorf("저장소는 생성되었으나 확인에 실패했습니다: %w", err)
		}
		a.refreshSizeAsync(name)
		return nil
	}()
	if err != nil {
		a.done(w, r, "/admin/repos", "", err)
		return
	}
	a.done(w, r, "/admin/repos/"+name, "저장소 '"+name+"' 를 생성했습니다. Apache 재시작 없이 바로 사용할 수 있습니다.", nil)
}

func (a *App) handleRepoOverview(w http.ResponseWriter, r *http.Request, s *Session) {
	repo, ok := a.repoFor(w, r, s)
	if !ok {
		return
	}
	ctx := r.Context()
	dir := a.repoDir(repo.Name)
	uuid, err := a.svn.UUID(ctx, dir)
	if err != nil {
		uuid = "확인 실패: " + err.Error()
	}
	if y, err := a.svn.Youngest(ctx, dir); err == nil {
		repo.Youngest = y
	}
	entries, _ := a.svn.Log(ctx, dir, "/", repo.Youngest, 8)
	entries = a.filterLog(s, repo.Name, entries)
	if s.IsAdmin() && time.Since(time.Unix(repo.SizeAt, 0)) > 10*time.Minute {
		a.refreshSizeAsync(repo.Name)
	}
	v := &View{Title: repo.Name, Nav: "repos", Tab: "overview", Repo: repo, D: map[string]any{
		"UUID": uuid, "Log": entries, "SvnURL": a.svnBaseURL(r) + "/" + repo.Name,
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
	a.render(w, r, s, http.StatusOK, "repo_settings", &View{Title: repo.Name + " 설정", Nav: "repos", Tab: "settings", Repo: repo,
		D: map[string]any{"Backups": backups, "Jobs": jobs}})
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
	a.done(w, r, "/admin/repos/"+repo.Name+"/settings", "설명을 저장했습니다.", err)
}

func (a *App) handleRepoToggle(w http.ResponseWriter, r *http.Request, s *Session) {
	repo, ok := a.repoFor(w, r, s)
	if !ok {
		return
	}
	action, msg := "repo.disable", "저장소를 비활성화했습니다. SVN 접근이 즉시 차단됩니다."
	if !repo.Enabled {
		action, msg = "repo.enable", "저장소를 활성화했습니다."
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
	err := func() error {
		if err := validate.RepoName(newName); err != nil {
			return err
		}
		if newName == repo.Name {
			return errors.New("새 이름이 기존 이름과 같습니다")
		}
		oldDir, newDir := a.repoDir(repo.Name), a.repoDir(newName)
		renamed := false
		err := a.mutate(ctx, s.Username, "repo.rename", repo.Name+" -> "+newName, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, "UPDATE repos SET name=? WHERE id=?", newName, repo.ID); err != nil {
				if strings.Contains(err.Error(), "UNIQUE") {
					return errors.New("이미 사용 중인 저장소 이름입니다")
				}
				return err
			}
			if _, err := os.Lstat(newDir); err == nil {
				return errors.New("같은 이름의 디렉터리가 이미 있습니다")
			}
			if err := os.Rename(oldDir, newDir); err != nil {
				return fmt.Errorf("디렉터리 이름 변경 실패: %w", err)
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
		a.done(w, r, "/admin/repos/"+repo.Name+"/settings", "", err)
		return
	}
	a.done(w, r, "/admin/repos/"+newName+"/settings", "저장소 이름을 '"+newName+"' 로 변경했습니다. 작업 복사본은 svn relocate 가 필요합니다.", nil)
}

func (a *App) handleRepoDelete(w http.ResponseWriter, r *http.Request, s *Session) {
	repo, ok := a.repoFor(w, r, s)
	if !ok {
		return
	}
	ctx := r.Context()
	if r.PostFormValue("confirm") != repo.Name {
		a.done(w, r, "/admin/repos/"+repo.Name+"/settings", "", errors.New("확인을 위해 저장소 이름을 정확히 입력해야 합니다"))
		return
	}
	dir := a.repoDir(repo.Name)
	trash := filepath.Join(a.cfg.TmpDir(), "trash-"+repo.Name+"-"+passwd.RandomString(8, "abcdefghijklmnopqrstuvwxyz0123456789"))
	moved := false
	err := a.mutate(ctx, s.Username, "repo.delete", repo.Name, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM repos WHERE id=?", repo.ID); err != nil {
			return err
		}
		if _, err := os.Lstat(dir); err == nil {
			if err := os.Rename(dir, trash); err != nil {
				return fmt.Errorf("저장소 디렉터리 이동 실패: %w", err)
			}
			moved = true
		}
		return nil
	})
	if err != nil {
		if moved {
			os.Rename(trash, dir)
		}
		a.done(w, r, "/admin/repos/"+repo.Name+"/settings", "", err)
		return
	}
	if moved {
		a.jobsWG.Add(1)
		go func() {
			defer a.jobsWG.Done()
			if err := os.RemoveAll(trash); err != nil {
				slog.Error("삭제된 저장소 정리 실패", "dir", trash, "err", err)
			}
		}()
	}
	a.done(w, r, "/admin/repos", "저장소 '"+repo.Name+"' 를 삭제했습니다.", nil)
}

func (a *App) handleRepoVerify(w http.ResponseWriter, r *http.Request, s *Session) {
	repo, ok := a.repoFor(w, r, s)
	if !ok {
		return
	}
	_, err := a.startJob(s.Username, "verify", repo.Name, a.jobVerify(repo.Name))
	a.done(w, r, "/admin/repos/"+repo.Name+"/settings", "저장소 검증 작업을 시작했습니다.", err)
}

func (a *App) handleRepoBackup(w http.ResponseWriter, r *http.Request, s *Session) {
	repo, ok := a.repoFor(w, r, s)
	if !ok {
		return
	}
	_, err := a.startJob(s.Username, "backup", repo.Name, a.jobBackup(repo.Name))
	a.done(w, r, "/admin/repos/"+repo.Name+"/settings", "백업 작업을 시작했습니다.", err)
}

func (a *App) handleRepoSize(w http.ResponseWriter, r *http.Request, s *Session) {
	repo, ok := a.repoFor(w, r, s)
	if !ok {
		return
	}
	n, err := a.refreshSize(r.Context(), repo.Name)
	a.done(w, r, "/admin/repos/"+repo.Name+"/settings", "저장소 크기: "+humanSize(n), err)
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
	n, err := dirSize(ctx, a.repoDir(name))
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
