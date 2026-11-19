package repo

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// writeFixture is a committed repository with an outside directory holding a
// file that no write may reach.
func writeFixture(t *testing.T) (r *Repo, dir, outside string) {
	t.Helper()
	dir = newDir(t)
	write(t, dir, "keep.txt", "keep\n")
	commit(t, dir, "c")
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, outside, "target.txt", "original\n")
	return open(t, dir, Options{}), dir, outside
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func TestWriteAtomicWritesAndCreatesParents(t *testing.T) {
	r, dir, _ := writeFixture(t)
	o := r.newOpener()
	defer o.Close()
	if err := o.WriteAtomic("keep.txt", []byte("changed\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, filepath.Join(dir, "keep.txt")); got != "changed\n" {
		t.Errorf("keep.txt = %q", got)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "keep.txt")); fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", fi.Mode().Perm())
	}
	if err := o.WriteAtomic("new/deep/file.txt", []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, filepath.Join(dir, "new/deep/file.txt")); got != "hi\n" {
		t.Errorf("new file = %q", got)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "new/deep/file.txt")); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", fi.Mode().Perm())
	}
	for _, d := range []string{dir, filepath.Join(dir, "new"), filepath.Join(dir, "new/deep")} {
		for _, n := range names(t, d) {
			if n != ".git" && n != "keep.txt" && n != "new" && n != "deep" && n != "file.txt" {
				t.Errorf("unexpected entry %q in %s", n, d)
			}
		}
	}
}

func TestWriteAtomicRefusesSymlinks(t *testing.T) {
	r, dir, outside := writeFixture(t)
	target := filepath.Join(outside, "target.txt")
	o := r.newOpener()
	defer o.Close()

	// The file itself is a link.
	if err := os.Symlink(target, filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := o.WriteAtomic("link.txt", []byte("pwned\n"), 0o644); err == nil {
		t.Error("wrote through a symlinked file")
	}
	if fi, err := os.Lstat(filepath.Join(dir, "link.txt")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced: %v", err)
	}

	// A parent directory is a link.
	if err := os.Symlink(outside, filepath.Join(dir, "ldir")); err != nil {
		t.Fatal(err)
	}
	if err := o.WriteAtomic("ldir/target.txt", []byte("pwned\n"), 0o644); err == nil {
		t.Error("wrote through a symlinked parent")
	}
	if err := o.WriteAtomic("ldir/new.txt", []byte("pwned\n"), 0o644); err == nil {
		t.Error("created a file through a symlinked parent")
	}

	// A parent is swapped for a link between two writes.
	if err := o.WriteAtomic("sub/target.txt", []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "sub"), filepath.Join(dir, "sub.old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := o.WriteAtomic("sub/target.txt", []byte("pwned\n"), 0o644); err == nil {
		t.Error("wrote through a parent swapped for a symlink")
	}
	if err := o.WriteAtomic("sub/other.txt", []byte("pwned\n"), 0o644); err == nil {
		t.Error("created a file through a parent swapped for a symlink")
	}

	if got := mustRead(t, target); got != "original\n" {
		t.Errorf("the link's target changed: %q", got)
	}
	if got := names(t, outside); len(got) != 1 || got[0] != "target.txt" {
		t.Errorf("the outside directory gained entries: %v", got)
	}
	if got := mustRead(t, filepath.Join(dir, "sub.old/target.txt")); got != "first\n" {
		t.Errorf("the moved directory's file changed: %q", got)
	}
}

func TestWriteAtomicLeavesNoTempFileOnFailure(t *testing.T) {
	r, dir, _ := writeFixture(t)
	write(t, dir, "adir/inside.txt", "x\n")
	before := names(t, dir)
	o := r.newOpener()
	defer o.Close()
	// The target is a directory: the content is written to a temporary file
	// first, and the rename cannot happen.
	if err := o.WriteAtomic("adir", []byte("data\n"), 0o644); err == nil {
		t.Fatal("replaced a directory with a file")
	}
	if got := names(t, dir); len(got) != len(before) {
		t.Errorf("entries after a failed write: %v, before: %v", got, before)
	}
	if got := names(t, filepath.Join(dir, "adir")); len(got) != 1 {
		t.Errorf("the directory changed: %v", got)
	}
	// So is a target that is a link, found only after the content is written.
	if err := os.Symlink("keep.txt", filepath.Join(dir, "l2")); err != nil {
		t.Fatal(err)
	}
	before = names(t, dir)
	if err := o.WriteAtomic("l2", []byte("data\n"), 0o644); err == nil {
		t.Fatal("replaced a symlink")
	}
	if got := names(t, dir); len(got) != len(before) {
		t.Errorf("entries after a failed write: %v, before: %v", got, before)
	}
}

func TestRemovePrunesEmptyParents(t *testing.T) {
	r, dir, outside := writeFixture(t)
	write(t, dir, "a/b/c.txt", "x\n")
	write(t, dir, "a/keep.txt", "x\n")
	o := r.newOpener()
	defer o.Close()
	if err := o.Remove("a/b/c.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "a/b")); !os.IsNotExist(err) {
		t.Errorf("the empty directory a/b remains: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "a/keep.txt")); err != nil {
		t.Errorf("a/keep.txt is gone: %v", err)
	}
	if err := o.Remove("a/keep.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "a")); !os.IsNotExist(err) {
		t.Errorf("the empty directory a remains: %v", err)
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Fatalf("the root is gone: %v", err)
	}
	if err := o.Remove("missing.txt"); err == nil {
		t.Error("removing a missing file succeeded")
	}

	// Links are refused, as the file and as a parent.
	if err := os.Symlink(filepath.Join(outside, "target.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := o.Remove("link.txt"); err == nil {
		t.Error("removed a symlink")
	}
	if err := os.Symlink(outside, filepath.Join(dir, "ldir")); err != nil {
		t.Fatal(err)
	}
	if err := o.Remove("ldir/target.txt"); err == nil {
		t.Error("removed a file through a symlinked parent")
	}
	if got := mustRead(t, filepath.Join(outside, "target.txt")); got != "original\n" {
		t.Errorf("the outside file changed: %q", got)
	}
	// A directory is not a file.
	write(t, dir, "d/x.txt", "x\n")
	if err := o.Remove("d"); err == nil {
		t.Error("removed a directory")
	}
}
