package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"svngo/internal/authz"
	"svngo/internal/fsutil"
	"svngo/internal/i18n"
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
		return i18n.Wrap(err, "sync.err.tx")
	}
	defer tx.Rollback()

	if fn != nil {
		if err := fn(tx); err != nil {
			return err
		}
	}
	pol, err := authz.Load(ctx, tx)
	if err != nil {
		return i18n.Wrap(err, "sync.err.load")
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
			slog.Error("restoring auth files failed", "err", rerr)
		}
		return i18n.Wrap(err, "sync.err.commit")
	}
	a.policy.Store(pol)
	if action != "" {
		slog.Info("change applied", "actor", actor, "action", action, "detail", validate.Clip(detail, 200))
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
		return nil, i18n.Wrap(errH, "sync.err.read_htpasswd")
	}
	if errA != nil && !errors.Is(errA, os.ErrNotExist) {
		return nil, i18n.Wrap(errA, "sync.err.read_authz")
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
		return nil, i18n.Wrap(err, "sync.err.write_temp")
	}
	defer os.Remove(tmpAuthz)
	if err := a.svn.ValidateAuthz(ctx, tmpAuthz); err != nil {
		return nil, i18n.Wrap(err, "sync.err.authz_invalid")
	}

	changed := false
	if errH != nil || !bytes.Equal(prevHtp, htp) {
		if err := fsutil.AtomicWrite(htpFile, htp); err != nil {
			return nil, i18n.Wrap(err, "sync.err.write_htpasswd")
		}
		changed = true
	}
	if errA != nil || !bytes.Equal(prevAuthz, authzData) {
		if err := os.Rename(tmpAuthz, authzFile); err != nil {
			if changed {
				fsutil.AtomicWrite(htpFile, prevHtp)
			}
			return nil, i18n.Wrap(err, "sync.err.replace_authz")
		}
		fsutil.SyncDir(filepath.Dir(authzFile))
	}

	// read back what Apache will read
	gotH, err1 := os.ReadFile(htpFile)
	gotA, err2 := os.ReadFile(authzFile)
	if err1 != nil || err2 != nil || !bytes.Equal(gotH, htp) || !bytes.Equal(gotA, authzData) {
		restore()
		return nil, i18n.E("sync.err.readback")
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
			return i18n.E("sync.err.htpasswd_line", i+1)
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
	Conflicts    []string // on disk, but the name is used by another repository
	AccessChecks int
	AccessErrs   []i18n.Msg
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
	locs := a.repoLocations(ctx)
	for _, name := range slices.Sorted(maps.Keys(pol.Repos)) {
		if !svn.IsRepoDir(a.repoPath(locs[name], name)) {
			rep.MissingDirs = append(rep.MissingDirs, repoLabel(locs[name], name))
		}
	}
	found, conflicts := a.scanRepos(ctx)
	rep.Unregistered, rep.Conflicts = repoLabels(found), repoLabels(conflicts)
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
						rep.AccessErrs = append(rep.AccessErrs, i18n.FromErr(err))
						break outer
					}
					if got != want {
						rep.AccessErrs = append(rep.AccessErrs, i18n.M("sync.access_diff", repo, path, u, want, got))
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
			return i18n.Wrap(err, "sync.err.access_run")
		}
		if got != want {
			return i18n.E("sync.err.access_mismatch", u, repo, path, want, got)
		}
	}
	return nil
}
