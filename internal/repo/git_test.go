package repo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// historyRepo builds a small history:
//
//	c1 initial (README, src/main.go, .env, config/secrets.yaml)
//	c2 only a withheld file changes (credentials.json)
//	c3 src/main.go and .env change together
//	c4 rename src/main.go → src/app.go
//	feature branch off c1 with one commit, merged into main (merge commit)
func historyRepo(t *testing.T) (dir string, oids map[string]string) {
	dir = newDir(t)
	oids = map[string]string{}
	write(t, dir, "README.md", "# Demo\n")
	write(t, dir, "src/main.go", "package main\n\nfunc main() {\n\tprintln(\"one\")\n}\n")
	write(t, dir, ".env", "PASSWORD=NOPE-TOP-SECRET-1\n")
	write(t, dir, "config/secrets.yaml", "db: NOPE-TOP-SECRET-1\n")
	oids["c1"] = commit(t, dir, "initial commit")

	write(t, dir, "credentials.json", "{\"key\": \"NOPE-TOP-SECRET-2\"}\n")
	oids["c2"] = commit(t, dir, "add credentials")

	write(t, dir, "src/main.go", "package main\n\nfunc main() {\n\tprintln(\"two\")\n}\n")
	write(t, dir, ".env", "PASSWORD=NOPE-TOP-SECRET-3\n")
	oids["c3"] = commit(t, dir, "change greeting and env")

	gitCmd(t, dir, "mv", "src/main.go", "src/app.go")
	oids["c4"] = commit(t, dir, "rename main to app")

	gitCmd(t, dir, "checkout", "-q", "-b", "feature", oids["c1"])
	write(t, dir, "docs/feature.md", "feature docs\n")
	oids["f1"] = commit(t, dir, "feature work")
	gitCmd(t, dir, "checkout", "-q", "main")
	gitCmd(t, dir, "merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	oids["merge"] = gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "tag", "v1.0", oids["c3"])
	return dir, oids
}

func TestStatus(t *testing.T) {
	dir, _ := historyRepo(t)
	write(t, dir, "README.md", "# Demo changed\n")
	write(t, dir, "staged.txt", "staged\n")
	gitCmd(t, dir, "add", "staged.txt")
	write(t, dir, "new.txt", "new\n")
	write(t, dir, ".env", "PASSWORD=changed\n")
	write(t, dir, "prod.tfvars", "x = 1\n")
	r := open(t, dir, Options{})
	res, err := r.Status(bg)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, res.Text, "BRANCH main at", "STAGED (index vs HEAD) (1)", "A  staged.txt", "UNSTAGED (working tree vs index) (1)",
		"M  README.md", "UNTRACKED (not ignored) (1)", "new.txt", "2 changed path(s) withheld by policy")
	mustNotContain(t, res.Text, ".env", "tfvars")
	if res.Staged != 1 || res.Unstaged != 1 || res.Untracked != 1 || res.Withheld != 2 {
		t.Errorf("counts: %+v", res)
	}
	named := open(t, dir, Options{ShowWithheldNames: true})
	res, _ = named.Status(bg)
	mustContain(t, res.Text, ".env  [withheld]", "prod.tfvars  [withheld]")
}

func TestUnbornAndDetached(t *testing.T) {
	dir := newDir(t)
	write(t, dir, "a.txt", "a\n")
	r := open(t, dir, Options{})
	st, err := r.Status(bg)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, st.Text, "no commits yet", "a.txt")
	ov, err := r.Overview(bg)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, ov.Text, "no commits yet", "1 served")
	lg, err := r.Log(bg, LogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, lg.Text, "no commits yet")
	gitCmd(t, dir, "add", "a.txt")
	df, err := r.Diff(bg, DiffOptions{Mode: DiffStaged, Context: -1}, r.Budget())
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, df.Text, "a.txt", "+a")
	if _, err := r.ResolveCommit(bg, "HEAD"); err == nil {
		t.Error("HEAD does not resolve in an unborn repository")
	}

	oid := commit(t, dir, "first")
	gitCmd(t, dir, "checkout", "-q", "--detach", oid)
	fresh(r)
	st, _ = r.Status(bg)
	mustContain(t, st.Text, "HEAD detached at "+oid[:10])
}

