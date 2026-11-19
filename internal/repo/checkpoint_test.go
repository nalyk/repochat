package repo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fullID resolves a checkpoint ID (or any revision) in the workspace.
func (c *changeFixture) fullID(t *testing.T, rev string) string {
	t.Helper()
	return gitCmd(t, c.ws.Root, "rev-parse", rev+"^{commit}")
}

func (c *changeFixture) base(t *testing.T) string {
	t.Helper()
	info, err := c.s.Current(bg)
	if err != nil {
		t.Fatal(err)
	}
	return info.Base
}

func (c *changeFixture) checkpoints(t *testing.T) []Checkpoint {
	t.Helper()
	cps, err := c.s.Checkpoints(bg)
	if err != nil {
		t.Fatalf("Checkpoints: %v", err)
	}
	return cps
}

func (c *changeFixture) undo(t *testing.T, id string) *Applied {
	t.Helper()
	a, err := c.s.UndoTo(bg, id)
	if err != nil {
		t.Fatalf("UndoTo(%s): %v", id, err)
	}
	return a
}

func (c *changeFixture) refuseUndo(t *testing.T, id, want string) {
	t.Helper()
	head := c.head(t)
	_, err := c.s.UndoTo(bg, id)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("UndoTo(%q): %v; want an error containing %q", id, err, want)
	}
	if c.head(t) != head {
		t.Fatal("a refused undo moved the branch")
	}
	if got := gitCmd(t, c.ws.Root, "status", "--porcelain", "--untracked-files=all"); got != "" {
		t.Errorf("a refused undo left changes:\n%s", got)
	}
}

func kinds(cps []Checkpoint) string {
	var k []string
	for _, cp := range cps {
		k = append(k, cp.Kind)
	}
	return strings.Join(k, ",")
}

func TestCheckpointsListEveryChangeNewestFirst(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n", ".env": "TOKEN=x\n"}, nil)
	base := c.base(t)
	if cps := c.checkpoints(t); kinds(cps) != "start" || cps[0].ID != base[:12] {
		t.Fatalf("a fresh session: %+v", cps)
	}

	first := c.apply(t, "First", Change{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 1), Text: "A\nA2"})
	// A model writes the message body: a kind trailer there is not believed.
	second := c.apply(t, "Second\n\nRepochat-Kind: update", Change{Op: "create", Path: "c.txt", Text: "c"})
	// A commit made by hand on the session branch, with no trailers, that
	// also changes a withheld file.
	blob := hashObject(t, c.ws.Root, "TOKEN=y\n")
	gitCmd(t, c.ws.Root, "update-index", "--cacheinfo", "100644,"+blob+",.env")
	gitCmd(t, c.ws.Root, "commit", "-q", "-m", "By hand\n\nRepochat-Kind: update")
	hand := c.head(t)
	// The same by hand, with the trailers as the message's last paragraph.
	gitCmd(t, c.ws.Root, "commit", "-q", "--allow-empty", "-m", "Repochat-Kind: update")
	bare := c.head(t)

	cps := c.checkpoints(t)
	if got := kinds(cps); got != "unknown,unknown,change,change,start" {
		t.Fatalf("kinds %s; %+v", got, cps)
	}
	want := []string{bare[:12], hand[:12], second.Checkpoint.ID, first.Checkpoint.ID, base[:12]}
	for i, cp := range cps {
		if cp.ID != want[i] {
			t.Errorf("entry %d is %s, want %s", i, cp.ID, want[i])
		}
		if cp.Time.IsZero() {
			t.Errorf("entry %d has no time", i)
		}
	}
	if cps[3].Message != "First" || strings.Join(cps[3].Files, ",") != "a.txt" || cps[3].Added != 2 || cps[3].Removed != 1 {
		t.Errorf("first: %+v", cps[3])
	}
	if cps[2].Message != "Second\n\nRepochat-Kind: update" || strings.Join(cps[2].Files, ",") != "c.txt" || cps[2].Added != 1 || cps[2].Removed != 0 {
		t.Errorf("second: %+v", cps[2])
	}
	if strings.Contains(cps[2].Message, "Made-through") {
		t.Errorf("the trailers are part of the message: %q", cps[2].Message)
	}
	if cps[1].Message != "By hand\n\nRepochat-Kind: update" || len(cps[1].Files) != 0 || cps[1].Added != 0 {
		t.Errorf("a withheld file is listed in a checkpoint: %+v", cps[1])
	}
	if cps[4].Message != "start" || len(cps[4].Files) != 0 {
		t.Errorf("start: %+v", cps[4])
	}
}

