package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Diff modes.
const (
	DiffHead     = "head"     // all uncommitted changes: HEAD → working tree
	DiffStaged   = "staged"   // HEAD → index
	DiffWorktree = "worktree" // index → working tree
	DiffRange    = "range"    // base → target
)

const (
	MaxDiffContext   = 10
	maxDiffPathArgs  = 20
	maxDiffListed    = 400
	maxDiffPatches   = 400
	diffListCap      = 32 << 20
	minPerFilePatch  = 16 << 10
	diffHeaderMargin = 2048
)

// DiffOptions selects what to compare.
type DiffOptions struct {
	Mode      string
	Base      string // range mode
	Target    string // range mode (default HEAD)
	MergeBase bool   // range mode: compare merge-base(base, target) → target
	Paths     []string
	StatOnly  bool
	Context   int
}

// DiffResult is the rendered comparison.
type DiffResult struct {
	Text     string
	Files    int      // changed paths that may be shown
	Withheld int      // changed paths withheld by policy
	Shown    int      // patches included
	Omitted  []string // allowed paths whose patch was left out (size budget)
	Cut      []string // paths whose patch was included but cut short
	// Incomplete reports anything else that a caller asking for patches did
	// not get: changed files beyond the listing limit, a patch held back
	// because it could not be examined for key material, a conflicted file.
	Incomplete bool
	Redactions int
	BaseOID    string
	TargetOID  string
}

type diffEntry struct {
	Status   byte
	Path     string
	Orig     string
	Add, Del int
	Binary   bool
	HasStat  bool
}

const (
	sideNone = iota // nothing exists on this side
	sideTree        // a commit or tree object
	sideIndex
	sideWorktree
)

type diffSide struct {
	kind int
	oid  string
}

type diffPlan struct {
	cmd      []string // git subcommand and its mode flags
	revs     []string
	old, new diffSide
	title    string
}

// diffFlags go on every diff command, whichever sides it compares.
//
// A submodule is a separate repository with its own untrusted configuration
// and files that are not in the served set, so git must never enter one:
// --submodule=short keeps a changed gitlink a two-line "Subproject commit"
// patch (the "log" and "diff" formats print the submodule's commit subjects
// or run `git diff` inside it, under its configuration), and
// --ignore-submodules=dirty stops git from running `git status` inside it.
// Both are given on the command line because that is the only level that
// outranks .gitmodules and submodule.<name>.* settings.
var diffFlags = []string{"--no-color", "--no-ext-diff", "--no-textconv", "--submodule=short", "--ignore-submodules=dirty", "-M"}

