package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"svngo/internal/apache"
	"svngo/internal/i18n"
	"svngo/internal/passwd"
	"svngo/internal/store"
	"svngo/internal/svn"
	"svngo/internal/validate"
)

// SVN locations: besides /svn/<repo> (repositories directly in ReposDir,
// location ""), every registered location <loc> serves /<loc>/<repo> from
// ReposDir/<loc>/<repo>. Repository names stay unique across all locations,
// so the authz file, the web UI routes and backups keep using the name alone.

// defaultLocation is the URL path of repositories stored directly in ReposDir.
const defaultLocation = "svn"

// locDir is the parent folder of the repositories of a location.
func (a *App) locDir(loc string) string {
	if loc == "" {
		return a.cfg.ReposDir
	}
	return filepath.Join(a.cfg.ReposDir, loc)
}

func (a *App) repoPath(loc, name string) string { return filepath.Join(a.locDir(loc), name) }

// tmpDir is the work folder of a location. It sits next to the repositories
// so finished repositories can be moved into place with a rename.
func (a *App) tmpDir(loc string) string {
	if loc == "" {
		return a.cfg.TmpDir()
	}
	return filepath.Join(a.locDir(loc), ".svngo-tmp")
}

// tmpPath returns a new unique path in the work folder of a location.
func (a *App) tmpPath(loc, prefix string) (string, error) {
	d := a.tmpDir(loc)
	if err := os.MkdirAll(d, 0o750); err != nil {
		return "", err
	}
	return filepath.Join(d, prefix+passwd.RandomString(10, "abcdefghijklmnopqrstuvwxyz0123456789")), nil
}

// locPrefix is the URL path segment of a location.
func locPrefix(loc string) string {
	if loc == "" {
		return defaultLocation
	}
	return loc
}

// repoLabel names a repository together with its location ("loc/name").
func repoLabel(loc, name string) string {
	if loc == "" {
		return name
	}
	return loc + "/" + name
}

// locationNames lists the registered locations (sorted). Rows that are not
// valid names (edited outside SvnGo) are skipped so Apache can still start.
func locationNames(ctx context.Context, q store.DBTX) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT name FROM locations ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		if validate.LocationName(n) != nil {
			slog.Error("invalid location name in the database ignored", "name", n)
			continue
		}
		res = append(res, n)
	}
	return res, rows.Err()
}

// checkLocation fails unless loc is "" or a registered location.
func checkLocation(ctx context.Context, q store.DBTX, loc string) error {
	if loc == "" {
		return nil
	}
	if validate.LocationName(loc) != nil {
		return i18n.E("location.err.unknown", validate.Clip(loc, 64))
	}
	var n int
	if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM locations WHERE name=?", loc).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return i18n.E("location.err.unknown", loc)
	}
	return nil
}

// checkRepoPlacement fails when a repository called name cannot live in loc:
// unknown location, or a default-location name that is a location folder.
func checkRepoPlacement(ctx context.Context, q store.DBTX, loc, name string) error {
	if err := checkLocation(ctx, q, loc); err != nil {
		return err
	}
	if loc != "" {
		return nil
	}
	var n int
	if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM locations WHERE name=?", name).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return i18n.E("repo.err.location_name", name)
	}
	return nil
}

// repoDirByName resolves the folder of a registered repository.
func (a *App) repoDirByName(ctx context.Context, name string) (string, error) {
	var loc string
	if err := a.db.QueryRowContext(ctx, "SELECT location FROM repos WHERE name=?", name).Scan(&loc); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", i18n.E("repo.err.not_found", name)
		}
		return "", err
	}
	return a.repoPath(loc, name), nil
}

// repoLocations maps repository names to their location.
func (a *App) repoLocations(ctx context.Context) map[string]string {
	res := map[string]string{}
	rows, err := a.db.QueryContext(ctx, "SELECT name, location FROM repos")
	if err != nil {
		return res
	}
	defer rows.Close()
	for rows.Next() {
		var n, l string
		if rows.Scan(&n, &l) == nil {
			res[n] = l
		}
	}
	return res
}

// ---- URLs ----

