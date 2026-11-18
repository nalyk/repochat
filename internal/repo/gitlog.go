package repo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultLogLimit = 20
	MaxLogLimit     = 100
	maxLogWithStats = 30
	maxLogScan      = 2000 // commits examined per call when a filter is applied in-process
	maxLogStatFiles = 25
	MaxBlameLines   = 200
	maxRefsPerKind  = 200
)

// LogOptions selects commits.
type LogOptions struct {
	Limit     int
	Skip      int
	Rev       string // start from this revision (default HEAD)
	Range     string // "a..b": commits reachable from b and not from a
	Path      string // only commits touching this file or directory
	Author    string
	Since     string
	Until     string
	Grep      string // case-insensitive literal in the commit message
	Pickaxe   string // commits that add or remove this exact string (-S)
	WithStats bool
}

// LogResult is the rendered commit list.
type LogResult struct {
	Text       string
	Count      int
	More       bool
	NextSkip   int
	Redactions int
}

type logCommit struct {
	oid, parents, date, author, refs, subject string
}

// Log lists commits, newest first.
func (r *Repo) Log(ctx context.Context, o LogOptions) (*LogResult, error) {
	if r.Opts.NoHistory {
		return nil, ErrNoHistory
	}
	if o.Limit < 1 {
		o.Limit = DefaultLogLimit
	}
	o.Limit = min(o.Limit, MaxLogLimit)
	if o.WithStats {
		o.Limit = min(o.Limit, maxLogWithStats)
	}
	o.Skip = max(o.Skip, 0)
	pol, err := r.Policy(ctx)
	if err != nil {
		return nil, err
	}

	var revs []string
	title := ""
	switch {
	case o.Range != "" && o.Rev != "":
		return nil, errors.New("use either rev or range, not both")
	case o.Range != "":
		if strings.Contains(o.Range, "...") {
			return nil, errors.New(`range must have the form "a..b" (commits reachable from b and not from a)`)
		}
		a, b, ok := strings.Cut(o.Range, "..")
		if !ok || a == "" {
			return nil, errors.New(`range must have the form "a..b" (commits reachable from b and not from a)`)
		}
		if b == "" {
			b = "HEAD"
		}
		aOID, err := r.ResolveCommit(ctx, a)
		if err != nil {
			return nil, fmt.Errorf("range start: %w", err)
		}
		bOID, err := r.ResolveCommit(ctx, b)
		if err != nil {
			return nil, fmt.Errorf("range end: %w", err)
		}
		revs = []string{bOID, "^" + aOID}
		title = fmt.Sprintf("commits in %s and not in %s", short(bOID), short(aOID))
	default:
		rev := o.Rev
		if rev == "" {
			if _, ok, err := r.head(ctx); err != nil {
				return nil, err
			} else if !ok {
				return &LogResult{Text: "LOG\n[this repository has no commits yet]\n"}, nil
			}
			rev = "HEAD"
		}
		oid, err := r.ResolveCommit(ctx, rev)
		if err != nil {
			return nil, err
		}
		revs = []string{oid}
		title = "history of " + short(oid)
	}

	// With masking on, the message filter runs here, on the MASKED message,
	// and paging counts matched commits only. What the caller can observe
	// (results, counts, next_skip) therefore depends on disclosed text alone
	// and cannot be used to test guesses about masked content.
	maskedGrep := o.Grep != "" && r.red.Enabled()
	maskedAuthor := o.Author != "" && r.red.Enabled()
	format := "--format=%H%x00%P%x00%ad%x00%an%x00%D%x00%s"
	fields := 6
	if maskedGrep || maskedAuthor {
		format += "%x00%ae%x00%B"
		fields = 8
	}
	args := []string{"log", "-z", "--no-color", "--date=short", format}
	var filters []string
	if o.Author != "" {
		if !maskedAuthor {
			args = append(args, "-i", "--author="+o.Author)
		}
		filters = append(filters, "author~"+strconv.Quote(o.Author))
	}
	for _, d := range []struct{ flag, v string }{{"since", o.Since}, {"until", o.Until}} {
		if d.v == "" {
			continue
		}
		if !dateRe.MatchString(d.v) {
			return nil, fmt.Errorf("invalid %s: use a date such as 2026-01-31 or a phrase such as \"2 weeks ago\"", d.flag)
		}
		args = append(args, "--"+d.flag+"="+d.v)
		filters = append(filters, d.flag+"="+d.v)
	}
	if o.Grep != "" {
		if !maskedGrep {
			args = append(args, "-i", "--fixed-strings", "--grep="+o.Grep)
		}
		filters = append(filters, "message~"+strconv.Quote(o.Grep))
	}
	if o.Pickaxe != "" {
		if r.red.Enabled() {
			return nil, errors.New("pickaxe is unavailable while secret masking is on, because it would let a search test for masked text. Use grep (commit messages) or search_code (current files) instead; the repository owner can start repochat with --no-redact to enable it")
		}
		args = append(args, "-S"+o.Pickaxe)
		filters = append(filters, "pickaxe="+strconv.Quote(o.Pickaxe))
	}
	var pathArgs []string
	if o.Path != "" {
		rel, err := CheckPath(o.Path)
		if err != nil {
			return nil, err
		}
		if reason := pol.Check(rel); reason != "" {
			return nil, withheldError(rel, reason)
		}
		pathArgs = []string{rel}
		filters = append(filters, "path="+rel)
	}
	// Filters applied here rather than by git need a wider look: scan a
	// bounded number of commits and count matches ourselves.
	local := maskedGrep || maskedAuthor || o.Pickaxe != ""
	fetch := o.Limit + 1
	if local {
		fetch = maxLogScan + 1
	} else {
		args = append(args, "--skip="+strconv.Itoa(o.Skip))
	}
	args = append(args, "-n", strconv.Itoa(fetch))
	args = append(args, revs...)
	args = append(args, "--")
	args = append(args, pathArgs...)

	out, truncated, code, err := r.gitRaw(ctx, 32<<20, args...)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, r.gitFailure("log", out)
	}
	toks := splitNUL(out)
	if truncated {
		toks = toks[:len(toks)/fields*fields] // drop the incomplete last record
	} else if len(toks)%fields != 0 {
		return nil, errors.New("unexpected output from git log")
	}
	res := &LogResult{}
	var commits []logCommit
	scanned, matched := 0, 0
	scanCapped := truncated
	needle, authorNeedle := strings.ToLower(o.Grep), strings.ToLower(o.Author)
	for i := 0; i+fields <= len(toks); i += fields {
		c := logCommit{toks[i], toks[i+1], toks[i+2], toks[i+3], toks[i+4], toks[i+5]}
		scanned++
		if local && scanned > maxLogScan {
			scanCapped = true
			break
		}
		if maskedAuthor {
			masked, _ := r.red.Line(c.author + " <" + toks[i+6] + ">")
			if !strings.Contains(strings.ToLower(masked), authorNeedle) {
				continue
			}
		}
		if maskedGrep {
			masked, _ := r.red.Text(toks[i+7], false)
			if !strings.Contains(strings.ToLower(masked), needle) {
				continue
			}
		}
		if o.Pickaxe != "" {
			ok, err := r.pickaxeTouchesServedPath(ctx, c.oid, o.Pickaxe, pol)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		matched++
		if local && matched <= o.Skip {
			continue
		}
		if len(commits) == o.Limit {
			res.More = true
			break
		}
		commits = append(commits, c)
	}
	res.Count = len(commits)
	res.NextSkip = o.Skip + len(commits)

	var b strings.Builder
	fmt.Fprintf(&b, "LOG %s", title)
	if len(filters) > 0 {
		fmt.Fprintf(&b, " · %s", SanitizeLine(strings.Join(filters, ", ")))
	}
	fmt.Fprintf(&b, " · %d commit(s)", len(commits))
	if o.Skip > 0 {
		fmt.Fprintf(&b, " after skipping %d", o.Skip)
	}
	b.WriteString("\n")
	for _, c := range commits {
		subject, n := r.red.Line(c.subject)
		res.Redactions += n
		author, n := r.red.Line(c.author)
		res.Redactions += n
		line := fmt.Sprintf("%s  %s  %s", short(c.oid), c.date, SanitizeLine(author))
		if c.refs != "" {
			line += "  (" + SanitizeLine(c.refs) + ")"
		}
		if strings.Contains(c.parents, " ") {
			line += "  [merge]"
		}
		b.WriteString(line + "\n    " + SanitizeLine(subject) + "\n")
		if o.WithStats {
			stat, err := r.commitStat(ctx, c, pol)
			if err != nil {
				return nil, err
			}
			b.WriteString(stat)
		}
	}
	if len(commits) == 0 {
		b.WriteString("[no commits match]\n")
	}
	if res.More {
		fmt.Fprintf(&b, "[more commits: call git_log again with skip=%d]\n", res.NextSkip)
	} else if scanCapped {
		fmt.Fprintf(&b, "[only the %d most recent commits were examined for this filter; narrow with rev, path, since or until to look further back]\n", maxLogScan)
		res.More = true
	}
	if res.Redactions > 0 {
		fmt.Fprintf(&b, "[note: %d secret-looking value(s) masked as [REDACTED:…]]\n", res.Redactions)
	}
	b.WriteString("[use git_show with a commit hash to see its full message and changes]\n")
	res.Text = b.String()
	return res, nil
}

