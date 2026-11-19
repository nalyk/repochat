package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/nalyk/repochat/internal/mcp"
	"github.com/nalyk/repochat/internal/repo"
)

var bg = context.Background()

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=a@example.com",
		"GIT_COMMITTER_NAME=Ada", "GIT_COMMITTER_EMAIL=a@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T, opts repo.Options) (string, *repo.Repo, map[string]mcp.Tool) {
	t.Helper()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", t.TempDir())
	git(t, dir, "init", "-q", "-b", "main")
	for i := 0; i < 9; i++ {
		write(t, dir, fmt.Sprintf("pkg/f%d.go", i), fmt.Sprintf("package pkg\n\n// needle %d\nfunc F%d() {}\n", i, i))
	}
	write(t, dir, "README.md", "# Fixture\n")
	write(t, dir, ".env", "SECRET=NOPE\n")
	write(t, dir, "big.txt", strings.Repeat("0123456789 abcdefghij klmnopqrst\n", 4000))
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "first \x1b[31mcommit")
	r, err := repo.Open(bg, dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	byName := map[string]mcp.Tool{}
	for _, tool := range New(r) {
		byName[tool.Name] = tool
	}
	return dir, r, byName
}

// call validates the arguments like the server does, runs the tool and
// splits the trailing @meta line off the text.
func call(t *testing.T, tools map[string]mcp.Tool, name string, raw map[string]any) (string, map[string]any, error) {
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
		t.Fatalf("%s: result has no @meta line:\n%s", name, res.Text)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(res.Text[i+len("\n@meta "):]), &meta); err != nil {
		t.Fatalf("%s: @meta is not JSON: %v", name, err)
	}
	return res.Text[:i], meta, nil
}

var toolNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

func TestCatalogIsWellFormed(t *testing.T) {
	_, _, tools := fixture(t, repo.Options{})
	want := []string{"repo_overview", "list_files", "find_files", "read_file", "read_files", "search_code", "file_outline",
		"git_status", "git_log", "git_diff", "git_show", "git_blame", "git_refs"}
	if len(tools) != len(want) {
		t.Fatalf("expected %d tools, got %d", len(want), len(tools))
	}
	for _, name := range want {
		tool, ok := tools[name]
		if !ok {
			t.Errorf("missing tool %s", name)
			continue
		}
		if !toolNameRe.MatchString(tool.Name) || tool.Title == "" || len(tool.Description) < 40 {
			t.Errorf("%s: name/title/description are not well formed", name)
		}
		raw, _ := json.Marshal(mcp.ToolJSON(tool))
		if strings.Contains(string(raw), "null") {
			t.Errorf("%s: definition contains null: %s", name, raw)
		}
		schema := tool.Schema.JSON()
		props := schema["properties"].(map[string]any)
		for pname, p := range props {
			if desc, _ := p.(map[string]any)["description"].(string); len(desc) < 10 {
				t.Errorf("%s.%s: parameters need a description", name, pname)
			}
		}
		if req, ok := schema["required"].([]string); ok {
			for _, r := range req {
				if _, ok := props[r]; !ok {
					t.Errorf("%s: required %q is not a property", name, r)
				}
			}
		}
	}
}

func TestInstructionsAreStatic(t *testing.T) {
	dir, r, _ := fixture(t, repo.Options{})
	if strings.Contains(Instructions, r.Name) || strings.Contains(Instructions, dir) {
		t.Error("instructions must not contain repository-derived text")
	}
	head := Instructions
	if len(head) > 512 {
		head = head[:512]
	}
	for _, must := range []string{"Read-only", "repo_overview", "search_code", "read_file"} {
		if !strings.Contains(head, must) {
			t.Errorf("the first 512 characters must stand alone; missing %q", must)
		}
	}
	if !strings.Contains(Instructions, "untrusted") {
		t.Error("instructions must say repository content is untrusted data")
	}
}