func TestUndoRestoresTheTreeAsANewCheckpoint(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"}, nil)
	base := c.base(t)
	first := c.apply(t, "one", Change{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 1), Text: "A1"})
	c.apply(t, "two", Change{Op: "replace", Path: "b.txt", From: c.anchor(t, "b.txt", 1), Text: "B2\nB3"})
	c.apply(t, "three", Change{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 1), Text: "A3"})
	firstID := c.fullID(t, first.Checkpoint.ID)

	// A unique prefix of seven characters names the checkpoint.
	u := c.undo(t, firstID[:7])
	head := c.head(t)
	if u.Checkpoint.Kind != "undo" || u.Checkpoint.ID != head[:12] {
		t.Errorf("undo checkpoint: %+v", u.Checkpoint)
	}
	if strings.Join(u.Checkpoint.Files, ",") != "a.txt,b.txt" || u.Checkpoint.Added != 2 || u.Checkpoint.Removed != 3 {
		t.Errorf("undo checkpoint: %+v", u.Checkpoint)
	}
	if !strings.Contains(u.Checkpoint.Message, first.Checkpoint.ID) {
		t.Errorf("the undo message does not name its target: %q", u.Checkpoint.Message)
	}
	if got, want := gitCmd(t, c.ws.Root, "rev-parse", "HEAD^{tree}"), gitCmd(t, c.ws.Root, "rev-parse", firstID+"^{tree}"); got != want {
		t.Errorf("HEAD's tree %s, the target's %s", got, want)
	}
	if got := gitCmd(t, c.ws.Root, "rev-list", "--count", base+"..HEAD"); got != "4" {
		t.Errorf("%s commits since the start, want 4: history was rewritten", got)
	}
	if got := gitCmd(t, c.ws.Root, "cat-file", "commit", "HEAD"); !strings.HasSuffix(got, "Made-through: repochat\nRepochat-Kind: undo") {
		t.Errorf("undo commit:\n%s", got)
	}
	if got := kinds(c.checkpoints(t)); got != "undo,change,change,change,start" {
		t.Errorf("kinds %s", got)
	}
	if mustRead(t, filepath.Join(c.ws.Root, "a.txt")) != "A1\n" || mustRead(t, filepath.Join(c.ws.Root, "b.txt")) != "b\n" {
		t.Error("the files on disk are not the target's")
	}
	if got := gitCmd(t, c.ws.Root, "status", "--porcelain", "--untracked-files=all"); got != "" {
		t.Errorf("status:\n%s", got)
	}

	// Undo to the current checkpoint changes nothing.
	again := c.undo(t, head)
	if c.head(t) != head || again.Checkpoint.ID != head[:12] || again.Checkpoint.Kind != "undo" {
		t.Errorf("undo to HEAD: %+v, HEAD moved: %v", again.Checkpoint, c.head(t) != head)
	}
}

