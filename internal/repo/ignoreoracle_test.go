package repo

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// What git prints for a path takes three forms, and the exit status two that
// are not failures. Each is read for what it is.
func TestOracleReadsGitsAnswers(t *testing.T) {
	newDir(t) // isolates HOME
	o := newIgnoreOracle("*.log\n!keep.log\nprivate/\n")
	want := map[string]bool{
		"a.log":             true,  // matched by a rule
		"keep.log":          false, // matched by a rule that re-includes it
		"main.go":           false, // matched by nothing
		"private/keep.log":  true,  // beneath an excluded directory
		":colon.log":        true,  // a name git would otherwise read as pathspec magic
		":(glob)x.log":      true,
		":!bang.go":         false,
		"-dash.log":         true,
		" lead.log":         true,
		`back\slash.log`:    true,
		"dir with sp/b.log": true,
		"é/ü.go":            false,
	}
	var paths []string
	for p := range want {
		paths = append(paths, p)
	}
	if err := o.decide(bg, paths); err != nil {
		t.Fatal(err)
	}
	for p, w := range want {
		if got, err := o.ignored(bg, p); err != nil || got != w {
			t.Errorf("%q: ignored=%v (%v), want %v", p, got, err, w)
		}
	}
	if o.runs != 1 {
		t.Errorf("%d paths took %d git processes, want one", len(paths), o.runs)
	}

	// Nothing ignored at all: git exits 1, which is an answer, not a failure.
	none := newIgnoreOracle("*.log\n")
	if err := none.decide(bg, []string{"a.go", "b.go"}); err != nil {
		t.Fatalf("exit status 1 (nothing ignored) was taken for a failure: %v", err)
	}
	if got, err := none.ignored(bg, "a.go"); got || err != nil {
		t.Errorf("a.go: %v %v", got, err)
	}
	// Only a negation matches: git exits 0 and the path is not ignored.
	if got, err := newIgnoreOracle("*.log\n!keep.log\n").ignored(bg, "keep.log"); got || err != nil {
		t.Errorf("keep.log: %v %v", got, err)
	}
}

// The rules are the only source. Nothing the user or the system has
// configured for git may add to them or take away from them.
func TestOracleIgnoresEveryOtherIgnoreSource(t *testing.T) {
	newDir(t)
	home := os.Getenv("HOME")
	hostile := filepath.Join(home, "ignore-everything")
	write(t, home, "ignore-everything", "*\n")
	write(t, home, ".gitconfig", "[core]\n\texcludesFile = "+hostile+"\n\tignoreCase = true\n")
	write(t, home, ".config/git/ignore", "*\n")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.excludesFile")
	t.Setenv("GIT_CONFIG_VALUE_0", hostile)
	// Positive control: plain git, with this environment, ignores everything.
	probe := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", probe).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := exec.Command("git", "-C", probe, "check-ignore", "-q", "--no-index", "main.go").Run(); err != nil {
		t.Fatalf("positive control: with the user's configuration plain git must ignore main.go (%v)", err)
	}
	// And a GIT_DIR pointing somewhere else must not redirect the question.
	t.Setenv("GIT_DIR", filepath.Join(probe, ".git"))
	write(t, probe, ".git/info/exclude", "*\n")

	o := newIgnoreOracle("*.log\n")
	for p, want := range map[string]bool{"main.go": false, "a.log": true, "A.LOG": false} {
		if got, err := o.ignored(bg, p); err != nil || got != want {
			t.Errorf("%q: ignored=%v (%v), want %v", p, got, err, want)
		}
	}
}

