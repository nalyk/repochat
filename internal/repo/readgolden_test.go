package repo

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/read_golden.txt from the current code")

// goldenRepo holds files that exercise every path of render: line endings,
// invalid UTF-8, control characters, lines cut for display and lines cut on
// reading, masked values (one-line and a key block), UTF-16, binary, empty
// and LFS pointer files.
func goldenRepo(t *testing.T) string {
	t.Helper()
	dir := newDir(t)
	var mixed strings.Builder
	mixed.WriteString("plain line\n")
	mixed.WriteString("windows line\r\n")
	mixed.WriteString("bad \xff\xfe bytes\n")
	mixed.WriteString("esc \x1b[31m red \x7f del\n")
	mixed.WriteString("bidi ‮ reversed ⁦ isolate\n")
	mixed.WriteString("\tindented with a tab   \n")
	mixed.WriteString("token = \"ghp_" + strings.Repeat("A", 36) + "\"\n")
	mixed.WriteString("aws " + fakeAWSKey + " end\n")
	mixed.WriteString(fakePEM)
	mixed.WriteString("long " + strings.Repeat("x", 2500) + " tail\n")
	mixed.WriteString("huge " + strings.Repeat("y", 70000) + " tail\n")
	mixed.WriteString(strings.Repeat("z", 1990) + " ghp_" + strings.Repeat("B", 36) + " after\n")
	mixed.WriteString("\n")
	mixed.WriteString("last line without newline")
	write(t, dir, "g/mixed.txt", mixed.String())

	write(t, dir, "g/utf16.txt", "\xff\xfeh\x00i\x00\n\x00t\x00w\x00o\x00\n\x00")
	write(t, dir, "g/bin.dat", "abc\x00def")
	write(t, dir, "g/empty.txt", "")
	write(t, dir, "g/lfs.bin", "version https://git-lfs.github.com/spec/v1\noid sha256:"+strings.Repeat("0", 64)+"\nsize 12\n")
	var many strings.Builder
	for i := 1; i <= 120; i++ {
		fmt.Fprintf(&many, "line number %d of a longer file\n", i)
	}
	write(t, dir, "g/many.txt", many.String())
	commit(t, dir, "golden")
	write(t, dir, "g/untracked.txt", "fresh\r\nfile\n")
	return dir
}

type goldenRead struct {
	path               string
	start, max, budget int
	rev                string
}

var goldenReads = []goldenRead{
	{"g/mixed.txt", 1, 0, 0, ""},
	{"g/mixed.txt", 3, 4, 0, ""},
	{"g/mixed.txt", 1, 0, 0, "HEAD"},
	{"g/mixed.txt", 99, 0, 0, ""},
	{"g/utf16.txt", 1, 0, 0, ""},
	{"g/bin.dat", 1, 0, 0, ""},
	{"g/empty.txt", 1, 0, 0, ""},
	{"g/lfs.bin", 1, 0, 0, ""},
	{"g/many.txt", 1, 0, 0, ""},
	{"g/many.txt", 50, 10, 0, ""},
	{"g/many.txt", 1, 0, 1200, ""},
	{"g/untracked.txt", 1, 0, 0, ""},
}

// goldenOutput renders every read in goldenReads with r.
func goldenOutput(t *testing.T, r *Repo) string {
	t.Helper()
	var b strings.Builder
	for _, g := range goldenReads {
		budget := g.budget
		if budget == 0 {
			budget = r.Budget()
		}
		res, err := r.ReadFile(bg, g.path, g.start, g.max, g.rev, budget)
		fmt.Fprintf(&b, "=== %s start=%d max=%d budget=%d rev=%q\n", g.path, g.start, g.max, g.budget, g.rev)
		if err != nil {
			fmt.Fprintf(&b, "error: %v\n", err)
			continue
		}
		rev := res.Rev
		if rev != "" {
			rev = "<sha>"
			res.Text = strings.ReplaceAll(res.Text, "at "+res.Rev[:10], "at <sha>")
		}
		fmt.Fprintf(&b, "first=%d last=%d total=%d known=%v next=%d size=%d binary=%v redactions=%d rev=%s\n",
			res.First, res.Last, res.Total, res.TotalKnown, res.NextStart, res.Size, res.Binary, res.Redactions, rev)
		b.WriteString(res.Text)
	}
	return b.String()
}

// TestReadOutputGolden pins read mode output byte for byte to what it was
// before anchors existed.
func TestReadOutputGolden(t *testing.T) {
	dir := goldenRepo(t)
	got := goldenOutput(t, open(t, dir, Options{}))
	path := filepath.Join("testdata", "read_golden.txt")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("read output differs from %s\n--- got ---\n%s", path, firstDiff(got, string(want)))
	}
}

// firstDiff describes where two texts start to differ.
func firstDiff(got, want string) string {
	i := 0
	for i < len(got) && i < len(want) && got[i] == want[i] {
		i++
	}
	lo := max(0, i-200)
	return fmt.Sprintf("byte %d (got %d bytes, want %d)\ngot:  %q\nwant: %q", i, len(got), len(want), got[lo:min(len(got), i+200)], want[lo:min(len(want), i+200)])
}
