package repo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ownerState is everything Finish may change in the owner's checkout: the
// branch and its commit, the index with its flags, git's status, and the
// files given.
func ownerState(t *testing.T, dir string, files ...string) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "HEAD %s %s\n", gitCmd(t, dir, "rev-parse", "--symbolic-full-name", "HEAD"), gitCmd(t, dir, "rev-parse", "HEAD"))
	fmt.Fprintf(&b, "index\n%s\n", gitCmd(t, dir, "ls-files", "-s", "-v"))
	fmt.Fprintf(&b, "status\n%s\n", gitCmd(t, dir, "status", "--porcelain", "--untracked-files=all"))
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(dir, f))
		fmt.Fprintf(&b, "%s %q %v\n", f, data, err)
	}
	return b.String()
}

// replaceLine replaces line n of path with text.
func (c *changeFixture) replaceLine(t *testing.T, path string, n int, text string) *Applied {
	t.Helper()
	return c.apply(t, "edit "+path, Change{Op: "replace", Path: path, From: c.anchor(t, path, n), Text: text})
}

func (c *changeFixture) sessionDone(t *testing.T, info *SessionInfo) {
	t.Helper()
	if exists(info.Workspace) || exists(filepath.Dir(info.Workspace)) {
		t.Errorf("the workspace %s is still there", info.Workspace)
	}
	if strings.Contains(branches(t, c.f.dir), info.Branch) {
		t.Errorf("the session branch is still there: %s", branches(t, c.f.dir))
	}
	if exists(filepath.Join(c.f.state, c.f.owner.ID, sessionFile)) {
		t.Error("session.json is still there")
	}
	if wt := gitCmd(t, c.f.dir, "worktree", "list", "--porcelain"); strings.Contains(wt, info.Workspace) {
		t.Errorf("the worktree is still registered:\n%s", wt)
	}
}

func (c *changeFixture) info(t *testing.T) *SessionInfo {
	t.Helper()
	info, err := c.s.Current(bg)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	return info
}

func TestStatusReportsAheadFilesAndRunsLater(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "run.sh": "#!/bin/sh\necho hi\n", ".env": "TOKEN=x\n"}, func(dir string) {
		if err := os.Chmod(filepath.Join(dir, "run.sh"), 0o755); err != nil {
			t.Fatal(err)
		}
	})
	st, err := c.s.Status(bg)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Ahead != 0 || len(st.Files) != 0 || st.OwnerMoved || st.OwnerBranch != "main" || len(st.RunsLater) != 0 {
		t.Errorf("a fresh session: %+v", st)
	}

	c.replaceLine(t, "a.txt", 1, "A")
	c.replaceLine(t, "run.sh", 2, "echo bye")
	c.apply(t, "build", Change{Op: "create", Path: "sub/Makefile", Text: "all:\n\ttrue"}, Change{Op: "create", Path: "notes.md", Text: "n"})
	st, err = c.s.Status(bg)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Ahead != 3 {
		t.Errorf("Ahead = %d, want 3", st.Ahead)
	}
	if got := strings.Join(st.Files, ","); got != "a.txt,notes.md,run.sh,sub/Makefile" {
		t.Errorf("Files = %s", got)
	}
	if got := strings.Join(st.RunsLater, ","); got != "run.sh,sub/Makefile" {
		t.Errorf("RunsLater = %s", got)
	}
	if st.OwnerMoved || st.OwnerBranch != "main" || st.Info.Branch != c.branch(t) || st.Info.Head != c.head(t) {
		t.Errorf("status: %+v", st)
	}

	// The owner commits on the start branch, changing a withheld file too:
	// the session lacks those commits, and the withheld file is never named.
	write(t, c.f.dir, "owner.txt", "o\n")
	write(t, c.f.dir, ".env", "TOKEN=y\n")
	commit(t, c.f.dir, "owner's own")
	st, err = c.s.Status(bg)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.OwnerMoved {
		t.Error("OwnerMoved is false after the owner committed")
	}
	if got := strings.Join(st.Files, ","); got != "a.txt,notes.md,owner.txt,run.sh,sub/Makefile" {
		t.Errorf("Files = %s", got)
	}

	gitCmd(t, c.f.dir, "checkout", "-q", "--detach")
	if st, err = c.s.Status(bg); err != nil || st.OwnerBranch != "" {
		t.Errorf("detached: %+v, %v", st, err)
	}
	gitCmd(t, c.f.dir, "checkout", "-q", "-b", "other")
	if st, err = c.s.Status(bg); err != nil || st.OwnerBranch != "other" {
		t.Errorf("on another branch: %+v, %v", st, err)
	}
}

func TestFinishFastForwardsTheOwnersBranch(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"}, nil)
	info := c.info(t)
	c.replaceLine(t, "a.txt", 1, "A")
	c.apply(t, "two", Change{Op: "create", Path: "deploy.sh", Text: "echo"}, Change{Op: "replace", Path: "b.txt", From: c.anchor(t, "b.txt", 1), Text: "B"})
	head := c.head(t)

	fin, err := c.s.Finish(bg, false, "")
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if got := gitCmd(t, c.f.dir, "rev-parse", "refs/heads/main"); got != head || fin.Commit != head {
		t.Errorf("main is at %s, Finished.Commit %s; want the session head %s", got, fin.Commit, head)
	}
	if fin.Squashed {
		t.Error("Squashed")
	}
	if got := strings.Join(fin.Files, ","); got != "a.txt,b.txt,deploy.sh" {
		t.Errorf("Files = %s", got)
	}
	if got := strings.Join(fin.RunsLater, ","); got != "deploy.sh" {
		t.Errorf("RunsLater = %s", got)
	}
	if !strings.Contains(strings.Join(fin.Notes, "\n"), "unsigned") {
		t.Errorf("Notes = %q", fin.Notes)
	}
	if mustRead(t, filepath.Join(c.f.dir, "a.txt")) != "A\n" || mustRead(t, filepath.Join(c.f.dir, "b.txt")) != "B\n" || mustRead(t, filepath.Join(c.f.dir, "deploy.sh")) != "echo\n" {
		t.Error("the owner's files were not updated")
	}
	if got := gitCmd(t, c.f.dir, "status", "--porcelain", "--untracked-files=all"); got != "" {
		t.Errorf("owner's status:\n%s", got)
	}
	if got := gitCmd(t, c.f.dir, "symbolic-ref", "HEAD"); got != "refs/heads/main" {
		t.Errorf("owner's HEAD is %s", got)
	}
	if cfg := mustRead(t, filepath.Join(c.f.dir, ".git", "config")); strings.Contains(strings.ToLower(cfg), "sparsecheckout") {
		t.Errorf("the owner's configuration has a sparse checkout:\n%s", cfg)
	}
	if got := gitCmd(t, c.f.dir, "ls-files", "-v"); strings.Contains(got, "S ") {
		t.Errorf("the workspace's sparse checkout reached the owner's index:\n%s", got)
	}
	c.sessionDone(t, info)
}

func TestFinishSquashLandsOneCommit(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	info := c.info(t)
	tip := gitCmd(t, c.f.dir, "rev-parse", "HEAD")
	c.replaceLine(t, "a.txt", 1, "A")
	c.replaceLine(t, "a.txt", 1, "AA")
	head := c.head(t)

	fin, err := c.s.Finish(bg, true, "Squashed work\n\nAll of it.")
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	now := gitCmd(t, c.f.dir, "rev-parse", "HEAD")
	if !fin.Squashed || fin.Commit != now || now == head {
		t.Errorf("Finished %+v; main at %s, session head %s", fin, now, head)
	}
	if got := gitCmd(t, c.f.dir, "rev-list", "--parents", "-n", "1", "HEAD"); got != now+" "+tip {
		t.Errorf("the squashed commit's parents: %s, want only %s", got, tip)
	}
	if gitCmd(t, c.f.dir, "rev-parse", "HEAD^{tree}") != gitCmd(t, c.f.dir, "rev-parse", head+"^{tree}") {
		t.Error("the squashed commit does not hold the session's tree")
	}
	if got := gitCmd(t, c.f.dir, "log", "-1", "--format=%B", "HEAD"); got != "Squashed work\n\nAll of it.\n\nMade-through: repochat" {
		t.Errorf("message %q", got)
	}
	if raw := gitCmd(t, c.f.dir, "cat-file", "commit", "HEAD"); strings.Contains(raw, "gpgsig") || !strings.Contains(raw, "author Ada <ada@example.com>") {
		t.Errorf("commit:\n%s", raw)
	}
	if mustRead(t, filepath.Join(c.f.dir, "a.txt")) != "AA\n" {
		t.Error("a.txt was not updated")
	}
	if got := gitCmd(t, c.f.dir, "status", "--porcelain", "--untracked-files=all"); got != "" {
		t.Errorf("owner's status:\n%s", got)
	}
	c.sessionDone(t, info)

	// Without a message, the default one.
	c2 := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	c2.replaceLine(t, "a.txt", 1, "A")
	if _, err := c2.s.Finish(bg, true, ""); err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, c2.f.dir, "log", "-1", "--format=%B", "HEAD"); got != defaultMessage+"\n\nMade-through: repochat" {
		t.Errorf("message %q", got)
	}
}

