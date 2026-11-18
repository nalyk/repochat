package repo

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactorRules(t *testing.T) {
	r := NewRedactor(true)
	secrets := map[string]string{
		"aws id":     "key = " + fakeAWSKey,
		"aws secret": `aws_secret_access_key = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"`,
		"github":     "token: ghp_0123456789abcdefghijklmnopqrstuvwxyzAB",
		"github pat": "github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz",
		"gitlab":     "glpat-abcdefghij0123456789",
		"slack":      "xoxb-123456789012-abcdefghijkl",
		"openai":     "OPENAI_API_KEY=sk-proj-abcdefghijklmnopqrstuvwxyz0123456789",
		"anthropic":  "sk-ant-api03-abcdefghijklmnopqrstuvwxyz",
		"google":     "AIzaSyA0123456789abcdefghijklmnopqrstuvw",
		"stripe":     "sk_live_0123456789abcdefghijklmn",
		"jwt":        "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk",
		"url":        "postgres://admin:s3cr3tpass@db.internal:5432/app",
		"pem inline": "-----BEGIN PRIVATE KEY-----MIIabc-----END PRIVATE KEY-----",
	}
	for name, line := range secrets {
		out, n := r.Line(line)
		if n == 0 || !strings.Contains(out, "[REDACTED:") {
			t.Errorf("%s: not masked: %q", name, out)
		}
	}
	if out, _ := r.Line("postgres://admin:s3cr3tpass@db.internal:5432/app"); strings.Contains(out, "s3cr3tpass") || !strings.Contains(out, "admin:") || !strings.Contains(out, "@db.internal") {
		t.Errorf("url credentials: %q", out)
	}
	clean := []string{
		"func main() { fmt.Println(\"hello\") }",
		"https://example.com/path?x=1",
		"git@github.com:owner/repo.git",
		"url := \"https://${USER}:${PASS}@host/\"",
		"see https://user@example.com/",
		"sha256:9e91a539e3ae1c436d499f216a1d7514b3c86cc7779032f41b2a05821d74b03a",
		"ask-me-anything about skeletons",
		"if strings.HasPrefix(s, \"-----BEGIN RSA PRIVATE KEY-----\") {",
	}
	for _, line := range clean {
		st := r.NewState()
		if out := st.Line(line); out != line {
			t.Errorf("false positive: %q became %q", line, out)
		}
		if st.inPEM {
			t.Errorf("code mentioning a key marker must not open a key block: %q", line)
		}
	}
	if out, n := NewRedactor(false).Line("key = " + fakeAWSKey); n != 0 || !strings.Contains(out, fakeAWSKey) {
		t.Errorf("a disabled redactor must not change text")
	}
}

func TestRedactorPEMBlocks(t *testing.T) {
	r := NewRedactor(true)
	text := "before\n" + fakePEM + "after AKIAIOSFODNN7EXAMPLE\n"
	out, n := r.Text(text, false)
	mustNotContain(t, out, "MIIEow", "S2tU3v", "T3uU4v", "BEGIN RSA", "END RSA", fakeAWSKey)
	mustContain(t, out, "before", "after", "[REDACTED:private-key]", "[REDACTED:aws-access-key-id]")
	if n != 2 {
		t.Errorf("expected 2 masked values, got %d", n)
	}
	if got := strings.Count(out, "\n"); got != strings.Count(text, "\n") {
		t.Errorf("masking must preserve the line count: %d vs %d", got, strings.Count(text, "\n"))
	}

	// A key embedded as a string literal across lines.
	code := "const key = \"-----BEGIN PRIVATE KEY-----\\n\" +\n\t\"MIIEvQIBADANBgkqhkiG9w0BAQEFAASC\\n\" +\n\t\"-----END PRIVATE KEY-----\"\nfunc next() {}\n"
	out, _ = r.Text(code, false)
	mustNotContain(t, out, "MIIEvQIBADANBgkqhkiG9w0BAQEFAASC")
	mustContain(t, out, "func next() {}")

	// In hunk mode a key body without its BEGIN line is masked too.
	hunk := "@@ -3,2 +3,2 @@\n-S2tU3vW4xY5zA6bC7dD8eE9fF0gG1hH2iI3jJ4kK5lL6mM7nN8oO9pP0qQ1rR2sS\n+T3uU4vV5wW6xX7yY8zZ9aA0bB1cC2dD3eE4fF5gG6hH7iI8jJ9kK0lL1mM2nN3oO\n context line\n"
	out, _ = r.Text(hunk, true)
	mustNotContain(t, out, "S2tU3v", "T3uU4v")
	mustContain(t, out, "-[REDACTED:base64-block]", "+[REDACTED:base64-block]", " context line")
}

