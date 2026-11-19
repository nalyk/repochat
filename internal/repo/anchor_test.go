package repo

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var (
	testKey  = []byte("0123456789abcdef0123456789abcdef")
	otherKey = []byte("fedcba9876543210fedcba9876543210")
	// anchoredLine matches one content line of read_file output in write mode.
	anchoredLine = regexp.MustCompile(`^\s*(\d+):([a-z2-7]{3})\| `)
	anyAnchor    = regexp.MustCompile(`(?m)^\s*\d+:[a-z2-7]{3}\|`)
)

// contentLines returns the numbered lines of a rendered read: everything but
// the FILE header and the bracketed notes.
func contentLines(text string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if strings.HasPrefix(l, "FILE ") || strings.HasPrefix(l, "[") {
			continue
		}
		out = append(out, l)
	}
	return out
}

// anchorsOf maps line number to anchor for every content line of a read.
func anchorsOf(t *testing.T, text string) map[int]string {
	t.Helper()
	got := map[int]string{}
	for _, l := range contentLines(text) {
		m := anchoredLine.FindStringSubmatch(l)
		if m == nil {
			t.Fatalf("line without an anchor: %q", l)
		}
		n, _ := strconv.Atoi(m[1])
		got[n] = m[2]
	}
	return got
}

func readText(t *testing.T, r *Repo, p string) *ReadResult {
	t.Helper()
	res, err := r.ReadFile(bg, p, 1, 0, "", r.Budget())
	if err != nil {
		t.Fatalf("ReadFile %s: %v", p, err)
	}
	return res
}

