package repo

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// sessionFixture is an owner repository with a few committed files and a
// state directory outside it.
type sessionFixture struct {
	dir   string // the owner's checkout
	owner *Repo
	state string

	mu    sync.Mutex
	notes []string
}

func newSessionFixture(t *testing.T, files map[string]string) *sessionFixture {
	t.Helper()
	dir := newDir(t)
	gitCmd(t, dir, "config", "user.name", "Ada")
	gitCmd(t, dir, "config", "user.email", "ada@example.com")
	if files == nil {
		files = map[string]string{"main.go": "package main\n", "notes.txt": "hello\nworld\n"}
	}
	for rel, content := range files {
		write(t, dir, rel, content)
	}
	commit(t, dir, "start")
	state, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &sessionFixture{dir: dir, owner: open(t, dir, Options{}), state: filepath.Join(state, "sessions")}
}

func (f *sessionFixture) notify(msg string) {
	f.mu.Lock()
	f.notes = append(f.notes, msg)
	f.mu.Unlock()
}

func (f *sessionFixture) sessions(t *testing.T, opts Options) *Sessions {
	t.Helper()
	s, err := NewSessions(f.owner, SessionOptions{StateDir: f.state, Repo: opts, Notify: f.notify})
	if err != nil {
		t.Fatalf("NewSessions: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func (f *sessionFixture) start(t *testing.T, opts Options) (*Sessions, *SessionInfo) {
	t.Helper()
	s := f.sessions(t, opts)
	info, err := s.Start(bg, false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return s, info
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func branches(t *testing.T, dir string) string {
	return gitCmd(t, dir, "for-each-ref", "--format=%(refname)", "refs/heads/")
}

func TestStartCreatesWorktreeOnASideBranch(t *testing.T) {
	f := newSessionFixture(t, nil)
	write(t, f.dir, "untracked.txt", "mine\n")
	statusBefore := gitCmd(t, f.dir, "status", "--porcelain")
	head := gitCmd(t, f.dir, "rev-parse", "HEAD")

	s, info := f.start(t, Options{})
	if rel, err := filepath.Rel(f.dir, info.Workspace); err == nil && !strings.HasPrefix(rel, "..") {
		t.Errorf("the workspace %s is inside the owner's checkout", info.Workspace)
	}
	if !strings.HasPrefix(info.Workspace, f.state+string(filepath.Separator)) {
		t.Errorf("the workspace %s is not under the state directory", info.Workspace)
	}
	if !regexp.MustCompile(`^repochat/\d{4}-\d{2}-\d{2}-[0-9a-f]{8}$`).MatchString(info.Branch) {
		t.Errorf("branch %q", info.Branch)
	}
	if !strings.HasSuffix(info.Branch, "-"+info.ID) {
		t.Errorf("branch %q does not end in the id %q", info.Branch, info.ID)
	}
	if info.Base != head || info.Head != head || info.StartBranch != "main" {
		t.Errorf("info = %+v, owner HEAD %s", info, head)
	}
	if info.Created.IsZero() {
		t.Error("no creation time")
	}
	if got := gitCmd(t, f.dir, "status", "--porcelain"); got != statusBefore {
		t.Errorf("owner's status changed:\n%s\nwas:\n%s", got, statusBefore)
	}
	if got := gitCmd(t, f.dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("owner's checkout is on %q", got)
	}
	if got := gitCmd(t, info.Workspace, "rev-parse", "--abbrev-ref", "HEAD"); got != info.Branch {
		t.Errorf("workspace is on %q", got)
	}
	if got := mustRead(t, filepath.Join(info.Workspace, "notes.txt")); got != "hello\nworld\n" {
		t.Errorf("notes.txt in the workspace = %q", got)
	}
	if exists(filepath.Join(info.Workspace, "untracked.txt")) {
		t.Error("the owner's untracked file is in the workspace")
	}
	found := false
	for _, n := range f.notes {
		if strings.Contains(n, "not part of the session") {
			found = true
		}
	}
	if !found {
		t.Errorf("no notice about the owner's uncommitted changes: %q", f.notes)
	}

	st, err := os.Stat(filepath.Join(f.state, f.owner.ID, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("session.json mode %v", st.Mode().Perm())
	}

	ws, err := s.Serve(bg)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Root != info.Workspace {
		t.Errorf("Serve().Root = %s, want %s", ws.Root, info.Workspace)
	}
	if len(ws.Warnings) != 0 {
		t.Errorf("the workspace opened with warnings: %q", ws.Warnings)
	}
	cur, err := s.Current(bg)
	if err != nil || cur.ID != info.ID || cur.Branch != info.Branch {
		t.Errorf("Current = %+v, %v", cur, err)
	}
	// Starting again without fresh resumes the same session.
	again, err := s.Start(bg, false)
	if err != nil || again.ID != info.ID {
		t.Errorf("a second Start gave %+v, %v", again, err)
	}
}

func TestNoSessionServesTheOwner(t *testing.T) {
	f := newSessionFixture(t, nil)
	s := f.sessions(t, Options{})
	if _, err := s.Current(bg); !errors.Is(err, ErrNoSession) {
		t.Errorf("Current without a session: %v", err)
	}
	r, err := s.Serve(bg)
	if err != nil || r != f.owner {
		t.Errorf("Serve without a session = %v, %v", r, err)
	}
}

func TestOwnStateDirDoesNotHideTheWorkspace(t *testing.T) {
	f := newSessionFixture(t, nil)
	s, _ := f.start(t, Options{Own: []string{filepath.Dir(f.state), f.state}})
	ws, err := s.Serve(bg)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := ws.Snapshot(bg)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := snap.Lookup("notes.txt"); !ok || e.Withheld != "" {
		t.Errorf("notes.txt in the workspace: %+v (visible %d)", e, snap.Visible)
	}
	res, err := ws.ReadFile(bg, "notes.txt", 1, 10, "", 1<<20)
	if err != nil || !strings.Contains(res.Text, "world") {
		t.Errorf("read notes.txt: %v", err)
	}
}

// F1: repochat's own directories are named by absolute path, inside the
// owner's checkout. A tracked file under one is withheld in the workspace as
// it is in the owner's repository: not on disk, not readable, not editable.
func TestOwnDirectoriesAreWithheldInTheWorkspace(t *testing.T) {
	f := newSessionFixture(t, map[string]string{
		"cfg/config.toml": "api_key = 1\n",
		"notes.txt":       "hello\n",
	})
	own := Options{Own: []string{filepath.Join(f.dir, "cfg")}}
	s, info := f.start(t, own)
	if exists(filepath.Join(info.Workspace, "cfg")) {
		t.Error("cfg exists in the workspace")
	}
	ws, err := s.Serve(bg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.ReadFile(bg, "cfg/config.toml", 1, 10, "", 1<<20); err == nil || !strings.Contains(err.Error(), ReasonOwnFiles) {
		t.Errorf("read cfg/config.toml in the workspace: %v", err)
	}
	if _, err := ws.ReadFile(bg, "cfg/config.toml", 1, 10, "HEAD", 1<<20); err == nil {
		t.Error("cfg/config.toml readable from history in the workspace")
	}
	for _, c := range []Change{
		{Op: "insert", Path: "cfg/config.toml", At: "end", Text: "x = 1"},
		{Op: "create", Path: "cfg/new.toml", Text: "x = 1"},
		{Op: "move", Path: "notes.txt", ToPath: "cfg/notes.txt"},
	} {
		if _, err := s.Apply(bg, "", []Change{c}, ""); err == nil || !strings.Contains(err.Error(), "withheld") {
			t.Errorf("%s %s: %v", c.Op, c.Path, err)
		}
	}
	if got := gitCmd(t, info.Workspace, "status", "--porcelain"); got != "" {
		t.Errorf("workspace status:\n%s", got)
	}
	if got := mustRead(t, filepath.Join(f.dir, "cfg", "config.toml")); got != "api_key = 1\n" {
		t.Errorf("the owner's cfg/config.toml = %q", got)
	}
	s.Close()

	// A workspace synced without the directory is brought in line by the
	// next run that names it.
	f2 := newSessionFixture(t, map[string]string{"cfg/config.toml": "api_key = 1\n", "notes.txt": "hello\n"})
	s1, info2 := f2.start(t, Options{})
	if !exists(filepath.Join(info2.Workspace, "cfg", "config.toml")) {
		t.Fatal("cfg/config.toml missing without the option")
	}
	s1.Close()
	s2 := f2.sessions(t, Options{Own: []string{filepath.Join(f2.dir, "cfg")}})
	if _, err := s2.Serve(bg); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(info2.Workspace, "cfg")) {
		t.Error("cfg is still in the workspace after a resume that names it")
	}
}

func TestWithheldTrackedFilesAreAbsentFromTheWorkspace(t *testing.T) {
	f := newSessionFixture(t, map[string]string{
		".env":            "TOKEN=x\n",
		".repochatignore": "secret/\n",
		"secret/x.txt":    "classified\n",
		"later.txt":       "soon hidden\n",
		"open.txt":        "fine\n",
	})
	// Only the owner's uncommitted rules withhold later.txt.
	write(t, f.dir, ".repochatignore", "secret/\nlater.txt\n")
	s, info := f.start(t, Options{})
	for _, p := range []string{".env", "secret/x.txt", "later.txt", "secret"} {
		if exists(filepath.Join(info.Workspace, p)) {
			t.Errorf("%s exists in the workspace", p)
		}
	}
	if !exists(filepath.Join(info.Workspace, "open.txt")) {
		t.Error("open.txt is missing from the workspace")
	}
	if got := gitCmd(t, info.Workspace, "status", "--porcelain"); got != "" {
		t.Errorf("workspace status:\n%s", got)
	}
	listed := gitCmd(t, info.Workspace, "ls-files")
	mustContain(t, listed, ".env", "secret/x.txt", "later.txt", "open.txt")

	ws, err := s.Serve(bg)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".env", "secret/x.txt", "later.txt"} {
		if _, err := ws.ReadFile(bg, p, 1, 10, "", 1<<20); err == nil || !strings.Contains(err.Error(), "withheld") {
			t.Errorf("read %s: %v", p, err)
		}
	}
}

// A .repochatignore that withholds itself is not in the workspace either,
// and its rules still apply there.
func TestWithheldIgnoreFileStillAppliesInTheWorkspace(t *testing.T) {
	f := newSessionFixture(t, map[string]string{
		".repochatignore": ".repochatignore\nsecret/\n",
		"secret/x.txt":    "classified\n",
		"open.txt":        "fine\n",
	})
	s, info := f.start(t, Options{})
	for _, p := range []string{".repochatignore", "secret/x.txt"} {
		if exists(filepath.Join(info.Workspace, p)) {
			t.Errorf("%s exists in the workspace", p)
		}
	}
	// The owner's own copy is withheld there too, and is the second source;
	// take it away so only the workspace's rules remain.
	if err := os.Remove(filepath.Join(f.dir, ".repochatignore")); err != nil {
		t.Fatal(err)
	}
	ws, err := s.Serve(bg)
	if err != nil {
		t.Fatal(err)
	}
	fresh(ws)
	snap, err := ws.Snapshot(bg)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := snap.Lookup("secret/x.txt"); e.Withheld != ReasonIgnoreFile {
		t.Errorf("secret/x.txt in the workspace: %+v", e)
	}
	if _, err := ws.ReadFile(bg, "secret/x.txt", 1, 10, "HEAD", 1<<20); err == nil {
		t.Error("secret/x.txt readable from history in the workspace")
	}
}

func TestPathsWithPatternCharactersAreWithheldExactly(t *testing.T) {
	withheld := []string{"a*b.key", "!x.pem", " lead.key", "t#.key", "b[1].key", "id_rsa  ", `back\slash.key`, "dir?/q.key"}
	served := []string{"ab.txt", "x.txt", "id_rsa.pub", "a.key.txt", "dirx/q.txt"}
	files := map[string]string{}
	for _, p := range append(append([]string{}, withheld...), served...) {
		files[p] = "x\n"
	}
	f := newSessionFixture(t, files)
	_, info := f.start(t, Options{})
	for _, p := range withheld {
		if exists(filepath.Join(info.Workspace, p)) {
			t.Errorf("withheld %q exists in the workspace", p)
		}
	}
	for _, p := range served {
		if !exists(filepath.Join(info.Workspace, p)) {
			t.Errorf("served %q is missing from the workspace", p)
		}
	}
	if got := gitCmd(t, info.Workspace, "status", "--porcelain"); got != "" {
		t.Errorf("workspace status:\n%s", got)
	}
}

func TestWithheldNameWithNewlineFailsClosed(t *testing.T) {
	f := newSessionFixture(t, map[string]string{"a\nb.key": "x\n", "ok.txt": "x\n"})
	branchesBefore := branches(t, f.dir)
	s := f.sessions(t, Options{})
	if _, err := s.Start(bg, false); err == nil {
		t.Fatal("a withheld name with a newline was accepted")
	}
	if got := branches(t, f.dir); got != branchesBefore {
		t.Errorf("branches after a failed start:\n%s", got)
	}
	if got := gitCmd(t, f.dir, "worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
		t.Errorf("a worktree was left behind:\n%s", got)
	}
	if _, err := s.Current(bg); !errors.Is(err, ErrNoSession) {
		t.Errorf("a session was recorded: %v", err)
	}
}

func TestResumeKeepsBranchAnchorsAndCheckpoints(t *testing.T) {
	f := newSessionFixture(t, nil)
	s1, info := f.start(t, Options{})
	ws1, err := s1.Serve(bg)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws1.Opts.AnchorKey) != 32 {
		t.Fatalf("anchor key of %d bytes", len(ws1.Opts.AnchorKey))
	}
	read1, err := ws1.ReadFile(bg, "notes.txt", 1, 10, "", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^\s*1:[^|]+\| hello$`).MatchString(read1.Text) {
		t.Errorf("no anchors in:\n%s", read1.Text)
	}
	s1.Close()

	s2 := f.sessions(t, Options{})
	cur, err := s2.Current(bg)
	if err != nil {
		t.Fatal(err)
	}
	if cur.ID != info.ID || cur.Branch != info.Branch || cur.Base != info.Base || cur.Head != info.Head || cur.Workspace != info.Workspace {
		t.Errorf("resumed %+v, started %+v", cur, info)
	}
	ws2, err := s2.Serve(bg)
	if err != nil {
		t.Fatal(err)
	}
	if string(ws2.Opts.AnchorKey) != string(ws1.Opts.AnchorKey) {
		t.Error("the anchor key changed on resume")
	}
	read2, err := ws2.ReadFile(bg, "notes.txt", 1, 10, "", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if read2.Text != read1.Text {
		t.Errorf("anchors differ after resume:\n%s\nvs\n%s", read2.Text, read1.Text)
	}
}

func TestResumeDropsFilesThatBecameWithheld(t *testing.T) {
	f := newSessionFixture(t, nil)
	s1, info := f.start(t, Options{})
	s1.Close()
	if !exists(filepath.Join(info.Workspace, "notes.txt")) {
		t.Fatal("notes.txt missing before the rules changed")
	}
	s2 := f.sessions(t, Options{Deny: []string{"notes.txt"}})
	ws, err := s2.Serve(bg)
	if err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(info.Workspace, "notes.txt")) {
		t.Error("notes.txt is still in the workspace")
	}
	if _, err := ws.ReadFile(bg, "notes.txt", 1, 10, "", 1<<20); err == nil {
		t.Error("notes.txt is readable")
	}
	if got := gitCmd(t, info.Workspace, "status", "--porcelain"); got != "" {
		t.Errorf("workspace status:\n%s", got)
	}
	// And back again once the rule is gone.
	s2.Close()
	s3 := f.sessions(t, Options{})
	if _, err := s3.Serve(bg); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, filepath.Join(info.Workspace, "notes.txt")); got != "hello\nworld\n" {
		t.Errorf("notes.txt after the rule went: %q", got)
	}
}

func TestResumeRestoresAnInterruptedApply(t *testing.T) {
	f := newSessionFixture(t, nil)
	s1, info := f.start(t, Options{})
	s1.Close()
	write(t, info.Workspace, "notes.txt", "half written\n")
	write(t, info.Workspace, "stray/new.txt", "left over\n")
	write(t, info.Workspace, "staged.txt", "staged\n")
	gitCmd(t, info.Workspace, "add", "staged.txt")
	if err := os.Remove(filepath.Join(info.Workspace, "main.go")); err != nil {
		t.Fatal(err)
	}
	f.notes = nil

	s2 := f.sessions(t, Options{})
	if _, err := s2.Serve(bg); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, filepath.Join(info.Workspace, "notes.txt")); got != "hello\nworld\n" {
		t.Errorf("notes.txt = %q", got)
	}
	if got := mustRead(t, filepath.Join(info.Workspace, "main.go")); got != "package main\n" {
		t.Errorf("main.go = %q", got)
	}
	for _, p := range []string{"stray/new.txt", "stray", "staged.txt"} {
		if exists(filepath.Join(info.Workspace, p)) {
			t.Errorf("%s survived", p)
		}
	}
	if got := gitCmd(t, info.Workspace, "status", "--porcelain"); got != "" {
		t.Errorf("workspace status:\n%s", got)
	}
	if len(f.notes) == 0 {
		t.Error("no notice about the restored workspace")
	}
}

func TestResumeWithMissingWorkspaceOrBranchSaysSo(t *testing.T) {
	t.Run("workspace", func(t *testing.T) {
		f := newSessionFixture(t, nil)
		s1, info := f.start(t, Options{})
		s1.Close()
		if err := os.RemoveAll(info.Workspace); err != nil {
			t.Fatal(err)
		}
		branchesBefore := branches(t, f.dir)
		s2 := f.sessions(t, Options{})
		for _, call := range []func() error{
			func() error { _, err := s2.Serve(bg); return err },
			func() error { _, err := s2.Current(bg); return err },
			func() error { _, err := s2.Start(bg, false); return err },
		} {
			if err := call(); err == nil || !strings.Contains(err.Error(), "--new-session") {
				t.Errorf("error %v does not name --new-session", err)
			}
		}
		if exists(info.Workspace) {
			t.Error("the workspace was recreated")
		}
		if got := branches(t, f.dir); got != branchesBefore {
			t.Errorf("branches changed:\n%s", got)
		}
	})
	t.Run("branch", func(t *testing.T) {
		f := newSessionFixture(t, nil)
		s1, info := f.start(t, Options{})
		s1.Close()
		gitCmd(t, f.dir, "update-ref", "-d", "refs/heads/"+info.Branch)
		worktreesBefore := gitCmd(t, f.dir, "worktree", "list", "--porcelain")
		s2 := f.sessions(t, Options{})
		if _, err := s2.Serve(bg); err == nil || !strings.Contains(err.Error(), "--new-session") {
			t.Errorf("error %v does not name --new-session", err)
		}
		if strings.Contains(branches(t, f.dir), info.Branch) {
			t.Error("the branch was recreated")
		}
		if got := gitCmd(t, f.dir, "worktree", "list", "--porcelain"); got != worktreesBefore {
			t.Errorf("worktrees changed:\n%s", got)
		}
	})
}

// A workspace deleted while this process has the session open is noticed on
// the next call, with the same advice as after a restart, instead of being
// served or written through the handle that is still open.
func TestWorkspaceRemovedWhileOpenSaysSo(t *testing.T) {
	f := newSessionFixture(t, nil)
	s, info := f.start(t, Options{})
	if _, err := s.Serve(bg); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(info.Workspace); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"Serve":  func() error { _, err := s.Serve(bg); return err },
		"Status": func() error { _, err := s.Status(bg); return err },
		"Apply": func() error {
			_, err := s.Apply(bg, "", []Change{{Op: "create", Path: "new.txt", Text: "x"}}, "add")
			return err
		},
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "workspace is missing") || !strings.Contains(err.Error(), "--new-session") {
			t.Errorf("%s: error %v does not say the workspace is missing and name --new-session", name, err)
		}
	}
	if exists(info.Workspace) {
		t.Error("the workspace was recreated")
	}
}

func TestFreshStartLeavesTheOldBranch(t *testing.T) {
	f := newSessionFixture(t, nil)
	s, old := f.start(t, Options{})
	info, err := s.Start(bg, true)
	if err != nil {
		t.Fatal(err)
	}
	if info.ID == old.ID || info.Branch == old.Branch || info.Workspace == old.Workspace {
		t.Fatalf("a fresh start resumed: %+v", info)
	}
	if !strings.Contains(branches(t, f.dir), "refs/heads/"+old.Branch) {
		t.Error("the old branch is gone")
	}
	if exists(old.Workspace) {
		t.Error("the old workspace is still there")
	}
	cur, err := s.Current(bg)
	if err != nil || cur.ID != info.ID {
		t.Errorf("Current = %+v, %v", cur, err)
	}
	ws, err := s.Serve(bg)
	if err != nil || ws.Root != info.Workspace {
		t.Errorf("Serve = %v, %v", ws, err)
	}
}

func TestStartRefusesDetachedHead(t *testing.T) {
	f := newSessionFixture(t, nil)
	gitCmd(t, f.dir, "checkout", "-q", "--detach")
	s := f.sessions(t, Options{})
	if _, err := s.Start(bg, false); err == nil || !strings.Contains(err.Error(), "branch") {
		t.Errorf("detached HEAD: %v", err)
	}

	// Nor without any commit.
	dir := newDir(t)
	owner := open(t, dir, Options{})
	s2, err := NewSessions(owner, SessionOptions{StateDir: filepath.Join(t.TempDir(), "s")})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.Start(bg, false); err == nil {
		t.Error("a repository without commits got a session")
	}
}

func TestNoRepositoryHookRunsOnStart(t *testing.T) {
	f := newSessionFixture(t, map[string]string{".gitattributes": "* filter=evil\n", "a.txt": "a\n"})
	marker := filepath.Join(t.TempDir(), "marker")
	script := "#!/bin/sh\ntouch " + marker + "\ncat\n"
	hooks := filepath.Join(f.dir, ".git", "hooks")
	for _, h := range []string{"post-checkout", "reference-transaction", "post-index-change"} {
		if err := os.WriteFile(filepath.Join(hooks, h), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prog := filepath.Join(t.TempDir(), "filter.sh")
	if err := os.WriteFile(prog, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, f.dir, "config", "filter.evil.smudge", prog)
	gitCmd(t, f.dir, "config", "filter.evil.clean", prog)
	gitCmd(t, f.dir, "config", "filter.evil.required", "true")
	gitCmd(t, f.dir, "config", "core.hooksPath", hooks)
	os.Remove(marker)

	s, info := f.start(t, Options{})
	if _, err := s.Serve(bg); err != nil {
		t.Fatal(err)
	}
	if exists(marker) {
		t.Fatal("a repository-configured program ran")
	}
	if got := mustRead(t, filepath.Join(info.Workspace, "a.txt")); got != "a\n" {
		t.Errorf("a.txt = %q", got)
	}

	// Resuming a dirtied workspace restores it (status, read-tree, clean)
	// with the same programs configured: still none of them runs.
	s.Close()
	write(t, info.Workspace, "a.txt", "dirty\n")
	write(t, info.Workspace, "stray.txt", "stray\n")
	f.notes = nil
	s2 := f.sessions(t, Options{})
	if _, err := s2.Serve(bg); err != nil {
		t.Fatal(err)
	}
	if len(f.notes) == 0 {
		t.Error("the dirty workspace was not restored")
	}
	if exists(marker) {
		t.Fatal("a repository-configured program ran while the workspace was restored")
	}
	if got := mustRead(t, filepath.Join(info.Workspace, "a.txt")); got != "a\n" {
		t.Errorf("a.txt after restore = %q", got)
	}
}

func TestDamagedRecordIsSetAsideOnlyByAFreshStart(t *testing.T) {
	f := newSessionFixture(t, nil)
	s, old := f.start(t, Options{})
	// A damaged record whose fields point at things outside the state
	// directory: none of them may be acted on.
	victim := t.TempDir()
	write(t, victim, "precious.txt", "keep\n")
	record := filepath.Join(f.state, f.owner.ID, "session.json")
	for _, garbage := range []string{
		"{not json",
		`{"id":"zz","branch":"main","workspace":"` + victim + `","base":"x","anchor_key":"x"}`,
	} {
		if err := os.WriteFile(record, []byte(garbage), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Start(bg, false); err == nil || !strings.Contains(err.Error(), "--new-session") {
			t.Errorf("Start(false) on a damaged record: %v", err)
		}
		if _, err := s.Serve(bg); err == nil || !strings.Contains(err.Error(), "--new-session") {
			t.Errorf("Serve on a damaged record: %v", err)
		}
		info, err := s.Start(bg, true)
		if err != nil {
			t.Fatalf("Start(true) on a damaged record: %v", err)
		}
		if info.ID == old.ID {
			t.Error("the fresh start resumed")
		}
		if got, err := os.ReadFile(record + ".damaged"); err != nil || string(got) != garbage {
			t.Errorf("the damaged record was not set aside: %q, %v", got, err)
		}
		if got := mustRead(t, filepath.Join(victim, "precious.txt")); got != "keep\n" {
			t.Errorf("a file named by the damaged record changed: %q", got)
		}
		if !strings.Contains(branches(t, f.dir), "refs/heads/main") {
			t.Error("the owner's branch is gone")
		}
		ws, err := s.Serve(bg)
		if err != nil || ws.Root != info.Workspace {
			t.Errorf("Serve after the fresh start = %v, %v", ws, err)
		}
		old = info
	}
}

func TestStateDirMustBeOutsideTheCheckout(t *testing.T) {
	f := newSessionFixture(t, nil)
	for _, state := range []string{filepath.Join(f.dir, "state"), f.dir} {
		if s, err := NewSessions(f.owner, SessionOptions{StateDir: state}); err == nil {
			s.Close()
			t.Errorf("a state directory inside the checkout (%s) was accepted", state)
		}
	}
	// Reached through a symbolic link, too.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(f.dir, link); err != nil {
		t.Fatal(err)
	}
	if s, err := NewSessions(f.owner, SessionOptions{StateDir: filepath.Join(link, "state")}); err == nil {
		s.Close()
		t.Error("a state directory inside the checkout through a link was accepted")
	}
}

func TestOwnersIgnoreFileMustBeARegularFile(t *testing.T) {
	f := newSessionFixture(t, nil)
	link := filepath.Join(f.dir, ".repochatignore")
	if err := os.Symlink(filepath.Join(t.TempDir(), "nowhere"), link); err != nil {
		t.Fatal(err)
	}
	s := f.sessions(t, Options{})
	if _, err := s.Start(bg, false); err == nil {
		t.Fatal("a dangling symlink as the owner's .repochatignore was accepted")
	}
	if strings.Count(gitCmd(t, f.dir, "worktree", "list", "--porcelain"), "worktree ") != 1 {
		t.Error("a worktree was created")
	}

	// Started with a regular file, then swapped for a link: serving stops.
	os.Remove(link)
	write(t, f.dir, ".repochatignore", "secret/\n")
	if _, err := s.Start(bg, false); err != nil {
		t.Fatal(err)
	}
	os.Remove(link)
	target := filepath.Join(t.TempDir(), "rules")
	write(t, filepath.Dir(target), "rules", "secret/\n")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Serve(bg); err == nil {
		t.Error("served with a symlink as the owner's .repochatignore")
	}
	os.Remove(link)
	if err := os.Mkdir(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Serve(bg); err == nil {
		t.Error("served with a directory as the owner's .repochatignore")
	}
}

func TestSparseEscape(t *testing.T) {
	for in, want := range map[string]string{
		"plain.txt":    "plain.txt",
		"a*b?c[d]e":    `a\*b\?c\[d]e`,
		`back\slash`:   `back\\slash`,
		"!bang#hash":   `\!bang\#hash`,
		" lead trail ": `\ lead\ trail\ `,
	} {
		got, err := sparseEscape(in)
		if err != nil || got != want {
			t.Errorf("sparseEscape(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"a\nb", "end\r"} {
		if _, err := sparseEscape(bad); err == nil {
			t.Errorf("sparseEscape(%q) accepted", bad)
		}
	}
}

// C4: the owner adds a rule to .repochatignore, uncommitted, while a server
// runs. The next call takes the newly withheld file out of the workspace,
// and no write reaches it, not even right after a read that cached the
// earlier policy.
func TestOwnerIgnoreEditedWhileServingIsAppliedAtOnce(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"notes.txt": "hello\nworld\n", "open.txt": "fine\n"}, nil)
	ws := c.ws.Root
	anchor := c.anchor(t, "notes.txt", 1) // a read: the snapshot is cached now
	plan := c.accept(t, Change{Op: "replace", Path: "notes.txt", From: anchor, Text: "HELLO"})
	head := c.head(t)

	write(t, c.f.dir, ".repochatignore", "notes.txt\n")

	// The write half: within the snapshot's lifetime, directly and by plan.
	for _, try := range []struct {
		name  string
		apply func() error
	}{
		{"direct", func() error {
			_, err := c.s.Apply(bg, "", []Change{{Op: "replace", Path: "notes.txt", From: anchor, Text: "HELLO"}}, "")
			return err
		}},
		{"plan", func() error { _, err := c.s.Apply(bg, plan.ID, nil, ""); return err }},
		{"propose", func() error {
			_, err := c.s.Propose(bg, []Change{{Op: "insert", Path: "notes.txt", At: "end", Text: "x"}})
			return err
		}},
		{"move onto it", func() error {
			_, err := c.s.Apply(bg, "", []Change{{Op: "move", Path: "open.txt", ToPath: "notes.txt"}}, "")
			return err
		}},
	} {
		if err := try.apply(); err == nil {
			t.Errorf("%s: a change to the newly withheld notes.txt was accepted", try.name)
		} else {
			t.Logf("%s refused: %v", try.name, err)
		}
	}
	if got := c.head(t); got != head {
		t.Errorf("the session branch moved to %s", got)
	}
	if got := gitCmd(t, ws, "show", "HEAD:notes.txt"); got != "hello\nworld" {
		t.Errorf("notes.txt at the session's head = %q", got)
	}

	// The on-disk half: the file is gone from the workspace, and unreadable.
	if exists(filepath.Join(ws, "notes.txt")) {
		t.Error("notes.txt is still in the workspace")
	}
	if !exists(filepath.Join(ws, "open.txt")) {
		t.Error("open.txt left the workspace")
	}
	now, err := c.s.Serve(bg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := now.ReadFile(bg, "notes.txt", 1, 10, "", 1<<20); err == nil || !strings.Contains(err.Error(), "withheld") {
		t.Errorf("read notes.txt: %v", err)
	}
	if got := gitCmd(t, ws, "status", "--porcelain"); got != "" {
		t.Errorf("workspace status:\n%s", got)
	}

	// The rule goes again: the file comes back.
	if err := os.Remove(filepath.Join(c.f.dir, ".repochatignore")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.s.Start(bg, false); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, filepath.Join(ws, "notes.txt")); got != "hello\nworld\n" {
		t.Errorf("notes.txt after the rule went: %q", got)
	}
}

// The same with only a resume in between (Codex's call: start_session again
// in the same server process).
func TestOwnerIgnoreEditedWhileServingResyncsOnStart(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"notes.txt": "hello\n", "open.txt": "fine\n"}, nil)
	c.anchor(t, "notes.txt", 1)
	write(t, c.f.dir, ".repochatignore", "notes.txt\n")
	if _, err := c.s.Start(bg, false); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(c.ws.Root, "notes.txt")) {
		t.Error("notes.txt is still in the workspace after the resume")
	}
}

// C2: session.json rewritten to name a session whose workspace path is a
// symbolic link to another linked worktree of the repository. git resolves
// the link, so `worktree remove --force` would delete that worktree with
// everything in it. Nothing is removed through such a path, by any of the
// calls that remove a workspace.
func TestTamperedRecordCannotRemoveAnotherWorktree(t *testing.T) {
	for _, shape := range []string{"the workspace is a link", "its directory is a link"} {
		for _, call := range []string{"abandon", "fresh start"} {
			t.Run(shape+", "+call, func(t *testing.T) {
				f := newSessionFixture(t, nil)
				s, info := f.start(t, Options{})
				s.Close()
				// Another linked worktree, with work that exists nowhere else.
				victimParent := newDirPath(t)
				victim := filepath.Join(victimParent, "w")
				gitCmd(t, f.dir, "worktree", "add", "-q", "--detach", victim, "HEAD")
				write(t, victim, "precious.txt", "untracked, and nowhere else\n")

				dir := filepath.Dir(filepath.Dir(info.Workspace)) // <state>/<owner ID>
				fake := filepath.Join(dir, "deadbeef")
				if shape == "the workspace is a link" {
					if err := os.Mkdir(fake, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(victim, filepath.Join(fake, "w")); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(victimParent, fake); err != nil {
					t.Fatal(err)
				}
				recPath := filepath.Join(dir, sessionFile)
				raw := mustRead(t, recPath)
				raw = strings.ReplaceAll(raw, info.ID, "deadbeef")
				if err := os.WriteFile(recPath, []byte(raw), 0o600); err != nil {
					t.Fatal(err)
				}

				s2 := f.sessions(t, Options{})
				if rec, err := s2.readRecord(); err != nil || rec == nil || rec.Workspace != filepath.Join(fake, "w") {
					t.Fatalf("the tampered record was not read as one: %+v, %v", rec, err)
				}
				var err error
				if call == "abandon" {
					err = s2.Abandon(bg, false)
					if err == nil {
						t.Error("Abandon reported success")
					}
				} else {
					_, err = s2.Start(bg, true)
				}
				t.Logf("%s: %v", call, err)
				if got := mustRead(t, filepath.Join(victim, "precious.txt")); got != "untracked, and nowhere else\n" {
					t.Errorf("the other worktree's file = %q", got)
				}
				if got := gitCmd(t, f.dir, "worktree", "list", "--porcelain"); !strings.Contains(got, "worktree "+victim+"\n") {
					t.Errorf("the other worktree is no longer registered:\n%s", got)
				}
				if got := gitCmd(t, victim, "status", "--porcelain"); got != "?? precious.txt" {
					t.Errorf("the other worktree's status: %q", got)
				}
			})
		}
	}
}

// What is removed is the session's own workspace and nothing else: a
// directory at the recorded path that git does not know as that worktree
// stays.
func TestWorkspaceGitDoesNotKnowIsNotRemoved(t *testing.T) {
	f := newSessionFixture(t, nil)
	s, info := f.start(t, Options{})
	gitCmd(t, f.dir, "worktree", "remove", "--force", info.Workspace)
	write(t, info.Workspace, "kept.txt", "not a worktree any more\n")
	if err := s.Abandon(bg, false); err == nil {
		t.Error("Abandon reported success")
	}
	if !exists(filepath.Join(info.Workspace, "kept.txt")) {
		t.Error("a directory git does not know as the session's worktree was removed")
	}
}

func TestNoResyncRefusesInsteadOfSyncing(t *testing.T) {
	f := newSessionFixture(t, nil)
	s1, info := f.start(t, Options{Deny: []string{"notes.txt"}})
	if _, err := s1.Apply(bg, "", []Change{{Op: "create", Path: "new.txt", Text: "x\n"}}, "add"); err != nil {
		t.Fatal(err)
	}
	head := gitCmd(t, info.Workspace, "rev-parse", "HEAD")
	if exists(filepath.Join(info.Workspace, "notes.txt")) {
		t.Fatal("notes.txt is in the workspace although it is denied")
	}
	s1.Close()

	s2, err := NewSessions(f.owner, SessionOptions{StateDir: f.state, Repo: Options{}, NoResync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	_, err = s2.Apply(bg, "", []Change{{Op: "create", Path: "other.txt", Text: "y\n"}}, "add")
	if err == nil || !strings.Contains(err.Error(), "other exposure options") {
		t.Fatalf("Apply with other options and NoResync: %v", err)
	}
	if exists(filepath.Join(info.Workspace, "notes.txt")) || exists(filepath.Join(info.Workspace, "other.txt")) {
		t.Error("the workspace was changed")
	}
	if got := gitCmd(t, info.Workspace, "rev-parse", "HEAD"); got != head {
		t.Errorf("head moved: %s -> %s", head, got)
	}

	// With the same options it works, resync or not.
	s3, err := NewSessions(f.owner, SessionOptions{StateDir: f.state, Repo: Options{Deny: []string{"notes.txt"}}, NoResync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	if _, err := s3.Apply(bg, "", []Change{{Op: "create", Path: "other.txt", Text: "y\n"}}, "add"); err != nil {
		t.Errorf("Apply with matching options: %v", err)
	}
}
