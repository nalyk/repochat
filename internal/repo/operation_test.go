package repo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// movedSession is a session that took two checkpoints after its start. A call
// that still believes the session is at first is made against one that has
// moved on to second.
type movedSession struct {
	c             *changeFixture
	first, second string
}

func newMovedSession(t *testing.T) *movedSession {
	t.Helper()
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	first := c.head(t)
	c.replaceLine(t, "b.txt", 1, "B")
	return &movedSession{c: c, first: first, second: c.head(t)}
}

// callKinds runs each of the four mutating calls with the given Call. Every
// one would change the session if it were let through.
var callKinds = []struct {
	name string
	// prep makes the call do something when it is allowed to.
	prep func(t *testing.T, c *changeFixture)
	run  func(c *changeFixture, m *movedSession, call Call) error
}{
	{
		name: "apply",
		run: func(c *changeFixture, m *movedSession, call Call) error {
			_, err := c.s.ApplyCall(bg, call, "", []Change{{Op: "create", Path: "c.txt", Text: "C"}}, "C")
			return err
		},
	},
	{
		name: "undo",
		run: func(c *changeFixture, m *movedSession, call Call) error {
			_, err := c.s.UndoToCall(bg, call, m.first[:12])
			return err
		},
	},
	{
		name: "update",
		prep: func(t *testing.T, c *changeFixture) {
			write(t, c.f.dir, "owner.txt", "owner\n")
			commit(t, c.f.dir, "owner's own")
		},
		run: func(c *changeFixture, m *movedSession, call Call) error {
			_, err := c.s.UpdateCall(bg, call)
			return err
		},
	},
	{
		name: "finish",
		run: func(c *changeFixture, m *movedSession, call Call) error {
			_, err := c.s.FinishCall(bg, call, false, "")
			return err
		},
	},
}

func TestExpectedCheckpointRefusesAMovedSession(t *testing.T) {
	for _, k := range callKinds {
		t.Run(k.name, func(t *testing.T) {
			m := newMovedSession(t)
			c := m.c
			if k.prep != nil {
				k.prep(t, c)
			}
			ownerTip := gitCmd(t, c.f.dir, "rev-parse", "refs/heads/main")
			ownerBefore := ownerState(t, c.f.dir, "a.txt", "b.txt", "owner.txt", "c.txt")
			wsBefore := wsState(t, c.ws.Root)
			info := c.info(t)

			err := k.run(c, m, Call{Expect: m.first[:12]})
			if err == nil || !strings.Contains(err.Error(), "the session moved") {
				t.Fatalf("%s with a stale checkpoint: %v; want a refusal saying the session moved", k.name, err)
			}
			var ce *ChangeError
			if !errors.As(err, &ce) {
				t.Errorf("the refusal is a %T, not a *ChangeError", err)
			}
			mustContain(t, err.Error(), "it is at checkpoint "+m.second[:12], "not "+m.first[:12], "session_status")
			if c.head(t) != m.second {
				t.Errorf("a refused call moved the branch to %s, from %s", c.head(t), m.second)
			}
			if got := wsState(t, c.ws.Root); got != wsBefore {
				t.Errorf("a refused call changed the workspace:\n%s\nwas:\n%s", got, wsBefore)
			}
			if exists(filepath.Join(c.ws.Root, "c.txt")) {
				t.Error("part of the refused change is on disk")
			}
			if got := gitCmd(t, c.f.dir, "rev-parse", "refs/heads/main"); got != ownerTip {
				t.Errorf("the owner's branch is at %s, was %s", got, ownerTip)
			}
			if got := ownerState(t, c.f.dir, "a.txt", "b.txt", "owner.txt", "c.txt"); got != ownerBefore {
				t.Errorf("a refused call changed the owner's checkout:\n%s\nwas:\n%s", got, ownerBefore)
			}
			if now := c.info(t); now.Head != info.Head || !exists(info.Workspace) {
				t.Errorf("a refused call changed the session: %+v, was %+v", now, info)
			}
			if !strings.Contains(branches(t, c.f.dir), info.Branch) {
				t.Error("a refused call deleted the session branch")
			}
		})
	}
}

func TestExpectedCheckpointAcceptsAnyPrefixForm(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	forms := []struct {
		name string
		of   func(head string) string
	}{
		{"7 characters", func(h string) string { return h[:7] }},
		{"12 characters", func(h string) string { return h[:12] }},
		{"40 characters", func(h string) string { return h }},
		{"12 upper-case characters", func(h string) string { return strings.ToUpper(h[:12]) }},
	}
	for i, form := range forms {
		before := c.head(t)
		path := fmt.Sprintf("n%d.txt", i)
		a, err := c.s.ApplyCall(bg, Call{Expect: form.of(before)}, "", []Change{{Op: "create", Path: path, Text: "x"}}, "new")
		if err != nil {
			t.Fatalf("ApplyCall expecting %s: %v", form.name, err)
		}
		after := c.head(t)
		if after == before || a.Checkpoint.ID != after[:12] {
			t.Errorf("%s: head %s, was %s, checkpoint %+v", form.name, after, before, a.Checkpoint)
		}
		if mustRead(t, filepath.Join(c.ws.Root, path)) != "x\n" {
			t.Errorf("%s: %s was not written", form.name, path)
		}
	}
}