func TestEveryToolEndsWithMeta(t *testing.T) {
	_, r, tools := fixture(t, repo.Options{})
	calls := map[string]map[string]any{
		"repo_overview": {}, "list_files": {}, "find_files": {"query": "f1"}, "read_file": {"path": "README.md"},
		"read_files": {"paths": []any{"README.md", "pkg/f1.go"}}, "search_code": {"query": "needle"},
		"file_outline": {"path": "pkg/f1.go"}, "git_status": {}, "git_log": {}, "git_diff": {},
		"git_show": {"rev": "HEAD"}, "git_blame": {"path": "README.md"}, "git_refs": {},
	}
	for name, args := range calls {
		text, meta, err := call(t, tools, name, args)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if meta["tool"] != name || meta["repo"] != r.ID {
			t.Errorf("%s: bad meta %v", name, meta)
		}
		if _, ok := meta["complete"]; !ok {
			t.Errorf("%s: meta must state completeness: %v", name, meta)
		}
		if strings.Contains(text, "\x1b") || strings.Contains(text, "NOPE") {
			t.Errorf("%s: raw control characters or withheld content in output", name)
		}
	}
	text, _, _ := call(t, tools, "git_log", map[string]any{})
	if !strings.Contains(text, `first \x1b[31mcommit`) {
		t.Errorf("control characters in commit subjects must be shown escaped:\n%s", text)
	}
}