// Diff compares two states of the repository. Only changes to served paths
// are ever shown; a change set that touches nothing but withheld paths yields
// counts and no content, and never widens into an unrestricted diff.
func (r *Repo) Diff(ctx context.Context, o DiffOptions, budget int) (*DiffResult, error) {
	// Every mode has a commit or the index on its old side, and the removed
	// lines of a patch are content that exists only there. (The index is
	// refused as well: without history it is the last commit plus whatever
	// was staged, and the two cannot be told apart from here.)
	if r.Opts.NoHistory {
		return nil, ErrNoHistory
	}
	plan, err := r.planDiff(ctx, o)
	if err != nil {
		return nil, err
	}
	if plan.new.kind == sideWorktree {
		if err := r.beforeWorktreeRead(ctx); err != nil {
			return nil, err
		}
		private, release, err := r.withPrivateIndex(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
		ctx = private
	}
	res := &DiffResult{}
	if plan.old.kind == sideTree {
		res.BaseOID = plan.old.oid
	}
	if plan.new.kind == sideTree {
		res.TargetOID = plan.new.oid
	}
	body, err := r.renderDiff(ctx, plan, o, budget-len(plan.title)-diffHeaderMargin, res)
	if err != nil {
		return nil, err
	}
	res.Text = "DIFF " + plan.title + "\n" + body
	return res, nil
}

func (r *Repo) planDiff(ctx context.Context, o DiffOptions) (*diffPlan, error) {
	mode := o.Mode
	if mode == "" {
		mode = DiffHead
		if o.Base != "" || o.Target != "" {
			mode = DiffRange
		}
	}
	if mode != DiffRange && (o.Base != "" || o.Target != "" || o.MergeBase) {
		return nil, fmt.Errorf("base, target and merge_base only apply to mode=%q", DiffRange)
	}
	headOID, hasHead, err := r.head(ctx)
	if err != nil {
		return nil, err
	}
	headSide := diffSide{kind: sideTree, oid: headOID}
	headName := "HEAD " + short(headOID)
	if !hasHead {
		empty, err := r.emptyTreeOID(ctx)
		if err != nil {
			return nil, err
		}
		headSide = diffSide{kind: sideTree, oid: empty}
		headName = "(no commits yet)"
	}
	switch mode {
	case DiffHead:
		return &diffPlan{cmd: []string{"diff"}, revs: []string{headSide.oid},
			old: headSide, new: diffSide{kind: sideWorktree},
			title: "all uncommitted changes: " + headName + " → working tree"}, nil
	case DiffStaged:
		return &diffPlan{cmd: []string{"diff", "--cached"}, revs: []string{headSide.oid},
			old: headSide, new: diffSide{kind: sideIndex},
			title: "staged changes: " + headName + " → index"}, nil
	case DiffWorktree:
		return &diffPlan{cmd: []string{"diff"},
			old: diffSide{kind: sideIndex}, new: diffSide{kind: sideWorktree},
			title: "unstaged changes: index → working tree"}, nil
	case DiffRange:
		if o.Base == "" {
			return nil, errors.New(`mode "range" needs base (and optionally target, default HEAD)`)
		}
		target := o.Target
		if target == "" {
			target = "HEAD"
		}
		baseOID, err := r.ResolveCommit(ctx, o.Base)
		if err != nil {
			return nil, fmt.Errorf("base: %w", err)
		}
		targetOID, err := r.ResolveCommit(ctx, target)
		if err != nil {
			return nil, fmt.Errorf("target: %w", err)
		}
		title := fmt.Sprintf("%s → %s", short(baseOID), short(targetOID))
		if o.MergeBase {
			out, err := r.git(ctx, 64<<10, "merge-base", "--all", baseOID, targetOID)
			if err != nil {
				return nil, errors.New("base and target have no common ancestor; use merge_base=false to compare them directly")
			}
			bases := strings.Fields(string(out))
			switch {
			case len(bases) == 0:
				return nil, errors.New("base and target have no common ancestor; use merge_base=false to compare them directly")
			case len(bases) > 1:
				return nil, fmt.Errorf("base and target have %d merge bases (criss-cross history); pick one explicitly as base (%s) and use merge_base=false",
					len(bases), strings.Join(shortAll(bases), ", "))
			}
			title = fmt.Sprintf("merge-base %s (of %s and %s) → %s", short(bases[0]), short(baseOID), short(targetOID), short(targetOID))
			baseOID = bases[0]
		}
		return r.treePlan(baseOID, targetOID, title), nil
	default:
		return nil, fmt.Errorf("unknown mode %q (use head, staged, worktree or range)", mode)
	}
}

func (r *Repo) treePlan(baseOID, targetOID, title string) *diffPlan {
	return &diffPlan{cmd: []string{"diff-tree", "-r"}, revs: []string{baseOID, targetOID},
		old: diffSide{kind: sideTree, oid: baseOID}, new: diffSide{kind: sideTree, oid: targetOID}, title: title}
}

func shortAll(oids []string) []string {
	out := make([]string, len(oids))
	for i, o := range oids {
		out[i] = short(o)
	}
	return out
}

func (p *diffPlan) args(extra []string, paths []string) []string {
	a := append([]string{}, p.cmd...)
	a = append(a, diffFlags...)
	a = append(a, extra...)
	a = append(a, p.revs...)
	a = append(a, "--")
	return append(a, paths...)
}

// diffEntries lists every changed path with its line counts. It is
// all-or-error: an incomplete list is never used to decide what to show.
func (r *Repo) diffEntries(ctx context.Context, p *diffPlan, paths []string) ([]diffEntry, error) {
	out, err := r.git(ctx, diffListCap, p.args([]string{"--name-status", "-z"}, paths)...)
	if err != nil {
		return nil, err
	}
	malformed := errors.New("unexpected output from git diff; refusing to continue")
	toks := splitNUL(out)
	var entries []diffEntry
	// A path with unresolved merge conflicts has several versions in the
	// index. When the index is one side of the comparison git reports it as
	// "U", and reports it again as "M" when a working-tree file exists. It is
	// one changed path: the unmerged state is kept, the duplicate dropped.
	at := map[string]int{}
	for i := 0; i < len(toks); {
		st := toks[i]
		i++
		if st == "" {
			return nil, malformed
		}
		e := diffEntry{Status: st[0]}
		if st[0] == 'R' || st[0] == 'C' {
			if i+1 >= len(toks) {
				return nil, malformed
			}
			e.Orig, e.Path = toks[i], toks[i+1]
			i += 2
		} else {
			if i >= len(toks) {
				return nil, malformed
			}
			e.Path = toks[i]
			i++
		}
		if j, dup := at[e.Path]; dup {
			if e.Status == 'U' {
				entries[j] = e
			} else if entries[j].Status != 'U' {
				return nil, malformed
			}
			continue
		}
		at[e.Path] = len(entries)
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return nil, nil
	}
	// These are paths out of history: most were never in the working-tree
	// list, so nothing has judged them yet. Every one is checked against the
	// policy by whoever uses the list; have git judge them all in one go.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Path, e.Orig)
	}
	r.primePolicy(ctx, names)

	out, err = r.git(ctx, diffListCap, p.args([]string{"--numstat", "-z"}, paths)...)
	if err != nil {
		return nil, err
	}
	stats := map[string][3]int{} // path → add, del, binary
	toks = splitNUL(out)
	for i := 0; i < len(toks); {
		parts := strings.SplitN(toks[i], "\t", 3)
		if len(parts) != 3 {
			return nil, malformed
		}
		path := parts[2]
		if path == "" { // rename/copy: two more records follow
			if i+2 >= len(toks) {
				return nil, malformed
			}
			path = toks[i+2]
			i += 3
		} else {
			i++
		}
		var s [3]int
		if parts[0] == "-" {
			s[2] = 1
		} else {
			s[0], _ = strconv.Atoi(parts[0])
			s[1], _ = strconv.Atoi(parts[1])
		}
		stats[path] = s
	}
	for i := range entries {
		// An unmerged path has no single old version to count lines against.
		if s, ok := stats[entries[i].Path]; ok && entries[i].Status != 'U' {
			entries[i].Add, entries[i].Del, entries[i].Binary, entries[i].HasStat = s[0], s[1], s[2] == 1, true
		}
	}
	return entries, nil
}

