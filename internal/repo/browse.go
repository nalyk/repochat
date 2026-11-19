package repo

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

const (
	DefaultListLimit = 300
	MaxListLimit     = 1000
	DefaultListDepth = 2
	MaxListDepth     = 12
	DefaultFindLimit = 50
	MaxFindLimit     = 200
)

// ---------------------------------------------------------------------------
// Overview
// ---------------------------------------------------------------------------

// Overview is the rendered repository summary.
type Overview struct {
	Text       string
	Generation string
	Head       string
	Branch     string
}

var manifestNames = set(
	"readme", "readme.md", "readme.rst", "readme.txt", "license", "license.md", "contributing.md", "changelog.md",
	"claude.md", "agents.md", "makefile", "dockerfile", "docker-compose.yml", "docker-compose.yaml", "compose.yaml",
	"go.mod", "package.json", "pnpm-workspace.yaml", "tsconfig.json", "pyproject.toml", "requirements.txt", "setup.py",
	"cargo.toml", "pom.xml", "build.gradle", "build.gradle.kts", "settings.gradle", "gemfile", "composer.json",
	"mix.exs", "cmakelists.txt", "justfile", "taskfile.yml", "angular.json", "next.config.js", "vite.config.ts",
	"deno.json", "flake.nix", "terraform.tf", "main.tf", ".gitlab-ci.yml", "jenkinsfile",
)

// Overview summarises the repository: where HEAD is, what changed, how the
// files are laid out and what this session is allowed to see.
func (r *Repo) Overview(ctx context.Context) (*Overview, error) {
	snap, err := r.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	st, err := r.status(ctx)
	if err != nil {
		return nil, err
	}
	res := &Overview{Generation: snap.Generation, Head: st.HeadOID, Branch: st.Branch}
	var b strings.Builder
	fmt.Fprintf(&b, "REPOSITORY %s (id %s)\n", r.Name, r.ID)
	b.WriteString(describeHead(st) + "\n")
	if !st.Unborn && !r.Opts.NoHistory {
		out, err := r.git(ctx, 64<<10, "log", "-1", "--no-walk", "--date=short", "--format=%h%x00%ad%x00%an%x00%s", "HEAD")
		if err == nil {
			if f := strings.Split(strings.TrimRight(string(out), "\n"), "\x00"); len(f) == 4 {
				subject, _ := r.red.Line(f[3])
				fmt.Fprintf(&b, "Last commit: %s %s %s — %s\n", f[0], f[1], SanitizeLine(f[2]), SanitizeLine(subject))
			}
		}
	}

	pol := snap.Policy
	staged, unstaged, conflicts, changedWithheld := 0, 0, 0, 0
	for _, e := range st.Entries {
		if pol.Check(e.Path) != "" {
			changedWithheld++
			continue
		}
		switch {
		case e.Unmerged:
			conflicts++
		default:
			if e.X != '.' {
				staged++
			}
			if e.Y != '.' {
				unstaged++
			}
		}
	}
	untracked := 0
	for _, p := range st.Untracked {
		if !strings.HasSuffix(p, "/") && pol.Check(p) == "" {
			untracked++
		}
	}
	if staged+unstaged+untracked+conflicts == 0 {
		b.WriteString("Working tree: clean\n")
	} else {
		fmt.Fprintf(&b, "Working tree: %d staged, %d unstaged, %d untracked", staged, unstaged, untracked)
		if conflicts > 0 {
			fmt.Fprintf(&b, ", %d with merge conflicts", conflicts)
		}
		b.WriteString(" (see git_status, git_diff)\n")
	}

	fmt.Fprintf(&b, "\nFILES: %d served (%d tracked, %d untracked)", snap.Visible, snap.Tracked, snap.Untracked)
	if n := snap.WithheldTotal(); n > 0 {
		fmt.Fprintf(&b, " · %d withheld by policy (%s)", n, snap.WithheldSummary())
	}
	b.WriteString("\n")

	dirs := map[string]int{}
	exts := map[string]int{}
	rootFiles := 0
	var manifests []string
	submodules, symlinks := 0, 0
	for _, e := range snap.Entries {
		if e.Withheld != "" {
			continue
		}
		switch e.Kind {
		case KindSubmodule:
			submodules++
		case KindSymlink:
			symlinks++
		}
		if i := strings.IndexByte(e.Path, '/'); i >= 0 {
			dirs[e.Path[:i]+"/"]++
		} else {
			rootFiles++
		}
		base := path.Base(e.Path)
		ext := strings.ToLower(path.Ext(base))
		if ext == "" || ext == base {
			ext = "(no extension)"
		}
		exts[ext]++
		if depth := strings.Count(e.Path, "/"); depth <= 1 && manifestNames[strings.ToLower(base)] && len(manifests) < 40 {
			manifests = append(manifests, e.Path)
		}
	}
	b.WriteString("TOP-LEVEL\n")
	for _, kv := range topCounts(dirs, 40) {
		fmt.Fprintf(&b, "  %-40s %d file(s)\n", SanitizeLine(kv.k), kv.n)
	}
	if len(dirs) > 40 {
		fmt.Fprintf(&b, "  … %d more directories\n", len(dirs)-40)
	}
	if rootFiles > 0 {
		fmt.Fprintf(&b, "  %-40s %d file(s)\n", "(files in the root)", rootFiles)
	}
	b.WriteString("FILE TYPES: ")
	var parts []string
	for _, kv := range topCounts(exts, 15) {
		parts = append(parts, fmt.Sprintf("%s %d", SanitizeLine(kv.k), kv.n))
	}
	b.WriteString(strings.Join(parts, " · ") + "\n")
	if len(manifests) > 0 {
		b.WriteString("KEY FILES: " + SanitizeLine(strings.Join(manifests, ", ")) + "\n")
	}
	if submodules > 0 {
		fmt.Fprintf(&b, "SUBMODULES: %d (their contents are separate repositories and are not served)\n", submodules)
	}
	if symlinks > 0 {
		fmt.Fprintf(&b, "SYMLINKS: %d (listed, never followed)\n", symlinks)
	}

	b.WriteString("\nSESSION\n")
	b.WriteString("  access: read-only; files ignored by .gitignore are not part of this view\n")
	if r.Opts.TrackedOnly {
		b.WriteString("  untracked files: not served (--tracked-only)\n")
	}
	if r.Opts.NoHistory {
		b.WriteString("  history: disabled (--no-history); only the current working tree is available\n")
	} else {
		b.WriteString("  history: commits reachable from HEAD, branches, remote branches and tags\n")
	}
	if r.red.Enabled() {
		b.WriteString("  masking: secret-looking values appear as [REDACTED:…] (best effort)\n")
	} else {
		b.WriteString("  masking: off\n")
	}
	fmt.Fprintf(&b, "  limits: %s per result; long results say how to continue\n", humanSize(int64(r.Budget())))
	for _, w := range snap.Warnings {
		fmt.Fprintf(&b, "  warning: %s\n", SanitizeLine(w))
	}
	b.WriteString("\nNEXT: list_files / find_files to navigate, search_code to locate text, read_file to read, file_outline for structure, git_log / git_diff / git_show / git_blame for history.\n")
	res.Text = b.String()
	return res, nil
}

