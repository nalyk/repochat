package repo

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/format"
	"go/parser"
	"go/scanner"
	"go/token"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A change set is a list of edit operations a model sends, addressed by the
// anchors it was shown. It is validated completely, in memory, before
// anything is written: the result is a Plan holding the whole new content of
// every file it touches. Every check refuses the whole change set.

const (
	maxChangeOps   = 500
	maxChangeFiles = 100
	maxChangeText  = 1 << 20 // bytes of new text in one change set
	maxPlannedFile = 2 << 20 // bytes of one resulting file
	diffContext    = 3
)

// Change is one operation of a change set.
type Change struct {
	Op      string `json:"op"`       // replace | insert | delete | create | remove | move
	Path    string `json:"path"`     //
	From    string `json:"from"`     // anchor "<line>:<hash>": first line of a range (replace, delete)
	To      string `json:"to"`       // anchor: last line of the range; empty = same as From
	At      string `json:"at"`       // insert: an anchor, or "start", or "end"
	Where   string `json:"where"`    // insert at an anchor: "before" | "after" (default "after")
	Text    string `json:"text"`     // new lines, "\n"-separated; one trailing "\n" is ignored
	ToPath  string `json:"to_path"`  // move
	FileTag string `json:"file_tag"` // remove: the FileTag the model was shown
}

// field returns the member of c that the JSON name denotes, nil for none.
func (c *Change) field(name string) *string {
	switch name {
	case "op":
		return &c.Op
	case "path":
		return &c.Path
	case "from":
		return &c.From
	case "to":
		return &c.To
	case "at":
		return &c.At
	case "where":
		return &c.Where
	case "text":
		return &c.Text
	case "to_path":
		return &c.ToPath
	case "file_tag":
		return &c.FileTag
	}
	return nil
}

// ParseChanges reads a change set: a JSON array of operations whose members
// are strings. A member that is unknown, spelled in another letter case,
// repeated, or not a string is an error, as is anything after the array.
func ParseChanges(raw json.RawMessage) ([]Change, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	notArray := errors.New("the change set must be a JSON array of operations")
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('[') {
		return nil, notArray
	}
	var out []Change
	for dec.More() {
		if len(out) >= maxChangeOps {
			return nil, fmt.Errorf("a change set may hold at most %d operations", maxChangeOps)
		}
		c, err := parseChange(dec, len(out)+1)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim(']') {
		return nil, notArray
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("the change set has data after its closing bracket")
	}
	if len(out) == 0 {
		return nil, errors.New("the change set is empty")
	}
	for i, c := range out {
		if err := checkShape(c); err != nil {
			return nil, fmt.Errorf("operation %d: %w", i+1, err)
		}
	}
	return out, nil
}

func parseChange(dec *json.Decoder, n int) (Change, error) {
	var c Change
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return c, fmt.Errorf("operation %d is not a JSON object", n)
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		name, ok := tok.(string)
		if err != nil || !ok {
			return c, fmt.Errorf("operation %d is not a valid JSON object", n)
		}
		f := c.field(name)
		if f == nil {
			if len(name) > 40 {
				name = name[:40] + "…"
			}
			return c, fmt.Errorf("operation %d has an unknown member %q (members: op, path, from, to, at, where, text, to_path, file_tag)", n, SanitizeLine(name))
		}
		if seen[name] {
			return c, fmt.Errorf("operation %d gives %q twice", n, name)
		}
		seen[name] = true
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return c, fmt.Errorf("operation %d is not valid JSON", n)
		}
		if len(v) == 0 || v[0] != '"' || json.Unmarshal(v, f) != nil {
			return c, fmt.Errorf("operation %d: %q must be a string", n, name)
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return c, fmt.Errorf("operation %d is not a valid JSON object", n)
	}
	return c, nil
}

var (
	anchorRe   = regexp.MustCompile(`^([1-9][0-9]{0,8}):([a-z2-7]{3})$`)
	cutAnchor  = regexp.MustCompile(`^[0-9]+:\?\?\?$`)
	fileTagRe  = regexp.MustCompile(`^[a-z2-7]{8}$`)
	changeArgs = []string{"from", "to", "at", "where", "text", "to_path", "file_tag"}
	// What each operation takes; every other member must be empty.
	opTakes = map[string]map[string]bool{
		"replace": set("from", "to", "text"),
		"delete":  set("from", "to"),
		"insert":  set("at", "where", "text"),
		"create":  set("text"),
		"remove":  set("file_tag"),
		"move":    set("to_path"),
	}
)

// parseAnchor splits an anchor that checkShape accepted.
func parseAnchor(a string) (line int, hash string) {
	m := anchorRe.FindStringSubmatch(a)
	if m == nil {
		return 0, ""
	}
	line, _ = strconv.Atoi(m[1])
	return line, m[2]
}

func checkAnchor(name, a string) error {
	if anchorRe.MatchString(a) {
		return nil
	}
	if cutAnchor.MatchString(a) {
		return fmt.Errorf("%s %q names a line that was cut on reading; such a line has no anchor and cannot be addressed", name, SanitizeLine(a))
	}
	if len(a) > 40 {
		a = a[:40] + "…"
	}
	return fmt.Errorf("%s %q is not an anchor of the form <line>:<hash> as read_file shows it (e.g. 12:abc)", name, SanitizeLine(a))
}