// refuseFinish checks that Finish fails with want and changes nothing: the
// owner's checkout and the session are as they were.
func (c *changeFixture) refuseFinish(t *testing.T, squash bool, want string, files ...string) {
	t.Helper()
	before := ownerState(t, c.f.dir, files...)
	info := c.info(t)
	_, err := c.s.Finish(bg, squash, "")
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Finish: %v; want an error containing %q", err, want)
	}
	if after := ownerState(t, c.f.dir, files...); after != before {
		t.Errorf("a refused Finish changed the owner's checkout:\n%s\nwas:\n%s", after, before)
	}
	now := c.info(t)
	if now.Head != info.Head || !exists(info.Workspace) {
		t.Errorf("a refused Finish changed the session: %+v, was %+v", now, info)
	}
	if !strings.Contains(branches(t, c.f.dir), info.Branch) {
		t.Error("a refused Finish deleted the session branch")
	}
}

func TestFinishRefusesWithNothingToFinish(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	c.refuseFinish(t, false, "no checkpoints")
}

func TestFinishRefusesWhenTheOwnersBranchMoved(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	write(t, c.f.dir, "b.txt", "owner\n")
	commit(t, c.f.dir, "owner's own")
	write(t, c.f.dir, "c.txt", "uncommitted\n")
	c.refuseFinish(t, false, "update_session", "a.txt", "b.txt", "c.txt")
	c.refuseFinish(t, true, "update_session", "a.txt", "b.txt", "c.txt")
}

func TestFinishRefusesOnAnotherBranchOrDetachedHead(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	gitCmd(t, c.f.dir, "checkout", "-q", "-b", "other")
	c.refuseFinish(t, false, "main", "a.txt")
	gitCmd(t, c.f.dir, "checkout", "-q", "--detach", "main")
	c.refuseFinish(t, false, "main", "a.txt")
	if gitCmd(t, c.f.dir, "rev-parse", "refs/heads/other") != gitCmd(t, c.f.dir, "rev-parse", "refs/heads/main") {
		t.Error("the other branch moved")
	}

	gitCmd(t, c.f.dir, "checkout", "-q", "main")
	if _, err := c.s.Finish(bg, false, ""); err != nil {
		t.Fatalf("Finish back on main: %v", err)
	}
	if gitCmd(t, c.f.dir, "rev-parse", "refs/heads/other") == gitCmd(t, c.f.dir, "rev-parse", "refs/heads/main") {
		t.Error("main did not move")
	}
}

func TestFinishRefusesToOverwriteUncommittedChanges(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	c.apply(t, "new", Change{Op: "create", Path: "n.txt", Text: "session"})
	write(t, c.f.dir, "a.txt", "owner's edit\n")
	c.refuseFinish(t, false, "a.txt", "a.txt", "b.txt")
	c.refuseFinish(t, true, "a.txt", "a.txt", "b.txt")

	// An untracked file in the way is refused the same way.
	gitCmd(t, c.f.dir, "checkout", "-q", "--", "a.txt")
	write(t, c.f.dir, "n.txt", "owner's own\n")
	c.refuseFinish(t, false, "n.txt", "a.txt", "n.txt")
}

// git's fast-forward replaces an ignored file in its way without asking, and
// an ignored file is the owner's own uncommitted work as much as any other.
func TestFinishRefusesToOverwriteIgnoredFiles(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", ".gitignore": "build/\ncache\nlinked\n"}, nil)
	c.apply(t, "new", Change{Op: "create", Path: "build/out.txt", Text: "session"})
	write(t, c.f.dir, "build/out.txt", "owner's ignored\n")
	c.refuseFinish(t, false, "build/out.txt", "build/out.txt")
	c.refuseFinish(t, true, "build/out.txt", "build/out.txt")

	// An ignored file where the merge needs a directory.
	os.RemoveAll(filepath.Join(c.f.dir, "build"))
	c.apply(t, "deeper", Change{Op: "create", Path: "cache/data.txt", Text: "session"})
	write(t, c.f.dir, "cache", "owner's ignored file\n")
	c.refuseFinish(t, false, "cache", "cache")

	// An ignored symbolic link where the merge needs a directory.
	os.Remove(filepath.Join(c.f.dir, "cache"))
	c.apply(t, "through", Change{Op: "create", Path: "linked/x.txt", Text: "session"})
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(c.f.dir, "linked")); err != nil {
		t.Fatal(err)
	}
	c.refuseFinish(t, false, "linked", "linked/x.txt")
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Errorf("a refused Finish wrote through the owner's link: %v", entries)
	}

	// Ignored files beside the merged ones are left alone.
	os.Remove(filepath.Join(c.f.dir, "linked"))
	write(t, c.f.dir, "build/other.txt", "owner's ignored, beside\n")
	if _, err := c.s.Finish(bg, false, ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if mustRead(t, filepath.Join(c.f.dir, "build/other.txt")) != "owner's ignored, beside\n" || mustRead(t, filepath.Join(c.f.dir, "build/out.txt")) != "session\n" {
		t.Error("the merge did not land beside the owner's ignored file")
	}
}

// A file may take the place of a directory of tracked files, but not of one
// that also holds an ignored file of the owner's.
func TestFinishReplacesADirectoryOnlyWhenNothingElseIsInIt(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"d/y": "y\n", ".gitignore": "*.log\n"}, nil)
	c.apply(t, "no directory", Change{Op: "remove", Path: "d/y", FileTag: c.tag(t, "d/y")})
	c.apply(t, "a file", Change{Op: "create", Path: "d", Text: "file"})
	write(t, c.f.dir, "d/keep.log", "owner's ignored\n")
	c.refuseFinish(t, false, "d", "d/keep.log", "d/y")
	os.Remove(filepath.Join(c.f.dir, "d/keep.log"))
	if _, err := c.s.Finish(bg, false, ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if mustRead(t, filepath.Join(c.f.dir, "d")) != "file\n" {
		t.Error("d did not become a file")
	}
}

