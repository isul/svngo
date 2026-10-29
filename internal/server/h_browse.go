package server

import (
	"bufio"
	"bytes"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"svngo/internal/i18n"
	"svngo/internal/repopath"
	"svngo/internal/svn"
)

const (
	maxViewBytes = 512 << 10 // file view limit; downloads are streamed without limit
	maxDiffLines = 5000
	maxDiffBytes = 2 << 20
	maxDiffFiles = 300
)

// resolveRev parses ?rev= and returns it bounded by the youngest revision.
func (a *App) resolveRev(r *http.Request, repo *RepoRow) (int64, error) {
	youngest, err := a.svn.Youngest(r.Context(), a.repoDir(repo.Name))
	if err != nil {
		return 0, err
	}
	repo.Youngest = youngest
	v := r.URL.Query().Get("rev")
	if v == "" || v == "HEAD" {
		return youngest, nil
	}
	rev, err := strconv.ParseInt(v, 10, 64)
	if err != nil || rev < 0 || rev > youngest {
		return 0, i18n.E("browse.err.revision")
	}
	return rev, nil
}

func (a *App) handleBrowse(w http.ResponseWriter, r *http.Request, s *Session) {
	repo, ok := a.repoFor(w, r, s)
	if !ok {
		return
	}
	ctx := r.Context()
	p, err := repopath.Clean(r.PathValue("path"))
	if err != nil {
		a.errorPage(w, r, s, http.StatusBadRequest, err)
		return
	}
	rev, err := a.resolveRev(r, repo)
	if err != nil {
		a.errorPage(w, r, s, http.StatusBadRequest, err)
		return
	}
	pol := a.policy.Load()
	if !s.IsAdmin() && !pol.Traversable(repo.Name, p, s.Username) {
		a.notFound(w, r, s)
		return
	}
	dir := a.repoDir(repo.Name)
	info, err := a.svn.NodeInfo(ctx, dir, p, rev)
	if err != nil {
		if svn.IsNotFound(err) {
			a.notFound(w, r, s)
			return
		}
		a.errorPage(w, r, s, http.StatusInternalServerError, err)
		return
	}
	v := &View{Title: i18n.Raw(repo.Name + p), Nav: "repos", Tab: "files", Repo: repo, D: map[string]any{
		"Path": p, "Rev": rev, "Info": info, "Pinned": r.URL.Query().Get("rev") != "",
		"SvnURL": a.svnBaseURL(r) + "/" + repo.Name + escapeRepoPath(p),
	}}

	if info.Kind == "dir" {
		entries, err := a.svn.List(ctx, dir, p, rev)
		if err != nil {
			a.errorPage(w, r, s, http.StatusInternalServerError, err)
			return
		}
		visible := entries[:0]
		for _, e := range entries {
			if s.IsAdmin() || pol.Traversable(repo.Name, e.Path, s.Username) {
				visible = append(visible, e)
			}
		}
		sort.SliceStable(visible, func(i, j int) bool {
			if visible[i].Kind != visible[j].Kind {
				return visible[i].Kind == "dir"
			}
			return strings.ToLower(visible[i].Name) < strings.ToLower(visible[j].Name)
		})
		v.D["Entries"] = visible
		a.render(w, r, s, http.StatusOK, "repo_files", v)
		return
	}

	if !s.IsAdmin() && !pol.CanRead(repo.Name, p, s.Username) {
		a.notFound(w, r, s)
		return
	}
	size, err := a.svn.FileSize(ctx, dir, p, rev)
	if err != nil {
		a.errorPage(w, r, s, http.StatusInternalServerError, err)
		return
	}
	v.D["Size"] = size
	data, truncated, err := a.svn.CatHead(ctx, dir, p, rev, maxViewBytes)
	if err != nil {
		a.errorPage(w, r, s, http.StatusInternalServerError, err)
		return
	}
	head := data[:min(len(data), 8000)]
	binary := bytes.IndexByte(head, 0) >= 0 || !utf8.Valid(trimPartialRune(data))
	v.D["Binary"] = binary
	v.D["Truncated"] = truncated || int64(len(data)) < size
	if !binary {
		text := strings.ReplaceAll(string(trimPartialRune(data)), "\r\n", "\n")
		lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
		v.D["Lines"] = lines
	}
	a.render(w, r, s, http.StatusOK, "repo_file", v)
}

// trimPartialRune drops an incomplete UTF-8 sequence at the end of a truncated buffer.
func trimPartialRune(b []byte) []byte {
	for i := 0; i < 3 && len(b) > 0; i++ {
		r, size := utf8.DecodeLastRune(b)
		if r != utf8.RuneError || size != 1 {
			return b
		}
		b = b[:len(b)-1]
	}
	return b
}