func TestUndoOfACreateRemovesTheFileAndOfARemoveBringsItBack(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"}, nil)
	base := c.base(t)
	c.apply(t, "create", Change{Op: "create", Path: "dir/new.txt", Text: "new"})
	removed := c.apply(t, "remove", Change{Op: "remove", Path: "b.txt", FileTag: c.tag(t, "b.txt")})

	c.undo(t, base[:12])
	if exists(filepath.Join(c.ws.Root, "dir")) {
		t.Error("undoing a create left the file (or its directory)")
	}
	if mustRead(t, filepath.Join(c.ws.Root, "b.txt")) != "b\n" {
		t.Error("undoing a remove did not bring the file back")
	}
	if got := gitCmd(t, c.ws.Root, "status", "--porcelain", "--untracked-files=all"); got != "" {
		t.Errorf("status:\n%s", got)
	}

	c.undo(t, removed.Checkpoint.ID)
	if mustRead(t, filepath.Join(c.ws.Root, "dir", "new.txt")) != "new\n" || exists(filepath.Join(c.ws.Root, "b.txt")) {
		t.Error("undoing to the later checkpoint did not restore its files")
	}
	if got := kinds(c.checkpoints(t)); got != "undo,undo,change,change,start" {
		t.Errorf("kinds %s", got)
	}
}

func TestUndoRestoresTheRecordedMode(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"run.sh": "echo hi\n"}, nil)
	base := c.base(t)
	gitCmd(t, c.ws.Root, "update-index", "--chmod=+x", "run.sh")
	if err := os.Chmod(filepath.Join(c.ws.Root, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, c.ws.Root, "commit", "-q", "-m", "executable")
	c.undo(t, base)
	if got := gitCmd(t, c.ws.Root, "ls-tree", "HEAD", "run.sh"); !strings.HasPrefix(got, "100644 ") {
		t.Errorf("mode in the commit: %s", got)
	}
	fi, err := os.Lstat(filepath.Join(c.ws.Root, "run.sh"))
	if err != nil || fi.Mode().Perm()&0o111 != 0 {
		t.Errorf("mode on disk: %v %v", fi.Mode(), err)
	}
	if got := gitCmd(t, c.ws.Root, "status", "--porcelain", "--untracked-files=all"); got != "" {
		t.Errorf("status:\n%s", got)
	}
}

func TestUndoCannotCrossAnUpdate(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	base := c.base(t)
	one := c.apply(t, "one", Change{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 1), Text: "1"})
	two := c.apply(t, "two", Change{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 1), Text: "2"})
	c.apply(t, "three", Change{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 1), Text: "3"})

	// The update boundary, as Update records it.
	rec, err := c.s.readRecord()
	if err != nil || rec == nil {
		t.Fatalf("readRecord: %v", err)
	}
	rec.LastUpdate = c.fullID(t, two.Checkpoint.ID)
	if err := c.s.writeRecord(rec); err != nil {
		t.Fatal(err)
	}

	c.refuseUndo(t, one.Checkpoint.ID, "cannot undo past the update from main")
	c.refuseUndo(t, base, "cannot undo past the update from main")
	c.undo(t, two.Checkpoint.ID)
	if mustRead(t, filepath.Join(c.ws.Root, "a.txt")) != "2\n" {
		t.Error("undo to the update itself did not restore it")
	}
}

func TestUndoToUnknownOrForeignCommitIsRefused(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	c.apply(t, "one", Change{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 1), Text: "1"})

	// A commit of the owner's branch, made after the session started.
	write(t, c.f.dir, "a.txt", "owner\n")
	foreign := commit(t, c.f.dir, "owner's own")
	c.refuseUndo(t, foreign, "not a checkpoint of this session")
	c.refuseUndo(t, foreign[:12], "not a checkpoint of this session")
	c.refuseUndo(t, strings.Repeat("0", 40), "not a checkpoint of this session")
	c.refuseUndo(t, "abc12", "at least 7")
	c.refuseUndo(t, "HEAD~1", "hexadecimal")
	c.refuseUndo(t, "--all", "hexadecimal")
	c.refuseUndo(t, "", "hexadecimal")
}