func TestFinishKeepsUnrelatedUncommittedChanges(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	write(t, c.f.dir, "b.txt", "owner's edit\n")
	write(t, c.f.dir, "mine.txt", "untracked\n")
	gitCmd(t, c.f.dir, "add", "mine.txt")
	write(t, c.f.dir, "loose.txt", "untracked too\n")

	if _, err := c.s.Finish(bg, false, ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if mustRead(t, filepath.Join(c.f.dir, "a.txt")) != "A\n" {
		t.Error("a.txt was not updated")
	}
	if mustRead(t, filepath.Join(c.f.dir, "b.txt")) != "owner's edit\n" || mustRead(t, filepath.Join(c.f.dir, "loose.txt")) != "untracked too\n" {
		t.Error("the owner's uncommitted changes were lost")
	}
	if got := gitCmd(t, c.f.dir, "status", "--porcelain", "--untracked-files=all"); got != "M b.txt\nA  mine.txt\n?? loose.txt" {
		t.Errorf("owner's status:\n%s", got)
	}
}

func TestFinishRefusesWhenSigningIsRequired(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	gitCmd(t, c.f.dir, "config", "commit.gpgsign", "true")
	c.refuseFinish(t, false, "git merge --ff-only "+c.branch(t), "a.txt")
	c.refuseFinish(t, true, "signed", "a.txt")
	gitCmd(t, c.f.dir, "config", "commit.gpgsign", "false")
	gitCmd(t, c.f.dir, "config", "merge.verifySignatures", "yes")
	c.refuseFinish(t, false, "verifySignatures", "a.txt")
	gitCmd(t, c.f.dir, "config", "--unset", "merge.verifySignatures")
	// tag.gpgsign does not matter.
	gitCmd(t, c.f.dir, "config", "tag.gpgsign", "true")
	if _, err := c.s.Finish(bg, false, ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}
}

func TestFinishRunsNoHook(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "x.dat": "x\n", ".gitattributes": "*.dat filter=evil\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	marker := filepath.Join(t.TempDir(), "marker")
	script := "#!/bin/sh\ntouch " + marker + "\ncat\n"
	hooks := filepath.Join(t.TempDir(), "myhooks")
	if err := os.Mkdir(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"post-merge", "post-checkout", "reference-transaction", "post-index-change", "pre-merge-commit", "post-rewrite"} {
		if err := os.WriteFile(filepath.Join(hooks, h), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prog := filepath.Join(t.TempDir(), "prog.sh")
	if err := os.WriteFile(prog, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, c.f.dir, "config", "core.hooksPath", hooks)
	gitCmd(t, c.f.dir, "config", "filter.evil.smudge", prog)
	gitCmd(t, c.f.dir, "config", "filter.evil.clean", prog)
	gitCmd(t, c.f.dir, "config", "filter.evil.required", "true")
	gitCmd(t, c.f.dir, "config", "diff.external", prog)
	gitCmd(t, c.f.dir, "config", "merge.autoStash", "true")
	write(t, c.f.dir, "loose.txt", "x\n")
	// A filtered file whose timestamp changed: refreshing the index would
	// clean it.
	write(t, c.f.dir, "x.dat", "x\n")
	os.Remove(marker)

	fin, err := c.s.Finish(bg, false, "")
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if exists(marker) {
		t.Fatal("a repository-configured program ran")
	}
	if mustRead(t, filepath.Join(c.f.dir, "a.txt")) != "A\n" {
		t.Error("a.txt was not updated")
	}
	notes := strings.Join(fin.Notes, "\n")
	mustContain(t, notes, "post-merge", "post-checkout", "did not run")
	mustNotContain(t, notes, "reference-transaction", hooks)

	// Without hooks there is no such note.
	c2 := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	c2.replaceLine(t, "a.txt", 1, "A")
	fin, err = c2.s.Finish(bg, false, "")
	if err != nil {
		t.Fatal(err)
	}
	mustNotContain(t, strings.Join(fin.Notes, "\n"), "did not run")
}

func TestFinishDoesNotMaterialiseWithheldFilesDifferently(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", ".env": "TOKEN=x\n", "keys/id.pem": fakePEM}, nil)
	if exists(filepath.Join(c.ws.Root, ".env")) {
		t.Fatal(".env is in the workspace")
	}
	c.replaceLine(t, "a.txt", 1, "A")
	if _, err := c.s.Finish(bg, false, ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if mustRead(t, filepath.Join(c.f.dir, ".env")) != "TOKEN=x\n" || mustRead(t, filepath.Join(c.f.dir, "keys/id.pem")) != fakePEM {
		t.Error("a withheld file changed in the owner's checkout")
	}
	if got := gitCmd(t, c.f.dir, "ls-files", "-v", ".env", "keys/id.pem"); got != "H .env\nH keys/id.pem" {
		t.Errorf("withheld files in the owner's index:\n%s", got)
	}
	if got := gitCmd(t, c.f.dir, "status", "--porcelain", "--untracked-files=all"); got != "" {
		t.Errorf("owner's status:\n%s", got)
	}
}

func TestUpdateMergesTheOwnersBranchAndResyncs(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n", ".env": "TOKEN=x\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	before := c.head(t)

	// Nothing to merge yet.
	u, err := c.s.Update(bg)
	if err != nil {
		t.Fatalf("Update with nothing to merge: %v", err)
	}
	if c.head(t) != before || u.Checkpoint.ID != before[:12] {
		t.Errorf("an update with nothing to merge made a commit: %+v", u.Checkpoint)
	}

	// The owner commits a new file, a new withheld file and a change to a
	// withheld file the workspace leaves out.
	write(t, c.f.dir, "b.txt", "owner\n")
	write(t, c.f.dir, "new.txt", "new\n")
	write(t, c.f.dir, "prod/.env", "TOKEN=prod\n")
	write(t, c.f.dir, ".env", "TOKEN=changed\n")
	tip := commit(t, c.f.dir, "owner's own")

	u, err = c.s.Update(bg)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	head := c.head(t)
	if u.Checkpoint.Kind != "update" || u.Checkpoint.ID != head[:12] {
		t.Errorf("checkpoint %+v, head %s", u.Checkpoint, head)
	}
	if got := strings.Join(u.Checkpoint.Files, ","); got != "b.txt,new.txt" {
		t.Errorf("update files %s", got)
	}
	if got := gitCmd(t, c.ws.Root, "rev-list", "--parents", "-n", "1", "HEAD"); got != head+" "+before+" "+tip {
		t.Errorf("update parents: %s", got)
	}
	if got := gitCmd(t, c.ws.Root, "log", "-1", "--format=%B"); !strings.HasSuffix(got, "\n\nMade-through: repochat\nRepochat-Kind: update") {
		t.Errorf("update message %q", got)
	}
	if mustRead(t, filepath.Join(c.ws.Root, "new.txt")) != "new\n" || mustRead(t, filepath.Join(c.ws.Root, "b.txt")) != "owner\n" || mustRead(t, filepath.Join(c.ws.Root, "a.txt")) != "A\n" {
		t.Error("the workspace does not hold the merged files")
	}
	for _, p := range []string{".env", "prod/.env", "prod"} {
		if exists(filepath.Join(c.ws.Root, p)) {
			t.Errorf("%s is on disk in the workspace", p)
		}
	}
	if got := gitCmd(t, c.ws.Root, "ls-files", "-v", ".env", "prod/.env"); got != "S .env\nS prod/.env" {
		t.Errorf("withheld files in the workspace's index:\n%s", got)
	}
	if got := gitCmd(t, c.ws.Root, "show", "HEAD:.env"); got != "TOKEN=changed" {
		t.Errorf(".env in the update commit: %q", got)
	}
	if got := gitCmd(t, c.ws.Root, "status", "--porcelain", "--untracked-files=all"); got != "" {
		t.Errorf("workspace status:\n%s", got)
	}
	if exists(filepath.Join(c.ws.GitDir, "MERGE_HEAD")) || exists(filepath.Join(c.ws.GitDir, "MERGE_MSG")) {
		t.Error("the merge state is left behind")
	}
	rec, err := c.s.readRecord()
	if err != nil || rec.LastUpdate != head {
		t.Errorf("last_update %q, want %s (%v)", rec.LastUpdate, head, err)
	}
	if got := kinds(c.checkpoints(t)); got != "update,change,start" {
		t.Errorf("checkpoints %s", got)
	}
	st, err := c.s.Status(bg)
	if err != nil || st.OwnerMoved || st.Ahead != 2 {
		t.Errorf("status after the update: %+v, %v", st, err)
	}

	// The session goes on, and finishes as a fast-forward.
	c.replaceLine(t, "new.txt", 1, "newer")
	head = c.head(t)
	if _, err := c.s.Finish(bg, false, ""); err != nil {
		t.Fatalf("Finish after Update: %v", err)
	}
	if gitCmd(t, c.f.dir, "rev-parse", "HEAD") != head || mustRead(t, filepath.Join(c.f.dir, ".env")) != "TOKEN=changed\n" {
		t.Error("the owner's checkout is not at the session head")
	}
}

func TestUpdateConflictIsAbandonedCleanly(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "session")
	write(t, c.f.dir, "a.txt", "owner\n")
	write(t, c.f.dir, "b.txt", "owner\n")
	commit(t, c.f.dir, "owner's own")
	before := wsState(t, c.ws.Root)
	recBefore, _ := c.s.readRecord()

	_, err := c.s.Update(bg)
	if err == nil || !strings.Contains(err.Error(), "a.txt") || strings.Contains(err.Error(), "b.txt") {
		t.Fatalf("Update: %v; want a conflict naming a.txt only", err)
	}
	if after := wsState(t, c.ws.Root); after != before {
		t.Errorf("a conflicted update changed the workspace:\n%s\nwas:\n%s", after, before)
	}
	for _, f := range []string{"MERGE_HEAD", "MERGE_MSG", "MERGE_MODE"} {
		if exists(filepath.Join(c.ws.GitDir, f)) {
			t.Errorf("%s is left behind", f)
		}
	}
	if recAfter, _ := c.s.readRecord(); *recAfter != *recBefore {
		t.Errorf("the record changed: %+v", recAfter)
	}
	// The session still works.
	c.replaceLine(t, "b.txt", 1, "B")
}

func TestUpdateRefusesWithAMergeDriverConfigured(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "1\n2\n3\n4\n5\n", ".gitattributes": "*.txt merge=evil\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "one")
	write(t, c.f.dir, "a.txt", "1\n2\n3\n4\nfive\n")
	commit(t, c.f.dir, "owner's own")
	marker := filepath.Join(t.TempDir(), "marker")
	prog := filepath.Join(t.TempDir(), "driver.sh")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\ntouch "+marker+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, c.f.dir, "config", "merge.evil.driver", prog+" %O %A %B")
	before := wsState(t, c.ws.Root)

	_, err := c.s.Update(bg)
	if err == nil || !strings.Contains(err.Error(), "merge driver") {
		t.Fatalf("Update: %v; want a refusal naming the merge driver", err)
	}
	if exists(marker) {
		t.Fatal("the merge driver ran")
	}
	if after := wsState(t, c.ws.Root); after != before {
		t.Errorf("a refused update changed the workspace:\n%s\nwas:\n%s", after, before)
	}
}

func TestUpdateRunsNoHookOrFilter(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "b.dat": "b\n", ".gitattributes": "*.dat filter=evil\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	write(t, c.f.dir, "b.dat", "owner\n")
	commit(t, c.f.dir, "owner's own")
	marker := filepath.Join(t.TempDir(), "marker")
	script := "#!/bin/sh\ntouch " + marker + "\ncat\n"
	hooks := filepath.Join(c.f.dir, ".git", "hooks")
	for _, h := range []string{"pre-merge-commit", "prepare-commit-msg", "commit-msg", "post-merge", "post-checkout", "reference-transaction", "post-index-change", "post-commit"} {
		if err := os.WriteFile(filepath.Join(hooks, h), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prog := filepath.Join(t.TempDir(), "prog.sh")
	if err := os.WriteFile(prog, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, c.f.dir, "config", "core.hooksPath", hooks)
	gitCmd(t, c.f.dir, "config", "filter.evil.smudge", prog)
	gitCmd(t, c.f.dir, "config", "filter.evil.clean", prog)
	gitCmd(t, c.f.dir, "config", "filter.evil.required", "true")
	gitCmd(t, c.f.dir, "config", "commit.gpgsign", "true")
	gitCmd(t, c.f.dir, "config", "gpg.program", prog)
	gitCmd(t, c.f.dir, "config", "merge.verifySignatures", "true")
	gitCmd(t, c.f.dir, "config", "rerere.enabled", "true")

	if _, err := c.s.Update(bg); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if exists(marker) {
		t.Fatal("a repository-configured program ran")
	}
	if raw := gitCmd(t, c.ws.Root, "cat-file", "commit", "HEAD"); strings.Contains(raw, "gpgsig") {
		t.Errorf("the update is signed:\n%s", raw)
	}
	if mustRead(t, filepath.Join(c.ws.Root, "b.dat")) != "owner\n" {
		t.Error("b.dat was not merged")
	}
}

func TestUndoCannotCrossARealUpdate(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"}, nil)
	one := c.replaceLine(t, "a.txt", 1, "1")
	write(t, c.f.dir, "b.txt", "owner\n")
	commit(t, c.f.dir, "owner's own")
	up, err := c.s.Update(bg)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	two := c.replaceLine(t, "a.txt", 1, "2")
	c.replaceLine(t, "a.txt", 1, "3")

	c.refuseUndo(t, one.Checkpoint.ID, "cannot undo past the update from main")
	c.refuseUndo(t, c.base(t), "cannot undo past the update from main")
	c.undo(t, two.Checkpoint.ID)
	if mustRead(t, filepath.Join(c.ws.Root, "a.txt")) != "2\n" {
		t.Error("undo to a checkpoint after the update did not restore it")
	}
	c.undo(t, up.Checkpoint.ID)
	if mustRead(t, filepath.Join(c.ws.Root, "a.txt")) != "1\n" || mustRead(t, filepath.Join(c.ws.Root, "b.txt")) != "owner\n" {
		t.Error("undo to the update itself did not restore it")
	}
}

func TestAbandonKeepsTheBranchUnlessTold(t *testing.T) {
	f := newSessionFixture(t, map[string]string{"a.txt": "a\n"})
	s, info := f.start(t, Options{})
	ownerBefore := ownerState(t, f.dir, "a.txt")
	if err := s.Abandon(bg, false); err != nil {
		t.Fatalf("Abandon: %v", err)
	}
	if exists(filepath.Dir(info.Workspace)) || exists(filepath.Join(f.state, f.owner.ID, sessionFile)) {
		t.Error("the workspace or session.json is still there")
	}
	if !strings.Contains(branches(t, f.dir), info.Branch) {
		t.Error("the branch was deleted")
	}
	if got := ownerState(t, f.dir, "a.txt"); got != ownerBefore {
		t.Errorf("Abandon changed the owner's checkout:\n%s", got)
	}
	if err := s.Abandon(bg, false); !errors.Is(err, ErrNoSession) {
		t.Errorf("a second Abandon: %v", err)
	}

	info2, err := s.Start(bg, false)
	if err != nil {
		t.Fatal(err)
	}
	if info2.ID == info.ID {
		t.Fatal("the abandoned session was resumed")
	}
	if err := s.Abandon(bg, true); err != nil {
		t.Fatalf("Abandon: %v", err)
	}
	if b := branches(t, f.dir); strings.Contains(b, info2.Branch) || !strings.Contains(b, info.Branch) {
		t.Errorf("branches after Abandon(true): %s", b)
	}
	if exists(filepath.Dir(info2.Workspace)) {
		t.Error("the workspace is still there")
	}

	// A session whose workspace is already gone can still be abandoned.
	s3, info3 := f.start(t, Options{})
	s3.Close()
	if err := os.RemoveAll(filepath.Dir(info3.Workspace)); err != nil {
		t.Fatal(err)
	}
	s4 := f.sessions(t, Options{})
	if err := s4.Abandon(bg, true); err != nil {
		t.Fatalf("Abandon without a workspace: %v", err)
	}
	if strings.Contains(branches(t, f.dir), info3.Branch) || exists(filepath.Join(f.state, f.owner.ID, sessionFile)) {
		t.Error("the session was not abandoned")
	}
	// Its own stale registration is gone too, without a prune.
	if wt := gitCmd(t, f.dir, "worktree", "list", "--porcelain"); strings.Contains(wt, info3.Workspace) {
		t.Errorf("the stale worktree entry is still registered:\n%s", wt)
	}
}

func TestAfterTheEndReadsServeTheOwnerAndWritesRefuse(t *testing.T) {
	for _, end := range []string{"finish", "abandon"} {
		t.Run(end, func(t *testing.T) {
			c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
			c.replaceLine(t, "a.txt", 1, "A")
			// The end comes from another process: a second Sessions.
			other := c.f.sessions(t, Options{})
			var err error
			if end == "finish" {
				_, err = other.Finish(bg, false, "")
			} else {
				err = other.Abandon(bg, false)
			}
			if err != nil {
				t.Fatalf("%s: %v", end, err)
			}
			for _, s := range []*Sessions{c.s, other} {
				r, err := s.Serve(bg)
				if err != nil || r != c.f.owner {
					t.Errorf("Serve = %v, %v; want the owner's repository", r, err)
				}
				checks := map[string]error{}
				_, checks["Apply"] = s.Apply(bg, "", []Change{{Op: "create", Path: "x.txt", Text: "x"}}, "")
				_, checks["UndoTo"] = s.UndoTo(bg, "0123456789")
				_, checks["Checkpoints"] = s.Checkpoints(bg)
				_, checks["Status"] = s.Status(bg)
				_, checks["Update"] = s.Update(bg)
				_, checks["Finish"] = s.Finish(bg, false, "")
				checks["Abandon"] = s.Abandon(bg, true)
				_, checks["Current"] = s.Current(bg)
				for name, err := range checks {
					if !errors.Is(err, ErrNoSession) {
						t.Errorf("%s after the end: %v", name, err)
					}
				}
			}
			if exists(filepath.Join(c.f.dir, "x.txt")) {
				t.Error("a write reached the owner's checkout")
			}
		})
	}
}

func TestFinishCompletesDespiteACancelledCall(t *testing.T) {
	for _, squash := range []bool{false, true} {
		c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
		info := c.info(t)
		c.replaceLine(t, "a.txt", 1, "A")
		ctx, cancel := context.WithCancel(bg)
		c.s.beforeOwnerMerge = cancel // the call is cancelled as the merge starts
		fin, err := c.s.Finish(ctx, squash, "")
		cancel()
		if err != nil {
			t.Fatalf("Finish(squash=%v) with the call cancelled: %v", squash, err)
		}
		if got := gitCmd(t, c.f.dir, "rev-parse", "refs/heads/main"); got != fin.Commit {
			t.Errorf("main at %s, Finished.Commit %s", got, fin.Commit)
		}
		if mustRead(t, filepath.Join(c.f.dir, "a.txt")) != "A\n" {
			t.Error("a.txt was not updated")
		}
		if got := gitCmd(t, c.f.dir, "status", "--porcelain", "--untracked-files=all"); got != "" {
			t.Errorf("owner's status:\n%s", got)
		}
		c.sessionDone(t, info)
	}
}

func TestFinishRefusesWhileTheOwnersIndexIsLocked(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	lock := filepath.Join(c.f.dir, ".git", "index.lock")
	write(t, c.f.dir, ".git/index.lock", "")
	c.refuseFinish(t, false, "nothing was changed", "a.txt")
	if !exists(lock) {
		t.Error("the owner's index.lock was removed")
	}
	os.Remove(lock)
	if _, err := c.s.Finish(bg, false, ""); err != nil {
		t.Fatalf("Finish once the lock is gone: %v", err)
	}
}

func TestFinishSaysWhenTheCheckoutMayBePartlyUpdated(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	info := c.info(t)
	lock := filepath.Join(c.f.dir, ".git", "index.lock")
	// Between the last check and the merge, git's index lock appears and a
	// file changes: what a merge killed halfway leaves behind.
	c.s.beforeOwnerMerge = func() {
		write(t, c.f.dir, ".git/index.lock", "")
		write(t, c.f.dir, "b.txt", "half\n")
	}
	_, err := c.s.Finish(bg, false, "")
	os.Remove(lock)
	if err == nil {
		t.Fatal("Finish succeeded")
	}
	mustContain(t, err.Error(), "partly updated", "index.lock", "git merge --ff-only", "checkout --no-overlay")
	mustNotContain(t, err.Error(), "git reset --merge")
	mustNotContain(t, err.Error(), "nothing was changed")
	now := c.info(t)
	if now.Head != info.Head || !exists(info.Workspace) || !strings.Contains(branches(t, c.f.dir), info.Branch) {
		t.Errorf("the session was not kept: %+v", now)
	}
}

func TestSessionBranchCheckedOutElsewhereIsKept(t *testing.T) {
	// Finish: the session branch is also checked out in another worktree.
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	branch := c.branch(t)
	other := filepath.Join(t.TempDir(), "other")
	gitCmd(t, c.f.dir, "worktree", "add", "-q", "--force", other, branch)
	fin, err := c.s.Finish(bg, false, "")
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	mustContain(t, strings.Join(fin.Notes, "\n"), branch, "checked out")
	if !strings.Contains(branches(t, c.f.dir), branch) {
		t.Error("the session branch checked out elsewhere was deleted")
	}
	if got := gitCmd(t, other, "symbolic-ref", "HEAD"); got != "refs/heads/"+branch {
		t.Errorf("the other worktree is on %s", got)
	}

	// Abandon: the owner checked the branch out in the main checkout.
	f := newSessionFixture(t, map[string]string{"a.txt": "a\n"})
	s, info := f.start(t, Options{})
	s.Close()
	if err := os.RemoveAll(filepath.Dir(info.Workspace)); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, f.dir, "checkout", "-q", "--ignore-other-worktrees", info.Branch)
	s2 := f.sessions(t, Options{})
	err = s2.Abandon(bg, true)
	if err == nil || !strings.Contains(err.Error(), info.Branch) || !strings.Contains(err.Error(), "checked out") {
		t.Fatalf("Abandon: %v", err)
	}
	if got := gitCmd(t, f.dir, "symbolic-ref", "HEAD"); got != "refs/heads/"+info.Branch {
		t.Errorf("the owner's checkout is on %s", got)
	}
	if !strings.Contains(branches(t, f.dir), info.Branch) {
		t.Error("the branch the owner has checked out was deleted")
	}
	if exists(filepath.Join(f.state, f.owner.ID, sessionFile)) {
		t.Error("the session was not abandoned")
	}
}

func TestFinishRefusesARewrittenSessionBranch(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	// The branch rewritten by hand: its commit no longer descends from the
	// session's start along first parents.
	tip := gitCmd(t, c.f.dir, "rev-parse", "HEAD")
	tree := gitCmd(t, c.ws.Root, "rev-parse", "HEAD^{tree}")
	orphan := gitCmd(t, c.ws.Root, "commit-tree", "-m", "rewritten", tree)
	merged := gitCmd(t, c.ws.Root, "commit-tree", "-p", orphan, "-p", tip, "-m", "by hand", tree)
	gitCmd(t, c.ws.Root, "update-ref", "refs/heads/"+c.branch(t), merged)
	c.refuseFinish(t, false, "no longer descends", "a.txt")
}

func TestAbandonKeepsTheRecordWhenTheWorkspaceCannotBeRemoved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	f := newSessionFixture(t, map[string]string{"a.txt": "a\n"})
	s, info := f.start(t, Options{})
	dir := filepath.Dir(info.Workspace)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if err := s.Abandon(bg, true); err == nil {
		t.Fatal("Abandon succeeded with the workspace still there")
	}
	if !exists(filepath.Join(f.state, f.owner.ID, sessionFile)) {
		t.Error("the record was deleted while the workspace remains")
	}
	if !strings.Contains(branches(t, f.dir), info.Branch) {
		t.Error("the branch was deleted while the workspace remains")
	}
}

func TestUpdateDropsPendingPlans(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	plan, err := c.s.Propose(bg, []Change{{Op: "replace", Path: "b.txt", From: c.anchor(t, "b.txt", 1), Text: "B"}})
	if err != nil {
		t.Fatal(err)
	}
	write(t, c.f.dir, "b.txt", "owner\n")
	commit(t, c.f.dir, "owner's own")
	if _, err := c.s.Update(bg); err != nil {
		t.Fatalf("Update: %v", err)
	}
	_, err = c.s.Apply(bg, plan.ID, nil, "")
	if err == nil || !strings.Contains(err.Error(), "unknown or has expired") {
		t.Fatalf("Apply of a plan made before the update: %v", err)
	}
}

func TestUpdateIsUndoneWhenAWithheldFileStaysOnDisk(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", ".env": "TOKEN=x\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	head := c.head(t)
	branch := c.branch(t)
	write(t, c.f.dir, "new.txt", "new\n")
	commit(t, c.f.dir, "owner's own")
	recBefore, _ := c.s.readRecord()
	// What a git that cannot keep a withheld file out of a merge leaves.
	c.s.afterUpdateMerge = func() { write(t, c.ws.Root, ".env", "TOKEN=x\n") }

	_, err := c.s.Update(bg)
	if err == nil || !strings.Contains(err.Error(), "withholds") {
		t.Fatalf("Update: %v", err)
	}
	if got := gitCmd(t, c.f.dir, "rev-parse", "refs/heads/"+branch); got != head {
		t.Errorf("the session branch is at %s, want %s: the update was not undone", got, head)
	}
	if recAfter, _ := c.s.readRecord(); recAfter == nil || *recAfter != *recBefore {
		t.Errorf("the record changed: %+v", recAfter)
	}
	if exists(filepath.Join(c.ws.Root, ".env")) {
		t.Error("the withheld file is still in the workspace")
	}
}

// shOut runs a shell command line in dir with the environment gitCmd uses
// and returns what it printed.
func shOut(dir, line string, env ...string) (string, error) {
	cmd := exec.Command("sh", "-c", line)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(),
		"GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@example.com",
		"GIT_COMMITTER_NAME=Ada", "GIT_COMMITTER_EMAIL=ada@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null"), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// shCmd runs a recovery command line in dir and returns what it printed; it
// must succeed.
func shCmd(t *testing.T, dir, line string) string {
	t.Helper()
	out, err := shOut(dir, line)
	if err != nil {
		t.Fatalf("sh -c %q in %s: %v\n%s", line, dir, err, out)
	}
	return out
}

var backticked = regexp.MustCompile("`([^`]+)`")

func TestPartlyUpdatedCheckoutRecoveryCommandsWork(t *testing.T) {
	type state struct {
		name string
		kind string // what the session changes: work, adds or swap
		// leave builds what a cut-off merge left in the owner's checkout.
		leave func(t *testing.T, dir, tip, land string)
	}
	states := []state{
		{"index at tip", "work", func(t *testing.T, dir, tip, land string) {
			write(t, dir, "a.txt", "A\n")
			write(t, dir, "new.txt", "new\n")
			os.Remove(filepath.Join(dir, "gone.txt"))
		}},
		{"index at land", "work", func(t *testing.T, dir, tip, land string) {
			gitCmd(t, dir, "read-tree", "-m", "-u", tip, land)
		}},
		{"only additions", "adds", func(t *testing.T, dir, tip, land string) {
			write(t, dir, "-new.txt", "n\n")
		}},
		// A file takes the place of a directory, and git was cut off before
		// it wrote anything.
		{"file for directory, nothing written", "swap", func(t *testing.T, dir, tip, land string) {}},
		{"file for directory, index at land", "swap", func(t *testing.T, dir, tip, land string) {
			gitCmd(t, dir, "read-tree", "-m", "-u", tip, land)
		}},
	}
	for _, st := range states {
		for _, way := range []string{"go back", "complete"} {
			for _, from := range []string{".", "docs"} {
				t.Run(st.name+", "+way+", from "+from, func(t *testing.T) {
					c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "-dash.txt": "d\n", "gone.txt": "g\n", "u.txt": "u\n",
						"d/y": "y\n", "docs/keep.txt": "keep\n"}, nil)
					want := map[string]string{"a.txt": "a\n", "-dash.txt": "d\n", "gone.txt": "g\n", "d/y": "y\n"}
					var added []string
					switch st.kind {
					case "adds":
						c.apply(t, "adds", Change{Op: "create", Path: "-new.txt", Text: "n"}, Change{Op: "create", Path: "with space.txt", Text: "s"})
						added = []string{"-new.txt", "with space.txt"}
					case "swap":
						c.apply(t, "no directory", Change{Op: "remove", Path: "d/y", FileTag: c.tag(t, "d/y")})
						c.apply(t, "a file", Change{Op: "create", Path: "d", Text: "file"}, Change{Op: "create", Path: "new.txt", Text: "new"})
						added = []string{"new.txt"}
					default:
						c.apply(t, "work",
							Change{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 1), Text: "A"},
							Change{Op: "replace", Path: "-dash.txt", From: c.anchor(t, "-dash.txt", 1), Text: "D"},
							Change{Op: "create", Path: "new.txt", Text: "new"},
							Change{Op: "create", Path: "with space.txt", Text: "s"},
							Change{Op: "remove", Path: "gone.txt", FileTag: c.tag(t, "gone.txt")})
						added = []string{"new.txt", "with space.txt"}
					}
					tip := gitCmd(t, c.f.dir, "rev-parse", "HEAD")
					land := c.head(t)
					// The owner's own uncommitted work, none of it on a path
					// the merge changes: docs/new.txt only shares its name
					// with a file the merge adds at the top.
					write(t, c.f.dir, "u.txt", "owner's edit\n")
					write(t, c.f.dir, "docs/keep.txt", "owner's edit\n")
					write(t, c.f.dir, "docs/new.txt", "owner's own\n")
					const status = "M docs/keep.txt\n M u.txt\n?? docs/new.txt"
					lock := filepath.Join(c.f.dir, ".git", "index.lock")
					c.s.beforeOwnerMerge = func() {
						st.leave(t, c.f.dir, tip, land)
						write(t, c.f.dir, ".git/index.lock", "") // the merge fails on it
					}
					_, err := c.s.Finish(bg, false, "")
					if err == nil {
						t.Fatal("Finish succeeded")
					}
					msg := err.Error()
					mustContain(t, msg, "partly updated", tip, land, lock)
					cmds := backticked.FindAllStringSubmatch(msg, -1)
					if len(cmds) < 2 || !strings.Contains(cmds[len(cmds)-1][1], "git merge --ff-only "+land) {
						t.Fatalf("want the commands to go back and then the merge, got %d in:\n%s", len(cmds), msg)
					}
					os.Remove(lock) // "if no git command is running, remove the lock"
					run := cmds[:len(cmds)-1]
					if way == "complete" {
						run = cmds
					}
					for i, m := range run {
						// The owner stops at whatever looks like an error:
						// going back prints nothing.
						if out := shCmd(t, filepath.Join(c.f.dir, from), m[1]); out != "" && i < len(cmds)-1 {
							t.Errorf("%s printed:\n%s", m[1], out)
						}
					}

					if got := gitCmd(t, c.f.dir, "symbolic-ref", "HEAD"); got != "refs/heads/main" {
						t.Fatalf("HEAD is %s", got)
					}
					if got := gitCmd(t, c.f.dir, "status", "--porcelain", "--untracked-files=all"); got != status {
						t.Errorf("status after %s:\n%s", way, got)
					}
					if got := gitCmd(t, c.f.dir, "diff", "--cached", "--name-only"); got != "" {
						t.Errorf("staged after %s:\n%s", way, got)
					}
					for p, w := range map[string]string{"u.txt": "owner's edit\n", "docs/keep.txt": "owner's edit\n", "docs/new.txt": "owner's own\n"} {
						if got, _ := os.ReadFile(filepath.Join(c.f.dir, p)); string(got) != w {
							t.Errorf("the owner's own %s is now %q", p, got)
						}
					}
					main := gitCmd(t, c.f.dir, "rev-parse", "refs/heads/main")
					if way == "complete" {
						if main != land {
							t.Errorf("main at %s, want %s", main, land)
						}
						return
					}
					if main != tip {
						t.Errorf("main at %s, want %s", main, tip)
					}
					for p, w := range want {
						if got := mustRead(t, filepath.Join(c.f.dir, p)); got != w {
							t.Errorf("%s = %q, want %q", p, got, w)
						}
					}
					for _, p := range added {
						if exists(filepath.Join(c.f.dir, p)) {
							t.Errorf("%s is still there", p)
						}
					}
				})
			}
		}
	}
}