// checkShape checks one operation on its own: a known op, the members it
// needs, none it does not take, and anchors of the right form.
func checkShape(c Change) error {
	takes, ok := opTakes[c.Op]
	if !ok {
		return fmt.Errorf("unknown op %q (replace, insert, delete, create, remove or move)", SanitizeLine(c.Op))
	}
	if c.Path == "" {
		return fmt.Errorf("%s needs a path", c.Op)
	}
	for _, name := range changeArgs {
		if *c.field(name) != "" && !takes[name] {
			return fmt.Errorf("%s does not take %q", c.Op, name)
		}
	}
	switch c.Op {
	case "replace", "delete":
		if c.From == "" {
			return fmt.Errorf("%s needs from (and to, for more than one line)", c.Op)
		}
		if err := checkAnchor("from", c.From); err != nil {
			return err
		}
		if c.To != "" {
			if err := checkAnchor("to", c.To); err != nil {
				return err
			}
		}
		if c.Op == "replace" && c.Text == "" {
			return errors.New("replace needs text; to remove lines use delete")
		}
	case "insert":
		switch c.At {
		case "":
			return errors.New(`insert needs at: an anchor, "start" or "end"`)
		case "start", "end":
			if c.Where != "" {
				return fmt.Errorf("insert at %s does not take where", c.At)
			}
		default:
			if err := checkAnchor("at", c.At); err != nil {
				return err
			}
			if c.Where != "" && c.Where != "before" && c.Where != "after" {
				return fmt.Errorf(`where must be "before" or "after", not %q`, SanitizeLine(c.Where))
			}
		}
		if c.Text == "" {
			return errors.New("insert needs text")
		}
	case "remove":
		if !fileTagRe.MatchString(c.FileTag) {
			return errors.New("remove needs the file_tag that read_file showed for the file")
		}
	case "move":
		if c.ToPath == "" {
			return errors.New("move needs to_path")
		}
	}
	if strings.Contains(strings.ReplaceAll(c.Text, "\r\n", ""), "\r") {
		return errors.New("text holds a carriage return that does not end a line")
	}
	return nil
}

// splitText turns new text into lines: "" is no lines; CRLF counts as one
// line break; one trailing line break is ignored.
func splitText(text string) []string {
	if text == "" {
		return nil
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

// ChangeError is the refusal of a whole change set.
type ChangeError struct {
	Path, Reason string
	Fresh        string // for a stale anchor: the current lines around it, with fresh anchors
}

func (e *ChangeError) Error() string {
	msg := e.Reason
	if e.Path != "" {
		msg = e.Path + ": " + msg
	}
	msg += " (the whole change set was refused; nothing was changed)"
	if e.Fresh != "" {
		msg += "\nCurrent lines, with fresh anchors:\n" + e.Fresh
	}
	return msg
}

func refusal(path, format string, a ...any) *ChangeError {
	return &ChangeError{Path: SanitizeLine(path), Reason: fmt.Sprintf(format, a...)}
}

// Plan is a validated change set: the complete new content of every file it
// touches. Nothing has been written.
type Plan struct {
	ID       string        // 16 random hex characters
	Files    []PlannedFile // sorted by path
	Diff     string        // unified-style, built from the operations, old lines as shown (masked)
	Warnings []string
	Added    int // line counts
	Removed  int
}

// PlannedFile is one touched path. A move is two: the old path (After nil)
// and the new one (Before nil).
type PlannedFile struct {
	Path   string
	Before []byte // nil when the file did not exist
	After  []byte // nil when the file is removed
	// Regions are the changed lines of After as [start, end): 0-based line
	// indexes, end exclusive; start == end marks a place where lines were
	// only deleted. Adjacent regions are merged.
	Regions [][2]int
}

// Propose validates a change set against the open session's workspace and
// returns its plan. It writes nothing.
func (s *Sessions) Propose(ctx context.Context, changes []Change) (*Plan, error) {
	release, err := s.acquire()
	if err != nil {
		return nil, err
	}
	defer release()
	rec, err := s.ensure(ctx)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, ErrNoSession
	}
	return s.ws.planChanges(ctx, changes)
}

// changeWork collects what the change set does, path by path.
type changeWork struct {
	changes []Change
	edits   map[string][]int // path → indexes of its replace/insert/delete
	whole   map[string]int   // path → index of its create, remove or move
	targets map[string]int   // move target → index of the move
	paths   []string         // every touched path, sorted
}

// plainPath accepts a path only as CheckPath would leave it: a write must
// name exactly the file it means.
func plainPath(p string) (string, error) {
	clean, err := CheckPath(p)
	if err != nil {
		return "", refusal(p, "%v", err)
	}
	if clean != p || !exactPath(p) {
		return "", refusal(p, "write the path exactly as listed, without \"./\", blanks at either end, backslashes or a trailing slash (did you mean %q?)", clean)
	}
	return p, nil
}

// gather checks the set as a whole and sorts its operations by path.
func gather(changes []Change) (*changeWork, error) {
	if len(changes) == 0 {
		return nil, refusal("", "the change set is empty")
	}
	if len(changes) > maxChangeOps {
		return nil, refusal("", "a change set may hold at most %d operations; this one has %d", maxChangeOps, len(changes))
	}
	text := 0
	for i, c := range changes {
		if err := checkShape(c); err != nil {
			return nil, refusal(c.Path, "operation %d: %v", i+1, err)
		}
		text += len(c.Text)
	}
	if text > maxChangeText {
		return nil, refusal("", "the change set carries %d bytes of new text; at most 1 MiB (%d bytes) is allowed", text, maxChangeText)
	}
	w := &changeWork{changes: changes, edits: map[string][]int{}, whole: map[string]int{}, targets: map[string]int{}}
	for i, c := range changes {
		p, err := plainPath(c.Path)
		if err != nil {
			return nil, err
		}
		switch c.Op {
		case "replace", "insert", "delete":
			w.edits[p] = append(w.edits[p], i)
			continue
		}
		if j, dup := w.whole[p]; dup {
			return nil, refusal(p, "operation %d %ss it and operation %d %ss it; a path may be created, removed or moved by one operation only", j+1, changes[j].Op, i+1, c.Op)
		}
		w.whole[p] = i
		if c.Op == "move" {
			to, err := plainPath(c.ToPath)
			if err != nil {
				return nil, err
			}
			if j, dup := w.targets[to]; dup {
				return nil, refusal(to, "operations %d and %d both move a file there", j+1, i+1)
			}
			w.targets[to] = i
		}
	}
	for p, i := range w.whole {
		if j, ok := w.targets[p]; ok {
			return nil, refusal(p, "operation %d moves a file onto a path that operation %d %ss", j+1, i+1, changes[i].Op)
		}
	}
	for p, idx := range w.edits {
		if i, ok := w.whole[p]; ok && changes[i].Op != "move" {
			return nil, refusal(p, "operation %d %ss it, so operation %d cannot edit its lines", i+1, changes[i].Op, idx[0]+1)
		}
		if i, ok := w.targets[p]; ok {
			return nil, refusal(p, "operation %d edits the lines of a path that operation %d moves a file onto; anchors refer to the files before the change set", idx[0]+1, i+1)
		}
	}
	seen := map[string]bool{}
	for _, m := range []map[string]int{w.whole, w.targets} {
		for p := range m {
			seen[p] = true
		}
	}
	for p := range w.edits {
		seen[p] = true
	}
	for p := range seen {
		w.paths = append(w.paths, p)
	}
	sort.Strings(w.paths)
	if len(w.paths) > maxChangeFiles {
		return nil, refusal("", "a change set may touch at most %d files; this one touches %d", maxChangeFiles, len(w.paths))
	}
	return w, nil
}

// isNew reports whether the change set brings p into existence.
func (w *changeWork) isNew(p string) bool {
	if _, ok := w.targets[p]; ok {
		return true
	}
	i, ok := w.whole[p]
	return ok && w.changes[i].Op == "create"
}

// dirsOf returns the directory prefixes of p, shortest first.
func dirsOf(p string) []string {
	var out []string
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			out = append(out, p[:i])
		}
	}
	return out
}