func TestUndoRefusesPathsASessionCannotWrite(t *testing.T) {
	type tamper func(t *testing.T, c *changeFixture)
	byHand := func(path, content, mode string) tamper {
		return func(t *testing.T, c *changeFixture) {
			blob := hashObject(t, c.ws.Root, content)
			gitCmd(t, c.ws.Root, "update-index", "--add", "--cacheinfo", mode+","+blob+","+path)
			gitCmd(t, c.ws.Root, "commit", "-q", "-m", "by hand")
		}
	}
	cases := []struct {
		name string
		do   tamper
		want string
	}{
		{"withheld", byHand(".env", "TOKEN=y\n", "100644"), "withholds"},
		{"protected", byHand(".gitattributes", "* text\n", "100644"), "protected"},
		{"symlink", byHand("link", "a.txt", "120000"), "symbolic link"},
		{"submodule", func(t *testing.T, c *changeFixture) {
			gitCmd(t, c.ws.Root, "update-index", "--add", "--cacheinfo", "160000,"+c.head(t)+",sub")
			gitCmd(t, c.ws.Root, "commit", "-q", "-m", "by hand")
		}, "submodule"},
		{"skip-worktree", func(t *testing.T, c *changeFixture) {
			c.apply(t, "", Change{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 1), Text: "A"})
			gitCmd(t, c.ws.Root, "update-index", "--skip-worktree", "a.txt")
			if err := os.Remove(filepath.Join(c.ws.Root, "a.txt")); err != nil {
				t.Fatal(err)
			}
		}, "not in the session workspace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newApplyFixture(t, map[string]string{"a.txt": "a\n", ".env": "TOKEN=x\n"}, nil)
			base := c.base(t)
			tc.do(t, c)
			head := c.head(t)
			before := wsState(t, c.ws.Root)
			_, err := c.s.UndoTo(bg, base)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("UndoTo: %v; want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), ".env") {
				t.Errorf("the refusal names a withheld file: %v", err)
			}
			if c.head(t) != head || wsState(t, c.ws.Root) != before {
				t.Error("a refused undo changed the workspace")
			}
		})
	}
}

// A base the index copy cannot even write a tree from (an entry whose
// object is missing) is the index out of step: refused, and the next call
// finds the workspace restored.
func TestUnwritableIndexBaseIsRestoredOnTheNextCall(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "other.txt": "other\n"}, nil)
	head := c.head(t)
	gitCmd(t, c.ws.Root, "update-index", "--cacheinfo", "100644,"+strings.Repeat("1", len(head))+",other.txt")
	_, err := c.s.Apply(bg, "", []Change{{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 1), Text: "A"}}, "x")
	if err == nil || !strings.Contains(err.Error(), "out of step") {
		t.Fatalf("Apply: %v", err)
	}
	if c.head(t) != head || mustRead(t, filepath.Join(c.ws.Root, "a.txt")) != "a\n" {
		t.Fatal("the failed apply left a trace")
	}
	ws, err := c.s.Serve(bg)
	if err != nil {
		t.Fatal(err)
	}
	c.ws = ws
	if got := gitCmd(t, c.ws.Root, "status", "--porcelain", "--untracked-files=all"); got != "" {
		t.Errorf("status:\n%s", got)
	}
	c.apply(t, "y", Change{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 1), Text: "A"})
	if mustRead(t, filepath.Join(c.ws.Root, "a.txt")) != "A\n" {
		t.Error("the apply after the restore did not write")
	}
}

