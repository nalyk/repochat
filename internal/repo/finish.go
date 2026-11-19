package repo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// The end of a session. Update merges the owner's branch into the session;
// Finish merges the session into the owner's branch, in the owner's own
// checkout, as a fast-forward and nothing else; Abandon discards the
// workspace. Finish is the only operation that changes the owner's checkout
// and branch: it either moves the branch forward to a commit that descends
// from it, through `git merge --ff-only`, or changes nothing. No hook, merge
// driver, filter or signing program runs at any step: hooks are off for
// every git command (core.hooksPath=/dev/null), filter drivers are
// neutralised before every command that writes files, an update is refused
// while any merge driver is configured, and commits are made with plumbing,
// unsigned.

// SessionStatus describes where the open session stands. (Status is taken
// by the parsed `git status`.)
type SessionStatus struct {
	Info        SessionInfo
	Ahead       int      // checkpoints since the start
	Files       []string // paths that differ from the owner's branch tip
	OwnerMoved  bool     // the start branch has commits the session lacks
	OwnerBranch string   // the branch the owner's checkout is on now ("" when detached)
	RunsLater   []string // changed files that get executed on the owner's machine
}

// Finished is the result of Finish.
type Finished struct {
	Commit    string // the commit the owner's branch is on now
	Squashed  bool
	Files     []string
	RunsLater []string
	Notes     []string // unsigned commits; hooks that did not run
}

// Status reports where the open session stands against the owner's branch.
func (s *Sessions) Status(ctx context.Context) (*SessionStatus, error) {
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
	info, err := s.info(ctx, rec)
	if err != nil {
		return nil, err
	}
	head, chain, err := s.chain(ctx, rec)
	if err != nil {
		return nil, err
	}
	tip, err := s.ownerTip(ctx, rec)
	if err != nil {
		return nil, err
	}
	contained, err := s.owner.isAncestor(ctx, tip, head)
	if err != nil {
		return nil, err
	}
	files, later, _, err := s.changedFiles(ctx, tip, head)
	if err != nil {
		return nil, err
	}
	branch, err := s.ownerBranch(ctx)
	if err != nil {
		return nil, err
	}
	return &SessionStatus{
		Info: *info, Ahead: len(chain) - 1, Files: files, OwnerMoved: !contained,
		OwnerBranch: branch, RunsLater: later,
	}, nil
}

// ownerTip is the commit the session's start branch is on now.
func (s *Sessions) ownerTip(ctx context.Context, rec *sessionRecord) (string, error) {
	tip, code, err := s.owner.gitText(ctx, "rev-parse", "--verify", "-q", "refs/heads/"+rec.StartBranch+"^{commit}")
	if err != nil {
		return "", err
	}
	if code != 0 || !objectIDRe.MatchString(tip) {
		return "", fmt.Errorf("the branch %s, which the session started from, no longer exists in the repository", rec.StartBranch)
	}
	return tip, nil
}

// ownerBranch is the branch the owner's checkout is on, "" when detached.
func (s *Sessions) ownerBranch(ctx context.Context) (string, error) {
	ref, code, err := s.owner.gitText(ctx, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return "", err
	}
	if code != 0 || !strings.HasPrefix(ref, "refs/heads/") {
		return "", nil
	}
	return strings.TrimPrefix(ref, "refs/heads/"), nil
}

// isAncestor reports whether commit a is b or one of b's ancestors.
func (r *Repo) isAncestor(ctx context.Context, a, b string) (bool, error) {
	out, _, code, err := r.gitRaw(ctx, 4096, "merge-base", "--is-ancestor", a, b)
	switch {
	case err != nil:
		return false, err
	case code == 0:
		return true, nil
	case code == 1:
		return false, nil
	}
	return false, r.gitFailure("merge-base", out)
}

