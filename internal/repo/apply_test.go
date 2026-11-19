package repo

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// newApplyFixture is newChangeFixture with a chance to change the owner's
// checkout (and commit it) before the session starts.
func newApplyFixture(t *testing.T, files map[string]string, prep func(dir string)) *changeFixture {
	t.Helper()
	f := newSessionFixture(t, files)
	if prep != nil {
		prep(f.dir)
		commit(t, f.dir, "prepared")
	}
	s, _ := f.start(t, Options{})
	ws, err := s.Serve(bg)
	if err != nil {
		t.Fatal(err)
	}
	return &changeFixture{f: f, s: s, ws: ws}
}

func (c *changeFixture) branch(t *testing.T) string {
	t.Helper()
	return strings.TrimPrefix(gitCmd(t, c.ws.Root, "symbolic-ref", "HEAD"), "refs/heads/")
}

func (c *changeFixture) head(t *testing.T) string {
	t.Helper()
	return gitCmd(t, c.ws.Root, "rev-parse", "HEAD")
}

func (c *changeFixture) apply(t *testing.T, message string, changes ...Change) *Applied {
	t.Helper()
	a, err := c.s.Apply(bg, "", changes, message)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return a
}

// wsState is everything an apply may change in the workspace: the branch,
// the index with its flags, git's status, and every file on disk.
func wsState(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "HEAD %s\n", gitCmd(t, dir, "rev-parse", "HEAD"))
	fmt.Fprintf(&b, "index\n%s\n", gitCmd(t, dir, "ls-files", "-s", "-v"))
	fmt.Fprintf(&b, "status\n%s\n", gitCmd(t, dir, "status", "--porcelain", "--untracked-files=all"))
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == ".git" {
			return nil
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		entry := fmt.Sprintf("%s %v", rel, info.Mode())
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			entry += fmt.Sprintf(" %q", data)
		}
		files = append(files, entry)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	b.WriteString(strings.Join(files, "\n"))
	return b.String()
}

var freshLine = regexp.MustCompile(`^\s*(\d+):([a-z2-7]{3})\| (.*)$`)

// checkFresh checks that every anchored line of fresh carries the anchor and
// text that ReadFile shows for that line now, and returns the lines it saw
// per file.
func checkFresh(t *testing.T, ws *Repo, fresh string) map[string]map[int]string {
	t.Helper()
	seen := map[string]map[int]string{}
	cur := ""
	var anchors map[int]string
	var shown map[int]string
	for _, l := range strings.Split(strings.TrimSuffix(fresh, "\n"), "\n") {
		if strings.HasPrefix(l, "FILE ") {
			cur = strings.TrimPrefix(l, "FILE ")
			seen[cur] = map[int]string{}
			anchors, shown = nil, nil
			continue
		}
		m := freshLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		if cur == "" {
			t.Fatalf("an anchored line before any FILE header: %q", l)
		}
		if anchors == nil {
			res := readText(t, ws, cur)
			anchors = anchorsOf(t, res.Text)
			shown = map[int]string{}
			for _, rl := range contentLines(res.Text) {
				if rm := freshLine.FindStringSubmatch(rl); rm != nil {
					n, _ := strconv.Atoi(rm[1])
					shown[n] = rm[3]
				}
			}
		}
		n, _ := strconv.Atoi(m[1])
		if anchors[n] != m[2] {
			t.Errorf("%s line %d: fresh anchor %s, read_file shows %s", cur, n, m[2], anchors[n])
		}
		if shown[n] != m[3] {
			t.Errorf("%s line %d: fresh text %q, read_file shows %q", cur, n, m[3], shown[n])
		}
		seen[cur][n] = m[3]
	}
	return seen
}