func (r *Repo) firstParentPlan(ctx context.Context, oid, parents string) (*diffPlan, error) {
	base := ""
	if p := strings.Fields(parents); len(p) > 0 {
		base = p[0]
	} else {
		var err error
		if base, err = r.emptyTreeOID(ctx); err != nil {
			return nil, err
		}
	}
	return r.treePlan(base, oid, ""), nil
}

func (r *Repo) commitStat(ctx context.Context, c logCommit, pol *Policy) (string, error) {
	plan, err := r.firstParentPlan(ctx, c.oid, c.parents)
	if err != nil {
		return "", err
	}
	entries, err := r.diffEntries(ctx, plan, nil)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	shown, withheld := 0, 0
	for _, e := range entries {
		if pol.Check(e.Path) != "" || (e.Orig != "" && pol.Check(e.Orig) != "") {
			withheld++
			continue
		}
		shown++
		if shown > maxLogStatFiles {
			continue
		}
		name := SanitizeLine(e.Path)
		if e.Orig != "" {
			name = SanitizeLine(e.Orig) + " → " + name
		}
		if e.Binary {
			fmt.Fprintf(&b, "      %c %s (binary)\n", e.Status, name)
		} else {
			fmt.Fprintf(&b, "      %c %s +%d −%d\n", e.Status, name, e.Add, e.Del)
		}
	}
	if shown > maxLogStatFiles {
		fmt.Fprintf(&b, "      … %d more file(s)\n", shown-maxLogStatFiles)
	}
	if withheld > 0 {
		fmt.Fprintf(&b, "      (+ %d withheld path(s))\n", withheld)
	}
	return b.String(), nil
}