// checkoutState is every file of a checkout (content, executable bit, link
// target) and its index. Empty directories do not count: git keeps none.
func checkoutState(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		switch {
		case rel == ".git":
			if fi.IsDir() {
				return filepath.SkipDir
			}
		case fi.Mode()&os.ModeSymlink != 0:
			to, _ := os.Readlink(p)
			fmt.Fprintf(&b, "%q -> %q\n", rel, to)
		case !fi.IsDir():
			data, err := os.ReadFile(p)
			fmt.Fprintf(&b, "%q x=%v %q %v\n", rel, fi.Mode()&0o100 != 0, data, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&b, "index\n%s\n", gitCmd(t, dir, "ls-files", "-s"))
	return b.String()
}

// TestRecoveryCommandsMatrix runs the recovery commands against real
// checkouts in every state a cut-off fast-forward can leave: going back must
// give exactly the checkout the owner had before the merge, their own
// uncommitted work included.
func TestRecoveryCommandsMatrix(t *testing.T) {
	link := func(t *testing.T, dir, name, to string) {
		t.Helper()
		if err := os.Symlink(to, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	remove := func(t *testing.T, dir, name string) {
		t.Helper()
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	type kase struct {
		name   string
		tip    func(t *testing.T, dir string) // more files in the old tip
		land   func(t *testing.T, dir string) // what the landed commit changes
		own    func(t *testing.T, dir string) // more uncommitted work of the owner
		half   func(t *testing.T, dir string) // files git was cut off in the middle of
		linked bool                           // the owner's checkout is a linked worktree
		roots  bool                           // also run from the top of the checkout
		// kept names a path the commands must report as left alone;
		// completed is git's status after completing, when it is not the
		// status from before the merge.
		kept, completed string
	}
	plainLand := func(t *testing.T, dir string) {
		write(t, dir, "a.txt", "A\n")
		write(t, dir, "keep.txt", "KEEP\n")
		write(t, dir, "new.txt", "new\n")
		write(t, dir, "sub/deep/n.txt", "n\n")
		remove(t, dir, "gone.txt")
		remove(t, dir, "only")
	}
	plainTip := func(t *testing.T, dir string) {
		write(t, dir, "a.txt", "a\n")
		write(t, dir, "gone.txt", "g\n")
		write(t, dir, "only/x.txt", "x\n")
	}
	plainHalf := func(t *testing.T, dir string) {
		write(t, dir, "a.txt", "") // cut off while writing it
		write(t, dir, "new.txt", "ne")
		remove(t, dir, "sub")               // not written yet
		write(t, dir, "keep.txt", "keep\n") // not written yet
	}
	cases := []kase{
		{name: "plain", tip: plainTip, land: plainLand, half: plainHalf, roots: true},
		{name: "linked worktree", tip: plainTip, land: plainLand, half: plainHalf, linked: true, roots: true},
		{name: "odd names",
			tip: func(t *testing.T, dir string) {
				write(t, dir, "-dash.txt", "d\n")
				write(t, dir, "g*.txt", "g\n")
				write(t, dir, "gx.txt", "gx\n")
				write(t, dir, "line\nbreak.txt", "l\n")
			},
			land: func(t *testing.T, dir string) {
				write(t, dir, "-dash.txt", "D\n")
				write(t, dir, "-n.txt", "n\n")
				write(t, dir, "-rf", "n\n")
				write(t, dir, "sp ace.txt", "s\n")
				write(t, dir, "new\nline.txt", "s\n")
				write(t, dir, "*.txt", "star\n")
				write(t, dir, "[u].txt", "class\n")
				write(t, dir, "?.txt", "any\n")
				write(t, dir, ":colon.txt", "c\n")
				write(t, dir, "docs/*", "c\n")
				write(t, dir, "line\nbreak.txt", "L\n")
				remove(t, dir, "g*.txt")
			},
			half: func(t *testing.T, dir string) {
				write(t, dir, "*.txt", "st")
				write(t, dir, "line\nbreak.txt", "")
				remove(t, dir, "-rf")
			}},
		{name: "file for directory",
			tip: func(t *testing.T, dir string) {
				write(t, dir, "d/y", "y\n")
				write(t, dir, "d/sub/z", "z\n")
			},
			land: func(t *testing.T, dir string) {
				remove(t, dir, "d")
				write(t, dir, "d", "file\n")
			},
			half: func(t *testing.T, dir string) { remove(t, dir, "d") }}, // directory gone, file not written
		{name: "directory for file",
			tip: func(t *testing.T, dir string) { write(t, dir, "d", "file\n") },
			land: func(t *testing.T, dir string) {
				remove(t, dir, "d")
				write(t, dir, "d/y", "y\n")
				write(t, dir, "d/sub/z", "z\n")
			},
			half: func(t *testing.T, dir string) { remove(t, dir, "d/sub") }},
		// The paths the merge adds lie behind a link in the old tip: seen
		// through it they are the owner's own files in docs.
		{name: "directory for link", roots: true,
			tip: func(t *testing.T, dir string) { link(t, dir, "d", "docs") },
			land: func(t *testing.T, dir string) {
				remove(t, dir, "d")
				write(t, dir, "d/new.txt", "n\n")
				write(t, dir, "d/keep.txt", "k\n")
			},
			half: func(t *testing.T, dir string) { remove(t, dir, "d/keep.txt") }},
		{name: "link for directory",
			tip: func(t *testing.T, dir string) {
				write(t, dir, "d/new.txt", "n\n")
				write(t, dir, "d/keep.txt", "k\n")
			},
			land: func(t *testing.T, dir string) {
				remove(t, dir, "d")
				link(t, dir, "d", "docs")
			},
			half: func(t *testing.T, dir string) { remove(t, dir, "d") }},
		{name: "links and modes",
			tip: func(t *testing.T, dir string) {
				write(t, dir, "a.txt", "a\n")
				write(t, dir, "run.sh", "#!/bin/sh\n")
				link(t, dir, "oldlink", "u.txt")
				link(t, dir, "tofile", "docs")
			},
			land: func(t *testing.T, dir string) {
				remove(t, dir, "a.txt")
				link(t, dir, "a.txt", "u.txt")
				link(t, dir, "todocs", "docs")
				remove(t, dir, "oldlink")
				remove(t, dir, "tofile")
				write(t, dir, "tofile", "now a file\n")
				if err := os.Chmod(filepath.Join(dir, "run.sh"), 0o755); err != nil {
					t.Fatal(err)
				}
			}},
		// The owner had staged the very content the merge brings and then
		// kept editing: git merges over that, and the edits are theirs.
		{name: "the owner's own edits on the merge's paths", tip: plainTip, land: plainLand,
			own: func(t *testing.T, dir string) {
				write(t, dir, "a.txt", "A\n")
				write(t, dir, "new.txt", "new\n")
				gitCmd(t, dir, "add", "a.txt", "new.txt")
				write(t, dir, "a.txt", "mine\n")
				write(t, dir, "new.txt", "mine too\n")
			},
			roots:     true,
			kept:      "new.txt",
			completed: "M a.txt\n M docs/keep.txt\n M new.txt\nM  s.txt\n M u.txt\n?? docs/new.txt"},
	}
	states := []string{"nothing written", "files written", "cut off in a file", "index at land"}
	for _, k := range cases {
		for _, st := range states {
			if st == "cut off in a file" && k.half == nil {
				continue
			}
			for _, way := range []string{"go back", "complete"} {
				for _, from := range []string{"docs", "."} {
					if from == "." && !k.roots {
						continue
					}
					t.Run(k.name+", "+st+", "+way+", from "+from, func(t *testing.T) {
						dir := newDir(t)
						gitCmd(t, dir, "config", "user.name", "Ada")
						gitCmd(t, dir, "config", "user.email", "ada@example.com")
						for p, text := range map[string]string{"u.txt": "u\n", "s.txt": "s\n", "keep.txt": "keep\n", "docs/keep.txt": "keep\n"} {
							write(t, dir, p, text)
						}
						if k.tip != nil {
							k.tip(t, dir)
						}
						tip := commit(t, dir, "tip")
						gitCmd(t, dir, "checkout", "-q", "-b", "work")
						k.land(t, dir)
						land := commit(t, dir, "land")
						if k.linked {
							wt := filepath.Join(newDirPath(t), "owner")
							gitCmd(t, dir, "worktree", "add", "-q", wt, "main")
							dir = wt
						} else {
							gitCmd(t, dir, "checkout", "-q", "main")
						}

						// The owner's uncommitted work: edits, a staged edit,
						// and files named like ones the merge touches.
						write(t, dir, "u.txt", "owner's edit\n")
						write(t, dir, "docs/keep.txt", "owner's edit\n")
						write(t, dir, "docs/new.txt", "owner's own\n")
						write(t, dir, "s.txt", "staged\n")
						gitCmd(t, dir, "add", "s.txt")
						if k.own != nil {
							k.own(t, dir)
						}
						before := checkoutState(t, dir)
						status := gitCmd(t, dir, "status", "--porcelain", "--untracked-files=all")

						// What git merge --ff-only does, up to where it was
						// cut off: it writes the files, then the index, then
						// the branch.
						switch st {
						case "files written", "cut off in a file":
							index := gitCmd(t, dir, "rev-parse", "--git-path", "index")
							if !filepath.IsAbs(index) {
								index = filepath.Join(dir, index)
							}
							data, err := os.ReadFile(index)
							if err != nil {
								t.Fatal(err)
							}
							tmp := filepath.Join(t.TempDir(), "index")
							if err := os.WriteFile(tmp, data, 0o644); err != nil {
								t.Fatal(err)
							}
							if out, err := shOut(dir, "git read-tree -m -u "+tip+" "+land, "GIT_INDEX_FILE="+tmp); err != nil {
								t.Fatalf("read-tree: %v\n%s", err, out)
							}
							if st == "cut off in a file" {
								k.half(t, dir)
							}
						case "index at land":
							gitCmd(t, dir, "read-tree", "-m", "-u", tip, land)
						}

						cmds := recoveryCommands(tip, land)
						run := cmds[:len(cmds)-1]
						if way == "complete" {
							run = cmds
						}
						var printed string
						for i, line := range run {
							if out := shCmd(t, filepath.Join(dir, from), line); i < len(cmds)-1 {
								printed += out
							}
						}
						if k.kept == "" && printed != "" {
							t.Errorf("the commands printed:\n%s", printed)
						}
						if k.kept != "" && !strings.Contains(printed, k.kept) {
							t.Errorf("the commands do not say that %s was left alone:\n%s", k.kept, printed)
						}

						if got := gitCmd(t, dir, "symbolic-ref", "HEAD"); got != "refs/heads/main" {
							t.Fatalf("HEAD is %s", got)
						}
						main := gitCmd(t, dir, "rev-parse", "refs/heads/main")
						if way == "go back" {
							if main != tip {
								t.Errorf("main at %s, want %s", main, tip)
							}
							if got := checkoutState(t, dir); got != before {
								t.Errorf("the checkout is not what it was before the merge:\n%s\nwant:\n%s", got, before)
							}
							return
						}
						if main != land {
							t.Errorf("main at %s, want %s", main, land)
						}
						want := status
						if k.completed != "" {
							want = k.completed
						}
						if got := gitCmd(t, dir, "status", "--porcelain", "--untracked-files=all"); got != want {
							t.Errorf("status after completing:\n%s\nwant:\n%s", got, want)
						}
						for p, w := range map[string]string{"u.txt": "owner's edit\n", "docs/keep.txt": "owner's edit\n", "docs/new.txt": "owner's own\n", "s.txt": "staged\n"} {
							if got, _ := os.ReadFile(filepath.Join(dir, p)); string(got) != w {
								t.Errorf("the owner's own %s is now %q", p, got)
							}
						}
						if k.own != nil {
							for p, w := range map[string]string{"a.txt": "mine\n", "new.txt": "mine too\n"} {
								if got, _ := os.ReadFile(filepath.Join(dir, p)); string(got) != w {
									t.Errorf("the owner's own %s is now %q", p, got)
								}
							}
						}
					})
				}
			}
		}
	}
}

// The commands to go back reason from a checkout that was at the old tip:
// anywhere else they must change nothing.
func TestRecoveryCommandsDoNothingElsewhere(t *testing.T) {
	dir := newDir(t)
	gitCmd(t, dir, "config", "user.name", "Ada")
	gitCmd(t, dir, "config", "user.email", "ada@example.com")
	write(t, dir, "a.txt", "a\n")
	write(t, dir, "config", "c\n")
	tip := commit(t, dir, "tip")
	write(t, dir, "a.txt", "A\n")
	write(t, dir, "HEAD", "new\n")
	land := commit(t, dir, "land")
	write(t, dir, "b.txt", "b\n")
	commit(t, dir, "elsewhere")
	write(t, dir, "a.txt", "uncommitted\n")
	write(t, dir, "HEAD", "uncommitted\n")
	before := checkoutState(t, dir)
	head := mustRead(t, filepath.Join(dir, ".git", "HEAD"))
	cmds := recoveryCommands(tip, land)
	for _, from := range []string{".", ".git"} {
		for _, line := range cmds[:len(cmds)-1] {
			if out, err := shOut(filepath.Join(dir, from), line); err == nil {
				t.Errorf("from %s, in a checkout that is not at the old tip, sh -c %q succeeded:\n%s", from, line, out)
			}
		}
	}
	if got := checkoutState(t, dir); got != before {
		t.Errorf("the checkout changed:\n%s\nwant:\n%s", got, before)
	}
	if got := mustRead(t, filepath.Join(dir, ".git", "HEAD")); got != head {
		t.Errorf(".git/HEAD is now %q", got)
	}

	// At the old tip, but run from inside the git directory, where the
	// merge's paths would name git's own files.
	gitCmd(t, dir, "checkout", "-q", "-f", "-B", "main", tip)
	before = checkoutState(t, dir)
	for _, line := range cmds[:len(cmds)-1] {
		if out, err := shOut(filepath.Join(dir, ".git"), line); err == nil {
			t.Errorf("from the git directory, sh -c %q succeeded:\n%s", line, out)
		}
	}
	if got := checkoutState(t, dir); got != before {
		t.Errorf("the checkout changed:\n%s\nwant:\n%s", got, before)
	}
	if got := mustRead(t, filepath.Join(dir, ".git", "HEAD")); got != head {
		t.Errorf(".git/HEAD is now %q", got)
	}
}

// newDirPath is a fresh resolved directory outside any repository.
func newDirPath(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSessionBranchBeingRebasedElsewhereIsKept(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	branch := c.branch(t)
	base := c.base(t)
	other := filepath.Join(t.TempDir(), "other")
	gitCmd(t, c.f.dir, "worktree", "add", "-q", "--force", other, branch)
	// A rebase that stops: the other worktree shows as detached.
	cmd := exec.Command("git", "-c", "core.hooksPath=/dev/null", "rebase", "-q", "--exec", "false", base)
	cmd.Dir = other
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_EDITOR=true",
		"GIT_COMMITTER_NAME=Ada", "GIT_COMMITTER_EMAIL=ada@example.com")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("the rebase did not stop:\n%s", out)
	}
	if wt := gitCmd(t, c.f.dir, "worktree", "list", "--porcelain"); strings.Count(wt, "branch refs/heads/"+branch) != 1 {
		t.Fatalf("expected the rebasing worktree to show as detached:\n%s", wt)
	}
	err := c.s.Abandon(bg, true)
	if err == nil || !strings.Contains(err.Error(), branch) || !strings.Contains(err.Error(), "checked out") {
		t.Fatalf("Abandon: %v", err)
	}
	if !strings.Contains(branches(t, c.f.dir), branch) {
		t.Error("the branch being rebased was deleted")
	}
}

// F3 / C1 / C7a: git reads branch.<name>.mergeOptions as more command-line
// options of the merge, and with them the fast-forward stops being one: it
// stashes and leaves conflict markers, stages without moving the branch, or
// makes a merge commit and runs the signing program. Finish refuses while the
// option is set, and changes nothing.
func TestFinishRefusesWhenMergeOptionsAreSet(t *testing.T) {
	for _, opts := range []string{"--autostash", "--squash", "-s ours --gpg-sign"} {
		t.Run(opts, func(t *testing.T) {
			c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"}, nil)
			c.replaceLine(t, "a.txt", 1, "A")
			marker := filepath.Join(t.TempDir(), "signing-ran")
			prog := filepath.Join(t.TempDir(), "sign-probe")
			if err := os.WriteFile(prog, []byte("#!/bin/sh\n: > "+marker+"\nexit 1\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, c.f.dir, "config", "commit.gpgsign", "false")
			gitCmd(t, c.f.dir, "config", "merge.verifySignatures", "false")
			gitCmd(t, c.f.dir, "config", "gpg.program", prog)
			gitCmd(t, c.f.dir, "config", "branch.main.mergeOptions", opts)
			// The owner's own uncommitted edit of a file the session changed.
			if opts == "--autostash" {
				write(t, c.f.dir, "a.txt", "owner's edit\n")
			}
			refs := func() string {
				return gitCmd(t, c.f.dir, "for-each-ref") + "\nstash: " + gitCmd(t, c.f.dir, "stash", "list")
			}
			before := refs()
			c.refuseFinish(t, false, "mergeOptions", "a.txt", "b.txt")
			c.refuseFinish(t, true, "git merge --ff-only "+c.branch(t), "a.txt", "b.txt")
			if after := refs(); after != before {
				t.Errorf("refs or stashes changed:\n%s\nwas:\n%s", after, before)
			}
			if exists(marker) {
				t.Error("the configured signing program ran")
			}
			for _, f := range []string{"MERGE_HEAD", "SQUASH_MSG", "MERGE_MSG", "AUTO_MERGE"} {
				if exists(filepath.Join(c.f.dir, ".git", f)) {
					t.Errorf("%s was left in the owner's git directory", f)
				}
			}
		})
	}

	// Another branch's options, or none, are no obstacle.
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	gitCmd(t, c.f.dir, "config", "branch.other.mergeOptions", "--squash")
	if _, err := c.s.Finish(bg, false, ""); err != nil {
		t.Fatalf("Finish with another branch's mergeOptions: %v", err)
	}
}

// C7b: an ignored file of the owner's that appears where the merge adds a
// file after the last check is not replaced: git itself refuses.
func TestFinishDoesNotReplaceAnIgnoredFileThatAppearsLate(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", ".gitignore": "new.txt\nbuild/\n"}, nil)
	c.apply(t, "new", Change{Op: "create", Path: "new.txt", Text: "session"}, Change{Op: "create", Path: "build/out.txt", Text: "session"})
	c.s.beforeOwnerMerge = func() {
		write(t, c.f.dir, "new.txt", "owner's ignored, late\n")
		write(t, c.f.dir, "build/out.txt", "owner's ignored, late\n")
	}
	c.refuseFinish(t, false, "nothing was changed", "a.txt")
	for _, p := range []string{"new.txt", "build/out.txt"} {
		if got := mustRead(t, filepath.Join(c.f.dir, p)); got != "owner's ignored, late\n" {
			t.Errorf("the owner's ignored %s was replaced: %q", p, got)
		}
	}
	if exists(filepath.Join(c.f.dir, ".git", "index.lock")) {
		t.Error("an index lock was left")
	}
	// Once the files are out of the way the same session lands.
	c.s.beforeOwnerMerge = nil
	os.Remove(filepath.Join(c.f.dir, "new.txt"))
	os.RemoveAll(filepath.Join(c.f.dir, "build"))
	if _, err := c.s.Finish(bg, false, ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}
}

// F2: git refuses the fast-forward by itself, cleanly, because the owner
// edited one of the merge's files after the last check. Nothing was written,
// so the answer is the refusal, never the recovery commands: "go back" would
// overwrite that very edit.
func TestCleanGitRefusalIsNeverReportedAsPartlyUpdated(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	info := c.info(t)
	tip := gitCmd(t, c.f.dir, "rev-parse", "HEAD")
	c.s.beforeOwnerMerge = func() {
		write(t, c.f.dir, "a.txt", "owner's late edit\n")
		write(t, c.f.dir, "b.txt", "owner's other late edit\n")
	}
	_, err := c.s.Finish(bg, false, "")
	if err == nil {
		t.Fatal("Finish succeeded over the owner's edit")
	}
	t.Logf("refused: %v", err)
	mustContain(t, err.Error(), "finish refused", "a.txt", "nothing was changed")
	mustNotContain(t, err.Error(), "partly updated", "checkout --no-overlay", "`")
	if got := mustRead(t, filepath.Join(c.f.dir, "a.txt")); got != "owner's late edit\n" {
		t.Errorf("the owner's a.txt = %q", got)
	}
	if got := gitCmd(t, c.f.dir, "rev-parse", "refs/heads/main"); got != tip {
		t.Errorf("main moved to %s", got)
	}
	if now := c.info(t); now.Head != info.Head || !exists(info.Workspace) {
		t.Errorf("the session was not kept: %+v", now)
	}
}

// T8(c): the partly-updated message says what "go back" does to an edit made
// while the merge ran, and promises nothing it cannot keep.
func TestPartlyUpdatedMessageWarnsAboutEditsMadeDuringTheMerge(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	lock := filepath.Join(c.f.dir, ".git", "index.lock")
	c.s.beforeOwnerMerge = func() { write(t, c.f.dir, ".git/index.lock", "") }
	_, err := c.s.Finish(bg, false, "")
	os.Remove(lock)
	if err == nil {
		t.Fatal("Finish succeeded")
	}
	mustContain(t, err.Error(), "partly updated", "edited by hand while the merge ran", "overwrites that edit", "save a copy first")
	mustNotContain(t, err.Error(), "is all they undo", "Other uncommitted changes are kept")
}

// T8(b): going back never deletes a file git ignores, even at a path the
// landed commit adds: it may be the owner's own.
func TestGoingBackNeverDeletesAnIgnoredFile(t *testing.T) {
	dir := newDir(t)
	gitCmd(t, dir, "config", "user.name", "Ada")
	gitCmd(t, dir, "config", "user.email", "ada@example.com")
	write(t, dir, ".gitignore", "build/\n*.log\n")
	write(t, dir, "a.txt", "a\n")
	tip := commit(t, dir, "tip")
	gitCmd(t, dir, "checkout", "-q", "-b", "work")
	write(t, dir, "a.txt", "A\n")
	write(t, dir, "plain.txt", "p\n")
	write(t, dir, "build/out.txt", "landed\n")
	write(t, dir, "run.log", "landed\n")
	gitCmd(t, dir, "add", "-f", "build/out.txt", "run.log")
	land := commit(t, dir, "land")
	gitCmd(t, dir, "checkout", "-q", "main")
	// git was cut off after writing a.txt and plain.txt; the ignored files at
	// the paths the commit adds are the owner's own.
	write(t, dir, "a.txt", "A\n")
	write(t, dir, "plain.txt", "p\n")
	write(t, dir, "build/out.txt", "owner's ignored\n")
	write(t, dir, "run.log", "owner's ignored\n")
	cmds := recoveryCommands(tip, land)
	for _, line := range cmds[:len(cmds)-1] {
		shCmd(t, dir, line)
	}
	for _, p := range []string{"build/out.txt", "run.log"} {
		if got, _ := os.ReadFile(filepath.Join(dir, p)); string(got) != "owner's ignored\n" {
			t.Errorf("the owner's ignored %s is now %q", p, got)
		}
	}
	if exists(filepath.Join(dir, "plain.txt")) || mustRead(t, filepath.Join(dir, "a.txt")) != "a\n" {
		t.Error("the merge's own writing was not undone")
	}
	if got := gitCmd(t, dir, "status", "--porcelain", "--untracked-files=all"); got != "" {
		t.Errorf("status after going back:\n%s", got)
	}
}

// F6: the update is committed, and then the workspace cannot be brought in
// line with it. The workspace is closed, so nothing is served or written
// from it as it is; the next call resumes and syncs it.
func TestUpdateWhoseSyncFailsClosesTheWorkspace(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "plain.txt": "p\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	write(t, c.f.dir, ".env", "TOKEN=owner\n")
	write(t, c.f.dir, "b.txt", "owner\n")
	gitCmd(t, c.f.dir, "add", "-f", ".env")
	commit(t, c.f.dir, "owner's own, with a file the policy withholds")
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	c.s.afterUpdateMerge = cancel // the sync that follows fails
	_, err := c.s.Update(ctx)
	if err == nil || !strings.Contains(err.Error(), "was recorded as") {
		t.Fatalf("Update: %v", err)
	}
	if c.s.ws != nil {
		t.Error("the workspace is still open after a sync that failed")
	}
	ws, err := c.s.Serve(bg)
	if err != nil {
		t.Fatalf("Serve after the failed sync: %v", err)
	}
	if exists(filepath.Join(ws.Root, ".env")) {
		t.Error("the withheld file the update brought is in the workspace")
	}
	if got := mustRead(t, filepath.Join(ws.Root, "b.txt")); got != "owner\n" {
		t.Errorf("b.txt = %q", got)
	}
	if got := gitCmd(t, ws.Root, "status", "--porcelain", "--untracked-files=all"); got != "" {
		t.Errorf("workspace status:\n%s", got)
	}
}