func splitNUL(b []byte) []string {
	b = bytes.TrimSuffix(b, []byte{0})
	if len(b) == 0 {
		return nil
	}
	parts := bytes.Split(b, []byte{0})
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = string(p)
	}
	return out
}

func (r *Repo) renderDiff(ctx context.Context, plan *diffPlan, o DiffOptions, budget int, res *DiffResult) (string, error) {
	pol, err := r.Policy(ctx)
	if err != nil {
		return "", err
	}
	if len(o.Paths) > maxDiffPathArgs {
		return "", fmt.Errorf("at most %d paths per call", maxDiffPathArgs)
	}
	var paths []string
	for _, p := range o.Paths {
		rel, err := r.gitPath(ctx, p)
		if err != nil {
			return "", err
		}
		paths = append(paths, rel)
	}
	entries, err := r.diffEntries(ctx, plan, paths)
	if err != nil {
		return "", err
	}

	withheldPath := func(e diffEntry) bool {
		return pol.Check(e.Path) != "" || (e.Orig != "" && pol.Check(e.Orig) != "")
	}
	var allowed []diffEntry
	var withheld []diffEntry
	for _, e := range entries {
		if withheldPath(e) {
			withheld = append(withheld, e)
		} else {
			allowed = append(allowed, e)
		}
	}
	res.Files, res.Withheld = len(allowed), len(withheld)

	var b strings.Builder
	add, del := 0, 0
	for _, e := range allowed {
		add, del = add+e.Add, del+e.Del
	}
	fmt.Fprintf(&b, "%d file(s) changed, +%d −%d", len(allowed), add, del)
	if len(withheld) > 0 {
		fmt.Fprintf(&b, " · plus %d changed path(s) withheld by policy (content never shown)", len(withheld))
	}
	b.WriteString("\n")
	if len(entries) == 0 {
		b.WriteString("[no differences]\n")
		return b.String(), nil
	}
	for i, e := range allowed {
		if i == maxDiffListed {
			fmt.Fprintf(&b, "  … %d more changed file(s) not listed; narrow with paths=[…]\n", len(allowed)-i)
			res.Incomplete = true
			break
		}
		name := SanitizeLine(e.Path)
		if e.Orig != "" {
			name = SanitizeLine(e.Orig) + " → " + name
		}
		switch {
		case e.Status == 'U':
			fmt.Fprintf(&b, "  %c  %s  (unresolved merge conflict)\n", e.Status, name)
		case e.Binary:
			fmt.Fprintf(&b, "  %c  %s  (binary)\n", e.Status, name)
		default:
			fmt.Fprintf(&b, "  %c  %s  +%d −%d\n", e.Status, name, e.Add, e.Del)
		}
	}
	if r.Opts.ShowWithheldNames {
		for _, e := range withheld {
			fmt.Fprintf(&b, "  %c  %s  [withheld]\n", e.Status, SanitizeLine(e.Path))
		}
	}
	if o.StatOnly || len(allowed) == 0 {
		return b.String(), nil
	}

	// A literal pathspec for file P also selects everything under P/. When a
	// change set contains both (file ↔ directory type change), P's own patch
	// is skipped so a pathspec can never pull in another entry's content.
	hasChildren := func(p string) bool {
		prefix := p + "/"
		for _, e := range entries {
			if strings.HasPrefix(e.Path, prefix) || (e.Orig != "" && strings.HasPrefix(e.Orig, prefix)) {
				return true
			}
		}
		return false
	}

	context := min(max(o.Context, 0), MaxDiffContext)
	if !o.hasContext() {
		context = 3
	}
	perFile := max(minPerFilePatch, budget/2)
	var skippedType, unmerged []string
	for i, e := range allowed {
		if e.Status == 'U' {
			// git would print a combined ("diff --cc") patch against the
			// versions in the index. Its hunks have another shape than the
			// masking of key material follows, so it is never shown; the
			// conflict can be read as an ordinary patch against HEAD.
			unmerged = append(unmerged, e.Path)
			res.Incomplete = true
			continue
		}
		if i >= maxDiffPatches || b.Len() >= budget {
			res.Omitted = append(res.Omitted, e.Path)
			continue
		}
		if e.Binary {
			continue
		}
		if hasChildren(e.Path) || (e.Orig != "" && hasChildren(e.Orig)) {
			skippedType = append(skippedType, e.Path)
			continue
		}
		specs := []string{e.Path}
		if e.Orig != "" {
			specs = []string{e.Orig, e.Path}
		}
		room := min(perFile, budget-b.Len())
		if room < 512 {
			res.Omitted = append(res.Omitted, e.Path)
			continue
		}
		out, truncated, code, err := r.gitRaw(ctx, room, plan.args([]string{"-p", "-U" + strconv.Itoa(context)}, specs)...)
		if err != nil {
			return "", err
		}
		if code != 0 {
			return "", r.gitFailure("diff", out)
		}
		patch := string(out)
		if truncated {
			if cut := strings.LastIndexByte(patch, '\n'); cut > 0 {
				patch = patch[:cut]
			}
		}
		masked, n, ok := r.maskPatch(ctx, plan, e, patch)
		if !ok {
			fmt.Fprintf(&b, "\n── %s ──\n[patch omitted: it may contain private-key material and the full file versions could not be examined]\n", SanitizeLine(e.Path))
			res.Incomplete = true
			continue
		}
		res.Redactions += n
		res.Shown++
		b.WriteString("\n")
		b.WriteString(Sanitize(strings.TrimRight(masked, "\n")))
		b.WriteString("\n")
		if truncated {
			fmt.Fprintf(&b, "[patch for %s cut at %s; read the file or request it alone with paths=[…] and a smaller context]\n",
				SanitizeLine(e.Path), humanSize(int64(room)))
			res.Cut = append(res.Cut, e.Path)
		}
	}
	if len(skippedType) > 0 {
		fmt.Fprintf(&b, "\n[patch not shown for %s: it changed between a file and a directory; see the entries beneath it]\n", SanitizeLine(strings.Join(skippedType, ", ")))
	}
	if len(unmerged) > 0 {
		list, more := unmerged, ""
		if len(list) > 30 {
			more = fmt.Sprintf(" and %d more", len(list)-30)
			list = list[:30]
		}
		fmt.Fprintf(&b, "\n[no patch for %s%s: unresolved merge conflict, so the index holds several versions. Use git_diff with mode=head to see the file against HEAD, or read_file to see the conflict markers]\n",
			SanitizeLine(strings.Join(list, ", ")), more)
	}
	if len(res.Omitted) > 0 {
		list := res.Omitted
		more := ""
		if len(list) > 30 {
			more = fmt.Sprintf(" and %d more", len(list)-30)
			list = list[:30]
		}
		fmt.Fprintf(&b, "\n[size budget reached — patches NOT shown for: %s%s. Call git_diff again with paths=[…] for those]\n",
			SanitizeLine(strings.Join(list, ", ")), more)
	}
	if res.Redactions > 0 {
		fmt.Fprintf(&b, "[note: %d secret-looking value(s) masked as [REDACTED:…]]\n", res.Redactions)
	}
	return b.String(), nil
}

