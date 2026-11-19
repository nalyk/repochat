package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

// Applying a change set writes the new content of every file it touches
// into the session workspace and records the result as one checkpoint: a
// commit on the session branch. Everything happens or nothing does: a
// failure on the way puts every written file back, and the branch moves
// only through `update-ref` with its old value, as the very last step.
//
// The commit is built with plumbing (hash-object, update-index --index-info,
// write-tree, commit-tree, update-ref), which runs no hook, and never signs.
// Only the touched paths are staged, from the validated bytes rather than
// from disk, so the tracked files the policy withholds, which are absent
// from the workspace and marked skip-worktree in its index, stay in the
// commit's tree as they were.

const (
	maxPlans       = 32
	planLifetime   = 30 * time.Minute
	maxSubject     = 100     // characters of a checkpoint message's first line
	maxMessageBody = 8 << 10 // bytes of the rest
	defaultMessage = "Changes through repochat"
	fallbackName   = "repochat"
	fallbackEmail  = "repochat@localhost"
)

// checkpointKinds are the kinds of commit a session branch holds; the kind
// is recorded in the Repochat-Kind trailer.
var checkpointKinds = map[string]bool{"start": true, "change": true, "undo": true, "update": true}

// Checkpoint is one commit on the session branch.
type Checkpoint struct {
	ID             string // first 12 hex characters of the commit
	Kind           string // start | change | undo | update
	Message        string
	Time           time.Time
	Files          []string
	Added, Removed int
}

// Applied is the result of applying a change set.
type Applied struct {
	Checkpoint Checkpoint
	Fresh      string // touched regions ±3 lines with fresh anchors, per file
	Warnings   []string
}

func (a *Applied) clone() *Applied {
	c := *a
	c.Checkpoint.Files = slices.Clone(a.Checkpoint.Files)
	c.Warnings = slices.Clone(a.Warnings)
	return &c
}

// storedPlan is a plan Propose made, kept so that Apply can take its
// identifier. It is tied to the session and to the content of every file it
// touches.
type storedPlan struct {
	session string
	changes []Change
	befores map[string][]byte // path → its content when the plan was made; nil: absent
	made    time.Time
	seq     uint64   // order of making, for eviction among equal times
	applied *Applied // set once applied: a second apply returns it
}

func (s *Sessions) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// prunePlans drops the plans older than planLifetime. The caller holds the
// locks.
func (s *Sessions) prunePlans() {
	now := s.clock()
	for id, p := range s.plans {
		if now.Sub(p.made) > planLifetime {
			delete(s.plans, id)
		}
	}
}

// storePlan keeps plan for Apply, dropping the oldest plans beyond
// maxPlans. The caller holds the locks.
func (s *Sessions) storePlan(rec *sessionRecord, changes []Change, plan *Plan) {
	if s.plans == nil {
		s.plans = map[string]*storedPlan{}
	}
	s.prunePlans()
	for len(s.plans) >= maxPlans {
		var oldest *storedPlan
		oldestID := ""
		for id, p := range s.plans {
			if oldest == nil || p.made.Before(oldest.made) || (p.made.Equal(oldest.made) && p.seq < oldest.seq) {
				oldest, oldestID = p, id
			}
		}
		delete(s.plans, oldestID)
	}
	befores := map[string][]byte{}
	for _, f := range plan.Files {
		befores[f.Path] = bytes.Clone(f.Before) // nil stays nil
	}
	s.planSeq++
	s.plans[plan.ID] = &storedPlan{
		session: rec.ID,
		changes: slices.Clone(changes),
		befores: befores,
		made:    s.clock(),
		seq:     s.planSeq,
	}
}

// matches checks that a fresh validation of the stored changes touches the
// same files, each with byte-identical content before.
func (sp *storedPlan) matches(plan *Plan) error {
	if len(plan.Files) != len(sp.befores) {
		return refusal("", "the files the plan touches changed since the plan was made; propose the change set again")
	}
	for _, f := range plan.Files {
		before, ok := sp.befores[f.Path]
		if !ok || (before == nil) != (f.Before == nil) || !bytes.Equal(before, f.Before) {
			return refusal(f.Path, "changed since the plan was made; propose the change set again")
		}
	}
	return nil
}

