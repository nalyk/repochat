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

// shCmd runs a shell command line in dir with the environment gitCmd uses.
func shCmd(t *testing.T, dir, line string) {
	t.Helper()
	cmd := exec.Command("sh", "-c", line)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@example.com",
		"GIT_COMMITTER_NAME=Ada", "GIT_COMMITTER_EMAIL=ada@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sh -c %q: %v\n%s", line, err, out)
	}
}

var backticked = regexp.MustCompile("`([^`]+)`")

func TestPartlyUpdatedCheckoutRecoveryCommandsWork(t *testing.T) {
	type state struct {
		name     string
		addsOnly bool
		// leave builds what a cut-off merge left in the owner's checkout.
		leave func(t *testing.T, dir, tip, land string)
	}
	states := []state{
		{"index at tip", false, func(t *testing.T, dir, tip, land string) {
			write(t, dir, "a.txt", "A\n")
			write(t, dir, "new.txt", "new\n")
			os.Remove(filepath.Join(dir, "gone.txt"))
		}},
		{"index at land", false, func(t *testing.T, dir, tip, land string) {
			gitCmd(t, dir, "read-tree", "-m", "-u", tip, land)
		}},
		{"only additions", true, func(t *testing.T, dir, tip, land string) {
			write(t, dir, "-new.txt", "n\n")
		}},
	}
	for _, st := range states {
		for _, way := range []string{"go back", "complete"} {
			t.Run(st.name+", "+way, func(t *testing.T) {
				c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "-dash.txt": "d\n", "gone.txt": "g\n", "u.txt": "u\n"}, nil)
				want := map[string]string{"a.txt": "a\n", "-dash.txt": "d\n", "gone.txt": "g\n"}
				var added []string
				if st.addsOnly {
					c.apply(t, "adds", Change{Op: "create", Path: "-new.txt", Text: "n"}, Change{Op: "create", Path: "with space.txt", Text: "s"})
					added = []string{"-new.txt", "with space.txt"}
				} else {
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
				write(t, c.f.dir, "u.txt", "owner's edit\n") // unrelated, uncommitted
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
				if len(cmds) != 4 {
					t.Fatalf("want three commands to go back and one more to complete, got %d in:\n%s", len(cmds), msg)
				}
				os.Remove(lock) // "if no git command is running, remove the lock"
				run := cmds[:3]
				if way == "complete" {
					run = cmds
				}
				for _, m := range run {
					shCmd(t, c.f.dir, m[1])
				}

				if got := gitCmd(t, c.f.dir, "symbolic-ref", "HEAD"); got != "refs/heads/main" {
					t.Fatalf("HEAD is %s", got)
				}
				if got := gitCmd(t, c.f.dir, "status", "--porcelain", "--untracked-files=all"); got != "M u.txt" {
					t.Errorf("status after %s:\n%s", way, got)
				}
				if got := gitCmd(t, c.f.dir, "diff", "--cached", "--name-only"); got != "" {
					t.Errorf("staged after %s:\n%s", way, got)
				}
				if mustRead(t, filepath.Join(c.f.dir, "u.txt")) != "owner's edit\n" {
					t.Error("the owner's unrelated edit was lost")
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