// F4: an undo across a file that became a directory, or a directory that
// became a file, removes first and writes afterwards.
func TestUndoAcrossAFileThatBecameADirectory(t *testing.T) {
	for _, tc := range []struct {
		name, first, second string
		start               map[string]string
	}{
		{"file to directory", "a", "a/b.txt", map[string]string{"a": "file\n", "z.txt": "z\n"}},
		{"directory to file", "a/b.txt", "a", map[string]string{"a/b.txt": "inside\n", "z.txt": "z\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newApplyFixture(t, tc.start, nil)
			base := c.base(t)
			start := wsState(t, c.ws.Root)
			c.apply(t, "remove", Change{Op: "remove", Path: tc.first, FileTag: c.tag(t, tc.first)})
			c.apply(t, "create", Change{Op: "create", Path: tc.second, Text: "new"})
			swapped := wsState(t, c.ws.Root)
			swappedHead := c.head(t)

			// A failure after the files were swapped back puts them as they were.
			c.s.applyFault = func(i int) error {
				if i == 2 {
					return errors.New("injected after the swap")
				}
				return nil
			}
			if _, err := c.s.UndoTo(bg, base); err == nil || !strings.Contains(err.Error(), "injected after the swap") || !strings.Contains(err.Error(), "nothing was changed") {
				t.Fatalf("UndoTo with a failure after the swap: %v", err)
			}
			if got := wsState(t, c.ws.Root); got != swapped {
				t.Errorf("the rollback did not restore the swap:\n%s\nwant:\n%s", got, swapped)
			}
			c.s.applyFault = nil

			a := c.undo(t, base)
			if a.Checkpoint.Kind != "undo" {
				t.Errorf("checkpoint %+v", a.Checkpoint)
			}
			got := wsState(t, c.ws.Root)
			if strings.Replace(got, "HEAD "+c.head(t), "", 1) != strings.Replace(start, "HEAD "+base, "", 1) {
				t.Errorf("the files after the undo:\n%s\nwant those of the start:\n%s", got, start)
			}
			if gitCmd(t, c.ws.Root, "rev-parse", "HEAD^{tree}") != gitCmd(t, c.ws.Root, "rev-parse", base+"^{tree}") {
				t.Error("the undo's tree is not the start's")
			}
			// And forward again, across the same swap.
			c.undo(t, swappedHead)
			if got := wsState(t, c.ws.Root); strings.Replace(got, "HEAD "+c.head(t), "", 1) != strings.Replace(swapped, "HEAD "+swappedHead, "", 1) {
				t.Errorf("the files after undoing the undo:\n%s\nwant:\n%s", got, swapped)
			}
		})
	}
}

// A prefix that names more than one checkpoint, none, one before the latest
// update, and a latest update that is not on the branch.
func TestFindCheckpoint(t *testing.T) {
	id := func(prefix string) string { return prefix + strings.Repeat("0", 40-len(prefix)) }
	chain := []string{id("abcdef1a"), id("abcdef1b"), id("1234567"), id("7654321"), id("ba5e")} // newest first
	base, update := chain[4], chain[2]
	for _, tc := range []struct {
		want, boundary, refusal string
		index                   int
	}{
		{"abcdef1a", base, "", 0},
		{"abcdef1b", base, "", 1},
		{chain[3], base, "", 3},
		{"ba5e000", base, "", 4},
		{"abcdef1", base, "names more than one checkpoint; give more characters", 0},
		{"abcdef", update, "names more than one checkpoint", 0},
		{"fffffff", base, "is not a checkpoint of this session", 0},
		{"1234567", update, "", 2},
		{"7654321", update, "cannot undo past the update from main", 0},
		{"ba5e000", update, "cannot undo past the update from main", 0},
		{"abcdef1a", id("0ff"), "is not on its branch", 0},
		{"fffffff", id("0ff"), "is not on its branch", 0},
	} {
		got, err := findCheckpoint(chain, tc.want, tc.boundary, "main")
		switch {
		case tc.refusal == "" && (err != nil || got != tc.index):
			t.Errorf("findCheckpoint(%s, boundary %s) = %d, %v; want %d", tc.want, tc.boundary[:7], got, err, tc.index)
		case tc.refusal != "" && (err == nil || !strings.Contains(err.Error(), tc.refusal) || !strings.Contains(err.Error(), "undo refused") || !strings.Contains(err.Error(), "nothing was changed")):
			t.Errorf("findCheckpoint(%s, boundary %s) = %d, %v; want a refusal with %q that says nothing was changed", tc.want, tc.boundary[:7], got, err, tc.refusal)
		}
	}
}

