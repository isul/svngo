package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"svngo/internal/authz"
	"svngo/internal/fsutil"
	"svngo/internal/passwd"
	"svngo/internal/store"
	"svngo/internal/svn"
	"svngo/internal/validate"
)

// mutate applies a change to the database and regenerates htpasswd/authz in
// one serialized step. The database is the single source of truth:
//   - files are written to temp files, validated and atomically renamed
//   - if the DB commit fails afterwards, the previous files are restored
//
// so the web UI never reports success for a change Apache does not see.
func (a *App) mutate(ctx context.Context, actor, action, detail string, fn func(tx *sql.Tx) error) error {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("DB 트랜잭션 시작 실패: %w", err)
	}
	defer tx.Rollback()

	if fn != nil {
		if err := fn(tx); err != nil {
			return err
		}
	}
	pol, err := authz.Load(ctx, tx)
	if err != nil {
		return fmt.Errorf("권한 정보 로드 실패: %w", err)
	}
	if action != "" {
		if err := store.Audit(ctx, tx, actor, action, detail); err != nil {
			return err
		}
	}
	restore, err := a.writeAuthFiles(ctx, pol.Htpasswd(), pol.Authz())
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		if rerr := restore(); rerr != nil {
			slog.Error("인증 파일 원복 실패", "err", rerr)
		}
		return fmt.Errorf("DB 저장 실패 (인증 파일은 이전 상태로 복원됨): %w", err)
	}
	a.policy.Store(pol)
	if action != "" {
		slog.Info("변경 적용", "actor", actor, "action", action, "detail", validate.Clip(detail, 200))
	}
	return nil
}

// writeAuthFiles validates and atomically replaces both files. The returned
// function restores the previous contents.
func (a *App) writeAuthFiles(ctx context.Context, htp, authzData []byte) (func() error, error) {
	htpFile, authzFile := a.cfg.HtpasswdFile(), a.cfg.AuthzFile()
	prevHtp, errH := os.ReadFile(htpFile)
	prevAuthz, errA := os.ReadFile(authzFile)
	if errH != nil && !errors.Is(errH, os.ErrNotExist) {
		return nil, fmt.Errorf("htpasswd 읽기 실패: %w", errH)
	}
	if errA != nil && !errors.Is(errA, os.ErrNotExist) {
		return nil, fmt.Errorf("authz 읽기 실패: %w", errA)
	}
	restore := func() error {
		e1 := fsutil.AtomicWrite(htpFile, prevHtp)
		e2 := fsutil.AtomicWrite(authzFile, prevAuthz)
		return errors.Join(e1, e2)
	}

	if err := checkHtpasswd(htp); err != nil {
		return nil, err
	}
	tmpAuthz, err := fsutil.WriteTemp(filepath.Dir(authzFile), ".authz-*.tmp", authzData)
	if err != nil {
		return nil, fmt.Errorf("authz 임시 파일 기록 실패: %w", err)
	}
	defer os.Remove(tmpAuthz)
	if err := a.svn.ValidateAuthz(ctx, tmpAuthz); err != nil {
		return nil, fmt.Errorf("authz 검증 실패 (적용하지 않음): %w", err)
	}

	changed := false
	if errH != nil || !bytes.Equal(prevHtp, htp) {
		if err := fsutil.AtomicWrite(htpFile, htp); err != nil {
			return nil, fmt.Errorf("htpasswd 기록 실패: %w", err)
		}
		changed = true
	}
	if errA != nil || !bytes.Equal(prevAuthz, authzData) {
		if err := os.Rename(tmpAuthz, authzFile); err != nil {
			if changed {
				fsutil.AtomicWrite(htpFile, prevHtp)
			}
			return nil, fmt.Errorf("authz 교체 실패: %w", err)
		}
		fsutil.SyncDir(filepath.Dir(authzFile))
	}

	// read back what Apache will read
	gotH, err1 := os.ReadFile(htpFile)
	gotA, err2 := os.ReadFile(authzFile)
	if err1 != nil || err2 != nil || !bytes.Equal(gotH, htp) || !bytes.Equal(gotA, authzData) {
		restore()
		return nil, errors.New("인증 파일 기록 후 검증 불일치 (이전 상태로 복원됨)")
	}
	return restore, nil
}

