package repo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Regression tests for the defects found in the security and correctness
// review of the git tools.

// ---------------------------------------------------------------------------
// Submodules
// ---------------------------------------------------------------------------

type subFixture struct {
	super, sub string
	s          [4]string // the submodule's commits, oldest first
	bump       string    // superproject commit that moves the gitlink s[0] → s[1]
}

// Every string that exists only inside the submodule: file names, file
// content, commit subjects, uncommitted and untracked work.
var submoduleOnly = []string{"DB_PASSWORD", "subsecret", "SUBSUBJECT", "lib.txt", "SUBDIRTY", "SUBUNTRACKED", "sub/.env", "a/sub/", "b/sub/"}

// submoduleFixture builds a superproject whose submodule "sub" is at a
// different commit on every side a tool can compare:
//
//	superproject HEAD~1  gitlink s[0]
//	superproject HEAD    gitlink s[1]
//	index                gitlink s[2]
//	checkout of sub      s[3], plus a modified and an untracked file
//
// The submodule tracks a .env file, which the secret-file policy would
// withhold if it were part of the served file set.
func submoduleFixture(t *testing.T) subFixture {
	t.Helper()
	lib, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fx := subFixture{super: newDir(t)}
	gitCmd(t, lib, "init", "-q", "-b", "main")
	write(t, lib, "lib.txt", "v1\n")
	write(t, lib, ".env", "DB_PASSWORD=subsecret-1\n")
	fx.s[0] = commit(t, lib, "SUBSUBJECT one")

	write(t, fx.super, "a.txt", "hello\n")
	commit(t, fx.super, "super first")
	gitCmd(t, fx.super, "-c", "protocol.file.allow=always", "submodule", "add", "-q", lib, "sub")
	commit(t, fx.super, "add submodule")
	fx.sub = filepath.Join(fx.super, "sub")

	step := func(n string) string {
		write(t, fx.sub, "lib.txt", "v"+n+"\n")
		write(t, fx.sub, ".env", "DB_PASSWORD=subsecret-"+n+"\n")
		return commit(t, fx.sub, "SUBSUBJECT "+n)
	}
	fx.s[1] = step("2")
	fx.bump = commit(t, fx.super, "bump submodule")
	fx.s[2] = step("3")
	gitCmd(t, fx.super, "add", "sub")
	fx.s[3] = step("4")
	// Same size as the committed content and a different timestamp: git has
	// to read the file (through its clean filter) to tell whether it changed.
	write(t, fx.sub, "lib.txt", "v9\n")
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(fx.sub, "lib.txt"), later, later); err != nil {
		t.Fatal(err)
	}
	write(t, fx.sub, "SUBUNTRACKED.txt", "SUBDIRTY\n")
	return fx
}

// everyGitTool runs every tool that can touch a submodule and returns their
// combined output.
func everyGitTool(t *testing.T, r *Repo, fx subFixture) string {
	t.Helper()
	b := r.Budget()
	var all strings.Builder
	add := func(name, text string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		all.WriteString(text + "\n")
	}
	for _, mode := range []string{DiffHead, DiffStaged, DiffWorktree} {
		for _, statOnly := range []bool{false, true} {
			d, err := r.Diff(bg, DiffOptions{Mode: mode, StatOnly: statOnly, Context: -1}, b)
			if err != nil {
				t.Fatalf("diff %s: %v", mode, err)
			}
			add("diff "+mode, d.Text, nil)
		}
		d, err := r.Diff(bg, DiffOptions{Mode: mode, Paths: []string{"sub"}, Context: -1}, b)
		if err != nil {
			t.Fatalf("diff %s paths=[sub]: %v", mode, err)
		}
		add("diff "+mode, d.Text, nil)
	}
	d, err := r.Diff(bg, DiffOptions{Base: "HEAD~1", Target: "HEAD", Context: -1}, b)
	if err != nil {
		t.Fatalf("diff range: %v", err)
	}
	add("diff range", d.Text, nil)
	sh, err := r.Show(bg, "HEAD", nil, false, -1, b)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	add("show", sh.Text, nil)
	lg, err := r.Log(bg, LogOptions{WithStats: true})
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	add("log", lg.Text, nil)
	st, err := r.Status(bg)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	add("status", st.Text, nil)
	ov, err := r.Overview(bg)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	add("overview", ov.Text, nil)
	rf, err := r.Refs(bg, "")
	if err != nil {
		t.Fatalf("refs: %v", err)
	}
	add("refs", rf.Text, nil)
	bl, err := r.Blame(bg, "a.txt", 1, 1, "")
	if err != nil {
		t.Fatalf("blame: %v", err)
	}
	add("blame", bl.Text, nil)
	bl, err = r.Blame(bg, ".gitmodules", 1, 1, "HEAD")
	if err != nil {
		t.Fatalf("blame at HEAD: %v", err)
	}
	add("blame at HEAD", bl.Text, nil)
	return all.String()
}

// A changed submodule is a gitlink change and nothing more: whatever the
// superproject's or the user's configuration asks git to print about it, no
// file name, file content or commit subject from inside the submodule may
// appear.
func TestSubmoduleAppearsOnlyAsAGitlinkChange(t *testing.T) {
	configs := map[string]func(t *testing.T, fx subFixture){
		"defaults": func(*testing.T, subFixture) {},
		"superproject asks for inline diffs": func(t *testing.T, fx subFixture) {
			gitCmd(t, fx.super, "config", "diff.submodule", "diff")
			gitCmd(t, fx.super, "config", "submodule.sub.ignore", "none")
			gitCmd(t, fx.super, "config", "diff.ignoreSubmodules", "none")
		},
		"superproject asks for commit lists": func(t *testing.T, fx subFixture) {
			gitCmd(t, fx.super, "config", "diff.submodule", "log")
			gitCmd(t, fx.super, "config", "status.submoduleSummary", "true")
		},
		"user configuration asks for inline diffs": func(t *testing.T, fx subFixture) {
			write(t, os.Getenv("HOME"), ".gitconfig", "[diff]\n\tsubmodule = diff\n[status]\n\tsubmoduleSummary = true\n")
		},
	}
	for name, configure := range configs {
		t.Run(name, func(t *testing.T) {
			fx := submoduleFixture(t)
			configure(t, fx)
			r := open(t, fx.super, Options{})
			text := everyGitTool(t, r, fx)
			mustNotContain(t, text, submoduleOnly...)

			b := r.Budget()
			for mode, want := range map[string][2]string{
				DiffHead:     {fx.s[1], fx.s[3]},
				DiffStaged:   {fx.s[1], fx.s[2]},
				DiffWorktree: {fx.s[2], fx.s[3]},
			} {
				d, err := r.Diff(bg, DiffOptions{Mode: mode, Context: -1}, b)
				if err != nil {
					t.Fatal(err)
				}
				mustContain(t, d.Text, "M  sub  +1 −1", "-Subproject commit "+want[0]+"\n", "+Subproject commit "+want[1]+"\n")
			}
			sh, err := r.Show(bg, fx.bump, nil, false, -1, b)
			if err != nil {
				t.Fatal(err)
			}
			mustContain(t, sh.Text, "-Subproject commit "+fx.s[0]+"\n", "+Subproject commit "+fx.s[1]+"\n")
			st, err := r.Status(bg)
			if err != nil {
				t.Fatal(err)
			}
			mustContain(t, st.Text, "STAGED (index vs HEAD) (1)\n  M  sub\n", "UNSTAGED (working tree vs index) (1)\n  M  sub\n", "UNTRACKED (not ignored) (0)")
		})
	}
}

