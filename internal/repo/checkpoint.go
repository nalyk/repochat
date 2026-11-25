package repo

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Checkpoints are the commits on the first-parent chain of the session
// branch, from its head back to the commit the session started from. Undo
// restores the files of an earlier one as a new checkpoint; nothing is ever
// taken off the branch.

// maxUndoFiles is the most paths one undo restores.
const maxUndoFiles = 1000

var checkpointIDRe = regexp.MustCompile(`^[0-9a-f]+$`)

// Checkpoints lists the session's checkpoints, newest first; the last entry
// is the start, the commit the session was made from. Files and line counts
// are those of each commit against its first parent, without the paths the
// policy withholds.
func (s *Sessions) Checkpoints(ctx context.Context) ([]Checkpoint, error) {
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
	_, chain, err := s.chain(ctx, rec)
	if err != nil {
		return nil, err
	}
	pol, err := s.policy(ctx, rec)
	if err != nil {
		return nil, err
	}
	out := make([]Checkpoint, 0, len(chain))
	for i, id := range chain {
		parent := ""
		if i+1 < len(chain) {
			parent = chain[i+1]
		}
		cp, err := s.reader().describeCheckpoint(ctx, pol, id, parent)
		if err != nil {
			return nil, err
		}
		out = append(out, *cp)
	}
	return out, nil
}

// chain returns the head of the session branch and its first-parent chain,
// newest first, down to and including the session's base. A branch that no
// longer descends from its base along first parents is refused.
func (s *Sessions) chain(ctx context.Context, rec *sessionRecord) (head string, chain []string, err error) {
	ws := s.reader()
	head, code, err := ws.gitText(ctx, "rev-parse", "--verify", "-q", "refs/heads/"+rec.Branch+"^{commit}")
	if err != nil {
		return "", nil, err
	}
	if code != 0 || !objectIDRe.MatchString(head) {
		return "", nil, missing(rec, "branch")
	}
	out, err := ws.git(ctx, 64<<20, "rev-list", "--first-parent", "--parents", head, "^"+rec.Base)
	if err != nil {
		return "", nil, err
	}
	broken := fmt.Errorf("the session branch %s no longer descends from the commit the session started from (rewritten outside repochat); its checkpoints cannot be listed or restored", rec.Branch)
	want := head
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l == "" {
			continue
		}
		f := strings.Fields(l)
		if len(f) < 2 || f[0] != want || !objectIDRe.MatchString(f[1]) {
			return "", nil, broken
		}
		chain = append(chain, f[0])
		want = f[1]
	}
	if want != rec.Base {
		return "", nil, broken
	}
	return head, append(chain, rec.Base), nil
}

// describeCheckpoint describes commit id; parent is its first parent, empty
// for the start.
func (r *Repo) describeCheckpoint(ctx context.Context, pol *Policy, id, parent string) (*Checkpoint, error) {
	when, msg, err := r.commitInfo(ctx, id)
	if err != nil {
		return nil, err
	}
	cp := &Checkpoint{ID: id[:12], Time: when}
	if parent == "" {
		cp.Kind, cp.Message = "start", strings.TrimRight(msg, " \t\r\n")
		return cp, nil
	}
	cp.Kind, cp.Message = checkpointKind(msg)
	stats, err := r.numstat(ctx, parent, id)
	if err != nil {
		return nil, err
	}
	paths := make([]string, len(stats))
	for i, st := range stats {
		paths[i] = st.path
	}
	// A failure leaves the answers missing, and every check then withholds.
	_ = pol.Prime(ctx, paths)
	for _, st := range stats {
		if pol.Check(st.path) != "" {
			continue
		}
		cp.Files = append(cp.Files, st.path)
		cp.Added += st.add
		cp.Removed += st.del
	}
	return cp, nil
}