// checkPaths applies every check that depends on the paths alone: protected
// paths, the policy, what git lists there, letter case, what is on disk, and
// filter attributes.
func (r *Repo) checkPaths(ctx context.Context, w *changeWork, snap *Snapshot, op *opener) error {
	for _, p := range w.paths {
		if protectedPath(p) {
			return refusal(p, "is a protected path (git's own files, continuous-integration definitions, hook directories, .gitmodules, .gitattributes, .repochatignore); the owner has to change it by hand")
		}
	}
	if err := snap.Policy.Prime(ctx, w.paths); err != nil {
		return refusal("", "the repository owner's policy could not be evaluated for these paths (%s: %v)", ReasonUncertain, err)
	}
	for _, p := range w.paths {
		if reason := snap.Policy.Check(p); reason != "" {
			return refusal(p, "is withheld by the repository owner's policy (%s); a change set may neither change nor create such a path", reason)
		}
	}

	// Every spelling git lists, for each case-folded path and directory.
	folds := map[string]map[string]bool{}
	note := func(p string) {
		k := foldName(p)
		if folds[k] == nil {
			folds[k] = map[string]bool{}
		}
		folds[k][p] = true
	}
	for _, e := range snap.Entries {
		note(e.Path)
		for _, d := range dirsOf(e.Path) {
			note(d)
		}
	}
	newFold := map[string]string{} // case-folded new path or directory → its spelling
	newFiles := map[string]bool{}
	for _, p := range w.paths {
		for _, d := range dirsOf(p) {
			e, ok := snap.Lookup(d)
			if ok && !e.Tracked {
				continue // git knows nothing of its kind: what is on disk decides below
			}
			switch {
			case ok && e.Kind == KindSymlink:
				return refusal(p, "passes through %s, a symbolic link; symbolic links are never followed", d)
			case ok && e.Kind == KindSubmodule:
				return refusal(p, "lies inside %s, a submodule; its files are not part of this repository", d)
			case ok:
				return refusal(p, "%s is a file, not a directory", d)
			}
		}
		if !w.isNew(p) {
			e, ok := snap.Lookup(p)
			switch {
			case !ok:
				return refusal(p, "is not a file in the session workspace (not tracked, or ignored by .gitignore); use find_files to locate it")
			case e.Kind == KindSymlink:
				return refusal(p, "is a symbolic link; a change set cannot change, move or remove one")
			case e.Kind == KindSubmodule:
				return refusal(p, "is a submodule; a change set cannot change, move or remove one")
			case e.Conflict:
				return refusal(p, "has unresolved merge conflicts")
			}
			continue
		}
		if _, ok := snap.Lookup(p); ok {
			return refusal(p, "already exists")
		}
		k := foldName(p)
		if spellings := folds[k]; len(spellings) > 0 {
			if spellings[p] {
				return refusal(p, "already exists (as a directory)")
			}
			return refusal(p, "differs only in letter case from an existing path; on a case-insensitive file system they are the same file")
		}
		for _, d := range dirsOf(p) {
			if spellings := folds[foldName(d)]; len(spellings) > 0 && !spellings[d] {
				return refusal(p, "its directory %s differs only in letter case from an existing one", d)
			}
		}
		for _, q := range append(dirsOf(p), p) {
			kq := foldName(q)
			if other, ok := newFold[kq]; ok && other != q {
				return refusal(p, "differs only in letter case from %s, which the change set also creates", other)
			}
			newFold[kq] = q
		}
		newFiles[p] = true
	}
	for p := range newFiles {
		for _, d := range dirsOf(p) {
			if newFiles[d] {
				return refusal(p, "the change set creates %s both as a file and as a directory", d)
			}
		}
	}

	// What is on disk now: no element may be a symbolic link, a directory
	// holding another repository, or anything but a directory on the way.
	for _, p := range w.paths {
		for _, q := range append(dirsOf(p), p) {
			fi, err := op.Lstat(q)
			if errors.Is(err, os.ErrNotExist) {
				if !w.isNew(p) {
					return refusal(p, "is not present in the session workspace")
				}
				break
			}
			if err != nil {
				return refusal(p, "cannot be checked: %v", describeErr(q, err))
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				return refusal(p, "%s is a symbolic link; symbolic links are never followed", q)
			}
			if q != p {
				if !fi.IsDir() {
					return refusal(p, "%s is not a directory", q)
				}
				if _, err := op.Lstat(q + "/.git"); !errors.Is(err, os.ErrNotExist) {
					return refusal(p, "%s holds another repository (a submodule or a nested checkout)", q)
				}
				continue
			}
			if w.isNew(p) {
				return refusal(p, "already exists in the session workspace")
			}
			if !fi.Mode().IsRegular() {
				return refusal(p, "is not a regular file")
			}
		}
	}
	return r.checkFilters(ctx, w.paths)
}