// pickaxeTouchesServedPath reports whether the commit adds or removes the
// string in at least one path that may be served.
func (r *Repo) pickaxeTouchesServedPath(ctx context.Context, oid, needle string, pol *Policy) (bool, error) {
	out, err := r.git(ctx, diffListCap, "diff-tree", "-r", "--root", "--no-commit-id", "--name-only", "-z",
		"--no-ext-diff", "--no-textconv", "-S"+needle, oid, "--")
	if err != nil {
		return false, err
	}
	for _, p := range splitNUL(out) {
		if pol.Check(p) == "" {
			return true, nil
		}
	}
	return false, nil
}

// ---------------------------------------------------------------------------
// Blame
// ---------------------------------------------------------------------------

// BlameResult is the rendered attribution of a line range.
type BlameResult struct {
	Text       string
	First      int
	Last       int
	Total      int
	Rev        string
	Redactions int
}

type blameCommit struct {
	author, summary string
	when            int64
}

// Blame attributes lines [start, end] of a file to the commits that last
// changed them. The displayed text comes from the complete file run through
// the same masking as read_file, never from git's excerpt.
func (r *Repo) Blame(ctx context.Context, p string, start, end int, rev string) (*BlameResult, error) {
	if r.Opts.NoHistory {
		return nil, ErrNoHistory
	}
	rel, err := CheckPath(p)
	if err != nil {
		return nil, err
	}
	var source []byte
	oid := ""
	if rev != "" {
		if oid, err = r.ResolveCommit(ctx, rev); err != nil {
			return nil, err
		}
		pol, err := r.Policy(ctx)
		if err != nil {
			return nil, err
		}
		if reason := pol.Check(rel); reason != "" {
			return nil, withheldError(rel, reason)
		}
		if source, err = r.blobAt(ctx, oid, rel); err != nil {
			return nil, err
		}
	} else {
		_, entry, _, err := r.authorizeFile(ctx, p)
		if err != nil {
			return nil, err
		}
		if !entry.Tracked {
			return nil, fmt.Errorf("%s is untracked: it has no history to blame", rel)
		}
		if entry.Kind != KindRegular {
			return nil, fmt.Errorf("%s is not a regular file", rel)
		}
		if _, ok, err := r.head(ctx); err != nil {
			return nil, err
		} else if !ok {
			return nil, errors.New("this repository has no commits yet")
		}
		if err := r.beforeWorktreeRead(ctx); err != nil {
			return nil, err
		}
		op := r.newOpener()
		data, _, truncated, rerr := op.ReadAll(rel, maxRevBlobBytes)
		op.Close()
		if rerr != nil {
			return nil, describeErr(rel, rerr)
		}
		if truncated {
			return nil, fmt.Errorf("%s is larger than %s; blame is not available for it", rel, humanSize(maxRevBlobBytes))
		}
		source = data
	}
	if i := indexNUL(source); i >= 0 {
		return nil, fmt.Errorf("%s is a binary file", rel)
	}
	text := strings.ToValidUTF8(string(source), "\uFFFD")
	lines := strings.Split(text, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	total := len(lines)
	if total == 0 {
		return nil, fmt.Errorf("%s is empty", rel)
	}
	if start < 1 {
		start = 1
	}
	if start > total {
		return nil, fmt.Errorf("start_line %d is past the end of %s (%d lines)", start, rel, total)
	}
	if end < start {
		end = start + 49
	}
	end = min(end, total, start+MaxBlameLines-1)

	st := r.red.NewState()
	masked := make([]string, total)
	for i, l := range lines {
		masked[i] = st.Line(strings.TrimSuffix(l, "\r"))
	}

	args := []string{"blame", "--porcelain", "--no-textconv", "-L", fmt.Sprintf("%d,%d", start, end)}
	if oid != "" {
		args = append(args, oid)
	}
	args = append(args, "--", rel)
	out, _, code, err := r.gitRaw(ctx, 8<<20, args...)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, r.gitFailure("blame", out)
	}

	commits := map[string]*blameCommit{}
	lineCommit := map[int]string{}
	var cur *blameCommit
	for _, l := range strings.Split(string(out), "\n") {
		switch {
		case l == "" || l[0] == '\t':
			continue
		case len(l) >= 40 && isHex(l[:40]):
			f := strings.Fields(l)
			if len(f) < 3 {
				continue
			}
			n, _ := strconv.Atoi(f[2])
			lineCommit[n] = f[0]
			if commits[f[0]] == nil {
				commits[f[0]] = &blameCommit{}
			}
			cur = commits[f[0]]
		case cur == nil:
		case strings.HasPrefix(l, "author "):
			cur.author = l[len("author "):]
		case strings.HasPrefix(l, "author-time "):
			cur.when, _ = strconv.ParseInt(l[len("author-time "):], 10, 64)
		case strings.HasPrefix(l, "summary "):
			cur.summary = l[len("summary "):]
		}
	}

	res := &BlameResult{First: start, Last: end, Total: total, Rev: oid}
	label := func(id string) string {
		if strings.Trim(id, "0") == "" {
			return "uncommitted"
		}
		return short(id)
	}
	var b strings.Builder
	where := "working tree"
	if oid != "" {
		where = "at " + short(oid)
	}
	fmt.Fprintf(&b, "BLAME %s · lines %d-%d of %d · %s\n", rel, start, end, total, where)
	width := len(strconv.Itoa(end))
	prev := ""
	for n := start; n <= end; n++ {
		id := lineCommit[n]
		tag := label(id)
		if id == prev {
			tag = ""
		}
		prev = id
		content := masked[n-1]
		if len(content) > maxLineDisplay {
			content = truncateLine(content, false)
		}
		fmt.Fprintf(&b, "%*d| %-11s| %s\n", width, n, tag, Sanitize(content))
	}
	ids := make([]string, 0, len(commits))
	for id := range commits {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return commits[ids[i]].when > commits[ids[j]].when })
	b.WriteString("COMMITS\n")
	for _, id := range ids {
		c := commits[id]
		if strings.Trim(id, "0") == "" {
			b.WriteString("  uncommitted  changes in the working tree, not committed yet\n")
			continue
		}
		summary, n := r.red.Line(c.summary)
		res.Redactions += n
		fmt.Fprintf(&b, "  %s  %s  %s  %s\n", short(id), time.Unix(c.when, 0).UTC().Format("2006-01-02"),
			SanitizeLine(c.author), SanitizeLine(summary))
	}
	if end < total {
		fmt.Fprintf(&b, "[more lines: call git_blame again with start_line=%d]\n", end+1)
	}
	res.Redactions += st.Count()
	if res.Redactions > 0 {
		fmt.Fprintf(&b, "[note: %d secret-looking value(s) masked as [REDACTED:…]]\n", res.Redactions)
	}
	res.Text = b.String()
	return res, nil
}

