package repo

import (
	"fmt"
	"strings"
	"testing"
)

func searchRepo(t *testing.T) (string, *Repo) {
	dir := fixtureRepo(t)
	write(t, dir, "src/util.go", "package main\n\n// Helper does things.\nfunc Helper() int {\n\treturn helperValue\n}\n\nvar helperValue = 42\n")
	write(t, dir, "src/util_test.go", "package main\n\nfunc TestHelper() { Helper() }\n")
	write(t, dir, "docs/guide.md", "Use the helper wisely.\nHELPER in caps.\n")
	write(t, dir, "bin.dat", "helper\x00binary")
	write(t, dir, "ignored/x.txt", "helper in ignored file\n")
	write(t, dir, "config/secrets.yaml", "helper: NOPE-TOP-SECRET\n")
	return dir, open(t, dir, Options{})
}

func TestSearchModes(t *testing.T) {
	_, r := searchRepo(t)
	b := r.Budget()

	res, err := r.Search(bg, SearchOptions{Query: "helper"}, b)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Complete || res.Files != 3 {
		t.Errorf("case-insensitive literal: files=%d complete=%v\n%s", res.Files, res.Complete, res.Text)
	}
	mustContain(t, res.Text, "src/util.go", "  4: func Helper() int {", "docs/guide.md", "1 binary skipped", "2 withheld file(s) not searched", "[complete")
	mustNotContain(t, res.Text, "ignored/x.txt", "NOPE", "secrets.yaml")

	res, _ = r.Search(bg, SearchOptions{Query: "helper", CaseSensitive: true}, b)
	mustNotContain(t, res.Text, "func Helper() int", "HELPER in caps")
	mustContain(t, res.Text, "helperValue")

	res, _ = r.Search(bg, SearchOptions{Query: "Helper", WholeWord: true, CaseSensitive: true}, b)
	mustContain(t, res.Text, "func Helper() int", "func TestHelper() { Helper() }")
	mustNotContain(t, res.Text, "helperValue")

	res, _ = r.Search(bg, SearchOptions{Query: `func \w+\(\) int`, Regex: true}, b)
	if res.Matches != 1 {
		t.Errorf("regex: %d matches\n%s", res.Matches, res.Text)
	}
	if _, err := r.Search(bg, SearchOptions{Query: "(unclosed", Regex: true}, b); err == nil || !strings.Contains(err.Error(), "invalid regular expression") {
		t.Errorf("a bad regex must be explained: %v", err)
	}

	res, _ = r.Search(bg, SearchOptions{Query: "return helperValue", Context: 1}, b)
	mustContain(t, res.Text, "  4- func Helper() int {", "  5: \treturn helperValue", "  6- }")

	res, _ = r.Search(bg, SearchOptions{Query: "helper", Include: []string{"*.go"}, Exclude: []string{"*_test.go"}}, b)
	if res.Files != 1 {
		t.Errorf("include/exclude: %d files\n%s", res.Files, res.Text)
	}

	res, _ = r.Search(bg, SearchOptions{Query: "zzz-not-present"}, b)
	if !res.Complete || res.Matches != 0 {
		t.Errorf("no match: %+v", res)
	}
	mustContain(t, res.Text, "[complete", "no match")
}

// A search that stops early must say so and be resumable; it must never look
// like "nothing found".
func TestSearchIsNeverSilentlyIncomplete(t *testing.T) {
	dir := newDir(t)
	for i := 0; i < 12; i++ {
		write(t, dir, fmt.Sprintf("f%02d.txt", i), "needle one\nfiller\nneedle two\nneedle three\n")
	}
	r := open(t, dir, Options{})
	b := r.Budget()

	seen := map[string]bool{}
	total := 0
	opts := SearchOptions{Query: "needle", MaxResults: 7}
	for page := 0; page < 20; page++ {
		res, err := r.Search(bg, opts, b)
		if err != nil {
			t.Fatal(err)
		}
		if res.Matches > 7 {
			t.Fatalf("the result limit must be exact: %d", res.Matches)
		}
		file := ""
		for _, line := range strings.Split(res.Text, "\n") {
			switch {
			case strings.HasSuffix(line, ".txt"):
				file = line
			case strings.HasPrefix(line, "  ") && strings.Contains(line, ": needle"):
				key := file + line
				if seen[key] {
					t.Fatalf("duplicate match across pages: %s", key)
				}
				seen[key] = true
				total++
			}
		}
		if res.Complete {
			break
		}
		mustContain(t, res.Text, "INCOMPLETE", "result limit")
		mustNotContain(t, res.Text, "no match")
		if !res.Resumable {
			t.Fatal("an incomplete search must be resumable")
		}
		opts.FromFile, opts.FromLine, opts.FromFingerprint = res.NextFile, res.NextLine, res.NextFingerprint
	}
	if total != 36 {
		t.Fatalf("paging must visit every match exactly once: got %d of 36", total)
	}

	// Resuming inside a file that changed since is refused.
	first, _ := r.Search(bg, SearchOptions{Query: "needle", MaxResults: 2}, b)
	if first.NextLine == 0 {
		t.Fatal("expected the search to stop inside the first file")
	}
	write(t, dir, "f00.txt", "needle one\nnew line\nfiller\nneedle two\nneedle three\n")
	_, err := r.Search(bg, SearchOptions{Query: "needle", MaxResults: 2, FromFile: first.NextFile, FromLine: first.NextLine, FromFingerprint: first.NextFingerprint}, b)
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("a stale in-file position must be rejected: %v", err)
	}

	// A tiny size budget is also reported as incomplete.
	small, _ := r.Search(bg, SearchOptions{Query: "needle", MaxResults: 200}, 1400)
	if small.Complete || !small.Resumable {
		t.Errorf("size budget: %+v", small)
	}
	mustContain(t, small.Text, "INCOMPLETE")
}

