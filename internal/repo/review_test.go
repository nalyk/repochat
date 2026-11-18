package repo

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Regression tests for issues found in the adversarial implementation review.

func TestIgnoreFileUnderstandsPosixClassesAndFailsClosed(t *testing.T) {
	dir := newDir(t)
	write(t, dir, "data1.txt", "WITHHELD_BY_POSIX_PATTERN\n")
	write(t, dir, "dataX.txt", "visible\n")
	write(t, dir, ".repochatignore", "data[[:digit:]].txt\n")
	r := open(t, dir, Options{})
	if _, err := r.ReadFile(bg, "data1.txt", 1, 5, "", r.Budget()); err == nil || !strings.Contains(err.Error(), "withheld") {
		t.Fatalf("a file matched by a POSIX-class pattern must be withheld, got %v", err)
	}
	if _, err := r.ReadFile(bg, "dataX.txt", 1, 5, "", r.Budget()); err != nil {
		t.Fatalf("non-matching file: %v", err)
	}

	// A pattern repochat cannot understand stops everything instead of being skipped.
	write(t, dir, ".repochatignore", "ok.txt\nsecret[0-9.txt\n")
	fresh(r)
	_, err := r.Snapshot(bg)
	if err == nil || !strings.Contains(err.Error(), ".repochatignore line 2") {
		t.Fatalf("an unreadable ignore pattern must fail closed, got %v", err)
	}
	if _, err := r.ReadFile(bg, "dataX.txt", 1, 5, "", r.Budget()); err == nil {
		t.Fatal("nothing may be served while the policy cannot be read")
	}
}

