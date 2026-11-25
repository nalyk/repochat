package repo

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func opCall(op, digest string) Call { return Call{Op: op, Digest: digest} }

func createC(c *changeFixture, call Call) (*Applied, error) {
	return c.s.ApplyCall(bg, call, "", []Change{{Op: "create", Path: "c.txt", Text: "C"}}, "C")
}

func TestOperationReplayReturnsTheSameCheckpoint(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	call := opCall("op-aaaa1111", "d1d1d1d1d1d1d1d1")
	first, err := createC(c, call)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || first.Checkpoint.Operation != "op-aaaa1111" {
		t.Errorf("first call: %+v", first)
	}
	head := c.head(t)
	second, err := createC(c, call)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.Replayed || second.Checkpoint.ID != first.Checkpoint.ID {
		t.Errorf("replay: %+v, first %+v", second, first)
	}
	if c.head(t) != head || len(c.checkpoints(t)) != 2 {
		t.Errorf("the replay wrote: head %s, was %s; %d checkpoints", c.head(t), head, len(c.checkpoints(t)))
	}
}

func TestOperationReuseWithOtherArgumentsIsRefused(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	if _, err := createC(c, opCall("op-aaaa1111", "d1d1d1d1d1d1d1d1")); err != nil {
		t.Fatal(err)
	}
	head := c.head(t)
	_, err := c.s.ApplyCall(bg, opCall("op-aaaa1111", "d2d2d2d2d2d2d2d2"), "", []Change{{Op: "create", Path: "d.txt", Text: "D"}}, "D")
	if err == nil || !strings.Contains(err.Error(), "different call") {
		t.Fatalf("got %v; want a refusal naming a different call", err)
	}
	if c.head(t) != head || exists(filepath.Join(c.ws.Root, "d.txt")) {
		t.Error("the refused call changed the session")
	}
}

func TestOperationSurvivesRestart(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	call := opCall("op-aaaa1111", "d1d1d1d1d1d1d1d1")
	first, err := createC(c, call)
	if err != nil {
		t.Fatal(err)
	}
	c.s.Close()
	s2, _ := c.f.start(t, Options{})
	if _, err := s2.Serve(bg); err != nil {
		t.Fatal(err)
	}
	c.s = s2
	again, err := createC(c, call)
	if err != nil {
		t.Fatalf("replay after restart: %v", err)
	}
	if !again.Replayed || again.Checkpoint.ID != first.Checkpoint.ID {
		t.Errorf("after restart: %+v, first %+v", again, first)
	}
}

func TestOperationReplayWinsOverExpect(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	before := c.head(t)
	call := opCall("op-aaaa1111", "d1d1d1d1d1d1d1d1")
	call.Expect = before[:12]
	first, err := createC(c, call)
	if err != nil {
		t.Fatal(err)
	}
	again, err := createC(c, call)
	if err != nil || !again.Replayed || again.Checkpoint.ID != first.Checkpoint.ID {
		t.Fatalf("retry with the old Expect: %+v, %v", again, err)
	}
}

func TestStoredPlanReplayWinsOverExpect(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	before := c.head(t)
	plan := c.accept(t, Change{Op: "create", Path: "c.txt", Text: "C"})
	call := Call{Expect: before[:12]}
	first, err := c.s.ApplyCall(bg, call, plan.ID, nil, "C")
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.s.ApplyCall(bg, call, plan.ID, nil, "C")
	if err != nil {
		t.Fatalf("retry of an applied plan with the old Expect: %v", err)
	}
	if again.Checkpoint.ID != first.Checkpoint.ID || len(c.checkpoints(t)) != 2 {
		t.Errorf("replay %+v, first %+v", again.Checkpoint, first.Checkpoint)
	}
	// A plan not applied yet is still held to Expect.
	plan2 := c.accept(t, Change{Op: "create", Path: "e.txt", Text: "E"})
	if _, err := c.s.ApplyCall(bg, call, plan2.ID, nil, "E"); err == nil || !strings.Contains(err.Error(), "the session moved") {
		t.Errorf("an unapplied plan with a stale Expect: %v", err)
	}
}