func TestLog(t *testing.T) {
	dir, oids := historyRepo(t)
	r := open(t, dir, Options{})

	res, err := r.Log(bg, LogOptions{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 3 || !res.More || res.NextSkip != 3 {
		t.Errorf("paging: %+v", res)
	}
	mustContain(t, res.Text, "merge feature", "[merge]", "Ada", "2026-01-02", "skip=3")
	res, _ = r.Log(bg, LogOptions{Limit: 10, Skip: 3})
	if res.More || res.Count != 3 {
		t.Errorf("second page: %+v\n%s", res, res.Text)
	}

	res, _ = r.Log(bg, LogOptions{Path: "src"})
	mustContain(t, res.Text, "rename main to app", "change greeting", "initial commit")
	mustNotContain(t, res.Text, "add credentials", "feature work")

	res, _ = r.Log(bg, LogOptions{Grep: "GREETING"})
	if res.Count != 1 {
		t.Errorf("grep: %s", res.Text)
	}
	res, _ = r.Log(bg, LogOptions{Range: "v1.0..main"})
	mustContain(t, res.Text, "rename main to app", "merge feature", "feature work")
	mustNotContain(t, res.Text, "initial commit", "change greeting")
	res, _ = r.Log(bg, LogOptions{Rev: "feature", Author: "ada@example"})
	if res.Count != 2 {
		t.Errorf("rev+author: %s", res.Text)
	}
	res, _ = r.Log(bg, LogOptions{Since: "2030-01-01"})
	mustContain(t, res.Text, "no commits match")

	// with_stats lists only served paths and counts the withheld ones.
	res, _ = r.Log(bg, LogOptions{WithStats: true, Rev: oids["c3"]})
	mustContain(t, res.Text, "M src/main.go +1 −1", "(+ 1 withheld path(s))", "A README.md")
	mustNotContain(t, res.Text, ".env", "credentials.json", "secrets.yaml")

	for name, bad := range map[string]LogOptions{
		"withheld path": {Path: ".env"},
		"bad date":      {Since: "$(rm -rf)"},
		"three dots":    {Range: "a...b"},
		"rev and range": {Rev: "main", Range: "a..b"},
		"unknown rev":   {Rev: "nope"},
		"option-like":   {Rev: "--all"},
		"pickaxe":       {Pickaxe: "println"},
	} {
		if _, err := r.Log(bg, bad); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := r.Log(bg, LogOptions{Pickaxe: "println"}); err == nil || !strings.Contains(err.Error(), "masking") {
		t.Errorf("pickaxe must explain why it is unavailable: %v", err)
	}

	// With masking off the owner gets pickaxe, restricted to served paths.
	raw := open(t, dir, Options{NoRedact: true})
	res, err = raw.Log(bg, LogOptions{Pickaxe: "\"two\""})
	if err != nil || res.Count != 1 {
		t.Errorf("pickaxe: %v %s", err, res.Text)
	}
	res, _ = raw.Log(bg, LogOptions{Pickaxe: "NOPE-TOP-SECRET-2"})
	if res.Count != 0 {
		t.Errorf("pickaxe must not report commits that only touch withheld paths:\n%s", res.Text)
	}

	none := open(t, dir, Options{NoHistory: true})
	if _, err := none.Log(bg, LogOptions{}); err != ErrNoHistory {
		t.Errorf("--no-history: %v", err)
	}
}

// A commit that touches only withheld paths must produce no content, and an
// empty set of allowed paths must never turn into "diff everything".
func TestDiffNeverWidensToWithheldPaths(t *testing.T) {
	dir, oids := historyRepo(t)
	r := open(t, dir, Options{})
	b := r.Budget()

	show, err := r.Show(bg, oids["c2"], nil, false, -1, b)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, show.Text, "add credentials", "0 file(s) changed", "1 changed path(s) withheld by policy")
	mustNotContain(t, show.Text, "NOPE", "credentials.json", "diff --git")
	if show.Diff.Withheld != 1 || show.Diff.Shown != 0 {
		t.Errorf("counts: %+v", show.Diff)
	}

	show, _ = r.Show(bg, oids["c3"], nil, false, -1, b)
	mustContain(t, show.Text, "M  src/main.go  +1 −1", "+\tprintln(\"two\")", "1 changed path(s) withheld")
	mustNotContain(t, show.Text, "NOPE", ".env", "PASSWORD")

	diff, err := r.Diff(bg, DiffOptions{Base: oids["c1"], Target: oids["c3"], Context: -1}, b)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, diff.Text, "src/main.go", "2 changed path(s) withheld")
	mustNotContain(t, diff.Text, "NOPE", "credentials", ".env")

	if _, err := r.Diff(bg, DiffOptions{Base: oids["c1"], Target: oids["c3"], Paths: []string{".env"}, Context: -1}, b); err == nil {
		t.Error("asking for a withheld path must be refused")
	}
	if _, err := r.Show(bg, oids["c2"], []string{"credentials.json"}, false, -1, b); err == nil {
		t.Error("asking for a withheld path must be refused")
	}

	// Working tree: only a withheld file is modified.
	write(t, dir, ".env", "PASSWORD=NOPE-TOP-SECRET-4\n")
	fresh(r)
	for _, mode := range []string{DiffHead, DiffWorktree} {
		d, err := r.Diff(bg, DiffOptions{Mode: mode, Context: -1}, b)
		if err != nil {
			t.Fatal(err)
		}
		mustContain(t, d.Text, "0 file(s) changed", "withheld")
		mustNotContain(t, d.Text, "NOPE", "PASSWORD", "diff --git")
	}

	// With names opted in, names appear but never content.
	named := open(t, dir, Options{ShowWithheldNames: true})
	show, _ = named.Show(bg, oids["c2"], nil, false, -1, b)
	mustContain(t, show.Text, "credentials.json  [withheld]")
	mustNotContain(t, show.Text, "NOPE")
}

func TestDiffModesAndShow(t *testing.T) {
	dir, oids := historyRepo(t)
	r := open(t, dir, Options{})
	b := r.Budget()

	show, err := r.Show(bg, oids["c4"], nil, false, -1, b)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, show.Text, "R  src/main.go → src/app.go", "rename main to app", "parent:")

	show, _ = r.Show(bg, oids["merge"], nil, false, -1, b)
	mustContain(t, show.Text, "merge commit; changes are shown against the FIRST parent", "docs/feature.md")
	show, _ = r.Show(bg, oids["c1"], nil, true, -1, b)
	mustContain(t, show.Text, "root commit", "A  README.md", "A  src/main.go")
	mustNotContain(t, show.Text, "diff --git", "NOPE")

	d, err := r.Diff(bg, DiffOptions{Base: "feature", Target: "main", MergeBase: true, StatOnly: true, Context: -1}, b)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, d.Text, "merge-base "+gitCmd(t, dir, "merge-base", "feature", "main")[:10])
	d, _ = r.Diff(bg, DiffOptions{Base: oids["c3"], Paths: []string{"src"}, Context: 0}, b)
	mustContain(t, d.Text, "src/main.go → src/app.go")
	mustNotContain(t, d.Text, "docs/feature.md")

	write(t, dir, "README.md", "# Demo\nworking change\n")
	write(t, dir, "src/app.go", "package main\n\nfunc main() {\n\tprintln(\"staged\")\n}\n")
	gitCmd(t, dir, "add", "src/app.go")
	fresh(r)
	d, _ = r.Diff(bg, DiffOptions{Mode: DiffStaged, Context: -1}, b)
	mustContain(t, d.Text, "src/app.go", "+\tprintln(\"staged\")")
	mustNotContain(t, d.Text, "working change")
	d, _ = r.Diff(bg, DiffOptions{Mode: DiffWorktree, Context: -1}, b)
	mustContain(t, d.Text, "+working change")
	mustNotContain(t, d.Text, "staged\")")
	d, _ = r.Diff(bg, DiffOptions{Context: -1}, b)
	mustContain(t, d.Text, "+working change", "+\tprintln(\"staged\")")

	for name, bad := range map[string]DiffOptions{
		"range without base": {Mode: DiffRange},
		"base outside range": {Mode: DiffHead, Base: "main"},
		"unknown mode":       {Mode: "sideways"},
		"unknown base":       {Base: "nope"},
	} {
		bad.Context = -1
		if _, err := r.Diff(bg, bad, b); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}

	// A tiny budget cuts patches with an explicit notice naming what is missing.
	for i := 0; i < 6; i++ {
		write(t, dir, "many/"+string(rune('a'+i))+".txt", strings.Repeat("line of text\n", 400))
	}
	base := commit(t, dir, "many files")
	for i := 0; i < 6; i++ {
		write(t, dir, "many/"+string(rune('a'+i))+".txt", strings.Repeat("changed line\n", 400))
	}
	commit(t, dir, "change many")
	fresh(r)
	d, err = r.Diff(bg, DiffOptions{Base: base, Context: -1}, 40<<10)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Omitted) == 0 {
		t.Errorf("expected omitted patches under a small budget")
	}
	mustContain(t, d.Text, "size budget reached", "NOT shown for")
	if len(d.Text) > 48<<10 {
		t.Errorf("budget exceeded: %d", len(d.Text))
	}
}

