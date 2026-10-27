package server

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"svngo/internal/passwd"
	"svngo/internal/store"
	"svngo/internal/validate"
)

type DiskInfo struct {
	Total, Free, Used int64
	Pct               int
}

func diskUsage(path string) DiskInfo {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return DiskInfo{Total: -1, Free: -1, Used: -1}
	}
	total := int64(st.Blocks) * int64(st.Bsize)
	free := int64(st.Bavail) * int64(st.Bsize)
	d := DiskInfo{Total: total, Free: free, Used: total - free}
	if total > 0 {
		d.Pct = int((total - free) * 100 / total)
	}
	return d
}

type AuditRow struct {
	ID     int64
	At     int64
	Actor  string
	Action string
	Detail string
}

func (a *App) listAudit(ctx context.Context, limit, offset int) ([]AuditRow, error) {
	rows, err := a.db.QueryContext(ctx, "SELECT id, at, actor, action, detail FROM audit ORDER BY id DESC LIMIT ? OFFSET ?", limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []AuditRow
	for rows.Next() {
		var r AuditRow
		if rows.Scan(&r.ID, &r.At, &r.Actor, &r.Action, &r.Detail) == nil {
			res = append(res, r)
		}
	}
	return res, rows.Err()
}

type AdminRow struct {
	ID        int64
	Username  string
	CreatedAt int64
	LastLogin int64
}

func (a *App) systemView(r *http.Request) *View {
	ctx := r.Context()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	var admins []AdminRow
	if rows, err := a.db.QueryContext(ctx, "SELECT id, username, created_at, last_login FROM admins ORDER BY username"); err == nil {
		for rows.Next() {
			var ad AdminRow
			if rows.Scan(&ad.ID, &ad.Username, &ad.CreatedAt, &ad.LastLogin) == nil {
				admins = append(admins, ad)
			}
		}
		rows.Close()
	}
	var dbSize int64
	if st, err := os.Stat(a.cfg.DBFile()); err == nil {
		dbSize = st.Size()
	}
	if st, err := os.Stat(a.cfg.DBFile() + "-wal"); err == nil {
		dbSize += st.Size()
	}
	versions := a.svn.Versions(ctx)
	if a.apache != nil {
		versions["apache"] = a.apache.Version(ctx)
	}
	settings := map[string]string{}
	for k := range store.SettingDefaults {
		settings[k] = store.Setting(ctx, a.db, k)
	}
	return &View{Title: "시스템 설정", Nav: "system", Tab: "status", D: map[string]any{
		"Version": Version, "GoVersion": runtime.Version(), "Uptime": time.Since(a.started).Round(time.Second).String(),
		"MemAlloc": int64(ms.HeapAlloc), "MemSys": int64(ms.Sys), "Goroutines": runtime.NumGoroutine(),
		"Apache": a.apacheStatus(), "Versions": versions, "Disk": diskUsage(a.cfg.ReposDir), "BackupDisk": diskUsage(a.cfg.BackupDir),
		"DBSize": dbSize, "Admins": admins, "Settings": settings, "Cfg": a.cfg, "AuthzTool": a.svn.Authz, "SvnBlocked": a.svnGuard.List(),
		"Unregistered": a.unregisteredRepos(ctx), "SvnBase": a.svnBaseURL(r),
	}}
}

func (a *App) handleSystem(w http.ResponseWriter, r *http.Request, s *Session) {
	a.render(w, r, s, http.StatusOK, "system", a.systemView(r))
}

func (a *App) handleSettingsSave(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	err := func() error {
		base := strings.TrimRight(strings.TrimSpace(r.PostFormValue("svn_base_url")), "/")
		if base != "" {
			u, err := url.Parse(base)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.ContainsAny(base, " \"'<>") {
				return errors.New("SVN 기본 URL 은 http:// 또는 https:// 로 시작해야 합니다")
			}
		}
		pageSize, err := strconv.Atoi(r.PostFormValue("page_size"))
		if err != nil || pageSize < 10 || pageSize > 200 {
			return errors.New("페이지 크기는 10~200 사이여야 합니다")
		}
		attempts, err := strconv.Atoi(r.PostFormValue("login_attempts"))
		if err != nil || attempts < 3 || attempts > 100 {
			return errors.New("로그인 실패 허용 횟수는 3~100 사이여야 합니다")
		}
		svnAttempts, err := strconv.Atoi(r.PostFormValue("svn_block_attempts"))
		if err != nil || svnAttempts < 0 || svnAttempts > 1000 || (svnAttempts > 0 && svnAttempts < 3) {
			return errors.New("SVN 로그인 실패 허용 횟수는 0(사용 안 함) 또는 3~1000 사이여야 합니다")
		}
		svnMinutes, err := strconv.Atoi(r.PostFormValue("svn_block_minutes"))
		if err != nil || svnMinutes < 1 || svnMinutes > 1440 {
			return errors.New("SVN 차단 시간은 1~1440분 사이여야 합니다")
		}
		layout := "0"
		if r.PostFormValue("default_layout") == "1" {
			layout = "1"
		}
		for k, v := range map[string]string{"svn_base_url": base, "page_size": strconv.Itoa(pageSize),
			"login_attempts": strconv.Itoa(attempts), "default_layout": layout,
			"svn_block_attempts": strconv.Itoa(svnAttempts), "svn_block_minutes": strconv.Itoa(svnMinutes)} {
			if err := store.SetSetting(ctx, a.db, k, v); err != nil {
				return err
			}
		}
		return store.Audit(ctx, a.db, s.Username, "settings.update", "")
	}()
	a.done(w, r, "/admin/system", "설정을 저장했습니다.", err)
}

func (a *App) handleSvnUnblock(w http.ResponseWriter, r *http.Request, s *Session) {
	ip := strings.TrimSpace(r.PostFormValue("ip"))
	err := func() error {
		if net.ParseIP(ip) == nil {
			return errors.New("잘못된 IP 주소입니다")
		}
		ok, err := a.svnGuard.Unblock(ip)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("차단 목록에 없는 IP 입니다 (이미 만료되었을 수 있습니다)")
		}
		return store.Audit(r.Context(), a.db, s.Username, "svn.unblock", ip)
	}()
	a.done(w, r, "/admin/system", "SVN 접속 차단을 해제했습니다: "+ip, err)
}