// changedFiles lists the paths that differ between two commits, without
// those the policy withholds (only counted), and among them the ones that
// get executed on the owner's machine later.
func (s *Sessions) changedFiles(ctx context.Context, from, to string) (files, later []string, withheld int, err error) {
	diff, err := s.ws.treeDiff(ctx, from, to)
	if err != nil {
		return nil, nil, 0, err
	}
	pol, err := s.ws.Policy(ctx)
	if err != nil {
		return nil, nil, 0, err
	}
	paths := make([]string, len(diff))
	for i, d := range diff {
		paths[i] = d.path
	}
	// A failure leaves the answers missing, and every check then withholds.
	_ = pol.Prime(ctx, paths)
	for _, d := range diff {
		if pol.Check(d.path) != "" {
			withheld++
			continue
		}
		files = append(files, d.path)
		if d.newMode == "000000" {
			continue
		}
		mode, err := strconv.ParseUint(d.newMode, 8, 32)
		if err != nil {
			return nil, nil, 0, errors.New("unexpected output from git diff-tree; refusing to continue")
		}
		if runsLater(d.path, os.FileMode(mode)&os.ModePerm) {
			later = append(later, d.path)
		}
	}
	sort.Strings(files)
	sort.Strings(later)
	return files, later, withheld, nil
}

// Update merges the owner's start branch into the session as a checkpoint
// of kind update, when it has commits the session lacks. A conflict, or any
// other failure, abandons the merge and leaves the session as it was.
func (s *Sessions) Update(ctx context.Context) (*Applied, error) {
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
	ws := s.ws
	head, chain, err := s.chain(ctx, rec)
	if err != nil {
		return nil, err
	}
	tip, err := s.ownerTip(ctx, rec)
	if err != nil {
		return nil, err
	}
	contained, err := ws.isAncestor(ctx, tip, head)
	if err != nil {
		return nil, err
	}
	pol, err := ws.Policy(ctx)
	if err != nil {
		return nil, err
	}
	if contained {
		parent := ""
		if len(chain) > 1 {
			parent = chain[1]
		}
		cp, err := ws.describeCheckpoint(ctx, pol, head, parent)
		if err != nil {
			return nil, err
		}
		return &Applied{Checkpoint: *cp, Fresh: fmt.Sprintf("[nothing to merge: the session already contains every commit of %s]\n", rec.StartBranch)}, nil
	}

	// A merge driver is a program; git would run it for any file whose
	// attributes name it.
	out, _, code, err := ws.gitRaw(ctx, 64<<10, "config", "-z", "--get-regexp", `^merge\..*\.driver$`)
	switch {
	case err != nil:
		return nil, err
	case code == 0:
		return nil, fmt.Errorf("update refused: a merge driver (merge.<name>.driver, a program git runs to merge files) is configured for this repository, and repochat runs no program. The owner can merge %s into %s by hand with git (nothing was changed)", rec.StartBranch, rec.Branch)
	case code != 1:
		return nil, ws.gitFailure("config", out)
	}

	// The merge starts from a workspace that is exactly its HEAD.
	if err := s.restore(ctx); err != nil {
		return nil, err
	}
	if err := s.checkIndexLock(ws); err != nil {
		return nil, err
	}
	if err := ws.beforeWorktreeRead(ctx); err != nil {
		return nil, err
	}
	// The files withheld now must stay off the disk through the merge; those
	// the owner's branch brings are left out as well wherever git can (sync
	// takes out the rest once the merge is committed).
	before, err := ws.withheldAt(ctx, "HEAD")
	if err != nil {
		return nil, err
	}
	incoming, err := ws.withheldAt(ctx, tip)
	if err != nil {
		return nil, err
	}
	if err := ws.writeSparse(union(before, incoming)); err != nil {
		return nil, err
	}

	ident := s.identity(ctx)
	args := append(slices.Clone(ident),
		"-c", "rerere.enabled=false", "-c", "merge.autoStash=false", "-c", "merge.log=false",
		"merge", "--no-ff", "--no-commit", "--no-verify-signatures", "--no-stat", tip)
	out, _, code, err = ws.gitRaw(ctx, 1<<20, args...)
	if err != nil || code != 0 {
		var cause error
		switch {
		case err != nil:
			cause = err
		default:
			if conflicts, hidden := s.conflicts(ctx, pol); len(conflicts) > 0 || hidden > 0 {
				cause = fmt.Errorf("update refused: merging %s into the session conflicts in %s; the merge was abandoned. Resolving conflicts through the tools is not supported: change the session so that it no longer conflicts, or the owner can merge by hand with git", rec.StartBranch, listPaths(conflicts, hidden))
			} else {
				cause = ws.gitFailure("merge", out)
			}
		}
		return nil, s.abortMerge(ctx, rec, head, cause)
	}
	for _, p := range before {
		if onDisk(ws.Root, p) {
			if s.owner.Logf != nil {
				s.owner.Logf("update: withheld %q written by the merge", p)
			}
			return nil, s.abortMerge(ctx, rec, head, errors.New("update refused: this version of git cannot merge without writing a file the policy withholds into the session workspace; the merge was abandoned. The owner can merge by hand with git"))
		}
	}

	id, err := s.commitUpdate(ctx, rec, ident, head, tip)
	if err != nil {
		return nil, s.abortMerge(ctx, rec, head, err)
	}
	// Files that the policy withholds and that the merge brought leave the
	// disk here; a failure removes the workspace.
	if err := s.sync(ctx); err != nil {
		return nil, fmt.Errorf("the update was recorded as %s, but %w", id[:12], err)
	}
	if pol, err = ws.Policy(ctx); err != nil {
		return nil, err
	}
	cp, err := ws.describeCheckpoint(ctx, pol, id, head)
	if err != nil {
		return nil, err
	}
	return &Applied{Checkpoint: *cp, Fresh: "[the owner's changes were merged: read files again for fresh anchors]\n"}, nil
}