// hasContext reports whether the caller set a context size (0 is a valid
// explicit value, so "unset" is encoded as a negative number).
func (o DiffOptions) hasContext() bool { return o.Context >= 0 }

var (
	hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)
	base64Only = regexp.MustCompile(`^["'` + "`" + `]?[A-Za-z0-9+/=]{4,}(?:\\n|\\r)*["'` + "`" + `]?\s*[+,]?$`)
	// conflictMarker matches the lines git writes around the versions of a
	// conflicted region (including the "|||||||" of the diff3 style).
	conflictMarker = regexp.MustCompile(`^(?:<{7}|={7}|>{7}|\|{7})(?: .*)?$`)
)

// keyish reports whether a patch line, or the context text git appends to a
// hunk header, consists only of base64 characters.
func keyish(l string) bool {
	switch {
	case l == "":
		return false
	case l[0] == '+' || l[0] == '-' || l[0] == ' ':
		l = l[1:]
	case strings.HasPrefix(l, "@@"):
		m := hunkHeader.FindString(l)
		if m == "" {
			return false
		}
		l = l[len(m):]
	default:
		return false
	}
	return base64Only.MatchString(strings.TrimSpace(l))
}

// maskPatch masks secrets in one file's patch. Private-key blocks are located
// in the complete old and new versions of the file — not in the excerpt — and
// projected onto the hunk lines, so a hunk that shows only the middle of a
// key is masked too. ok=false means the sources could not be examined and the
// patch must not be shown.
func (r *Repo) maskPatch(ctx context.Context, plan *diffPlan, e diffEntry, patch string) (string, int, bool) {
	if !r.red.Enabled() {
		return patch, 0, true
	}
	lines := strings.Split(patch, "\n")
	// Locating key blocks needs both complete file versions, so it is only
	// done when the patch shows something that could be key material: a
	// marker, or a line (or hunk-header context) made only of base64 text.
	needSpans := false
	for _, l := range lines {
		if strings.Contains(l, "PRIVATE KEY") || keyish(l) {
			needSpans = true
			break
		}
	}
	projected := 0
	if needSpans {
		oldPath := e.Path
		if e.Orig != "" {
			oldPath = e.Orig
		}
		oldText, ok1 := r.sideContent(ctx, plan.old, oldPath)
		newText, ok2 := r.sideContent(ctx, plan.new, e.Path)
		if !ok1 || !ok2 {
			return "", 0, false
		}
		oldSpans, newSpans := pemLineSet(oldText), pemLineSet(newText)
		if len(oldSpans)+len(newSpans) > 0 {
			oldNo, newNo, inHunk := 0, 0, false
			for i, l := range lines {
				if m := hunkHeader.FindStringSubmatch(l); m != nil {
					oldNo, _ = strconv.Atoi(m[1])
					newNo, _ = strconv.Atoi(m[2])
					inHunk = true
					// git appends a nearby earlier line as "function context"
					// after the second @@; in a file with key material that
					// line can be part of the key, so it is dropped.
					if len(l) > len(m[0]) {
						lines[i] = m[0]
						projected++
					}
					continue
				}
				if !inHunk || l == "" {
					continue
				}
				switch l[0] {
				case ' ':
					if oldSpans[oldNo] || newSpans[newNo] {
						lines[i] = " " + pemPlaceholder
						projected++
					}
					oldNo++
					newNo++
				case '-':
					if oldSpans[oldNo] {
						lines[i] = "-" + pemPlaceholder
						projected++
					}
					oldNo++
				case '+':
					if newSpans[newNo] {
						lines[i] = "+" + pemPlaceholder
						projected++
					}
					newNo++
				case '\\':
				default:
					inHunk = false // next file header
				}
			}
		}
	}
	masked, n := r.red.Text(strings.Join(lines, "\n"), true)
	if projected > 0 {
		n++
	}
	return masked, n, true
}