func (a *App) handleSyncCheck(w http.ResponseWriter, r *http.Request, s *Session) {
	v := a.systemView(r)
	rep, err := a.checkSync(r.Context())
	if err != nil {
		v.FlashErr = err.Error()
	}
	v.D["Sync"] = rep
	w.Header().Set("HX-Replace-Url", "/admin/system")
	a.render(w, r, s, http.StatusOK, "system", v)
}

func (a *App) handleSyncRepair(w http.ResponseWriter, r *http.Request, s *Session) {
	err := a.mutate(r.Context(), s.Username, "sync.repair", "htpasswd/authz 재생성", nil)
	a.done(w, r, "/admin/system", "DB 기준으로 htpasswd / authz 파일을 다시 생성했습니다.", err)
}

func (a *App) handleRepoScan(w http.ResponseWriter, r *http.Request, s *Session) {
	n, err := a.registerRepos(r.Context(), s.Username)
	a.done(w, r, "/admin/system", fmt.Sprintf("저장소 %d개를 등록했습니다.", n), err)
}

func (a *App) handleAdminCreate(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	name := strings.TrimSpace(r.PostFormValue("username"))
	pw, pw2 := r.PostFormValue("password"), r.PostFormValue("password2")
	err := func() error {
		if err := validate.Username(name); err != nil {
			return err
		}
		if pw != pw2 {
			return errors.New("비밀번호 확인이 일치하지 않습니다")
		}
		if err := validate.Password(pw); err != nil {
			return err
		}
		hash, err := passwd.Hash(pw, 12)
		if err != nil {
			return err
		}
		if _, err := a.db.ExecContext(ctx, "INSERT INTO admins(username, password_hash, created_at) VALUES(?,?,?)", name, hash, store.Now()); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return errors.New("이미 존재하는 관리자입니다")
			}
			return err
		}
		return store.Audit(ctx, a.db, s.Username, "admin.create", name)
	}()
	a.done(w, r, "/admin/system", "관리자 '"+name+"' 를 추가했습니다.", err)
}