// A hunk that shows only the middle of a private key must be masked: key
// spans come from the complete file versions, not from the excerpt.
func TestDiffMasksKeyMaterialOutsideTheHunk(t *testing.T) {
	dir := newDir(t)
	body := strings.Split(strings.TrimSpace(fakePEM), "\n")
	var filler []string
	for i := 0; i < 12; i++ {
		filler = append(filler, "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVphYmNkZWZnaGlqa2xtbm9wcXJzdHV2"[i:]+strings.Repeat("A", i))
	}
	pem := body[0] + "\n" + strings.Join(filler, "\n") + "\n" + body[len(body)-1] + "\n"
	write(t, dir, "notes.txt", "intro\n"+pem+"outro\n")
	base := commit(t, dir, "with key")
	changed := strings.Replace(pem, filler[6], "Q0hBTkdFRENIQU5HRURDSEFOR0VEQ0hBTkdFRENIQU5HRURDSEFOR0VEQ0hBTkdF", 1)
	write(t, dir, "notes.txt", "intro\n"+changed+"outro\n")
	head := commit(t, dir, "rotate one line")

	r := open(t, dir, Options{})
	d, err := r.Diff(bg, DiffOptions{Base: base, Target: head, Context: 1}, r.Budget())
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, d.Text, "notes.txt", "[REDACTED:private-key]")
	for _, secret := range append(filler, "Q0hBTkdFRENIQU5HRURD") {
		if strings.Contains(d.Text, secret[:20]) {
			t.Fatalf("key material leaked through a hunk:\n%s", d.Text)
		}
	}
	if d.Redactions == 0 {
		t.Error("redactions must be counted")
	}

	bl, err := r.Blame(bg, "notes.txt", 6, 9, "")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, bl.Text, "[REDACTED:private-key]")
	for _, secret := range filler {
		if strings.Contains(bl.Text, secret[:20]) {
			t.Fatalf("key material leaked through blame:\n%s", bl.Text)
		}
	}
}