// checkFilters asks git, in one batch, for the filter attribute of every
// path; anything but "unspecified" is refused. Such files are kept as git
// stores them, since no filter program is ever run, so an edit would reach
// the owner's checkout in the wrong form.
func (r *Repo) checkFilters(ctx context.Context, paths []string) error {
	var in bytes.Buffer
	for _, p := range paths {
		in.WriteString(p)
		in.WriteByte(0)
	}
	out, err := r.gitInput(ctx, in.Bytes(), 4<<20, "check-attr", "-z", "--stdin", "filter")
	if err != nil {
		return refusal("", "cannot tell whether the paths have a filter attribute (%v)", err)
	}
	fields := strings.Split(string(out), "\x00")
	if len(fields) != 3*len(paths)+1 || fields[len(fields)-1] != "" {
		return refusal("", "git answered about the filter attribute of %d path(s) when asked about %d", (len(fields)-1)/3, len(paths))
	}
	for i, p := range paths {
		path, attr, value := fields[3*i], fields[3*i+1], fields[3*i+2]
		if path != p || attr != "filter" {
			return refusal("", "git answered about a different path or attribute than it was asked about")
		}
		if value != "unspecified" {
			return refusal(p, "has a filter attribute (filter: %s), as Git LFS or git-crypt files do; the workspace holds such a file as git stores it, so a change set cannot change, create, move or remove it", SanitizeLine(value))
		}
	}
	return nil
}