// The hardening must not depend on every call site remembering its flags:
// the options applied to every git process already keep plain porcelain
// commands out of the submodule.
func TestHardenedGitStaysOutOfSubmodules(t *testing.T) {
	fx := submoduleFixture(t)
	gitCmd(t, fx.super, "config", "diff.submodule", "diff")
	gitCmd(t, fx.super, "config", "status.submoduleSummary", "true")
	control := gitCmd(t, fx.super, "diff") + gitCmd(t, fx.super, "status")
	if !strings.Contains(control, "DB_PASSWORD") || !strings.Contains(control, "SUBSUBJECT") {
		t.Fatal("positive control: plain git does not print submodule content here")
	}
	// A clean filter that exists only in the submodule's configuration: git
	// runs it when it looks for uncommitted changes inside the submodule.
	script, flag := marker(t)
	write(t, fx.sub, ".gitattributes", "*.txt filter=evil\n")
	gitCmd(t, fx.sub, "config", "filter.evil.clean", script)
	plainGit(t, fx.super, nil, "-c", "diff.submodule=short", "diff")
	if !executed(flag) {
		t.Fatal("positive control: plain `git diff` did not look inside the submodule")
	}
	os.Remove(flag)

	r := open(t, fx.super, Options{})
	for _, args := range [][]string{
		{"diff"}, {"diff", "--cached"}, {"diff", "HEAD"}, {"show", "HEAD"}, {"log", "-p", "-2"}, {"status"},
	} {
		out, _, code, err := r.gitRaw(bg, 1<<20, args...)
		if err != nil || code != 0 {
			t.Fatalf("git %v: %v (exit %d): %s", args, err, code, out)
		}
		mustNotContain(t, string(out), submoduleOnly...)
		if executed(flag) {
			t.Fatalf("git %v ran a program configured only in the submodule", args)
		}
	}
}

