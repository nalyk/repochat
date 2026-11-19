package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nalyk/repochat/internal/mcp"
	"github.com/nalyk/repochat/internal/repo"
)

// Regression tests for the defects found in the security and correctness
// review of the tool layer.

// reopen opens dir again (after a test changed it) and returns its tools.
func reopen(t *testing.T, dir string, opts repo.Options) (*repo.Repo, map[string]mcp.Tool) {
	t.Helper()
	r, err := repo.Open(bg, dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	byName := map[string]mcp.Tool{}
	for _, tool := range New(r) {
		byName[tool.Name] = tool
	}
	return r, byName
}

// rawCall runs a tool and returns the complete result text, footer included,
// together with the decoded footer.
func rawCall(t *testing.T, tools map[string]mcp.Tool, name string, raw map[string]any) (string, map[string]any, error) {
	t.Helper()
	tool, ok := tools[name]
	if !ok {
		t.Fatalf("no tool %s", name)
	}
	args, err := tool.Schema.Validate(raw)
	if err != nil {
		t.Fatalf("%s: invalid arguments: %v", name, err)
	}
	res, err := tool.Handler(bg, args)
	if err != nil {
		return "", nil, err
	}
	i := strings.LastIndex(res.Text, "\n@meta ")
	if i < 0 {
		t.Fatalf("%s: result has no @meta line:\n%s", name, res.Text[max(0, len(res.Text)-300):])
	}
	footer := res.Text[i+len("\n@meta "):]
	if strings.Contains(footer, "\n") {
		t.Fatalf("%s: the @meta footer must be the last line and a single line", name)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(footer), &meta); err != nil {
		t.Fatalf("%s: @meta is not JSON: %v\n%s", name, err, footer[:min(len(footer), 300)])
	}
	return res.Text, meta, nil
}

// ---------------------------------------------------------------------------
// complete
// ---------------------------------------------------------------------------

// "complete" is what a caller relies on to stop asking. It must be false
// whenever anything was cut short or left out, whatever the reason.
func TestCompleteIsFalseWheneverSomethingWasCut(t *testing.T) {
	dir, _, _ := fixture(t, repo.Options{})

	// One patch larger than the per-file share of the budget.
	write(t, dir, "wide.txt", strings.Repeat("old line of text, sixty characters long, more or less .....\n", 4000))
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "wide")
	write(t, dir, "wide.txt", strings.Repeat("new line of text, sixty characters long, more or less .....\n", 4000))
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "rewrite wide")
	// One commit with more files than a log entry lists.
	for i := 0; i < 30; i++ {
		write(t, dir, fmt.Sprintf("many/f%02d.txt", i), "x\n")
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "many files")
	// More untracked files than one status section lists, more tags than
	// one refs section lists.
	for i := 0; i < 305; i++ {
		write(t, dir, fmt.Sprintf("untracked/u%03d.txt", i), "x\n")
	}
	for i := 0; i < 205; i++ {
		git(t, dir, "tag", fmt.Sprintf("t%03d", i))
	}
	_, tools := reopen(t, dir, repo.Options{})

	for name, c := range map[string]struct {
		tool string
		args map[string]any
		want []string // text that shows the cut really happened
	}{
		"diff: patch cut":     {"git_diff", map[string]any{"base": "HEAD~2", "target": "HEAD~1"}, []string{"[patch for wide.txt cut at"}},
		"show: patch cut":     {"git_show", map[string]any{"rev": "HEAD~1"}, []string{"[patch for wide.txt cut at"}},
		"log: file list cut":  {"git_log", map[string]any{"with_stats": true, "range": "HEAD~1..HEAD"}, []string{"… 5 more file(s)"}},
		"status: section cut": {"git_status", map[string]any{}, []string{"… 5 more"}},
		"refs: section cut":   {"git_refs", map[string]any{}, []string{"… 5 more; narrow with filter"}},
	} {
		text, meta, err := call(t, tools, c.tool, c.args)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(text, w) {
				t.Errorf("%s: the fixture did not produce the cut (%q missing):\n%s", name, w, text[max(0, len(text)-600):])
			}
		}
		if meta["complete"] != false {
			t.Errorf("%s: something was cut, yet complete=%v in %v", name, meta["complete"], meta)
		}
	}
	_, meta, err := call(t, tools, "git_diff", map[string]any{"base": "HEAD~2", "target": "HEAD~1"})
	if err != nil {
		t.Fatal(err)
	}
	if cut, _ := meta["patches_cut"].([]any); len(cut) != 1 || cut[0] != "wide.txt" {
		t.Errorf("the footer must name the patch that was cut: %v", meta)
	}

	// Nothing cut: complete stays true.
	for name, c := range map[string]struct {
		tool string
		args map[string]any
	}{
		"diff":  {"git_diff", map[string]any{"base": "HEAD~1", "paths": []any{"many/f01.txt"}}},
		"show":  {"git_show", map[string]any{"rev": "HEAD", "stat_only": true}},
		"log":   {"git_log", map[string]any{"limit": 100}},
		"refs":  {"git_refs", map[string]any{"filter": "main"}},
		"blame": {"git_blame", map[string]any{"path": "README.md"}},
	} {
		_, meta, err := call(t, tools, c.tool, c.args)
		if err != nil || meta["complete"] != true {
			t.Errorf("%s: an uncut result must be complete: %v %v", name, err, meta)
		}
	}
	for i := 0; i < 305; i++ {
		os.Remove(filepath.Join(dir, fmt.Sprintf("untracked/u%03d.txt", i)))
	}
	_, tools = reopen(t, dir, repo.Options{})
	if _, meta, err := call(t, tools, "git_status", map[string]any{}); err != nil || meta["complete"] != true {
		t.Errorf("status: an uncut result must be complete: %v %v", err, meta)
	}
}