// planChanges validates changes against this workspace and builds the plan.
func (r *Repo) planChanges(ctx context.Context, changes []Change) (*Plan, error) {
	key := r.Opts.AnchorKey
	if len(key) == 0 {
		return nil, errors.New("change sets need a session workspace")
	}
	w, err := gather(changes)
	if err != nil {
		return nil, err
	}
	snap, err := r.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	op := r.newOpener()
	defer op.Close()
	if err := r.checkPaths(ctx, w, snap, op); err != nil {
		return nil, err
	}

	// Read every existing file once.
	srcs := map[string]*source{}
	for _, p := range w.paths {
		if w.isNew(p) {
			continue
		}
		needText := len(w.edits[p]) > 0
		if i, ok := w.whole[p]; ok && changes[i].Op == "remove" {
			needText = true
		}
		s, err := r.readSource(ctx, op, p, needText)
		if err != nil {
			return nil, err
		}
		srcs[p] = s
	}

	// Anchors first, all of them: a stale view is reported with the current
	// lines around every stale anchor.
	var stale []staleAnchor
	for _, p := range w.paths {
		for _, i := range w.edits[p] {
			c := changes[i]
			for _, a := range []string{c.From, c.To, c.At} {
				if a == "" || a == "start" || a == "end" {
					continue
				}
				n, hash := parseAnchor(a)
				lines := srcs[p].shown
				if n > len(lines) || lineAnchor(key, lines[n-1]) != hash {
					stale = append(stale, staleAnchor{path: p, anchor: a, line: n})
				}
			}
		}
	}
	if len(stale) > 0 {
		return nil, r.staleError(key, stale, srcs)
	}

	plan := &Plan{}
	var sections []diffSection
	add := func(f PlannedFile) { plan.Files = append(plan.Files, f) }
	for _, p := range w.paths {
		src := srcs[p]
		i, isWhole := w.whole[p]
		var c Change
		if isWhole {
			c = changes[i]
		}
		switch {
		case w.isNew(p) && c.Op == "create":
			lines := splitText(c.Text)
			after := []byte{}
			for _, l := range lines {
				after = append(after, l+"\n"...)
			}
			f := PlannedFile{Path: p, After: after}
			if len(lines) > 0 {
				f.Regions = [][2]int{{0, len(lines)}}
			}
			if err := r.checkContent(p, nil, after, nil, true, plan); err != nil {
				return nil, err
			}
			add(f)
			plan.Added += len(lines)
			sections = append(sections, diffSection{key: p, text: createDiff(p, lines)})
		case w.isNew(p):
			// A move target: written with the move's source below.
		case c.Op == "remove":
			if tag := fileTagOf(key, src.shown); tag != c.FileTag {
				return nil, refusal(p, "file_tag %s does not match the file as it is now: it changed since you read it. Read it again before removing it", c.FileTag)
			}
			add(PlannedFile{Path: p, Before: src.data})
			plan.Removed += len(src.shown)
			sections = append(sections, diffSection{key: p, text: removeDiff(p, src.shown)})
		default: // edited, moved, or both
			after, regions, hunks := src.data, [][2]int(nil), ""
			to := p
			if isWhole { // a move
				to = c.ToPath
			}
			if idx := w.edits[p]; len(idx) > 0 {
				e, err := r.editFile(key, p, src, changes, idx)
				if err != nil {
					return nil, err
				}
				after, regions = e.after, e.regions
				plan.Added += e.added
				plan.Removed += e.removed
				hunks = unifiedHunks(src.shown, e.edits)
				if err := r.checkContent(to, src.data, after, e.lockedAfter, isWhole && syntaxKind(p) != syntaxKind(to), plan); err != nil {
					return nil, err
				}
			} else if isWhole {
				// Moved unchanged: the syntax gate applies only when the new
				// name gives the content a format the old one did not.
				if syntaxKind(p) != syntaxKind(to) {
					hidden, err := r.lockedLines(ctx, src.data)
					if err != nil {
						return nil, err
					}
					if msg := r.syntaxProblem(to, nil, true, src.data, hidden); msg != "" {
						return nil, refusal(to, "%s", msg)
					}
				}
			}
			var d strings.Builder
			if isWhole {
				add(PlannedFile{Path: p, Before: src.data})
				add(PlannedFile{Path: to, After: after, Regions: regions})
				fmt.Fprintf(&d, "rename from %s\nrename to %s\n", Sanitize(p), Sanitize(to))
			} else {
				add(PlannedFile{Path: p, Before: src.data, After: after, Regions: regions})
			}
			if hunks != "" {
				fmt.Fprintf(&d, "--- a/%s\n+++ b/%s\n%s", Sanitize(p), Sanitize(to), hunks)
			}
			sections = append(sections, diffSection{key: p, text: d.String()})
		}
	}
	sort.Slice(plan.Files, func(i, j int) bool { return plan.Files[i].Path < plan.Files[j].Path })
	sort.SliceStable(sections, func(i, j int) bool { return sections[i].key < sections[j].key })
	var diff strings.Builder
	for _, s := range sections {
		diff.WriteString(s.text)
	}
	plan.Diff = diff.String()
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	plan.ID = hex.EncodeToString(id[:])
	return plan, nil
}

// source is an existing file the change set touches.
type source struct {
	data  []byte
	shown []shownLine // nil unless it had to be text
}

// readSource reads an existing file. With needText it must be UTF-8 text,
// and its lines as read_file shows them are computed.
func (r *Repo) readSource(ctx context.Context, op *opener, p string, needText bool) (*source, error) {
	data, _, truncated, err := op.ReadAll(p, maxPlannedFile)
	if err != nil {
		return nil, refusal(p, "%v", describeErr(p, err))
	}
	if truncated {
		return nil, refusal(p, "is larger than 2 MiB; a change set cannot change, move or remove it")
	}
	s := &source{data: data}
	if !needText {
		return s, nil
	}
	why := ""
	switch {
	case hasUTF16BOM(data):
		why = "is UTF-16 text"
	case bytes.IndexByte(data, 0) >= 0:
		why = "is a binary file"
	case !utf8.Valid(data):
		why = "is not valid UTF-8"
	}
	if why != "" {
		return nil, refusal(p, "%s; only UTF-8 text files can be edited or removed through a change set (moving one is allowed)", why)
	}
	if s.shown, err = r.shownLines(ctx, data); err != nil {
		return nil, refusal(p, "cannot be read as lines: %v", err)
	}
	return s, nil
}

type staleAnchor struct {
	path, anchor string
	line         int
}

// staleError reports every stale anchor with the current lines n-3 … n+3
// around it (the last lines of the file for one past its end).
func (r *Repo) staleError(key []byte, stale []staleAnchor, srcs map[string]*source) error {
	first := stale[0]
	reason := fmt.Sprintf("anchor %s does not match the current file: it changed since you read it, or the line number is wrong", first.anchor)
	if first.line > len(srcs[first.path].shown) {
		reason = fmt.Sprintf("anchor %s names line %d, but the file has %d lines", first.anchor, first.line, len(srcs[first.path].shown))
	}
	if len(stale) > 1 {
		reason += fmt.Sprintf(" (%d stale anchors in all)", len(stale))
	}
	var b strings.Builder
	byPath := map[string][][2]int{}
	var order []string
	for _, s := range stale {
		n := len(srcs[s.path].shown)
		from, to := s.line-diffContext, s.line+diffContext
		if s.line > n {
			from, to = n-2*diffContext, n
		}
		from, to = max(from, 1), min(to, n)
		if from > to {
			continue
		}
		if _, ok := byPath[s.path]; !ok {
			order = append(order, s.path)
		}
		byPath[s.path] = append(byPath[s.path], [2]int{from, to})
	}
	for _, p := range order {
		wins := byPath[p]
		sort.Slice(wins, func(i, j int) bool { return wins[i][0] < wins[j][0] })
		var merged [][2]int
		for _, wi := range wins {
			if k := len(merged); k > 0 && wi[0] <= merged[k-1][1]+1 {
				merged[k-1][1] = max(merged[k-1][1], wi[1])
				continue
			}
			merged = append(merged, wi)
		}
		fmt.Fprintf(&b, "FILE %s\n", Sanitize(p))
		lines := srcs[p].shown
		for k, wi := range merged {
			if k > 0 {
				b.WriteString("…\n")
			}
			width := len(strconv.Itoa(wi[1]))
			for n := wi[0]; n <= wi[1]; n++ {
				fmt.Fprintf(&b, "%*d:%s| %s\n", width, n, lineAnchor(key, lines[n-1]), displayShown(lines[n-1]))
			}
		}
	}
	return &ChangeError{Path: SanitizeLine(first.path), Reason: reason, Fresh: b.String()}
}