func (a *App) handleRaw(w http.ResponseWriter, r *http.Request, s *Session) {
	repo, ok := a.repoFor(w, r, s)
	if !ok {
		return
	}
	p, err := repopath.Clean(r.PathValue("path"))
	if err != nil || p == "/" {
		a.notFound(w, r, s)
		return
	}
	rev, err := a.resolveRev(r, repo)
	if err != nil {
		a.errorPage(w, r, s, http.StatusBadRequest, err)
		return
	}
	if !s.IsAdmin() && !a.policy.Load().CanRead(repo.Name, p, s.Username) {
		a.notFound(w, r, s)
		return
	}
	dir := a.repoDir(repo.Name)
	size, err := a.svn.FileSize(r.Context(), dir, p, rev)
	if err != nil {
		a.notFound(w, r, s)
		return
	}
	name := repopath.Base(p)
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	h.Set("Cache-Control", "private, no-store")
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Now().Add(2 * time.Hour))
	bw := bufio.NewWriterSize(w, 64<<10)
	if err := a.svn.Cat(r.Context(), bw, dir, p, rev); err == nil {
		bw.Flush()
	}
}

// filterLog applies path based read access to log entries for SVN users:
// entries with no readable path are dropped, partially readable entries keep
// only readable paths and hide the message (like Subversion itself).
func (a *App) filterLog(s *Session, repo string, entries []svn.LogEntry) []svn.LogEntry {
	if s.IsAdmin() {
		return entries
	}
	pol := a.policy.Load()
	out := entries[:0]
	for _, e := range entries {
		var keep []svn.LogPath
		for _, p := range e.Paths {
			if pol.CanRead(repo, p.Path, s.Username) {
				keep = append(keep, p)
			}
		}
		if len(keep) == 0 && len(e.Paths) > 0 {
			continue
		}
		if len(keep) < len(e.Paths) {
			e.Hidden, e.Msg = true, ""
		}
		e.Paths = keep
		out = append(out, e)
	}
	return out
}

func (a *App) handleCommits(w http.ResponseWriter, r *http.Request, s *Session) {
	repo, ok := a.repoFor(w, r, s)
	if !ok {
		return
	}
	ctx := r.Context()
	p, err := repopath.Clean(r.URL.Query().Get("path"))
	if err != nil {
		a.errorPage(w, r, s, http.StatusBadRequest, err)
		return
	}
	if !s.IsAdmin() && !a.policy.Load().Traversable(repo.Name, p, s.Username) {
		a.notFound(w, r, s)
		return
	}
	dir := a.repoDir(repo.Name)
	youngest, err := a.svn.Youngest(ctx, dir)
	if err != nil {
		a.errorPage(w, r, s, 500, err)
		return
	}
	repo.Youngest = youngest
	start := youngest
	if b := r.URL.Query().Get("before"); b != "" {
		if n, err := strconv.ParseInt(b, 10, 64); err == nil && n >= 0 && n < youngest {
			start = n
		}
	}
	limit := a.pageSize(r)
	entries, err := a.svn.Log(ctx, dir, p, start, limit+1)
	if err != nil && !svn.IsNotFound(err) {
		a.errorPage(w, r, s, 500, err)
		return
	}
	var next int64 = -1
	if len(entries) > limit {
		next = entries[limit].Rev
		entries = entries[:limit]
	}
	entries = a.filterLog(s, repo.Name, entries)
	a.render(w, r, s, http.StatusOK, "repo_commits", &View{Title: i18n.M("title.repo_commits", repo.Name), Nav: "repos", Tab: "commits", Repo: repo,
		D: map[string]any{"Log": entries, "Path": p, "Next": next, "Start": start, "PathQ": url.QueryEscape(p)}})
}

// DiffFile is one file section of a revision diff.
type DiffFile struct {
	Path      string
	Header    string
	Lines     []DiffLine
	Truncated bool
}

type DiffLine struct {
	Text  string
	Class string
}

// diffParser splits `svnlook diff` output into file sections. A section
// header ("Modified: x") only counts when followed by a ==== / ____ rule line,
// so property names like "Added: svn:ignore" inside a section are not headers.
type diffParser struct {
	pending  string
	hasPend  bool
	onHeader func(header, path string)
	onLine   func(line string)
}

func (d *diffParser) feed(line string) {
	if d.hasPend {
		d.hasPend = false
		if strings.HasPrefix(line, "=====") || strings.HasPrefix(line, "_____") {
			p, _ := svn.DiffSectionPath(d.pending)
			d.onHeader(d.pending, p)
			d.onLine(line)
			return
		}
		d.onLine(d.pending)
	}
	if _, ok := svn.DiffSectionPath(line); ok {
		d.pending, d.hasPend = line, true
		return
	}
	d.onLine(line)
}

func (d *diffParser) flush() {
	if d.hasPend {
		d.hasPend = false
		d.onLine(d.pending)
	}
}

