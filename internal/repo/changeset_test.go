package repo

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// changeFixture is an open session over an owner repository with the given
// committed files.
type changeFixture struct {
	f  *sessionFixture
	s  *Sessions
	ws *Repo
}

func newChangeFixture(t *testing.T, files map[string]string, opts Options) *changeFixture {
	t.Helper()
	f := newSessionFixture(t, files)
	s, _ := f.start(t, opts)
	ws, err := s.Serve(bg)
	if err != nil {
		t.Fatal(err)
	}
	return &changeFixture{f: f, s: s, ws: ws}
}

// anchor returns the anchor of line n of path as read_file shows it.
func (c *changeFixture) anchor(t *testing.T, path string, n int) string {
	t.Helper()
	a, ok := anchorsOf(t, readText(t, c.ws, path).Text)[n]
	if !ok {
		t.Fatalf("%s has no line %d", path, n)
	}
	return fmt.Sprintf("%d:%s", n, a)
}

func (c *changeFixture) tag(t *testing.T, path string) string {
	t.Helper()
	tag := readText(t, c.ws, path).FileTag
	if tag == "" {
		t.Fatalf("%s has no file tag", path)
	}
	return tag
}

func (c *changeFixture) accept(t *testing.T, changes ...Change) *Plan {
	t.Helper()
	p, err := c.s.Propose(bg, changes)
	if err != nil {
		t.Fatalf("Propose refused: %v", err)
	}
	return p
}

// refuse checks that the change set is refused with a *ChangeError whose
// text contains want.
func (c *changeFixture) refuse(t *testing.T, want string, changes ...Change) *ChangeError {
	t.Helper()
	p, err := c.s.Propose(bg, changes)
	if err == nil {
		t.Fatalf("Propose accepted %+v (plan %s, %d files)", changes, p.ID, len(p.Files))
	}
	var ce *ChangeError
	if !errors.As(err, &ce) {
		t.Fatalf("error %T %v is not a *ChangeError", err, err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err, want)
	}
	t.Logf("refused: %s", strings.SplitN(err.Error(), "\n", 2)[0])
	return ce
}

func plannedFile(t *testing.T, p *Plan, path string) PlannedFile {
	t.Helper()
	for _, f := range p.Files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("the plan has no file %s: %+v", path, p.Files)
	return PlannedFile{}
}

func afterOf(t *testing.T, p *Plan, path string) string {
	t.Helper()
	f := plannedFile(t, p, path)
	if f.After == nil {
		t.Fatalf("%s has no new content", path)
	}
	return string(f.After)
}

func numbered(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%d\n", i)
	}
	return b.String()
}

func TestProposeBuildsTheNewContent(t *testing.T) {
	c := newChangeFixture(t, map[string]string{
		"a.txt": numbered(10),
		"c.txt": "x\ny\n",
		"d.txt": "moved\n",
	}, Options{})
	plan := c.accept(t,
		Change{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 2), To: c.anchor(t, "a.txt", 3), Text: "two\nthree\n"},
		Change{Op: "insert", Path: "a.txt", At: c.anchor(t, "a.txt", 5), Where: "before", Text: "four.5"},
		Change{Op: "insert", Path: "a.txt", At: c.anchor(t, "a.txt", 7), Text: "seven+"},
		Change{Op: "delete", Path: "a.txt", From: c.anchor(t, "a.txt", 9)},
		Change{Op: "insert", Path: "a.txt", At: "start", Text: "zero"},
		Change{Op: "insert", Path: "a.txt", At: "end", Text: "eleven\n"},
		Change{Op: "create", Path: "new/b.txt", Text: "hello\nworld"},
		Change{Op: "remove", Path: "c.txt", FileTag: c.tag(t, "c.txt")},
		Change{Op: "move", Path: "d.txt", ToPath: "e/d.txt"},
	)
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(plan.ID) {
		t.Errorf("ID = %q", plan.ID)
	}
	var paths []string
	for _, f := range plan.Files {
		paths = append(paths, f.Path)
	}
	if got := strings.Join(paths, " "); got != "a.txt c.txt d.txt e/d.txt new/b.txt" {
		t.Errorf("files = %s", got)
	}
	a := plannedFile(t, plan, "a.txt")
	if string(a.Before) != numbered(10) {
		t.Errorf("a.txt Before = %q", a.Before)
	}
	if want := "zero\n1\ntwo\nthree\n4\nfour.5\n5\n6\n7\nseven+\n8\n10\neleven\n"; string(a.After) != want {
		t.Errorf("a.txt After = %q, want %q", a.After, want)
	}
	if got, want := fmt.Sprint(a.Regions), "[[0 1] [2 4] [5 6] [9 10] [11 11] [12 13]]"; got != want {
		t.Errorf("a.txt Regions = %s, want %s", got, want)
	}
	b := plannedFile(t, plan, "new/b.txt")
	if b.Before != nil || string(b.After) != "hello\nworld\n" || fmt.Sprint(b.Regions) != "[[0 2]]" {
		t.Errorf("new/b.txt = %q → %q %v", b.Before, b.After, b.Regions)
	}
	if f := plannedFile(t, plan, "c.txt"); string(f.Before) != "x\ny\n" || f.After != nil {
		t.Errorf("c.txt = %q → %q", f.Before, f.After)
	}
	if f := plannedFile(t, plan, "d.txt"); string(f.Before) != "moved\n" || f.After != nil {
		t.Errorf("d.txt = %q → %q", f.Before, f.After)
	}
	if f := plannedFile(t, plan, "e/d.txt"); f.Before != nil || string(f.After) != "moved\n" {
		t.Errorf("e/d.txt = %q → %q", f.Before, f.After)
	}
	// a.txt: +6 −3; new/b.txt: +2; c.txt: −2.
	if plan.Added != 8 || plan.Removed != 5 {
		t.Errorf("Added %d Removed %d", plan.Added, plan.Removed)
	}
	mustContain(t, plan.Diff, "--- a/a.txt\n+++ b/a.txt\n", "-2\n-3\n+two\n+three\n", "+four.5\n", "-9\n",
		"--- /dev/null\n+++ b/new/b.txt\n", "+hello\n", "--- a/c.txt\n+++ /dev/null\n",
		"rename from d.txt\nrename to e/d.txt\n")
	if len(plan.Warnings) != 0 {
		t.Errorf("warnings: %q", plan.Warnings)
	}

	// An edited file can be moved in the same change set: its anchors are
	// those of its old path.
	c2 := newChangeFixture(t, map[string]string{"old.txt": "a\nb\n"}, Options{})
	plan = c2.accept(t,
		Change{Op: "replace", Path: "old.txt", From: c2.anchor(t, "old.txt", 2), Text: "B"},
		Change{Op: "move", Path: "old.txt", ToPath: "dir/new.txt"},
	)
	if got := afterOf(t, plan, "dir/new.txt"); got != "a\nB\n" {
		t.Errorf("moved and edited: %q", got)
	}
}

func TestProposeNeedsASession(t *testing.T) {
	f := newSessionFixture(t, nil)
	s := f.sessions(t, Options{})
	if _, err := s.Propose(bg, []Change{{Op: "create", Path: "x.txt", Text: "x"}}); !errors.Is(err, ErrNoSession) {
		t.Errorf("err = %v, want ErrNoSession", err)
	}
}