func TestSearchSkipsAreCounted(t *testing.T) {
	dir := newDir(t)
	write(t, dir, "ok.txt", "needle\n")
	write(t, dir, "huge.txt", "needle\n"+strings.Repeat("x", searchFileLimit+10))
	write(t, dir, "blob.bin", "needle\x00")
	r := open(t, dir, Options{})
	res, err := r.Search(bg, SearchOptions{Query: "needle"}, r.Budget())
	if err != nil {
		t.Fatal(err)
	}
	if res.Matches != 1 {
		t.Errorf("matches: %d", res.Matches)
	}
	mustContain(t, res.Text, "1 binary skipped", "larger than", "of the files that could be searched")
}

// Masked text must not be discoverable through search: the predicate runs on
// what would be disclosed, so guessing a secret's characters changes nothing.
func TestSearchIsNotAnOracleForMaskedSecrets(t *testing.T) {
	dir := newDir(t)
	write(t, dir, "deploy.sh", "#!/bin/sh\nexport AWS_ACCESS_KEY_ID="+fakeAWSKey+"\necho done\n")
	write(t, dir, "id.txt", "notes\n"+fakePEM)
	r := open(t, dir, Options{})
	b := r.Budget()

	for _, guess := range []string{fakeAWSKey, "AKIAIOSF", "IOSFODNN7", "MIIEowIBAAKCAQ", "S2tU3vW4xY5", "BEGIN RSA PRIVATE"} {
		for _, opts := range []SearchOptions{
			{Query: guess, CaseSensitive: true},
			{Query: guess},
			{Query: guess[:4] + ".*", Regex: true, CaseSensitive: true},
		} {
			res, err := r.Search(bg, opts, b)
			if err != nil {
				t.Fatal(err)
			}
			if opts.Regex {
				mustNotContain(t, res.Text, fakeAWSKey, "MIIEow", "S2tU3v")
				continue
			}
			if res.Matches != 0 {
				t.Errorf("guess %q matched masked content:\n%s", guess, res.Text)
			}
		}
	}
	// The surrounding, disclosed text is still searchable and shows the mask.
	res, _ := r.Search(bg, SearchOptions{Query: "AWS_ACCESS_KEY_ID"}, b)
	if res.Matches != 1 {
		t.Fatalf("disclosed text must stay searchable:\n%s", res.Text)
	}
	mustContain(t, res.Text, "[REDACTED:aws-access-key-id]")
	mustNotContain(t, res.Text, fakeAWSKey)
	if res.Redactions == 0 {
		t.Error("masked values in results must be counted")
	}

	// With masking switched off by the owner, the same search finds it.
	raw := open(t, dir, Options{NoRedact: true})
	res, _ = raw.Search(bg, SearchOptions{Query: "AKIAIOSF"}, b)
	if res.Matches != 1 {
		t.Errorf("--no-redact must search raw content:\n%s", res.Text)
	}
}

func TestSearchSurvivesHostileContent(t *testing.T) {
	dir := newDir(t)
	write(t, dir, "evil.txt", "needle \x1b]0;pwned\x07 \x1b[2J and "+strings.Repeat("z", 3000)+" needle-tail\n")
	r := open(t, dir, Options{})
	res, err := r.Search(bg, SearchOptions{Query: "needle"}, r.Budget())
	if err != nil {
		t.Fatal(err)
	}
	mustNotContain(t, res.Text, "\x1b", "\x07")
	if len(res.Text) > 2000 {
		t.Errorf("long matching lines must be windowed, got %d bytes", len(res.Text))
	}
}
