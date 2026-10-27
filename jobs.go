package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type JobRow struct {
	ID         int64
	Kind       string
	Repo       string
	Actor      string
	Status     string
	Message    string
	StartedAt  int64
	FinishedAt int64
}

func (j JobRow) KindLabel() string {
	switch j.Kind {
	case "backup":
		return "백업"
	case "verify":
		return "검증"
	case "restore":
		return "복원"
	}
	return j.Kind
}

func (a *App) listJobs(ctx context.Context, repo string, limit int) ([]JobRow, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT id, kind, repo, actor, status, message, started_at, finished_at
		FROM jobs WHERE (?='' OR repo=?) ORDER BY id DESC LIMIT ?`, repo, repo, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []JobRow
	for rows.Next() {
		var j JobRow
		if rows.Scan(&j.ID, &j.Kind, &j.Repo, &j.Actor, &j.Status, &j.Message, &j.StartedAt, &j.FinishedAt) == nil {
			res = append(res, j)
		}
	}
	return res, rows.Err()
}

func hasActiveJobs(jobs []JobRow) bool {
	for _, j := range jobs {
		if j.Status == "queued" || j.Status == "running" {
			return true
		}
	}
	return false
}

// startJob runs a long operation (backup/verify/restore) asynchronously.
// At most two jobs run at the same time; the rest wait in "queued" state.
func (a *App) startJob(actor, kind, repo string, fn func(ctx context.Context) (string, error)) (int64, error) {
	res, err := a.db.ExecContext(a.ctx, `INSERT INTO jobs(kind, repo, actor, status, started_at) VALUES(?,?,?,'queued',?)`,
		kind, repo, actor, now())
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	audit(a.ctx, a.db, actor, "job."+kind, repo)
	a.jobsWG.Add(1)
	go func() {
		defer a.jobsWG.Done()
		finish := func(status, msg string) {
			a.db.ExecContext(context.Background(), "UPDATE jobs SET status=?, message=?, finished_at=? WHERE id=?",
				status, clip(msg, 2000), now(), id)
		}
		select {
		case a.jobSem <- struct{}{}:
		case <-a.ctx.Done():
			finish("failed", "서버 종료로 취소됨")
			return
		}
		defer func() { <-a.jobSem }()
		defer func() {
			if p := recover(); p != nil {
				slog.Error("작업 패닉", "job", id, "panic", p)
				finish("failed", fmt.Sprint("내부 오류: ", p))
			}
		}()
		a.db.ExecContext(a.ctx, "UPDATE jobs SET status='running', started_at=? WHERE id=?", now(), id)
		ctx, cancel := context.WithTimeout(a.ctx, 12*time.Hour)
		defer cancel()
		start := time.Now()
		msg, err := fn(ctx)
		if err != nil {
			slog.Error("작업 실패", "job", id, "kind", kind, "repo", repo, "err", err)
			finish("failed", err.Error())
			return
		}
		slog.Info("작업 완료", "job", id, "kind", kind, "repo", repo, "elapsed", time.Since(start).Round(time.Millisecond).String())
		finish("ok", msg)
	}()
	return id, nil
}

func (a *App) jobVerify(name string) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		dir := a.repoDir(name)
		y, err := a.svn.Youngest(ctx, dir)
		if err != nil {
			return "", err
		}
		if err := a.svn.Verify(ctx, dir); err != nil {
			return "", fmt.Errorf("검증 실패: %w", err)
		}
		return fmt.Sprintf("r0 ~ r%d 검증 성공", y), nil
	}
}

func (a *App) jobBackup(name string) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		dir := a.repoDir(name)
		y, err := a.svn.Youngest(ctx, dir)
		if err != nil {
			return "", err
		}
		fname := fmt.Sprintf("%s-%s-r%d.svndump.gz", name, time.Now().Format("20060102-150405"), y)
		final := filepath.Join(a.cfg.BackupDir, fname)
		tmp := filepath.Join(a.cfg.BackupDir, "."+fname+".partial")
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
		if err != nil {
			return "", err
		}
		defer os.Remove(tmp)
		bw := bufio.NewWriterSize(f, 256<<10)
		gz := gzip.NewWriter(bw)
		err = a.svn.Dump(ctx, dir, gz)
		if err == nil {
			err = gz.Close()
		}
		if err == nil {
			err = bw.Flush()
		}
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return "", fmt.Errorf("백업 실패: %w", err)
		}
		if err := os.Rename(tmp, final); err != nil {
			return "", err
		}
		st, _ := os.Stat(final)
		return fmt.Sprintf("%s (%s, r%d)", fname, humanSize(st.Size()), y), nil
	}
}

func (a *App) jobRestore(file, target string, overwrite bool, actor string) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		f, err := os.Open(filepath.Join(a.cfg.BackupDir, file))
		if err != nil {
			return "", err
		}
		defer f.Close()
		gz, err := gzip.NewReader(bufio.NewReaderSize(f, 256<<10))
		if err != nil {
			return "", fmt.Errorf("gzip 형식이 아닙니다: %w", err)
		}
		defer gz.Close()
		tmp := filepath.Join(a.cfg.TmpDir(), "restore-"+randomString(10, "abcdefghijklmnopqrstuvwxyz0123456789"))
		defer os.RemoveAll(tmp)
		if err := a.svn.Create(ctx, tmp); err != nil {
			return "", err
		}
		if err := a.svn.Load(ctx, tmp, gz); err != nil {
			return "", fmt.Errorf("svnadmin load 실패: %w", err)
		}
		final := a.repoDir(target)
		trash, placed := "", false
		err = a.mutate(ctx, actor, "repo.restore", file+" -> "+target, func(tx *sql.Tx) error {
			var id int64
			exists := tx.QueryRowContext(ctx, "SELECT id FROM repos WHERE name=?", target).Scan(&id) == nil
			_, statErr := os.Lstat(final)
			if (exists || statErr == nil) && !overwrite {
				return errors.New("대상 저장소가 이미 존재합니다 (덮어쓰기를 선택하지 않음)")
			}
			if !exists {
				if _, err := tx.ExecContext(ctx, "INSERT INTO repos(name, description, created_at) VALUES(?,?,?)",
					target, "백업에서 복원: "+file, now()); err != nil {
					return err
				}
			} else if _, err := tx.ExecContext(ctx, "UPDATE repos SET size_bytes=-1 WHERE id=?", id); err != nil {
				return err
			}
			if statErr == nil {
				trash = filepath.Join(a.cfg.TmpDir(), "trash-"+target+"-"+randomString(8, "abcdefghijklmnopqrstuvwxyz0123456789"))
				if err := os.Rename(final, trash); err != nil {
					return err
				}
			}
			if err := os.Rename(tmp, final); err != nil {
				if trash != "" {
					os.Rename(trash, final)
				}
				return err
			}
			placed = true
			return nil
		})
		if err != nil {
			if placed {
				os.Rename(final, tmp)
				if trash != "" {
					os.Rename(trash, final)
				}
			}
			return "", err
		}
		if trash != "" {
			os.RemoveAll(trash)
		}
		y, _ := a.svn.Youngest(ctx, final)
		return fmt.Sprintf("'%s' 복원 완료 (r%d)", target, y), nil
	}
}

type BackupFile struct {
	Name    string
	Size    int64
	ModTime time.Time
}

var reBackupSuffix = regexp.MustCompile(`^-\d{8}-\d{6}-r\d+\.svndump\.gz$`)

func (a *App) listBackups(repo string) ([]BackupFile, error) {
	ents, err := os.ReadDir(a.cfg.BackupDir)
	if err != nil {
		return nil, err
	}
	var res []BackupFile
	for _, e := range ents {
		n := e.Name()
		if !e.Type().IsRegular() || validBackupName(n) != nil {
			continue
		}
		if repo != "" && !(strings.HasPrefix(n, repo+"-") && reBackupSuffix.MatchString(n[len(repo):])) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		res = append(res, BackupFile{Name: n, Size: info.Size(), ModTime: info.ModTime()})
	}
	sort.Slice(res, func(i, j int) bool { return res[i].ModTime.After(res[j].ModTime) })
	return res, nil
}