func TestProposeWritesNothing(t *testing.T) {
	c := newChangeFixture(t, map[string]string{"a.txt": numbered(5), "c.txt": "x\n", "d.txt": "d\n"}, Options{})
	ws := c.ws.Root
	state := func() string {
		var b strings.Builder
		b.WriteString(gitCmd(t, ws, "rev-parse", "HEAD^{tree}") + "\n")
		b.WriteString(gitCmd(t, ws, "status", "--porcelain", "--untracked-files=all", "--ignored") + "\n")
		b.WriteString(gitCmd(t, ws, "ls-files", "-s") + "\n")
		filepath.Walk(ws, func(p string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(ws, p)
			if rel == ".git" {
				return nil
			}
			fmt.Fprintf(&b, "%s %v %d %d\n", rel, fi.Mode(), fi.Size(), fi.ModTime().UnixNano())
			return nil
		})
		return b.String()
	}
	before := state()
	c.accept(t,
		Change{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 1), Text: "one"},
		Change{Op: "create", Path: "n/new.txt", Text: "new"},
		Change{Op: "remove", Path: "c.txt", FileTag: c.tag(t, "c.txt")},
		Change{Op: "move", Path: "d.txt", ToPath: "m/d.txt"},
	)
	if after := state(); after != before {
		t.Errorf("the workspace changed:\n%s\nwas:\n%s", after, before)
	}
}

func TestStaleAnchorRefusesEverythingAndReturnsFreshLines(t *testing.T) {
	c := newChangeFixture(t, map[string]string{"a.txt": numbered(10), "b.txt": "b\n"}, Options{})
	good := Change{Op: "create", Path: "other.txt", Text: "x"}
	fresh := func(t *testing.T, from, to int) string {
		var b strings.Builder
		for n := from; n <= to; n++ {
			a := strings.SplitN(c.anchor(t, "a.txt", n), ":", 2)[1]
			fmt.Fprintf(&b, "%*d:%s| %d\n", len(strconv.Itoa(to)), n, a, n)
		}
		return b.String()
	}

	// A hash that differs.
	real := c.anchor(t, "a.txt", 5)
	wrong := real[:len(real)-1] + map[bool]string{true: "b", false: "a"}[strings.HasSuffix(real, "a")]
	ce := c.refuse(t, "a.txt", good, Change{Op: "replace", Path: "a.txt", From: wrong, Text: "five"})
	if ce.Path != "a.txt" {
		t.Errorf("Path = %q", ce.Path)
	}
	if want := fresh(t, 2, 8); !strings.Contains(ce.Fresh, want) {
		t.Errorf("Fresh = %q, want it to contain %q", ce.Fresh, want)
	}

	// A line past the end.
	ce = c.refuse(t, "a.txt", Change{Op: "insert", Path: "a.txt", At: "40:abc", Text: "x"})
	if want := fresh(t, 4, 10); !strings.Contains(ce.Fresh, want) {
		t.Errorf("Fresh = %q, want it to contain %q", ce.Fresh, want)
	}

	// The file changed behind the model's back: the old anchor is stale.
	old := c.anchor(t, "a.txt", 3)
	if err := os.WriteFile(filepath.Join(c.ws.Root, "a.txt"), []byte("1\n2\nTHREE\n4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ce = c.refuse(t, "a.txt", Change{Op: "delete", Path: "a.txt", From: old})
	if !strings.Contains(ce.Fresh, "| THREE\n") || strings.Contains(ce.Fresh, "| 5\n") {
		t.Errorf("Fresh = %q", ce.Fresh)
	}
}

func TestOverlappingRangesAreRefused(t *testing.T) {
	c := newChangeFixture(t, map[string]string{"a.txt": numbered(8)}, Options{})
	at := func(n int) string { return c.anchor(t, "a.txt", n) }
	// The tag a range of three or more lines needs, so that each case is
	// refused for its overlap and not for lacking it.
	tag := c.tag(t, "a.txt")
	rep := func(from, to int) Change {
		return Change{Op: "replace", Path: "a.txt", From: at(from), To: at(to), Text: "x", FileTag: tag}
	}
	ins := func(n int, where string) Change {
		return Change{Op: "insert", Path: "a.txt", At: at(n), Where: where, Text: "x"}
	}
	refused := []struct {
		name    string
		changes []Change
	}{
		{"ranges share a line", []Change{rep(2, 4), {Op: "delete", Path: "a.txt", From: at(4)}}},
		{"same line twice", []Change{rep(3, 3), rep(3, 3)}},
		{"insert inside a range", []Change{rep(2, 4), ins(3, "after")}},
		{"insert at the last line of a range", []Change{rep(2, 4), ins(4, "after")}},
		{"insert at the first line of a range", []Change{rep(2, 4), ins(2, "before")}},
		{"two inserts at one point", []Change{ins(3, "after"), ins(4, "before")}},
		{"start and before line 1", []Change{{Op: "insert", Path: "a.txt", At: "start", Text: "x"}, ins(1, "before")}},
		{"end and after the last line", []Change{{Op: "insert", Path: "a.txt", At: "end", Text: "x"}, ins(8, "after")}},
		{"range backwards", []Change{rep(5, 3)}},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) { c.refuse(t, "a.txt", tc.changes...) })
	}
	plan := c.accept(t, rep(2, 3), rep(4, 5), ins(1, "after"), ins(8, "before"), ins(6, "after"))
	if got, want := afterOf(t, plan, "a.txt"), "1\nx\nx\nx\n6\nx\n7\nx\n8\n"; got != want {
		t.Errorf("adjacent edits: %q, want %q", got, want)
	}
}