func TestBlame(t *testing.T) {
	dir, oids := historyRepo(t)
	r := open(t, dir, Options{})
	res, err := r.Blame(bg, "src/app.go", 1, 5, "")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, res.Text, "BLAME src/app.go · lines 1-5 of 5", oids["c1"][:10], oids["c3"][:10], "println(\"two\")", "Ada", "change greeting")
	write(t, dir, "src/app.go", "package main\n\nfunc main() {\n\tprintln(\"local\")\n}\n")
	res, _ = r.Blame(bg, "src/app.go", 4, 4, "")
	mustContain(t, res.Text, "uncommitted", "println(\"local\")")
	res, err = r.Blame(bg, "src/main.go", 1, 0, oids["c3"])
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, res.Text, "at "+oids["c3"][:10], "println(\"two\")")

	write(t, dir, "untracked.txt", "x\n")
	fresh(r)
	for path, want := range map[string]string{
		".env": "withheld", "untracked.txt": "untracked", "missing.go": "not a file", "src": "directory",
	} {
		if _, err := r.Blame(bg, path, 1, 5, ""); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("blame %s: %v", path, err)
		}
	}
	if _, err := r.Blame(bg, "src/app.go", 99, 100, ""); err == nil {
		t.Error("a range past the end must be rejected")
	}
	if _, err := r.Blame(bg, ".env", 1, 2, oids["c1"]); err == nil {
		t.Error("withheld paths stay withheld at any revision")
	}
}