func TestSanitize(t *testing.T) {
	in := "ok\ttab\nline\x1b[31mred\x00nul\x7fdel\u0085c1\u202ertl" + string([]byte{0xff, 0xfe})
	out := Sanitize(in)
	for _, bad := range []string{"\x1b", "\x00", "\x7f", "\u0085", "\u202e", "\xff"} {
		if strings.Contains(out, bad) {
			t.Errorf("Sanitize left %q in %q", bad, out)
		}
	}
	mustContain(t, out, "ok\ttab\nline", `\x1b[31mred`, `\x00`, `\x7f`, `\u202e`)
	if Sanitize("plain ascii") != "plain ascii" {
		t.Error("plain text must pass through unchanged")
	}
	if got := SanitizeLine("a\nb\tc"); got != `a\nb\tc` {
		t.Errorf("SanitizeLine must escape line breaks: %q", got)
	}
}

// fixture: a small repository with ignored, untracked, secret and nested files.
func fixtureRepo(t *testing.T) string {
	dir := newDir(t)
	write(t, dir, ".gitignore", "ignored/\n*.log\n")
	write(t, dir, "README.md", "# Demo\nhello world\n")
	write(t, dir, "src/main.go", "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n")
	write(t, dir, "src/token.go", "package main\n\nconst tokenKind = \"bearer\"\n")
	write(t, dir, ".env", "PASSWORD=NOPE-TOP-SECRET\n")
	write(t, dir, "config/secrets.yaml", "db: NOPE-TOP-SECRET\n")
	write(t, dir, "ignored/cache.txt", "IGNORED-CONTENT\n")
	write(t, dir, "debug.log", "IGNORED-CONTENT\n")
	commit(t, dir, "initial")
	write(t, dir, "notes/untracked.txt", "untracked note\n")
	return dir
}

func TestSnapshotFollowsGitignoreAndPolicy(t *testing.T) {
	dir := fixtureRepo(t)
	r := open(t, dir, Options{})
	snap, err := r.Snapshot(bg)
	if err != nil {
		t.Fatal(err)
	}
	visible := map[string]bool{}
	for _, e := range snap.Entries {
		if e.Withheld == "" {
			visible[e.Path] = true
		}
	}
	for _, want := range []string{"README.md", "src/main.go", "src/token.go", "notes/untracked.txt", ".gitignore"} {
		if !visible[want] {
			t.Errorf("%s should be served", want)
		}
	}
	for _, hidden := range []string{"ignored/cache.txt", "debug.log"} {
		if _, ok := snap.Lookup(hidden); ok {
			t.Errorf("%s is ignored by git and must not be in the file set at all", hidden)
		}
	}
	for _, withheld := range []string{".env", "config/secrets.yaml"} {
		e, ok := snap.Lookup(withheld)
		if !ok || e.Withheld != ReasonSecretName {
			t.Errorf("%s should be listed as withheld (secret-name), got %+v", withheld, e)
		}
	}
	if snap.Withheld[ReasonSecretName] != 2 || snap.Untracked != 1 {
		t.Errorf("counts: %+v untracked=%d", snap.Withheld, snap.Untracked)
	}

	// The generation identifies policy + file membership: stable across
	// refreshes and content edits, different when a file appears.
	gen := snap.Generation
	write(t, dir, "src/main.go", "package main // edited\n")
	fresh(r)
	if s2, _ := r.Snapshot(bg); s2.Generation != gen {
		t.Error("the generation must not change when only file contents change")
	}
	write(t, dir, "src/new.go", "package main\n")
	fresh(r)
	if s3, _ := r.Snapshot(bg); s3.Generation == gen {
		t.Error("the generation must change when a file is added")
	}

	tracked := open(t, dir, Options{TrackedOnly: true})
	ts, _ := tracked.Snapshot(bg)
	if _, ok := ts.Lookup("notes/untracked.txt"); ok {
		t.Error("--tracked-only must hide untracked files")
	}
}

func TestRepochatignoreAndFlags(t *testing.T) {
	dir := fixtureRepo(t)
	write(t, dir, ".repochatignore", "src/token.go\nnotes/\n")
	r := open(t, dir, Options{Allow: []string{".env", "notes/untracked.txt"}, Deny: []string{"README.md"}})
	snap, err := r.Snapshot(bg)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"src/token.go":        ReasonIgnoreFile,
		"notes/untracked.txt": ReasonIgnoreFile, // allow cannot loosen the repository's own ignore file
		"README.md":           ReasonDenyFlag,
		".env":                "", // explicitly allowed by the user
		"config/secrets.yaml": ReasonSecretName,
		"src/main.go":         "",
	}
	for path, reason := range want {
		e, ok := snap.Lookup(path)
		if !ok || e.Withheld != reason {
			t.Errorf("%s: got %q (present=%v), want %q", path, e.Withheld, ok, reason)
		}
	}
}