func TestApplyWritesCommitsAndReturnsFreshAnchors(t *testing.T) {
	c := newApplyFixture(t, map[string]string{
		"a.txt":  numbered(20),
		"run.sh": "#!/bin/sh\necho hi\n",
		"c.txt":  "gone\n",
	}, func(dir string) {
		if err := os.Chmod(filepath.Join(dir, "run.sh"), 0o755); err != nil {
			t.Fatal(err)
		}
	})
	f := c.f
	ownerHead := gitCmd(t, f.dir, "rev-parse", "HEAD")
	ownerStatus := gitCmd(t, f.dir, "status", "--porcelain")
	base := c.head(t)

	plan := c.accept(t,
		Change{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 10), Text: "ten"},
		Change{Op: "replace", Path: "run.sh", From: c.anchor(t, "run.sh", 2), Text: "echo bye"},
		Change{Op: "create", Path: "dir/new.txt", Text: "fresh\nfile\n"},
		Change{Op: "remove", Path: "c.txt", FileTag: c.tag(t, "c.txt")},
	)
	a, err := c.s.Apply(bg, plan.ID, nil, "Edit three files")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	head := c.head(t)
	if head == base {
		t.Fatal("the branch did not move")
	}
	if got := gitCmd(t, c.ws.Root, "rev-parse", "HEAD^"); got != base {
		t.Errorf("parent %s, want %s", got, base)
	}
	if got := gitCmd(t, f.dir, "rev-parse", "refs/heads/"+c.branch(t)); got != head {
		t.Errorf("the session branch is at %s, the workspace HEAD at %s", got, head)
	}
	cp := a.Checkpoint
	if cp.ID != head[:12] || cp.Kind != "change" || cp.Message != "Edit three files" || cp.Time.IsZero() {
		t.Errorf("checkpoint %+v, head %s", cp, head)
	}
	if want := []string{"a.txt", "c.txt", "dir/new.txt", "run.sh"}; strings.Join(cp.Files, ",") != strings.Join(want, ",") {
		t.Errorf("files %q, want %q", cp.Files, want)
	}
	if cp.Added != plan.Added || cp.Removed != plan.Removed || cp.Added != 4 || cp.Removed != 3 {
		t.Errorf("counts +%d -%d, plan +%d -%d", cp.Added, cp.Removed, plan.Added, plan.Removed)
	}

	if got := gitCmd(t, c.ws.Root, "diff", "--name-status", base, head); got != "M\ta.txt\nD\tc.txt\nA\tdir/new.txt\nM\trun.sh" {
		t.Errorf("committed changes:\n%s", got)
	}
	want := strings.Replace(numbered(20), "\n10\n", "\nten\n", 1)
	if got := gitCmd(t, c.ws.Root, "show", head+":a.txt"); got+"\n" != want {
		t.Errorf("committed a.txt = %q", got)
	}
	if got := gitCmd(t, c.ws.Root, "ls-tree", head, "run.sh", "dir/new.txt"); !strings.Contains(got, "100755 blob") || !strings.Contains(got, "100644 blob") {
		t.Errorf("modes in the commit:\n%s", got)
	}
	if fi, err := os.Stat(filepath.Join(c.ws.Root, "run.sh")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("run.sh mode on disk: %v %v", fi.Mode(), err)
	}
	if fi, err := os.Stat(filepath.Join(c.ws.Root, "dir", "new.txt")); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("dir/new.txt mode on disk: %v %v", fi.Mode(), err)
	}
	if exists(filepath.Join(c.ws.Root, "c.txt")) {
		t.Error("c.txt is still on disk")
	}
	if got := gitCmd(t, c.ws.Root, "status", "--porcelain", "--untracked-files=all"); got != "" {
		t.Errorf("workspace status after apply:\n%s", got)
	}

	// Fresh: the touched lines with the anchors read_file shows now.
	seen := checkFresh(t, c.ws, a.Fresh)
	if seen["a.txt"][10] != "ten" || seen["a.txt"][7] != "7" || seen["a.txt"][13] != "13" || seen["a.txt"][6] != "" || seen["a.txt"][14] != "" {
		t.Errorf("a.txt in Fresh: %v\n%s", seen["a.txt"], a.Fresh)
	}
	if seen["run.sh"][2] != "echo bye" || seen["dir/new.txt"][1] != "fresh" || seen["dir/new.txt"][2] != "file" {
		t.Errorf("Fresh:\n%s", a.Fresh)
	}
	mustContain(t, a.Fresh, "FILE c.txt\n[removed]")

	// The owner's checkout and branch are untouched.
	if got := gitCmd(t, f.dir, "rev-parse", "HEAD"); got != ownerHead {
		t.Errorf("owner's HEAD moved to %s", got)
	}
	if got := gitCmd(t, f.dir, "status", "--porcelain"); got != ownerStatus {
		t.Errorf("owner's status:\n%s", got)
	}
	if got := gitCmd(t, f.dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("owner's checkout is on %s", got)
	}
	if got := mustRead(t, filepath.Join(f.dir, "a.txt")); got != numbered(20) {
		t.Error("the owner's a.txt changed")
	}
}