func checkHtpasswd(b []byte) error {
	for i, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		u, h, ok := strings.Cut(line, ":")
		if !ok || validate.Username(u) != nil || !passwd.ValidStoredHash(h) {
			return fmt.Errorf("htpasswd 생성 결과 검증 실패 (%d행)", i+1)
		}
	}
	return nil
}

// SyncReport describes the result of comparing DB state to files on disk.
type SyncReport struct {
	HtpasswdOK   bool
	AuthzOK      bool
	AuthzValid   bool
	AuthzErr     string
	Users        int
	MissingDirs  []string
	Unregistered []string
	AccessChecks int
	AccessErrs   []string
}

func (r SyncReport) OK() bool {
	return r.HtpasswdOK && r.AuthzOK && r.AuthzValid && len(r.AccessErrs) == 0 && len(r.MissingDirs) == 0
}

// checkSync compares generated content with the files Apache uses and
// cross-checks effective access with libsvn for a sample of users.
func (a *App) checkSync(ctx context.Context) (SyncReport, error) {
	var rep SyncReport
	pol, err := authz.Load(ctx, a.db)
	if err != nil {
		return rep, err
	}
	rep.Users = len(pol.Users)
	gotH, _ := os.ReadFile(a.cfg.HtpasswdFile())
	gotA, _ := os.ReadFile(a.cfg.AuthzFile())
	rep.HtpasswdOK = bytes.Equal(gotH, pol.Htpasswd())
	rep.AuthzOK = bytes.Equal(gotA, pol.Authz())
	if err := a.svn.ValidateAuthz(ctx, a.cfg.AuthzFile()); err != nil {
		rep.AuthzErr = err.Error()
	} else {
		rep.AuthzValid = true
	}
	for name := range pol.Repos {
		if !svn.IsRepoDir(filepath.Join(a.cfg.ReposDir, name)) {
			rep.MissingDirs = append(rep.MissingDirs, name)
		}
	}
	rep.Unregistered = a.unregisteredRepos(ctx)
	if rep.AuthzValid && a.svn.Authz != "" {
		n := 0
	outer:
		for _, repo := range slices.Sorted(maps.Keys(pol.Rules)) {
			for _, path := range slices.Sorted(maps.Keys(pol.Rules[repo])) {
				for _, u := range pol.UsersOf("all", "", 20) {
					if n >= 200 {
						break outer
					}
					n++
					want := pol.Access(repo, path, u)
					got, err := a.svn.AccessOf(ctx, a.cfg.AuthzFile(), repo, path, u)
					if err != nil {
						rep.AccessErrs = append(rep.AccessErrs, err.Error())
						break outer
					}
					if got != want {
						rep.AccessErrs = append(rep.AccessErrs, fmt.Sprintf("%s:%s %s 예상=%s 실제=%s", repo, path, u, want, got))
					}
				}
			}
		}
		rep.AccessChecks = n
	}
	return rep, nil
}

// verifyAccess confirms with libsvn (the engine used by mod_authz_svn) that
// the authz file now grants exactly what the SvnGo policy says.
func (a *App) verifyAccess(ctx context.Context, repo, path string, users []string) error {
	pol := a.policy.Load()
	for _, u := range users {
		want := pol.Access(repo, path, u)
		got, err := a.svn.AccessOf(ctx, a.cfg.AuthzFile(), repo, path, u)
		if err != nil {
			return fmt.Errorf("권한 검증 실행 실패: %w", err)
		}
		if got != want {
			return fmt.Errorf("권한 검증 불일치: %s 의 %s:%s 권한 예상=%s, SVN 실제=%s", u, repo, path, want, got)
		}
	}
	return nil
}