func TestLockedLinesCannotBeReplacedOrDeleted(t *testing.T) {
	content := "a\n" +
		"aws_key = \"" + fakeAWSKey + "\"\n" + // 2
		"b\n" + // 3
		fakePEM + // 4-8
		"c\n" + // 9
		strings.Repeat("y", maxLineDisplay+10) + "\n" + // 10: shown cut
		"d\n" + // 11
		strings.Repeat("z", maxLineBytes+100) + "\n" + // 12: cut on reading, anchor ???
		"e\n" // 13
	c := newChangeFixture(t, map[string]string{"s.txt": content}, Options{})
	at := func(n int) string { return c.anchor(t, "s.txt", n) }
	if a := at(12); !strings.HasSuffix(a, ":???") {
		t.Fatalf("line 12 anchor %s, want ???", a)
	}
	const locked = "holds a value you were not shown; the owner has to change it by hand"
	refused := []struct {
		name, want string
		change     Change
	}{
		{"single line", "line 2 " + locked, Change{Op: "replace", Path: "s.txt", From: at(2), Text: "x"}},
		{"inside a range", "line 2 " + locked, Change{Op: "delete", Path: "s.txt", From: at(1), To: at(3)}},
		{"PEM begin", "line 4 " + locked, Change{Op: "delete", Path: "s.txt", From: at(4)}},
		{"PEM body", "line 6 " + locked, Change{Op: "replace", Path: "s.txt", From: at(6), Text: "x"}},
		{"PEM end", "line 8 " + locked, Change{Op: "replace", Path: "s.txt", From: at(8), To: at(9), Text: "x"}},
		{"long line", "line 10 was shown cut", Change{Op: "replace", Path: "s.txt", From: at(10), Text: "x"}},
		{"long line in a range", "line 10 was shown cut", Change{Op: "delete", Path: "s.txt", From: at(9), To: at(11)}},
		{"cut line in a range", "line 12 ", Change{Op: "delete", Path: "s.txt", From: at(11), To: at(13)}},
		{"anchor ???", "", Change{Op: "replace", Path: "s.txt", From: "12:???", Text: "x"}},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) { c.refuse(t, tc.want, tc.change) })
	}
	// Inserting beside a locked or long line is fine (one operation per
	// line: two that name the same line are refused as touching it twice).
	c.refuse(t, "both address line 2",
		Change{Op: "insert", Path: "s.txt", At: at(2), Where: "before", Text: "x"},
		Change{Op: "insert", Path: "s.txt", At: at(2), Where: "after", Text: "y"})
	c.accept(t, Change{Op: "insert", Path: "s.txt", At: at(2), Where: "after", Text: "after key"})
	// But not between two lines of one masked value: that changes the key.
	for _, ins := range []Change{
		{Op: "insert", Path: "s.txt", At: at(4), Where: "after", Text: "x"},
		{Op: "insert", Path: "s.txt", At: at(5), Where: "after", Text: "x"},
		{Op: "insert", Path: "s.txt", At: at(6), Where: "before", Text: "x"},
		{Op: "insert", Path: "s.txt", At: at(8), Where: "before", Text: "x"},
	} {
		c.refuse(t, "lies inside a value you were not shown", ins)
	}
	c.accept(t, Change{Op: "insert", Path: "s.txt", At: at(4), Where: "before", Text: "before pem"})
	c.accept(t, Change{Op: "insert", Path: "s.txt", At: at(9), Where: "before", Text: "after pem"})
	plan := c.accept(t,
		Change{Op: "insert", Path: "s.txt", At: at(2), Where: "before", Text: "before key"},
		Change{Op: "insert", Path: "s.txt", At: at(3), Where: "before", Text: "after key"},
		Change{Op: "insert", Path: "s.txt", At: at(8), Text: "after pem"},
		Change{Op: "insert", Path: "s.txt", At: at(10), Text: "after long"},
	)
	want := strings.Replace(content, "aws_key", "before key\naws_key", 1)
	want = strings.Replace(want, fakeAWSKey+"\"\n", fakeAWSKey+"\"\nafter key\n", 1)
	want = strings.Replace(want, "-----END RSA PRIVATE KEY-----\n", "-----END RSA PRIVATE KEY-----\nafter pem\n", 1)
	want = strings.Replace(want, "y\nd\n", "y\nafter long\nd\n", 1)
	if got := afterOf(t, plan, "s.txt"); got != want {
		t.Errorf("inserts beside locked lines: got %d bytes, want %d", len(got), len(want))
	}
	// The diff shows the old lines as the model saw them.
	plan = c.accept(t, Change{Op: "replace", Path: "s.txt", From: at(3), Text: "B"})
	mustNotContain(t, plan.Diff, fakeAWSKey, "MIIEowIBAAKCAQEA")
	mustContain(t, plan.Diff, "[REDACTED:")

	// A syntax error on a locked line never quotes it: go/parser would print
	// the string literal it found there.
	c3 := newChangeFixture(t, map[string]string{"k.go": "package k\n\nvar a = []string{\n\t\"" + fakeAWSKey + "\",\n}\n"}, Options{})
	ce := c3.refuse(t, "line 5 (the parser's message is not shown", Change{Op: "insert", Path: "k.go", At: c3.anchor(t, "k.go", 4), Where: "before", Text: "\tx."})
	mustNotContain(t, ce.Error(), fakeAWSKey, "AKIA")
	if got := goMessage(`expected selector or type assertion, found "` + fakeAWSKey + `"`); strings.Contains(got, "AKIA") {
		t.Errorf("goMessage kept the literal: %q", got)
	}

	// Without redaction nothing is masked, so nothing is locked.
	c2 := newChangeFixture(t, map[string]string{"s.txt": content}, Options{NoRedact: true})
	c2.accept(t, Change{Op: "replace", Path: "s.txt", From: c2.anchor(t, "s.txt", 2), Text: "x"})
	c2.refuse(t, "line 10 was shown cut", Change{Op: "delete", Path: "s.txt", From: c2.anchor(t, "s.txt", 10)})
}

func TestGuessingASecretLearnsNothing(t *testing.T) {
	c := newChangeFixture(t, map[string]string{"cfg.txt": "aws_key = \"" + fakeAWSKey + "\"\n"}, Options{})
	real := strings.SplitN(c.anchor(t, "cfg.txt", 1), ":", 2)[1]
	var plans []*Plan
	for _, guess := range []string{fakeAWSKey, "AKIAABCDEFGHIJKLMNOP"} {
		plan := c.accept(t, Change{Op: "create", Path: "guess.txt", Text: "aws_key = \"" + guess + "\"\n"})
		lines, err := c.ws.shownLines(bg, plannedFile(t, plan, "guess.txt").After)
		if err != nil {
			t.Fatal(err)
		}
		if got := lineAnchor(c.ws.Opts.AnchorKey, lines[0]); got != real {
			t.Errorf("guess %s: anchor %s, the real line's is %s", guess, got, real)
		}
		plans = append(plans, plan)
	}
	if fmt.Sprint(plans[0].Warnings, plans[0].Added, plans[0].Removed) != fmt.Sprint(plans[1].Warnings, plans[1].Added, plans[1].Removed) {
		t.Errorf("a right and a wrong guess give different plans: %+v / %+v", plans[0], plans[1])
	}
}

func TestPlaceholderSpellingIsOrdinaryText(t *testing.T) {
	c := newChangeFixture(t, map[string]string{"r.go": "package r\n\nconst m = \"[REDACTED:aws-access-key]\"\n"}, Options{})
	plan := c.accept(t,
		Change{Op: "replace", Path: "r.go", From: c.anchor(t, "r.go", 3), Text: "const m = \"[REDACTED:github-token]\""},
		Change{Op: "create", Path: "p.txt", Text: "[REDACTED:private-key]\n"},
	)
	if got := afterOf(t, plan, "r.go"); got != "package r\n\nconst m = \"[REDACTED:github-token]\"\n" {
		t.Errorf("r.go = %q", got)
	}
}

func TestProtectedPathsAreRefused(t *testing.T) {
	c := newChangeFixture(t, map[string]string{"a.txt": "a\n", ".gitattributes": "*.txt text\n"}, Options{})
	paths := []string{
		".git/config", ".GIT/hooks/pre-commit", ".github/workflows/ci.yml", ".GitHub/Workflows/x.yml",
		".gitlab-ci.yml", ".gitlab/ci/x.yml", ".circleci/config.yml", ".buildkite/p.yml", ".woodpecker/x.yml",
		".woodpecker.yml", ".drone.yml", ".travis.yml", "azure-pipelines.yml", "bitbucket-pipelines.yml",
		"Jenkinsfile", ".husky/pre-commit", ".githooks/pre-push", ".gitmodules", "sub/.gitmodules",
		".gitattributes", "sub/.gitattributes", ".repochatignore", "sub/.repochatignore",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			c.refuse(t, "", Change{Op: "create", Path: p, Text: "x"})
			c.refuse(t, "", Change{Op: "move", Path: "a.txt", ToPath: p})
		})
	}
	c.refuse(t, "protected", Change{Op: "create", Path: ".GitHub/Workflows/x.yml", Text: "x"})
	c.refuse(t, "protected", Change{Op: "remove", Path: ".gitattributes", FileTag: c.tag(t, ".gitattributes")})
	c.refuse(t, "protected", Change{Op: "replace", Path: ".gitattributes", From: c.anchor(t, ".gitattributes", 1), Text: "*.txt -text"})
}