// A program named only in the submodule's own configuration must never run,
// through any tool. Every case first shows that plain git, run in the
// superproject, DOES run it.
func TestSubmoduleConfigurationDoesNotExecute(t *testing.T) {
	type fixture struct {
		name     string
		prepare  func(t *testing.T, fx subFixture, script string)
		controls [][]string // plain git commands in the superproject; one must run the program
	}
	inline := []string{"-c", "diff.submodule=diff", "diff"}
	fixtures := []fixture{
		{"diff.external", func(t *testing.T, fx subFixture, script string) {
			gitCmd(t, fx.sub, "config", "diff.external", script)
		}, [][]string{inline}},
		{"textconv", func(t *testing.T, fx subFixture, script string) {
			write(t, fx.sub, ".gitattributes", "*.txt diff=evil\n")
			gitCmd(t, fx.sub, "config", "diff.evil.textconv", script)
		}, [][]string{inline}},
		{"fsmonitor hook", func(t *testing.T, fx subFixture, script string) {
			gitCmd(t, fx.sub, "config", "core.fsmonitor", script)
		}, [][]string{{"status"}}},
		{"clean filter", func(t *testing.T, fx subFixture, script string) {
			write(t, fx.sub, ".gitattributes", "*.txt filter=evil\n")
			gitCmd(t, fx.sub, "config", "filter.evil.clean", script)
		}, [][]string{{"status"}, inline}},
	}
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			fx := submoduleFixture(t)
			script, flag := marker(t)
			f.prepare(t, fx, script)
			// The superproject's configuration asks for everything that
			// makes git enter a submodule.
			gitCmd(t, fx.super, "config", "diff.submodule", "diff")
			gitCmd(t, fx.super, "config", "submodule.sub.ignore", "none")
			gitCmd(t, fx.super, "config", "diff.ignoreSubmodules", "none")
			gitCmd(t, fx.super, "config", "status.submoduleSummary", "true")

			for _, control := range f.controls {
				plainGit(t, fx.super, nil, control...)
				if !executed(flag) {
					t.Fatalf("positive control: plain `git %s` in the superproject did not run the submodule's program (%s)",
						strings.Join(control, " "), gitCmd(t, fx.super, "version"))
				}
				os.Remove(flag)
			}

			r := open(t, fx.super, Options{})
			text := everyGitTool(t, r, fx)
			if executed(flag) {
				t.Fatalf("%s: a program configured only in the submodule was executed by a read operation", f.name)
			}
			mustNotContain(t, text, submoduleOnly...)
			d, err := r.Diff(bg, DiffOptions{Context: -1}, r.Budget())
			if err != nil || !strings.Contains(d.Text, "+Subproject commit "+fx.s[3]) {
				t.Fatalf("the gitlink change is no longer reported: %v\n%v", err, d)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Revisions
// ---------------------------------------------------------------------------

// revisionCalls runs every operation that takes a revision with rev in each
// of its revision arguments and returns the errors by argument name.
func revisionCalls(r *Repo, rev string) map[string]error {
	b := r.Budget()
	errs := map[string]error{}
	_, errs["resolve"] = r.ResolveCommit(bg, rev)
	_, errs["log rev"] = r.Log(bg, LogOptions{Rev: rev})
	_, errs["log range start"] = r.Log(bg, LogOptions{Range: rev + "..HEAD"})
	_, errs["log range end"] = r.Log(bg, LogOptions{Range: "HEAD.." + rev})
	_, errs["show rev"] = r.Show(bg, rev, nil, true, -1, b)
	_, errs["diff base"] = r.Diff(bg, DiffOptions{Base: rev, Context: -1}, b)
	_, errs["diff target"] = r.Diff(bg, DiffOptions{Base: "HEAD", Target: rev, Context: -1}, b)
	_, errs["diff merge base"] = r.Diff(bg, DiffOptions{Base: rev, Target: "HEAD", MergeBase: true, Context: -1}, b)
	_, errs["blame rev"] = r.Blame(bg, "a.txt", 1, 1, rev)
	_, errs["read rev"] = r.ReadFile(bg, "a.txt", 1, 5, rev, b)
	return errs
}

// Revision syntax that makes git search commit messages would let a caller
// recover masked text guess by guess (a guess that matches resolves, one that
// does not is "unknown"), and reach the messages of commits that are not
// served at all. Such forms are refused before git sees them.
func TestRevisionSyntaxCannotSearchCommitMessages(t *testing.T) {
	dir := newDir(t)
	token := "ghp_" + strings.Repeat("0", 36)
	write(t, dir, "a.txt", "one\n")
	commit(t, dir, "deploy with "+token)
	write(t, dir, "a.txt", "two\n")
	commit(t, dir, "second")
	write(t, dir, "a.txt", "stashed\n")
	gitCmd(t, dir, "stash", "push", "-q", "-m", "STASHWORD notes")

	// Positive control: git itself tells the two guesses apart.
	if gitCmd(t, dir, "rev-parse", "HEAD^{/ghp_0}") == "" {
		t.Fatal("positive control: git does not resolve a message search")
	}
	if _, _, code, _ := runGit(bg, dir, nil, 1024, "rev-parse", "--verify", "--quiet", "HEAD^{/ghp_9}"); code == 0 {
		t.Fatal("positive control: a message search that matches nothing resolved")
	}

	r := open(t, dir, Options{})
	pairs := [][2]string{
		{"HEAD^{/ghp_0}", "HEAD^{/ghp_9}"},
		{"main^{/deploy}", "main^{/nothing}"},
		{"HEAD^{/!-ghp_9}", "HEAD^{/!-second}"},
		{"refs/stash^{/STASHWORD}", "refs/stash^{/OTHERWORD}"},
		{"stash^{/STASHWORD}", "stash^{/OTHERWORD}"},
		{":/ghp_0", ":/ghp_9"},
		{":/deploy", ":/nothing"},
		{"HEAD^{/deploy}^{commit}", "HEAD^{/nothing}^{commit}"},
		{"HEAD~0^{/deploy}", "HEAD~0^{/nothing}"},
	}
	for _, p := range pairs {
		hit, miss := revisionCalls(r, p[0]), revisionCalls(r, p[1])
		for arg, herr := range hit {
			merr := miss[arg]
			if herr == nil || merr == nil {
				t.Errorf("%s: message-search revisions must be refused: %q → %v, %q → %v", arg, p[0], herr, p[1], merr)
				continue
			}
			if herr.Error() != merr.Error() {
				t.Errorf("%s: the two guesses are distinguishable:\n  %q → %v\n  %q → %v", arg, p[0], herr, p[1], merr)
			}
			mustNotContain(t, herr.Error(), "ghp_", "STASHWORD", "deploy")
		}
	}
	// The same holds when masking is off: the stash is still not served.
	raw := open(t, dir, Options{NoRedact: true})
	if _, err := raw.ResolveCommit(bg, "refs/stash^{/STASHWORD}"); err == nil {
		t.Error("message search must be refused with masking off too")
	}

	// Forms that only peel or navigate keep working.
	head := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "tag", "-a", "-m", "annotated", "v2.0", "HEAD")
	gitCmd(t, dir, "branch", "feat{x}", "HEAD~1")
	for _, ok := range []string{"HEAD", "@", "main", "HEAD^", "HEAD~1", "HEAD^1", "main~1^0", "v2.0", "v2.0^{}", "v2.0^{commit}", "HEAD^{commit}", "refs/heads/main", "heads/main", head, head[:7], "feat{x}", "feat{x}~0"} {
		if _, err := r.ResolveCommit(bg, ok); err != nil {
			t.Errorf("%q should resolve: %v", ok, err)
		}
	}
	// Everything else is refused on its syntax alone, before git runs.
	for _, bad := range []string{
		"HEAD^{/x}", "HEAD^{tree}", "HEAD^{blob}", "HEAD^{tag}", "HEAD^{object}", "v2.0^{tag}", "HEAD^{", "HEAD^{}^{/x}", "HEAD^{commit",
		":/x", ":0:a.txt", "HEAD:a.txt", "HEAD@{0}", "@{-1}", "@{u}", "main@{upstream}", "main@{push}", "HEAD@{yesterday}", "@{now}",
		"HEAD^@", "HEAD^!", "HEAD^-", "HEAD^-1", "HEAD...main", "HEAD..main", "^HEAD", "-n1", "--all", "--stdin", "--reflog", "--glob=refs/*",
		"HEAD --all", "HEAD\x00main", "HEAD\nmain", "ma*n", "ma?n", "ma[i]n", "$(id)", "`id`", "a;b", "a|b", "a&b", "a\\b", "a'b", "a\"b", "<x>",
		strings.Repeat("a", 201),
	} {
		for arg, err := range revisionCalls(r, bad) {
			if err == nil {
				t.Errorf("%s: %q must be refused", arg, bad)
			} else if !strings.Contains(err.Error(), "invalid revision") && !strings.Contains(err.Error(), "not available") &&
				!strings.Contains(err.Error(), "not a range") && !strings.Contains(err.Error(), "range must have the form") {
				t.Errorf("%s: %q must be refused on its syntax, got: %v", arg, bad, err)
			}
		}
	}
}

// countingGit puts a wrapper in front of git that records every invocation,
// and returns a function that reports how many were made since the last call.
func countingGit(t *testing.T) func() int {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	logFile := filepath.Join(dir, "calls")
	shim := "#!/bin/sh\necho x >> '" + logFile + "'\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		data, _ := os.ReadFile(logFile)
		os.Remove(logFile)
		return strings.Count(string(data), "\n")
	}
}