// Repository configuration can name files outside the repository for git to
// read (mailmap, ignore-revs). Their content must not reach any output.
func TestFilesNamedByRepositoryConfigAreNotRead(t *testing.T) {
	dir := newDir(t)
	write(t, dir, "code.txt", "hello\n")
	commit(t, dir, "initial")
	outside := t.TempDir()

	mailmap := filepath.Join(outside, "mailmap")
	os.WriteFile(mailmap, []byte("OUTSIDE_FILE_MARKER <ada@example.com>\n"), 0o644)
	gitCmd(t, dir, "config", "mailmap.file", mailmap)
	if out := gitCmd(t, dir, "blame", "--porcelain", "code.txt"); !strings.Contains(out, "OUTSIDE_FILE_MARKER") {
		t.Skip("positive control: this git does not apply mailmap.file to blame")
	}
	r := open(t, dir, Options{})
	bl, err := r.Blame(bg, "code.txt", 1, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	mustNotContain(t, bl.Text, "OUTSIDE_FILE_MARKER")
	mustContain(t, bl.Text, "Ada")
	lg, _ := r.Log(bg, LogOptions{})
	sh, _ := r.Show(bg, "HEAD", nil, false, -1, r.Budget())
	mustNotContain(t, lg.Text+sh.Text, "OUTSIDE_FILE_MARKER")

	// blame.ignoreRevsFile cannot be overridden on every git version, and
	// git quotes the file's first line when it is not a list of revisions.
	// That text must stay in the local log and never reach a tool result.
	revs := filepath.Join(outside, "revs")
	os.WriteFile(revs, []byte("OUTSIDE_FIRST_LINE\n"), 0o644)
	gitCmd(t, dir, "config", "blame.ignoreRevsFile", revs)
	var local []string
	r.Logf = func(format string, args ...any) { local = append(local, fmt.Sprintf(format, args...)) }
	bl, err = r.Blame(bg, "code.txt", 1, 1, "")
	if err == nil {
		mustNotContain(t, bl.Text, "OUTSIDE_FIRST_LINE")
	} else {
		if strings.Contains(err.Error(), "OUTSIDE_FIRST_LINE") {
			t.Fatalf("git's error text leaked a file named by the repository configuration: %v", err)
		}
		if len(local) == 0 || !strings.Contains(local[0], "OUTSIDE_FIRST_LINE") {
			t.Errorf("the reason must be available in the local log, got %v", local)
		}
	}

	// A legitimate ignore-revs file inside the repository keeps working.
	write(t, dir, ".git-blame-ignore-revs", "# formatting commits\n")
	gitCmd(t, dir, "config", "blame.ignoreRevsFile", ".git-blame-ignore-revs")
	if _, err := r.Blame(bg, "code.txt", 1, 1, ""); err != nil {
		t.Fatalf("a valid ignore-revs file must not break blame: %v", err)
	}
}

func TestKeyMaterialEdgeCases(t *testing.T) {
	dir := newDir(t)
	key := "-----BEGIN PRIVATE KEY-----\n" + strings.Repeat("A", 64) + "\nQUJDRA==\n-----END PRIVATE KEY-----\n"
	write(t, dir, "code.txt", key)
	commit(t, dir, "initial")
	write(t, dir, "code.txt", strings.Replace(key, "QUJDRA==", "RUZHSA==", 1))
	write(t, dir, "long.txt", strings.Repeat(" ", 70000)+"-----BEGIN PRIVATE KEY-----\n"+strings.Repeat("B", 64)+"\nQkJC\n-----END PRIVATE KEY-----\nafter the key\n")
	write(t, dir, "mixed.txt", "AWS_sEcReT_ACCESS_KEY="+strings.Repeat("A", 40)+"\n")
	write(t, dir, "pgp.txt", "-----BEGIN PGP PRIVATE KEY BLOCK-----\n\nlQOYBF\n=AbCd\n-----END PGP PRIVATE KEY BLOCK-----\ntail\n")
	r := open(t, dir, Options{})
	b := r.Budget()

	res, err := r.ReadFile(bg, "code.txt", 1, 10, "", b)
	if err != nil {
		t.Fatal(err)
	}
	mustNotContain(t, res.Text, "RUZHSA", "AAAAAAAA", "END PRIVATE KEY")

	// The last, short line of a key body, changed in a diff: neither the
	// changed lines nor the hunk-header context may show key material.
	d, err := r.Diff(bg, DiffOptions{Context: 0}, b)
	if err != nil {
		t.Fatal(err)
	}
	mustNotContain(t, d.Text, "QUJDRA", "RUZHSA", "AAAAAAAA")
	mustContain(t, d.Text, "[REDACTED:private-key]")

	// A BEGIN marker pushed past the part of a long line that is kept.
	res, err = r.ReadFile(bg, "long.txt", 2, 10, "", b)
	if err != nil {
		t.Fatal(err)
	}
	mustNotContain(t, res.Text, "BBBBBBBB", "QkJC")
	mustContain(t, res.Text, "after the key")

	res, _ = r.ReadFile(bg, "mixed.txt", 1, 5, "", b)
	mustNotContain(t, res.Text, strings.Repeat("A", 40))
	res, _ = r.ReadFile(bg, "pgp.txt", 1, 10, "", b)
	mustNotContain(t, res.Text, "lQOYBF", "=AbCd")
	mustContain(t, res.Text, "tail")

	sr, _ := r.Search(bg, SearchOptions{Query: "RUZHSA", CaseSensitive: true}, b)
	if sr.Matches != 0 {
		t.Errorf("masked key material must not be searchable:\n%s", sr.Text)
	}
}

func TestFilterDriversWithOddNamesOrAddedLaterDoNotExecute(t *testing.T) {
	// A driver with an empty name.
	dir := newDir(t)
	write(t, dir, "code.txt", "original\n")
	write(t, dir, ".gitattributes", "code.txt filter=\n")
	commit(t, dir, "initial")
	script, flag := marker(t)
	gitCmd(t, dir, "config", "filter..clean", script)
	write(t, dir, "code.txt", "different\n")
	plainGit(t, dir, nil, "diff")
	if !executed(flag) {
		t.Skip("positive control: this git does not run an empty-named filter")
	}
	os.Remove(flag)
	r := open(t, dir, Options{})
	r.Status(bg)
	d, err := r.Diff(bg, DiffOptions{Context: -1}, r.Budget())
	if err != nil || !strings.Contains(d.Text, "+different") {
		t.Fatalf("diff: %v", err)
	}
	if executed(flag) {
		t.Fatal("an empty-named filter driver was executed")
	}

	// A driver configured after the repository was opened and listed.
	dir = newDir(t)
	write(t, dir, "code.txt", "original\n")
	write(t, dir, ".gitattributes", "code.txt filter=late\n")
	commit(t, dir, "initial")
	r = open(t, dir, Options{})
	if _, err := r.Snapshot(bg); err != nil {
		t.Fatal(err)
	}
	script, flag = marker(t)
	gitCmd(t, dir, "config", "filter.late.clean", script)
	write(t, dir, "code.txt", "different\n")
	r.Status(bg)
	r.Diff(bg, DiffOptions{Context: -1}, r.Budget())
	r.Diff(bg, DiffOptions{Mode: DiffWorktree, Context: -1}, r.Budget())
	r.Blame(bg, "code.txt", 1, 1, "")
	r.Overview(bg)
	if executed(flag) {
		t.Fatal("a filter driver configured after start was executed")
	}
}

// The commit-message filter must not reveal whether a guess matches text
// that is shown masked: results, counts and paging hints must be identical
// for a guess that matches the raw message and one that matches nothing.
func TestLogGrepIsNotAnOracleForMaskedText(t *testing.T) {
	dir := newDir(t)
	token := "ghp_" + strings.Repeat("A", 36)
	for i := 0; i < 5; i++ {
		write(t, dir, "f.txt", strings.Repeat("x", i+1))
		commit(t, dir, "deploy with "+token+" step")
	}
	write(t, dir, "f.txt", "final")
	commit(t, dir, "plain commit about deploy")
	r := open(t, dir, Options{})

	normalise := func(text, query string) string { return strings.ReplaceAll(text, query, "<Q>") }
	hit, err := r.Log(bg, LogOptions{Grep: "ghp_AAAA", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	miss, _ := r.Log(bg, LogOptions{Grep: "ghp_ZZZZ", Limit: 2})
	if hit.Count != 0 || hit.More || hit.NextSkip != 0 {
		t.Errorf("a guess matching masked text must look like no match: %+v", hit)
	}
	if normalise(hit.Text, "ghp_AAAA") != normalise(miss.Text, "ghp_ZZZZ") {
		t.Errorf("the two guesses are distinguishable:\n%s\n---\n%s", hit.Text, miss.Text)
	}

	// Disclosed words stay searchable and page by matched commits.
	page1, _ := r.Log(bg, LogOptions{Grep: "DEPLOY", Limit: 4})
	if page1.Count != 4 || !page1.More || page1.NextSkip != 4 {
		t.Fatalf("page 1: %+v\n%s", page1, page1.Text)
	}
	mustContain(t, page1.Text, "[REDACTED:github-token]")
	mustNotContain(t, page1.Text, token)
	page2, _ := r.Log(bg, LogOptions{Grep: "DEPLOY", Limit: 4, Skip: page1.NextSkip})
	if page2.Count != 2 || page2.More {
		t.Fatalf("page 2: %+v\n%s", page2, page2.Text)
	}
	masked, _ := r.Log(bg, LogOptions{Grep: "REDACTED:github-token"})
	if masked.Count != 5 {
		t.Errorf("the filter runs on the masked message, so the mask itself matches: %d", masked.Count)
	}
}

func TestReachabilityIsRecheckedAfterAWhile(t *testing.T) {
	dir := newDir(t)
	write(t, dir, "a.txt", "a\n")
	commit(t, dir, "first")
	gitCmd(t, dir, "checkout", "-q", "-b", "temp")
	write(t, dir, "b.txt", "b\n")
	oid := commit(t, dir, "on temp")
	gitCmd(t, dir, "checkout", "-q", "main")
	r := open(t, dir, Options{})
	if _, err := r.ResolveCommit(bg, oid); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "branch", "-q", "-D", "temp")
	r.mu.Lock()
	for k := range r.authorized { // let the short-lived cache entry expire
		r.authorized[k] = time.Now().Add(-2 * authCacheTTL)
	}
	r.mu.Unlock()
	if _, err := r.ResolveCommit(bg, oid); err == nil {
		t.Fatal("a commit that became unreachable must stop being served")
	}
}

func TestStrayDotGitEntriesAreSkipped(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	os.MkdirAll(filepath.Join(base, ".git"), 0o755) // not a repository: no HEAD
	sub := filepath.Join(base, "work")
	os.MkdirAll(sub, 0o755)
	if root, err := FindRoot(sub); err == nil && root == base {
		t.Fatalf("a directory merely named .git was taken for a repository: %s", root)
	}
	os.WriteFile(filepath.Join(sub, ".git"), []byte("not a gitdir pointer\n"), 0o644)
	if root, err := FindRoot(sub); err == nil && root == sub {
		t.Fatalf("a stray .git file was taken for a repository: %s", root)
	}
}