// commitUpdate records the merge in progress in the workspace as a commit of
// kind update with parents head and tip, records it as the latest update, and
// moves the session branch to it. The record is written first: should the
// branch then fail to move, the record names a commit that is not on the
// branch, and undo refuses until the session starts again.
func (s *Sessions) commitUpdate(ctx context.Context, rec *sessionRecord, ident []string, head, tip string) (string, error) {
	ws := s.ws
	out, err := ws.git(ctx, 4096, "write-tree")
	if err != nil {
		return "", err
	}
	tree := strings.TrimSpace(string(out))
	if !objectIDRe.MatchString(tree) {
		return "", errors.New("git write-tree gave no tree id")
	}
	_, msg := commitMessage("Update from "+rec.StartBranch, "update")
	args := append(slices.Clone(ident), "commit-tree", "-p", head, "-p", tip, "--no-gpg-sign", "-F", "-", tree)
	if out, err = ws.gitInput(ctx, msg, 4096, args...); err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if !objectIDRe.MatchString(id) {
		return "", errors.New("git commit-tree gave no commit id")
	}

	updated := *rec
	updated.LastUpdate = id
	if err := s.writeRecord(&updated); err != nil {
		return "", fmt.Errorf("cannot record the update: %v", pathErrCause(err))
	}
	ref := "refs/heads/" + rec.Branch
	if _, err := ws.git(ctx, 4096, append(slices.Clone(ident), "update-ref", "-m", "repochat: update", ref, id, head)...); err != nil {
		if werr := s.writeRecord(rec); werr != nil && s.owner.Logf != nil {
			s.owner.Logf("update: cannot restore session.json: %v", werr)
		}
		return "", err
	}
	*rec = updated
	if s.rec != nil {
		r := updated
		s.rec = &r
	}
	// The merge is committed: git's record of it in progress goes.
	if out, _, code, err := ws.gitRaw(ctx, 4096, "merge", "--quit"); err != nil || code != 0 {
		if s.owner.Logf != nil {
			s.owner.Logf("update: git merge --quit failed: %v %s", err, firstErr(nil, out))
		}
		for _, f := range []string{"MERGE_HEAD", "MERGE_MSG", "MERGE_MODE", "AUTO_MERGE"} {
			os.Remove(filepath.Join(ws.GitDir, f))
		}
	}
	return id, nil
}

