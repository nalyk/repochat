package tools

import (
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

// The exposure boundary, exercised from outside: every tool, with every kind
// of argument that can name or reach a file, against a repository whose
// .repochatignore withholds some files in the working tree and some that
// exist only in history. Each withheld file carries a canary no result may
// contain; each served file carries a marker that must come through.
func TestExposureBoundaryThroughEveryTool(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", t.TempDir())
	git(t, dir, "init", "-q")
	git(t, dir, "symbolic-ref", "HEAD", "refs/heads/main")
	rules := "vault/\n*.hidden.md\n!keep.hidden.md\n*.dat\n!/a**/z.dat\n"
	write(t, dir, ".repochatignore", rules)

	withheld := map[string]string{ // path → canary
		"vault/keys.txt":       "CANARY-VAULT",
		"notes.hidden.md":      "CANARY-NOTES",
		"vault/keep.hidden.md": "CANARY-VKEEP", // a negation cannot reach into an excluded directory
		"old/gone.hidden.md":   "CANARY-GONE",  // history only
		"vault/old.go":         "CANARY-VOLD",  // history only
		"data/table.dat":       "CANARY-TABLE",
	}
	served := map[string]string{ // path → marker
		"src/app.go":            "SERVED-APP",
		"public/keep.hidden.md": "SERVED-KEEP", // re-included by the negation
		"old/gone.go":           "SERVED-GONE", // history only
	}
	// git releases disagree on what "**" means in "/a**/z.dat"; whatever the
	// installed git says is what the boundary must do for this file.
	const divergent, divergentMark = "a/b/z.dat", "DIVERGENT-Z"
	for p, c := range withheld {
		write(t, dir, p, "package x\n// "+c+"\nfunc Withheld() {}\n")
	}
	for p, c := range served {
		write(t, dir, p, "package x\n// "+c+"\nfunc Served() {}\n")
	}
	write(t, dir, divergent, divergentMark+"\n")
	git(t, dir, "add", "-A", "-f")
	git(t, dir, "commit", "-q", "-m", "first: everything")
	first := revParse(t, dir, "HEAD")
	for _, p := range []string{"old/gone.hidden.md", "vault/old.go", "old/gone.go"} {
		os.Remove(filepath.Join(dir, p))
	}
	git(t, dir, "add", "-A", "-f")
	git(t, dir, "commit", "-q", "-m", "second: some files leave the working tree")
	// Uncommitted and untracked changes on both sides of the boundary.
	write(t, dir, "vault/keys.txt", "package x\n// CANARY-VAULT\n// CANARY-EDIT\nfunc Withheld() {}\n")
	write(t, dir, "src/app.go", "package x\n// SERVED-APP\n// SERVED-EDIT\nfunc Served() {}\n")
	write(t, dir, "vault/new.txt", "CANARY-NEW\n")
	write(t, dir, "src/new.go", "package x\n// SERVED-NEW\n")
	withheld["vault/new.txt"] = "CANARY-NEW"
	served["src/new.go"] = "SERVED-NEW"

	gitIgnoresDivergent := gitSaysIgnored(t, rules, divergent)
	t.Logf("installed git ignores %s under these rules: %v", divergent, gitIgnoresDivergent)

	for _, opts := range []repo.Options{{}, {NoRedact: true}} {
		r, err := repo.Open(bg, dir, opts)
		if err != nil {
			t.Fatal(err)
		}
		tools := map[string]mcp.Tool{}
		for _, tool := range New(r) {
			tools[tool.Name] = tool
		}
		// run returns what the model would see: the text, or the error text.
		run := func(name string, args map[string]any) string {
			text, meta, err := call(t, tools, name, args)
			if err != nil {
				return "ERROR: " + err.Error()
			}
			return text + fmt.Sprint(meta)
		}

		all := []string{divergent, ".repochatignore"}
		for p := range withheld {
			all = append(all, p)
		}
		for p := range served {
			all = append(all, p)
		}
		// Calls that name no file: what they reveal, they reveal unasked.
		var discovery []string
		discovery = append(discovery,
			run("repo_overview", nil),
			run("list_files", map[string]any{"depth": 8}),
			run("list_files", map[string]any{"path": "vault"}),
			run("list_files", map[string]any{"glob": "*.txt"}),
			run("find_files", map[string]any{"query": "keys"}),
			run("find_files", map[string]any{"query": "*.txt"}),
			run("find_files", map[string]any{"query": "gone"}),
			run("search_code", map[string]any{"query": "CANARY"}),
			run("search_code", map[string]any{"query": "SERVED"}),
			run("search_code", map[string]any{"query": "func \\w+", "regex": true, "context_lines": 3}),
			run("search_code", map[string]any{"query": "CANARY", "include": []any{"vault/", "*.hidden.md"}}),
			run("git_status", nil),
			run("git_log", map[string]any{"with_stats": true}),
			run("git_log", map[string]any{"grep": "first"}),
			run("git_diff", nil),
			run("git_diff", map[string]any{"mode": "staged"}),
			run("git_diff", map[string]any{"mode": "worktree"}),
			run("git_diff", map[string]any{"mode": "range", "base": first, "target": "HEAD"}),
			run("git_diff", map[string]any{"mode": "range", "base": first, "target": "HEAD", "stat_only": true}),
			run("git_show", map[string]any{"rev": first}),
			run("git_show", map[string]any{"rev": "HEAD"}),
			run("git_show", map[string]any{"rev": first, "stat_only": true}),
			run("git_refs", nil),
		)
		if opts.NoRedact {
			// The query is the caller's own text and is echoed, so this
			// answer is judged on what it found, not scanned for canaries.
			hidden := run("git_log", map[string]any{"pickaxe": "CANARY-GONE"})
			if !strings.Contains(hidden, "commits:0") || strings.Contains(hidden, "first: everything") {
				t.Errorf("a pickaxe search confirmed text that exists only in a withheld file:\n%s", clipped(hidden))
			}
			found := run("git_log", map[string]any{"pickaxe": "SERVED-GONE", "with_stats": true})
			if !strings.Contains(found, "first: everything") {
				t.Errorf("a pickaxe search for text in a served file should find its commit:\n%s", clipped(found))
			}
			discovery = append(discovery, found)
		}
		// Calls that name a file, for every file on both sides.
		var targeted []string
		answers := map[string]string{} // "tool path" → result
		ask := func(key, name string, args map[string]any) {
			out := run(name, args)
			targeted = append(targeted, out)
			answers[key] = out
		}
		for _, p := range all {
			ask("read "+p, "read_file", map[string]any{"path": p})
			ask("read@first "+p, "read_file", map[string]any{"path": p, "rev": first})
			ask("outline "+p, "file_outline", map[string]any{"path": p})
			ask("blame "+p, "git_blame", map[string]any{"path": p})
			ask("blame@first "+p, "git_blame", map[string]any{"path": p, "rev": first})
			ask("log "+p, "git_log", map[string]any{"path": p, "with_stats": true})
			ask("diff "+p, "git_diff", map[string]any{"paths": []any{p}})
			ask("range "+p, "git_diff", map[string]any{"mode": "range", "base": first, "target": "HEAD", "paths": []any{p}})
			ask("show "+p, "git_show", map[string]any{"rev": first, "paths": []any{p}})
			ask("search-in "+p, "search_code", map[string]any{"query": "func", "include": []any{p}})
		}
		for i := 0; i < len(all); i += 10 {
			ask(fmt.Sprint("read_files ", i), "read_files", map[string]any{"paths": toAny(all[i:min(i+10, len(all))])})
		}

		// Negative: no canary anywhere, and no withheld name in an answer to
		// a question that did not name it.
		canary := regexp.MustCompile(`CANARY-[A-Z]+`)
		for _, out := range append(append([]string{}, discovery...), targeted...) {
			if m := canary.FindString(out); m != "" {
				t.Errorf("[NoRedact=%v] content of a withheld file crossed the boundary (%s):\n%s", opts.NoRedact, m, clipped(out))
			}
		}
		for _, out := range discovery {
			for p := range withheld {
				base := filepath.Base(p)
				for q := range served { // a served file may share the name
					if filepath.Base(q) == base {
						base = p
					}
				}
				if strings.Contains(out, p) || strings.Contains(out, base) {
					t.Errorf("[NoRedact=%v] the name of withheld %q appears unasked:\n%s", opts.NoRedact, p, clipped(out))
				}
			}
		}
		for p := range withheld {
			for _, key := range []string{"read " + p, "read@first " + p, "blame@first " + p} {
				if out := answers[key]; !strings.Contains(out, "withheld") {
					t.Errorf("[NoRedact=%v] %s must be refused as withheld, got:\n%s", opts.NoRedact, key, clipped(out))
				}
			}
		}

		// Positive: what is allowed does come through, by every route.
		want := func(key string, parts ...string) {
			t.Helper()
			for _, part := range parts {
				if !strings.Contains(answers[key], part) {
					t.Errorf("[NoRedact=%v] %s should contain %q, got:\n%s", opts.NoRedact, key, part, clipped(answers[key]))
				}
			}
		}
		want("read src/app.go", "SERVED-APP", "SERVED-EDIT")
		want("read@first src/app.go", "SERVED-APP")
		want("read public/keep.hidden.md", "SERVED-KEEP")
		want("read src/new.go", "SERVED-NEW")
		want("read@first old/gone.go", "SERVED-GONE")
		want("blame@first old/gone.go", "SERVED-GONE")
		want("show old/gone.go", "SERVED-GONE")
		want("log old/gone.go", "first: everything")
		want("diff src/app.go", "+// SERVED-EDIT")
		want("range old/gone.go", "-// SERVED-GONE")
		want("outline src/app.go", "func Served()")
		want("blame src/app.go", "SERVED-APP")
		want("search-in src/app.go", "func Served()")
		joined := strings.Join(discovery, "\n")
		for _, part := range []string{"src/app.go", "public/keep.hidden.md", "old/gone.go", "SERVED-APP", "SERVED-KEEP", "SERVED-GONE", "SERVED-NEW", "+// SERVED-EDIT"} {
			if !strings.Contains(joined, part) {
				t.Errorf("[NoRedact=%v] %q is allowed and should appear in listings, searches, diffs or history", opts.NoRedact, part)
			}
		}

		// The file git releases disagree about follows the installed git.
		everything := joined + strings.Join(targeted, "\n")
		switch got := strings.Contains(everything, divergentMark); {
		case gitIgnoresDivergent && got:
			t.Errorf("[NoRedact=%v] git ignores %s here, but its content was served", opts.NoRedact, divergent)
		case !gitIgnoresDivergent && !strings.Contains(answers["read "+divergent], divergentMark):
			t.Errorf("[NoRedact=%v] git does not ignore %s here, but it could not be read: %s", opts.NoRedact, divergent, clipped(answers["read "+divergent]))
		}
		r.Close()
	}
}

func toAny(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}

func clipped(s string) string {
	if len(s) > 700 {
		return s[:700] + " …"
	}
	return s
}

func revParse(t *testing.T, dir, rev string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", rev)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// gitSaysIgnored asks the installed git, about a real file in a real
// repository, independently of how the policy asks.
func gitSaysIgnored(t *testing.T, rules, path string) bool {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	write(t, dir, ".git/info/exclude", rules)
	write(t, dir, path, "x\n")
	cmd := exec.Command("git", "-c", "core.ignorecase=false", "check-ignore", "-q", "--no-index", path)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
		return false
	}
	if err != nil {
		t.Fatalf("git check-ignore: %v", err)
	}
	return true
}