func (a *App) handleAdminDelete(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	err := func() error {
		if id == s.PrincipalID {
			return errors.New("현재 로그인한 관리자는 삭제할 수 없습니다")
		}
		var n int
		a.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM admins").Scan(&n)
		if n <= 1 {
			return errors.New("마지막 관리자는 삭제할 수 없습니다")
		}
		var name string
		if err := a.db.QueryRowContext(ctx, "SELECT username FROM admins WHERE id=?", id).Scan(&name); err != nil {
			return errors.New("관리자를 찾을 수 없습니다")
		}
		if _, err := a.db.ExecContext(ctx, "DELETE FROM admins WHERE id=?", id); err != nil {
			return err
		}
		a.db.ExecContext(ctx, "DELETE FROM sessions WHERE kind='admin' AND principal_id=?", id)
		return store.Audit(ctx, a.db, s.Username, "admin.delete", name)
	}()
	a.done(w, r, "/admin/system", "관리자를 삭제했습니다.", err)
}

// ---- backups ----

func (a *App) handleBackups(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	backups, err := a.listBackups("")
	if err != nil {
		a.errorPage(w, r, s, 500, err.Error())
		return
	}
	jobs, _ := a.listJobs(ctx, "", 20)
	a.render(w, r, s, http.StatusOK, "backups", &View{Title: "백업 / 복원", Nav: "system", Tab: "backups", D: map[string]any{
		"Backups": backups, "Jobs": jobs, "Repos": a.repoNames(ctx), "Disk": diskUsage(a.cfg.BackupDir),
	}})
}

func (a *App) handleBackupDownload(w http.ResponseWriter, r *http.Request, s *Session) {
	name := r.PathValue("file")
	if validate.BackupName(name) != nil {
		a.notFound(w, r, s)
		return
	}
	f, err := os.Open(filepath.Join(a.cfg.BackupDir, name))
	if err != nil {
		a.notFound(w, r, s)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		a.notFound(w, r, s)
		return
	}
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(6 * time.Hour))
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func (a *App) handleBackupDelete(w http.ResponseWriter, r *http.Request, s *Session) {
	name := r.PathValue("file")
	err := validate.BackupName(name)
	if err == nil {
		err = os.Remove(filepath.Join(a.cfg.BackupDir, name))
	}
	if err == nil {
		store.Audit(r.Context(), a.db, s.Username, "backup.delete", name)
	}
	a.done(w, r, safeNext(r.PostFormValue("back")), "백업 파일을 삭제했습니다.", err)
}

func (a *App) handleRestore(w http.ResponseWriter, r *http.Request, s *Session) {
	file := r.PostFormValue("file")
	target := strings.TrimSpace(r.PostFormValue("target"))
	overwrite := r.PostFormValue("overwrite") == "1"
	err := func() error {
		if err := validate.BackupName(file); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(a.cfg.BackupDir, file)); err != nil {
			return errors.New("백업 파일을 찾을 수 없습니다")
		}
		if err := validate.RepoName(target); err != nil {
			return err
		}
		if overwrite && r.PostFormValue("confirm") != target {
			return errors.New("덮어쓰기 복원은 확인란에 대상 저장소 이름을 정확히 입력해야 합니다")
		}
		_, err := a.startJob(s.Username, "restore", target, a.jobRestore(file, target, overwrite, s.Username))
		return err
	}()
	a.done(w, r, "/admin/system/backups", "복원 작업을 시작했습니다. 작업 목록에서 진행 상태를 확인하세요.", err)
}