// Whether a name resolves to a commit that is merely not served (stash,
// reflog-only, dangling) or to nothing at all must not be observable: same
// error text, same amount of git work.
func TestUnknownAndUnreachableRevisionsAreIndistinguishable(t *testing.T) {
	dir := newDir(t)
	write(t, dir, "a.txt", "one\n")
	commit(t, dir, "first")
	write(t, dir, "a.txt", "two\n")
	commit(t, dir, "second")
	tree := gitCmd(t, dir, "rev-parse", "HEAD^{tree}")
	dangling := gitCmd(t, dir, "commit-tree", tree, "-m", "dangling")
	write(t, dir, "a.txt", "stashed\n")
	gitCmd(t, dir, "stash", "push", "-q")
	stash := gitCmd(t, dir, "rev-parse", "refs/stash")
	gitCmd(t, dir, "checkout", "-q", "-b", "gone")
	write(t, dir, "gone.txt", "gone\n")
	gone := commit(t, dir, "gone commit")
	gitCmd(t, dir, "checkout", "-q", "main")
	gitCmd(t, dir, "branch", "-q", "-D", "gone")

	const unknownHash = "0123456789abcdef0123456789abcdef01234567"
	existing := map[string]string{
		"dangling commit": dangling, "abbreviated dangling commit": dangling[:12], "stash commit": stash, "stash ref": "refs/stash",
		"stash": "stash", "stash parent": "stash^2", "deleted branch": gone, "navigation from the stash": "refs/stash~0",
	}
	missing := map[string]string{
		"unknown hash": unknownHash, "unknown short hash": unknownHash[:12],
		"unknown ref": "refs/nope", "unknown name": "nope", "unknown parent": "nope^2", "tree, not a commit": tree,
	}
	count := countingGit(t)
	r := open(t, dir, Options{})
	normalise := func(err error, rev string) string {
		if err == nil {
			return "<no error>"
		}
		// Only the echoed argument differs; it is quoted in the message.
		return strings.ReplaceAll(err.Error(), `"`+rev+`"`, "<REV>")
	}

	// Each argument adds its own prefix, so texts are compared per argument.
	want := map[string]string{}
	for arg, err := range revisionCalls(r, unknownHash) {
		want[arg] = normalise(err, unknownHash)
	}
	for _, set := range []map[string]string{existing, missing} {
		for name, rev := range set {
			for arg, err := range revisionCalls(r, rev) {
				if err == nil {
					t.Errorf("%s (%s) must not be served through %s", name, rev, arg)
				} else if got := normalise(err, rev); got != want[arg] {
					t.Errorf("%s: %s (%s) is distinguishable by its error:\n  %s\n  %s", arg, name, rev, got, want[arg])
				}
			}
		}
	}
	mustContain(t, want["resolve"], "not reachable")

	count()
	calls := map[string]int{}
	for _, set := range []map[string]string{existing, missing} {
		for name, rev := range set {
			r.ResolveCommit(bg, rev)
			calls[name] = count()
		}
	}
	for name, n := range calls {
		if n == 0 {
			t.Fatalf("the git wrapper is not counting (%s)", name)
		}
		if n != calls["dangling commit"] {
			t.Errorf("%s costs %d git invocations, an unreachable commit costs %d: the difference is observable as time", name, n, calls["dangling commit"])
		}
	}
}

// ---------------------------------------------------------------------------
// Pickaxe
// ---------------------------------------------------------------------------