// displayShown is a shown line as read_file prints it.
func displayShown(l shownLine) string {
	if l.Cut {
		return l.Text
	}
	return truncateLine(l.Text, false)
}

// lineEdit replaces old lines [a, b) (0-based) with lines; a == b inserts.
type lineEdit struct {
	a, b  int
	lines []string
}

type editResult struct {
	after          []byte
	regions        [][2]int
	edits          []lineEdit // sorted by position
	lockedAfter    map[int]bool
	added, removed int
}

// rawLine is one line of a file as stored: its text and its terminator
// ("\n", "\r\n", or "" for a last line without one).
type rawLine struct{ text, term string }

// splitRaw splits content into stored lines, the way readLongLine counts
// them. A UTF-8 byte order mark is returned apart, so that it survives a
// change to the first line.
func splitRaw(data []byte) (lines []rawLine, bom string) {
	s := string(data)
	if strings.HasPrefix(s, "\ufeff") {
		bom, s = "\ufeff", s[len("\ufeff"):]
	}
	for s != "" {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			lines = append(lines, rawLine{text: s})
			break
		}
		l := rawLine{text: s[:i], term: "\n"}
		if strings.HasSuffix(l.text, "\r") {
			l.text, l.term = l.text[:len(l.text)-1], "\r\n"
		}
		lines = append(lines, l)
		s = s[i+1:]
	}
	return lines, bom
}

// editFile applies the line operations idx to one file, bottom-up in effect:
// every position refers to the file before the change set.
func (r *Repo) editFile(key []byte, p string, src *source, changes []Change, idx []int) (*editResult, error) {
	old, bom := splitRaw(src.data)
	shown := src.shown
	if len(old) != len(shown) || (bom != "" && len(old) == 0) {
		return nil, refusal(p, "its lines could not be lined up with what read_file shows; refusing to guess")
	}
	n := len(old)
	crlf, lf := 0, 0
	for _, l := range old {
		switch l.term {
		case "\r\n":
			crlf++
		case "\n":
			lf++
		}
	}
	eol := "\n"
	if crlf > lf {
		eol = "\r\n"
	}
	finalNL := n == 0 || old[n-1].term != ""

	touched := map[int]int{} // 0-based line → operation
	gaps := map[int]int{}    // insertion point → operation
	touch := func(op, line int) error {
		if j, ok := touched[line]; ok {
			return refusal(p, "operations %d and %d both address line %d; ranges and insertion points in one file may not overlap", j+1, op+1, line+1)
		}
		touched[line] = op
		return nil
	}
	var edits []lineEdit
	for _, i := range idx {
		c := changes[i]
		switch c.Op {
		case "replace", "delete":
			from, _ := parseAnchor(c.From)
			to := from
			if c.To != "" {
				to, _ = parseAnchor(c.To)
			}
			if to < from {
				return nil, refusal(p, "operation %d: the range ends at line %d, before it starts (line %d)", i+1, to, from)
			}
			for l := from - 1; l < to; l++ {
				if shown[l].Locked {
					return nil, refusal(p, "line %d holds a value you were not shown; the owner has to change it by hand", l+1)
				}
				if shown[l].Long {
					return nil, refusal(p, "line %d was shown cut (it is longer than %d bytes); a change set cannot replace or delete it, but may insert beside it", l+1, maxLineDisplay)
				}
				if err := touch(i, l); err != nil {
					return nil, err
				}
			}
			edits = append(edits, lineEdit{a: from - 1, b: to, lines: splitText(c.Text)})
		case "insert":
			var gap int
			switch c.At {
			case "start":
				gap = 0
			case "end":
				gap = n
			default:
				line, _ := parseAnchor(c.At)
				if err := touch(i, line-1); err != nil {
					return nil, err
				}
				gap = line
				if c.Where == "before" {
					gap = line - 1
				}
			}
			// Between two locked lines the point may lie inside one masked
			// value (a key block): inserting there would change it.
			if gap > 0 && gap < n && shown[gap-1].Locked && shown[gap].Locked {
				return nil, refusal(p, "the insertion point between lines %d and %d lies inside a value you were not shown; the owner has to change it by hand", gap, gap+1)
			}
			if j, ok := gaps[gap]; ok {
				return nil, refusal(p, "operations %d and %d insert at the same point (after line %d)", j+1, i+1, gap)
			}
			gaps[gap] = i
			edits = append(edits, lineEdit{a: gap, b: gap, lines: splitText(c.Text)})
		}
	}
	// A range that touches a line twice was caught above; an insertion point
	// strictly inside a range addresses a line of it, so it was too.
	sort.Slice(edits, func(i, j int) bool {
		if edits[i].a != edits[j].a {
			return edits[i].a < edits[j].a
		}
		return edits[i].b < edits[j].b // an insertion before the range starting there
	})
	inserts := map[int][]string{}
	ranges := map[int]lineEdit{}
	res := &editResult{edits: edits, lockedAfter: map[int]bool{}}
	for _, e := range edits {
		if e.a == e.b {
			inserts[e.a] = e.lines
		} else {
			ranges[e.a] = e
		}
		res.added += len(e.lines)
		res.removed += e.b - e.a
	}
	var out []rawLine
	region := func(start int) {
		end := len(out)
		if k := len(res.regions); k > 0 && start <= res.regions[k-1][1] {
			res.regions[k-1][1] = max(res.regions[k-1][1], end)
			return
		}
		res.regions = append(res.regions, [2]int{start, end})
	}
	emit := func(lines []string) {
		start := len(out)
		for _, l := range lines {
			out = append(out, rawLine{text: l, term: eol})
		}
		region(start)
	}
	for i := 0; ; {
		if ins, ok := inserts[i]; ok {
			emit(ins)
		}
		if i == n {
			break
		}
		if e, ok := ranges[i]; ok {
			emit(e.lines)
			i = e.b
			continue
		}
		if shown[i].Locked {
			res.lockedAfter[len(out)] = true
		}
		out = append(out, old[i])
		i++
	}
	for k := range out {
		switch {
		case k == len(out)-1 && !finalNL:
			out[k].term = ""
		case out[k].term == "":
			out[k].term = eol
		}
	}
	var b bytes.Buffer
	b.WriteString(bom)
	for _, l := range out {
		b.WriteString(l.text)
		b.WriteString(l.term)
	}
	res.after = b.Bytes()
	if res.after == nil {
		res.after = []byte{}
	}
	return res, nil
}