// svnLocationURL returns the checkout base URL of a location. A configured
// svn_base_url is the /svn base; other locations replace its trailing /svn.
func (a *App) svnLocationURL(r *http.Request, loc string) string {
	if v := strings.TrimRight(store.Setting(r.Context(), a.db, "svn_base_url"), "/"); v != "" {
		if loc == "" {
			return v
		}
		if root, ok := strings.CutSuffix(v, "/"+defaultLocation); ok {
			return root + "/" + loc
		}
		if u, err := url.Parse(v); err == nil && u.Host != "" {
			return u.Scheme + "://" + u.Host + "/" + loc
		}
		return v
	}
	scheme := "http"
	if a.isSecure(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/" + locPrefix(loc)
}

// svnRepoURL returns the checkout URL of a repository.
func (a *App) svnRepoURL(r *http.Request, loc, name string) string {
	return a.svnLocationURL(r, loc) + "/" + name
}

// ---- listing ----

// LocationRow describes one location for the web UI (Name "" = default).
type LocationRow struct {
	Name   string
	Prefix string
	Dir    string
	URL    string
	Repos  int
}

// locationRows lists the default location followed by registered ones.
func (a *App) locationRows(r *http.Request) []LocationRow {
	ctx := r.Context()
	counts := map[string]int{}
	if rows, err := a.db.QueryContext(ctx, "SELECT location, COUNT(*) FROM repos GROUP BY location"); err == nil {
		for rows.Next() {
			var l string
			var n int
			if rows.Scan(&l, &n) == nil {
				counts[l] = n
			}
		}
		rows.Close()
	}
	names, _ := locationNames(ctx, a.db)
	res := make([]LocationRow, 0, len(names)+1)
	for _, n := range append([]string{""}, names...) {
		res = append(res, LocationRow{Name: n, Prefix: locPrefix(n), Dir: a.locDir(n), URL: a.svnLocationURL(r, n), Repos: counts[n]})
	}
	return res
}

// ---- scanning ----

type foundRepo struct{ Location, Name string }

func (f foundRepo) String() string { return repoLabel(f.Location, f.Name) }

// scanRepos looks for repository folders in the default location and every
// registered location that are not in the database yet. Folders whose name is
// already taken (by a registered repository elsewhere or earlier in the scan)
// are returned as conflicts.
func (a *App) scanRepos(ctx context.Context) (found, conflicts []foundRepo) {
	known := map[string]string{}
	rows, err := a.db.QueryContext(ctx, "SELECT name, location FROM repos")
	if err != nil {
		return nil, nil
	}
	for rows.Next() {
		var n, l string
		if rows.Scan(&n, &l) == nil {
			known[n] = l
		}
	}
	rows.Close()
	locs, err := locationNames(ctx, a.db)
	if err != nil {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, loc := range append([]string{""}, locs...) {
		ents, err := os.ReadDir(a.locDir(loc))
		if err != nil {
			continue
		}
		for _, e := range ents {
			n := e.Name()
			if !e.IsDir() || validate.RepoName(n) != nil || !svn.IsRepoDir(a.repoPath(loc, n)) {
				continue
			}
			if l, ok := known[n]; ok {
				if l != loc {
					conflicts = append(conflicts, foundRepo{loc, n})
				}
				continue
			}
			if seen[n] {
				conflicts = append(conflicts, foundRepo{loc, n})
				continue
			}
			seen[n] = true
			found = append(found, foundRepo{loc, n})
		}
	}
	return found, conflicts
}

func repoLabels(list []foundRepo) []string {
	res := make([]string, len(list))
	for i, f := range list {
		res[i] = f.String()
	}
	return res
}

// locationCandidates lists folders in ReposDir that are not registered as a
// location but contain repositories (e.g. copied from an old server).
func (a *App) locationCandidates(ctx context.Context) []string {
	locs, err := locationNames(ctx, a.db)
	if err != nil {
		return nil
	}
	ents, err := os.ReadDir(a.cfg.ReposDir)
	if err != nil {
		return nil
	}
	var res []string
	for _, e := range ents {
		n := e.Name()
		dir := filepath.Join(a.cfg.ReposDir, n)
		if !e.IsDir() || slices.Contains(locs, n) || validate.LocationName(n) != nil || svn.IsRepoDir(dir) {
			continue
		}
		sub, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, s := range sub {
			if s.IsDir() && validate.RepoName(s.Name()) == nil && svn.IsRepoDir(filepath.Join(dir, s.Name())) {
				res = append(res, n)
				break
			}
		}
	}
	return res
}

// ---- Apache ----

// initLocations writes the <Location> file before Apache starts.
func (a *App) initLocations(ctx context.Context) error {
	names, err := locationNames(ctx, a.db)
	if err != nil {
		return err
	}
	if err := apache.WriteLocations(a.cfg.RunDir, names); err != nil {
		return fmt.Errorf("writing the SVN locations file failed: %w", err)
	}
	return nil
}

// applyLocations gives Apache the locations stored in q; pass the transaction
// that changes them so a rejected configuration rolls the change back.
func (a *App) applyLocations(ctx context.Context, q store.DBTX) error {
	if a.apache == nil {
		return nil
	}
	names, err := locationNames(ctx, q)
	if err != nil {
		return err
	}
	if err := a.apache.ApplyLocations(ctx, names); err != nil {
		return i18n.Wrap(err, "location.err.apply")
	}
	return nil
}

// ---- handlers ----

func (a *App) handleLocationCreate(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	name := strings.TrimSpace(r.PostFormValue("name"))
	registered := 0
	err := func() error {
		if err := validate.LocationName(name); err != nil {
			return err
		}
		dir := a.locDir(name)
		if svn.IsRepoDir(dir) {
			return i18n.E("location.err.is_repo", name)
		}
		created := false
		if st, err := os.Lstat(dir); err == nil {
			if !st.IsDir() {
				return i18n.E("location.err.not_dir", name)
			}
		} else if os.IsNotExist(err) {
			if err := os.Mkdir(dir, 0o750); err != nil {
				return i18n.Wrap(err, "location.err.mkdir")
			}
			created = true
		} else {
			return err
		}
		err := a.mutate(ctx, s.Username, "location.create", name, func(tx *sql.Tx) error {
			var n int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM repos WHERE location='' AND name=?", name).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return i18n.E("location.err.repo_conflict", name)
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO locations(name, created_at) VALUES(?,?)", name, store.Now()); err != nil {
				if strings.Contains(err.Error(), "UNIQUE") {
					return i18n.E("location.err.exists", name)
				}
				return err
			}
			return a.applyLocations(ctx, tx)
		})
		if err != nil {
			if created {
				os.Remove(dir)
			}
			// re-sync in case the commit failed after Apache took the new file
			if rerr := a.applyLocations(ctx, a.db); rerr != nil {
				slog.Error("restoring the SVN locations failed", "err", rerr)
			}
			return err
		}
		registered, err = a.registerRepos(ctx, s.Username)
		return err
	}()
	a.done(w, r, "/admin/system#locations", i18n.M("location.created", name, registered), err)
}

func (a *App) handleLocationDelete(w http.ResponseWriter, r *http.Request, s *Session) {
	ctx := r.Context()
	name := r.PathValue("name")
	err := func() error {
		if err := checkLocation(ctx, a.db, name); err != nil || name == "" {
			return i18n.E("location.err.unknown", validate.Clip(name, 64))
		}
		err := a.mutate(ctx, s.Username, "location.delete", name, func(tx *sql.Tx) error {
			var n int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM repos WHERE location=?", name).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return i18n.E("location.err.not_empty", name, n)
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM locations WHERE name=?", name); err != nil {
				return err
			}
			return a.applyLocations(ctx, tx)
		})
		if err != nil {
			if rerr := a.applyLocations(ctx, a.db); rerr != nil {
				slog.Error("restoring the SVN locations failed", "err", rerr)
			}
			return err
		}
		// the folder stays unless it is empty (unregistered repositories survive)
		os.RemoveAll(a.tmpDir(name))
		os.Remove(a.locDir(name))
		return nil
	}()
	a.done(w, r, "/admin/system#locations", i18n.M("location.deleted", name), err)
}