// Apply validates a change set against the current files, writes it into
// the workspace and records it as a checkpoint. Exactly one of planID (from
// Propose) and changes is given. A plan is applied only if every file it
// touches is byte-identical to what it was when the plan was made; a plan
// already applied returns the same result again without writing.
func (s *Sessions) Apply(ctx context.Context, planID string, changes []Change, message string) (*Applied, error) {
	if (planID == "") == (len(changes) == 0) {
		return nil, errors.New("give either a plan identifier from propose_changes or a change set, not both and not neither")
	}
	release, err := s.acquire()
	if err != nil {
		return nil, err
	}
	defer release()

	var sp *storedPlan
	if planID != "" {
		s.prunePlans()
		sp = s.plans[planID]
		if sp == nil {
			shown := planID
			if len(shown) > 40 {
				shown = shown[:40] + "…"
			}
			return nil, refusal("", "plan %q is unknown or has expired (plans are kept for 30 minutes, at most %d at a time); propose the change set again, or pass it to apply_changes directly", SanitizeLine(shown), maxPlans)
		}
	}
	rec, err := s.ensure(ctx)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, ErrNoSession
	}
	if sp != nil {
		if sp.session != rec.ID {
			return nil, refusal("", "the plan belongs to an earlier session; propose the change set again")
		}
		if sp.applied != nil {
			return sp.applied.clone(), nil
		}
		changes = sp.changes
	}
	plan, err := s.ws.planChanges(ctx, changes)
	if err != nil {
		return nil, err
	}
	if sp != nil {
		if err := sp.matches(plan); err != nil {
			return nil, err
		}
	}

	moves := map[string]string{} // source → target
	for _, c := range changes {
		if c.Op == "move" {
			moves[c.Path] = c.ToPath
		}
	}
	writes := make([]fileWrite, 0, len(plan.Files))
	for _, f := range plan.Files {
		w := fileWrite{Path: f.Path, Before: f.Before, After: f.After}
		if f.Before == nil && f.After != nil {
			w.Mode = 0o644
			for src, to := range moves {
				if to == f.Path {
					w.Mode, w.ModeOf = 0, src // a moved file keeps its mode
				}
			}
		}
		writes = append(writes, w)
	}
	cp, err := s.checkpoint(ctx, rec, "change", message, writes)
	if err != nil {
		return nil, err
	}
	cp.Added, cp.Removed = plan.Added, plan.Removed
	applied := &Applied{
		Checkpoint: *cp,
		Fresh:      s.ws.freshText(ctx, plan.Files, moves),
		Warnings:   slices.Clone(plan.Warnings),
	}
	if sp != nil {
		sp.applied = applied
	}
	return applied.clone(), nil
}

// fileWrite is one path of a checkpoint: what it holds now and what it
// will hold.
type fileWrite struct {
	Path   string
	Before []byte // its content now; nil: it does not exist
	After  []byte // its new content; nil: it is removed
	// Mode of After. 0 keeps the mode of the file at ModeOf, or else at
	// Path; a new file without either is 0644.
	Mode   os.FileMode
	ModeOf string
}

// checkpoint writes files into the workspace, then commits exactly their
// paths on the session branch as one commit of the given kind, with the
// branch's head and then extraParents as parents. All or nothing: on any
// failure the files are put back and the branch stays where it was. Added
// and Removed are left to the caller.
func (s *Sessions) checkpoint(ctx context.Context, rec *sessionRecord, kind, message string, files []fileWrite, extraParents ...string) (*Checkpoint, error) {
	if !checkpointKinds[kind] {
		return nil, fmt.Errorf("unknown checkpoint kind %q", kind)
	}
	ws := s.ws
	old, code, err := ws.gitText(ctx, "rev-parse", "--verify", "-q", "refs/heads/"+rec.Branch+"^{commit}")
	if err != nil {
		return nil, err
	}
	if code != 0 || !objectIDRe.MatchString(old) {
		return nil, missing(rec, "branch")
	}
	for _, p := range extraParents {
		if !objectIDRe.MatchString(p) {
			return nil, errors.New("a checkpoint parent is not a commit id")
		}
	}
	text, msg := commitMessage(message, kind)
	ident := s.identity(ctx)

	files = slices.Clone(files)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	paths := make([]string, len(files))
	for i, f := range files {
		if i > 0 && f.Path == paths[i-1] {
			return nil, fmt.Errorf("%s is written twice in one checkpoint", SanitizeLine(f.Path))
		}
		paths[i] = f.Path
	}

	if err := s.checkIndexLock(ws); err != nil {
		return nil, err
	}
	undo, err := s.writeFiles(ws, files)
	if err != nil {
		return nil, s.rollBack(ctx, err, undo)
	}
	if s.applyFault != nil { // after the last file, before the commit
		if err := s.applyFault(len(files)); err != nil {
			return nil, s.rollBack(ctx, err, undo)
		}
	}
	id, err := ws.commitPaths(ctx, ident, msg, files, rec.Branch, old, extraParents, kind)
	var stale *staleIndexError
	if errors.As(err, &stale) {
		// The branch holds the checkpoint and the files match it; only the
		// index lags behind. Resuming restores it from the branch.
		s.closeWorkspace()
		return nil, err
	}
	if errors.Is(err, errIndexOutOfStep) {
		err = s.rollBack(ctx, err, undo)
		// Resuming restores the index from the branch.
		s.closeWorkspace()
		return nil, err
	}
	if err != nil {
		return nil, s.rollBack(ctx, err, undo)
	}
	ws.mu.Lock()
	ws.snap = nil
	ws.mu.Unlock()

	cp := &Checkpoint{ID: id[:12], Kind: kind, Message: text, Files: paths}
	if cp.Time, err = ws.commitTime(ctx, id); err != nil {
		cp.Time = time.Now().UTC().Truncate(time.Second)
	}
	return cp, nil
}