// conflictMarkerRe matches a line git writes into an unresolved merge
// conflict: seven markers, then a blank and a label or the end of the line.
var conflictMarkerRe = regexp.MustCompile(`^(?:<{7}|={7}|>{7})(?: .*)?$`)

// checkContent checks a file's new content: size, NUL bytes, UTF-8, new
// conflict markers, and the syntax gate; a gofmt difference is a warning.
// before is nil for a new file; gateAsNew judges the syntax as for a new
// file. lockedAfter marks lines of after that hold a value the model was
// not shown.
func (r *Repo) checkContent(p string, before, after []byte, lockedAfter map[int]bool, gateAsNew bool, plan *Plan) error {
	if len(after) > maxPlannedFile {
		return refusal(p, "would be %d bytes; a resulting file may be at most 2 MiB (%d bytes)", len(after), maxPlannedFile)
	}
	if bytes.IndexByte(after, 0) >= 0 {
		return refusal(p, "would contain a NUL byte; only text can be written")
	}
	if !utf8.Valid(after) {
		return refusal(p, "would not be valid UTF-8")
	}
	had := map[string]bool{}
	for _, l := range strings.Split(string(before), "\n") {
		if l = strings.TrimSuffix(l, "\r"); conflictMarkerRe.MatchString(l) {
			had[l] = true
		}
	}
	for n, l := range strings.Split(string(after), "\n") {
		if l = strings.TrimSuffix(l, "\r"); conflictMarkerRe.MatchString(l) && !had[l] {
			return refusal(p, "line %d would be a conflict marker (%s) the file did not have; resolve the conflict instead of writing it", n+1, SanitizeLine(l[:7]))
		}
	}
	if msg := r.syntaxProblem(p, before, gateAsNew || before == nil, after, lockedAfter); msg != "" {
		return refusal(p, "%s", msg)
	}
	// gofmt aligns comments by the length of neighbouring lines, so on a
	// file holding a masked value the warning would measure the value.
	if syntaxKind(p) == "go" && len(lockedAfter) == 0 && goParse(p, after) == nil {
		if formatted, err := format.Source(after); err == nil && !bytes.Equal(formatted, after) {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s is not formatted as gofmt would format it; it was left as written", Sanitize(p)))
		}
	}
	return nil
}

// syntaxKind names the format the syntax gate checks for a path: "go",
// "json", or "" for none.
func syntaxKind(p string) string {
	switch {
	case strings.HasSuffix(p, ".go"):
		return "go"
	case strings.HasSuffix(p, ".json"):
		return "json"
	}
	return ""
}

// jsoncFile reports a .json file that by convention may hold comments: a new
// one is not held to strict JSON.
func jsoncFile(p string) bool {
	base := path.Base(p)
	if strings.HasPrefix(base, "tsconfig") || strings.HasPrefix(base, "jsconfig") {
		return true
	}
	for _, d := range strings.Split(p, "/")[:strings.Count(p, "/")] {
		if d == ".vscode" {
			return true
		}
	}
	return false
}

func goParse(p string, src []byte) error {
	_, err := parser.ParseFile(token.NewFileSet(), p, src, parser.SkipObjectResolution)
	return err
}