func TestReadAtRevision(t *testing.T) {
	dir, oids := historyRepo(t)
	r := open(t, dir, Options{})
	b := r.Budget()
	res, err := r.ReadFile(bg, "src/main.go", 1, 10, oids["c1"], b)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, res.Text, "println(\"one\")", "at "+oids["c1"][:10])
	if res.Rev != oids["c1"] {
		t.Errorf("rev: %s", res.Rev)
	}
	if res, _ := r.ReadFile(bg, "src/main.go", 1, 10, "v1.0", b); !strings.Contains(res.Text, "println(\"two\")") {
		t.Error("tags resolve")
	}
	for path, want := range map[string]string{
		".env":                "withheld",
		"config/secrets.yaml": "withheld",
		"credentials.json":    "withheld",
		"src/app.go":          "does not exist at revision",
		"src":                 "directory",
	} {
		if _, err := r.ReadFile(bg, path, 1, 10, oids["c1"], b); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("read %s@c1: %v", path, err)
		}
	}
}

// Only history reachable from HEAD, branches, remote branches and tags is
// served; the reflog, the stash and dangling commits are not.
func TestRevisionAuthorization(t *testing.T) {
	dir, oids := historyRepo(t)
	// A dangling commit, reachable from nothing.
	tree := gitCmd(t, dir, "rev-parse", "HEAD^{tree}")
	dangling := gitCmd(t, dir, "commit-tree", tree, "-m", "dangling NOPE")
	// A stash.
	write(t, dir, "README.md", "# stashed NOPE\n")
	gitCmd(t, dir, "stash")
	stash := gitCmd(t, dir, "rev-parse", "refs/stash")
	// A commit that is only in the reflog (branch deleted).
	gitCmd(t, dir, "checkout", "-q", "-b", "gone")
	write(t, dir, "gone.txt", "gone\n")
	gone := commit(t, dir, "gone commit")
	gitCmd(t, dir, "checkout", "-q", "main")
	gitCmd(t, dir, "branch", "-q", "-D", "gone")

	r := open(t, dir, Options{})
	for _, ok := range []string{"HEAD", "main", "feature", "v1.0", "HEAD~2", "main^2", oids["c1"], oids["c1"][:8], oids["f1"]} {
		if _, err := r.ResolveCommit(bg, ok); err != nil {
			t.Errorf("%s should resolve: %v", ok, err)
		}
	}
	for name, rev := range map[string]string{
		"dangling": dangling, "stash oid": stash, "stash ref": "refs/stash", "stash": "stash", "reflog": "HEAD@{1}",
		"upstream": "main@{u}", "deleted branch": gone, "range": "main..feature", "option": "--all", "empty": "",
		"path": "HEAD:README.md", "shell": "main;id", "space": "main feature", "unknown": "nope",
	} {
		oid, err := r.ResolveCommit(bg, rev)
		if err == nil {
			t.Errorf("%s (%q) must not be served, resolved to %s", name, rev, oid)
		}
	}
	if _, err := r.Show(bg, dangling, nil, false, -1, r.Budget()); err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Errorf("unreachable commits must be refused with a reason: %v", err)
	}
}

func TestRefs(t *testing.T) {
	dir, oids := historyRepo(t)
	gitCmd(t, dir, "update-ref", "refs/remotes/origin/main", oids["c4"])
	r := open(t, dir, Options{})
	res, err := r.Refs(bg, "")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, res.Text, "LOCAL BRANCHES (2)", " * main", "feature", "REMOTE BRANCHES (1)", "origin/main", "TAGS (1)", "v1.0", oids["c3"][:10])
	res, _ = r.Refs(bg, "FEAT")
	if res.Local != 1 || res.TagRefs != 0 {
		t.Errorf("filter: %+v", res)
	}
}