func TestApplyIsAllOrNothing(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n", "c.txt": "c\n"}, nil)
	before := wsState(t, c.ws.Root)
	changes := []Change{
		{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 1), Text: "A"},
		{Op: "replace", Path: "b.txt", From: c.anchor(t, "b.txt", 1), Text: "B"},
		{Op: "create", Path: "new/d.txt", Text: "d"},
	}
	var calls []int
	c.s.applyFault = func(i int) error {
		calls = append(calls, i)
		if i == 1 {
			return errors.New("injected failure")
		}
		return nil
	}
	if _, err := c.s.Apply(bg, "", changes, "x"); err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("Apply: %v", err)
	}
	if fmt.Sprint(calls) != "[0 1]" {
		t.Errorf("fault hook calls %v", calls)
	}
	if after := wsState(t, c.ws.Root); after != before {
		t.Errorf("the workspace changed:\n%s\nwas:\n%s", after, before)
	}

	// A failure on the last file rolls back the ones written before it.
	c.s.applyFault = func(i int) error {
		if i == 2 {
			return errors.New("injected late")
		}
		return nil
	}
	if _, err := c.s.Apply(bg, "", changes, "x"); err == nil || !strings.Contains(err.Error(), "injected late") {
		t.Fatalf("Apply: %v", err)
	}
	if after := wsState(t, c.ws.Root); after != before {
		t.Errorf("the workspace changed:\n%s\nwas:\n%s", after, before)
	}

	// The same change set goes through once nothing fails.
	c.s.applyFault = nil
	c.apply(t, "x", changes...)
	if got := mustRead(t, filepath.Join(c.ws.Root, "new", "d.txt")); got != "d\n" {
		t.Errorf("new/d.txt = %q", got)
	}
}

// The branch moves while the files are being written (another git working
// on it): update-ref refuses, and the files are put back.
func TestBranchMovedDuringApplyRollsBack(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"}, nil)
	branch := c.branch(t)
	other := gitCmd(t, c.ws.Root, "commit-tree", "-p", "HEAD", "-m", "elsewhere", "HEAD^{tree}")
	before := wsState(t, c.ws.Root)
	c.s.applyFault = func(i int) error {
		if i == 1 {
			gitCmd(t, c.ws.Root, "update-ref", "refs/heads/"+branch, other)
		}
		return nil
	}
	_, err := c.s.Apply(bg, "", []Change{
		{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 1), Text: "A"},
		{Op: "replace", Path: "b.txt", From: c.anchor(t, "b.txt", 1), Text: "B"},
	}, "x")
	if err == nil || !strings.Contains(err.Error(), "moved while the files were being written") {
		t.Fatalf("Apply although the branch moved under it: %v", err)
	}
	t.Logf("refused: %v", err)
	if got := c.head(t); got != other {
		t.Errorf("the branch is at %s, not where the other writer put it (%s)", got, other)
	}
	// Same tree, so apart from HEAD nothing differs.
	if after := wsState(t, c.ws.Root); strings.Replace(after, other, "", 1) != strings.Replace(before, gitCmd(t, c.ws.Root, "rev-parse", other+"^"), "", 1) {
		t.Errorf("the workspace changed:\n%s\nwas:\n%s", after, before)
	}
}

// A rollback write that fails is reported as such, and the next call finds
// the workspace restored from the last checkpoint.
func TestFailedRollbackIsRestoredOnTheNextCall(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not stop root")
	}
	c := newApplyFixture(t, map[string]string{"d/a.txt": "a\n", "e.txt": "e\n"}, nil)
	before := wsState(t, c.ws.Root)
	dir := filepath.Join(c.ws.Root, "d")
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	c.s.applyFault = func(i int) error {
		if i == 1 {
			if err := os.Chmod(dir, 0o555); err != nil {
				t.Fatal(err)
			}
			return errors.New("injected failure")
		}
		return nil
	}
	_, err := c.s.Apply(bg, "", []Change{
		{Op: "replace", Path: "d/a.txt", From: c.anchor(t, "d/a.txt", 1), Text: "A"},
		{Op: "replace", Path: "e.txt", From: c.anchor(t, "e.txt", 1), Text: "E"},
	}, "x")
	if err == nil || !strings.Contains(err.Error(), "inconsistent") {
		t.Fatalf("Apply: %v", err)
	}
	t.Logf("refused: %v", err)
	c.s.applyFault = nil
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := c.s.Serve(bg); err != nil {
		t.Fatalf("Serve after the failed rollback: %v", err)
	}
	if after := wsState(t, c.ws.Root); after != before {
		t.Errorf("the workspace was not restored:\n%s\nwas:\n%s", after, before)
	}
}