func TestCreateBesideACaseVariantIsRefused(t *testing.T) {
	c := newChangeFixture(t, map[string]string{"README.md": "r\n", "docs/a.md": "a\n", "x.txt": "x\n"}, Options{})
	c.refuse(t, "letter case", Change{Op: "create", Path: "readme.md", Text: "x"})
	c.refuse(t, "letter case", Change{Op: "move", Path: "x.txt", ToPath: "ReadMe.MD"})
	c.refuse(t, "letter case", Change{Op: "create", Path: "Docs/b.md", Text: "x"})
	c.refuse(t, "letter case", Change{Op: "create", Path: "n.txt", Text: "x"}, Change{Op: "create", Path: "N.txt", Text: "y"})
	c.refuse(t, "already exists", Change{Op: "create", Path: "README.md", Text: "x"})
	c.refuse(t, "already exists", Change{Op: "move", Path: "x.txt", ToPath: "docs/a.md"})
	c.accept(t, Change{Op: "create", Path: "docs/b.md", Text: "x"})
}

func TestWithheldPathsCannotBeTouchedOrCreated(t *testing.T) {
	c := newChangeFixture(t, map[string]string{
		"a.txt":           "a\n",
		".env":            "SECRET=1\n",
		".repochatignore": "private/\n",
		"private/p.txt":   "p\n",
		"denied.txt":      "d\n",
	}, Options{Deny: []string{"denied.txt", "nope/"}})
	targets := []string{".env", "conf/.env", "private/p.txt", "private/new.txt", "denied.txt", "nope/x.txt"}
	for _, p := range targets {
		t.Run(p, func(t *testing.T) {
			c.refuse(t, "withheld", Change{Op: "create", Path: p, Text: "x"})
			c.refuse(t, "withheld", Change{Op: "move", Path: "a.txt", ToPath: p})
		})
	}
	c.refuse(t, "withheld", Change{Op: "remove", Path: ".env", FileTag: "aaaaaaaa"})
	c.refuse(t, "withheld", Change{Op: "replace", Path: "denied.txt", From: "1:aaa", Text: "x"})
	c.refuse(t, "withheld", Change{Op: "move", Path: "private/p.txt", ToPath: "public.txt"})
}

func TestSymlinkAndSubmodulePathsAreRefused(t *testing.T) {
	f := newSessionFixture(t, map[string]string{"a.txt": "a\n", "dir/f.txt": "f\n"})
	if err := os.Symlink("dir", filepath.Join(f.dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(f.dir, "lf")); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, f.dir, "add", "-A")
	head := gitCmd(t, f.dir, "rev-parse", "HEAD")
	gitCmd(t, f.dir, "update-index", "--add", "--cacheinfo", "160000,"+head+",sub")
	gitCmd(t, f.dir, "commit", "-q", "-m", "links")
	s, info := f.start(t, Options{})
	c := &changeFixture{f: f, s: s}
	var err error
	if c.ws, err = s.Serve(bg); err != nil {
		t.Fatal(err)
	}
	// A link made in the workspace behind git's back, to a directory outside.
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(info.Workspace, "evil")); err != nil {
		t.Fatal(err)
	}
	refused := []struct {
		name   string
		change Change
	}{
		{"create through a tracked link", Change{Op: "create", Path: "link/new.txt", Text: "x"}},
		{"move into a tracked link", Change{Op: "move", Path: "a.txt", ToPath: "link/a.txt"}},
		{"remove a link", Change{Op: "remove", Path: "lf", FileTag: "aaaaaaaa"}},
		{"edit a link", Change{Op: "replace", Path: "lf", From: "1:aaa", Text: "x"}},
		{"move a link", Change{Op: "move", Path: "lf", ToPath: "lf2"}},
		{"create through an untracked link", Change{Op: "create", Path: "evil/x.txt", Text: "x"}},
		{"create inside a submodule", Change{Op: "create", Path: "sub/x.txt", Text: "x"}},
		{"remove a submodule", Change{Op: "remove", Path: "sub", FileTag: "aaaaaaaa"}},
		{"move onto a submodule", Change{Op: "move", Path: "a.txt", ToPath: "sub"}},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) { c.refuse(t, "", tc.change) })
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("something was written outside: %v", entries)
	}
	c.accept(t, Change{Op: "create", Path: "dir/new.txt", Text: "x"})
}

func TestFilteredPathsAreRefused(t *testing.T) {
	f := newSessionFixture(t, map[string]string{
		"a.txt": "a\n", "y.bin": "y\n", ".gitattributes": "*.bin filter=lfs\nsecret/** filter=crypt\n",
	})
	marker := filepath.Join(t.TempDir(), "ran")
	for _, k := range []string{"clean", "smudge"} {
		gitCmd(t, f.dir, "config", "filter.lfs."+k, "touch "+marker)
		gitCmd(t, f.dir, "config", "filter.crypt."+k, "touch "+marker)
	}
	s, _ := f.start(t, Options{})
	c := &changeFixture{f: f, s: s}
	var err error
	if c.ws, err = s.Serve(bg); err != nil {
		t.Fatal(err)
	}
	c.refuse(t, "filter", Change{Op: "create", Path: "x.bin", Text: "x"})
	c.refuse(t, "filter", Change{Op: "create", Path: "secret/k.txt", Text: "x"})
	c.refuse(t, "filter", Change{Op: "replace", Path: "y.bin", From: c.anchor(t, "y.bin", 1), Text: "x"})
	c.refuse(t, "filter", Change{Op: "move", Path: "a.txt", ToPath: "z.bin"})
	c.refuse(t, "filter", Change{Op: "move", Path: "y.bin", ToPath: "y.txt"})
	c.accept(t, Change{Op: "create", Path: "x.txt", Text: "x"})
	if exists(marker) {
		t.Error("a filter program ran")
	}
}

func TestSyntaxGateDoesNotMakeItWorse(t *testing.T) {
	c := newChangeFixture(t, map[string]string{
		"ok.go":              "package ok\n\nfunc F() {}\n",
		"bad.go":             "package bad\n\nfunc F( {}\n",
		"ok.json":            "{\n\"a\": 1\n}\n",
		"bad.json":           "{\n\"a\": 1,\n}\n",
		"tsconfig.json":      "{\n// comment\n\"a\": 1\n}\n",
		".vscode/conf.json":  "{}\n",
		"notes.txt":          "func (\n",
		"web/tsconfig.json":  "{}\n",
		"web/jsconfig.json":  "{}\n",
		"ok.txt":             "x\n",
		"sub/ok/deeper.json": "[]\n",
	}, Options{})
	at := func(p string, n int) string { return c.anchor(t, p, n) }
	refused := []struct {
		name, want string
		change     Change
	}{
		{"go valid to invalid", "ok.go", Change{Op: "replace", Path: "ok.go", From: at("ok.go", 3), Text: "func F( {}"}},
		{"new go invalid", "line 1", Change{Op: "create", Path: "n.go", Text: "pakage n"}},
		{"json valid to invalid", "ok.json", Change{Op: "replace", Path: "ok.json", From: at("ok.json", 2), Text: "\"a\": 1,"}},
		{"new json invalid", "n.json", Change{Op: "create", Path: "n.json", Text: "{"}},
		{"existing valid tsconfig made invalid", "tsconfig.json", Change{Op: "insert", Path: "web/tsconfig.json", At: "end", Text: "// c"}},
		{"text moved to go", "t.go", Change{Op: "move", Path: "notes.txt", ToPath: "t.go"}},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) { c.refuse(t, tc.want, tc.change) })
	}
	ce := c.refuse(t, "", Change{Op: "replace", Path: "ok.go", From: at("ok.go", 3), Text: "func F( {}"})
	if !regexp.MustCompile(`line \d+`).MatchString(ce.Reason) {
		t.Errorf("the syntax error names no line: %q", ce.Reason)
	}
	c.accept(t, Change{Op: "replace", Path: "bad.go", From: at("bad.go", 1), Text: "package bad2"})
	c.accept(t, Change{Op: "replace", Path: "bad.json", From: at("bad.json", 1), Text: "{ "})
	c.accept(t, Change{Op: "create", Path: "new/tsconfig.base.json", Text: "{\n// comment\n}"})
	c.accept(t, Change{Op: "create", Path: "jsconfig.json", Text: "{ /* c */ }"})
	c.accept(t, Change{Op: "create", Path: ".vscode/settings.json", Text: "{ // c\n}"})
	c.accept(t, Change{Op: "replace", Path: "tsconfig.json", From: at("tsconfig.json", 2), Text: "// another comment"})
	c.accept(t, Change{Op: "create", Path: "n.go", Text: "package n\n"})
	c.accept(t, Change{Op: "move", Path: "ok.txt", ToPath: "ok.md"})
}