type countKV struct {
	k string
	n int
}

func topCounts(m map[string]int, limit int) []countKV {
	out := make([]countKV, 0, len(m))
	for k, n := range m {
		out = append(out, countKV{k, n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].k < out[j].k
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// ---------------------------------------------------------------------------
// Listing
// ---------------------------------------------------------------------------

// ListOptions selects what list_files shows.
type ListOptions struct {
	Path   string // directory to list ("" = repository root)
	Depth  int    // levels below Path shown individually; deeper directories are summarised
	Glob   string // optional gitignore-style filter
	Limit  int
	Offset int
}

// ListResult is the rendered listing.
type ListResult struct {
	Text       string
	Shown      int
	Total      int
	NextOffset int // 0 when the listing is complete
	Generation string
}

// List shows the files under a directory, summarising deep subtrees.
func (r *Repo) List(ctx context.Context, o ListOptions) (*ListResult, error) {
	snap, err := r.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	prefix := ""
	if strings.TrimSpace(o.Path) != "" && o.Path != "." && o.Path != "/" {
		// The directory is the first reading of the path (see pathForms)
		// that names something the caller may see; a listing prints
		// directory names as they are, blanks and backslashes included.
		forms, err := pathForms(o.Path)
		if err != nil {
			return nil, err
		}
		rel := forms[len(forms)-1]
		for _, f := range forms {
			if reason := snap.Policy.Check(f); reason != "" {
				return nil, withheldError(f, reason)
			}
			if _, ok := snap.Lookup(f); ok {
				return nil, fmt.Errorf("%s is a file, not a directory; use read_file to read it", f)
			}
			if snap.hasVisibleBelow(f, r.Opts.ShowWithheldNames) {
				rel = f
				break
			}
		}
		prefix = rel + "/"
	}
	if o.Depth < 1 {
		o.Depth = DefaultListDepth
	}
	o.Depth = min(o.Depth, MaxListDepth)
	if o.Limit < 1 {
		o.Limit = DefaultListLimit
	}
	o.Limit = min(o.Limit, MaxListLimit)
	o.Offset = max(o.Offset, 0)
	var glob *Glob
	if strings.TrimSpace(o.Glob) != "" {
		if glob, err = CompileGlob(o.Glob); err != nil {
			return nil, fmt.Errorf("glob: %w", err)
		}
	}

	type row struct {
		text  string
		path  string // set for files that should be stat'ed
		entry Entry
	}
	// Unless the owner chose to show their names, withheld paths take no part
	// in anything below: not in the rows, not in the counts, and not in the
	// difference between an empty listing and a directory that does not
	// exist. Otherwise the caller's path and glob could be tested against
	// withheld names and recover them one character at a time.
	var rows []row
	underPrefix, inScope := 0, 0
	collapsed := map[string]int{}
	for _, e := range snap.Entries {
		if e.Withheld != "" && !r.Opts.ShowWithheldNames {
			continue
		}
		if prefix != "" && !strings.HasPrefix(e.Path, prefix) {
			continue
		}
		underPrefix++
		if glob != nil && !glob.Match(e.Path) {
			continue
		}
		inScope++
		relToPrefix := e.Path[len(prefix):]
		segs := strings.Split(relToPrefix, "/")
		if len(segs) > o.Depth {
			dir := prefix + strings.Join(segs[:o.Depth], "/") + "/"
			if _, seen := collapsed[dir]; !seen {
				rows = append(rows, row{text: dir})
			}
			collapsed[dir]++
			continue
		}
		rows = append(rows, row{path: e.Path, entry: e})
	}
	if underPrefix == 0 && prefix != "" {
		return nil, fmt.Errorf("no files under %s (not a directory in this repository's view, or everything in it is ignored by .gitignore or withheld)", strings.TrimSuffix(prefix, "/"))
	}
	if inScope == 0 && glob != nil {
		return &ListResult{Text: fmt.Sprintf("FILES matching %q: none\n", SanitizeLine(o.Glob)) + withheldNote(snap, r.Opts.ShowWithheldNames), Generation: snap.Generation}, nil
	}
	res := &ListResult{Total: len(rows), Generation: snap.Generation}
	if o.Offset > len(rows) {
		return nil, errors.New("cursor is past the end of this listing; restart without cursor")
	}

	op := r.newOpener()
	defer op.Close()
	var b strings.Builder
	where := "repository root"
	if prefix != "" {
		where = strings.TrimSuffix(prefix, "/")
	}
	end := min(o.Offset+o.Limit, len(rows))
	fmt.Fprintf(&b, "FILES under %s · entries %d-%d of %d", SanitizeLine(where), min(o.Offset+1, len(rows)), end, len(rows))
	if glob != nil {
		fmt.Fprintf(&b, " · glob %q", SanitizeLine(o.Glob))
	}
	fmt.Fprintf(&b, " · depth %d\n", o.Depth)
	for _, rw := range rows[o.Offset:end] {
		res.Shown++
		if rw.path == "" {
			// The name between plain quotes, not Go-quoted: a doubled
			// backslash would be the name of a different directory.
			fmt.Fprintf(&b, "  %s  (%d file(s) below; list it with path=\"%s\")\n", SanitizeLine(rw.text), collapsed[rw.text], SanitizeLine(strings.TrimSuffix(rw.text, "/")))
			continue
		}
		e := rw.entry
		note := ""
		switch {
		case e.Withheld != "":
			note = "[withheld: " + e.Withheld + "]"
		case e.Kind == KindSymlink:
			note = "[symlink, not followed]"
		case e.Kind == KindSubmodule:
			note = "[submodule, not served]"
		default:
			if fi, err := op.Lstat(e.Path); err != nil {
				note = "[not in working tree]"
			} else if !fi.Mode().IsRegular() {
				note = "[not a regular file]"
			} else {
				note = humanSize(fi.Size())
			}
			if !e.Tracked {
				note += " · untracked"
			}
			if e.Conflict {
				note += " · merge conflict"
			}
		}
		fmt.Fprintf(&b, "  %s  %s%s\n", SanitizeLine(e.Path), note, exactNameNote(e.Path))
	}
	if end < len(rows) {
		res.NextOffset = end
		b.WriteString("[more entries: pass the cursor from @meta to continue]\n")
	}
	b.WriteString(withheldNote(snap, r.Opts.ShowWithheldNames))
	res.Text = b.String()
	return res, nil
}

// exactNameNote spells out a name that begins or ends with a blank, which the
// layout of a listing cannot show. Such a name has to be passed back exactly
// (see pathForms); without the note the model would not know it has blanks.
func exactNameNote(p string) string {
	if p == strings.TrimSpace(p) {
		return ""
	}
	return `  [exact name: "` + SanitizeLine(p) + `"]`
}

// withheldNote tells the model that withheld paths exist, so "not listed" is
// not taken for "absent". It is built from the repository's totals alone and
// is therefore the same whatever was asked for: it cannot be used to test a
// path or pattern against the names of withheld files.
func withheldNote(snap *Snapshot, namesShown bool) string {
	n := snap.WithheldTotal()
	if n == 0 || namesShown {
		return ""
	}
	return fmt.Sprintf("[%d path(s) in this repository are withheld by policy (%s); they are never listed, whether or not they match this request]\n", n, snap.WithheldSummary())
}

// ---------------------------------------------------------------------------
// Find
// ---------------------------------------------------------------------------

// FindResult is the rendered path search.
type FindResult struct {
	Text  string
	Shown int
	Total int
}

// Find locates files by name: a case-insensitive substring of the path, or a
// gitignore-style pattern when the query contains *, ? or [.
func (r *Repo) Find(ctx context.Context, query string, limit int) (*FindResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("query must not be empty")
	}
	if limit < 1 {
		limit = DefaultFindLimit
	}
	limit = min(limit, MaxFindLimit)
	snap, err := r.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	var glob *Glob
	if strings.ContainsAny(query, "*?[") {
		if glob, err = CompileGlob(query); err != nil {
			return nil, fmt.Errorf("pattern: %w", err)
		}
	}
	lq := strings.ToLower(strings.TrimPrefix(query, "./"))
	type hit struct {
		path string
		rank int
	}
	// As in List: a withheld path is never matched against the query unless
	// the owner shows withheld names, so the answer says nothing about them.
	var hits []hit
	withheld := 0
	for _, e := range snap.Entries {
		if e.Withheld != "" && !r.Opts.ShowWithheldNames {
			continue
		}
		lp := strings.ToLower(e.Path)
		rank := -1
		if glob != nil {
			if glob.Match(e.Path) {
				rank = 3
			}
		} else if strings.Contains(lp, lq) {
			base := path.Base(lp)
			stem := strings.TrimSuffix(base, path.Ext(base))
			switch {
			case lp == lq || base == lq:
				rank = 0
			case stem == lq:
				rank = 1
			case strings.HasPrefix(base, lq):
				rank = 2
			case strings.Contains(base, lq):
				rank = 3
			default:
				rank = 4
			}
		}
		if rank < 0 {
			continue
		}
		if e.Withheld != "" {
			withheld++
			continue
		}
		hits = append(hits, hit{e.Path, rank})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].rank != hits[j].rank {
			return hits[i].rank < hits[j].rank
		}
		if a, b := strings.Count(hits[i].path, "/"), strings.Count(hits[j].path, "/"); a != b {
			return a < b
		}
		return hits[i].path < hits[j].path
	})
	res := &FindResult{Total: len(hits), Shown: min(len(hits), limit)}
	var b strings.Builder
	fmt.Fprintf(&b, "FIND %q: %d file(s)", SanitizeLine(query), len(hits))
	if len(hits) > limit {
		fmt.Fprintf(&b, ", showing the best %d", limit)
	}
	b.WriteString("\n")
	for _, h := range hits[:res.Shown] {
		b.WriteString("  " + SanitizeLine(h.path) + exactNameNote(h.path) + "\n")
	}
	if len(hits) > limit {
		b.WriteString("[more matches exist: use a more specific query or a pattern such as src/**/*.ts]\n")
	}
	if withheld > 0 {
		fmt.Fprintf(&b, "[%d matching path(s) are withheld by policy and not listed]\n", withheld)
	}
	if len(hits) == 0 && withheld == 0 {
		b.WriteString("[no file path matches. Files ignored by .gitignore are not part of this view. To search file CONTENTS use search_code]\n")
	}
	b.WriteString(withheldNote(snap, r.Opts.ShowWithheldNames))
	res.Text = b.String()
	return res, nil
}