// abortMerge abandons a merge in the workspace and returns it to head, its
// files in line with the policy, and returns cause. When that cannot be
// done, the workspace is closed, so the next call resumes it, which
// restores it, or removed (see sync).
func (s *Sessions) abortMerge(ctx context.Context, rec *sessionRecord, head string, cause error) error {
	ws := s.ws
	if out, _, code, err := ws.gitRaw(ctx, 64<<10, "merge", "--abort"); (err != nil || code != 0) && s.owner.Logf != nil {
		s.owner.Logf("update: git merge --abort failed: %v %s", err, firstErr(nil, out))
	}
	for _, f := range []string{"MERGE_HEAD", "MERGE_MSG", "MERGE_MODE", "AUTO_MERGE"} {
		os.Remove(filepath.Join(ws.GitDir, f))
	}
	now, _, _ := ws.gitText(ctx, "rev-parse", "--verify", "-q", "refs/heads/"+rec.Branch+"^{commit}")
	var err error
	if now != head {
		err = errors.New("the session branch moved during the update")
	}
	if err == nil {
		err = s.restore(ctx)
	}
	if err == nil {
		err = s.sync(ctx)
	}
	if err != nil {
		if s.owner.Logf != nil {
			s.owner.Logf("update: cleaning up after a failed merge: %v", err)
		}
		s.closeWorkspace()
		return fmt.Errorf("%w; the session workspace could not be returned to its last checkpoint (%v): it is restored on the next call", cause, err)
	}
	return fmt.Errorf("%w (nothing was changed)", cause)
}