// rollBack puts back the files an apply wrote and returns the error that
// made it fail. When a file cannot be put back the workspace is restored
// from its last checkpoint; when that fails too, the workspace is closed so
// that the next call resumes it, which restores it.
func (s *Sessions) rollBack(ctx context.Context, cause error, undo func() error) error {
	rerr := undo()
	if rerr == nil {
		return fmt.Errorf("%w (nothing was changed)", cause)
	}
	if s.owner.Logf != nil {
		s.owner.Logf("apply: rollback failed: %v", rerr)
	}
	if err := s.restore(ctx); err != nil {
		if s.owner.Logf != nil {
			s.owner.Logf("apply: restore after a failed rollback failed: %v", err)
		}
		s.closeWorkspace()
		return fmt.Errorf("%w; putting the files back failed too (%v), so the session workspace may be inconsistent: it is restored from the last checkpoint on the next call", cause, rerr)
	}
	return fmt.Errorf("%w; putting the files back failed (%v), so the session workspace was restored from the last checkpoint", cause, rerr)
}

// writeErr describes a failed file write without local paths.
func writeErr(p string, err error) error {
	return fmt.Errorf("writing %s failed: %v", SanitizeLine(p), pathErrCause(err))
}

// writeFiles writes files in order (sorted by path), each through the no-
// follow writer, after checking that every one still holds its Before. The
// mode each file is written with is set in its Mode. It returns the function
// that puts back the files written so far, which is also valid after an
// error.
func (s *Sessions) writeFiles(ws *Repo, files []fileWrite) (undo func() error, err error) {
	op := ws.newOpener()
	var done []fileWrite
	modes := map[string]os.FileMode{}
	undo = func() error {
		defer op.Close()
		var first error
		for i := len(done) - 1; i >= 0; i-- {
			f := done[i]
			var err error
			if f.Before == nil {
				err = op.Remove(f.Path)
			} else {
				err = op.WriteAtomic(f.Path, f.Before, modes[f.Path])
			}
			if err != nil && first == nil {
				first = writeErr(f.Path, err)
			}
		}
		return first
	}

	// Each file must still be what the change set was validated against:
	// that is also what a rollback writes back.
	for _, f := range files {
		if f.Before == nil {
			_, err := op.Lstat(f.Path)
			switch {
			case errors.Is(err, os.ErrNotExist):
			case errors.Is(err, errSymlink) || errors.Is(err, syscall.ELOOP):
				return undo, fmt.Errorf("%s passes through a symbolic link; symbolic links are never followed", SanitizeLine(f.Path))
			case err != nil:
				return undo, fmt.Errorf("%s cannot be checked: %v", SanitizeLine(f.Path), pathErrCause(err))
			default:
				return undo, fmt.Errorf("%s appeared in the workspace while the change set was being applied", SanitizeLine(f.Path))
			}
			continue
		}
		data, fi, truncated, err := op.ReadAll(f.Path, int64(len(f.Before)))
		if err != nil {
			return undo, fmt.Errorf("%s cannot be read: %v", SanitizeLine(f.Path), pathErrCause(err))
		}
		if truncated || !bytes.Equal(data, f.Before) {
			return undo, fmt.Errorf("%s changed while the change set was being applied", SanitizeLine(f.Path))
		}
		modes[f.Path] = fi.Mode().Perm()
	}
	op.Close()

	for i := range files {
		f := &files[i]
		if s.applyFault != nil {
			if err := s.applyFault(i); err != nil {
				return undo, writeErr(f.Path, err)
			}
		}
		var err error
		if f.After == nil {
			err = op.Remove(f.Path)
		} else {
			mode := f.Mode
			if mode == 0 {
				switch {
				case f.ModeOf != "":
					mode = modes[f.ModeOf]
				case f.Before != nil:
					mode = modes[f.Path]
				}
			}
			if mode == 0 {
				mode = 0o644
			}
			f.Mode = mode
			err = op.WriteAtomic(f.Path, f.After, mode)
		}
		if err != nil {
			return undo, writeErr(f.Path, err)
		}
		done = append(done, *f)
	}
	return undo, nil
}