// The pickaxe search (only available with masking off) compares file
// versions, and git converts them first with a repository-configured textconv
// program unless told not to.
func TestPickaxeDoesNotRunTextconv(t *testing.T) {
	for _, driver := range []string{"textconv", "command"} {
		t.Run("diff driver "+driver, func(t *testing.T) {
			dir := newDir(t)
			write(t, dir, ".gitattributes", "*.txt diff=evil\n")
			write(t, dir, "a.txt", "one\n")
			commit(t, dir, "first")
			write(t, dir, "a.txt", "one\nneedle x\n")
			commit(t, dir, "second")
			script, flag := marker(t)
			gitCmd(t, dir, "config", "diff.evil."+driver, script)

			plainGit(t, dir, nil, "log", "-Sneedle")
			control := executed(flag)
			os.Remove(flag)
			if driver == "textconv" && !control {
				t.Fatalf("positive control: plain `git log -S` did not run the textconv program (%s)", gitCmd(t, dir, "version"))
			}

			r := open(t, dir, Options{NoRedact: true})
			res, err := r.Log(bg, LogOptions{Pickaxe: "needle"})
			if executed(flag) {
				t.Fatal("git_log with pickaxe ran a repository-configured program")
			}
			if err != nil || res.Count != 1 || !strings.Contains(res.Text, "second") {
				t.Fatalf("pickaxe no longer finds the commit: %v\n%v", err, res)
			}
			if res, err = r.Log(bg, LogOptions{Pickaxe: "needle", WithStats: true, Path: "a.txt"}); err != nil || res.Count != 1 {
				t.Fatalf("pickaxe with path and stats: %v\n%v", err, res)
			}
			if executed(flag) {
				t.Fatal("git_log with pickaxe, path and stats ran a repository-configured program")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Blame
// ---------------------------------------------------------------------------

// git's blame output repeats every line's content, so a range of very long
// lines can exceed what repochat reads from git. The lines that were not
// received must be reported as not attributed and offered for a further
// call; they must never be shown as if they were uncommitted.
func TestBlameCutAtTheOutputCapIsReported(t *testing.T) {
	dir := newDir(t)
	var content strings.Builder
	for i := 1; i <= MaxBlameLines; i++ {
		content.WriteString(strings.Repeat("x", 60<<10))
		content.WriteString("\n")
	}
	write(t, dir, "wide.txt", content.String())
	oid := commit(t, dir, "wide lines")
	r := open(t, dir, Options{})

	for name, rev := range map[string]string{"working tree": "", "at a revision": "HEAD"} {
		seen := 0
		for start, calls := 1, 0; start <= MaxBlameLines; calls++ {
			if calls > 10 {
				t.Fatalf("%s: blame does not make progress", name)
			}
			res, err := r.Blame(bg, "wide.txt", start, MaxBlameLines, rev)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if strings.Contains(res.Text, "uncommitted") {
				t.Fatalf("%s: committed lines beyond the cut are labelled uncommitted (lines %d-%d)", name, res.First, res.Last)
			}
			if res.First != start || res.Last < res.First || res.Last > MaxBlameLines {
				t.Fatalf("%s: bad range %d-%d for start %d", name, res.First, res.Last, start)
			}
			mustContain(t, res.Text, oid[:10], "wide lines")
			body := strings.Count(res.Text, "| ") / 2
			if body != res.Last-res.First+1 {
				t.Fatalf("%s: %d lines shown for the range %d-%d", name, body, res.First, res.Last)
			}
			if res.Last < MaxBlameLines {
				if !res.Truncated {
					t.Fatalf("%s: a blame that stops at line %d of %d must say it was cut", name, res.Last, MaxBlameLines)
				}
				mustContain(t, res.Text, "blame output was cut", "start_line="+strconv.Itoa(res.Last+1))
				mustContain(t, res.Text, "lines "+strconv.Itoa(res.First)+"-"+strconv.Itoa(res.Last)+" of")
			}
			seen += res.Last - res.First + 1
			start = res.Last + 1
		}
		if seen != MaxBlameLines {
			t.Errorf("%s: %d of %d lines attributed over all calls", name, seen, MaxBlameLines)
		}
	}
	// A normal blame is not marked as cut.
	write(t, dir, "small.txt", "a\nb\n")
	commit(t, dir, "small")
	fresh(r)
	res, err := r.Blame(bg, "small.txt", 1, 2, "")
	if err != nil || res.Truncated || res.Last != 2 {
		t.Fatalf("small blame: %v %+v", err, res)
	}
}

// One commit must show the same day in every tool. git_log prints the date
// in the zone recorded in the commit, so blame does too.
func TestBlameAndLogAgreeOnTheCommitDate(t *testing.T) {
	dir := newDir(t)
	for zone, tz := range map[string]string{"east": "Pacific/Kiritimati", "west": "Pacific/Pago_Pago", "utc": "UTC"} {
		t.Run(zone, func(t *testing.T) {
			// The machine's own zone must play no part either.
			t.Setenv("TZ", tz)
			cases := []struct{ file, date, day string }{
				{"early.txt", "2026-03-10T01:30:00+0500", "2026-03-10"}, // 2026-03-09 in UTC
				{"late.txt", "2026-03-10T22:30:00-0800", "2026-03-10"},  // 2026-03-11 in UTC
				{"half.txt", "2026-03-10T00:10:00+0530", "2026-03-10"},  // 2026-03-09 in UTC
			}
			r := open(t, dir, Options{})
			for _, c := range cases {
				write(t, dir, c.file, zone+" "+c.date+"\n")
				gitCmd(t, dir, "add", "-A")
				gitCmd(t, dir, "commit", "-q", "--date="+c.date, "-m", "dated "+c.file)
				fresh(r)
				lg, err := r.Log(bg, LogOptions{Limit: 1})
				if err != nil {
					t.Fatal(err)
				}
				bl, err := r.Blame(bg, c.file, 1, 1, "")
				if err != nil {
					t.Fatal(err)
				}
				mustContain(t, lg.Text, "  "+c.day+"  Ada")
				mustContain(t, bl.Text, "  "+c.day+"  Ada")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Merge conflicts
// ---------------------------------------------------------------------------

// conflictFixture leaves a merge stopped on conflicts: c.txt and key.txt
// changed on both sides, del.txt deleted on one side and edited on the other,
// plus an ordinary unstaged change in other.txt.
func conflictFixture(t *testing.T) (dir string, keyLines []string) {
	t.Helper()
	dir = newDir(t)
	body := strings.Split(strings.TrimSpace(fakePEM), "\n")
	for i := 0; i < 12; i++ {
		keyLines = append(keyLines, "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVphYmNkZWZnaGlqa2xtbm9wcXJzdHV2"[i:]+strings.Repeat("A", i))
	}
	key := func(lines []string) string {
		return "intro\n" + body[0] + "\n" + strings.Join(lines, "\n") + "\n" + body[len(body)-1] + "\noutro\n"
	}
	variant := func(i int, replacement string) []string {
		out := append([]string(nil), keyLines...)
		out[i] = replacement
		return out
	}
	write(t, dir, "c.txt", "base\nline\n")
	write(t, dir, "other.txt", "other\n")
	write(t, dir, "del.txt", "to delete\n")
	write(t, dir, "key.txt", key(keyLines))
	commit(t, dir, "base")
	gitCmd(t, dir, "checkout", "-q", "-b", "side")
	write(t, dir, "c.txt", "side\nline\n")
	write(t, dir, "del.txt", "side edit\n")
	write(t, dir, "key.txt", key(variant(6, "U0lERVNJREVTSURFU0lERVNJREVTSURFU0lERVNJREVTSURFU0lERVNJREVTSURF")))
	commit(t, dir, "side")
	gitCmd(t, dir, "checkout", "-q", "main")
	write(t, dir, "c.txt", "main\nline\n")
	os.Remove(filepath.Join(dir, "del.txt"))
	write(t, dir, "key.txt", key(variant(6, "TUFJTk1BSU5NQUlOTUFJTk1BSU5NQUlOTUFJTk1BSU5NQUlOTUFJTk1BSU5NQUlO")))
	commit(t, dir, "main")
	keyLines = append(keyLines, "U0lERVNJREVTSURFU0lERVNJREVT", "TUFJTk1BSU5NQUlOTUFJTk1BSU5N")

	cmd := exec.Command("git", "merge", "-q", "side")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@example.com", "GIT_COMMITTER_NAME=Ada", "GIT_COMMITTER_EMAIL=ada@example.com")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("the merge was expected to stop on conflicts:\n%s", out)
	}
	write(t, dir, "other.txt", "other changed\n")
	return dir, keyLines
}

// A conflicted file has several versions in the index, so git reports it
// more than once when the index is one side of the comparison. It must be
// listed once, and its patch must not be repeated — nor shown in the
// combined format, which the key-material masking cannot follow.
func TestDiffDuringAMergeConflict(t *testing.T) {
	dir, keyLines := conflictFixture(t)
	if out := gitCmd(t, dir, "diff", "--name-status"); strings.Count(out, "c.txt") != 2 {
		t.Fatalf("positive control: git lists the conflicted file once here:\n%s", out)
	}
	r := open(t, dir, Options{})
	b := r.Budget()
	noKey := func(text string) {
		t.Helper()
		for _, l := range keyLines {
			if strings.Contains(text, l[:20]) {
				t.Fatalf("key material from a conflicted file is shown:\n%s", text)
			}
		}
	}

	d, err := r.Diff(bg, DiffOptions{Mode: DiffWorktree, Context: -1}, b)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"c.txt", "del.txt", "key.txt", "other.txt"} {
		if n := strings.Count(d.Text, "  "+name+"  "); n != 1 {
			t.Errorf("worktree: %s is listed %d times:\n%s", name, n, d.Text)
		}
	}
	if d.Files != 4 || d.Shown != 1 {
		t.Errorf("worktree: 4 changed files and one patch expected, got %d and %d:\n%s", d.Files, d.Shown, d.Text)
	}
	mustContain(t, d.Text, "4 file(s) changed", "U  c.txt  (unresolved merge conflict)", "U  del.txt  (unresolved merge conflict)",
		"M  other.txt  +1 −1", "+other changed", "no patch for c.txt, del.txt, key.txt", "mode=head")
	mustNotContain(t, d.Text, "diff --cc", "@@@", "Unmerged path", "<<<<<<<")
	if strings.Count(d.Text, "diff --git") != 1 {
		t.Errorf("worktree: exactly one patch expected:\n%s", d.Text)
	}
	noKey(d.Text)

	d, err = r.Diff(bg, DiffOptions{Mode: DiffStaged, Context: -1}, b)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, d.Text, "3 file(s) changed", "U  c.txt  (unresolved merge conflict)", "no patch for c.txt, del.txt, key.txt")
	mustNotContain(t, d.Text, "Unmerged path", "diff --cc")
	if d.Files != 3 || d.Shown != 0 {
		t.Errorf("staged: three conflicted files and no patch expected, got %d and %d:\n%s", d.Files, d.Shown, d.Text)
	}
	noKey(d.Text)

	// Against HEAD there is one old version, so the ordinary patch (with the
	// conflict markers) is shown, once, and masked like any other.
	d, err = r.Diff(bg, DiffOptions{Mode: DiffHead, Context: -1}, b)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, d.Text, "4 file(s) changed", "M  c.txt  +4 −0", "+<<<<<<< HEAD", "+>>>>>>> side", "[REDACTED:private-key]")
	if strings.Count(d.Text, "diff --git a/c.txt") != 1 || d.Shown != 4 {
		t.Errorf("head: every file once:\n%s", d.Text)
	}
	noKey(d.Text)

	if d.Incomplete {
		t.Error("head: every patch was shown, the result is complete")
	}

	// A path filter on a conflicted file behaves the same.
	d, err = r.Diff(bg, DiffOptions{Mode: DiffWorktree, Paths: []string{"c.txt"}, Context: -1}, b)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, d.Text, "1 file(s) changed", "U  c.txt  (unresolved merge conflict)")
	mustNotContain(t, d.Text, "diff --cc", "M  c.txt")
	// The caller asked for patches and did not get this one.
	if !d.Incomplete {
		t.Error("worktree: a conflicted file without a patch must make the result incomplete")
	}
	if d, _ = r.Diff(bg, DiffOptions{Mode: DiffWorktree, StatOnly: true, Context: -1}, b); d == nil || d.Incomplete {
		t.Errorf("worktree, stat only: the list is all that was asked for: %+v", d)
	}
}

// Whatever a diff leaves out must be visible to the caller as an incomplete
// result, not only as a sentence in the text.
func TestDiffReportsEverythingItLeavesOut(t *testing.T) {
	dir := newDir(t)
	write(t, dir, "seed.txt", "seed\n")
	base := commit(t, dir, "seed")
	for i := 0; i < maxDiffListed+5; i++ {
		write(t, dir, "many/f"+strconv.Itoa(i)+".txt", "x\n")
	}
	many := commit(t, dir, "many files")
	r := open(t, dir, Options{})

	d, err := r.Diff(bg, DiffOptions{Base: base, Target: many, StatOnly: true, Context: -1}, r.Budget())
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, d.Text, "… 5 more changed file(s) not listed")
	if !d.Incomplete {
		t.Error("a file list cut at its limit must make the result incomplete")
	}
	d, err = r.Diff(bg, DiffOptions{Base: base, Target: many, Paths: []string{"many/f1.txt"}, Context: -1}, r.Budget())
	if err != nil || d.Incomplete || len(d.Cut) != 0 || len(d.Omitted) != 0 {
		t.Errorf("a diff that shows everything is complete: %v %+v", err, d)
	}

	// A patch cut at its share of the budget.
	write(t, dir, "wide.txt", strings.Repeat("old line of text\n", 20000))
	wideBase := commit(t, dir, "wide")
	write(t, dir, "wide.txt", strings.Repeat("new line of text\n", 20000))
	commit(t, dir, "rewrite wide")
	fresh(r)
	d, err = r.Diff(bg, DiffOptions{Base: wideBase, Context: -1}, r.Budget())
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, d.Text, "[patch for wide.txt cut at")
	if len(d.Cut) != 1 || d.Cut[0] != "wide.txt" {
		t.Errorf("a cut patch must be reported: %v", d.Cut)
	}

	// A patch that may show key material from a file too large to examine.
	if testing.Short() {
		t.Skip("the last case writes two 33 MB files")
	}
	filler := strings.Repeat("ordinary line of text, nothing to see here\n", (maxRevBlobBytes+(1<<20))/43)
	write(t, dir, "huge.txt", "-----BEGIN RSA PRIVATE KEY-----\nQUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVphYmNkZWZnaGlqa2xtbm9wcXJzdHV2\n"+filler)
	hugeBase := commit(t, dir, "huge")
	write(t, dir, "huge.txt", "-----BEGIN RSA PRIVATE KEY-----\nQ0hBTkdFRENIQU5HRURDSEFOR0VEQ0hBTkdFRENIQU5HRURDSEFOR0VEQ0hBTkdF\n"+filler)
	commit(t, dir, "rotate")
	fresh(r)
	d, err = r.Diff(bg, DiffOptions{Base: hugeBase, Context: -1}, r.Budget())
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, d.Text, "[patch omitted: it may contain private-key material")
	mustNotContain(t, d.Text, "QUJDREVGR0hJ", "Q0hBTkdFRENI")
	if !d.Incomplete || d.Shown != 0 {
		t.Errorf("a patch that was held back must make the result incomplete: %+v", d)
	}
}

// ---------------------------------------------------------------------------
// Read-only
// ---------------------------------------------------------------------------

// A file whose timestamp changed but whose content did not makes git want to
// record the new timestamp in the index. A read tool must leave the index
// file exactly as it found it.
func TestReadsDoNotRewriteTheIndex(t *testing.T) {
	for _, layout := range []string{"ordinary index", "split index"} {
		t.Run(layout, func(t *testing.T) { readsDoNotRewriteTheIndex(t, layout == "split index") })
	}
}

func readsDoNotRewriteTheIndex(t *testing.T, split bool) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp) // so that leftovers can be told from other processes' files
	dir := newDir(t)
	write(t, dir, "a.txt", "one\n")
	write(t, dir, "b.txt", "two\n")
	commit(t, dir, "first")
	if split {
		// A split index keeps most entries in a shared file in the git
		// directory, which git rewrites when enough entries changed; here,
		// whenever any did.
		gitCmd(t, dir, "config", "core.splitIndex", "true")
		gitCmd(t, dir, "config", "splitIndex.maxPercentChange", "0")
		gitCmd(t, dir, "update-index", "--split-index")
		write(t, dir, "c.txt", "three\n")
		gitCmd(t, dir, "add", "c.txt")
	}
	touch := func(hours int) {
		t.Helper()
		when := time.Now().Add(time.Duration(hours) * time.Hour)
		if err := os.Chtimes(filepath.Join(dir, "a.txt"), when, when); err != nil {
			t.Fatal(err)
		}
	}
	// Everything in the git directory that belongs to the index: the file
	// itself, its lock, and the shared part of a split index.
	snapshot := func() string {
		t.Helper()
		entries, err := os.ReadDir(filepath.Join(dir, ".git"))
		if err != nil {
			t.Fatal(err)
		}
		var all strings.Builder
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), "index") && !strings.HasPrefix(e.Name(), "sharedindex") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, ".git", e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			all.WriteString(e.Name() + "\x00" + string(data) + "\x00")
		}
		return all.String()
	}

	touch(-48)
	before := snapshot()
	if split != strings.Contains(before, "sharedindex.") {
		t.Fatalf("fixture: split=%v but the git directory says otherwise", split)
	}
	gitCmd(t, dir, "diff")
	if snapshot() == before {
		t.Fatalf("positive control: plain `git diff` did not rewrite the index (%s)", gitCmd(t, dir, "version"))
	}

	touch(-72)
	before = snapshot()
	r := open(t, dir, Options{})
	b := r.Budget()
	for _, mode := range []string{DiffHead, DiffWorktree, DiffStaged} {
		for _, statOnly := range []bool{false, true} {
			d, err := r.Diff(bg, DiffOptions{Mode: mode, StatOnly: statOnly, Context: -1}, b)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot() != before {
				t.Fatalf("git_diff mode=%s stat_only=%v rewrote the index in the git directory", mode, statOnly)
			}
			// A changed timestamp alone is still not reported as a change.
			mustNotContain(t, d.Text, "a.txt")
		}
	}
	// Real changes are still seen, on either side of the index.
	write(t, dir, "b.txt", "TWO\n")
	write(t, dir, "staged.txt", "staged\n")
	gitCmd(t, dir, "add", "staged.txt")
	touch(-96)
	before = snapshot()
	fresh(r)
	for mode, want := range map[string][]string{
		DiffHead: {"+TWO", "+staged"}, DiffWorktree: {"+TWO"}, DiffStaged: {"+staged"},
	} {
		d, err := r.Diff(bg, DiffOptions{Mode: mode, Context: -1}, b)
		if err != nil {
			t.Fatal(err)
		}
		mustContain(t, d.Text, want...)
		mustNotContain(t, d.Text, "a.txt")
		if mode == DiffStaged {
			mustNotContain(t, d.Text, "+TWO")
		}
		if snapshot() != before {
			t.Fatalf("git_diff mode=%s rewrote the index in the git directory", mode)
		}
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("temporary files were left behind: %v", left)
	}
	steps := map[string]func() error{
		"status":   func() error { _, err := r.Status(bg); return err },
		"overview": func() error { _, err := r.Overview(bg); return err },
		"blame":    func() error { _, err := r.Blame(bg, "a.txt", 1, 1, ""); return err },
		"log":      func() error { _, err := r.Log(bg, LogOptions{WithStats: true}); return err },
		"show":     func() error { _, err := r.Show(bg, "HEAD", nil, false, -1, b); return err },
		"refs":     func() error { _, err := r.Refs(bg, ""); return err },
		"list":     func() error { _, err := r.List(bg, ListOptions{}); return err },
	}
	for name, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if snapshot() != before {
			t.Fatalf("%s rewrote .git/index", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Git directory outside the working tree
// ---------------------------------------------------------------------------

// A ".git" file may name any git directory. Linked worktrees, submodule
// checkouts and `git init --separate-git-dir` use that legitimately, and
// there the git directory points back at the working tree. When it does not,
// the folder is served with another repository's history; that is allowed,
// but the person running repochat is told.
func TestGitDirThatDoesNotPointBackIsReported(t *testing.T) {
	quiet := func(name, dir string) {
		t.Helper()
		r := open(t, dir, Options{})
		if len(r.Warnings) != 0 {
			t.Errorf("%s: unexpected warning: %q", name, r.Warnings)
		}
		if _, err := r.Status(bg); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	dir, _ := historyRepo(t)
	quiet("ordinary repository", dir)
	quiet("subdirectory of an ordinary repository", filepath.Join(dir, "src"))

	linked := filepath.Join(filepath.Dir(dir), "linked")
	gitCmd(t, dir, "worktree", "add", "-q", linked, "feature")
	quiet("linked worktree", linked)

	separate, _ := filepath.EvalSymlinks(t.TempDir())
	work := filepath.Join(separate, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, work, "init", "-q", "-b", "main", "--separate-git-dir", filepath.Join(separate, "store.git"))
	write(t, work, "a.txt", "a\n")
	commit(t, work, "first")
	// git records nothing in a separate git directory that leads back to the
	// working tree, so this layout cannot be told from a borrowed one: it
	// gets a notice that says so, until the owner records the working tree.
	sep := open(t, work, Options{})
	if len(sep.Warnings) != 1 || !strings.Contains(sep.Warnings[0], "separate-git-dir") || strings.Contains(sep.Warnings[0], "\n") {
		t.Errorf("separate git directory without a back-reference: %q", sep.Warnings)
	}
	gitCmd(t, work, "config", "core.worktree", work)
	quiet("separate git directory that names its working tree", work)

	fx := submoduleFixture(t)
	quiet("superproject", fx.super)
	quiet("submodule checkout", fx.sub)

	// A folder whose .git file names the first repository's git directory.
	foreign, _ := filepath.EvalSymlinks(t.TempDir())
	write(t, foreign, "mine.txt", "this folder's own file\n")
	write(t, foreign, ".git", "gitdir: "+filepath.Join(dir, ".git")+"\n")
	if out := gitCmd(t, foreign, "log", "--format=%s", "-1"); out != "merge feature" {
		t.Fatalf("positive control: git does not serve the other repository's history here: %q", out)
	}
	r := open(t, foreign, Options{})
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], ".git") || !strings.Contains(r.Warnings[0], "another") {
		t.Fatalf("a git directory that belongs elsewhere must be reported, got %q", r.Warnings)
	}
	if strings.Contains(r.Warnings[0], "\n") || strings.Contains(r.Warnings[0], dir) || strings.Contains(r.Warnings[0], foreign) {
		t.Errorf("the warning must be one line and name no path: %q", r.Warnings[0])
	}
	// It is not refused: the folder is served as git sees it.
	if r.Root != foreign || r.GitDir != filepath.Join(dir, ".git") {
		t.Errorf("root %s, git directory %s", r.Root, r.GitDir)
	}
	if lg, err := r.Log(bg, LogOptions{Limit: 1}); err != nil || !strings.Contains(lg.Text, "merge feature") {
		t.Errorf("log: %v", err)
	}

	// The .git file of a linked worktree, copied into another folder: the
	// back-reference names the original worktree, not this one.
	copied, _ := filepath.EvalSymlinks(t.TempDir())
	pointer, err := os.ReadFile(filepath.Join(linked, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	write(t, copied, ".git", string(pointer))
	if r := open(t, copied, Options{}); len(r.Warnings) != 1 {
		t.Errorf("a copied worktree pointer must be reported, got %q", r.Warnings)
	}
	// The same for the pointer of a submodule checkout.
	stolen, _ := filepath.EvalSymlinks(t.TempDir())
	write(t, stolen, ".git", "gitdir: "+filepath.Join(fx.super, ".git", "modules", "sub")+"\n")
	if r, err := Open(bg, stolen, Options{}); err == nil {
		defer r.Close()
		if len(r.Warnings) != 1 {
			t.Errorf("a submodule's git directory used from elsewhere must be reported, got %q", r.Warnings)
		}
	}
}

// ---------------------------------------------------------------------------
// NoHistory
// ---------------------------------------------------------------------------

// With NoHistory only the working tree is served: no tool may return the
// content of a commit (a diff against HEAD shows the removed lines, which
// exist only there), nor anything that describes one (hash, subject, author,
// counts of commits, names of files that only a commit still has).
func TestNoHistoryServesNothingFromCommits(t *testing.T) {
	dir := newDir(t)
	write(t, dir, "kept.txt", "kept\n")
	write(t, dir, "gone.txt", "ONLY-IN-HEAD one\n")
	write(t, dir, "edited.txt", "ONLY-IN-HEAD two\n")
	write(t, dir, "staged-gone.txt", "ONLY-IN-HEAD three\n")
	write(t, dir, "old-name.txt", strings.Repeat("renamed content\n", 20))
	first := commit(t, dir, "COMMITSUBJECT first")
	write(t, dir, "kept.txt", "kept, twice\n")
	head := commit(t, dir, "COMMITSUBJECT second")
	gitCmd(t, dir, "update-ref", "refs/remotes/origin/main", first)
	gitCmd(t, dir, "config", "remote.origin.url", "https://example.invalid/x.git")
	gitCmd(t, dir, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	gitCmd(t, dir, "config", "branch.main.remote", "origin")
	gitCmd(t, dir, "config", "branch.main.merge", "refs/heads/main")

	os.Remove(filepath.Join(dir, "gone.txt"))
	write(t, dir, "edited.txt", "new text\n")
	gitCmd(t, dir, "rm", "-q", "staged-gone.txt")
	gitCmd(t, dir, "mv", "old-name.txt", "new-name.txt")
	write(t, dir, "added.txt", "added\n")
	gitCmd(t, dir, "add", "added.txt")

	// Positive control: with history on, these are all visible.
	full := open(t, dir, Options{})
	d, err := full.Diff(bg, DiffOptions{Context: -1}, full.Budget())
	if err != nil {
		t.Fatal(err)
	}
	st, err := full.Status(bg)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, d.Text, "-ONLY-IN-HEAD one", "-ONLY-IN-HEAD two", "-ONLY-IN-HEAD three")
	mustContain(t, st.Text, head[:10], "ahead 1", "staged-gone.txt", "old-name.txt -> new-name.txt")

	r := open(t, dir, Options{NoHistory: true})
	b := r.Budget()
	for name, o := range map[string]DiffOptions{
		"head": {}, "head stat": {StatOnly: true}, "head paths": {Paths: []string{"gone.txt"}},
		"explicit head": {Mode: DiffHead}, "staged": {Mode: DiffStaged}, "worktree": {Mode: DiffWorktree},
		"range": {Base: "HEAD~1"}, "range mode": {Mode: DiffRange, Base: "HEAD~1", Target: "HEAD"}, "merge base": {Base: "HEAD~1", MergeBase: true},
		"unknown mode": {Mode: "sideways"},
	} {
		o.Context = -1
		d, err := r.Diff(bg, o, b)
		if err != ErrNoHistory {
			t.Errorf("diff %s under NoHistory: want ErrNoHistory, got %v\n%v", name, err, d)
		}
	}
	if _, err := r.Log(bg, LogOptions{}); err != ErrNoHistory {
		t.Errorf("log: %v", err)
	}
	if _, err := r.Show(bg, "HEAD", nil, false, -1, b); err != ErrNoHistory {
		t.Errorf("show: %v", err)
	}
	if _, err := r.Refs(bg, ""); err != ErrNoHistory {
		t.Errorf("refs: %v", err)
	}
	if _, err := r.Blame(bg, "kept.txt", 1, 1, ""); err != ErrNoHistory {
		t.Errorf("blame: %v", err)
	}
	if _, err := r.Blame(bg, "kept.txt", 1, 1, "HEAD"); err != ErrNoHistory {
		t.Errorf("blame at a revision: %v", err)
	}
	if _, err := r.ReadFile(bg, "gone.txt", 1, 5, "HEAD", b); err != ErrNoHistory {
		t.Errorf("read at a revision: %v", err)
	}
	if _, err := r.ResolveCommit(bg, "HEAD"); err != ErrNoHistory {
		t.Errorf("resolve: %v", err)
	}

	// What stays available describes the working tree and the index only.
	st, err = r.Status(bg)
	if err != nil {
		t.Fatal(err)
	}
	ov, err := r.Overview(bg)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := r.ReadFile(bg, "kept.txt", 1, 5, "", b)
	if err != nil {
		t.Fatal(err)
	}
	ls, err := r.List(bg, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	all := st.Text + ov.Text + rd.Text + ls.Text
	mustNotContain(t, all, head[:7], first[:7], "COMMITSUBJECT", "ONLY-IN-HEAD", "Ada", "Last commit", "ahead", "behind",
		"staged-gone.txt", "old-name.txt")
	if st.Head != "" || ov.Head != "" {
		t.Errorf("the HEAD commit id must not be reported under NoHistory: status %q, overview %q", st.Head, ov.Head)
	}
	mustContain(t, st.Text, "BRANCH main", "upstream origin/main", "A  added.txt", "R  new-name.txt", "D  gone.txt", "M  edited.txt",
		"1 staged deletion(s) not named")
	mustContain(t, ov.Text, "BRANCH main", "history: disabled")
	mustContain(t, rd.Text, "kept, twice")
	if st.Staged != 3 || st.Unstaged != 2 {
		t.Errorf("counts still describe the working tree: %+v", st)
	}

	// A detached HEAD is described without its commit.
	gitCmd(t, dir, "stash", "push", "-q", "--include-untracked")
	gitCmd(t, dir, "checkout", "-q", "--detach", head)
	fresh(r)
	st, err = r.Status(bg)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, st.Text, "HEAD detached\n")
	mustNotContain(t, st.Text, head[:7])
}
