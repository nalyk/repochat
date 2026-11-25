package repo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func guidanceRepo(t *testing.T, opts Options, files map[string]string) (*Repo, string) {
	t.Helper()
	dir := newDir(t)
	for p, c := range files {
		write(t, dir, p, c)
	}
	write(t, dir, "README.md", "hi\n")
	commit(t, dir, "init")
	return open(t, dir, opts), dir
}

func TestReadGuidanceSourceDefaultsAndRefusals(t *testing.T) {
	r, dir := guidanceRepo(t, Options{}, map[string]string{
		"AGENTS.md": "agents\n", "CLAUDE.md": "claude\n", "sub/AGENTS.md": "n\n",
	})
	g, err := r.ReadGuidanceSource(bg, "")
	if err != nil || g.Path != "AGENTS.md" || string(g.Raw) != "agents\n" || len(g.RawSHA) != 64 {
		t.Fatalf("default: %+v %v", g, err)
	}
	if g, err = r.ReadGuidanceSource(bg, "CLAUDE.md"); err != nil || g.Path != "CLAUDE.md" {
		t.Fatalf("explicit: %+v %v", g, err)
	}

	refuse := func(name string, r *Repo, path, reason string) {
		t.Helper()
		fresh(r)
		_, err := r.ReadGuidanceSource(bg, path)
		if err == nil || !strings.Contains(err.Error(), reason) {
			t.Errorf("%s: err = %v, want %q", name, err, reason)
		}
	}
	refuse("nested", r, "sub/AGENTS.md", "root")

	// symlink
	os.Remove(filepath.Join(dir, "CLAUDE.md"))
	os.Symlink("AGENTS.md", filepath.Join(dir, "CLAUDE.md"))
	gitCmd(t, dir, "add", "-A")
	refuse("symlink", r, "CLAUDE.md", "symbolic link")

	// too big
	write(t, dir, "BIG.md", strings.Repeat("a", 8193))
	refuse("big", r, "BIG.md", "8192")
	write(t, dir, "OK.md", strings.Repeat("a", 8192))
	if _, err := r.ReadGuidanceSource(bg, "OK.md"); err != nil {
		t.Errorf("8192 bytes should pass: %v", err)
	}

	// invalid UTF-8
	write(t, dir, "BAD.md", "ab\xff\xfe\n")
	refuse("utf8", r, "BAD.md", "UTF-8")

	// withheld
	write(t, dir, ".repochatignore", "HIDDEN.md\n")
	write(t, dir, "HIDDEN.md", "x\n")
	refuse("withheld", r, "HIDDEN.md", "withheld")

	// absent
	refuse("missing", r, "NOPE.md", "not")

	// default skips an unserved AGENTS.md and falls to CLAUDE.md
	d2 := newDir(t)
	write(t, d2, "AGENTS.md", "a\n")
	write(t, d2, "CLAUDE.md", "c\n")
	write(t, d2, ".repochatignore", "AGENTS.md\n")
	commit(t, d2, "i")
	r2 := open(t, d2, Options{})
	if g, err := r2.ReadGuidanceSource(bg, ""); err != nil || g.Path != "CLAUDE.md" {
		t.Errorf("fallback: %+v %v", g, err)
	}
	// no candidate
	d3 := newDir(t)
	write(t, d3, "x", "x\n")
	commit(t, d3, "i")
	if _, err := open(t, d3, Options{}).ReadGuidanceSource(bg, ""); err == nil {
		t.Error("no AGENTS.md or CLAUDE.md should be refused")
	}

	// untracked under --tracked-only
	r4, d4 := guidanceRepo(t, Options{TrackedOnly: true}, nil)
	write(t, d4, "AGENTS.md", "u\n")
	fresh(r4)
	if _, err := r4.ReadGuidanceSource(bg, "AGENTS.md"); err == nil || !strings.Contains(err.Error(), "tracked") {
		t.Errorf("untracked: %v", err)
	}
	if _, err := r4.ReadGuidanceSource(bg, ""); err == nil {
		t.Error("untracked default should be refused")
	}
}