// checkpointKind reads a commit's kind from its trailers, which must be the
// message's last paragraph and exactly what commitMessage writes there; a
// kind written anywhere else, in the body a model wrote, is not believed.
// It returns the message without the trailers. A commit without them (made
// on the branch by hand) is of kind "unknown", with its whole message.
func checkpointKind(msg string) (kind, text string) {
	whole := strings.TrimRight(msg, " \t\r\n")
	lines := strings.Split(whole, "\n")
	blank := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			blank = i
			break
		}
	}
	if blank < 0 {
		return "unknown", whole
	}
	text = strings.TrimRight(strings.Join(lines[:blank], "\n"), " \t\r\n")
	trailers := lines[blank+1:]
	if text == "" || len(trailers) != 2 || trailers[0] != "Made-through: repochat" {
		return "unknown", whole
	}
	k, ok := strings.CutPrefix(trailers[1], "Repochat-Kind: ")
	// "start" is the session's base, never a commit on its branch.
	if !ok || k == "start" || !checkpointKinds[k] {
		return "unknown", whole
	}
	return k, text
}

// commitInfo reads the committer time and the message of a commit. A
// message too long to read whole is cut.
func (r *Repo) commitInfo(ctx context.Context, id string) (time.Time, string, error) {
	out, _, code, err := r.gitRaw(ctx, 1<<20, "cat-file", "commit", id)
	if err != nil {
		return time.Time{}, "", err
	}
	if code != 0 {
		return time.Time{}, "", r.gitFailure("cat-file", out)
	}
	header, msg, _ := strings.Cut(string(out), "\n\n")
	for _, l := range strings.Split(header, "\n") {
		if !strings.HasPrefix(l, "committer ") {
			continue
		}
		f := strings.Fields(l)
		if len(f) < 3 {
			break
		}
		sec, err := strconv.ParseInt(f[len(f)-2], 10, 64)
		if err != nil {
			break
		}
		return time.Unix(sec, 0).UTC(), msg, nil
	}
	return time.Time{}, "", errors.New("the commit has no committer time")
}

type numstatEntry struct {
	path     string
	add, del int
}