// syntaxProblem is the gate "do not make it worse": a Go file that parsed
// before (or is new) must parse after, a JSON file that was valid before (or
// is new, unless it is a JSON-with-comments file) must be valid after. The
// message carries the parser's first complaint with its line, never the text
// of a line that holds a value the model was not shown.
func (r *Repo) syntaxProblem(p string, before []byte, isNew bool, after []byte, lockedAfter map[int]bool) string {
	switch syntaxKind(p) {
	case "go":
		if !isNew && goParse(p, before) != nil {
			return ""
		}
		err := goParse(p, after)
		if err == nil {
			return ""
		}
		var list scanner.ErrorList
		if errors.As(err, &list) && len(list) > 0 {
			line := list[0].Pos.Line
			return "would not parse as Go: " + r.parserMessage(line, goMessage(list[0].Msg), lockedAfter)
		}
		return "would not parse as Go"
	case "json":
		if isNew {
			if jsoncFile(p) {
				return ""
			}
		} else if !json.Valid(before) {
			return ""
		}
		if json.Valid(after) {
			return ""
		}
		var v json.RawMessage
		err := json.Unmarshal(after, &v)
		var se *json.SyntaxError
		if errors.As(err, &se) {
			off := min(max(int(se.Offset)-1, 0), len(after))
			line := bytes.Count(after[:off], []byte("\n")) + 1
			return "would not be valid JSON: " + r.parserMessage(line, se.Error(), lockedAfter)
		}
		return "would not be valid JSON"
	}
	return ""
}

// goMessage drops the literal text go/parser quotes after "found".
func goMessage(msg string) string {
	i := strings.Index(msg, ", found ")
	if i < 0 {
		return msg
	}
	rest := msg[i+len(", found "):]
	if rest == "newline" || rest == "EOF" || (len(rest) <= 16 && strings.HasPrefix(rest, "'") && strings.HasSuffix(rest, "'")) {
		return msg
	}
	return msg[:i] + ", found a literal"
}

// hideAll, as a key of a lockedAfter map, withholds the parser's message
// for every line.
const hideAll = -1

// lockedLines marks the lines of content that hold a value the model was
// not shown; for content that is not UTF-8 text, all of them (hideAll).
func (r *Repo) lockedLines(ctx context.Context, content []byte) (map[int]bool, error) {
	if hasUTF16BOM(content) || bytes.IndexByte(content, 0) >= 0 || !utf8.Valid(content) {
		return map[int]bool{hideAll: true}, nil
	}
	shown, err := r.shownLines(ctx, content)
	if err != nil {
		return nil, err
	}
	locked := map[int]bool{}
	for i, l := range shown {
		if l.Locked {
			locked[i] = true
		}
	}
	return locked, nil
}

func (r *Repo) parserMessage(line int, msg string, lockedAfter map[int]bool) string {
	if lockedAfter[hideAll] || lockedAfter[line-1] {
		return fmt.Sprintf("line %d (the parser's message is not shown: the line holds a value you were not shown)", line)
	}
	msg, _ = r.red.Line(msg)
	if len(msg) > 200 {
		msg = msg[:cutPoint(msg, 200)] + "…"
	}
	return fmt.Sprintf("line %d: %s", line, SanitizeLine(msg))
}

// diffSection is the diff of one path, sorted by key.
type diffSection struct{ key, text string }

func newLineDisplay(l string) string { return Sanitize(truncateLine(l, false)) }

func createDiff(p string, lines []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "--- /dev/null\n+++ b/%s\n", Sanitize(p))
	if len(lines) > 0 {
		fmt.Fprintf(&b, "@@ -0,0 +1,%d @@\n", len(lines))
		for _, l := range lines {
			b.WriteString("+" + newLineDisplay(l) + "\n")
		}
	}
	return b.String()
}

func removeDiff(p string, shown []shownLine) string {
	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ /dev/null\n", Sanitize(p))
	if len(shown) > 0 {
		fmt.Fprintf(&b, "@@ -1,%d +0,0 @@\n", len(shown))
		for _, l := range shown {
			b.WriteString("-" + displayShown(l) + "\n")
		}
	}
	return b.String()
}

// unifiedHunks renders sorted edits against the old lines as shown, with
// diffContext lines of context; edits whose context meets share a hunk.
func unifiedHunks(old []shownLine, edits []lineEdit) string {
	var b strings.Builder
	n := len(old)
	label := func(start, count int) string {
		if count == 0 {
			return fmt.Sprintf("%d,0", start)
		}
		return fmt.Sprintf("%d,%d", start+1, count)
	}
	delta := 0 // new lines minus old lines of the edits before the hunk
	for i := 0; i < len(edits); {
		j, end := i+1, edits[i].b
		for j < len(edits) && edits[j].a-end <= 2*diffContext {
			end = max(end, edits[j].b)
			j++
		}
		group := edits[i:j]
		oldStart, oldEnd := max(0, group[0].a-diffContext), min(n, end+diffContext)
		var body strings.Builder
		k, gdelta := oldStart, 0
		for _, e := range group {
			for ; k < e.a; k++ {
				body.WriteString(" " + displayShown(old[k]) + "\n")
			}
			for x := e.a; x < e.b; x++ {
				body.WriteString("-" + displayShown(old[x]) + "\n")
			}
			for _, l := range e.lines {
				body.WriteString("+" + newLineDisplay(l) + "\n")
			}
			k = max(k, e.b)
			gdelta += len(e.lines) - (e.b - e.a)
		}
		for ; k < oldEnd; k++ {
			body.WriteString(" " + displayShown(old[k]) + "\n")
		}
		oldCount := oldEnd - oldStart
		fmt.Fprintf(&b, "@@ -%s +%s @@\n%s", label(oldStart, oldCount), label(oldStart+delta, oldCount+gdelta), body.String())
		delta += gdelta
		i = j
	}
	return b.String()
}