// The record names a latest update that is not on the session branch (the
// branch failed to move after the record was written, or the record was
// edited): undo refuses, saying that nothing was changed.
func TestUndoRefusesWhenTheRecordedUpdateIsOffTheBranch(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	base := c.base(t)
	c.replaceLine(t, "a.txt", 1, "A")
	write(t, c.f.dir, "o.txt", "o\n")
	foreign := commit(t, c.f.dir, "owner's own")
	rec, err := c.s.readRecord()
	if err != nil {
		t.Fatal(err)
	}
	rec.LastUpdate = foreign
	if err := c.s.writeRecord(rec); err != nil {
		t.Fatal(err)
	}
	c.refuseUndo(t, base, "is not on its branch")
	c.refuseUndo(t, base, "nothing was changed")
}

// Files the undo would restore were changed in the workspace behind
// repochat's back, on disk or in git's index: refused, nothing changed.
func TestUndoRefusesFilesChangedOutside(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		tamper     func(t *testing.T, ws string)
	}{
		{"edited on disk", "belong to no checkpoint (made outside repochat)", func(t *testing.T, ws string) {
			write(t, ws, "a.txt", "edited by hand\n")
		}},
		{"deleted on disk", "", func(t *testing.T, ws string) {
			if err := os.Remove(filepath.Join(ws, "a.txt")); err != nil {
				t.Fatal(err)
			}
		}},
		{"an untracked file where the undo restores one", "belong to no checkpoint (made outside repochat)", func(t *testing.T, ws string) {
			write(t, ws, "gone.txt", "put here by hand\n")
		}},
		{"index flagged", "is not in the session workspace's git index as the current checkpoint has it (changed outside repochat)", func(t *testing.T, ws string) {
			gitCmd(t, ws, "update-index", "--assume-unchanged", "a.txt")
		}},
		{"staged by hand", "", func(t *testing.T, ws string) {
			write(t, ws, "a.txt", "staged by hand\n")
			gitCmd(t, ws, "add", "a.txt")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "gone.txt": "g\n", "b.txt": "b\n"}, nil)
			base := c.base(t)
			c.apply(t, "change", Change{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 1), Text: "A"},
				Change{Op: "remove", Path: "gone.txt", FileTag: c.tag(t, "gone.txt")})
			head := c.head(t)
			tc.tamper(t, c.ws.Root)
			before := wsState(t, c.ws.Root)
			_, err := c.s.UndoTo(bg, base)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "nothing was changed") {
				t.Fatalf("UndoTo: %v; want %q and that nothing was changed", err, tc.want)
			}
			t.Logf("refused: %v", err)
			if c.head(t) != head || wsState(t, c.ws.Root) != before {
				t.Error("a refused undo changed the workspace")
			}
		})
	}
}

// One undo restores at most maxUndoFiles paths.
func TestUndoRefusesMoreFilesThanTheLimit(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	base := c.base(t)
	// One commit by hand on the session branch, larger than any change set.
	for i := 0; i <= maxUndoFiles; i++ {
		write(t, c.ws.Root, fmt.Sprintf("many/f%04d.txt", i), "x\n")
	}
	gitCmd(t, c.ws.Root, "add", "-A")
	gitCmd(t, c.ws.Root, "commit", "-q", "-m", "by hand")
	head := c.head(t)
	c.refuseUndo(t, base, fmt.Sprintf("it would restore %d files; at most %d are restored at once", maxUndoFiles+1, maxUndoFiles))
	if c.head(t) != head || !exists(filepath.Join(c.ws.Root, "many", "f0000.txt")) {
		t.Error("a refused undo changed the workspace")
	}
	// One file fewer goes through.
	if err := os.Remove(filepath.Join(c.ws.Root, "many", "f0000.txt")); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, c.ws.Root, "commit", "-q", "-a", "-m", "one fewer")
	c.undo(t, base)
	if exists(filepath.Join(c.ws.Root, "many")) {
		t.Error("the undo left files behind")
	}
}