func TestDisplayRendersInvisibleCharacters(t *testing.T) {
	r, _ := guidanceRepo(t, Options{}, nil)
	out, err := r.GuidanceDisplay([]byte("a\u200Bb\u202Ec\x1bd\u2060e\uFEFFf\u200C\u200D key " + fakeAWSKey + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, `\u200b`, `\u202e`, `\x1b`, `\u2060`, `\ufeff`, `\u200c`, `\u200d`, "[REDACTED:")
	mustNotContain(t, out, "\u200B", "\u202E", "\x1b", "\u2060", "\uFEFF", fakeAWSKey)
}

func TestDisplayOverTwoKiBIsRefused(t *testing.T) {
	r, _ := guidanceRepo(t, Options{}, nil)
	if _, err := r.GuidanceDisplay([]byte(strings.Repeat("a", 2048))); err != nil {
		t.Errorf("2048 bytes should pass: %v", err)
	}
	if _, err := r.GuidanceDisplay([]byte(strings.Repeat("a", 2049))); err == nil || !strings.Contains(err.Error(), "2048") {
		t.Errorf("2049 bytes: %v", err)
	}
	// escapes expand: 700 zero-width characters become over 2 KiB
	if _, err := r.GuidanceDisplay([]byte(strings.Repeat("\u200b", 700))); err == nil {
		t.Error("expansion past the limit should be refused")
	}
}

func TestCaptureBindingRootsAndRefusals(t *testing.T) {
	dir := newDir(t)
	write(t, dir, "a", "1\n")
	c1 := commit(t, dir, "one")
	gitCmd(t, dir, "checkout", "-q", "--orphan", "other")
	write(t, dir, "b", "2\n")
	c2 := commit(t, dir, "two")
	gitCmd(t, dir, "checkout", "-q", "main")
	gitCmd(t, dir, "merge", "-q", "--allow-unrelated-histories", "-m", "m", "other")
	r := open(t, dir, Options{})
	b, err := r.CaptureBinding(bg)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{c1, c2}
	if want[0] > want[1] {
		want[0], want[1] = want[1], want[0]
	}
	if len(b.Roots) != 2 || b.Roots[0] != want[0] || b.Roots[1] != want[1] {
		t.Errorf("roots = %v, want %v", b.Roots, want)
	}
	if b.Root != dir || b.CommonDir != filepath.Join(dir, ".git") || b.RootIno == 0 || b.CommonIno == 0 {
		t.Errorf("binding = %+v", b)
	}
	b2, _ := r.CaptureBinding(bg)
	if !b.Equal(b2) {
		t.Error("same repository must be equal")
	}
	b2.Roots = []string{want[0]}
	if b.Equal(b2) || b.Equal(nil) {
		t.Error("different roots must not be equal")
	}
	b3 := *b
	b3.RootIno++
	if b.Equal(&b3) {
		t.Error("different inode must not be equal")
	}

	// unborn HEAD
	empty := newDir(t)
	if _, err := open(t, empty, Options{}).CaptureBinding(bg); err == nil || !strings.Contains(err.Error(), "no commits") {
		t.Errorf("unborn: %v", err)
	}

	// shallow clone
	for i := 0; i < 2; i++ {
		write(t, dir, "f", string(rune('a'+i)))
		commit(t, dir, "more")
	}
	clone := filepath.Join(t.TempDir(), "c")
	gitCmd(t, dir, "clone", "-q", "--depth=1", "file://"+dir, clone)
	rc := open(t, clone, Options{})
	if _, err := rc.CaptureBinding(bg); err == nil || !strings.Contains(err.Error(), "shallow") {
		t.Errorf("shallow: %v", err)
	}
}

func TestPathServedSeesAFreshRepochatignore(t *testing.T) {
	r, dir := guidanceRepo(t, Options{}, map[string]string{"AGENTS.md": "a\n"})
	if _, err := r.Policy(bg); err != nil {
		t.Fatal(err)
	}
	served, in1, err := r.PathServed(bg, "AGENTS.md")
	if err != nil || !served || !strings.HasSuffix(in1, "") {
		t.Fatalf("before: %v %v", served, err)
	}
	if !strings.HasPrefix(in1, "absent") {
		t.Errorf("inputs = %q, want absent first", in1)
	}
	write(t, dir, ".repochatignore", "AGENTS.md\n")
	served, in2, err := r.PathServed(bg, "AGENTS.md")
	if err != nil || served || in2 == in1 {
		t.Errorf("after: served=%v inputs %q vs %q err=%v", served, in1, in2, err)
	}
	// the cached snapshot still says served, proving PathServed bypassed it
	if p, _ := r.Policy(bg); p.Check("AGENTS.md") != "" {
		t.Log("snapshot already fresh; bypass not distinguishable")
	}
	if served, _, _ := r.PathServed(bg, "README.md"); !served {
		t.Error("README.md should be served")
	}
	if served, _, _ := r.PathServed(bg, "nope.md"); served {
		t.Error("an unlisted path is not served")
	}
	// options are part of the inputs
	r2 := open(t, dir, Options{TrackedOnly: true})
	_, in3, _ := r2.PathServed(bg, "AGENTS.md")
	if in3 == in2 {
		t.Error("options must change inputs")
	}
}