func TestApplyingAPlanTwiceGivesTheSameCheckpoint(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\nb\n"}, nil)
	base := c.head(t)
	plan := c.accept(t, Change{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 2), Text: "B"})
	a1, err := c.s.Apply(bg, plan.ID, nil, "once")
	if err != nil {
		t.Fatal(err)
	}
	head := c.head(t)
	a2, err := c.s.Apply(bg, plan.ID, nil, "twice")
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if fmt.Sprintf("%+v", a1) != fmt.Sprintf("%+v", a2) {
		t.Errorf("second result differs:\n%+v\n%+v", a1, a2)
	}
	if got := c.head(t); got != head {
		t.Errorf("the second apply moved the branch to %s", got)
	}
	if got := gitCmd(t, c.ws.Root, "rev-list", "--count", base+"..HEAD"); got != "1" {
		t.Errorf("%s commits", got)
	}

	if _, err := c.s.Apply(bg, plan.ID, []Change{{Op: "create", Path: "x.txt", Text: "x"}}, ""); err == nil {
		t.Error("Apply took both a plan and a change set")
	}
	if _, err := c.s.Apply(bg, "", nil, ""); err == nil {
		t.Error("Apply took neither a plan nor a change set")
	}
}

func TestPlanIsRefusedWhenAFileChangedSince(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": numbered(10)}, nil)
	plan := c.accept(t, Change{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 2), Text: "two"})
	// Another change to the same file, far from line 2: the plan's anchors
	// still hold, the file is not the one the plan was made from.
	c.apply(t, "", Change{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 9), Text: "nine"})
	head := c.head(t)
	before := wsState(t, c.ws.Root)
	_, err := c.s.Apply(bg, plan.ID, nil, "")
	var ce *ChangeError
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("Apply: %v", err)
	}
	if c.head(t) != head || wsState(t, c.ws.Root) != before {
		t.Error("a refused plan changed the workspace")
	}
}

func TestPlansExpire(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	clock := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	c.s.now = func() time.Time { return clock }

	old := c.accept(t, Change{Op: "create", Path: "old.txt", Text: "x"})
	clock = clock.Add(30*time.Minute + time.Second)
	if _, err := c.s.Apply(bg, old.ID, nil, ""); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("an expired plan: %v", err)
	}

	recent := c.accept(t, Change{Op: "create", Path: "recent.txt", Text: "x"})
	clock = clock.Add(29 * time.Minute)
	if _, err := c.s.Apply(bg, recent.ID, nil, ""); err != nil {
		t.Fatalf("a plan 29 minutes old: %v", err)
	}

	// At most 32 plans are kept: the oldest goes first.
	var ids []string
	for i := 0; i < 33; i++ {
		clock = clock.Add(time.Second)
		ids = append(ids, c.accept(t, Change{Op: "create", Path: fmt.Sprintf("n%d.txt", i), Text: "x"}).ID)
	}
	if _, err := c.s.Apply(bg, ids[0], nil, ""); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("the 33rd plan back: %v", err)
	}
	if _, err := c.s.Apply(bg, ids[1], nil, ""); err != nil {
		t.Fatalf("the 32nd plan back: %v", err)
	}
	if _, err := c.s.Apply(bg, "0123456789abcdef", nil, ""); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("an unknown plan: %v", err)
	}
}