func TestRepochatignoreSymlinkFailsClosed(t *testing.T) {
	dir := fixtureRepo(t)
	outside := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(outside, []byte("*.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, ".repochatignore")); err != nil {
		t.Fatal(err)
	}
	r := open(t, dir, Options{})
	if _, err := r.Snapshot(bg); err == nil || !strings.Contains(err.Error(), ".repochatignore") {
		t.Fatalf("a symlinked policy file must fail closed, got %v", err)
	}
}

func TestReadFile(t *testing.T) {
	dir := fixtureRepo(t)
	var lines []string
	for i := 1; i <= 30; i++ {
		lines = append(lines, "line "+strings.Repeat("x", i))
	}
	write(t, dir, "big.txt", strings.Join(lines, "\n")+"\n")
	write(t, dir, "crlf.txt", "one\r\ntwo\r\n")
	write(t, dir, "noeol.txt", "only line")
	write(t, dir, "empty.txt", "")
	write(t, dir, "bin.dat", "ab\x00cd")
	write(t, dir, "long.txt", strings.Repeat("y", 5000)+"\nshort\n")
	write(t, dir, "esc.txt", "a\x1b[2Jb\n")
	write(t, dir, "utf16.txt", string([]byte{0xFF, 0xFE, 'h', 0, 'i', 0, '\n', 0}))
	write(t, dir, "key.txt", "intro\n"+fakePEM+"outro\n")
	r := open(t, dir, Options{})
	budget := r.Budget()

	res, err := r.ReadFile(bg, "big.txt", 10, 5, "", budget)
	if err != nil {
		t.Fatal(err)
	}
	if res.First != 10 || res.Last != 14 || res.Total != 30 || !res.TotalKnown || res.NextStart != 15 {
		t.Errorf("range: %+v", res)
	}
	mustContain(t, res.Text, "lines 10-14 of 30", "10| line xxxxxxxxxx", "start_line=15")
	mustNotContain(t, res.Text, " 9| ", "15| ")

	res, _ = r.ReadFile(bg, "big.txt", 26, 100, "", budget)
	if res.NextStart != 0 || res.Last != 30 {
		t.Errorf("end of file: %+v", res)
	}
	res, _ = r.ReadFile(bg, "big.txt", 99, 5, "", budget)
	mustContain(t, res.Text, "past the end")

	res, _ = r.ReadFile(bg, "crlf.txt", 1, 10, "", budget)
	mustContain(t, res.Text, "1| one\n", "2| two\n")
	res, _ = r.ReadFile(bg, "noeol.txt", 1, 10, "", budget)
	if res.Total != 1 {
		t.Errorf("a final line without newline counts: %+v", res)
	}
	res, _ = r.ReadFile(bg, "empty.txt", 1, 10, "", budget)
	mustContain(t, res.Text, "empty file")
	res, _ = r.ReadFile(bg, "bin.dat", 1, 10, "", budget)
	if !res.Binary {
		t.Error("binary files must be reported, not dumped")
	}
	mustNotContain(t, res.Text, "ab\x00cd")
	res, _ = r.ReadFile(bg, "long.txt", 1, 10, "", budget)
	mustContain(t, res.Text, "bytes]", "2| short", "very long line")
	if len(res.Text) > 3000 {
		t.Errorf("a long line must be shortened, result is %d bytes", len(res.Text))
	}
	res, _ = r.ReadFile(bg, "esc.txt", 1, 10, "", budget)
	mustNotContain(t, res.Text, "\x1b")
	mustContain(t, res.Text, `\x1b`)
	res, _ = r.ReadFile(bg, "utf16.txt", 1, 10, "", budget)
	mustContain(t, res.Text, "1| hi", "UTF-16")

	// Reading the middle of a key must be masked even though BEGIN is not in the range.
	res, _ = r.ReadFile(bg, "key.txt", 3, 2, "", budget)
	mustNotContain(t, res.Text, "MIIEow", "S2tU3v", "T3uU4v")
	mustContain(t, res.Text, "[REDACTED:private-key]")
	if res.Redactions == 0 {
		t.Error("redactions must be counted")
	}

	// A small budget stops on a line boundary and says how to continue.
	res, _ = r.ReadFile(bg, "big.txt", 1, 30, "", 900)
	if res.NextStart == 0 || res.Last >= 30 {
		t.Errorf("budget: %+v", res)
	}
}

func TestReadFileRefusals(t *testing.T) {
	dir := fixtureRepo(t)
	if err := os.Symlink(".env", filepath.Join(dir, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(dir, "external.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("config", filepath.Join(dir, "cfglink")); err != nil {
		t.Fatal(err)
	}
	r := open(t, dir, Options{})
	b := r.Budget()
	cases := map[string]string{
		".env":                 "withheld",
		"config/secrets.yaml":  "withheld",
		"alias.txt":            "symbolic link",
		"external.txt":         "symbolic link",
		"cfglink/secrets.yaml": "withheld",
		"ignored/cache.txt":    "not a file in this repository",
		"debug.log":            "not a file in this repository",
		"missing.go":           "not a file in this repository",
		"src":                  "is a directory",
		"../outside":           "..",
		"/etc/passwd":          "repository-relative",
		".git/config":          ".git",
		".git/HEAD":            ".git",
	}
	for path, want := range cases {
		res, err := r.ReadFile(bg, path, 1, 10, "", b)
		if err == nil {
			t.Errorf("%s: expected a refusal, got:\n%s", path, res.Text)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %q should mention %q", path, err, want)
		}
		if strings.Contains(err.Error(), "NOPE") || strings.Contains(err.Error(), dir) {
			t.Errorf("%s: error leaks content or an absolute path: %q", path, err)
		}
	}
	if _, err := r.ReadFile(bg, ".env", 1, 10, "", b); !errors.Is(err, ErrWithheld) {
		t.Errorf("withheld paths must be distinguishable from missing ones: %v", err)
	}
}

// A path component replaced by a symlink is never followed, whether the swap
// happens before the lookup or after the directory handle was verified.
func TestOpenerNeverFollowsSwappedDirectories(t *testing.T) {
	dir := fixtureRepo(t)
	write(t, dir, "safe/data.txt", "SAFE\n")
	write(t, dir, "secretdir/data.txt", "STOLEN\n")
	r := open(t, dir, Options{})

	// (a) swapped before traversal.
	op := r.newOpener()
	if err := os.Rename(filepath.Join(dir, "safe"), filepath.Join(dir, "safe.real")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("secretdir", filepath.Join(dir, "safe")); err != nil {
		t.Fatal(err)
	}
	if data, _, _, err := op.ReadAll("safe/data.txt", 1<<20); err == nil {
		t.Fatalf("a symlinked directory must not be traversed, read %q", data)
	}
	op.Close()
	os.Remove(filepath.Join(dir, "safe"))
	os.Rename(filepath.Join(dir, "safe.real"), filepath.Join(dir, "safe"))

	// (b) swapped after the directory handle was verified: later access stays
	// anchored to the directory object that was verified.
	op = r.newOpener()
	defer op.Close()
	if _, err := op.dir("safe"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "safe"), filepath.Join(dir, "safe.real")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("secretdir", filepath.Join(dir, "safe")); err != nil {
		t.Fatal(err)
	}
	data, _, _, err := op.ReadAll("safe/data.txt", 1<<20)
	if err != nil {
		t.Fatalf("anchored read failed: %v", err)
	}
	if string(data) != "SAFE\n" {
		t.Fatalf("read escaped through a swapped symlink: %q", data)
	}

	// (c) the leaf itself replaced by a symlink.
	os.Remove(filepath.Join(dir, "safe.real", "data.txt"))
	if err := os.Symlink(filepath.Join(dir, ".env"), filepath.Join(dir, "safe.real", "data.txt")); err != nil {
		t.Fatal(err)
	}
	if data, _, _, err := op.ReadAll("safe/data.txt", 1<<20); err == nil {
		t.Fatalf("a symlinked file must not be opened, read %q", data)
	}

	// A FIFO must not block.
	fifo := filepath.Join(dir, "pipe")
	if err := mkfifo(fifo); err == nil {
		if _, _, _, err := op.ReadAll("pipe", 10); err == nil {
			t.Error("a FIFO is not a regular file")
		}
	}
}

func TestRootReplacementFailsClosed(t *testing.T) {
	dir := fixtureRepo(t)
	r := open(t, dir, Options{})
	if _, err := r.Snapshot(bg); err != nil {
		t.Fatal(err)
	}
	moved := dir + ".moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Rename(moved, dir) })
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fresh(r)
	if _, err := r.Snapshot(bg); err == nil || !strings.Contains(err.Error(), "moved or replaced") {
		t.Fatalf("a replaced root must fail closed, got %v", err)
	}
	if _, err := r.Status(bg); err == nil {
		t.Fatal("git must not run against a replaced root")
	}
}

func TestOpenRejectsNonRepositories(t *testing.T) {
	if _, err := Open(bg, t.TempDir(), Options{}); !errors.Is(err, ErrNotRepo) {
		t.Fatalf("want ErrNotRepo, got %v", err)
	}
	dir := fixtureRepo(t)
	if _, err := Open(bg, dir, Options{Deny: []string{""}}); err != nil {
		t.Fatalf("empty patterns are ignored: %v", err)
	}
	sub := open(t, filepath.Join(dir, "src"), Options{})
	if sub.Root != dir {
		t.Errorf("opening from a subdirectory must find the root: %s", sub.Root)
	}
}