// conflicts lists the unmerged paths of the workspace's index, without the
// ones the policy withholds, which are counted.
func (s *Sessions) conflicts(ctx context.Context, pol *Policy) (paths []string, hidden int) {
	out, err := s.ws.git(ctx, 16<<20, "ls-files", "-u", "-z")
	if err != nil {
		return nil, 0
	}
	seen := map[string]bool{}
	var all []string
	for _, tok := range splitNUL(out) {
		_, p, ok := strings.Cut(tok, "\t")
		if ok && p != "" && !seen[p] {
			seen[p] = true
			all = append(all, p)
		}
	}
	_ = pol.Prime(ctx, all)
	for _, p := range all {
		if pol.Check(p) != "" {
			hidden++
			continue
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, hidden
}

// listPaths names paths for a message, at most 20, and counts the rest and
// the withheld ones.
func listPaths(paths []string, hidden int) string {
	const most = 20
	shown := make([]string, 0, min(len(paths), most))
	for i, p := range paths {
		if i == most {
			break
		}
		shown = append(shown, SanitizeLine(p))
	}
	text := strings.Join(shown, ", ")
	if more := len(paths) - len(shown); more > 0 {
		text += fmt.Sprintf(" and %d more", more)
	}
	if hidden > 0 {
		if text != "" {
			text += ", plus "
		}
		text += fmt.Sprintf("%d file(s) the policy withholds", hidden)
	}
	return text
}

// union returns the paths of a and b, each once.
func union(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	var out []string
	for _, l := range [][]string{a, b} {
		for _, p := range l {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// onDisk reports whether anything is at the repository-relative path p
// under root; anything but a clear absence counts as present.
func onDisk(root, p string) bool {
	_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p)))
	return !(err != nil && (errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)))
}

// finishRefused is the error of a Finish that changed nothing.
func finishRefused(format string, a ...any) error {
	return fmt.Errorf("finish refused: "+format+" (nothing was changed)", a...)
}

// Finish merges the session into its start branch in the owner's checkout,
// as a fast-forward to the session's head or, with squash, to one new
// commit holding the session's tree on top of the branch; then removes the
// workspace, the session branch and the record. It refuses, changing
// nothing, when the session has nothing to merge, when the owner's checkout
// is not on the start branch, when the owner's configuration asks for
// signed commits, when the start branch has commits the session lacks, and
// when git would have to overwrite uncommitted changes in the checkout.
func (s *Sessions) Finish(ctx context.Context, squash bool, message string) (*Finished, error) {
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
	owner := s.owner
	head, err := s.present(ctx, rec)
	if err != nil {
		return nil, err
	}

	// 1. Something to merge.
	if head == rec.Base {
		return nil, finishRefused("the session has no checkpoints beyond its start, so there is nothing to merge")
	}
	// 2. The owner's checkout is on the start branch.
	onBranch := func() error {
		branch, err := s.ownerBranch(ctx)
		if err != nil {
			return err
		}
		if branch != rec.StartBranch {
			where := "on a detached HEAD"
			if branch != "" {
				where = "on the branch " + SanitizeLine(branch)
			}
			return finishRefused("the repository's checkout is %s, not on %s, the branch the session started from; the owner has to check out %s first", where, rec.StartBranch, rec.StartBranch)
		}
		return nil
	}
	if err := onBranch(); err != nil {
		return nil, err
	}
	// 3. No signing asked for.
	byHand := fmt.Sprintf("The owner can merge the session by hand in the checkout: git merge --ff-only %s", rec.Branch)
	for _, c := range []struct{ key, why string }{
		{"commit.gpgsign", "asks for signed commits (commit.gpgsign), and repochat signs nothing: it does not land unsigned commits on %s through the tool"},
		{"merge.verifySignatures", "sets merge.verifySignatures, so git would refuse to merge the session's unsigned commits into %s anyway"},
	} {
		v, code, err := owner.gitText(ctx, "config", "--type=bool", "--get", c.key)
		switch {
		case err != nil:
			return nil, err
		case code == 0 && v == "true":
			return nil, finishRefused("the repository's git configuration "+c.why+". %s", rec.StartBranch, byHand)
		case code == 0 && v == "false", code == 1:
		default:
			return nil, finishRefused("%s in the repository's git configuration cannot be read as true or false. %s", c.key, byHand)
		}
	}
	// 4. The session contains the start branch.
	tip, err := s.ownerTip(ctx, rec)
	if err != nil {
		return nil, err
	}
	moved := finishRefused("%s has commits the session lacks; call update_session to merge them into the session first, then finish again", rec.StartBranch)
	if ok, err := owner.isAncestor(ctx, tip, head); err != nil {
		return nil, err
	} else if !ok {
		return nil, moved
	}

	files, later, withheld, err := s.changedFiles(ctx, tip, head)
	if err != nil {
		return nil, err
	}
	land := head
	if squash {
		if land, err = s.squashCommit(ctx, head, tip, message); err != nil {
			return nil, err
		}
	}

	// The owner may have acted since: everything again, right before the
	// merge. git's --ff-only is the final guard.
	if err := owner.beforeWorktreeRead(ctx); err != nil {
		return nil, err
	}
	if err := onBranch(); err != nil {
		return nil, err
	}
	if now, err := s.ownerTip(ctx, rec); err != nil {
		return nil, err
	} else if now != tip {
		return nil, moved
	}
	if ok, err := owner.isAncestor(ctx, tip, land); err != nil {
		return nil, err
	} else if !ok {
		return nil, errors.New("finish refused: the commit to land does not descend from the branch (nothing was changed)")
	}
	out, _, code, err := owner.gitRaw(ctx, 1<<20, "-c", "merge.autoStash=false",
		"merge", "--ff-only", "--no-stat", "--no-verify-signatures", land)
	if err != nil {
		return nil, s.mergeFailed(ctx, tip, err)
	}
	if code != 0 {
		return nil, s.mergeFailed(ctx, tip, s.mergeRefusal(ctx, rec, out))
	}
	now, code, err := owner.gitText(ctx, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	if err != nil || code != 0 || !objectIDRe.MatchString(now) {
		now = land
	}

	fin := &Finished{Commit: now, Squashed: squash, Files: files, RunsLater: later}
	if squash {
		fin.Notes = append(fin.Notes, "The commit is unsigned: repochat runs no signing program.")
	} else {
		fin.Notes = append(fin.Notes, "The commits are unsigned: repochat runs no signing program.")
	}
	if withheld > 0 {
		fin.Notes = append(fin.Notes, fmt.Sprintf("%d changed file(s) that the policy withholds are part of the merge and not listed.", withheld))
	}
	if hooks := s.skippedHooks(ctx); len(hooks) > 0 {
		fin.Notes = append(fin.Notes, fmt.Sprintf("These hooks of the repository did not run, because repochat runs none: %s. Run them by hand if the repository relies on them.", strings.Join(hooks, ", ")))
	}

	// The merge has happened: what fails from here on is a note.
	s.closeWorkspace()
	s.plans = nil
	if err := s.removeWorkspace(ctx, rec.Workspace); err != nil {
		fin.Notes = append(fin.Notes, "The session workspace could not be removed completely; `git worktree prune` clears what is left of its registration.")
	}
	if _, err := owner.git(ctx, 4096, "update-ref", "-m", "repochat: finished", "-d", "refs/heads/"+rec.Branch, head); err != nil {
		fin.Notes = append(fin.Notes, fmt.Sprintf("The session branch %s could not be deleted; delete it with: git branch -D %s", rec.Branch, rec.Branch))
	}
	if err := os.Remove(filepath.Join(s.dir, sessionFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		fin.Notes = append(fin.Notes, "The session record could not be deleted; the next call reports the session as broken, and --new-session starts another.")
	}
	return fin, nil
}

// squashCommit makes one commit with head's tree on top of tip, with the
// given message and the Made-through trailer.
func (s *Sessions) squashCommit(ctx context.Context, head, tip, message string) (string, error) {
	tree, code, err := s.owner.gitText(ctx, "rev-parse", "--verify", "-q", head+"^{tree}")
	if err != nil {
		return "", err
	}
	if code != 0 || !objectIDRe.MatchString(tree) {
		return "", errors.New("cannot read the session's tree")
	}
	text, _ := commitMessage(message, "change")
	msg := []byte(text + "\n\nMade-through: repochat\n")
	args := append(s.identity(ctx), "commit-tree", "-p", tip, "--no-gpg-sign", "-F", "-", tree)
	out, err := s.owner.gitInput(ctx, msg, 4096, args...)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if !objectIDRe.MatchString(id) {
		return "", errors.New("git commit-tree gave no commit id")
	}
	return id, nil
}

// mergeRefusal describes why git refused the fast-forward: the paths whose
// uncommitted changes, or untracked files, it would have overwritten, or
// that the branch moved. git's other messages stay in the local log.
func (s *Sessions) mergeRefusal(ctx context.Context, rec *sessionRecord, stderr []byte) error {
	text := string(stderr)
	if strings.Contains(text, "Not possible to fast-forward") {
		return finishRefused("%s moved while the session was being finished; call update_session, then finish again", rec.StartBranch)
	}
	var paths []string
	for _, l := range strings.Split(text, "\n") {
		if p, ok := strings.CutPrefix(l, "\t"); ok && strings.TrimSpace(p) != "" {
			paths = append(paths, strings.TrimSpace(p))
		}
	}
	if len(paths) == 0 {
		s.owner.gitFailure("merge", stderr)
		return errors.New("finish refused: git refused to fast-forward the branch; the reason is in repochat's local log on the user's machine (nothing was changed)")
	}
	hidden := 0
	var shown []string
	if pol, err := s.ws.Policy(ctx); err == nil {
		_ = pol.Prime(ctx, paths)
		for _, p := range paths {
			if pol.Check(p) != "" {
				hidden++
			} else {
				shown = append(shown, p)
			}
		}
	} else {
		hidden = len(paths)
	}
	sort.Strings(shown)
	return finishRefused("the merge would overwrite changes in the repository's checkout that are not committed (or untracked files): %s. The owner has to commit, stash or move them first", listPaths(shown, hidden))
}

// mergeFailed returns cause for a fast-forward that did not happen, after
// checking that the owner's branch is indeed where it was.
func (s *Sessions) mergeFailed(ctx context.Context, tip string, cause error) error {
	now, _, err := s.owner.gitText(ctx, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	if err == nil && now != tip {
		if s.owner.Logf != nil {
			s.owner.Logf("finish: the merge failed and HEAD is at %s, not %s", now, tip)
		}
		return fmt.Errorf("%v; the repository's HEAD is no longer where it was before the merge: check the checkout with git status", cause)
	}
	return cause
}

// skippedHooks lists the merge hooks the owner's repository has, executable,
// in its hooks directory: they would have run after a merge made by hand.
// The files are only looked at.
func (s *Sessions) skippedHooks(ctx context.Context) []string {
	dir := ""
	// core.hooksPath as the owner configured it: every git command here
	// overrides it on its command line, which is not the owner's setting.
	out, _, code, err := s.owner.gitRaw(ctx, 64<<10, "config", "-z", "--show-origin", "--type=path", "--get-all", "core.hooksPath")
	if err == nil && code == 0 {
		toks := splitNUL(out)
		for i := 0; i+1 < len(toks); i += 2 {
			if toks[i] != "command line:" {
				dir = toks[i+1]
			}
		}
	}
	if dir == "" {
		common, code, err := s.owner.gitText(ctx, "rev-parse", "--git-common-dir")
		if err != nil || code != 0 || common == "" {
			return nil
		}
		if !filepath.IsAbs(common) {
			common = filepath.Join(s.owner.Root, common)
		}
		dir = filepath.Join(common, "hooks")
	} else if !filepath.IsAbs(dir) {
		dir = filepath.Join(s.owner.Root, dir)
	}
	var found []string
	for _, h := range []string{"post-merge", "post-checkout"} {
		fi, err := os.Stat(filepath.Join(dir, h))
		if err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			found = append(found, h)
		}
	}
	return found
}

// Abandon ends the session without merging: the workspace and the record are
// removed; the branch only with deleteBranch.
func (s *Sessions) Abandon(ctx context.Context, deleteBranch bool) error {
	release, err := s.acquire()
	if err != nil {
		return err
	}
	defer release()
	rec, err := s.readRecord()
	if err != nil {
		return err
	}
	if rec == nil {
		s.closeWorkspace()
		return ErrNoSession
	}
	s.closeWorkspace()
	s.plans = nil
	s.removeWorkspace(ctx, rec.Workspace)
	var berr error
	if deleteBranch {
		ref := "refs/heads/" + rec.Branch
		head, code, err := s.owner.gitText(ctx, "rev-parse", "--verify", "-q", ref+"^{commit}")
		switch {
		case err != nil:
			berr = err
		case code == 0 && objectIDRe.MatchString(head):
			_, berr = s.owner.git(ctx, 4096, "update-ref", "-m", "repochat: abandoned", "-d", ref, head)
		}
	}
	if err := os.Remove(filepath.Join(s.dir, sessionFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("the session workspace was removed, but its record could not be deleted: %v", pathErrCause(err))
	}
	if berr != nil {
		return fmt.Errorf("the session was abandoned, but its branch %s could not be deleted (%v); delete it with: git branch -D %s", rec.Branch, berr, rec.Branch)
	}
	return nil
}