func TestGofmtDifferenceIsAWarning(t *testing.T) {
	c := newChangeFixture(t, map[string]string{"x.go": "package x\n"}, Options{})
	plan := c.accept(t, Change{Op: "insert", Path: "x.go", At: "end", Text: "func  F(){}"})
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "x.go") || !strings.Contains(plan.Warnings[0], "gofmt") {
		t.Errorf("warnings %q", plan.Warnings)
	}
	if got := afterOf(t, plan, "x.go"); got != "package x\nfunc  F(){}\n" {
		t.Errorf("the file was reformatted: %q", got)
	}
	plan = c.accept(t, Change{Op: "insert", Path: "x.go", At: "end", Text: "\nfunc F() {}"})
	if len(plan.Warnings) != 0 {
		t.Errorf("warnings for formatted code: %q", plan.Warnings)
	}

	// gofmt aligns trailing comments by the length of the lines around them,
	// so on a file with a masked value the warning would measure the value.
	c2 := newChangeFixture(t, map[string]string{"s.go": "package s\n\nvar k = \"" + fakeAWSKey + "\" // key\n"}, Options{})
	plan = c2.accept(t, Change{Op: "insert", Path: "s.go", At: "end", Text: "var x = 1 // x"})
	if len(plan.Warnings) != 0 {
		t.Errorf("a gofmt warning on a file with a masked value: %q", plan.Warnings)
	}
}

// A file moved to a name the syntax gate checks is refused without the
// parser quoting a line that holds a masked value, or any line of a file
// that is not text.
func TestMovedFileSyntaxErrorShowsNoMaskedText(t *testing.T) {
	c := newChangeFixture(t, map[string]string{
		"notes.txt": "aws_key = \"" + fakeAWSKey + "\"\n",
		"plain.txt": "zzz\n",
		"bin.dat":   "q\x00\n",
	}, Options{})
	ce := c.refuse(t, "the parser's message is not shown", Change{Op: "move", Path: "notes.txt", ToPath: "notes.json"})
	mustNotContain(t, ce.Error(), "'a'", "AKIA")
	c.refuse(t, "the parser's message is not shown", Change{Op: "move", Path: "bin.dat", ToPath: "bin.json"})
	c.refuse(t, "line 1: invalid character 'z'", Change{Op: "move", Path: "plain.txt", ToPath: "plain.json"})
}