func TestAnchorsAppearOnlyWithAKey(t *testing.T) {
	dir := goldenRepo(t)
	want, err := os.ReadFile(filepath.Join("testdata", "read_golden.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := goldenOutput(t, open(t, dir, Options{})); got != string(want) {
		t.Errorf("without a key the output must be today's: %s", firstDiff(got, string(want)))
	}

	r := open(t, dir, Options{AnchorKey: testKey})
	for _, p := range []string{"g/mixed.txt", "g/many.txt", "g/untracked.txt", "g/utf16.txt", "g/lfs.bin"} {
		res := readText(t, r, p)
		lines := contentLines(res.Text)
		if len(lines) == 0 {
			t.Fatalf("%s: no content lines:\n%s", p, res.Text)
		}
		for _, l := range lines {
			if !anchoredLine.MatchString(l) {
				t.Errorf("%s: line without an anchor: %.80q", p, l)
				break
			}
		}
	}
	// Line numbers stay right-aligned, and the text after the bar is today's.
	res := readText(t, r, "g/mixed.txt")
	plain := readText(t, open(t, dir, Options{}), "g/mixed.txt")
	pl, al := contentLines(plain.Text), contentLines(res.Text)
	if len(pl) != len(al) {
		t.Fatalf("line counts differ: %d vs %d", len(pl), len(al))
	}
	for i := range pl {
		num, text, _ := strings.Cut(pl[i], "| ")
		if !strings.HasPrefix(al[i], num+":") || !strings.HasSuffix(al[i], "| "+text) || len(al[i]) != len(pl[i])+4 {
			t.Errorf("line %d: %q does not decorate %q", i+1, al[i], pl[i])
		}
	}
	if res.FileTag == "" || !regexp.MustCompile(`^[a-z2-7]{8}$`).MatchString(res.FileTag) {
		t.Errorf("FileTag = %q", res.FileTag)
	}
	if plain.FileTag != "" {
		t.Errorf("FileTag without a key: %q", plain.FileTag)
	}
}

// What render shows and what shownLines computes must be the same lines, or
// an anchor a reader saw would never match at edit time.
func TestRenderAndShownLinesAgree(t *testing.T) {
	dir := goldenRepo(t)
	r := open(t, dir, Options{AnchorKey: testKey})
	for _, p := range []string{"g/mixed.txt", "g/many.txt", "g/untracked.txt", "g/utf16.txt"} {
		content, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil {
			t.Fatal(err)
		}
		lines, err := r.shownLines(bg, content)
		if err != nil {
			t.Fatal(err)
		}
		res := readText(t, r, p)
		shown := anchorsOf(t, res.Text)
		if len(shown) != len(lines) || res.Total != len(lines) {
			t.Fatalf("%s: read shows %d lines of %d, shownLines has %d", p, len(shown), res.Total, len(lines))
		}
		for i, l := range lines {
			if a := anchorOf(testKey, l.Text); shown[i+1] != a {
				t.Errorf("%s line %d: read shows %s, shownLines gives %s", p, i+1, shown[i+1], a)
			}
		}
		if tag := fileTagOf(testKey, lines); res.FileTag != tag {
			t.Errorf("%s: FileTag %q, fileTagOf %q", p, res.FileTag, tag)
		}
		// A partial read scans the whole file, so it carries the tag too.
		part, err := r.ReadFile(bg, p, 2, 1, "", r.Budget())
		if err != nil {
			t.Fatal(err)
		}
		if part.FileTag != res.FileTag {
			t.Errorf("%s: partial read FileTag %q, full %q", p, part.FileTag, res.FileTag)
		}
	}

	content, _ := os.ReadFile(filepath.Join(dir, "g/mixed.txt"))
	lines, _ := r.shownLines(bg, content)
	// Line 14 is 2510 bytes: shown cut, anchored whole.
	if l := lines[13]; !l.Long || len(l.Text) != 2510 {
		t.Errorf("long line: Long=%v len=%d", l.Long, len(l.Text))
	}
	if l := lines[0]; l.Long || l.Locked || l.Text != "plain line" {
		t.Errorf("plain line: %+v", l)
	}
	if l := lines[3]; l.Text != `esc \x1b[31m red \x7f del` {
		t.Errorf("sanitised line: %q", l.Text)
	}
	if l := lines[2]; l.Text != "bad � bytes" {
		t.Errorf("repaired line: %q", l.Text)
	}
}

func TestAnchorDoesNotDependOnTheSecret(t *testing.T) {
	dir := newDir(t)
	write(t, dir, "a.txt", "first\ntoken = \"ghp_"+strings.Repeat("A", 36)+"\"\nlast\n")
	write(t, dir, "b.txt", "first\ntoken = \"ghp_"+strings.Repeat("Q", 36)+"\"\nlast\n")
	write(t, dir, "c.txt", "first\nkey:\n"+fakePEM+"last\n")
	write(t, dir, "d.txt", "first\nkey:\n"+strings.ReplaceAll(fakePEM, "MIIE", "ZZZZ")+"last\n")
	commit(t, dir, "secrets")
	r := open(t, dir, Options{AnchorKey: testKey})
	for _, pair := range [][2]string{{"a.txt", "b.txt"}, {"c.txt", "d.txt"}} {
		a, b := readText(t, r, pair[0]), readText(t, r, pair[1])
		if a.Text == "" || strings.Contains(a.Text, "ghp_") || strings.Contains(a.Text, "MIIE") {
			t.Fatalf("secret not masked:\n%s", a.Text)
		}
		if a.Text[len("FILE "+pair[0]):] != b.Text[len("FILE "+pair[1]):] {
			t.Errorf("%s and %s differ only in a masked value; their reads must be identical:\n%s\n%s", pair[0], pair[1], a.Text, b.Text)
		}
		if a.FileTag == "" || a.FileTag != b.FileTag {
			t.Errorf("FileTag %q vs %q", a.FileTag, b.FileTag)
		}
	}
}

func TestAnchorChangesWithTheKeyAndTheText(t *testing.T) {
	a := anchorOf(testKey, "return x")
	if !regexp.MustCompile(`^[a-z2-7]{3}$`).MatchString(a) || anchorOf(testKey, "return x") != a {
		t.Fatalf("anchor %q is not 3 stable characters of the alphabet", a)
	}
	// 15 bits: a single pair may collide by chance, a hundred cannot all.
	sameKey, sameText := 0, 0
	for i := 0; i < 100; i++ {
		s := "line " + strconv.Itoa(i)
		if anchorOf(testKey, s) == anchorOf(otherKey, s) {
			sameKey++
		}
		if anchorOf(testKey, s) == anchorOf(testKey, s+" ") {
			sameText++
		}
	}
	if sameKey > 2 || sameText > 2 {
		t.Errorf("anchors ignore the key (%d) or the text (%d)", sameKey, sameText)
	}
	lines := []shownLine{{Text: "a"}, {Text: "b"}}
	tag := fileTagOf(testKey, lines)
	if !regexp.MustCompile(`^[a-z2-7]{8}$`).MatchString(tag) {
		t.Fatalf("file tag %q", tag)
	}
	for name, other := range map[string]string{
		"other key":   fileTagOf(otherKey, lines),
		"other text":  fileTagOf(testKey, []shownLine{{Text: "a"}, {Text: "c"}}),
		"joined":      fileTagOf(testKey, []shownLine{{Text: "ab"}}),
		"split":       fileTagOf(testKey, []shownLine{{Text: "a"}, {Text: ""}, {Text: "b"}}),
		"extra empty": fileTagOf(testKey, []shownLine{{Text: "a"}, {Text: "b"}, {Text: ""}}),
	} {
		if other == tag {
			t.Errorf("%s: same file tag %q", name, tag)
		}
	}

	// Through a read: the same file under two keys.
	dir := newDir(t)
	write(t, dir, "f.txt", "one\ntwo\nthree\nfour\nfive\nsix\n")
	commit(t, dir, "f")
	x := anchorsOf(t, readText(t, open(t, dir, Options{AnchorKey: testKey}), "f.txt").Text)
	y := anchorsOf(t, readText(t, open(t, dir, Options{AnchorKey: otherKey}), "f.txt").Text)
	differ := 0
	for n := range x {
		if x[n] != y[n] {
			differ++
		}
	}
	if differ < 4 {
		t.Errorf("anchors under two keys: %v vs %v", x, y)
	}
}

const lockedContent = "plain\npassword = \"ghp_" + "realrealrealrealrealrealrealrealreal" + "\"\n" + fakePEM +
	"mentions [REDACTED:github-token] in prose\n"

func TestLinesWithMaskedValuesAreLocked(t *testing.T) {
	r := open(t, newDir(t), Options{AnchorKey: testKey})
	lines, err := r.shownLines(bg, []byte(lockedContent))
	if err != nil {
		t.Fatal(err)
	}
	want := []bool{false, true, true, true, true, true, true, false}
	if len(lines) != len(want) {
		t.Fatalf("%d lines: %+v", len(lines), lines)
	}
	for i, l := range lines {
		if l.Locked != want[i] {
			t.Errorf("line %d %q: Locked=%v, want %v", i+1, l.Text, l.Locked, want[i])
		}
		if strings.Contains(l.Text, "realreal") || strings.Contains(l.Text, "MIIE") {
			t.Errorf("line %d shows the secret: %q", i+1, l.Text)
		}
	}

	// A key that begins past the point where a very long line is cut is not
	// on the shown text at all, and still locks the line.
	cut := strings.Repeat("a", maxLineBytes+10) + " " + fakePEM + "after\n"
	lines, err = r.shownLines(bg, []byte(cut))
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 6 || !lines[0].Locked || !lines[0].Long || strings.Contains(lines[0].Text, "BEGIN") {
		t.Fatalf("cut line: %d lines, first Locked=%v Long=%v", len(lines), lines[0].Locked, lines[0].Long)
	}
	for i, l := range lines[1:5] {
		if !l.Locked {
			t.Errorf("key line %d after a cut line is not locked: %q", i+2, l.Text)
		}
	}
	if lines[5].Locked {
		t.Errorf("line after the key is locked: %q", lines[5].Text)
	}
}

func TestNoRedactLocksNothing(t *testing.T) {
	r := open(t, newDir(t), Options{AnchorKey: testKey, NoRedact: true})
	lines, err := r.shownLines(bg, []byte(lockedContent))
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 8 {
		t.Fatalf("%d lines", len(lines))
	}
	for i, l := range lines {
		if l.Locked {
			t.Errorf("line %d locked without redaction: %q", i+1, l.Text)
		}
	}
	if !strings.Contains(lines[1].Text, "realreal") {
		t.Errorf("NoRedact must show the value: %q", lines[1].Text)
	}
}

func TestHistoryReadsCarryNoAnchors(t *testing.T) {
	dir := goldenRepo(t)
	keyed := open(t, dir, Options{AnchorKey: testKey})
	plain := open(t, dir, Options{})
	for _, p := range []string{"g/mixed.txt", "g/many.txt"} {
		a, err := keyed.ReadFile(bg, p, 1, 0, "HEAD", keyed.Budget())
		if err != nil {
			t.Fatal(err)
		}
		b, err := plain.ReadFile(bg, p, 1, 0, "HEAD", plain.Budget())
		if err != nil {
			t.Fatal(err)
		}
		if anyAnchor.MatchString(a.Text) || a.FileTag != "" {
			t.Errorf("%s at HEAD carries anchors (tag %q):\n%s", p, a.FileTag, a.Text)
		}
		if a.Text != b.Text {
			t.Errorf("%s at HEAD: output depends on the key: %s", p, firstDiff(a.Text, b.Text))
		}
	}
}

func TestSearchResultsCarryAnchors(t *testing.T) {
	dir := newDir(t)
	write(t, dir, "s.go", "package s\n\n// key ghp_"+strings.Repeat("K", 36)+"\nvar a = 1\nvar b = 2\nvar c = 3\nneedle := find()\nvar d = 4\n")
	commit(t, dir, "s")

	plain := open(t, dir, Options{})
	res, err := plain.Search(bg, SearchOptions{Query: "needle", Context: 1}, plain.Budget())
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, res.Text, "  6- var c = 3\n", "  7: needle := find()\n", "  8- var d = 4\n")

	r := open(t, dir, Options{AnchorKey: testKey})
	read := anchorsOf(t, readText(t, r, "s.go").Text)
	res, err = r.Search(bg, SearchOptions{Query: "needle", Context: 1}, r.Budget())
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, res.Text,
		"  6:"+read[6]+"| var c = 3\n",
		"> 7:"+read[7]+"| needle := find()\n",
		"  8:"+read[8]+"| var d = 4\n")

	// A hit on a line after a masked one keeps the reader's anchor too.
	res, err = r.Search(bg, SearchOptions{Query: "key"}, r.Budget())
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, res.Text, "> 3:"+read[3]+"| // key [REDACTED:github-token]\n")
}