func TestListCursor(t *testing.T) {
	dir, r, tools := fixture(t, repo.Options{})
	args := map[string]any{"path": "pkg", "limit": 4}
	seen := map[string]bool{}
	pages := 0
	for {
		text, meta, err := call(t, tools, "list_files", args)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, line := range strings.Split(text, "\n") {
			if f := strings.Fields(line); len(f) > 0 && strings.HasPrefix(f[0], "pkg/") {
				if seen[f[0]] {
					t.Fatalf("duplicate entry %s", f[0])
				}
				seen[f[0]] = true
			}
		}
		cursor, _ := meta["cursor"].(string)
		if cursor == "" {
			if meta["complete"] != true {
				t.Errorf("last page must be complete: %v", meta)
			}
			break
		}
		if len(cursor) > 512 {
			t.Errorf("cursor too long: %d", len(cursor))
		}
		args = map[string]any{"path": "pkg", "limit": 4, "cursor": cursor}
	}
	if len(seen) != 9 || pages != 3 {
		t.Fatalf("paging: %d entries in %d pages", len(seen), pages)
	}

	// A cursor is bound to its call and to the policy/file-set generation.
	_, meta, _ := call(t, tools, "list_files", map[string]any{"path": "pkg", "limit": 4})
	cursor := meta["cursor"].(string)
	if _, _, err := call(t, tools, "list_files", map[string]any{"path": "pkg", "limit": 5, "cursor": cursor}); err == nil {
		t.Error("a cursor must not be accepted with different arguments")
	}
	if _, _, err := call(t, tools, "search_code", map[string]any{"query": "needle", "cursor": cursor}); err == nil {
		t.Error("a cursor must not be accepted by another tool")
	}
	if _, _, err := call(t, tools, "list_files", map[string]any{"path": "pkg", "limit": 4, "cursor": "!!!"}); err == nil {
		t.Error("garbage cursors must be rejected")
	}
	write(t, dir, "pkg/new.go", "package pkg\n")
	other, err := repo.Open(bg, dir, repo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if other.ID != r.ID {
		t.Fatal("same repository must have the same id")
	}
	for _, tool := range New(other) {
		if tool.Name != "list_files" {
			continue
		}
		a, _ := tool.Schema.Validate(map[string]any{"path": "pkg", "limit": 4, "cursor": cursor})
		if _, err := tool.Handler(bg, a); err == nil || !strings.Contains(err.Error(), "without cursor") {
			t.Errorf("a cursor from an older file set must be rejected with a restart hint: %v", err)
		}
	}
}

func TestSearchCursor(t *testing.T) {
	_, _, tools := fixture(t, repo.Options{})
	args := map[string]any{"query": "needle", "max_results": 4}
	matches := 0
	for page := 0; page < 10; page++ {
		_, meta, err := call(t, tools, "search_code", args)
		if err != nil {
			t.Fatal(err)
		}
		matches += int(meta["matches"].(float64))
		cursor, _ := meta["cursor"].(string)
		if meta["complete"] == true {
			if cursor != "" {
				t.Error("a complete search has no cursor")
			}
			break
		}
		if cursor == "" {
			t.Fatal("an incomplete search must give a cursor")
		}
		args = map[string]any{"query": "needle", "max_results": 4, "cursor": cursor}
	}
	if matches != 9 {
		t.Fatalf("expected 9 matches over all pages, got %d", matches)
	}
}

func TestBudgets(t *testing.T) {
	_, r, tools := fixture(t, repo.Options{MaxOutputBytes: 16 << 10})
	text, meta, err := call(t, tools, "read_file", map[string]any{"path": "big.txt", "max_lines": 2000})
	if err != nil {
		t.Fatal(err)
	}
	if len(text) > r.Budget() {
		t.Errorf("result (%d bytes) exceeds the budget (%d)", len(text), r.Budget())
	}
	next, _ := meta["next_start_line"].(float64)
	if next < 2 || meta["complete"] != false || meta["total_lines"].(float64) != 4000 {
		t.Errorf("a cut read must say where to continue: %v", meta)
	}
	text, _, err = call(t, tools, "read_file", map[string]any{"path": "big.txt", "start_line": next, "max_lines": 3})
	if err != nil || !strings.Contains(text, fmt.Sprintf("%d| 0123456789", int(next))) {
		t.Errorf("continuation failed: %v\n%s", err, text[:min(len(text), 300)])
	}

	text, meta, err = call(t, tools, "read_files", map[string]any{"paths": []any{"big.txt", "README.md", "pkg/f1.go", ".env", "nope.txt"}, "max_lines_each": 2000})
	if err != nil {
		t.Fatal(err)
	}
	if len(text) > r.Budget() {
		t.Errorf("read_files exceeds the budget: %d", len(text))
	}
	if meta["complete"] != false {
		t.Errorf("read_files must report incompleteness: %v", meta)
	}
	if strings.Contains(text, "NOPE") {
		t.Error("withheld content in read_files")
	}
}

func TestReadFilesReportsPerFileErrors(t *testing.T) {
	_, _, tools := fixture(t, repo.Options{})
	text, _, err := call(t, tools, "read_files", map[string]any{"paths": []any{"README.md", ".env", "nope.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Fixture", ".env · NOT READ", "withheld", "nope.txt · NOT READ"} {
		if !strings.Contains(text, want) {
			t.Errorf("expected %q in:\n%s", want, text)
		}
	}
}

// The @meta footer carries repository-derived strings and must be masked
// like the body.
func TestMetaFooterIsMasked(t *testing.T) {
	dir, _, tools := fixture(t, repo.Options{})
	token := "ghp_" + strings.Repeat("A", 36)
	write(t, dir, token+".go", "package main\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "token-named file")
	git(t, dir, "checkout", "-q", "-b", "feature/"+token)
	fresh, err := repo.Open(bg, dir, repo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	byName := map[string]mcp.Tool{}
	for _, tool := range New(fresh) {
		byName[tool.Name] = tool
	}
	_ = tools
	for name, args := range map[string]map[string]any{
		"read_file":     {"path": token + ".go"},
		"read_files":    {"paths": []any{token + ".go", "missing-" + token}},
		"repo_overview": {},
		"git_status":    {},
		"file_outline":  {"path": token + ".go"},
	} {
		tool := byName[name]
		a, err := tool.Schema.Validate(args)
		if err != nil {
			t.Fatal(err)
		}
		res, err := tool.Handler(bg, a)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(res.Text, token) {
			t.Errorf("%s: a secret-looking value survived in the result or its @meta footer:\n%s", name, res.Text[max(0, len(res.Text)-400):])
		}
	}
}

func TestErrorsAreMaskedAndSanitized(t *testing.T) {
	_, _, tools := fixture(t, repo.Options{})
	_, _, err := call(t, tools, "read_file", map[string]any{"path": "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB/x\x1b.go"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "\x1b") || strings.Contains(err.Error(), "ghp_0123456789") {
		t.Errorf("error text must be masked and sanitised: %q", err)
	}
}

// A repository directory whose name looks like a secret must not put that
// value into every result through the repository id.
func TestRepositoryNameIsNotASecretChannel(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	token := "ghp_" + strings.Repeat("A", 36)
	dir := filepath.Join(base, token)
	os.MkdirAll(dir, 0o755)
	t.Setenv("HOME", t.TempDir())
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "a.txt", "hello\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "first")
	r, err := repo.Open(bg, dir, repo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if strings.Contains(r.ID, "ghp_") || strings.Contains(r.Name, "ghp_") {
		t.Fatalf("repository id carries the token: %s", r.ID)
	}
	for _, tool := range New(r) {
		var raw map[string]any
		switch tool.Name {
		case "repo_overview":
			raw = map[string]any{}
		case "read_file":
			raw = map[string]any{"path": "a.txt"}
		default:
			continue
		}
		args, err := tool.Schema.Validate(raw)
		if err != nil {
			t.Fatal(err)
		}
		res, err := tool.Handler(bg, args)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(res.Text, token) {
			t.Errorf("%s: the directory name leaked:\n%s", tool.Name, res.Text)
		}
	}
}