func TestConcurrentAppliesDoNotInterleave(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 8; i++ {
		files[fmt.Sprintf("f%d.txt", i)] = "one\ntwo\n"
	}
	c := newApplyFixture(t, files, nil)
	base := c.head(t)
	var sets [][]Change
	for i := 0; i < 8; i++ {
		p := fmt.Sprintf("f%d.txt", i)
		sets = append(sets, []Change{
			{Op: "replace", Path: p, From: c.anchor(t, p, 2), Text: fmt.Sprintf("two by %d", i)},
			{Op: "create", Path: fmt.Sprintf("g%d/new.txt", i), Text: "new"},
		})
	}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range sets {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.s.Apply(bg, "", sets[i], fmt.Sprintf("set %d", i))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("set %d: %v", i, err)
		}
	}
	lines := strings.Split(gitCmd(t, c.ws.Root, "rev-list", "--parents", base+"..HEAD"), "\n")
	if len(lines) != 8 {
		t.Fatalf("%d commits:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) != 2 {
			t.Fatalf("commit with %d parents: %s", len(f)-1, l)
		}
		changed := gitCmd(t, c.ws.Root, "diff-tree", "-r", "--name-only", "--no-commit-id", f[1], f[0])
		m := regexp.MustCompile(`^f(\d)\.txt\ng(\d)/new\.txt$`).FindStringSubmatch(changed)
		if m == nil || m[1] != m[2] {
			t.Errorf("commit %s changes:\n%s", f[0], changed)
		}
	}
	for i := 0; i < 8; i++ {
		if got := gitCmd(t, c.ws.Root, "show", fmt.Sprintf("HEAD:f%d.txt", i)); got != fmt.Sprintf("one\ntwo by %d", i) {
			t.Errorf("f%d.txt = %q", i, got)
		}
	}
	if got := gitCmd(t, c.ws.Root, "status", "--porcelain", "--untracked-files=all"); got != "" {
		t.Errorf("status:\n%s", got)
	}
}

func TestWithheldFilesSurviveACommit(t *testing.T) {
	c := newApplyFixture(t, map[string]string{
		".env":            "TOKEN=x\n",
		".repochatignore": "secret/\n",
		"secret/x.txt":    "classified\n",
		"open.txt":        "fine\n",
	}, nil)
	ls := func(rev string) map[string]string {
		out := map[string]string{}
		for _, l := range strings.Split(gitCmd(t, c.ws.Root, "ls-tree", "-r", rev), "\n") {
			meta, path, _ := strings.Cut(l, "\t")
			out[path] = meta
		}
		return out
	}
	base := ls("HEAD")
	c.apply(t, "", Change{Op: "replace", Path: "open.txt", From: c.anchor(t, "open.txt", 1), Text: "changed"})
	after := ls("HEAD")
	for _, p := range []string{".env", ".repochatignore", "secret/x.txt"} {
		if base[p] == "" || after[p] != base[p] {
			t.Errorf("%s: %q in the commit, was %q", p, after[p], base[p])
		}
	}
	if after["open.txt"] == base["open.txt"] {
		t.Error("open.txt did not change")
	}
	for _, p := range []string{".env", "secret/x.txt", "secret"} {
		if exists(filepath.Join(c.ws.Root, p)) {
			t.Errorf("%s is on disk in the workspace", p)
		}
	}
	flags := gitCmd(t, c.ws.Root, "ls-files", "-v", "--", ".env", "secret/x.txt")
	if flags != "S .env\nS secret/x.txt" {
		t.Errorf("index flags:\n%s", flags)
	}
	if got := gitCmd(t, c.ws.Root, "status", "--porcelain", "--untracked-files=all"); got != "" {
		t.Errorf("status:\n%s", got)
	}
}