func marker(t *testing.T) (script, flag string) {
	t.Helper()
	dir := t.TempDir()
	flag = filepath.Join(dir, "executed")
	script = filepath.Join(dir, "evil.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+flag+"'\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, flag
}

func executed(flag string) bool {
	_, err := os.Stat(flag)
	return err == nil
}

// plainGit runs unhardened git, to prove that each hostile fixture really
// does execute its program when nothing prevents it (the positive control).
// It is time-boxed: a hostile helper may never answer.
func plainGit(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	cmd.Run()
}

// Repository-controlled configuration must not make a read operation run a
// program. Every case first shows that plain git DOES run it.
func TestHostileRepositoryConfigDoesNotExecute(t *testing.T) {
	type fixture struct {
		name    string
		prepare func(t *testing.T, dir, script string) (env []string)
		control []string // plain git command that triggers execution
	}
	fixtures := []fixture{
		{"textconv", func(t *testing.T, dir, script string) []string {
			write(t, dir, ".gitattributes", "*.txt diff=evil\n")
			gitCmd(t, dir, "config", "diff.evil.textconv", script)
			return nil
		}, []string{"diff", "HEAD"}},
		{"external diff driver", func(t *testing.T, dir, script string) []string {
			write(t, dir, ".gitattributes", "*.txt diff=evil\n")
			gitCmd(t, dir, "config", "diff.evil.command", script)
			return nil
		}, []string{"diff", "HEAD"}},
		{"diff.external", func(t *testing.T, dir, script string) []string {
			gitCmd(t, dir, "config", "diff.external", script)
			return nil
		}, []string{"diff", "HEAD"}},
		{"clean filter", func(t *testing.T, dir, script string) []string {
			write(t, dir, ".gitattributes", "*.txt filter=evil\n")
			gitCmd(t, dir, "config", "filter.evil.clean", script)
			return nil
		}, []string{"diff", "HEAD"}},
		{"fsmonitor hook", func(t *testing.T, dir, script string) []string {
			gitCmd(t, dir, "config", "core.fsmonitor", script)
			return nil
		}, []string{"status"}},
		{"GIT_EXTERNAL_DIFF in the environment", func(t *testing.T, dir, script string) []string {
			t.Setenv("GIT_EXTERNAL_DIFF", script)
			return []string{"GIT_EXTERNAL_DIFF=" + script}
		}, []string{"diff", "HEAD"}},
	}
	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			dir := newDir(t)
			write(t, dir, "a.txt", "one\n")
			commit(t, dir, "first")
			script, flag := marker(t)
			env := fx.prepare(t, dir, script)
			write(t, dir, "a.txt", "two\n")

			plainGit(t, dir, env, fx.control...)
			if !executed(flag) {
				t.Skipf("positive control did not execute on this git (%s); the negative result would prove nothing", gitCmd(t, dir, "version"))
			}
			os.Remove(flag)

			r := open(t, dir, Options{})
			b := r.Budget()
			if _, err := r.Snapshot(bg); err != nil {
				t.Fatal(err)
			}
			r.Status(bg)
			r.Overview(bg)
			r.Diff(bg, DiffOptions{Context: -1}, b)
			r.Diff(bg, DiffOptions{Mode: DiffWorktree, Context: -1}, b)
			r.Diff(bg, DiffOptions{Mode: DiffStaged, Context: -1}, b)
			r.Show(bg, "HEAD", nil, false, -1, b)
			r.Log(bg, LogOptions{WithStats: true})
			r.Blame(bg, "a.txt", 1, 1, "")
			r.Refs(bg, "")
			r.ReadFile(bg, "a.txt", 1, 5, "HEAD", b)
			r.Search(bg, SearchOptions{Query: "two"}, b)
			if executed(flag) {
				t.Fatalf("%s: a repository-configured program was executed by a read operation", fx.name)
			}
			// And the operations still work.
			d, err := r.Diff(bg, DiffOptions{Context: -1}, b)
			if err != nil || !strings.Contains(d.Text, "+two") {
				t.Fatalf("diff no longer works under hardening: %v\n%v", err, d)
			}
		})
	}
}