// ---------------------------------------------------------------------------
// NoHistory
// ---------------------------------------------------------------------------

// Every history tool answers --no-history with the same refusal, and the
// tools that remain put no commit id in their footer.
func TestNoHistoryThroughTheTools(t *testing.T) {
	dir, _, withHistory := fixture(t, repo.Options{})
	os.Remove(filepath.Join(dir, "README.md"))
	_, full, err := call(t, withHistory, "git_status", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	head, _ := full["head"].(string)
	if len(head) < 40 {
		t.Fatalf("positive control: with history the footer names HEAD: %v", full)
	}
	_, tools := reopen(t, dir, repo.Options{NoHistory: true})

	_, _, want := call(t, tools, "git_log", map[string]any{})
	if want == nil || !strings.Contains(want.Error(), "--no-history") {
		t.Fatalf("git_log under --no-history: %v", want)
	}
	for name, c := range map[string]struct {
		tool string
		args map[string]any
	}{
		"diff":          {"git_diff", map[string]any{}},
		"diff stat":     {"git_diff", map[string]any{"stat_only": true}},
		"diff staged":   {"git_diff", map[string]any{"mode": "staged"}},
		"diff worktree": {"git_diff", map[string]any{"mode": "worktree"}},
		"diff range":    {"git_diff", map[string]any{"base": "HEAD"}},
		"show":          {"git_show", map[string]any{"rev": "HEAD"}},
		"blame":         {"git_blame", map[string]any{"path": "pkg/f1.go"}},
		"refs":          {"git_refs", map[string]any{}},
		"read at rev":   {"read_file", map[string]any{"path": "README.md", "rev": "HEAD"}},
	} {
		text, _, err := call(t, tools, c.tool, c.args)
		if err == nil {
			t.Errorf("%s must be refused under --no-history, got:\n%s", name, text)
		} else if err.Error() != want.Error() {
			t.Errorf("%s: every history tool gives the same refusal:\n  %v\n  %v", name, err, want)
		}
	}
	for _, name := range []string{"git_status", "repo_overview"} {
		text, meta, err := call(t, tools, name, map[string]any{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, has := meta["head"]; has {
			t.Errorf("%s: the footer names the HEAD commit under --no-history: %v", name, meta)
		}
		if strings.Contains(text, head[:7]) || strings.Contains(text, "# Fixture") || strings.Contains(text, "Last commit") {
			t.Errorf("%s: commit data under --no-history:\n%s", name, text)
		}
	}
}

// ---------------------------------------------------------------------------
// Size budget
// ---------------------------------------------------------------------------

// The budget bounds the whole result, footer included. Paths can be long and
// the footer lists them, so the footer is bounded too: its lists are
// shortened, with a count and a flag that says so.
func TestFooterIsPartOfTheSizeBudget(t *testing.T) {
	dir, _, _ := fixture(t, repo.Options{MaxOutputBytes: repo.MinOutput})
	deep := strings.Repeat("a", 200) + "/" + strings.Repeat("b", 200) + "/" + strings.Repeat("c", 200) + "/" + strings.Repeat("d", 200)
	var long []string
	for i := 0; i < 60; i++ {
		p := fmt.Sprintf("%s/f%02d.txt", deep, i)
		long = append(long, p)
		write(t, dir, p, strings.Repeat("old line\n", 600))
	}
	deepGo := deep + "/code.go"
	write(t, dir, deepGo, "package deep\n\nfunc Deep() {}\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "deep files")
	for _, p := range long {
		write(t, dir, p, strings.Repeat("new line\n", 600))
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "rewrite deep files")
	branch := "topic/" + strings.Repeat("x", 180) + "/" + strings.Repeat("y", 180) + "/" + strings.Repeat("z", 180)
	git(t, dir, "checkout", "-q", "-b", branch)
	for i := 0; i < 12; i++ {
		write(t, dir, long[i], strings.Repeat("uncommitted line\n", 600))
	}
	write(t, dir, long[0], strings.Repeat("uncommitted line\n", 3000)) // several pages at this budget
	r, tools := reopen(t, dir, repo.Options{MaxOutputBytes: repo.MinOutput})
	if r.Budget() != repo.MinOutput {
		t.Fatalf("budget: %d", r.Budget())
	}

	anyList := func(xs []string) []any {
		out := make([]any, len(xs))
		for i, x := range xs {
			out[i] = x
		}
		return out
	}
	missing := make([]string, 10)
	for i := range missing {
		missing[i] = fmt.Sprintf("%s/missing%02d.txt", deep, i)
	}
	type listCheck struct {
		key   string
		total int // 0: only when present
	}
	calls := []struct {
		name  string
		tool  string
		args  map[string]any
		lists []listCheck
	}{
		{"diff range", "git_diff", map[string]any{"base": "HEAD~1"}, []listCheck{{"patches_omitted", 0}}},
		{"diff range, no context", "git_diff", map[string]any{"base": "HEAD~1", "context_lines": 0}, []listCheck{{"patches_omitted", 0}}},
		{"diff head", "git_diff", map[string]any{}, []listCheck{{"patches_omitted", 0}}},
		{"diff stat", "git_diff", map[string]any{"base": "HEAD~1", "stat_only": true}, nil},
		{"show", "git_show", map[string]any{"rev": "HEAD"}, []listCheck{{"patches_omitted", 0}}},
		{"show stat", "git_show", map[string]any{"rev": "HEAD", "stat_only": true}, nil},
		{"read_files, existing", "read_files", map[string]any{"paths": anyList(long[:10]), "max_lines_each": 2000}, []listCheck{{"files", 0}, {"not_read", 0}}},
		{"read_files, short reads", "read_files", map[string]any{"paths": anyList(long[:10]), "max_lines_each": 1}, []listCheck{{"files", 10}}},
		{"read_files, missing", "read_files", map[string]any{"paths": anyList(missing)}, []listCheck{{"files", 0}}},
		{"read_file", "read_file", map[string]any{"path": long[0], "max_lines": 2000}, nil},
		{"file_outline", "file_outline", map[string]any{"path": deepGo}, nil},
		{"git_blame", "git_blame", map[string]any{"path": long[20], "end_line": 200}, nil},
		{"git_status", "git_status", map[string]any{}, nil},
		{"repo_overview", "repo_overview", map[string]any{}, nil},
		{"git_log", "git_log", map[string]any{"with_stats": true}, nil},
		{"git_refs", "git_refs", map[string]any{}, nil},
		{"list_files", "list_files", map[string]any{"depth": 12, "limit": 1000}, nil},
		{"find_files", "find_files", map[string]any{"query": "f", "limit": 200}, nil},
		{"search_code", "search_code", map[string]any{"query": "line", "max_results": 200}, nil},
	}
	for _, c := range calls {
		text, meta, err := rawCall(t, tools, c.tool, c.args)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if len(text) > r.Budget() {
			t.Errorf("%s: the result is %d bytes, the budget is %d (footer: %d bytes)", c.name, len(text), r.Budget(),
				len(text)-strings.LastIndex(text, "\n@meta "))
		}
		if meta["tool"] != c.tool || meta["repo"] != r.ID {
			t.Errorf("%s: the footer lost its identity: %v", c.name, meta)
		}
		if _, ok := meta["complete"]; !ok {
			t.Errorf("%s: the footer must still state completeness", c.name)
		}
		// Hints for continuing were computed for the uncut text. Once the
		// text is cut here they would make a caller skip what was cut.
		if meta["truncated"] == true {
			if meta["complete"] != false {
				t.Errorf("%s: a cut result is not complete: %v", c.name, meta)
			}
			for _, hint := range []string{"cursor", "next_start_line", "next_skip", "last_line", "files"} {
				if _, has := meta[hint]; has && (c.tool == "read_file" || c.tool == "read_files" || hint != "files") {
					t.Errorf("%s: the text was cut at the budget but the footer still carries %q, which points past the cut", c.name, hint)
				}
			}
		}
		for _, l := range c.lists {
			list, _ := meta[l.key].([]any)
			count, hasCount := meta[l.key+"_count"].(float64)
			flagged := meta[l.key+"_truncated"] == true
			switch {
			case flagged != hasCount:
				t.Errorf("%s: %s: a shortened list needs both the count and the flag: %v", c.name, l.key, meta)
			case flagged && int(count) <= len(list):
				t.Errorf("%s: %s: flagged as shortened but %d of %d entries are present", c.name, l.key, len(list), int(count))
			case l.total > 0 && !flagged && len(list) != l.total:
				t.Errorf("%s: %s: %d entries without a flag, %d expected", c.name, l.key, len(list), l.total)
			case l.total > 0 && flagged && int(count) != l.total:
				t.Errorf("%s: %s: count %d, expected %d", c.name, l.key, int(count), l.total)
			}
		}
	}

	// The fixture really exercises the footer lists: the paths left out by a
	// diff do not all fit, and what is dropped is accounted for.
	_, meta, err := rawCall(t, tools, "git_diff", map[string]any{"base": "HEAD~1"})
	if err != nil {
		t.Fatal(err)
	}
	if meta["patches_omitted_truncated"] != true || meta["complete"] != false {
		t.Errorf("git_diff: a shortened patches_omitted list must be flagged: %v", meta)
	}
	if n, _ := meta["patches_omitted_count"].(float64); n < 50 {
		t.Errorf("git_diff: patches_omitted_count must be the number of patches left out, got %v", meta["patches_omitted_count"])
	}
	_, meta, err = rawCall(t, tools, "read_files", map[string]any{"paths": anyList(long[:10]), "max_lines_each": 1})
	if err != nil {
		t.Fatal(err)
	}
	if meta["files_truncated"] != true || meta["files_count"] != float64(10) {
		t.Errorf("read_files: a shortened files list must be flagged with its count: %v", meta)
	}

	// A handler that pages its output leaves room for the footer, so a long
	// path in the footer does not get the text cut (and the hint for
	// continuing withdrawn): reading a long file still pages.
	seen := 0
	for start, calls := 1, 0; start > 0; calls++ {
		if calls > 20 {
			t.Fatal("read_file does not make progress")
		}
		text, meta, err := rawCall(t, tools, "read_file", map[string]any{"path": long[0], "start_line": start, "max_lines": 2000})
		if err != nil {
			t.Fatal(err)
		}
		if meta["truncated"] == true || len(text) > r.Budget() {
			t.Fatalf("read_file with a long path: the text was cut at the budget instead of being paged: %d bytes, %v", len(text), meta)
		}
		first, _ := meta["first_line"].(float64)
		last, _ := meta["last_line"].(float64)
		next, _ := meta["next_start_line"].(float64)
		if int(first) != start || !strings.Contains(text, fmt.Sprintf("%d| uncommitted line\n", int(last))) {
			t.Fatalf("read_file: the footer does not describe the text: %v", meta)
		}
		seen += int(last) - int(first) + 1
		if next == 0 && meta["complete"] != true {
			t.Fatalf("read_file: neither complete nor a line to continue from: %v", meta)
		}
		start = int(next)
	}
	if seen != 3000 {
		t.Errorf("read_file: %d of 3000 lines over all pages", seen)
	}

	// With an ordinary budget and ordinary paths nothing is shortened.
	_, tools = reopen(t, dir, repo.Options{})
	_, meta, err = rawCall(t, tools, "read_files", map[string]any{"paths": []any{"README.md", "pkg/f1.go", "nope.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if files, _ := meta["files"].([]any); len(files) != 3 || meta["files_truncated"] != nil || meta["files_count"] != nil {
		t.Errorf("read_files: an ordinary footer must be left alone: %v", meta)
	}
}

// finish itself, for every text length around the point where the result
// stops fitting and for footers of every shape: the result fits, the footer
// is valid, and a result that was not cut keeps its text and its hints.
func TestFinishAtTheBudgetBoundary(t *testing.T) {
	_, r, _ := fixture(t, repo.Options{MaxOutputBytes: repo.MinOutput})
	c := &catalog{r: r}
	budget := r.Budget()
	line := "0123456789 abcdefghij klmnopqrstuvwxyz é\n" // multi-byte, so a cut can fall inside a character
	long := strings.Repeat("p", 3000)
	footers := map[string]func() meta{
		"small": func() meta { return meta{"complete": true, "next_start_line": 7} },
		"cursor": func() meta {
			return meta{"complete": false, "cursor": strings.Repeat("c", 500), "next_skip": 3, "last_line": 9}
		},
		"long strings": func() meta {
			return meta{"complete": true, "path": long, "branch": long + "b", "head": long + "h", "language": long, "rev": long}
		},
		"long string with wide characters": func() meta { return meta{"complete": true, "path": strings.Repeat("é", 2000)} },
		"lists": func() meta {
			files := make([]meta, 10)
			for i := range files {
				files[i] = meta{"path": long[:900], "first_line": 1, "last_line": 5, "complete": true}
			}
			return meta{"complete": true, "files": files, "not_read": []string{long, long, long}, "patches_omitted": []string{"a", "b"}}
		},
		"everything": func() meta {
			return meta{"complete": true, "cursor": strings.Repeat("c", 500), "path": long, "not_read": []string{long, long},
				"patches_omitted": []string{long}, "patches_cut": []string{long}, "next_start_line": 2}
		},
		// More long strings than fit even when each one is cut: no tool
		// produces this, but the footer must still be bounded.
		"more strings than fit": func() meta {
			m := meta{"complete": true, "cursor": strings.Repeat("c", 500)}
			for i := 0; i < 40; i++ {
				m[fmt.Sprintf("key%02d", i)] = long
			}
			return m
		},
	}
	for name, footer := range footers {
		for _, noNewlines := range []bool{false, true} {
			if noNewlines && name != "small" && name != "everything" {
				continue // one line without a place to cut: two footers are enough
			}
			// From where the largest footer stops fitting to past the budget.
			for size := budget - c.footerLimit() - 400; size <= budget+100; size += 149 {
				text := strings.Repeat(line, size/len(line)+1)[:size]
				text = strings.ToValidUTF8(text, "?")
				if noNewlines {
					text = strings.ReplaceAll(text, "\n", " ")
				}
				res := c.finish("some_tool", text, 0, footer())
				if len(res.Text) > budget {
					t.Fatalf("%s, %d bytes of text: the result is %d bytes, the budget %d", name, size, len(res.Text), budget)
				}
				if !utf8.ValidString(res.Text) {
					t.Fatalf("%s, %d bytes of text: the cut split a character", name, size)
				}
				i := strings.LastIndex(res.Text, "\n@meta ")
				if i < 0 {
					t.Fatalf("%s, %d bytes of text: no footer", name, size)
				}
				var got map[string]any
				if err := json.Unmarshal([]byte(res.Text[i+len("\n@meta "):]), &got); err != nil {
					t.Fatalf("%s, %d bytes of text: the footer is not JSON: %v", name, size, err)
				}
				if got["tool"] != "some_tool" || got["repo"] != r.ID {
					t.Fatalf("%s: the footer lost its identity: %v", name, got)
				}
				if got["truncated"] == true {
					if got["complete"] != false || got["cursor"] != nil || got["next_start_line"] != nil || got["next_skip"] != nil {
						t.Fatalf("%s, %d bytes of text: a cut result must not look complete or resumable: %v", name, size, got)
					}
					if !strings.Contains(res.Text[:i+1], "[result cut at the size budget") {
						t.Fatalf("%s, %d bytes of text: a cut result must say so in its text", name, size)
					}
					continue
				}
				if body := strings.TrimSuffix(res.Text[:i], "\n"); body != strings.TrimSuffix(text, "\n") {
					t.Fatalf("%s, %d bytes of text: the text changed although nothing was cut", name, size)
				}
				if want := footer()["cursor"]; want != nil && got["cursor"] != want {
					t.Fatalf("%s: the cursor must stay byte-exact: %v", name, got["cursor"])
				}
				for k, v := range got {
					if s, ok := v.(string); ok && len(s) > 600 && k != "cursor" {
						t.Fatalf("%s: %s is still %d bytes in a footer limited to %d", name, k, len(s), c.footerLimit())
					}
				}
			}
		}
	}
}

func TestOutlineCutIsNotComplete(t *testing.T) {
	dir, _, tools := fixture(t, repo.Options{})
	var src strings.Builder
	src.WriteString("package pkg\n")
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&src, "func G%d() {}\n", i)
	}
	write(t, dir, "pkg/many.go", src.String())
	time.Sleep(3100 * time.Millisecond) // past the snapshot's lifetime
	_, meta, err := call(t, tools, "file_outline", map[string]any{"path": "pkg/many.go"})
	if err != nil || meta["complete"] != false {
		t.Fatalf("a cut outline must not be reported complete: %v %v", err, meta)
	}
}