// staleIndexError is a checkpoint that was recorded on the branch while the
// workspace's index could not be brought in line with it.
type staleIndexError struct{ id string }

func (e *staleIndexError) Error() string {
	return fmt.Sprintf("checkpoint %s was recorded, but the session workspace's git index could not be updated; it is restored from the checkpoint on the next call", e.id[:12])
}

// errIndexOutOfStep is an index that does not hold the tree of the commit
// a checkpoint is made on.
var errIndexOutOfStep = errors.New("the session workspace's git index is out of step with its branch (changed by a git command outside repochat); nothing was committed, and the workspace is restored from the last checkpoint on the next call")

// commitPaths commits files, as written by writeFiles, on top of old and
// moves branch to the new commit, with git's index lock held throughout.
// The index entries are made from the bytes in files, never read back from
// disk: each blob goes through git's own checkin conversion for its path
// (hash-object --path), and its mode is the one the file was written with.
// They are staged in the lock file, a copy of the index that must hold
// old's tree, so the index itself changes only once the branch has moved;
// on any error before that, neither has. cfg are the identity overrides.
func (r *Repo) commitPaths(ctx context.Context, cfg []string, msg []byte, files []fileWrite, branch, old string, extraParents []string, kind string) (string, error) {
	// hash-object --path converts as a checkin would: no clean filter may run.
	if err := r.refreshFilterArgs(ctx); err != nil {
		return "", err
	}
	index := filepath.Join(r.GitDir, "index")
	lock, err := lockIndex(index)
	if err != nil {
		return "", err
	}
	locked := true
	defer func() {
		if locked {
			os.Remove(lock)
		}
	}()
	ictx := context.WithValue(ctx, gitIndexKey{}, lock)
	treeOf := func(c context.Context) (string, error) {
		out, err := r.git(c, 4096, "write-tree")
		if err != nil {
			return "", err
		}
		tree := strings.TrimSpace(string(out))
		if !objectIDRe.MatchString(tree) {
			return "", errors.New("git write-tree gave no tree id")
		}
		return tree, nil
	}

	// Whatever the index holds besides the touched paths goes into the
	// commit: it must be exactly old's tree. An index git cannot even write
	// a tree from (unmerged entries, an entry whose object is missing) is
	// out of step as well, and is restored from the branch the same way.
	base, err := treeOf(ictx)
	if err != nil {
		if ctx.Err() != nil {
			return "", err
		}
		if r.Logf != nil {
			r.Logf("apply: the session workspace's index holds no writable tree: %v", err)
		}
		return "", errIndexOutOfStep
	}
	if want, code, err := r.gitText(ctx, "rev-parse", "--verify", "-q", old+"^{tree}"); err != nil || code != 0 || want != base {
		if err != nil {
			return "", err
		}
		return "", errIndexOutOfStep
	}

	// Removals first, so that a path never meets a directory of its name.
	var adds, removes bytes.Buffer
	zero := strings.Repeat("0", len(old))
	for _, f := range files {
		if f.After == nil {
			fmt.Fprintf(&removes, "0 %s\t%s\x00", zero, f.Path)
			continue
		}
		out, err := r.gitInput(ctx, f.After, 4096, "hash-object", "-w", "--stdin", "--path="+f.Path)
		if err != nil {
			return "", err
		}
		blob := strings.TrimSpace(string(out))
		if !objectIDRe.MatchString(blob) || len(blob) != len(old) {
			return "", errors.New("git hash-object gave no blob id")
		}
		mode := "100644"
		if f.Mode&0o111 != 0 {
			mode = "100755"
		}
		fmt.Fprintf(&adds, "%s %s\t%s\x00", mode, blob, f.Path)
	}
	if _, err := r.gitInput(ictx, append(removes.Bytes(), adds.Bytes()...), 1<<20, "update-index", "-z", "--index-info"); err != nil {
		return "", err
	}
	tree, err := treeOf(ictx)
	if err != nil {
		return "", err
	}
	args := append(slices.Clone(cfg), "commit-tree", "-p", old)
	for _, p := range extraParents {
		args = append(args, "-p", p)
	}
	args = append(args, "--no-gpg-sign", "-F", "-", tree)
	out, err := r.gitInput(ctx, msg, 4096, args...)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if !objectIDRe.MatchString(id) {
		return "", errors.New("git commit-tree gave no commit id")
	}
	ref := "refs/heads/" + branch
	if _, err := r.git(ctx, 4096, append(slices.Clone(cfg), "update-ref", "-m", "repochat: "+kind, ref, id, old)...); err != nil {
		if now, _, _ := r.gitText(ctx, "rev-parse", "--verify", "-q", ref+"^{commit}"); now != old {
			return "", fmt.Errorf("the session branch %s moved while the files were being written (another git command working on it)", branch)
		}
		return "", err
	}
	if err := os.Rename(lock, index); err != nil {
		if r.Logf != nil {
			r.Logf("apply: cannot install the new index: %v", err)
		}
		if _, berr := r.git(ctx, 4096, append(slices.Clone(cfg), "update-ref", "-m", "repochat: undo a failed "+kind, ref, old, id)...); berr != nil {
			return "", &staleIndexError{id: id}
		}
		return "", errors.New("the session workspace's git index could not be updated")
	}
	locked = false
	return id, nil
}