func TestConcurrentSameOperationAppliesOnce(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	call := opCall("op-aaaa1111", "d1d1d1d1d1d1d1d1")
	res := make([]*Applied, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range res {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res[i], errs[i] = createC(c, call)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if res[0].Replayed == res[1].Replayed {
		t.Errorf("Replayed: %v and %v; want exactly one", res[0].Replayed, res[1].Replayed)
	}
	if res[0].Checkpoint.ID != res[1].Checkpoint.ID || len(c.checkpoints(t)) != 2 {
		t.Errorf("%s and %s; %d checkpoints", res[0].Checkpoint.ID, res[1].Checkpoint.ID, len(c.checkpoints(t)))
	}
}

func TestOperationFromAnAbandonedSessionIsNotReplayed(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	call := opCall("op-aaaa1111", "d1d1d1d1d1d1d1d1")
	changes := []Change{{Op: "replace", Path: "a.txt", From: c.anchor(t, "a.txt", 1), To: c.anchor(t, "a.txt", 1), Text: "AA\n"}}
	if _, err := c.s.ApplyCall(bg, call, "", changes, "AA"); err != nil {
		t.Fatal(err)
	}
	if err := c.s.Abandon(bg, false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.s.Start(bg, false); err != nil {
		t.Fatal(err)
	}
	ws, err := c.s.Serve(bg)
	if err != nil {
		t.Fatal(err)
	}
	c.ws = ws
	head := c.head(t)
	a, err := c.s.ApplyCall(bg, call, "", changes, "AA")
	if err == nil || (a != nil && a.Replayed) {
		t.Fatalf("the old call in a new session: %+v, %v; want a refusal for stale anchors", a, err)
	}
	if c.head(t) != head {
		t.Error("the refused call moved the branch")
	}
	fresh := []Change{{Op: "create", Path: "c.txt", Text: "C"}}
	a, err = c.s.ApplyCall(bg, call, "", fresh, "C")
	if err != nil || a.Replayed {
		t.Fatalf("op reused with fresh anchors: %+v, %v", a, err)
	}
}

func TestUndoAndUpdateCarryOperations(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	first := c.head(t)
	c.replaceLine(t, "b.txt", 1, "B")

	undo := opCall("op-undo0001", "a1a1a1a1a1a1a1a1")
	u1, err := c.s.UndoToCall(bg, undo, first[:12])
	if err != nil {
		t.Fatal(err)
	}
	head := c.head(t)
	u2, err := c.s.UndoToCall(bg, undo, first[:12])
	if err != nil || !u2.Replayed || u2.Checkpoint.ID != u1.Checkpoint.ID || c.head(t) != head {
		t.Fatalf("undo replay: %+v, %v", u2, err)
	}
	if u1.Checkpoint.Operation != "op-undo0001" {
		t.Errorf("undo checkpoint: %+v", u1.Checkpoint)
	}

	write(t, c.f.dir, "owner.txt", "owner\n")
	commit(t, c.f.dir, "owner's own")
	up := opCall("op-updt0001", "b1b1b1b1b1b1b1b1")
	m1, err := c.s.UpdateCall(bg, up)
	if err != nil || m1.Checkpoint.Kind != "update" {
		t.Fatalf("update: %+v, %v", m1, err)
	}
	head = c.head(t)
	m2, err := c.s.UpdateCall(bg, up)
	if err != nil || !m2.Replayed || m2.Checkpoint.ID != m1.Checkpoint.ID || c.head(t) != head {
		t.Fatalf("update replay: %+v, %v", m2, err)
	}
	if _, err := c.s.UpdateCall(bg, opCall("op-updt0001", "c0c0c0c0c0c0c0c0")); err == nil || !strings.Contains(err.Error(), "different call") {
		t.Errorf("update with another digest: %v", err)
	}
}

func TestCheckpointMessagesHideTheOperationTrailer(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	if _, err := createC(c, opCall("op-aaaa1111", "d1d1d1d1d1d1d1d1")); err != nil {
		t.Fatal(err)
	}
	cp := c.checkpoints(t)[0]
	if cp.Message != "C" || cp.Operation != "op-aaaa1111" || cp.Kind != "change" {
		t.Errorf("%+v", cp)
	}
}

func TestTwoTrailerCheckpointsStillParse(t *testing.T) {
	kind, text, op, _ := checkpointKind("hand\n\nMade-through: repochat\nRepochat-Kind: change\n")
	if kind != "change" || text != "hand" || op != "" {
		t.Errorf("%q %q %q", kind, text, op)
	}
	for _, bad := range []string{
		"x\n\nMade-through: repochat\nRepochat-Kind: change\nRepochat-Operation: short d\n",
		"x\n\nMade-through: repochat\nRepochat-Kind: change\nRepochat-Operation: op-aaaa1111\n",
		"x\n\nMade-through: repochat\nRepochat-Kind: change\nRepochat-Operation: op-aaaa1111 d1d1d1d1d1d1d1d1 extra\n",
		"x\n\nMade-through: repochat\nRepochat-Kind: change\nOther: op-aaaa1111 d1d1d1d1d1d1d1d1\n",
		"x\n\nMade-through: repochat\nRepochat-Kind: change\nRepochat-Operation: op-aaaa1111 d1d1d1d1d1d1d1d1\nMore: x\n",
	} {
		if k, _, _, _ := checkpointKind(bad); k != "unknown" {
			t.Errorf("%q parsed as %s", bad, k)
		}
	}
	// A commit made with the old two-trailer message, on a real session.
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	old := gitCmd(t, c.ws.Root, "commit-tree", "-p", "HEAD", "-m", "old\n\nMade-through: repochat\nRepochat-Kind: change", "HEAD^{tree}")
	gitCmd(t, c.ws.Root, "update-ref", "refs/heads/"+c.branch(t), old)
	cps := c.checkpoints(t)
	if cps[0].Kind != "change" || cps[0].Message != "old" || cps[0].Operation != "" {
		t.Errorf("%+v", cps[0])
	}
	_, msg := commitMessage("old", "change", Call{})
	if strings.Contains(string(msg), "Operation") {
		t.Errorf("a call without an operation wrote %q", msg)
	}
}

func TestDigestMustBeSixteenHexCharacters(t *testing.T) {
	for _, bad := range []string{"", "d1", "D1D1D1D1D1D1D1D1", "d1d1d1d1d1d1d1d1d", "d1d1d1d1d1d1d1\x1e1", "d1d1d1d1d1d1d1\x1f1", "d1d1d1d1d1d1d1\x001", "d1d1d1d1d1d1d1 1"} {
		if err := validateCall(Call{Op: "op-aaaa1111", Digest: bad}); err == nil {
			t.Errorf("digest %q was accepted", bad)
		}
	}
	if err := validateCall(Call{Op: "op-aaaa1111", Digest: "0123456789abcdef"}); err != nil {
		t.Error(err)
	}
}

func readReceipts(t *testing.T, s *Sessions) []receipt {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.dir, "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rs []receipt
	if err := json.Unmarshal(data, &rs); err != nil {
		t.Fatalf("operations.json: %v", err)
	}
	return rs
}

func TestFinishReplayAfterTheSessionClosed(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	call := opCall("fin-aaaa1111", "f1f1f1f1f1f1f1f1")
	first, err := c.s.FinishCall(bg, call, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed {
		t.Error("the first call reports Replayed")
	}
	owner := ownerState(t, c.f.dir, "a.txt")
	second, err := c.s.FinishCall(bg, call, false, "")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.Replayed || second.Commit != first.Commit || second.Head != first.Head || second.Base != first.Base {
		t.Errorf("replay %+v, first %+v", second, first)
	}
	if got := ownerState(t, c.f.dir, "a.txt"); got != owner {
		t.Errorf("the replay changed the owner's checkout:\n%s\nwas\n%s", got, owner)
	}
	other := opCall("fin-aaaa1111", "e2e2e2e2e2e2e2e2")
	if _, err := c.s.FinishCall(bg, other, false, ""); err == nil || !strings.Contains(err.Error(), "different call") {
		t.Errorf("other digest: %v", err)
	}
}

func TestFinishStartedReceiptRefuses(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	data, _ := json.Marshal([]receipt{{ID: "fin-xxxx1111", Tool: "finish", Digest: "f1f1f1f1f1f1f1f1", State: "started", Time: time.Now()}})
	if err := os.WriteFile(filepath.Join(c.s.dir, "operations.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	tip := gitCmd(t, c.f.dir, "rev-parse", "refs/heads/main")
	_, err := c.s.FinishCall(bg, opCall("fin-xxxx1111", "f1f1f1f1f1f1f1f1"), false, "")
	if err == nil || !strings.Contains(err.Error(), "did not report its outcome") {
		t.Fatalf("got %v", err)
	}
	if got := gitCmd(t, c.f.dir, "rev-parse", "refs/heads/main"); got != tip {
		t.Error("the owner's branch moved")
	}
}

func TestFinishWritesStartedBeforeTheMergeAndDoneAfter(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	c.replaceLine(t, "a.txt", 1, "A")
	var during []receipt
	c.s.beforeOwnerMerge = func() { during = readReceipts(t, c.s) }
	if _, err := c.s.FinishCall(bg, opCall("fin-aaaa1111", "f1f1f1f1f1f1f1f1"), false, ""); err != nil {
		t.Fatal(err)
	}
	if len(during) != 1 || during[0].State != "started" || during[0].Tool != "finish" {
		t.Errorf("at the merge: %+v", during)
	}
	if after := readReceipts(t, c.s); len(after) != 1 || after[0].State != "done" || after[0].Finished == nil {
		t.Errorf("after: %+v", after)
	}
}

func TestFinishRefusedBeforeTheMergeWritesNoReceipt(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	if _, err := c.s.FinishCall(bg, opCall("fin-aaaa1111", "f1f1f1f1f1f1f1f1"), false, ""); err == nil {
		t.Fatal("finish of an empty session succeeded")
	}
	if exists(filepath.Join(c.s.dir, "operations.json")) {
		t.Error("a refused finish left a receipt")
	}
}

func TestFinishReportsTheReviewedRevision(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	base := c.base(t)
	c.replaceLine(t, "a.txt", 1, "A")
	head := c.head(t)
	c.replaceLine(t, "a.txt", 1, "AA")
	_ = head
	head = c.head(t)
	fin, err := c.s.FinishCall(bg, Call{Expect: head}, true, "squashed")
	if err != nil {
		t.Fatal(err)
	}
	if fin.Head != head || fin.Base != base {
		t.Errorf("Base %s Head %s; want %s %s", fin.Base, fin.Head, base, head)
	}
	if fin.Commit == head {
		t.Error("a squash landed the session head itself")
	}
}

func TestStartFreshReplayDoesNotStartAThirdSession(t *testing.T) {
	f := newSessionFixture(t, nil)
	s, first := f.start(t, Options{})
	call := opCall("start-aaaa1111", "a1a1a1a1a1a1a1a1")
	a, err := s.StartCall(bg, call, true)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == first.ID || a.Replayed {
		t.Fatalf("fresh start: %+v, first %+v", a, first)
	}
	b, err := s.StartCall(bg, call, true)
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != a.ID || !b.Replayed {
		t.Errorf("replay: %+v, want session %s replayed", b, a.ID)
	}
	if cur, err := s.Current(bg); err != nil || cur.ID != a.ID {
		t.Errorf("current: %+v, %v", cur, err)
	}
}

func TestAbandonReplay(t *testing.T) {
	f := newSessionFixture(t, nil)
	s, _ := f.start(t, Options{})
	call := opCall("aban-aaaa1111", "b1b1b1b1b1b1b1b1")
	if replayed, err := s.AbandonCall(bg, call, false); err != nil || replayed {
		t.Fatalf("first: %v, %v", replayed, err)
	}
	if replayed, err := s.AbandonCall(bg, call, false); err != nil || !replayed {
		t.Fatalf("second: %v, %v", replayed, err)
	}
	if _, err := s.AbandonCall(bg, opCall("aban-aaaa1111", "c1c1c1c1c1c1c1c1"), false); err == nil || !strings.Contains(err.Error(), "different call") {
		t.Errorf("other digest: %v", err)
	}
	if _, err := s.AbandonCall(bg, opCall("aban-bbbb2222", "b1b1b1b1b1b1b1b1"), false); !errors.Is(err, ErrNoSession) {
		t.Errorf("new op with no session: %v", err)
	}
}

func TestReceiptsKeepTheNewest64(t *testing.T) {
	f := newSessionFixture(t, nil)
	s, _ := f.start(t, Options{})
	for i := 0; i < 65; i++ {
		if _, err := s.AbandonCall(bg, opCall(fmt.Sprintf("aban-%04d", i), "b1b1b1b1b1b1b1b1"), false); err != nil {
			t.Fatalf("abandon %d: %v", i, err)
		}
		if _, err := s.StartCall(bg, opCall(fmt.Sprintf("start-%04d", i), "a1a1a1a1a1a1a1a1"), false); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	rs := readReceipts(t, s)
	if len(rs) != 64 {
		t.Fatalf("%d receipts, want 64", len(rs))
	}
	for _, r := range rs {
		if r.ID == "aban-0000" {
			t.Error("the oldest receipt is still there")
		}
	}
	if rs[63].ID != "start-0064" {
		t.Errorf("newest is %s", rs[63].ID)
	}
}

func TestDamagedReceiptsAreReplacedButUnreadableRefusesTheCall(t *testing.T) {
	f := newSessionFixture(t, nil)
	s, _ := f.start(t, Options{})
	path := filepath.Join(s.dir, "operations.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AbandonCall(bg, opCall("aban-aaaa1111", "b1b1b1b1b1b1b1b1"), false); err != nil {
		t.Fatalf("damaged file: %v", err)
	}
	if rs := readReceipts(t, s); len(rs) != 1 || rs[0].ID != "aban-aaaa1111" {
		t.Errorf("receipts after a damaged file: %+v", rs)
	}
	// A directory where the file should be cannot be read.
	os.Remove(path)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartCall(bg, opCall("start-aaaa1111", "a1a1a1a1a1a1a1a1"), false); err == nil {
		t.Error("an unreadable receipt file did not refuse the call")
	}
	if _, err := s.Current(bg); !errors.Is(err, ErrNoSession) {
		t.Errorf("the refused start still opened a session: %v", err)
	}
}

func TestFinishRefusedByTheMergeCanBeRetried(t *testing.T) {
	c := newApplyFixture(t, map[string]string{"a.txt": "a\n"}, nil)
	if _, err := createC(c, Call{}); err != nil {
		t.Fatal(err)
	}
	call := opCall("fin-aaaa1111", "f1f1f1f1f1f1f1f1")
	// A file of the owner's appears where the merge adds one, after the
	// last check and before the merge: git refuses, touching nothing.
	c.s.beforeOwnerMerge = func() { write(t, c.f.dir, "c.txt", "mine\n") }
	if _, err := c.s.FinishCall(bg, call, false, ""); err == nil {
		t.Fatal("finish succeeded over the owner's file")
	}
	if rs := readReceipts(t, c.s); len(rs) != 0 {
		t.Fatalf("a clean refusal left receipts: %+v", rs)
	}
	c.s.beforeOwnerMerge = nil
	os.Remove(filepath.Join(c.f.dir, "c.txt"))
	if fin, err := c.s.FinishCall(bg, call, false, ""); err != nil || fin.Replayed {
		t.Fatalf("retry: %+v, %v", fin, err)
	}
}