// handleBackupUpload streams a dump file to the backup directory without
// buffering it in memory. The first multipart field must be the CSRF token.
func (a *App) handleBackupUpload(w http.ResponseWriter, r *http.Request, s *Session) {
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(6 * time.Hour))
	rc.SetWriteDeadline(time.Now().Add(6 * time.Hour))
	err := func() error {
		mr, err := r.MultipartReader()
		if err != nil {
			return errors.New("잘못된 업로드 요청입니다")
		}
		p, err := mr.NextPart()
		if err != nil || p.FormName() != "_csrf" {
			return errors.New("보안 토큰(CSRF)이 없습니다")
		}
		tok, _ := io.ReadAll(io.LimitReader(p, 256))
		if subtle.ConstantTimeCompare(tok, []byte(s.CSRF)) != 1 {
			return errors.New("보안 토큰(CSRF)이 유효하지 않습니다")
		}
		p, err = mr.NextPart()
		if err != nil || p.FormName() != "file" {
			return errors.New("업로드할 파일을 선택하세요")
		}
		br := bufio.NewReaderSize(p, 64<<10)
		head, _ := br.Peek(32)
		isGzip := len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b
		if !isGzip && !strings.HasPrefix(string(head), "SVN-fs-dump-format-version:") {
			return errors.New("svnadmin dump 파일(.svndump 또는 .svndump.gz)만 업로드할 수 있습니다")
		}
		name := "upload-" + time.Now().Format("20060102-150405") + "-r0.svndump.gz"
		tmp, err := os.CreateTemp(a.cfg.BackupDir, ".upload-*.partial")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		bw := bufio.NewWriterSize(tmp, 256<<10)
		var copyErr error
		if isGzip {
			_, copyErr = io.Copy(bw, br)
		} else {
			gz := gzip.NewWriter(bw)
			if _, copyErr = io.Copy(gz, br); copyErr == nil {
				copyErr = gz.Close()
			}
		}
		if copyErr == nil {
			copyErr = bw.Flush()
		}
		if copyErr == nil {
			copyErr = tmp.Sync()
		}
		if cerr := tmp.Close(); copyErr == nil {
			copyErr = cerr
		}
		if copyErr != nil {
			return fmt.Errorf("업로드 실패: %w", copyErr)
		}
		if err := os.Rename(tmp.Name(), filepath.Join(a.cfg.BackupDir, name)); err != nil {
			return err
		}
		os.Chmod(filepath.Join(a.cfg.BackupDir, name), 0o640)
		return store.Audit(r.Context(), a.db, s.Username, "backup.upload", name)
	}()
	a.done(w, r, "/admin/system/backups", "덤프 파일을 업로드했습니다. 복원할 수 있습니다.", err)
}

func (a *App) handleJobsFragment(w http.ResponseWriter, r *http.Request, s *Session) {
	repo := r.URL.Query().Get("repo")
	if repo != "" && validate.RepoName(repo) != nil {
		repo = ""
	}
	limit := 20
	if repo != "" {
		limit = 10
	}
	jobs, _ := a.listJobs(r.Context(), repo, limit)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	a.tmpl.pages["backups"].ExecuteTemplate(w, "jobs", map[string]any{"Jobs": jobs, "Repo": repo})
}

func (a *App) handleLogs(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	src := r.URL.Query().Get("src")
	v := &View{Title: "로그", Nav: "system", Tab: "logs", D: map[string]any{"Src": src}}
	switch src {
	case "app":
		v.D["Lines"] = a.appLogs.Lines(1000)
	case "apache":
		v.D["Lines"] = a.apLogs.Lines(1000)
	default:
		v.D["Src"] = "audit"
		var total int
		a.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit").Scan(&total)
		pg := newPager(r, total, 50)
		v.D["Audits"], _ = a.listAudit(ctx, pg.Size, pg.Offset())
		v.D["Pager"] = pg
	}
	a.render(w, r, s, http.StatusOK, "logs", v)
}

func (a *App) handleAPIStatus(w http.ResponseWriter, r *http.Request, s *Session) {
	ap := a.apacheStatus()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{
		"version": Version,
		"apache":  map[string]any{"managed": a.apache != nil, "running": ap.Running, "pid": ap.PID, "restarts": ap.Restarts},
		"uptime":  int64(time.Since(a.started).Seconds()),
	})
}