// Answers are kept while the rules and the git program stay the same, and
// only that long.
func TestOracleCacheLivesAsLongAsItsPremises(t *testing.T) {
	dir := newDir(t)
	write(t, dir, ".repochatignore", "*.log\n")
	write(t, dir, "a.log", "x\n")
	write(t, dir, "b.txt", "x\n")
	commit(t, dir, "c")
	r := open(t, dir, Options{})
	first, err := r.Snapshot(bg)
	if err != nil {
		t.Fatal(err)
	}
	oracle := first.Policy.oracle
	runs := oracle.runs
	for i := 0; i < 3; i++ {
		fresh(r)
		s, err := r.Snapshot(bg)
		if err != nil {
			t.Fatal(err)
		}
		if s.Policy.oracle != oracle || oracle.runs != runs {
			t.Fatalf("unchanged rules and paths asked git again (%d → %d processes)", runs, oracle.runs)
		}
		if s.Generation != first.Generation {
			t.Fatal("the generation changed although nothing did")
		}
	}

	// New rules: a new generation, nothing carried over.
	write(t, dir, ".repochatignore", "*.txt\n")
	fresh(r)
	second, err := r.Snapshot(bg)
	if err != nil {
		t.Fatal(err)
	}
	if second.Policy.oracle == oracle || second.Generation == first.Generation {
		t.Fatal("changed rules kept the old answers or the old generation")
	}
	if e, _ := second.Lookup("a.log"); e.Withheld != "" {
		t.Errorf("a.log is no longer ignored: %+v", e)
	}
	if e, _ := second.Lookup("b.txt"); e.Withheld != ReasonIgnoreFile {
		t.Errorf("b.txt is ignored now: %+v", e)
	}

	// Another git program: its answers may differ, so the old ones go.
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	write(t, bin, "git", "#!/bin/sh\nexec "+real+" \"$@\"\n")
	os.Chmod(filepath.Join(bin, "git"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	before := second.Policy.oracle.runs
	fresh(r)
	third, err := r.Snapshot(bg)
	if err != nil {
		t.Fatal(err)
	}
	if third.Policy.oracle.runs == before {
		t.Error("a different git program was trusted with the previous one's answers")
	}
	if third.Generation == second.Generation {
		t.Error("a different git program must give a new generation")
	}
}

// When git cannot be asked, nothing is known to be allowed: no list is built,
// single paths are withheld, and a failure does not turn into one git
// process per path.
func TestOracleFailureFailsClosed(t *testing.T) {
	dir := newDir(t)
	write(t, dir, ".repochatignore", "*.log\n")
	write(t, dir, "main.go", "package main\n")
	for i := 0; i < 50; i++ {
		write(t, dir, "src/f"+strings.Repeat("x", i)+".go", "package src\n")
	}
	commit(t, dir, "c")
	r := open(t, dir, Options{})
	good, err := r.Snapshot(bg)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := good.Lookup("main.go"); e.Withheld != "" {
		t.Fatalf("positive control: main.go must be served while git answers: %+v", e)
	}

	// A git that works for everything except judging ignore rules.
	real, _ := exec.LookPath("git")
	bin := t.TempDir()
	count := filepath.Join(bin, "count")
	write(t, bin, "git", "#!/bin/sh\nfor a in \"$@\"; do if [ \"$a\" = check-ignore ]; then echo x >> "+count+"; echo 'fatal: broken' >&2; exit 128; fi; done\nexec "+real+" \"$@\"\n")
	os.Chmod(filepath.Join(bin, "git"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	fresh(r)
	if _, err := r.Snapshot(bg); err == nil || !strings.Contains(err.Error(), ".repochatignore") {
		t.Fatalf("without git's answers there must be no list of served files: %v", err)
	}
	if _, err := r.ReadFile(bg, "main.go", 1, 5, "", 1<<20); err == nil {
		t.Error("read_file served a file whose authorisation is unknown")
	}
	pol := good.Policy
	for i := 0; i < 40; i++ {
		if reason := pol.Check("history/only" + strings.Repeat("y", i) + ".go"); reason != ReasonUncertain {
			t.Fatalf("a path git could not judge must be withheld as %q, got %q", ReasonUncertain, reason)
		}
	}
	raw, _ := os.ReadFile(count)
	if n := strings.Count(string(raw), "x"); n > 4 {
		t.Errorf("a failing git was started %d times for ~90 paths: one process per path", n)
	}
	// A broken answer is as good as none: fewer records than paths.
	write(t, bin, "git", "#!/bin/sh\nfor a in \"$@\"; do if [ \"$a\" = check-ignore ]; then printf '\\0\\0\\0./only-one\\0'; exit 1; fi; done\nexec "+real+" \"$@\"\n")
	if err := newIgnoreOracle("*.log\n").decide(bg, []string{"a.go", "b.go"}); err == nil {
		t.Error("an answer about the wrong paths was accepted")
	}
}

// History is full of paths the working tree no longer has. They are judged
// like any other, and a commit that touches hundreds of them costs one
// question to git, not hundreds.
func TestHistoryOnlyPathsAreJudgedInBatches(t *testing.T) {
	dir := newDir(t)
	write(t, dir, ".repochatignore", "attic/private/\n*.bak\n")
	write(t, dir, "main.go", "package main\n")
	for i := 0; i < 300; i++ {
		write(t, dir, "attic/"+strings.Repeat("d", i%7+1)+"/file"+strings.Repeat("n", i%11)+string(rune('a'+i%26))+strings.Repeat("x", i/26)+".txt", "kept in history\n")
	}
	write(t, dir, "attic/private/plan.txt", "CANARY-PLAN\n")
	write(t, dir, "attic/old.bak", "CANARY-BAK\n")
	first := commit(t, dir, "first")
	if err := os.RemoveAll(filepath.Join(dir, "attic")); err != nil {
		t.Fatal(err)
	}
	commit(t, dir, "second: the attic is cleared")
	r := open(t, dir, Options{})
	snap, err := r.Snapshot(bg)
	if err != nil {
		t.Fatal(err)
	}
	oracle := snap.Policy.oracle
	step := func(what string, max int, f func() (string, error)) {
		t.Helper()
		before := oracle.runs
		text, err := f()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if n := oracle.runs - before; n > max {
			t.Errorf("%s started %d git processes to judge its paths, want at most %d", what, n, max)
		}
		mustNotContain(t, text, "CANARY", "plan.txt", "old.bak")
		mustContain(t, text, "attic/")
	}
	step("git_show of a commit with 300 history-only files", 1, func() (string, error) {
		res, err := r.Show(bg, first, nil, false, -1, 4<<20)
		if err != nil {
			return "", err
		}
		return res.Text, nil
	})
	step("the same commit again", 0, func() (string, error) {
		res, err := r.Show(bg, first, nil, true, -1, 4<<20)
		if err != nil {
			return "", err
		}
		return res.Text, nil
	})
	step("git_diff across the deletion", 0, func() (string, error) {
		res, err := r.Diff(bg, DiffOptions{Mode: DiffRange, Base: first, Target: "HEAD"}, 4<<20)
		if err != nil {
			return "", err
		}
		return res.Text, nil
	})
	step("git_log with stats", 0, func() (string, error) {
		res, err := r.Log(bg, LogOptions{WithStats: true})
		if err != nil {
			return "", err
		}
		return res.Text, nil
	})
	// A single history-only path named by the caller: one question at most.
	before := oracle.runs
	if _, err := r.ReadFile(bg, "attic/private/plan.txt", 1, 5, first, 1<<20); err == nil || !strings.Contains(err.Error(), "withheld") {
		t.Errorf("a withheld history-only file must be refused: %v", err)
	}
	if res, err := r.ReadFile(bg, "attic/d/filea.txt", 1, 5, first, 1<<20); err != nil || !strings.Contains(res.Text, "kept in history") {
		t.Errorf("a served history-only file must be readable: %v", err)
	}
	if n := oracle.runs - before; n > 1 {
		t.Errorf("two single lookups started %d git processes", n)
	}
}

// Two sources of rules, and git can be asked about only one of them: a path
// the answering source does not withhold is not known to be served, so it is
// withheld; a path the answering source withholds stays withheld as ignored.
func TestOneOfTwoOraclesFailingFailsClosed(t *testing.T) {
	for _, failing := range []string{"the tree's rules", "the owner's rules"} {
		t.Run(failing, func(t *testing.T) {
			pol, err := NewPolicy("*.log\n!keep.log\n", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := pol.AddIgnore("*.tmp\n!keep.tmp\n"); err != nil {
				t.Fatal(err)
			}
			// While git answers for both: the positive control.
			if err := pol.Prime(bg, []string{"main.go"}); err != nil {
				t.Fatal(err)
			}
			if got := pol.Check("main.go"); got != "" {
				t.Fatalf("main.go with both sources answering: %q", got)
			}
			broken, working := pol.oracle, pol.extraOracle
			if failing == "the owner's rules" {
				broken, working = working, broken
			}
			broken.mu.Lock()
			broken.failed, broken.failedAt = errors.New("git cannot be asked"), time.Now()
			broken.mu.Unlock()
			// The working source is asked first, so that its answers are there.
			if err := working.decide(bg, []string{"other.go", "keep.log", "keep.tmp"}); err != nil {
				t.Fatal(err)
			}

			if err := pol.Prime(bg, []string{"other.go"}); err == nil {
				t.Error("Prime reported no error with one source unanswered")
			}
			if ignored, err := pol.decide(bg, "other.go"); ignored || err == nil {
				t.Errorf("decide(other.go) = %v, %v; want not known", ignored, err)
			}
			if got := pol.Check("other.go"); got != ReasonUncertain {
				t.Errorf("other.go with one source unanswered: %q, want %q", got, ReasonUncertain)
			}
			if !pol.ignored("other.go") {
				t.Error("ignored(other.go) is false with one source unanswered")
			}
			// The lenient reading still withholds what either source names.
			for _, p := range []string{"a.log", "a.tmp"} {
				if got := pol.Check(p); got != ReasonIgnoreFile {
					t.Errorf("%s: %q, want %q", p, got, ReasonIgnoreFile)
				}
			}
			// A negation in one source cannot stand in for the other's answer.
			for _, p := range []string{"keep.log", "keep.tmp"} {
				if got := pol.Check(p); got != ReasonUncertain {
					t.Errorf("%s with one source unanswered: %q, want %q", p, got, ReasonUncertain)
				}
			}
			// Answers known from before the failure are kept.
			if got := pol.Check("main.go"); got != "" {
				t.Errorf("main.go, judged before the failure: %q", got)
			}
		})
	}
}
