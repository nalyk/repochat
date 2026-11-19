package tools

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/nalyk/repochat/internal/mcp"
	"github.com/nalyk/repochat/internal/repo"
)

// The write boundary, exercised from outside: every write tool, driven with
// hostile arguments, against an owner repository that holds a canary in every
// place a write must never reach: its tracked, untracked, staged, ignored and
// edited files; withheld files of every kind; protected paths; its git
// configuration and hooks; the session worktree's own git directory; a
// directory outside both the repository and repochat's state; and HOME. The
// owner's configuration names hooks, filter drivers, an fsmonitor, a diff
// program and editors that all leave a mark outside if any of them ever runs.
//
// After every call: every canary is byte-identical, no file appeared outside
// the workspace, and the owner's branches, HEAD and git status are as they
// were. After every refused call the workspace and the session branch are
// also as they were (a change set applies completely or not at all). The one
// legitimate finish_session at the end is the only call that changes the
// owner's checkout, and it changes exactly what the session changed.
func TestWriteBoundaryThroughEveryTool(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", home)
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	bin, _ := filepath.EvalSymlinks(t.TempDir())
	stash, _ := filepath.EvalSymlinks(t.TempDir())
	stateParent, _ := filepath.EvalSymlinks(t.TempDir())
	stateDir := filepath.Join(stateParent, "sessions")

	// --- The owner's repository -------------------------------------------
	og := func(args ...string) string { t.Helper(); return wbGit(t, dir, args...) }
	og("init", "-q")
	og("symbolic-ref", "HEAD", "refs/heads/main")
	og("config", "user.name", "Ada")
	og("config", "user.email", "a@example.com")
	write(t, dir, "notes.txt", "alpha\nbeta\ngamma\n")
	write(t, dir, "other.txt", "other\n")
	og("add", "-A")
	og("commit", "-q", "-m", "first")
	first := og("rev-parse", "HEAD")
	// A commit on another branch, never part of a session.
	elsewhere := og("commit-tree", "-p", first, "-m", "elsewhere", first+"^{tree}")
	og("update-ref", "refs/heads/other", elsewhere)

	committed := map[string]string{
		"src/app.go":               "package app\n\nfunc App() {}\n",
		"lib/a.go":                 "package lib\n\nfunc A() {}\n",
		"lib/b.go":                 "package lib\n\nfunc B() {}\n",
		"docs/guide.md":            "# Guide\n",
		"old.txt":                  "to be removed\n",
		"x.evil":                   "CANARY-FILTERED\n",
		"x.evilp":                  "CANARY-FILTERED-PROCESS\n",
		".env":                     "TOKEN=CANARY-ENV\n", // built-in secret rule
		"vault/keys.txt":           "CANARY-VAULT\n",     // committed .repochatignore
		"private/plan.txt":         "CANARY-PRIVATE\n",   // the owner's uncommitted .repochatignore
		"denied/list.txt":          "CANARY-DENIED\n",    // --deny
		".repochatignore":          "vault/\n",           // protected
		".gitattributes":           "*.lock -diff\n",     // protected
		".gitignore":               "build/\n",           // the owner's ignored files live there
		".gitmodules":              "[submodule \"sub\"]\n\tpath = sub\n\turl = ./nowhere\n",
		".github/workflows/ci.yml": "on: push\n",
		".gitlab-ci.yml":           "job: {}\n",
		".husky/pre-commit":        "echo CANARY-HUSKY\n",
		".githooks/pre-push":       "echo CANARY-GITHOOKS\n",
		".circleci/config.yml":     "version: 2\n",
		"Jenkinsfile":              "pipeline {}\n",
	}
	for p, c := range committed {
		write(t, dir, p, c)
	}
	links := map[string]string{ // committed symbolic links → their targets
		"link-outside":      outside,
		"link-outside-file": filepath.Join(outside, "canary.txt"),
		"link-owner":        dir,
		"link-git":          ".git",
		"link-env":          ".env",
		"link-vault":        "vault",
		"link-src":          "src",
		"link-up":           "..",
	}
	for p, target := range links {
		if err := os.Symlink(target, filepath.Join(dir, p)); err != nil {
			t.Fatal(err)
		}
	}
	og("add", "-A", "-f")
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	og("update-index", "--add", "--cacheinfo", "160000,"+first+",sub") // a submodule
	og("commit", "-q", "-m", "second")

	// Programs the repository configures, each leaving a mark outside if run.
	mark := func(name string, passThrough bool) string {
		script := "#!/bin/sh\necho ran >> '" + filepath.Join(outside, "RAN-"+name) + "'\n"
		if passThrough {
			script += "cat\n"
		}
		p := filepath.Join(bin, name)
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, hook := range []string{"pre-commit", "prepare-commit-msg", "commit-msg", "post-commit", "post-checkout",
		"post-merge", "pre-merge-commit", "post-rewrite", "reference-transaction", "pre-auto-gc", "post-index-change", "pre-push"} {
		os.MkdirAll(filepath.Join(dir, ".git", "hooks"), 0o755)
		data, _ := os.ReadFile(mark("hook-"+hook, false))
		if err := os.WriteFile(filepath.Join(dir, ".git", "hooks", hook), data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The long-running process filter has a driver of its own: on a driver
	// that has one, git never starts the clean and smudge commands, and their
	// marks could not appear whatever repochat did.
	write(t, dir, ".git/info/attributes", "*.evil filter=evil\n*.evilp filter=evilproc\n*.go diff=evil\n")
	og("config", "filter.evil.clean", mark("filter-clean", true))
	og("config", "filter.evil.smudge", mark("filter-smudge", true))
	og("config", "filter.evilproc.process", mark("filter-process", false))
	og("config", "diff.evil.command", mark("diff-driver", false))
	og("config", "diff.external", mark("diff-external", false))
	og("config", "core.fsmonitor", mark("fsmonitor", false))
	og("config", "core.editor", mark("editor", false))
	og("config", "sequence.editor", mark("sequence-editor", false))

	// The owner's uncommitted work, on both sides of the boundary.
	write(t, dir, "src/app.go", "package app\n\n// CANARY-OWNER-EDIT\nfunc App() {}\n")
	write(t, dir, "scratch.txt", "CANARY-UNTRACKED\n")
	write(t, dir, "staged.txt", "CANARY-STAGED\n")
	og("add", "staged.txt")
	write(t, dir, "build/out.txt", "CANARY-IGNORED\n")
	write(t, dir, ".repochatignore", "vault/\nprivate/\n")

	// Outside everything: canaries, and a file a workspace file is hard-linked to.
	write(t, outside, "canary.txt", "CANARY-OUTSIDE\n")
	write(t, outside, "lib/a.go", "package lib\n\n// CANARY-OUTSIDE-LIB\nfunc A() {}\n")
	write(t, outside, "hard.txt", "alpha\nbeta\ngamma\n")
	write(t, home, "canary.txt", "CANARY-HOME\n")
	write(t, home, "hard.txt", "alpha\nbeta\ngamma\n")
	write(t, stateParent, "canary.txt", "CANARY-STATE-PARENT\n")

	// --- repochat in write mode -------------------------------------------
	opts := repo.Options{Deny: []string{"denied/"}}
	owner, err := repo.Open(bg, dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	sessions, err := repo.NewSessions(owner, repo.SessionOptions{StateDir: stateDir, Repo: opts})
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.Close()
	tools := map[string]mcp.Tool{}
	for _, tool := range NewWrite(owner, sessions) {
		tools[tool.Name] = tool
	}

	// --- What must not change ---------------------------------------------
	w := &wbWorld{t: t, owner: dir, areas: map[string]string{"outside": outside, "home": home, "bin": bin}, stateParent: stateParent}
	want := w.take()
	for _, m := range []string{"CANARY-OWNER-EDIT", "CANARY-UNTRACKED", "CANARY-STAGED", "CANARY-IGNORED", "CANARY-VAULT", "CANARY-ENV"} {
		if !strings.Contains(fmt.Sprint(want.files), m) {
			t.Fatalf("fixture: canary %s is not in the snapshot", m)
		}
	}
	if !strings.Contains(want.status, "staged.txt") || !strings.Contains(want.status, "scratch.txt") || !strings.Contains(want.status, "build/") {
		t.Fatalf("fixture: unexpected owner status %q", want.status)
	}

	var ws, wtGit string // the open session's workspace and its git directory
	check := func(label string) {
		t.Helper()
		w.compare(label, want, w.take())
		if wtGit != "" {
			if data, err := os.ReadFile(filepath.Join(wtGit, "CANARY")); err != nil || string(data) != "CANARY-WORKTREE-GITDIR\n" {
				t.Errorf("%s: the worktree's git directory canary changed: %q %v", label, data, err)
			}
		}
	}
	run := func(name string, args map[string]any) (string, error) {
		t.Helper()
		text, _, err := call(t, tools, name, args)
		return text, err
	}
	// hostile runs a call that must be refused and must change nothing: not
	// the boundary, not the workspace, not the session branch.
	refusals := 0
	hostile := func(label, name string, args map[string]any) string {
		t.Helper()
		var before map[string]string
		if ws != "" {
			before = wbTree(t, ws, nil)
		}
		refs := w.allRefs()
		text, err := run(name, args)
		if err == nil {
			t.Errorf("%s: %s was not refused: %s", label, name, clipped(text))
		} else {
			refusals++
			// A refusal names paths; it never carries what a canary holds.
			for _, c := range canaryRe.FindAllString(err.Error(), -1) {
				if c != "CANARY-WRITTEN" && c != "CANARY-PARTIAL" {
					t.Errorf("%s: the refusal carries %s:\n%s", label, c, clipped(err.Error()))
				}
			}
		}
		check(label)
		if ws != "" {
			if diff := wbDiff(before, wbTree(t, ws, nil)); diff != "" {
				t.Errorf("%s: a refused %s changed the workspace:\n%s", label, name, diff)
			}
		}
		if now := w.allRefs(); now != refs {
			t.Errorf("%s: a refused %s moved a branch:\n%s\nnow:\n%s", label, name, refs, now)
		}
		if err != nil {
			return err.Error()
		}
		return ""
	}
	// legit runs a call that must succeed; it may change only the workspace
	// and the session branch.
	legit := func(label, name string, args map[string]any) (string, map[string]any) {
		t.Helper()
		text, m, err := call(t, tools, name, args)
		if err != nil {
			t.Fatalf("%s: %s: %v", label, name, err)
		}
		check(label)
		return text, m
	}
	ops := func(list ...map[string]string) map[string]any {
		return map[string]any{"changes": changesJSON(t, list...)}
	}
	noSession := func(when string) {
		t.Helper()
		create := ops(map[string]string{"op": "create", "path": "x.txt", "text": "x\n"})
		for _, c := range []struct {
			name string
			args map[string]any
		}{
			{"propose_changes", create},
			{"apply_changes", create},
			{"apply_changes", map[string]any{"plan": "0123456789abcdef"}},
			{"list_checkpoints", map[string]any{}},
			{"undo_to_checkpoint", map[string]any{"checkpoint": first}},
			{"undo_to_checkpoint", map[string]any{"checkpoint": "--all"}},
			{"update_session", map[string]any{}},
			{"finish_session", map[string]any{}},
			{"finish_session", map[string]any{"squash": true, "message": "--all"}},
			{"abandon_session", map[string]any{"delete_branch": true}},
		} {
			if msg := hostile(when+": "+c.name+" without a session", c.name, c.args); !strings.Contains(msg, "start_session") && !strings.Contains(msg, "checkpoint") && !strings.Contains(msg, "propose the change set again") {
				t.Errorf("%s: %s without a session should say how to open one: %s", when, c.name, msg)
			}
		}
		if text, err := run("session_status", map[string]any{}); err != nil || !strings.Contains(text, "No session is open") {
			t.Errorf("%s: session_status without a session: %q %v", when, text, err)
		}
		check(when + ": session_status")
		if _, err := os.Stat(filepath.Join(dir, "x.txt")); err == nil {
			t.Fatalf("%s: a write without a session reached the owner's checkout", when)
		}
	}
	open := func(label string, args map[string]any) {
		t.Helper()
		legit(label, "start_session", args)
		info, err := sessions.Current(bg)
		if err != nil {
			t.Fatal(err)
		}
		ws = info.Workspace
		if within(dir, ws) || within(outside, ws) || !within(stateDir, ws) {
			t.Fatalf("the workspace %s is not under the state directory", ws)
		}
		wtGit = wbGit(t, ws, "rev-parse", "--absolute-git-dir")
		write(t, wtGit, "CANARY", "CANARY-WORKTREE-GITDIR\n")
		// Withheld files of every kind are not in the workspace at all.
		for _, p := range []string{".env", "vault/keys.txt", "private/plan.txt", "denied/list.txt"} {
			if _, err := os.Lstat(filepath.Join(ws, p)); !os.IsNotExist(err) {
				t.Errorf("%s: withheld %s is on disk in the workspace (%v)", label, p, err)
			}
		}
		// The owner's uncommitted, untracked and ignored files are not either.
		for _, p := range []string{"scratch.txt", "staged.txt", "build/out.txt"} {
			if _, err := os.Lstat(filepath.Join(ws, p)); !os.IsNotExist(err) {
				t.Errorf("%s: the owner's %s is in the workspace (%v)", label, p, err)
			}
		}
		if data, _ := os.ReadFile(filepath.Join(ws, "src/app.go")); strings.Contains(string(data), "CANARY-OWNER-EDIT") {
			t.Errorf("%s: the owner's uncommitted edit is in the workspace", label)
		}
	}
	closed := func() { ws, wtGit = "", "" }

	// --- 1. No session: every write tool refuses --------------------------
	noSession("before any session")

	// --- 2. A session, and every path a write must not reach --------------
	open("start session 1", map[string]any{})
	text, _ := legit("read notes.txt", "read_file", map[string]any{"path": "notes.txt"})
	beta := anchorFor(t, text, "beta")
	text, m := legit("read lib/a.go", "read_file", map[string]any{"path": "lib/a.go"})
	funcA := anchorFor(t, text, "func A() {}")
	libTag, _ := m["file_tag"].(string)
	text, m = legit("read notes.txt", "read_file", map[string]any{"path": "notes.txt"})
	notesTag, _ := m["file_tag"].(string)
	_, m = legit("read src/app.go", "read_file", map[string]any{"path": "src/app.go"})
	appTag, _ := m["file_tag"].(string)
	if libTag == "" || notesTag == "" || appTag == "" {
		t.Fatalf("read_file gave no file_tag")
	}
	// refusedForItsPath checks that a refusal came from the path alone: the
	// file was never opened to compare an anchor or a file tag.
	refusedForItsPath := func(label, msg string) {
		t.Helper()
		for _, read := range []string{"does not match the current file", "but the file has", "does not match the file as it is now"} {
			if strings.Contains(msg, read) {
				t.Errorf("%s: the path passed every check and the file was read:\n%s", label, clipped(msg))
			}
		}
	}
	// removes drives removals that carry the real tag of the file a link
	// leads to: nothing but the path checks stands between them and the file.
	removes := func(group string, list map[string]string) {
		t.Helper()
		for p, tag := range list {
			label := fmt.Sprintf("%s %q remove with the file's real tag", group, p)
			refusedForItsPath(label, hostile(label, "apply_changes", ops(map[string]string{"op": "remove", "path": p, "file_tag": tag})))
		}
	}

	relTo := func(target string) string { r, _ := filepath.Rel(ws, target); return r }
	paths := []string{
		// Out of the tree, by every spelling.
		"", ".", "..", "../x.txt", "../../x.txt", "src/../../x.txt", "src/../.git/config", "./notes.txt", "src/./app.go",
		"src//app.go", "src/app.go/", "src\\app.go", " notes.txt", "notes.txt ", "notes.txt\x00.go", "~/canary.txt",
		relTo(filepath.Join(outside, "canary.txt")), relTo(filepath.Join(outside, "new.txt")),
		relTo(filepath.Join(dir, "src/app.go")), relTo(filepath.Join(dir, "scratch.txt")), relTo(filepath.Join(dir, ".git/config")),
		relTo(filepath.Join(wtGit, "CANARY")), relTo(filepath.Join(home, "canary.txt")),
		filepath.Join(outside, "canary.txt"), filepath.Join(outside, "new.txt"), filepath.Join(dir, "src/app.go"),
		filepath.Join(dir, "new.txt"), filepath.Join(dir, ".git/hooks/pre-commit"), filepath.Join(wtGit, "CANARY"),
		filepath.Join(ws, "notes.txt"), filepath.Join(home, "canary.txt"), "/etc/x.txt",
		// git's own files, in every letter case and with code points HFS+ ignores.
		".git", ".git/config", ".git/HEAD", ".git/index", ".git/hooks/pre-commit", ".git/info/attributes",
		".GIT/config", ".Git/hooks/pre-commit", ".gIT/HEAD", ".git\u200c/config", "\ufeff.git/config", ".gi\u200dt/hooks/post-merge",
		"src/.git/config", "src/.GIT", "sub/.git/config",
		// Protected paths, existing and new, in other cases.
		".gitmodules", ".GITMODULES", "src/.gitmodules", ".gitattributes", ".GitAttributes", "src/.gitattributes",
		".repochatignore", ".RepochatIgnore", "src/.repochatignore", "vault/.repochatignore",
		".github/workflows/ci.yml", ".github/workflows/new.yml", ".GITHUB/Workflows/ci.yml", ".gitlab-ci.yml", ".GITLAB-CI.YML",
		".husky/pre-commit", ".Husky/new", ".githooks/pre-push", ".circleci/config.yml", "Jenkinsfile", "jenkinsfile",
		// Withheld: built-in secret rule, committed and uncommitted .repochatignore, --deny.
		".env", ".ENV", ".env.local", "src/.env", "vault/keys.txt", "vault/new.txt", "private/plan.txt", "private/new.txt",
		"denied/list.txt", "denied/new.txt", ".Env", "VAULT/keys.txt", "Vault/new.txt", "PRIVATE/plan.txt", "Denied/list.txt",
		// Through the committed symbolic links, and the links themselves.
		"link-outside", "link-outside/canary.txt", "link-outside/new.txt", "link-outside-file", "link-owner/src/app.go",
		"link-owner/new.txt", "link-owner/.git/config", "link-git", "link-git/config", "link-git/hooks/pre-commit", "link-env",
		"link-vault/keys.txt", "link-vault/new.txt", "link-src", "link-src/app.go", "link-src/new.go", "link-up/x.txt",
		// The submodule, and a file whose attributes name a filter.
		"sub", "sub/x.txt", "x.evil", "x.evilp",
	}
	// matrix drives one path through every operation that names a path, as
	// the path and as the target of a move, alone and next to a valid edit.
	matrix := func(group string, list []string, valid bool) {
		t.Helper()
		for _, p := range list {
			label := fmt.Sprintf("%s %q", group, p)
			for _, c := range []struct {
				what string
				ops  []map[string]string
			}{
				{"create", []map[string]string{{"op": "create", "path": p, "text": "CANARY-WRITTEN\n"}}},
				{"replace", []map[string]string{{"op": "replace", "path": p, "from": "1:aaa", "text": "CANARY-WRITTEN"}}},
				{"insert", []map[string]string{{"op": "insert", "path": p, "at": "start", "text": "CANARY-WRITTEN"}}},
				{"delete", []map[string]string{{"op": "delete", "path": p, "from": "1:aaa"}}},
				{"remove", []map[string]string{{"op": "remove", "path": p, "file_tag": "aaaaaaaa"}}},
				{"move from", []map[string]string{{"op": "move", "path": p, "to_path": "moved-here.txt"}}},
				{"move onto", []map[string]string{{"op": "move", "path": "notes.txt", "to_path": p}}},
				{"after a valid edit", []map[string]string{
					{"op": "replace", "path": "notes.txt", "from": beta, "text": "CANARY-PARTIAL"},
					{"op": "create", "path": p, "text": "CANARY-WRITTEN\n"},
				}},
			} {
				if valid && c.what != "after a valid edit" {
					continue
				}
				refusedForItsPath(label+" "+c.what, hostile(label+" "+c.what, "apply_changes", ops(c.ops...)))
				if c.what == "create" || c.what == "move onto" {
					hostile(label+" "+c.what+" (propose)", "propose_changes", ops(c.ops...))
				}
			}
		}
	}
	matrix("path", paths, false)
	// outside/hard.txt and the owner's notes.txt hold what the workspace's
	// notes.txt holds, and the owner's lib/a.go what the workspace's does: the
	// tags read from the workspace are theirs too.
	removes("committed link", map[string]string{
		"link-src/app.go":                             appTag,
		"link-owner/notes.txt":                        notesTag,
		"link-owner/lib/a.go":                         libTag,
		"link-outside/hard.txt":                       notesTag,
		"link-up/" + filepath.Base(ws) + "/notes.txt": notesTag,
	})

	// --- 3. Behind the session's back: links and swaps in the workspace ----
	plants := map[string]string{
		"plant-outside":      outside,
		"plant-outside-file": filepath.Join(outside, "canary.txt"),
		"plant-owner":        dir,
		"plant-owner-git":    filepath.Join(dir, ".git"),
		"plant-wt-git":       wtGit,
		"plant-owner-env":    filepath.Join(dir, ".env"),
		"plant-scratch":      filepath.Join(dir, "scratch.txt"),
		"plant-home":         home,
		"src/plant-up":       "../..",
	}
	var planted []string
	for p, target := range plants {
		if err := os.Symlink(target, filepath.Join(ws, p)); err != nil {
			t.Fatal(err)
		}
		planted = append(planted, p, p+"/canary.txt", p+"/new.txt", p+"/src/app.go", p+"/config", p+"/hooks/pre-commit")
	}
	sort.Strings(planted)
	matrix("planted link", planted, false)
	removes("planted link", map[string]string{
		"plant-outside/hard.txt": notesTag,
		"plant-owner/notes.txt":  notesTag,
		"plant-owner/lib/a.go":   libTag,
		"plant-home/hard.txt":    notesTag,
	})
	for p := range plants {
		os.Remove(filepath.Join(ws, p))
	}

	// A tracked directory, then a tracked file, swapped for links to their
	// counterparts outside, with anchors that are otherwise valid.
	swap := func(rel, target string, args map[string]any) {
		t.Helper()
		full := filepath.Join(ws, rel)
		aside := filepath.Join(stash, strings.ReplaceAll(rel, "/", "_"))
		if err := os.Rename(full, aside); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, full); err != nil {
			t.Fatal(err)
		}
		hostile(fmt.Sprintf("%s swapped for a link to %s", rel, target), "apply_changes", args)
		os.Remove(full)
		if err := os.Rename(aside, full); err != nil {
			t.Fatal(err)
		}
	}
	editA := ops(map[string]string{"op": "replace", "path": "lib/a.go", "from": funcA, "text": "func A() { /* CANARY-WRITTEN */ }"})
	for _, target := range []string{filepath.Join(dir, "lib"), filepath.Join(outside, "lib"), "../link-owner/lib"} {
		swap("lib", target, editA)
	}
	for _, args := range []map[string]any{
		editA,
		ops(map[string]string{"op": "remove", "path": "lib/a.go", "file_tag": libTag}),
		ops(map[string]string{"op": "move", "path": "lib/a.go", "to_path": "lib/z.go"}),
	} {
		swap("lib/a.go", filepath.Join(dir, "lib/a.go"), args)
		swap("lib/a.go", filepath.Join(outside, "lib/a.go"), args)
	}
	editNotes := ops(map[string]string{"op": "replace", "path": "notes.txt", "from": beta, "text": "CANARY-WRITTEN"})
	swap("notes.txt", filepath.Join(outside, "hard.txt"), editNotes)
	swap("notes.txt", filepath.Join(dir, "notes.txt"), ops(map[string]string{"op": "remove", "path": "notes.txt", "file_tag": notesTag}))
	// A link planted where a new file or a move would go.
	if err := os.Symlink(filepath.Join(outside, "canary.txt"), filepath.Join(ws, "landing.txt")); err != nil {
		t.Fatal(err)
	}
	hostile("create over a planted link", "apply_changes", ops(map[string]string{"op": "create", "path": "landing.txt", "text": "CANARY-WRITTEN\n"}))
	hostile("move onto a planted link", "apply_changes", ops(map[string]string{"op": "move", "path": "notes.txt", "to_path": "landing.txt"}))
	os.Remove(filepath.Join(ws, "landing.txt"))

	// --- 4. Hostile checkpoints, and an update with nothing to merge ---------
	undoValues := []string{"--all", "-x", "--hard", "-n", "HEAD", "HEAD~1", "main", "refs/heads/main", "@{-1}", "other",
		first, first[:7], elsewhere, elsewhere[:12], strings.Repeat("0", 40), strings.Repeat("a", 64), "abc", "abcdef1; rm -rf /",
		"abcdef1 --all", "deadbeef\n--all", "..", "../" + first}
	undoAll := func(when string) {
		t.Helper()
		for _, v := range undoValues {
			hostile(fmt.Sprintf("%s undo_to_checkpoint %q", when, v), "undo_to_checkpoint", map[string]any{"checkpoint": v})
		}
	}
	undoAll("no checkpoints:")
	neutral := func(label, name string, args map[string]any) {
		t.Helper()
		before := wbTree(t, ws, nil)
		refs := w.allRefs()
		run(name, args)
		check(label)
		if diff := wbDiff(before, wbTree(t, ws, nil)); diff != "" {
			t.Errorf("%s changed the workspace:\n%s", label, diff)
		}
		if now := w.allRefs(); now != refs {
			t.Errorf("%s moved a branch", label)
		}
	}
	neutral("update_session with nothing to merge", "update_session", map[string]any{})
	hostile("finish_session with nothing to merge", "finish_session", map[string]any{})
	hostile("finish_session squash with nothing to merge", "finish_session", map[string]any{"squash": true, "message": "--all -x"})

	// --- 5. A merge must not overwrite the owner's own files ----------------
	start := wbGit(t, ws, "rev-parse", "HEAD")
	text, _ = legit("read src/app.go", "read_file", map[string]any{"path": "src/app.go"})
	funcApp := anchorFor(t, text, "func App() {}")
	for _, c := range []struct {
		what string
		args map[string]any
	}{
		{"untracked scratch.txt", ops(map[string]string{"op": "create", "path": "scratch.txt", "text": "CANARY-WRITTEN\n"})},
		{"staged staged.txt", ops(map[string]string{"op": "create", "path": "staged.txt", "text": "CANARY-WRITTEN\n"})},
		{"ignored build/out.txt", ops(map[string]string{"op": "create", "path": "build/out.txt", "text": "CANARY-WRITTEN\n"})},
		{"edited src/app.go", ops(map[string]string{"op": "replace", "path": "src/app.go", "from": funcApp, "text": "func App() { /* CANARY-WRITTEN */ }"})},
	} {
		legit("session writes over the owner's "+c.what, "apply_changes", c.args)
		undoAll("with checkpoints:")
		for _, squash := range []bool{false, true} {
			hostile(fmt.Sprintf("finish over the owner's %s (squash=%v)", c.what, squash), "finish_session",
				map[string]any{"squash": squash, "message": "--all -x"})
		}
		legit("undo "+c.what, "undo_to_checkpoint", map[string]any{"checkpoint": start})
	}

	// An edit through a hard link replaces the workspace's file; the file it
	// shared its content with stays as it was.
	os.Remove(filepath.Join(ws, "notes.txt"))
	if err := os.Link(filepath.Join(outside, "hard.txt"), filepath.Join(ws, "notes.txt")); err != nil {
		t.Logf("no hard link across these directories (%v); that case is skipped", err)
		write(t, ws, "notes.txt", "alpha\nbeta\ngamma\n")
	} else {
		legit("edit through a hard link", "apply_changes", editNotes)
		if data, _ := os.ReadFile(filepath.Join(ws, "notes.txt")); !strings.Contains(string(data), "CANARY-WRITTEN") {
			t.Errorf("the edit through a hard link did not reach the workspace: %q", data)
		}
	}

	// --- 6. Closing a session whose workspace holds links out ---------------
	for p, target := range plants {
		os.Symlink(target, filepath.Join(ws, p))
	}
	os.MkdirAll(filepath.Join(ws, "deep/er"), 0o755)
	os.Symlink(outside, filepath.Join(ws, "deep/er/out"))
	os.Symlink(dir, filepath.Join(ws, "deep/owner"))
	closed()
	open("start_session fresh over a workspace with links out", map[string]any{"fresh": true})
	// The workspace itself replaced by a link to the outside directory.
	if err := os.Rename(ws, ws+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, ws); err != nil {
		t.Fatal(err)
	}
	linked := ws
	ws, wtGit = "", ""
	hostile("apply while the workspace is a link out", "apply_changes", editA)
	// Nothing is removed through a workspace path that is a link: abandon
	// refuses, twice, and the session stays recorded.
	for _, n := range []string{"", " again"} {
		if msg := hostile("abandon a session whose workspace is a link out"+n, "abandon_session", map[string]any{"delete_branch": true}); !strings.Contains(msg, "nothing was removed") || !strings.Contains(msg, "start_session with fresh=true") {
			t.Errorf("abandon through a link%s should say that nothing was removed and how to go on: %s", n, clipped(msg))
		}
	}
	if _, err := sessions.Current(bg); err == nil || err == repo.ErrNoSession {
		t.Errorf("the session whose workspace could not be removed should still be recorded, as broken: %v", err)
	}
	if fi, err := os.Lstat(linked); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link at the workspace path was touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(linked+".moved", "notes.txt")); err != nil {
		t.Errorf("the moved workspace was removed through the refusal: %v", err)
	}
	// A fresh start leaves it all where it is and opens another session,
	// which is abandoned as usual.
	open("start_session fresh past a workspace that is a link out", map[string]any{"fresh": true})
	closed() // its worktree's git directory goes with it
	legit("abandon the fresh session", "abandon_session", map[string]any{"delete_branch": true})
	if fi, err := os.Lstat(linked); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link at the old workspace path was touched by the fresh start: %v", err)
	}
	noSession("after abandon")

	// --- 7. The positive control: a legitimate multi-file change lands ------
	open("start session 2", map[string]any{})
	text, _ = legit("read lib/a.go", "read_file", map[string]any{"path": "lib/a.go"})
	funcA = anchorFor(t, text, "func A() {}")
	text, _ = legit("read docs/guide.md", "read_file", map[string]any{"path": "docs/guide.md"})
	guide := anchorFor(t, text, "# Guide")
	_, m = legit("read old.txt", "read_file", map[string]any{"path": "old.txt"})
	oldTag, _ := m["file_tag"].(string)
	legit("a multi-file change", "apply_changes", map[string]any{"message": "Control: a change in five files", "changes": changesJSON(t,
		map[string]string{"op": "replace", "path": "lib/a.go", "from": funcA, "text": "func A() int { return 1 }"},
		map[string]string{"op": "insert", "path": "docs/guide.md", "at": guide, "where": "after", "text": "\nMore."},
		map[string]string{"op": "create", "path": "docs/new.md", "text": "# New\n"},
		map[string]string{"op": "move", "path": "lib/b.go", "to_path": "lib/c.go"},
		map[string]string{"op": "remove", "path": "old.txt", "file_tag": oldTag},
	)})

	// The owner's branch moves, with a withheld file among its changes.
	write(t, dir, "other.txt", "other, moved on\n")
	write(t, dir, ".env.production", "TOKEN=CANARY-ENV-PRODUCTION\n")
	og("add", "other.txt", ".env.production")
	og("commit", "-q", "-m", "the owner moves on", "--", "other.txt", ".env.production")
	want = w.take()
	hostile("finish while the owner's branch has moved", "finish_session", map[string]any{})
	legit("update_session", "update_session", map[string]any{})
	if _, err := os.Lstat(filepath.Join(ws, ".env.production")); !os.IsNotExist(err) {
		t.Errorf("a withheld file merged in from the owner's branch is in the workspace (%v)", err)
	}
	undoAll("after an update:")

	// The one call that may change the owner's checkout.
	sessionHead := wbGit(t, ws, "rev-parse", "HEAD")
	if _, _, err := call(t, tools, "finish_session", map[string]any{}); err != nil {
		t.Fatalf("finish_session: %v", err)
	}
	closed()
	got := w.take()
	if tip := wbGit(t, dir, "rev-parse", "refs/heads/main"); tip != sessionHead {
		t.Errorf("after finish, main is at %s, want the session's head %s", tip, sessionHead)
	}
	// git writes the files it checks out with the permissions the umask leaves.
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(probe)
	perm := fmt.Sprintf("F%o:", fi.Mode().Perm())
	expect := want.clone()
	expect.files["owner/lib/a.go"] = perm + "package lib\n\nfunc A() int { return 1 }\n"
	expect.files["owner/docs/guide.md"] = perm + "# Guide\n\nMore.\n"
	expect.files["owner/docs/new.md"] = perm + "# New\n"
	expect.files["owner/lib/c.go"] = perm + "package lib\n\nfunc B() {}\n"
	delete(expect.files, "owner/lib/b.go")
	delete(expect.files, "owner/old.txt")
	expect.refs, expect.status = got.refs, got.status // the tip moved; compared above and below
	w.compare("finish_session", expect, got)
	if got.status != want.status {
		t.Errorf("after finish, the owner's uncommitted work differs:\nbefore %q\nafter  %q", want.status, got.status)
	}
	want = got
	noSession("after finish")
	t.Logf("%d hostile calls refused", refusals)

	// The control for the marks: the configured programs do run when git is
	// not told otherwise, so their absence above means something.
	cmd := exec.Command("git", "commit", "-q", "--allow-empty", "-m", "armed")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	for _, m := range []string{"RAN-hook-pre-commit", "RAN-hook-post-commit"} {
		if _, err := os.Stat(filepath.Join(outside, m)); err != nil {
			t.Errorf("the control: %s is missing after a plain git commit, so the marks prove nothing", m)
		}
	}
	// The same for the three filter programs: a plain git runs the clean
	// command on add, the smudge command on checkout, and starts the process
	// filter for the file whose driver has one.
	plain := func(args ...string) {
		// Without the configured fsmonitor: its mark script reports nothing,
		// which an older git reads as "no file changed" and then filters none.
		cmd := exec.Command("git", append([]string{"-c", "core.fsmonitor=false"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		t.Logf("control: git %s: %v %s", strings.Join(args, " "), err, clipped(string(out)))
	}
	write(t, dir, "x.evil", "changed\n")
	plain("add", "x.evil")
	os.Remove(filepath.Join(dir, "x.evil"))
	plain("checkout", "--", "x.evil")
	write(t, dir, "x.evilp", "changed\n")
	plain("add", "x.evilp") // the mark's script speaks no filter protocol: git reports it and goes on
	for _, m := range []string{"RAN-filter-clean", "RAN-filter-smudge", "RAN-filter-process"} {
		if _, err := os.Stat(filepath.Join(outside, m)); err != nil {
			t.Errorf("the control: %s is missing after plain git ran over a filtered file, so that mark proves nothing", m)
		}
	}
}

var canaryRe = regexp.MustCompile(`CANARY-[A-Z-]*[A-Z]`)

// within reports whether p is dir or lies beneath it.
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// wbGit runs git with every program the fixture configures switched off, so
// that the test's own git commands leave no mark either.
func wbGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	safe := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "filter.evil.clean=",
		"-c", "filter.evil.smudge=", "-c", "filter.evil.process=", "-c", "filter.evil.required=false", "-c", "filter.evilproc.process=", "-c", "diff.external=",
		"-c", "core.editor=true", "-c", "sequence.editor=true"}
	cmd := exec.Command("git", append(safe, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=a@example.com",
		"GIT_COMMITTER_NAME=Ada", "GIT_COMMITTER_EMAIL=a@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// wbTree describes every entry under root, without following links: kind,
// permission bits and content. skip names top-level entries left out.
func wbTree(t *testing.T, root string, skip map[string]bool) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		if skip[strings.SplitN(rel, string(filepath.Separator), 2)[0]] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			out[rel] = "L:" + target
		case fi.IsDir():
			out[rel] = fmt.Sprintf("D%o", fi.Mode().Perm())
		default:
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[rel] = fmt.Sprintf("F%o:%s", fi.Mode().Perm(), data)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walking %s: %v", root, err)
	}
	return out
}

func wbDiff(want, got map[string]string) string {
	var lines []string
	for p, v := range want {
		if g, ok := got[p]; !ok {
			lines = append(lines, "  gone:    "+p)
		} else if g != v {
			lines = append(lines, fmt.Sprintf("  changed: %s: %q → %q", p, clipped(v), clipped(g)))
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			lines = append(lines, "  new:     "+p+" "+clipped(got[p]))
		}
	}
	sort.Strings(lines)
	if len(lines) > 30 {
		lines = append(lines[:30], fmt.Sprintf("  … and %d more", len(lines)-30))
	}
	return strings.Join(lines, "\n")
}

// wbWorld is everything a write tool must leave alone.
type wbWorld struct {
	t           *testing.T
	owner       string
	areas       map[string]string // name → directory watched whole
	stateParent string            // watched except for the state directory itself
}

type wbState struct {
	files                 map[string]string
	refs, status, headRef string
}

func (s wbState) clone() wbState {
	c := s
	c.files = map[string]string{}
	for k, v := range s.files {
		c.files[k] = v
	}
	return c
}

func (w *wbWorld) take() wbState {
	t := w.t
	t.Helper()
	files := map[string]string{}
	add := func(area string, tree map[string]string) {
		for p, v := range tree {
			files[area+"/"+filepath.ToSlash(p)] = v
		}
	}
	add("owner", wbTree(t, w.owner, map[string]bool{".git": true}))
	gitDir := filepath.Join(w.owner, ".git")
	for _, p := range []string{"config", "description"} {
		if data, err := os.ReadFile(filepath.Join(gitDir, p)); err == nil {
			files["owner-git/"+p] = string(data)
		}
	}
	for _, sub := range []string{"hooks", "info"} {
		add("owner-git/"+sub, wbTree(t, filepath.Join(gitDir, sub), nil))
	}
	for name, d := range w.areas {
		add(name, wbTree(t, d, nil))
	}
	add("state-parent", wbTree(t, w.stateParent, map[string]bool{"sessions": true}))
	return wbState{
		files:   files,
		refs:    w.refs(false),
		status:  wbGit(t, w.owner, "status", "--porcelain", "--untracked-files=all", "--ignored", "--ignore-submodules=none"),
		headRef: wbGit(t, w.owner, "symbolic-ref", "HEAD"),
	}
}

// refs lists the owner's references; the session branches only when asked.
func (w *wbWorld) refs(sessions bool) string {
	var keep []string
	for _, l := range strings.Split(wbGit(w.t, w.owner, "for-each-ref", "--format=%(refname) %(objectname)"), "\n") {
		if sessions || !strings.HasPrefix(l, "refs/heads/repochat/") {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}

func (w *wbWorld) allRefs() string { return w.refs(true) }

func (w *wbWorld) compare(label string, want, got wbState) {
	t := w.t
	t.Helper()
	if diff := wbDiff(want.files, got.files); diff != "" {
		t.Errorf("%s: something outside the workspace changed:\n%s", label, diff)
	}
	if want.refs != got.refs {
		t.Errorf("%s: the owner's branches changed:\nwant %s\ngot  %s", label, want.refs, got.refs)
	}
	if want.status != got.status {
		t.Errorf("%s: the owner's git status changed:\nwant %q\ngot  %q", label, want.status, got.status)
	}
	if want.headRef != got.headRef {
		t.Errorf("%s: the owner's HEAD changed: %s → %s", label, want.headRef, got.headRef)
	}
}