// pemLineSet returns the 1-based numbers of the lines that belong to a
// multi-line private-key block in text.
func pemLineSet(text string) map[int]bool {
	set := map[int]bool{}
	if !strings.Contains(text, "PRIVATE KEY") {
		return set
	}
	in := false
	for i, l := range strings.Split(text, "\n") {
		l = strings.TrimSuffix(l, "\r")
		if in {
			if pemEnd.MatchString(l) {
				set[i+1] = true
				in = false
				continue
			}
			if pemBodyLine.MatchString(l) {
				set[i+1] = true
				continue
			}
			// A merge conflict inside a key leaves git's markers between
			// two versions of its lines. Both versions are key material, so
			// a marker does not end the block.
			if conflictMarker.MatchString(l) {
				continue
			}
			in = false
		}
		if loc := pemBegin.FindStringIndex(l); loc != nil {
			rest := l[loc[1]:]
			if pemEnd.MatchString(rest) {
				continue // a one-line block: handled by the line rules
			}
			if pemBeginTail.MatchString(rest) {
				set[i+1] = true
				in = true
			}
		}
	}
	return set
}

// sideContent returns the full content of path on one side of a comparison.
// A path that does not exist on that side is empty content, not a failure.
func (r *Repo) sideContent(ctx context.Context, side diffSide, rel string) (string, bool) {
	switch side.kind {
	case sideNone:
		return "", true
	case sideTree, sideIndex:
		object := side.oid + ":" + rel // ":path" names the index entry
		kind, code, err := r.gitText(ctx, "cat-file", "-t", object)
		if err != nil {
			return "", false
		}
		if code != 0 || kind != "blob" {
			return "", true
		}
		sizeText, code, err := r.gitText(ctx, "cat-file", "-s", object)
		if err != nil || code != 0 {
			return "", false
		}
		if size, _ := strconv.ParseInt(sizeText, 10, 64); size > maxRevBlobBytes {
			return "", false
		}
		data, err := r.git(ctx, maxRevBlobBytes+1, "cat-file", "blob", object)
		if err != nil {
			return "", false
		}
		return string(data), true
	case sideWorktree:
		op := r.newOpener()
		defer op.Close()
		data, _, truncated, err := op.ReadAll(rel, maxRevBlobBytes)
		switch {
		case err == nil && !truncated:
			return string(data), true
		case errors.Is(err, os.ErrNotExist), errors.Is(err, errSymlink), errors.Is(err, errNotRegular):
			return "", true
		default:
			return "", false
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Show
// ---------------------------------------------------------------------------

// ShowResult is one rendered commit.
type ShowResult struct {
	Text string
	OID  string
	Diff *DiffResult
}

// Show renders a commit: metadata, message and its changes relative to its
// first parent (or to the empty tree for a root commit).
func (r *Repo) Show(ctx context.Context, rev string, paths []string, statOnly bool, context, budget int) (*ShowResult, error) {
	oid, err := r.ResolveCommit(ctx, rev)
	if err != nil {
		return nil, err
	}
	out, err := r.git(ctx, 1<<20, "log", "-1", "--no-walk", "--date=iso-strict",
		"--format=%H%x00%P%x00%an%x00%ad%x00%cn%x00%cd%x00%D%x00%B", oid)
	if err != nil {
		return nil, err
	}
	f := strings.SplitN(string(out), "\x00", 8)
	if len(f) != 8 {
		return nil, errors.New("unexpected output from git log")
	}
	parents := strings.Fields(f[1])
	message, redactions := r.red.Text(strings.TrimRight(f[7], "\n"), false)

	var b strings.Builder
	fmt.Fprintf(&b, "COMMIT %s\n", oid)
	if f[6] != "" {
		fmt.Fprintf(&b, "refs:      %s\n", SanitizeLine(f[6]))
	}
	fmt.Fprintf(&b, "author:    %s · %s\n", SanitizeLine(f[2]), f[3])
	if f[4] != f[2] || f[5] != f[3] {
		fmt.Fprintf(&b, "committer: %s · %s\n", SanitizeLine(f[4]), f[5])
	}
	switch len(parents) {
	case 0:
		b.WriteString("parents:   none (root commit; shown against the empty tree)\n")
	case 1:
		fmt.Fprintf(&b, "parent:    %s\n", short(parents[0]))
	default:
		fmt.Fprintf(&b, "parents:   %s (merge commit; changes are shown against the FIRST parent %s)\n",
			strings.Join(shortAll(parents), ", "), short(parents[0]))
	}
	b.WriteString("\n")
	for _, l := range strings.Split(Sanitize(message), "\n") {
		b.WriteString("    " + l + "\n")
	}
	b.WriteString("\n")

	base := ""
	if len(parents) > 0 {
		base = parents[0]
	} else if base, err = r.emptyTreeOID(ctx); err != nil {
		return nil, err
	}
	plan := r.treePlan(base, oid, "")
	res := &DiffResult{BaseOID: base, TargetOID: oid, Redactions: redactions}
	body, err := r.renderDiff(ctx, plan, DiffOptions{Paths: paths, StatOnly: statOnly, Context: context}, budget-b.Len()-diffHeaderMargin, res)
	if err != nil {
		return nil, err
	}
	b.WriteString(body)
	return &ShowResult{Text: b.String(), OID: oid, Diff: res}, nil
}