func indexNUL(b []byte) int {
	for i, c := range b[:min(len(b), 8000)] {
		if c == 0 {
			return i
		}
	}
	return -1
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Refs
// ---------------------------------------------------------------------------

// RefsResult lists branches and tags.
type RefsResult struct {
	Text                   string
	Local, Remote, TagRefs int
}

// Refs lists local branches, remote branches and tags. filter is an optional
// case-insensitive substring of the ref name.
func (r *Repo) Refs(ctx context.Context, filter string) (*RefsResult, error) {
	if r.Opts.NoHistory {
		return nil, ErrNoHistory
	}
	st, err := r.status(ctx)
	if err != nil {
		return nil, err
	}
	out, err := r.git(ctx, 32<<20, "for-each-ref", "--sort=-creatordate",
		"--format=%(refname)%00%(objectname)%00%(creatordate:short)%00%(contents:subject)%00%(upstream:short)%00%(upstream:track)",
		"refs/heads/", "refs/remotes/", "refs/tags/")
	if err != nil {
		return nil, err
	}
	filter = strings.ToLower(strings.TrimSpace(filter))
	type ref struct{ name, oid, date, subject, upstream, track string }
	var local, remote, tags []ref
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Split(line, "\x00")
		if len(f) != 6 {
			continue
		}
		x := ref{f[0], f[1], f[2], f[3], f[4], f[5]}
		var dst *[]ref
		switch {
		case strings.HasPrefix(x.name, "refs/heads/"):
			x.name, dst = strings.TrimPrefix(x.name, "refs/heads/"), &local
		case strings.HasPrefix(x.name, "refs/remotes/"):
			x.name, dst = strings.TrimPrefix(x.name, "refs/remotes/"), &remote
			if strings.HasSuffix(x.name, "/HEAD") {
				continue
			}
		case strings.HasPrefix(x.name, "refs/tags/"):
			x.name, dst = strings.TrimPrefix(x.name, "refs/tags/"), &tags
		default:
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(x.name), filter) {
			continue
		}
		*dst = append(*dst, x)
	}
	res := &RefsResult{Local: len(local), Remote: len(remote), TagRefs: len(tags)}
	var b strings.Builder
	b.WriteString("REFS · " + describeHead(st) + "\n")
	section := func(title string, refs []ref, isLocal bool) {
		fmt.Fprintf(&b, "%s (%d)\n", title, len(refs))
		for i, x := range refs {
			if i == maxRefsPerKind {
				fmt.Fprintf(&b, "  … %d more; narrow with filter\n", len(refs)-i)
				break
			}
			mark := " "
			if isLocal && !st.Detached && x.name == st.Branch {
				mark = "*"
			}
			subject, _ := r.red.Line(x.subject)
			line := fmt.Sprintf(" %s %s  %s  %s  %s", mark, SanitizeLine(x.name), short(x.oid), x.date, SanitizeLine(subject))
			if x.upstream != "" {
				line += "  → " + SanitizeLine(x.upstream)
				if x.track != "" {
					line += " " + SanitizeLine(x.track)
				}
			}
			b.WriteString(line + "\n")
		}
	}
	section("LOCAL BRANCHES", local, true)
	section("REMOTE BRANCHES", remote, false)
	section("TAGS", tags, false)
	b.WriteString("[newest first. Any of these names can be used as rev/base/target in the git tools]\n")
	res.Text = b.String()
	return res, nil
}