// errIndexBusy is an index.lock that another git command may still hold.
var errIndexBusy = errors.New("another git command is running in the session workspace (its git index is locked); retry in a moment")

// staleLockAge is how old an index.lock must be to count as left behind by
// a git command that crashed. Under the session lock no repochat process can
// hold it; a git command the owner runs there holds it for far less.
const staleLockAge = 60 * time.Second

// checkIndexLock removes the workspace's index.lock when it is older than
// staleLockAge, with a notice for the owner, and refuses when it is
// fresher. The caller holds the session lock.
func (s *Sessions) checkIndexLock(ws *Repo) error {
	lock := filepath.Join(ws.GitDir, "index.lock")
	fi, err := os.Lstat(lock)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("cannot check the session workspace's git index lock: %v", pathErrCause(err))
	case time.Since(fi.ModTime()) < staleLockAge:
		return errIndexBusy
	}
	if err := os.Remove(lock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cannot remove a stale index.lock from the session workspace: %v", pathErrCause(err))
	}
	s.notify("A stale index.lock (older than a minute, left by an interrupted git command) was removed from the session workspace.")
	return nil
}

// lockIndex takes git's lock on index by creating index.lock, exclusively,
// as a copy of index with its modification time, and returns its path.
func lockIndex(index string) (string, error) {
	lock := index + ".lock"
	dst, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
	if errors.Is(err, os.ErrExist) {
		return "", errIndexBusy
	}
	if err != nil {
		return "", fmt.Errorf("cannot lock the session workspace's git index: %v", pathErrCause(err))
	}
	fail := func(err error) (string, error) {
		dst.Close()
		os.Remove(lock)
		return "", fmt.Errorf("cannot copy the session workspace's git index: %v", pathErrCause(err))
	}
	src, err := os.Open(index)
	if err != nil {
		return fail(err)
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return fail(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		return fail(err)
	}
	if err := dst.Close(); err != nil {
		os.Remove(lock)
		return "", err
	}
	if err := os.Chtimes(lock, fi.ModTime(), fi.ModTime()); err != nil {
		os.Remove(lock)
		return "", err
	}
	return lock, nil
}

// commitTime reads the committer time of a commit.
func (r *Repo) commitTime(ctx context.Context, id string) (time.Time, error) {
	t, _, err := r.commitInfo(ctx, id)
	return t, err
}

// identity returns the git options that make the owner's identity, as git
// reports it for the owner's repository, the author and committer of a
// checkpoint, and that keep git from signing it. author.* and committer.*
// are set too: git prefers them to user.*.
func (s *Sessions) identity(ctx context.Context) []string {
	get := func(key string) string {
		out, code, err := s.owner.gitText(ctx, "config", "--get", key)
		if err != nil || code != 0 {
			return ""
		}
		return out
	}
	// Either both are the owner's or both are repochat's.
	name, email := cleanIdentity(get("user.name"), ""), cleanIdentity(get("user.email"), "")
	if name == "" || email == "" {
		name, email = fallbackName, fallbackEmail
	}
	return []string{
		"-c", "user.name=" + name, "-c", "user.email=" + email,
		"-c", "author.name=" + name, "-c", "author.email=" + email,
		"-c", "committer.name=" + name, "-c", "committer.email=" + email,
		"-c", "user.useConfigOnly=false",
		"-c", "commit.gpgsign=false",
		"-c", "i18n.commitEncoding=UTF-8",
	}
}

// cleanIdentity makes a name or e-mail address safe for a commit header: no
// line breaks or other control characters, no angle brackets, blanks
// collapsed, at most 200 characters. What is left empty, or only of the
// characters git strips from a name, is fallback.
func cleanIdentity(v, fallback string) string {
	v = strings.ToValidUTF8(v, "")
	v = strings.Map(func(r rune) rune {
		if r == '<' || r == '>' || unicode.IsControl(r) || unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, v)
	v = strings.Join(strings.Fields(v), " ")
	if utf8.RuneCountInString(v) > 200 {
		v = string([]rune(v)[:200])
	}
	if strings.Trim(v, ".,:;\"' ") == "" {
		return fallback
	}
	return v
}

// commitMessage builds a checkpoint's message: the given one, first line cut
// to maxSubject characters (defaultMessage when there is none), then a
// blank line and the trailers. text is the message without the trailers.
func commitMessage(message, kind string) (text string, full []byte) {
	m := strings.ToValidUTF8(message, "�")
	m = strings.ReplaceAll(m, "\r\n", "\n")
	m = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r == '\r':
			return '\n'
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, m)
	lines := strings.Split(m, "\n")
	for i := range lines {
		lines[i] = strings.TrimRightFunc(lines[i], unicode.IsSpace)
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	subject, body := defaultMessage, ""
	if len(lines) > 0 {
		subject = strings.TrimSpace(lines[0])
		if utf8.RuneCountInString(subject) > maxSubject {
			subject = strings.TrimSpace(string([]rune(subject)[:maxSubject]))
		}
		body = strings.Trim(strings.Join(lines[1:], "\n"), "\n")
		if len(body) > maxMessageBody {
			cut := maxMessageBody
			for cut > 0 && !utf8.RuneStart(body[cut]) {
				cut--
			}
			body = body[:cut] + "…"
		}
	}
	text = subject
	if body != "" {
		text += "\n\n" + body
	}
	return text, []byte(text + "\n\nMade-through: repochat\nRepochat-Kind: " + kind + "\n")
}

// freshText shows, for each file, its changed regions with diffContext lines
// around them and the anchors read_file shows for them now. moves maps a
// moved file's old path to its new one.
func (r *Repo) freshText(ctx context.Context, files []PlannedFile, moves map[string]string) string {
	key := r.Opts.AnchorKey
	from := map[string]string{}
	for src, to := range moves {
		from[to] = src
	}
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "FILE %s\n", Sanitize(f.Path))
		switch {
		case f.After == nil && moves[f.Path] != "":
			fmt.Fprintf(&b, "[moved to %s]\n", Sanitize(moves[f.Path]))
			continue
		case f.After == nil:
			b.WriteString("[removed]\n")
			continue
		case len(f.Regions) == 0 && from[f.Path] != "":
			fmt.Fprintf(&b, "[moved from %s; content unchanged]\n", Sanitize(from[f.Path]))
			continue
		case len(f.Regions) == 0:
			b.WriteString("[content unchanged]\n")
			continue
		}
		lines, err := r.shownLines(ctx, f.After)
		if err != nil {
			b.WriteString("[fresh anchors unavailable: read the file again]\n")
			continue
		}
		if len(lines) == 0 {
			b.WriteString("[empty]\n")
			continue
		}
		var wins [][2]int
		for _, reg := range f.Regions {
			// 1-based, inclusive: the changed lines reg[0]+1 … reg[1], or the
			// point between lines reg[0] and reg[0]+1 where lines were deleted.
			from, to := max(reg[0]+1-diffContext, 1), min(reg[1]+diffContext, len(lines))
			if from <= to {
				wins = append(wins, [2]int{from, to})
			}
		}
		writeWindows(&b, key, lines, wins)
	}
	return b.String()
}