func diffClass(l string) string {
	switch {
	case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
		return "meta"
	case strings.HasPrefix(l, "+"):
		return "add"
	case strings.HasPrefix(l, "-"):
		return "del"
	case strings.HasPrefix(l, "@@"), strings.HasPrefix(l, "##"):
		return "hunk"
	case strings.HasPrefix(l, "====="), strings.HasPrefix(l, "_____"):
		return "meta"
	}
	return ""
}

func (a *App) handleCommit(w http.ResponseWriter, r *http.Request, s *Session) {
	repo, ok := a.repoFor(w, r, s)
	if !ok {
		return
	}
	ctx := r.Context()
	dir := a.repoDir(repo.Name)
	youngest, err := a.svn.Youngest(ctx, dir)
	if err != nil {
		a.errorPage(w, r, s, 500, err)
		return
	}
	repo.Youngest = youngest
	rev, err := strconv.ParseInt(r.PathValue("rev"), 10, 64)
	if err != nil || rev < 0 || rev > youngest {
		a.notFound(w, r, s)
		return
	}
	info, err := a.svn.Info(ctx, dir, rev)
	if err != nil {
		a.errorPage(w, r, s, 500, err)
		return
	}
	changes, err := a.svn.Changed(ctx, dir, rev)
	if err != nil {
		a.errorPage(w, r, s, 500, err)
		return
	}
	pol := a.policy.Load()
	readable := func(p string) bool { return s.IsAdmin() || pol.CanRead(repo.Name, p, s.Username) }
	var visible []svn.Change
	for _, c := range changes {
		if readable(c.Path) {
			visible = append(visible, c)
		}
	}
	if len(visible) == 0 && len(changes) > 0 {
		a.notFound(w, r, s)
		return
	}
	hidden := len(visible) < len(changes)
	if hidden {
		info.Msg = ""
	}

	var files []*DiffFile
	var cur *DiffFile
	totalLines, totalBytes, truncated := 0, 0, false
	dp := &diffParser{
		onHeader: func(header, path string) {
			cur = nil
			if !readable(path) || len(files) >= maxDiffFiles {
				return
			}
			cur = &DiffFile{Path: path, Header: header}
			files = append(files, cur)
		},
		onLine: func(line string) {
			if cur == nil || cur.Truncated {
				return
			}
			if len(cur.Lines) >= 2000 {
				cur.Truncated = true
				return
			}
			if len(line) > 2000 {
				line = line[:2000] + " …"
			}
			cur.Lines = append(cur.Lines, DiffLine{Text: line, Class: diffClass(line)})
			totalLines++
			totalBytes += len(line)
		},
	}
	err = a.svn.DiffLines(ctx, dir, rev, func(line string) bool {
		if totalLines >= maxDiffLines || totalBytes >= maxDiffBytes {
			truncated = true
			return false
		}
		dp.feed(line)
		return true
	})
	dp.flush()
	v := &View{Title: i18n.Raw(repo.Name + " r" + strconv.FormatInt(rev, 10)), Nav: "repos", Tab: "commits", Repo: repo, D: map[string]any{
		"Info": info, "Changes": visible, "HiddenPaths": hidden, "Files": files, "Truncated": truncated,
	}}
	if err != nil {
		v.D["DiffErr"] = err.Error()
	}
	a.render(w, r, s, http.StatusOK, "repo_commit", v)
}

func (a *App) handleRawDiff(w http.ResponseWriter, r *http.Request, s *Session) {
	repo, ok := a.repoFor(w, r, s)
	if !ok {
		return
	}
	ctx := r.Context()
	dir := a.repoDir(repo.Name)
	youngest, err := a.svn.Youngest(ctx, dir)
	if err != nil {
		a.errorPage(w, r, s, 500, err)
		return
	}
	rev, err := strconv.ParseInt(r.PathValue("rev"), 10, 64)
	if err != nil || rev < 1 || rev > youngest {
		a.notFound(w, r, s)
		return
	}
	pol := a.policy.Load()
	readable := func(p string) bool { return s.IsAdmin() || pol.CanRead(repo.Name, p, s.Username) }
	changes, err := a.svn.Changed(ctx, dir, rev)
	if err != nil {
		a.errorPage(w, r, s, 500, err)
		return
	}
	anyReadable := false
	for _, c := range changes {
		if readable(c.Path) {
			anyReadable = true
			break
		}
	}
	if !anyReadable && len(changes) > 0 {
		a.notFound(w, r, s)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+repo.Name+"-r"+strconv.FormatInt(rev, 10)+".diff\"")
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Hour))
	bw := bufio.NewWriterSize(w, 64<<10)
	defer bw.Flush()
	show := false
	dp := &diffParser{
		onHeader: func(header, path string) {
			show = readable(path)
			if show {
				bw.WriteString(header + "\n")
			}
		},
		onLine: func(line string) {
			if show {
				bw.WriteString(line + "\n")
			}
		},
	}
	a.svn.DiffLines(ctx, dir, rev, func(line string) bool {
		dp.feed(line)
		return ctx.Err() == nil
	})
	dp.flush()
}
