package repo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// observer opens the fixture's sessions as `repochat session` does: it looks
// and merges, and never changes the workspace's files.
func (f *sessionFixture) observer(t *testing.T, opts Options) *Sessions {
	t.Helper()
	s, err := NewSessions(f.owner, SessionOptions{StateDir: f.state, Repo: opts, Notify: f.notify, Observe: true})
	if err != nil {
		t.Fatalf("NewSessions: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sparseFile(t *testing.T, ws string) string {
	t.Helper()
	dir := strings.TrimSpace(gitCmd(t, ws, "rev-parse", "--absolute-git-dir"))
	raw, err := os.ReadFile(filepath.Join(dir, "info", "sparse-checkout"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

var withSecret = map[string]string{"main.go": "package main\n", "plans.txt": "hidden\n"}

func TestObserverDoesNotChangeTheWorkspace(t *testing.T) {
	f := newSessionFixture(t, withSecret)
	_, info := f.start(t, Options{Deny: []string{"plans.txt"}})
	secret := filepath.Join(info.Workspace, "plans.txt")
	if exists(secret) {
		t.Fatal("the withheld file is in the workspace after start")
	}
	sparse := sparseFile(t, info.Workspace)

	o := f.observer(t, Options{})
	if _, err := o.Status(bg); err != nil {
		t.Fatalf("observer Status: %v", err)
	}
	if cps, err := o.Checkpoints(bg); err != nil || len(cps) != 1 {
		t.Fatalf("observer Checkpoints: %v %v", cps, err)
	}
	if exists(secret) {
		t.Error("an observer put the withheld file into the workspace")
	}
	if got := sparseFile(t, info.Workspace); got != sparse {
		t.Errorf("an observer rewrote the sparse-checkout file:\n%s\nwas:\n%s", got, sparse)
	}
}

func TestObserverRefusesToChangeTheWorkspace(t *testing.T) {
	f := newSessionFixture(t, nil)
	f.start(t, Options{})
	o := f.observer(t, Options{})
	for name, call := range map[string]func() error{
		"Serve":   func() error { _, err := o.Serve(bg); return err },
		"Start":   func() error { _, err := o.Start(bg, false); return err },
		"Propose": func() error { _, err := o.Propose(bg, []Change{{Op: "create", Path: "n.txt", Text: "x"}}); return err },
		"Apply": func() error {
			_, err := o.Apply(bg, "", []Change{{Op: "create", Path: "n.txt", Text: "x"}}, "m")
			return err
		},
		"UndoTo": func() error { _, err := o.UndoTo(bg, "0123456789ab"); return err },
		"Update": func() error { _, err := o.Update(bg); return err },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "does not change the workspace") {
			t.Errorf("observer %s: %v", name, err)
		}
	}
}

func TestObserverFinishesAndAbandons(t *testing.T) {
	t.Run("finish", func(t *testing.T) {
		f := newSessionFixture(t, nil)
		s, info := f.start(t, Options{})
		if _, err := s.Apply(bg, "", []Change{{Op: "create", Path: "new.txt", Text: "x"}}, "add"); err != nil {
			t.Fatal(err)
		}
		head := strings.TrimSpace(gitCmd(t, f.dir, "rev-parse", info.Branch))
		fin, err := f.observer(t, Options{}).Finish(bg, false, "")
		if err != nil {
			t.Fatalf("observer Finish: %v", err)
		}
		if got := strings.TrimSpace(gitCmd(t, f.dir, "rev-parse", "HEAD")); got != head || fin.Commit != head {
			t.Errorf("main is at %s, want %s", got, head)
		}
		if len(fin.Files) != 1 || fin.Files[0] != "new.txt" {
			t.Errorf("files: %v", fin.Files)
		}
		if exists(info.Workspace) {
			t.Error("the workspace was not removed")
		}
	})
	t.Run("abandon", func(t *testing.T) {
		f := newSessionFixture(t, nil)
		_, info := f.start(t, Options{})
		if err := f.observer(t, Options{}).Abandon(bg, false); err != nil {
			t.Fatalf("observer Abandon: %v", err)
		}
		if exists(info.Workspace) {
			t.Error("the workspace was not removed")
		}
	})
}

// Two serving processes with different exposure options share one session:
// each brings the workspace in line with its own policy before it serves.
func TestEachServerSyncsTheWorkspaceToItsOwnPolicy(t *testing.T) {
	f := newSessionFixture(t, withSecret)
	a, info := f.start(t, Options{Deny: []string{"plans.txt"}})
	b := f.sessions(t, Options{})
	secret := filepath.Join(info.Workspace, "plans.txt")
	record := func() sessionRecord {
		var rec sessionRecord
		raw, _ := os.ReadFile(filepath.Join(a.dir, sessionFile))
		json.Unmarshal(raw, &rec)
		return rec
	}

	digest := func(s *Sessions) string {
		rec := record()
		d, err := s.optionsDigest(bg, &rec)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	if _, err := a.Serve(bg); err != nil || exists(secret) {
		t.Fatalf("A: %v, secret on disk: %v", err, exists(secret))
	}
	plan, err := a.Propose(bg, []Change{{Op: "create", Path: "n.txt", Text: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Serve(bg); err != nil || !exists(secret) {
		t.Fatalf("B: %v, secret on disk: %v", err, exists(secret))
	}
	if got := record().SyncedWith; got == "" || got != digest(b) || got == digest(a) {
		t.Errorf("the record's digest is %q, want B's %q", got, digest(b))
	}
	if _, err := a.Serve(bg); err != nil || exists(secret) {
		t.Fatalf("A again: %v, secret on disk: %v", err, exists(secret))
	}
	if got := record().SyncedWith; got != digest(a) {
		t.Errorf("the record's digest is %q, want A's %q", got, digest(a))
	}
	if _, err := a.Apply(bg, plan.ID, nil, "m"); err == nil || !strings.Contains(err.Error(), "unknown or has expired") {
		t.Errorf("A's plan survived a re-sync: %v", err)
	}
}
