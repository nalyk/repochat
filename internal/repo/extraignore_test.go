package repo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// extraFixture is a repository with tracked a.txt, b.txt and c.txt, the given
// committed .repochatignore, and a second rules file outside it.
func extraFixture(t *testing.T, ignore, extra string) (*Repo, string) {
	t.Helper()
	dir := newDir(t)
	write(t, dir, ".repochatignore", ignore)
	for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
		write(t, dir, f, "x\n")
	}
	commit(t, dir, "c")
	extraPath := filepath.Join(t.TempDir(), "owner.repochatignore")
	if err := os.WriteFile(extraPath, []byte(extra), 0o600); err != nil {
		t.Fatal(err)
	}
	return open(t, dir, Options{ExtraIgnoreFile: extraPath}), extraPath
}

func wantReasons(t *testing.T, s *Snapshot, want map[string]string) {
	t.Helper()
	for path, reason := range want {
		if got := s.Policy.Check(path); got != reason {
			t.Errorf("Check(%q) = %q, want %q", path, got, reason)
		}
		if e, ok := s.Lookup(path); !ok || e.Withheld != reason {
			t.Errorf("snapshot entry %q = %+v, want withheld %q", path, e, reason)
		}
	}
}

func TestEitherIgnoreFileWithholds(t *testing.T) {
	r, _ := extraFixture(t, "a.txt\n", "b.txt\n")
	s, err := r.Snapshot(bg)
	if err != nil {
		t.Fatal(err)
	}
	wantReasons(t, s, map[string]string{"a.txt": ReasonIgnoreFile, "b.txt": ReasonIgnoreFile, "c.txt": ""})
}

func TestExtraIgnoreCannotReinclude(t *testing.T) {
	r, _ := extraFixture(t, "*.txt\n", "!a.txt\n")
	s, err := r.Snapshot(bg)
	if err != nil {
		t.Fatal(err)
	}
	wantReasons(t, s, map[string]string{"a.txt": ReasonIgnoreFile, "b.txt": ReasonIgnoreFile})

	// Nor the other way round.
	r2, _ := extraFixture(t, "!a.txt\n", "*.txt\n")
	s2, err := r2.Snapshot(bg)
	if err != nil {
		t.Fatal(err)
	}
	wantReasons(t, s2, map[string]string{"a.txt": ReasonIgnoreFile, "b.txt": ReasonIgnoreFile})
}

func TestExtraIgnoreIsRereadOnChange(t *testing.T) {
	r, extraPath := extraFixture(t, "", "a.txt\n")
	first, err := r.Snapshot(bg)
	if err != nil {
		t.Fatal(err)
	}
	wantReasons(t, first, map[string]string{"a.txt": ReasonIgnoreFile, "b.txt": "", "c.txt": ""})
	oracle := first.Policy.extraOracle
	if oracle == nil {
		t.Fatal("the extra rules have no oracle")
	}

	// Unchanged rules keep their answers.
	fresh(r)
	same, err := r.Snapshot(bg)
	if err != nil {
		t.Fatal(err)
	}
	if same.Policy.extraOracle != oracle || same.Generation != first.Generation {
		t.Error("unchanged extra rules lost their answers or changed the generation")
	}

	if err := os.WriteFile(extraPath, []byte("b.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(snapshotTTL + 200*time.Millisecond)
	second, err := r.Snapshot(bg)
	if err != nil {
		t.Fatal(err)
	}
	wantReasons(t, second, map[string]string{"a.txt": "", "b.txt": ReasonIgnoreFile, "c.txt": ""})
	if second.Generation == first.Generation || second.Policy.hash == first.Policy.hash {
		t.Error("changed extra rules must give a new policy hash and generation")
	}
	if second.Policy.extraOracle == oracle {
		t.Error("changed extra rules kept the old answers")
	}

	// A missing file is no extra rules.
	if err := os.Remove(extraPath); err != nil {
		t.Fatal(err)
	}
	fresh(r)
	third, err := r.Snapshot(bg)
	if err != nil {
		t.Fatal(err)
	}
	wantReasons(t, third, map[string]string{"a.txt": "", "b.txt": "", "c.txt": ""})
}

func TestUnreadableExtraIgnoreFailsClosed(t *testing.T) {
	r, extraPath := extraFixture(t, "", "")
	if err := os.Remove(extraPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(extraPath, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := r.Snapshot(bg)
	if err == nil {
		t.Fatalf("a directory in place of the extra rules gave a snapshot (%d entries)", len(s.Entries))
	}
	if strings.Contains(err.Error(), extraPath) {
		t.Errorf("the error names the file's absolute path: %v", err)
	}

	// So does a named pipe, without waiting for a writer.
	if err := os.Remove(extraPath); err != nil {
		t.Fatal(err)
	}
	if err := mkfifo(extraPath); err != nil {
		t.Fatal(err)
	}
	fresh(r)
	if _, err := r.Snapshot(bg); err == nil {
		t.Error("a named pipe in place of the extra rules gave a snapshot")
	}

	// Rules that cannot be understood fail the same way.
	if err := os.Remove(extraPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(extraPath, []byte("ok\nx[[:nope:]]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh(r)
	if _, err := r.Snapshot(bg); err == nil {
		t.Error("an extra rule that cannot be understood gave a snapshot")
	}
}

func TestExtraIgnoreMustBeAbsolute(t *testing.T) {
	dir := newDir(t)
	commit(t, dir, "c")
	if r, err := Open(bg, dir, Options{ExtraIgnoreFile: "relative/rules"}); err == nil {
		r.Close()
		t.Fatal("a relative extra ignore file was accepted")
	}
}

func TestAddIgnore(t *testing.T) {
	base, err := NewPolicy("a.txt\n", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := NewPolicy("a.txt\n", nil, nil)
	if err := p.AddIgnore("b.txt\n"); err != nil {
		t.Fatal(err)
	}
	if p.hash == base.hash {
		t.Error("the second rules text does not change the hash")
	}
	q, _ := NewPolicy("a.txt\n", nil, nil)
	q.AddIgnore("c.txt\n")
	if q.hash == p.hash {
		t.Error("different second rules give the same hash")
	}
	if err := p.Prime(bg, []string{"a.txt", "b.txt", "c.txt", "d/b.txt"}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"a.txt": ReasonIgnoreFile, "b.txt": ReasonIgnoreFile, "d/b.txt": ReasonIgnoreFile, "c.txt": ""} {
		if got := p.Check(path); got != want {
			t.Errorf("Check(%q) = %q, want %q", path, got, want)
		}
	}
	// Each source is judged on its own.
	n, _ := NewPolicy("*.txt\n", nil, nil)
	n.AddIgnore("!a.txt\n")
	if got := n.Check("a.txt"); got != ReasonIgnoreFile {
		t.Errorf("a negation in the second source re-included a.txt (%q)", got)
	}
	// Only the second source has rules.
	e, _ := NewPolicy("", nil, nil)
	if err := e.AddIgnore("b.txt\n"); err != nil {
		t.Fatal(err)
	}
	if got := e.Check("b.txt"); got != ReasonIgnoreFile {
		t.Errorf("rules in the second source alone did not withhold b.txt (%q)", got)
	}
	// Text that is not UTF-8, or a line that cannot be understood, is an error.
	for _, bad := range []string{"a\x00b\n", "ok\nx[[:nope:]]\n"} {
		x, _ := NewPolicy("", nil, nil)
		if err := x.AddIgnore(bad); err == nil {
			t.Errorf("AddIgnore(%q) accepted", bad)
		}
	}
}