// A repository with a promisor remote must not make git fetch (and so run a
// transport helper) when an object is missing.
func TestNoLazyFetchThroughHostileRemote(t *testing.T) {
	dir := newDir(t)
	write(t, dir, "a.txt", "one\n")
	commit(t, dir, "first")
	_, flag := marker(t)
	script := filepath.Join(filepath.Dir(flag), "transport.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+flag+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "config", "remote.origin.url", "ext::"+script)
	gitCmd(t, dir, "config", "remote.origin.promisor", "true")
	gitCmd(t, dir, "config", "remote.origin.partialclonefilter", "blob:none")
	gitCmd(t, dir, "config", "extensions.partialClone", "origin")
	gitCmd(t, dir, "config", "protocol.ext.allow", "always")
	missing := "1234567890123456789012345678901234567890"

	plainGit(t, dir, nil, "cat-file", "-p", missing)
	if !executed(flag) {
		t.Skip("positive control: this git did not attempt a lazy fetch through ext::")
	}
	os.Remove(flag)

	r := open(t, dir, Options{})
	r.gitRaw(bg, 1024, "cat-file", "-p", missing)
	r.gitRaw(bg, 1024, "cat-file", "-t", missing+":a.txt")
	r.ReadFile(bg, "a.txt", 1, 5, missing, r.Budget())
	if executed(flag) {
		t.Fatal("a missing object triggered a fetch through a repository-configured transport")
	}
}

func TestGitEnvironmentIsAnAllowlist(t *testing.T) {
	t.Setenv("CONTROL_PLANE_API_KEY", "sk-should-not-leak")
	t.Setenv("OPENAI_API_KEY", "sk-should-not-leak")
	t.Setenv("GIT_DIR", "/somewhere/else")
	t.Setenv("GIT_SSH_COMMAND", "evil")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "nope")
	env := strings.Join(gitEnv(), "\n")
	mustNotContain(t, env, "sk-should-not-leak", "GIT_DIR=", "GIT_SSH_COMMAND", "AWS_SECRET")
	mustContain(t, env, "GIT_NO_LAZY_FETCH=1", "GIT_ALLOW_PROTOCOL=", "GIT_NO_REPLACE_OBJECTS=1", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "PATH=")
}

func TestRepositoryShapes(t *testing.T) {
	dir, oids := historyRepo(t)

	// Linked worktree: .git is a file.
	wt := filepath.Join(filepath.Dir(dir), "linked")
	gitCmd(t, dir, "worktree", "add", "-q", wt, "feature")
	lr := open(t, wt, Options{})
	ov, err := lr.Overview(bg)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, ov.Text, "BRANCH feature")
	if res, err := lr.ReadFile(bg, "docs/feature.md", 1, 5, "", lr.Budget()); err != nil || !strings.Contains(res.Text, "feature docs") {
		t.Errorf("linked worktree read: %v", err)
	}

	// Submodule entry: listed as an opaque gitlink, never read.
	gitCmd(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+oids["c1"]+",vendor/sub")
	// Sparse / missing working file: tracked but not present.
	os.Remove(filepath.Join(dir, "README.md"))
	// An LFS pointer.
	write(t, dir, "big.bin", "version https://git-lfs.github.com/spec/v1\noid sha256:abc\nsize 12345\n")

	r := open(t, dir, Options{})
	list, err := r.List(bg, ListOptions{Depth: 3})
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, list.Text, "vendor/sub  [submodule, not served]", "README.md  [not in working tree]")
	if _, err := r.ReadFile(bg, "vendor/sub", 1, 5, "", r.Budget()); err == nil || !strings.Contains(err.Error(), "submodule") {
		t.Errorf("submodule read: %v", err)
	}
	if _, err := r.ReadFile(bg, "README.md", 1, 5, "", r.Budget()); err == nil || !strings.Contains(err.Error(), "not present in the working tree") {
		t.Errorf("missing working file: %v", err)
	}
	res, err := r.ReadFile(bg, "big.bin", 1, 5, "", r.Budget())
	if err != nil || !strings.Contains(res.Text, "Git LFS pointer") {
		t.Errorf("LFS pointer: %v %v", err, res)
	}
	sr, _ := r.Search(bg, SearchOptions{Query: "Demo"}, r.Budget())
	mustContain(t, sr.Text, "not present in the working tree")
}

func TestCoreWorktreeCannotRedirect(t *testing.T) {
	dir := fixtureRepo(t)
	other := t.TempDir()
	write(t, other, "stolen.txt", "STOLEN\n")
	gitCmd(t, dir, "config", "core.worktree", other)
	r := open(t, dir, Options{})
	if r.Root != dir {
		t.Fatalf("root redirected to %s", r.Root)
	}
	snap, err := r.Snapshot(bg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snap.Lookup("stolen.txt"); ok {
		t.Fatal("core.worktree redirected the file listing")
	}
	if _, ok := snap.Lookup("README.md"); !ok {
		t.Fatal("the real working tree must be listed")
	}
}
