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
	"time"
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
	files, later, _, err := s.changedFiles(ctx, rec, tip, head)
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
func (s *Sessions) changedFiles(ctx context.Context, rec *sessionRecord, from, to string) (files, later []string, withheld int, err error) {
	diff, err := s.reader().treeDiff(ctx, from, to)
	if err != nil {
		return nil, nil, 0, err
	}
	pol, err := s.policy(ctx, rec)
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
	if s.opts.Observe {
		return nil, errObserving
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
	prev := *rec
	id, err := s.commitUpdate(ctx, rec, ident, head, tip)
	if err != nil {
		return nil, s.abortMerge(ctx, rec, head, err)
	}
	// The plans were validated against the files before the merge.
	s.plans = nil
	if s.afterUpdateMerge != nil {
		s.afterUpdateMerge()
	}
	// Whatever the policy withholds, kept out by git or brought by the
	// merge, leaves the disk here, and the check on disk is the guard: when
	// it fails, the update is undone.
	if err := s.syncFiles(ctx); err != nil {
		if !errors.Is(err, errWithheldOnDisk) {
			return nil, fmt.Errorf("the update was recorded as %s, but the session workspace could not be brought in line with it: %w", id[:12], err)
		}
		return nil, s.undoUpdate(ctx, &prev, id, head)
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

// undoUpdate takes back an update commit id whose files could not be kept
// in line with the policy: the branch returns to head, the record to prev,
// and the workspace to head's files, which sync then checks again (and
// removes the workspace when that fails too).
func (s *Sessions) undoUpdate(ctx context.Context, prev *sessionRecord, id, head string) error {
	ws := s.ws
	refusal := errors.New("update refused: git could not merge without leaving a file the policy withholds in the session workspace, so the update was undone. The owner can merge by hand with git")
	ident := s.identity(ctx)
	if _, err := ws.git(ctx, 4096, append(ident, "update-ref", "-m", "repochat: undo a failed update", "refs/heads/"+prev.Branch, head, id)...); err != nil {
		path := ws.Root
		s.closeWorkspace()
		s.removeWorkspace(ctx, path)
		return fmt.Errorf("%v; the update commit %s could not be taken off the branch either, so the workspace was removed", refusal, id[:12])
	}
	if err := s.writeRecord(prev); err != nil && s.owner.Logf != nil {
		s.owner.Logf("update: cannot restore session.json: %v", err)
	}
	if s.rec != nil {
		r := *prev
		s.rec = &r
	}
	_, rerr := ws.git(ctx, 1<<20, "read-tree", "--reset", "-u", "HEAD")
	if rerr == nil {
		_, rerr = ws.git(ctx, 1<<20, "clean", "-f", "-d", "-q")
	}
	if err := s.sync(ctx); err != nil {
		return fmt.Errorf("%v; %w", refusal, err)
	}
	if rerr != nil {
		s.closeWorkspace() // resuming restores it
	}
	return refusal
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

// checkInTheWay refuses a merge from tip to land that adds a file where the
// owner's checkout already holds something git does not track at tip, or
// where a directory the new file needs is anything but a directory there.
// git's fast-forward refuses an untracked file in its way, but replaces an
// ignored one, or an ignored file or link where it needs a directory,
// without asking; to the owner that is uncommitted work all the same. A
// tracked file the merge itself removes is git's to check.
func (s *Sessions) checkInTheWay(ctx context.Context, rec *sessionRecord, tip, land string) error {
	diff, err := s.reader().treeDiff(ctx, tip, land)
	if err != nil {
		return err
	}
	removed := map[string]bool{}
	for _, d := range diff {
		if d.newMode == "000000" {
			removed[d.path] = true
		}
	}
	seen := map[string]bool{}
	var blocked []string
	for _, d := range diff {
		if d.oldMode != "000000" || d.newMode == "000000" {
			continue
		}
		for _, q := range append(dirsOf(d.path), d.path) {
			if removed[q] {
				break
			}
			fi, err := os.Lstat(filepath.Join(s.owner.Root, filepath.FromSlash(q)))
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			if err == nil && q != d.path && fi.IsDir() {
				continue
			}
			// A directory of tip's files that the merge replaces with a file
			// holds nothing else of the owner's: git removes those files.
			if err == nil && q == d.path && fi.IsDir() && s.onlyRemoved(q, removed) {
				break
			}
			if !seen[q] {
				seen[q] = true
				blocked = append(blocked, q)
			}
			break
		}
	}
	if len(blocked) == 0 {
		return nil
	}
	sort.Strings(blocked)
	pol, err := s.policy(ctx, rec)
	if err != nil {
		return err
	}
	_ = pol.Prime(ctx, blocked) // a failure withholds every name below
	var shown []string
	hidden := 0
	for _, p := range blocked {
		if pol.Check(p) != "" {
			hidden++
		} else {
			shown = append(shown, p)
		}
	}
	return finishRefused("the checkout already holds something where the merge would write a new file: %s. It is not tracked by %s (an untracked or ignored file, a link, or a directory), and git would replace an ignored one without asking. The owner can move it aside and finish again, or merge by hand: git merge --ff-only %s",
		listPaths(shown, hidden), rec.StartBranch, rec.Branch)
}

// onlyRemoved reports whether every entry under the owner's directory rel,
// other than directories, is a file of tip that the merge removes. Anything
// else, or a walk that fails, is the owner's.
func (s *Sessions) onlyRemoved(rel string, removed map[string]bool) bool {
	root := filepath.Join(s.owner.Root, filepath.FromSlash(rel))
	err := filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		r, err := filepath.Rel(s.owner.Root, p)
		if err != nil || !removed[filepath.ToSlash(r)] {
			return errInTheWay
		}
		return nil
	})
	return err == nil
}

var errInTheWay = errors.New("something of the owner's is in the way")

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
// signed commits or sets branch.<start branch>.mergeOptions, when the start
// branch has commits the session lacks, and when git would have to overwrite
// uncommitted changes in the checkout, ignored files included.
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
	// A branch rewritten outside repochat is refused here as everywhere.
	head, _, err := s.chain(ctx, rec)
	if err != nil {
		return nil, err
	}

	// 1. Something to merge.
	if head == rec.Base {
		return nil, finishRefused("the session has no checkpoints beyond its start, so there is nothing to merge")
	}
	// 2. The owner's checkout is on the start branch.
	onBranch := func(ctx context.Context) error {
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
	if err := onBranch(ctx); err != nil {
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
	// git reads branch.<name>.mergeOptions as more options of every merge
	// into that branch, after its command line's: a strategy, --squash,
	// --autostash, --gpg-sign. With any of them the merge below is no longer
	// the fast-forward that lands or changes nothing.
	if err := s.checkMergeOptions(ctx, rec, byHand); err != nil {
		return nil, err
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
	files, later, withheld, err := s.changedFiles(ctx, rec, tip, head)
	if err != nil {
		return nil, err
	}
	// 5. Nothing of the owner's where the merge adds a file.
	if err := s.checkInTheWay(ctx, rec, tip, head); err != nil {
		return nil, err
	}

	// From here on the call's cancellation no longer applies: a merge into
	// the owner's checkout cut off halfway would leave it neither merged nor
	// as it was. What fails before the merge still changes nothing.
	wctx := context.WithoutCancel(ctx)
	land := head
	if squash {
		if land, err = s.squashCommit(wctx, head, tip, message); err != nil {
			return nil, err
		}
	}
	// The owner may have acted since: everything again, right before the
	// merge. git's --ff-only is the final guard.
	if err := owner.beforeWorktreeRead(wctx); err != nil {
		return nil, err
	}
	if err := onBranch(wctx); err != nil {
		return nil, err
	}
	if now, err := s.ownerTip(wctx, rec); err != nil {
		return nil, err
	} else if now != tip {
		return nil, moved
	}
	if ok, err := owner.isAncestor(wctx, tip, land); err != nil {
		return nil, err
	} else if !ok {
		return nil, finishRefused("the commit to land does not descend from %s", rec.StartBranch)
	}
	if err := s.checkInTheWay(wctx, rec, tip, land); err != nil {
		return nil, err
	}
	if err := s.checkMergeOptions(wctx, rec, byHand); err != nil {
		return nil, err
	}
	if ownerIndexLocked(owner) {
		return nil, finishRefused("another git command is running in the repository's checkout (its git index is locked by %s); retry once it has finished, or, if no git command is running, the owner can remove that file", SanitizeLine(ownerIndexLock(owner)))
	}
	before, err := ownerWorktreeState(wctx, owner)
	if err != nil {
		return nil, err
	}

	if s.beforeOwnerMerge != nil {
		s.beforeOwnerMerge()
	}
	mctx := context.WithValue(wctx, gitTimeoutKey{}, ownerMergeTimeout)
	// --no-overwrite-ignore: an ignored file that appears in the merge's way
	// after checkInTheWay makes git refuse instead of replacing it.
	out, _, code, err := owner.gitRaw(mctx, 1<<20, "-c", "merge.autoStash=false", "-c", "commit.gpgsign=false",
		"merge", "--ff-only", "--no-stat", "--no-verify-signatures", "--no-gpg-sign", "--no-overwrite-ignore", land)
	var notes []string
	if err != nil || code != 0 {
		if err != nil && s.owner.Logf != nil {
			s.owner.Logf("finish: git merge --ff-only: %v", err)
		}
		if now, _ := s.ownerTip(wctx, rec); now != land {
			// err == nil: git ran to its own end and said no.
			var refusal error
			if err == nil {
				refusal = s.mergeRefusal(wctx, rec, out)
			}
			return nil, s.failedMerge(wctx, rec, tip, land, before, refusal)
		}
		notes = append(notes, "git reported an error at the end of the merge, but the branch is at the merged commit; check the checkout with git status.")
	}

	// What the merge left: only a branch verifiably at the landed commit
	// lets the session go.
	fin := &Finished{Commit: land, Squashed: squash, Files: files, RunsLater: later}
	now, err := s.ownerTip(wctx, rec)
	switch {
	case err != nil:
		fin.Notes = append(notes, fmt.Sprintf("WARNING: git reported the merge of %s as done, but where %s is now could not be read (%v); the session, its branch and its workspace are kept. Check the checkout with git status and git log.", land, rec.StartBranch, err))
		return fin, nil
	case now != land:
		fin.Commit = now
		fin.Notes = append(notes, fmt.Sprintf("WARNING: git reported the merge as done, but %s is at %s, not at the merged commit %s; the session, its branch and its workspace are kept. Check the checkout with git status and git log.", rec.StartBranch, now, land))
		return fin, nil
	}
	fin.Notes = notes
	keepBranch := false
	switch branch, err := s.ownerBranch(wctx); {
	case err != nil:
		keepBranch = true
		fin.Notes = append(fin.Notes, fmt.Sprintf("WARNING: %s is at the merged commit, but the branch the repository's checkout is on could not be read (%v); the session branch %s is kept.", rec.StartBranch, err, rec.Branch))
	case branch != rec.StartBranch:
		keepBranch = true
		fin.Notes = append(fin.Notes, fmt.Sprintf("WARNING: %s is at the merged commit, but the repository's checkout is no longer on it (another git command switched it during the merge); the session branch %s is kept.", rec.StartBranch, rec.Branch))
	}
	if squash {
		fin.Notes = append(fin.Notes, "The commit is unsigned: repochat runs no signing program.")
	} else {
		fin.Notes = append(fin.Notes, "The commits are unsigned: repochat runs no signing program.")
	}
	if withheld > 0 {
		fin.Notes = append(fin.Notes, fmt.Sprintf("%d changed file(s) that the policy withholds are part of the merge and not listed.", withheld))
	}
	if hooks := s.skippedHooks(wctx); len(hooks) > 0 {
		fin.Notes = append(fin.Notes, fmt.Sprintf("These hooks of the repository did not run, because repochat runs none: %s. Run them by hand if the repository relies on them.", strings.Join(hooks, ", ")))
	}

	// The merge has happened: what fails from here on is a note.
	s.closeWorkspace()
	s.plans = nil
	if err := s.removeWorkspace(wctx, rec.Workspace); err != nil {
		fin.Notes = append(fin.Notes, "The session workspace could not be removed completely; it lies under repochat's state directory.")
	}
	if !keepBranch {
		if note := s.deleteBranch(wctx, rec, head); note != nil {
			fin.Notes = append(fin.Notes, note.Error())
		}
	}
	if err := os.Remove(filepath.Join(s.dir, sessionFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		fin.Notes = append(fin.Notes, "The session record could not be deleted; the next call reports the session as broken, and --new-session starts another.")
	}
	return fin, nil
}

// checkMergeOptions refuses a Finish while branch.<start branch>.mergeOptions
// is set, with any value, in the owner's effective configuration, or while
// that cannot be read.
func (s *Sessions) checkMergeOptions(ctx context.Context, rec *sessionRecord, byHand string) error {
	key := "branch." + rec.StartBranch + ".mergeOptions"
	out, _, code, err := s.owner.gitRaw(ctx, 64<<10, "config", "-z", "--get-all", key)
	switch {
	case err != nil:
		return err
	case code == 1 && len(out) == 0:
		return nil
	case code == 0:
		return finishRefused("the repository's git configuration sets %s, options git adds to every merge into %s (they can make the merge something other than a fast-forward, stash or stage changes, or run a signing program), and repochat merges only as a plain fast-forward. %s, or remove the setting and finish again", key, rec.StartBranch, byHand)
	}
	return finishRefused("whether the repository's git configuration sets %s could not be read. %s", key, byHand)
}

// ownerMergeTimeout bounds the one git command that writes the owner's
// checkout, far beyond any other git command: cutting it off is worse than
// waiting.
const ownerMergeTimeout = 10 * time.Minute

// ownerIndexLocked reports whether git's index lock exists in the owner's
// checkout.
func ownerIndexLocked(owner *Repo) bool {
	_, err := os.Lstat(ownerIndexLock(owner))
	return !errors.Is(err, os.ErrNotExist)
}

// ownerIndexLock is the path of git's index lock for the owner's checkout
// (in its own git directory, which for a linked worktree is not .git).
func ownerIndexLock(owner *Repo) string {
	return filepath.Join(owner.GitDir, "index.lock")
}

// ownerWorktreeState is what git status says of the owner's checkout. It
// writes nothing (GIT_OPTIONAL_LOCKS=0).
func ownerWorktreeState(ctx context.Context, owner *Repo) (string, error) {
	out, err := owner.git(ctx, 64<<20, "status", "--porcelain", "-z", "--untracked-files=all", "--ignore-submodules=dirty")
	return string(out), err
}

// failedMerge reports a fast-forward that did not land. refusal is git's
// refusal as mergeRefusal describes it, nil when git did not run to its own
// end (killed, timed out).
//
// git that ends by itself with a refusal has written nothing: it checks
// every path before it writes the first. With the branch still at tip and no
// index lock left, that refusal is the answer, whatever the checkout's status
// says: the owner may have edited files since it was last read, and the
// recovery commands would put those very files back. A git that was cut off
// changed nothing only when the branch, the lock and the status all say so.
// Everything else is a checkout that may be partly updated, with the owner's
// way back.
func (s *Sessions) failedMerge(ctx context.Context, rec *sessionRecord, tip, land, before string, refusal error) error {
	now, nowErr := s.ownerTip(ctx, rec)
	atTip := nowErr == nil && now == tip && !ownerIndexLocked(s.owner)
	if atTip && refusal != nil {
		return refusal
	}
	status, statusErr := ownerWorktreeState(ctx, s.owner)
	if atTip && statusErr == nil && status == before {
		return finishRefused("git merge --ff-only did not complete; the reason is in repochat's local log on the user's machine")
	}
	cmds := recoveryCommands(tip, land)
	return fmt.Errorf("finish failed, and the repository's checkout may be partly updated: git merge --ff-only did not complete. %s is still at %s; the commit being landed is %s. The session, its branch and its workspace are kept. "+
		"To recover, first, if no git command is running, remove the lock file %s. "+
		"Then, to go back to %s, run these two commands in order, from any directory of the checkout: `%s`, then `%s`. "+
		"To complete the merge instead, run the same two and then `%s`. "+
		"The two do nothing unless the checkout is at %s, and touch only the paths the merge changes, putting each back to that commit's content. "+
		"If one of those files was edited by hand while the merge ran, going back overwrites that edit: save a copy first (git status and git diff show what differs). "+
		"Two kinds of path they leave as they are: one whose staged content already was the landed commit's while the file was edited further, which they name, and a file the merge adds under a name git ignores, which stays on disk",
		rec.StartBranch, tip, land, SanitizeLine(ownerIndexLock(s.owner)),
		tip, cmds[0], cmds[1], cmds[2], tip)
}

// recoveryCommands are the shell command lines the owner runs after a
// fast-forward from tip to land was cut off halfway: all but the last put the
// checkout back to tip, and the last is the merge itself, so that completing
// is going back and then merging.
//
// git fast-forwards over a path that differs between tip and land in two
// cases only: index and file both hold tip's content (git then writes the
// file), or the index already holds land's content (git then leaves the file
// alone, whatever is in it). It writes every file before it writes the index.
// So a path whose index entry is land's while its file differs from the index
// holds the owner's own edit, in every state: it is left alone and named.
// Every other path of the diff held exactly tip's content when the merge
// began, and is put back: the paths land adds are taken out of the index and
// off the disk, the others checked out from tip, whether git was cut off
// before, while or after it wrote the files or the index. An edit made to
// such a file while the merge ran is overwritten with it, which the message
// says.
//
// Each line works from any directory of the checkout (the paths git diff
// prints are relative to its top) and does nothing outside a checkout that is
// at tip. Renames are listed as a deletion and an addition (--no-renames).
// Each list runs through sh, which does nothing for an empty one, since GNU
// xargs runs its command once even without input. Paths are literal, never
// patterns, and are handled one at a time, so that a file where the other
// commit has a directory is no obstacle. A path land adds is deleted from the
// disk only when git lists it as an untracked file there that no ignore rule
// covers: not while tip's directory still stands in its place, never through
// a link that tip has in its place, behind which the owner's own files lie,
// and never a file git ignores, which may be the owner's own (git's
// fast-forward was told not to replace those, see --no-overwrite-ignore).
func recoveryCommands(tip, land string) []string {
	const top = `cd "$(git rev-parse --show-toplevel)" && `
	own := `if git --literal-pathspecs diff-index --cached --quiet ` + land + ` -- "$p" && ! git --literal-pathspecs diff --no-ext-diff --quiet -- "$p"; then printf "%s\n" "left as it is, with your own uncommitted changes: $p"; continue; fi; `
	step := func(filter, restore string) string {
		return top + `test "$(git rev-parse --is-inside-work-tree),$(git rev-parse HEAD)" = true,` + tip +
			` && git diff -z --no-renames --name-only --diff-filter=` + filter + ` ` + tip + ` ` + land +
			` | xargs -0 sh -c 'test $# -gt 0 || exit 0; for p do ` + own + restore + ` || exit 255; done' sh`
	}
	return []string{
		step("A", `git update-index --force-remove -- "$p" || exit 255; if { test -L "$p" || test -f "$p"; } && test -n "$(git --literal-pathspecs ls-files -o --exclude-standard -- "$p")"; then rm -f -- "$p"; fi`),
		step("DMT", `git --literal-pathspecs checkout --no-overlay `+tip+` -- "$p"`),
		top + "git merge --ff-only " + land,
	}
}

// deleteBranch deletes the session branch, unless a worktree other than the
// session's own (the owner's checkout among them) has it checked out, or
// that cannot be known. The error is the reason it was kept.
func (s *Sessions) deleteBranch(ctx context.Context, rec *sessionRecord, head string) error {
	ref := "refs/heads/" + rec.Branch
	elsewhere, err := s.checkedOutElsewhere(ctx, rec)
	switch {
	case err != nil:
		return fmt.Errorf("The session branch %s is kept: whether it is checked out elsewhere could not be read (%v)", rec.Branch, err)
	case elsewhere:
		return fmt.Errorf("The session branch %s is kept: it is checked out in another worktree or checkout (or being rebased or bisected there)", rec.Branch)
	}
	if _, err := s.owner.git(ctx, 4096, "update-ref", "-m", "repochat: session ended", "-d", ref, head); err != nil {
		return fmt.Errorf("The session branch %s could not be deleted; delete it with: git branch -D %s", rec.Branch, rec.Branch)
	}
	return nil
}

// checkedOutElsewhere reports whether a worktree of the repository other
// than the session's workspace has the session branch checked out.
func (s *Sessions) checkedOutElsewhere(ctx context.Context, rec *sessionRecord) (bool, error) {
	out, err := s.owner.git(ctx, 16<<20, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	want := "branch refs/heads/" + rec.Branch
	path := ""
	for _, l := range strings.Split(string(out), "\n") {
		if p, ok := strings.CutPrefix(l, "worktree "); ok {
			path = p
			continue
		}
		if l != want {
			continue
		}
		if path == "" {
			return false, errors.New("unexpected output from git worktree list")
		}
		if filepath.Clean(path) != rec.Workspace && resolveExisting(path) != rec.Workspace {
			return true, nil
		}
	}
	// A branch being rebased or bisected shows as detached in that list.
	return s.inUseByRebaseOrBisect(ctx, rec)
}

// inUseByRebaseOrBisect reports whether a rebase or a bisect in any
// worktree of the repository (the main one and every linked one) names the
// session branch as the branch to return to.
func (s *Sessions) inUseByRebaseOrBisect(ctx context.Context, rec *sessionRecord) (bool, error) {
	common, code, err := s.owner.gitText(ctx, "rev-parse", "--git-common-dir")
	if err != nil {
		return false, err
	}
	if code != 0 || common == "" {
		return false, errors.New("cannot locate the git directory")
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(s.owner.Root, common)
	}
	dirs := []string{common}
	entries, err := os.ReadDir(filepath.Join(common, "worktrees"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(common, "worktrees", e.Name()))
		}
	}
	ref := "refs/heads/" + rec.Branch
	for _, d := range dirs {
		for _, f := range []string{"rebase-merge/head-name", "rebase-apply/head-name", "BISECT_START"} {
			data, err := os.ReadFile(filepath.Join(d, f))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return false, err
			}
			if name := strings.TrimSpace(string(data)); name == ref || name == rec.Branch {
				return true, nil
			}
		}
	}
	return false, nil
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
	if pol, err := s.policy(ctx, rec); err == nil {
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
	// A workspace that is still there keeps the session open: its record is
	// what lets the next call find it again.
	werr := s.removeWorkspace(ctx, rec.Workspace)
	if _, err := os.Lstat(filepath.Dir(rec.Workspace)); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("the session workspace could not be removed (%v); the session stays open, and abandoning it can be tried again", werr)
	}
	var berr error
	if deleteBranch {
		ref := "refs/heads/" + rec.Branch
		head, code, err := s.owner.gitText(ctx, "rev-parse", "--verify", "-q", ref+"^{commit}")
		switch {
		case err != nil:
			berr = err
		case code == 0 && objectIDRe.MatchString(head):
			berr = s.deleteBranch(ctx, rec, head)
		}
	}
	if err := os.Remove(filepath.Join(s.dir, sessionFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("the session workspace was removed, but its record could not be deleted: %v", pathErrCause(err))
	}
	if berr != nil {
		return fmt.Errorf("the session was abandoned, but its branch was not deleted. %v", berr)
	}
	return nil
}