func TestCommitRunsNoHookAndIsUnsigned(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	marker := filepath.Join(t.TempDir(), "marker")
	script := "#!/bin/sh\ntouch " + marker + "\ncat >/dev/null\nexit 1\n"
	hooks := filepath.Join(c.f.dir, ".git", "hooks")
	for _, h := range []string{"pre-commit", "prepare-commit-msg", "commit-msg", "post-commit", "post-index-change", "reference-transaction", "post-rewrite"} {
		if err := os.WriteFile(filepath.Join(hooks, h), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prog := filepath.Join(t.TempDir(), "gpg.sh")
	if err := os.WriteFile(prog, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, c.f.dir, "config", "core.hooksPath", hooks)
	gitCmd(t, c.f.dir, "config", "commit.gpgsign", "true")
	gitCmd(t, c.f.dir, "config", "gpg.program", prog)
	gitCmd(t, c.f.dir, "config", "gpg.format", "openpgp")
	gitCmd(t, c.f.dir, "config", "user.signingkey", "ABCDEF")
	base := c.head(t)

	c.apply(t, "signed?", Change{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 1), Text: "A"})
	if exists(marker) {
		t.Fatal("a repository-configured program ran")
	}
	head := c.head(t)
	if head == base {
		t.Fatal("no commit")
	}
	if raw := gitCmd(t, c.ws.Root, "cat-file", "commit", head); strings.Contains(raw, "gpgsig") {
		t.Errorf("the commit is signed:\n%s", raw)
	}
}

func TestCommitCarriesIdentityAndTrailers(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	// author.* outranks user.* in git; the identity is the owner's user.*.
	gitCmd(t, c.f.dir, "config", "author.name", "Mallory")
	gitCmd(t, c.f.dir, "config", "committer.email", "mallory@example.com")
	long := strings.Repeat("x", 150)
	c.apply(t, long+"\nwith details\n", Change{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 1), Text: "A"})
	if got := gitCmd(t, c.ws.Root, "log", "-1", "--format=%an <%ae>|%cn <%ce>"); got != "Ada <ada@example.com>|Ada <ada@example.com>" {
		t.Errorf("identity %q", got)
	}
	msg := gitCmd(t, c.ws.Root, "log", "-1", "--format=%B")
	if want := strings.Repeat("x", 100) + "\n\nwith details\n\nMade-through: repochat\nRepochat-Kind: change"; msg != want {
		t.Errorf("message:\n%s\nwant:\n%s", msg, want)
	}

	// No identity configured: repochat's own.
	gitCmd(t, c.f.dir, "config", "--unset", "user.name")
	gitCmd(t, c.f.dir, "config", "--unset", "user.email")
	a := c.apply(t, "", Change{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 1), Text: "AA"})
	if got := gitCmd(t, c.ws.Root, "log", "-1", "--format=%an <%ae>|%cn <%ce>"); got != "repochat <repochat@localhost>|repochat <repochat@localhost>" {
		t.Errorf("fallback identity %q", got)
	}
	if got := gitCmd(t, c.ws.Root, "log", "-1", "--format=%B"); got != "Changes through repochat\n\nMade-through: repochat\nRepochat-Kind: change" {
		t.Errorf("default message:\n%s", got)
	}
	if a.Checkpoint.Message != "Changes through repochat" {
		t.Errorf("checkpoint message %q", a.Checkpoint.Message)
	}
}

func TestIdentityIsSanitised(t *testing.T) {
	for _, tc := range []struct{ in, fallback, want string }{
		{"Ada", "repochat", "Ada"},
		{"  Ada <evil@x>\nMallory  ", "repochat", "Ada evil@x Mallory"},
		{"<>\n\t", "repochat", "repochat"},
		{"", "repochat@localhost", "repochat@localhost"},
	} {
		if got := cleanIdentity(tc.in, tc.fallback); got != tc.want {
			t.Errorf("cleanIdentity(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSymlinkSwappedInDuringApplyIsNotFollowed(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "sub/b.txt": "b\n"}, nil)
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, outside, "b.txt", "b\n")
	sub := filepath.Join(c.ws.Root, "sub")
	head := c.head(t)
	c.s.applyFault = func(i int) error {
		if i == 1 { // a.txt is written; sub/b.txt is next
			if err := os.Rename(sub, sub+".moved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, sub); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	_, err = c.s.Apply(bg, "", []Change{
		{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 1), Text: "A"},
		{Op: "replace", Path: "sub/b.txt", From: c.anchor(t, "sub/b.txt", 1), Text: "B"},
		{Op: "create", Path: "sub/c.txt", Text: "c"},
	}, "x")
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("Apply: %v", err)
	}
	t.Logf("refused: %v", err)
	if got := names(t, outside); strings.Join(got, ",") != "b.txt" {
		t.Errorf("outside directory holds %v", got)
	}
	if got := mustRead(t, filepath.Join(outside, "b.txt")); got != "b\n" {
		t.Errorf("outside b.txt = %q", got)
	}
	if got := mustRead(t, filepath.Join(c.ws.Root, "a.txt")); got != "a\n" {
		t.Errorf("a.txt was not rolled back: %q", got)
	}
	if c.head(t) != head {
		t.Error("the branch moved")
	}
}