// When git cannot judge the paths against .repochatignore, the change set
// is refused rather than checked path by path.
func TestPolicyFailureRefusesTheChangeSet(t *testing.T) {
	c := newChangeFixture(t, map[string]string{"a.txt": "a\n", ".repochatignore": "*.log\n"}, Options{})
	if _, err := c.ws.Snapshot(bg); err != nil {
		t.Fatal(err)
	}
	real, _ := exec.LookPath("git")
	bin := t.TempDir()
	write(t, bin, "git", "#!/bin/sh\nfor a in \"$@\"; do if [ \"$a\" = check-ignore ]; then echo 'fatal: broken' >&2; exit 128; fi; done\nexec "+real+" \"$@\"\n")
	os.Chmod(filepath.Join(bin, "git"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	c.refuse(t, ReasonUncertain, Change{Op: "create", Path: "new.txt", Text: "x"})
}

func TestLineEndingsAndFinalNewlineSurvive(t *testing.T) {
	c := newChangeFixture(t, map[string]string{
		"crlf.txt":     "a\r\nb\r\nc\r\n",
		"mixed.txt":    "a\r\nb\r\nc\n",
		"nofinal.txt":  "a\nb",
		"crlfnf.txt":   "a\r\nb",
		"empty.txt":    "",
		"one.txt":      "only\n",
		"blank.txt":    "\n",
		"bom.txt":      "\ufeffa\nb\n",
		"lastcrlf.txt": "a\nb\r\n",
	}, Options{})
	at := func(p string, n int) string { return c.anchor(t, p, n) }
	cases := []struct {
		name, path, want string
		change           Change
	}{
		{"CRLF replace", "crlf.txt", "a\r\nB\r\nc\r\n", Change{Op: "replace", Path: "crlf.txt", From: at("crlf.txt", 2), Text: "B"}},
		{"CRLF insert of text with CRLF", "crlf.txt", "a\r\nb\r\nc\r\nd\r\ne\r\n", Change{Op: "insert", Path: "crlf.txt", At: "end", Text: "d\r\ne\r\n"}},
		{"majority CRLF", "mixed.txt", "x\r\na\r\nb\r\nc\n", Change{Op: "insert", Path: "mixed.txt", At: "start", Text: "x"}},
		{"majority LF", "lastcrlf.txt", "a\nb\r\nc\n", Change{Op: "insert", Path: "lastcrlf.txt", At: "end", Text: "c"}},
		{"no final newline, insert at end", "nofinal.txt", "a\nb\nc", Change{Op: "insert", Path: "nofinal.txt", At: "end", Text: "c"}},
		{"no final newline, replace last", "nofinal.txt", "a\nB", Change{Op: "replace", Path: "nofinal.txt", From: at("nofinal.txt", 2), Text: "B"}},
		{"no final newline, delete last", "nofinal.txt", "a", Change{Op: "delete", Path: "nofinal.txt", From: at("nofinal.txt", 2)}},
		{"no final newline, insert at start", "nofinal.txt", "x\na\nb", Change{Op: "insert", Path: "nofinal.txt", At: "start", Text: "x"}},
		{"CRLF without final newline", "crlfnf.txt", "a\r\nb\r\nc", Change{Op: "insert", Path: "crlfnf.txt", At: "end", Text: "c"}},
		{"insert into an empty file", "empty.txt", "x\ny\n", Change{Op: "insert", Path: "empty.txt", At: "start", Text: "x\ny"}},
		{"insert at the end of an empty file", "empty.txt", "x\n", Change{Op: "insert", Path: "empty.txt", At: "end", Text: "x"}},
		{"delete every line", "one.txt", "", Change{Op: "delete", Path: "one.txt", From: at("one.txt", 1)}},
		{"a file of one empty line", "blank.txt", "\nx\n", Change{Op: "insert", Path: "blank.txt", At: "end", Text: "x"}},
		{"empty line as text", "one.txt", "only\n\n", Change{Op: "insert", Path: "one.txt", At: "end", Text: "\n"}},
		{"byte order mark kept", "bom.txt", "\ufeffA\nb\n", Change{Op: "replace", Path: "bom.txt", From: at("bom.txt", 1), Text: "A"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := c.accept(t, tc.change)
			if got := afterOf(t, plan, tc.path); got != tc.want {
				t.Errorf("%q, want %q", got, tc.want)
			}
		})
	}
	plan := c.accept(t, Change{Op: "create", Path: "new.txt", Text: "x\ny"})
	if got := afterOf(t, plan, "new.txt"); got != "x\ny\n" {
		t.Errorf("a new file: %q", got)
	}
	plan = c.accept(t, Change{Op: "create", Path: "new.txt", Text: ""})
	if f := plannedFile(t, plan, "new.txt"); f.After == nil || len(f.After) != 0 {
		t.Errorf("a new empty file: %q", f.After)
	}
}

func TestLimitsAreEnforced(t *testing.T) {
	big := strings.Repeat(strings.Repeat("b", 99)+"\n", (2<<20)/100-50) // just under 2 MiB
	c := newChangeFixture(t, map[string]string{"big.txt": big, "a.txt": "a\n"}, Options{})

	many := make([]Change, 501)
	for i := range many {
		many[i] = Change{Op: "insert", Path: "a.txt", At: "end", Text: "x"}
	}
	c.refuse(t, "500", many...)

	files := make([]Change, 101)
	for i := range files {
		files[i] = Change{Op: "create", Path: fmt.Sprintf("f%03d.txt", i), Text: "x"}
	}
	c.refuse(t, "100", files...)
	c.accept(t, files[:100]...)

	text := strings.Repeat("t", 1<<20)
	c.refuse(t, "1 MiB", Change{Op: "create", Path: "n1.txt", Text: text[:600<<10]}, Change{Op: "create", Path: "n2.txt", Text: text[:500<<10]})
	c.refuse(t, "2 MiB", Change{Op: "insert", Path: "big.txt", At: "end", Text: text[:(10 << 10)]})
	c.accept(t, Change{Op: "insert", Path: "big.txt", At: "end", Text: "small"})

	// ParseChanges applies the operation limit too.
	raw, _ := json.Marshal(many)
	if _, err := ParseChanges(raw); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("ParseChanges of 501 operations: %v", err)
	}
}

func TestRemoveNeedsTheCurrentFileTag(t *testing.T) {
	c := newChangeFixture(t, map[string]string{"r.txt": "r\n", "bin.dat": "a\x00b"}, Options{})
	tag := c.tag(t, "r.txt")
	other := "aaaaaaaa"
	if tag == other {
		other = "bbbbbbbb"
	}
	c.refuse(t, "file_tag", Change{Op: "remove", Path: "r.txt"})
	c.refuse(t, "changed", Change{Op: "remove", Path: "r.txt", FileTag: other})
	c.accept(t, Change{Op: "remove", Path: "r.txt", FileTag: tag})
	if err := os.WriteFile(filepath.Join(c.ws.Root, "r.txt"), []byte("R\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.refuse(t, "changed", Change{Op: "remove", Path: "r.txt", FileTag: tag})
	c.refuse(t, "", Change{Op: "remove", Path: "bin.dat", FileTag: other})
	c.refuse(t, "", Change{Op: "remove", Path: "missing.txt", FileTag: other})
}

func TestBinaryFilesCanOnlyBeMoved(t *testing.T) {
	c := newChangeFixture(t, map[string]string{
		"bin.dat":   "a\x00b\n",
		"u16.txt":   "\xff\xfea\x00\n\x00",
		"latin.txt": "caf\xe9\n",
	}, Options{})
	for _, p := range []string{"bin.dat", "u16.txt", "latin.txt"} {
		c.refuse(t, "", Change{Op: "replace", Path: p, From: "1:aaa", Text: "x"})
		c.refuse(t, "", Change{Op: "insert", Path: p, At: "end", Text: "x"})
	}
	plan := c.accept(t, Change{Op: "move", Path: "bin.dat", ToPath: "moved/bin.dat"}, Change{Op: "move", Path: "u16.txt", ToPath: "u16b.txt"})
	if got := afterOf(t, plan, "moved/bin.dat"); got != "a\x00b\n" {
		t.Errorf("moved binary: %q", got)
	}
}

func TestConflictMarkersAndNulAreRefused(t *testing.T) {
	c := newChangeFixture(t, map[string]string{"a.txt": "a\n", "rst.txt": "Title\n=======\n\ntext\n"}, Options{})
	for _, text := range []string{"<<<<<<< HEAD", ">>>>>>> branch", "x\n<<<<<<< ours\ny", "<<<<<<<", ">>>>>>>",
		"=======\n>>>>>>> theirs", "<<<<<<< ours\nmine\n=======\ntheirs\n>>>>>>> theirs"} {
		c.refuse(t, "conflict marker", Change{Op: "insert", Path: "a.txt", At: "end", Text: text})
		c.refuse(t, "conflict marker", Change{Op: "create", Path: "n.txt", Text: text})
	}
	// F8: a line of seven "=" is also how a title is underlined. It is
	// refused only in a file that gains a "<<<<<<<" or ">>>>>>>" line with it.
	c.accept(t, Change{Op: "create", Path: "doc/install.rst", Text: "Install\n=======\n\nRun make.\n"})
	c.accept(t, Change{Op: "insert", Path: "a.txt", At: "end", Text: "======="})
	c.accept(t, Change{Op: "insert", Path: "a.txt", At: "end", Text: "======= and more"})
	// A marker line the file already had is not new; a longer rule is no marker.
	c.accept(t, Change{Op: "insert", Path: "rst.txt", At: "end", Text: "Other\n======="})
	c.accept(t, Change{Op: "insert", Path: "a.txt", At: "end", Text: "=========\n<<<<<<<<"})
	// The file had its own "======="; a new side marker is still refused, and
	// the refusal names it.
	ce := c.refuse(t, "conflict marker (>>>>>>>)", Change{Op: "insert", Path: "rst.txt", At: "end", Text: "Again\n=======\n>>>>>>> x"})
	mustContain(t, ce.Error(), "line 7")

	c.refuse(t, "NUL", Change{Op: "insert", Path: "a.txt", At: "end", Text: "a\x00b"})
	c.refuse(t, "NUL", Change{Op: "create", Path: "n.txt", Text: "\x00"})
	c.refuse(t, "UTF-8", Change{Op: "create", Path: "n.txt", Text: "caf\xe9"})
}

func TestChangeShapesAreChecked(t *testing.T) {
	c := newChangeFixture(t, map[string]string{"a.txt": "a\nb\n"}, Options{})
	a1 := c.anchor(t, "a.txt", 1)
	refused := []struct {
		name   string
		change Change
	}{
		{"unknown op", Change{Op: "rewrite", Path: "a.txt"}},
		{"no path", Change{Op: "create", Text: "x"}},
		{"replace without from", Change{Op: "replace", Path: "a.txt", Text: "x"}},
		{"replace without text", Change{Op: "replace", Path: "a.txt", From: a1}},
		{"delete with text", Change{Op: "delete", Path: "a.txt", From: a1, Text: "x"}},
		{"insert without at", Change{Op: "insert", Path: "a.txt", Text: "x"}},
		{"insert without text", Change{Op: "insert", Path: "a.txt", At: a1}},
		{"insert with a bad where", Change{Op: "insert", Path: "a.txt", At: a1, Where: "under", Text: "x"}},
		{"insert at end with where", Change{Op: "insert", Path: "a.txt", At: "end", Where: "before", Text: "x"}},
		{"create with from", Change{Op: "create", Path: "n.txt", From: a1, Text: "x"}},
		{"move without to_path", Change{Op: "move", Path: "a.txt"}},
		{"move with text", Change{Op: "move", Path: "a.txt", ToPath: "b.txt", Text: "x"}},
		{"remove with text", Change{Op: "remove", Path: "a.txt", FileTag: "aaaaaaaa", Text: "x"}},
		{"anchor with a bar", Change{Op: "delete", Path: "a.txt", From: a1 + "|"}},
		{"anchor line zero", Change{Op: "delete", Path: "a.txt", From: "0:aaa"}},
		{"path not plain", Change{Op: "create", Path: "./n.txt", Text: "x"}},
		{"path with blanks", Change{Op: "create", Path: " n.txt", Text: "x"}},
		{"absolute path", Change{Op: "create", Path: "/tmp/n.txt", Text: "x"}},
		{"parent path", Change{Op: "create", Path: "../n.txt", Text: "x"}},
		{"create then edit", Change{Op: "create", Path: "a.txt", Text: "x"}},
		{"bare carriage return at the end", Change{Op: "insert", Path: "a.txt", At: "end", Text: "foo\r"}},
		{"bare carriage return inside", Change{Op: "create", Path: "n.txt", Text: "a\rb\n"}},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) { c.refuse(t, "", tc.change) })
	}
	c.refuse(t, "", Change{Op: "remove", Path: "a.txt", FileTag: c.tag(t, "a.txt")}, Change{Op: "delete", Path: "a.txt", From: a1})
	c.refuse(t, "", Change{Op: "remove", Path: "a.txt", FileTag: c.tag(t, "a.txt")}, Change{Op: "create", Path: "a.txt", Text: "x"})
	c.refuse(t, "", Change{Op: "remove", Path: "a.txt", FileTag: c.tag(t, "a.txt")}, Change{Op: "move", Path: "a.txt", ToPath: "b.txt"})
	c.refuse(t, "", Change{Op: "create", Path: "b.txt", Text: "x"}, Change{Op: "move", Path: "a.txt", ToPath: "b.txt"})
	c.refuse(t, "", Change{Op: "create", Path: "b.txt", Text: "x"}, Change{Op: "insert", Path: "b.txt", At: "end", Text: "x"})
	c.refuse(t, "empty", []Change{}...)
}

func TestParseChanges(t *testing.T) {
	got, err := ParseChanges(json.RawMessage(`[{"op":"replace","path":"a.txt","from":"1:abc","to":"2:abd","text":"x\n"},
		{"op":"move","path":"a","to_path":"b"},{"op":"remove","path":"c","file_tag":"abcdefgh"},
		{"op":"insert","path":"a","at":"end","where":"","text":"y"}]`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Change{
		{Op: "replace", Path: "a.txt", From: "1:abc", To: "2:abd", Text: "x\n"},
		{Op: "move", Path: "a", ToPath: "b"},
		{Op: "remove", Path: "c", FileTag: "abcdefgh"},
		{Op: "insert", Path: "a", At: "end", Text: "y"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %+v", got)
	}
	bad := []string{
		``, `null`, `{}`, `[]`, `[1]`, `["x"]`, `[{"op":"create","path":"a","extra":"x"}]`,
		`[{"op":"create","Path":"a"}]`, `[{"OP":"create","path":"a"}]`,
		`[{"op":"create","path":"a","path":"b"}]`, `[{"op":"create","path":1}]`,
		`[{"op":"create","path":"a"}] [`, `[{"op":"create","path":"a"}],`, `[{"op":"create","path":"a"}`,
		`[{"op":"create","path":["a"]}]`, `[{"op":"create","path":{"x":1}}]`,
	}
	for _, in := range bad {
		if _, err := ParseChanges(json.RawMessage(in)); err == nil {
			t.Errorf("ParseChanges(%s) accepted", in)
		}
	}
}

func FuzzParseChanges(f *testing.F) {
	for _, seed := range []string{
		`[{"op":"replace","path":"a.txt","from":"1:abc","text":"x"}]`,
		`[{"op":"create","path":"a","text":"\u0000"}]`,
		`[{"op":"move","path":"a","to_path":"b"},{"op":"remove","path":"c","file_tag":"x"}]`,
		`[{"op":"create","path":"a","path":"b"}]`, `[]`, `null`, `[{}]`, `[{"op":1}]`, `[[]]`, `{"a":[1]}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		changes, err := ParseChanges(raw)
		if err != nil {
			return
		}
		if len(changes) == 0 || len(changes) > maxChangeOps {
			t.Fatalf("ParseChanges accepted %d operations", len(changes))
		}
		for _, c := range changes {
			if err := checkShape(c); err != nil {
				t.Fatalf("ParseChanges accepted a malformed operation %+v: %v", c, err)
			}
		}
	})
}

// C3: a private-key block may hold a line the redactor does not mask (a
// conflict marker between two body lines). Inserting text beside it would
// end the block's masking, and the body lines after it would be shown.
func TestInsertCannotEndAKeyBlock(t *testing.T) {
	content := "-----BEGIN PRIVATE KEY-----\nQUJDRA==\n<<<<<<< HEAD\nU0VDUkVU\n-----END PRIVATE KEY-----\n"
	c := newApplyFixture(t, map[string]string{"notes.txt": content}, nil)
	mustNotContain(t, readText(t, c.ws, "notes.txt").Text, "QUJDRA==", "U0VDUkVU")
	before := wsState(t, c.ws.Root)
	change := Change{Op: "insert", Path: "notes.txt", At: c.anchor(t, "notes.txt", 3), Where: "before", Text: "# stop"}
	a, err := c.s.Apply(bg, "", []Change{change}, "")
	if err == nil {
		t.Fatalf("the insert was applied; Fresh:\n%s", a.Fresh)
	}
	mustContain(t, err.Error(), "value you were not shown")
	mustNotContain(t, err.Error(), "QUJDRA==", "U0VDUkVU")
	if after := wsState(t, c.ws.Root); after != before {
		t.Error("a refused insert changed the workspace")
	}
	// The marker line is part of the block: locked like the rest of it.
	c.refuse(t, "line 3 holds a value you were not shown", Change{Op: "replace", Path: "notes.txt", From: c.anchor(t, "notes.txt", 3), Text: "# stop"})
	c.refuse(t, "line 3 holds a value you were not shown", Change{Op: "delete", Path: "notes.txt", From: c.anchor(t, "notes.txt", 3)})
	// Outside the block, edits go through.
	c.accept(t, Change{Op: "insert", Path: "notes.txt", At: c.anchor(t, "notes.txt", 1), Where: "before", Text: "# a key follows"})
	c.accept(t, Change{Op: "insert", Path: "notes.txt", At: "end", Text: "# a key precedes"})
}

// No edit of a file that holds masked lines may show the text of one: not
// in the diff of the proposal, not in the refusal, not in the fresh lines of
// the result, and not in a later read.
func TestNoEditShowsAMaskedLine(t *testing.T) {
	const s1, s2, s3 = "QUJDRAsecretONE1111", "U0VDUkVUsecretTWO2222", "R0hJSktMsecretTHREE33"
	secrets := []string{s1, s2, s3}
	begin, end := "-----BEGIN PRIVATE KEY-----\n", "-----END PRIVATE KEY-----\n"
	shapes := map[string]string{
		"conflict marker inside":   "top\n" + begin + s1 + "\n<<<<<<< HEAD\n" + s2 + "\n" + end + "bottom\n",
		"three markers inside":     begin + s1 + "\n<<<<<<< ours\n" + s2 + "\n=======\n" + s3 + "\n>>>>>>> theirs\n" + end,
		"unterminated at the end":  "top\n" + begin + s1 + "\n||||||| base\n" + s2 + "\n",
		"two blocks":               begin + s1 + "\n" + end + "between\n" + begin + s2 + "\n=======\n" + s3 + "\n" + end + "bottom\n",
		"headers and a blank line": begin + "Proc-Type: 4,ENCRYPTED\n\n" + s1 + "\n>>>>>>> x\n" + s2 + "\n" + end,
		"token beside a block":     "aws = \"" + fakeAWSKey + "\"\n" + begin + s1 + "\n<<<<<<< HEAD\n" + s2 + "\n" + end,
	}
	texts := []string{"# stop", "-----END PRIVATE KEY-----", "-----BEGIN PRIVATE KEY-----", "not base64 !", ""}
	for name, content := range shapes {
		t.Run(name, func(t *testing.T) {
			c := newApplyFixture(t, map[string]string{"k.txt": content}, nil)
			base := c.head(t)
			hidden := append([]string{fakeAWSKey}, secrets...)
			mustNotContain(t, readText(t, c.ws, "k.txt").Text, hidden...)
			n := strings.Count(content, "\n")
			var tries []Change
			for _, text := range texts {
				if text == "" {
					continue
				}
				tries = append(tries,
					Change{Op: "insert", Path: "k.txt", At: "start", Text: text},
					Change{Op: "insert", Path: "k.txt", At: "end", Text: text})
			}
			for l := 1; l <= n; l++ {
				at := c.anchor(t, "k.txt", l)
				for _, text := range texts {
					if text == "" {
						tries = append(tries, Change{Op: "delete", Path: "k.txt", From: at})
						continue
					}
					tries = append(tries,
						Change{Op: "insert", Path: "k.txt", At: at, Where: "before", Text: text},
						Change{Op: "insert", Path: "k.txt", At: at, Where: "after", Text: text},
						Change{Op: "replace", Path: "k.txt", From: at, Text: text})
				}
			}
			accepted := 0
			for _, change := range tries {
				label := fmt.Sprintf("%s at=%s from=%s where=%s text=%q", change.Op, change.At, change.From, change.Where, change.Text)
				plan, err := c.s.Propose(bg, []Change{change})
				if err != nil {
					for _, h := range hidden {
						if strings.Contains(err.Error(), h) {
							t.Errorf("%s: the refusal shows %s:\n%v", label, h, err)
						}
					}
					continue
				}
				accepted++
				// What an apply would answer and what a read would then
				// show, from the plan; every seventh one is applied for real.
				var later strings.Builder
				lines, err := c.ws.shownLines(bg, plannedFile(t, plan, "k.txt").After)
				if err != nil {
					t.Fatal(err)
				}
				for _, l := range lines {
					later.WriteString(l.Text + "\n")
				}
				fresh, _ := c.ws.freshText(bg, plan.Files, nil)
				views := map[string]string{"the diff": plan.Diff, "Fresh": fresh, "a later read": later.String()}
				if accepted%7 == 1 {
					a, err := c.s.Apply(bg, plan.ID, nil, "")
					if err != nil {
						t.Fatalf("%s: proposed but not applied: %v", label, err)
					}
					views["the applied Fresh"], views["a read after the apply"] = a.Fresh, readText(t, c.ws, "k.txt").Text
					if _, err := c.s.UndoTo(bg, base); err != nil {
						t.Fatalf("undo: %v", err)
					}
				}
				for where, text := range views {
					for _, h := range hidden {
						if strings.Contains(text, h) {
							t.Errorf("%s: %s shows %s:\n%s", label, where, h, text)
						}
					}
				}
			}
			if accepted == 0 {
				t.Error("no edit at all was accepted")
			}
			t.Logf("%d of %d edits accepted", accepted, len(tries))
		})
	}
}

// C5: a range is addressed by its first and last line only, so a change to
// a line between them is not seen by the anchors. A replace or delete of
// three or more lines therefore needs the file's current file_tag.
func TestLongRangeNeedsTheCurrentFileTag(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"notes.txt": "first\nmiddle\nlast\n", "b.txt": numbered(6)}, nil)
	a1, a2, a3 := c.anchor(t, "notes.txt", 1), c.anchor(t, "notes.txt", 2), c.anchor(t, "notes.txt", 3)
	oldTag := c.tag(t, "notes.txt")
	applied := c.apply(t, "middle", Change{Op: "replace", Path: "notes.txt", From: a2, Text: "new middle"})
	newTag := c.tag(t, "notes.txt")
	if newTag == oldTag {
		t.Fatal("the file tag did not change with the file")
	}
	// The result of the apply gives the new tag, in Fresh and by path.
	if applied.FileTags["notes.txt"] != newTag {
		t.Errorf("FileTags = %v, want notes.txt: %s", applied.FileTags, newTag)
	}
	mustContain(t, applied.Fresh, "FILE notes.txt\n[file_tag "+newTag+"]\n")

	before := wsState(t, c.ws.Root)
	old := Change{Op: "replace", Path: "notes.txt", From: a1, To: a3, Text: "replacement"}
	// Without a tag: refused, and told what to send.
	ce := c.refuse(t, "three or more lines", old)
	mustContain(t, ce.Error(), `"file_tag"`, "read_file", "apply_changes")
	mustNotContain(t, ce.Error(), newTag, "new middle")
	// With the tag of the file as it was read: stale.
	old.FileTag = oldTag
	ce = c.refuse(t, "file_tag "+oldTag+" does not match the file as it is now", old)
	mustNotContain(t, ce.Error(), newTag, "new middle")
	if _, err := c.s.Apply(bg, "", []Change{old}, ""); err == nil {
		t.Fatal("a stale range was applied directly")
	}
	if after := wsState(t, c.ws.Root); after != before {
		t.Error("a refused range changed the workspace")
	}
	// With the current tag: accepted.
	old.FileTag = newTag
	c.apply(t, "all three", old)
	if got := mustRead(t, filepath.Join(c.ws.Root, "notes.txt")); got != "replacement\n" {
		t.Errorf("notes.txt = %q", got)
	}

	// delete alike; one and two lines need no tag, but a tag given must hold.
	at := func(n int) string { return c.anchor(t, "b.txt", n) }
	c.refuse(t, "three or more lines", Change{Op: "delete", Path: "b.txt", From: at(2), To: at(4)})
	c.accept(t, Change{Op: "delete", Path: "b.txt", From: at(2), To: at(3)})
	c.accept(t, Change{Op: "replace", Path: "b.txt", From: at(2), Text: "two"})
	c.refuse(t, "does not match the file as it is now", Change{Op: "replace", Path: "b.txt", From: at(2), Text: "two", FileTag: oldTag})
	c.accept(t, Change{Op: "delete", Path: "b.txt", From: at(2), To: at(4), FileTag: c.tag(t, "b.txt")},
		Change{Op: "replace", Path: "b.txt", From: at(6), Text: "six", FileTag: c.tag(t, "b.txt")})
	c.refuse(t, "file_tag", Change{Op: "delete", Path: "b.txt", From: at(2), To: at(4), FileTag: "not-a-tag"})
	c.refuse(t, `does not take "file_tag"`, Change{Op: "insert", Path: "b.txt", At: at(2), Text: "x", FileTag: c.tag(t, "b.txt")})
	// A locked line in the range is still the first thing said.
	c2 := newApplyFixture(t, map[string]string{"s.txt": "a\naws_key = \"" + fakeAWSKey + "\"\nb\n"}, nil)
	c2.refuse(t, "line 2 holds a value you were not shown", Change{Op: "delete", Path: "s.txt", From: c2.anchor(t, "s.txt", 1), To: c2.anchor(t, "s.txt", 3)})
}