// numstat lists the paths that differ between two commits, with their line
// counts (none for a binary file).
func (r *Repo) numstat(ctx context.Context, from, to string) ([]numstatEntry, error) {
	out, err := r.git(ctx, 16<<20, "diff-tree", "-r", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", "--numstat", from, to)
	if err != nil {
		return nil, err
	}
	var entries []numstatEntry
	for _, tok := range splitNUL(out) {
		f := strings.SplitN(tok, "\t", 3)
		if len(f) != 3 || f[2] == "" {
			return nil, errors.New("unexpected output from git diff-tree; refusing to continue")
		}
		e := numstatEntry{path: f[2]}
		if f[0] != "-" {
			e.add, _ = strconv.Atoi(f[0])
			e.del, _ = strconv.Atoi(f[1])
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// treeChange is one path that differs between two commits.
type treeChange struct {
	path             string
	oldMode, newMode string // "000000" where the path does not exist
}

// treeDiff lists the paths that differ between two commits, with their modes.
func (r *Repo) treeDiff(ctx context.Context, from, to string) ([]treeChange, error) {
	out, err := r.git(ctx, 64<<20, "diff-tree", "-r", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", from, to)
	if err != nil {
		return nil, err
	}
	malformed := errors.New("unexpected output from git diff-tree; refusing to continue")
	toks := splitNUL(out)
	var changes []treeChange
	for i := 0; i < len(toks); i += 2 {
		f := strings.Fields(strings.TrimPrefix(toks[i], ":"))
		if !strings.HasPrefix(toks[i], ":") || len(f) != 5 || i+1 >= len(toks) || toks[i+1] == "" {
			return nil, malformed
		}
		changes = append(changes, treeChange{path: toks[i+1], oldMode: f[0], newMode: f[1]})
	}
	return changes, nil
}

// undoRefused is the error of an undo that changed nothing.
func undoRefused(format string, a ...any) error {
	return fmt.Errorf("undo refused: "+format+" (nothing was changed)", a...)
}

// UndoTo restores the files of an earlier checkpoint and records them as a
// new checkpoint of kind undo; the checkpoints in between stay in history.
// id is a checkpoint ID or a unique prefix of at least 7 characters. The
// target must not lie before the latest update from the owner's branch.
// Undo to the current checkpoint changes nothing and returns it.
func (s *Sessions) UndoTo(ctx context.Context, id string) (*Applied, error) {
	return s.UndoToCall(ctx, Call{}, id)
}

// UndoToCall is UndoTo for a call that names the checkpoint the session must
// be at (call.Expect): at any other, it refuses before writing anything.
func (s *Sessions) UndoToCall(ctx context.Context, call Call, id string) (*Applied, error) {
	if s.opts.Observe {
		return nil, errObserving
	}
	want := strings.ToLower(id)
	if len(want) > 64 || !checkpointIDRe.MatchString(want) {
		return nil, undoRefused("a checkpoint is named by its hexadecimal ID as list_checkpoints shows it; %q is not one", SanitizeLine(truncate(id, 40)))
	}
	if len(want) < 7 {
		return nil, undoRefused("give at least 7 characters of the checkpoint ID")
	}
	if err := validateCall(call); err != nil {
		return nil, err
	}
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
	s.freshPolicy()
	ws := s.ws
	head, chain, err := s.chain(ctx, rec)
	if err != nil {
		return nil, err
	}
	if err := checkExpected(head, call.Expect); err != nil {
		return nil, err
	}
	boundary := rec.Base
	if rec.LastUpdate != "" {
		boundary = rec.LastUpdate
	}
	at, err := findCheckpoint(chain, want, boundary, rec.StartBranch)
	if err != nil {
		return nil, err
	}
	target := chain[at]
	parentOf := func(i int) string {
		if i+1 < len(chain) {
			return chain[i+1]
		}
		return ""
	}
	pol, err := ws.Policy(ctx)
	if err != nil {
		return nil, err
	}
	current := func() (*Applied, error) {
		cp, err := ws.describeCheckpoint(ctx, pol, head, parentOf(0))
		if err != nil {
			return nil, err
		}
		return &Applied{Checkpoint: *cp, Fresh: "[nothing to restore: the files are already those of this checkpoint]\n"}, nil
	}
	if target == head {
		return current()
	}

	diff, err := ws.treeDiff(ctx, head, target)
	if err != nil {
		return nil, err
	}
	if len(diff) == 0 {
		return current()
	}
	if len(diff) > maxUndoFiles {
		return nil, undoRefused("it would restore %d files; at most %d are restored at once", len(diff), maxUndoFiles)
	}
	if err := ws.checkUndoPaths(ctx, pol, diff); err != nil {
		return nil, err
	}

	op := ws.newOpener()
	defer op.Close()
	writes := make([]fileWrite, 0, len(diff))
	for _, d := range diff {
		w := fileWrite{Path: d.path}
		if d.oldMode != "000000" {
			data, _, truncated, err := op.ReadAll(d.path, maxRevBlobBytes)
			if err != nil {
				return nil, undoRefused("%v", describeErr(SanitizeLine(d.path), err))
			}
			if truncated {
				return nil, undoRefused("%s is larger than %s", SanitizeLine(d.path), humanSize(maxRevBlobBytes))
			}
			w.Before = data
		}
		if d.newMode != "000000" {
			data, err := ws.blobAt(ctx, target, d.path)
			if err != nil {
				return nil, undoRefused("%v", err)
			}
			if data == nil {
				data = []byte{}
			}
			w.After = data
			w.Mode = 0o644
			if d.newMode == "100755" {
				w.Mode = 0o755
			}
		}
		writes = append(writes, w)
	}
	op.Close()

	cp, err := s.checkpoint(ctx, rec, "undo", "Undo to checkpoint "+target[:12], writes)
	if err != nil {
		return nil, err
	}
	if stats, err := ws.numstat(ctx, head, target); err == nil {
		for _, st := range stats {
			cp.Added += st.add
			cp.Removed += st.del
		}
	}
	var fresh strings.Builder
	for _, w := range writes {
		fmt.Fprintf(&fresh, "FILE %s\n", Sanitize(w.Path))
		if w.After == nil {
			fresh.WriteString("[removed]\n")
		} else {
			fresh.WriteString("[restored: read the file again for fresh anchors]\n")
		}
	}
	return &Applied{Checkpoint: *cp, Fresh: fresh.String()}, nil
}

// findCheckpoint returns the index in chain (newest first) of the one
// checkpoint whose ID starts with want. boundary is the oldest checkpoint an
// undo may go back to: the latest update from the owner's branch, or the
// session's base. A prefix that names none or several, a target older than
// the boundary, and a boundary that is not on the chain are refused.
func findCheckpoint(chain []string, want, boundary, startBranch string) (int, error) {
	bound := -1
	var matches []int
	for i, c := range chain {
		if c == boundary {
			bound = i
		}
		if strings.HasPrefix(c, want) {
			matches = append(matches, i)
		}
	}
	switch {
	case bound < 0:
		return 0, undoRefused("the latest update recorded for the session is not on its branch, so how far back an undo may go is not known; undo is refused until a new session is started with --new-session")
	case len(matches) == 0:
		return 0, undoRefused("%s is not a checkpoint of this session; list_checkpoints shows them", want)
	case len(matches) > 1:
		return 0, undoRefused("%s names more than one checkpoint; give more characters", want)
	case matches[0] > bound:
		return 0, undoRefused("cannot undo past the update from %s: that would revert the owner's own commits and merge the reversal back into %s. Undo to the update or a later checkpoint", startBranch, startBranch)
	}
	return matches[0], nil
}

// checkUndoPaths refuses an undo that would write what a session never
// writes: a path the policy withholds, a protected path, a symbolic link or
// a submodule, or a file that is not in the workspace as its checkpoint
// has it (left out by skip-worktree, or changed outside repochat).
func (r *Repo) checkUndoPaths(ctx context.Context, pol *Policy, diff []treeChange) error {
	paths := make([]string, len(diff))
	for i, d := range diff {
		paths[i] = d.path
	}
	// A failure leaves the answers missing, and every check then withholds.
	_ = pol.Prime(ctx, paths)
	for _, d := range diff {
		if pol.Check(d.path) != "" {
			return undoRefused("a file the repository owner's policy withholds differs between the current checkpoint and the target, and a session never changes such files; the owner can restore it by hand")
		}
	}
	for _, d := range diff {
		p := SanitizeLine(d.path)
		if protectedPath(d.path) {
			return undoRefused("%s is a protected path, which a session never changes; the owner can restore it by hand", p)
		}
		for _, m := range []string{d.oldMode, d.newMode} {
			switch m {
			case "000000", "100644", "100755":
			case "120000":
				return undoRefused("%s is a symbolic link in one of the two checkpoints; a session never writes one", p)
			case "160000":
				return undoRefused("%s is a submodule in one of the two checkpoints; a session never writes one", p)
			default:
				return undoRefused("%s has a kind of entry (mode %s) a session never writes", p, SanitizeLine(m))
			}
		}
	}

	specs := make([]string, 0, len(paths)+4)
	specs = append(specs, "ls-files", "-v", "-z", "--")
	for _, p := range paths {
		specs = append(specs, p) // git runs with GIT_LITERAL_PATHSPECS=1
	}
	out, err := r.git(ctx, 64<<20, specs...)
	if err != nil {
		return err
	}
	tags := map[string][]string{}
	for _, tok := range splitNUL(out) {
		tag, p, ok := strings.Cut(tok, " ")
		if !ok {
			return errors.New("unexpected output from git ls-files; refusing to continue")
		}
		tags[p] = append(tags[p], tag)
	}
	for _, d := range diff {
		p := SanitizeLine(d.path)
		t := tags[d.path]
		switch {
		case d.oldMode == "000000" && len(t) == 0:
		case len(t) == 1 && t[0] == "S":
			return undoRefused("%s is not in the session workspace (left out of its checkout); it cannot be restored there", p)
		case d.oldMode == "000000" || len(t) != 1 || t[0] != "H":
			return undoRefused("%s is not in the session workspace's git index as the current checkpoint has it (changed outside repochat)", p)
		}
	}

	if err := r.beforeWorktreeRead(ctx); err != nil {
		return err
	}
	status := append([]string{"status", "--porcelain", "-z", "--untracked-files=all", "--no-renames", "--ignore-submodules=dirty", "--"}, specs[4:]...)
	if out, err = r.git(ctx, 64<<20, status...); err != nil {
		return err
	}
	if len(out) > 0 {
		return undoRefused("files it would restore have changes in the session workspace that belong to no checkpoint (made outside repochat)")
	}
	return nil
}

// truncate cuts s to n bytes, marking the cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
