package repo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The program that serves a repository keeps its own credentials in a
// directory of its own. When that directory lies inside the repository (a
// home directory under git), nothing in it may be served, whatever the
// patterns say, in the working tree or in history.
func TestOwnDirectoriesAreNeverServed(t *testing.T) {
	dir := newDir(t)
	write(t, dir, "notes.txt", "hello\n")
	write(t, dir, ".config/tool/config.json", `{"api_key":"sk-test-0123456789abcdefghijklmnop"}`+"\n")
	write(t, dir, ".config/toolbox/readme.txt", "a neighbour, not ours\n")
	commit(t, dir, "everything, the credentials file included")
	// The same directory reached through a symlink, and under another case.
	if err := os.Symlink(filepath.Join(dir, ".config"), filepath.Join(dir, "cfg-link")); err != nil {
		t.Fatal(err)
	}
	own := []string{filepath.Join(dir, "cfg-link", "tool"), filepath.Join(dir, "state-not-created-yet")}
	r := open(t, dir, Options{Own: own, Allow: []string{"**", ".config/"}, NoRedact: true})
	snap, err := r.Snapshot(bg)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := snap.Lookup(".config/tool/config.json"); !ok || e.Withheld != ReasonOwnFiles {
		t.Fatalf("the credentials file must be withheld as %q, got %+v", ReasonOwnFiles, e)
	}
	if e, _ := snap.Lookup(".config/toolbox/readme.txt"); e.Withheld != "" {
		t.Errorf("a directory that merely shares a prefix is not ours: %+v", e)
	}
	for _, p := range []string{".config/tool/config.json", ".CONFIG/Tool/config.json"} {
		if reason := snap.Policy.Check(p); reason != ReasonOwnFiles {
			t.Errorf("Check(%q) = %q", p, reason)
		}
	}
	if _, err := r.ReadFile(bg, ".config/tool/config.json", 1, 10, "", 1<<20); err == nil || !strings.Contains(err.Error(), "withheld") {
		t.Errorf("read_file must refuse it: %v", err)
	}
	if _, err := r.ReadFile(bg, ".config/tool/config.json", 1, 10, "HEAD", 1<<20); err == nil {
		t.Error("…and at a past revision too")
	}
	res, err := r.Search(bg, SearchOptions{Query: "sk-test"}, 1<<20)
	if err != nil || strings.Contains(res.Text, "sk-test-0123") {
		t.Errorf("search must not reach it: %v\n%s", err, res.Text)
	}
	show, err := r.Show(bg, "HEAD", nil, false, -1, 1<<20)
	if err != nil || strings.Contains(show.Text, "sk-test-0123") || strings.Contains(show.Text, "config.json") {
		t.Errorf("history must not reach it: %v\n%s", err, show.Text)
	}
}

// The repository root itself being one of those directories leaves nothing.
func TestOwnDirectoryAsRootWithholdsEverything(t *testing.T) {
	dir := newDir(t)
	write(t, dir, "config.json", "{}\n")
	commit(t, dir, "c")
	r := open(t, dir, Options{Own: []string{dir}})
	snap, err := r.Snapshot(bg)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Visible != 0 {
		t.Fatalf("%d file(s) served from the program's own directory", snap.Visible)
	}
}