func TestExpectedCheckpointIsAcceptedByEveryCallWhenTheSessionIsThere(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	head := c.head(t)

	u, err := c.s.UpdateCall(bg, Call{Expect: head[:12]})
	if err != nil {
		t.Fatalf("UpdateCall at the session's checkpoint: %v", err)
	}
	if u.Checkpoint.ID != head[:12] || c.head(t) != head {
		t.Errorf("an update with nothing to merge made a checkpoint: %+v", u.Checkpoint)
	}
	if _, err := c.s.UndoToCall(bg, Call{Expect: head[:12]}, head[:12]); err != nil {
		t.Fatalf("UndoToCall at the session's checkpoint: %v", err)
	}
	if c.head(t) != head {
		t.Error("an undo to the current checkpoint moved the branch")
	}
	// An undo that does restore files, then the finish that lands them.
	base := c.base(t)
	undone, err := c.s.UndoToCall(bg, Call{Expect: strings.ToUpper(head[:7])}, base[:12])
	if err != nil {
		t.Fatalf("UndoToCall to the start: %v", err)
	}
	if c.head(t) == head || undone.Checkpoint.Kind != "undo" {
		t.Errorf("the undo did not restore: %+v", undone.Checkpoint)
	}
	c.replaceLine(t, "b.txt", 1, "B")
	head = c.head(t)
	info := c.info(t)
	fin, err := c.s.FinishCall(bg, Call{Expect: head}, false, "")
	if err != nil {
		t.Fatalf("FinishCall at the session's checkpoint: %v", err)
	}
	if got := gitCmd(t, c.f.dir, "rev-parse", "refs/heads/main"); got != head || fin.Commit != head {
		t.Errorf("main is at %s, Finished.Commit %s; want %s", got, fin.Commit, head)
	}
	c.sessionDone(t, info)
}

func TestExpectedCheckpointRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"zzzzzzz", "abc", strings.Repeat("a", 41), "0123456 ", "0x01234567"} {
		for _, k := range callKinds {
			t.Run(fmt.Sprintf("%s/%q", k.name, bad), func(t *testing.T) {
				m := newMovedSession(t)
				c := m.c
				wsBefore := wsState(t, c.ws.Root)
				ownerTip := gitCmd(t, c.f.dir, "rev-parse", "refs/heads/main")

				// The session lock is held here, so a call that reads the
				// expected checkpoint only after taking it would not return.
				release, err := c.s.acquire()
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- k.run(c, m, Call{Expect: bad}) }()
				select {
				case err = <-done:
				case <-time.After(10 * time.Second):
					release()
					<-done
					t.Fatalf("%s waited for the session lock to refuse a malformed checkpoint", k.name)
				}
				release()

				if err == nil || !strings.Contains(err.Error(), "expected checkpoint") {
					t.Fatalf("%s expecting %q: %v; want a refusal naming the expected checkpoint", k.name, bad, err)
				}
				if strings.Contains(err.Error(), "the session moved") {
					t.Errorf("a malformed checkpoint was compared with the head: %v", err)
				}
				if c.head(t) != m.second {
					t.Errorf("the branch moved to %s", c.head(t))
				}
				if got := wsState(t, c.ws.Root); got != wsBefore {
					t.Errorf("the workspace changed:\n%s\nwas:\n%s", got, wsBefore)
				}
				if got := gitCmd(t, c.f.dir, "rev-parse", "refs/heads/main"); got != ownerTip {
					t.Errorf("the owner's branch moved to %s", got)
				}
				if _, err := os.Stat(c.info(t).Workspace); err != nil {
					t.Errorf("the session workspace is gone: %v", err)
				}
			})
		}
	}
}

func TestExpectedCheckpointPatterns(t *testing.T) {
	for _, ok := range []string{"0123456", "abcdef0123456789", "ABCDEF0", strings.Repeat("f", 40)} {
		if !ExpectRe.MatchString(ok) {
			t.Errorf("ExpectRe refuses %q", ok)
		}
	}
	for _, bad := range []string{"", "abc", "012345", "zzzzzzz", strings.Repeat("a", 41), "0123456\n"} {
		if ExpectRe.MatchString(bad) {
			t.Errorf("ExpectRe accepts %q", bad)
		}
	}
	for _, ok := range []string{"abcdefgh", "a-b_c.d1234", strings.Repeat("x", 64)} {
		if !OperationIDRe.MatchString(ok) {
			t.Errorf("OperationIDRe refuses %q", ok)
		}
	}
	for _, bad := range []string{"", "short", strings.Repeat("x", 65), "has space 1234", "slash/slash1", "line\nbreak123"} {
		if OperationIDRe.MatchString(bad) {
			t.Errorf("OperationIDRe accepts %q", bad)
		}
	}
}

func TestCheckExpected(t *testing.T) {
	head := "0123456789abcdef0123456789abcdef01234567"
	if err := checkExpected(head, ""); err != nil {
		t.Errorf("no expectation: %v", err)
	}
	if err := checkExpected(head, "0123456"); err != nil {
		t.Errorf("a prefix of the head: %v", err)
	}
	if err := checkExpected(head, "0123457"); err == nil {
		t.Error("a checkpoint that is not the head was accepted")
	} else {
		var ce *ChangeError
		if !errors.As(err, &ce) {
			t.Fatalf("%T is not a *ChangeError", err)
		}
		want := "the session moved: it is at checkpoint 0123456789ab, not 0123457; another conversation may be editing it. Call session_status, read the files again and retry with the new checkpoint"
		if ce.Reason != want {
			t.Errorf("Reason:\n%s\nwant:\n%s", ce.Reason, want)
		}
	}
}
