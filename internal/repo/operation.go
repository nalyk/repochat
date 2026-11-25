package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Call is what a mutating call says about itself, beyond its arguments.
type Call struct {
	// Op is the operation id the caller chose for this call; "" for none.
	Op string
	// Digest is the digest of the call's arguments, recorded with Op.
	Digest string
	// Expect is the checkpoint the session must be at, as a hexadecimal
	// prefix of 7 to 64 characters of its ID; "" for no check.
	Expect string
}

var (
	// OperationIDRe is the form of an operation id.
	OperationIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)
	// ExpectRe is the form of an expected checkpoint.
	ExpectRe = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
	// DigestRe is the form of an argument digest: the first 16 hexadecimal
	// characters of a SHA-256.
	DigestRe = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// validateCall refuses a Call whose fields are not in their forms. It reads
// no session state, so it runs before the lock is taken.
func validateCall(call Call) error {
	if call.Expect != "" && !ExpectRe.MatchString(call.Expect) {
		return fmt.Errorf("the expected checkpoint %q is not a checkpoint ID: give 7 to 64 hexadecimal characters of the ID that session_status shows (nothing was changed)", SanitizeLine(truncate(call.Expect, 40)))
	}
	if call.Op != "" {
		if !OperationIDRe.MatchString(call.Op) {
			return fmt.Errorf("the operation_id %q is not an operation id: give 8 to 64 letters, digits, dots, underscores or hyphens (nothing was changed)", SanitizeLine(truncate(call.Op, 40)))
		}
		if !DigestRe.MatchString(call.Digest) {
			return errors.New("an operation id needs a digest of the call's arguments: 16 lower-case hexadecimal characters (nothing was changed)")
		}
	}
	return nil
}

// checkExpected refuses a call made for a session that is no longer at the
// checkpoint it names: head is the session branch's head (the full ID),
// expect the hexadecimal prefix the caller gave, or "" for none.
func checkExpected(head, expect string) error {
	if reason := movedReason(head, expect); reason != "" {
		return &ChangeError{Reason: reason}
	}
	return nil
}

// checkExpectedPlain is checkExpected for the calls that are not change sets
// (undo, update, finish): the refusal is plain text, without a change set's
// "the whole change set was refused".
func checkExpectedPlain(head, expect string) error {
	if reason := movedReason(head, expect); reason != "" {
		return errors.New(reason + " (nothing was changed)")
	}
	return nil
}

// movedReason is why a session at head is not at the checkpoint expect
// names, or "" when it is (or no checkpoint was named).
func movedReason(head, expect string) string {
	if expect == "" || strings.HasPrefix(head, strings.ToLower(expect)) {
		return ""
	}
	return fmt.Sprintf("the session moved: it is at checkpoint %s, not %s; another conversation may be editing it. Call session_status, read the files again and retry with the new checkpoint", head[:12], SanitizeLine(expect))
}

// replayOperation answers a call whose operation id the session has already
// recorded: the same arguments give the earlier result (Replayed, nothing is
// written), other arguments are refused. It returns nil, nil when the call
// has no operation id or the id is new. chain is the session's checkpoints,
// newest first, and the lock is held.
func (s *Sessions) replayOperation(ctx context.Context, rec *sessionRecord, chain []string, call Call) (*Applied, error) {
	if call.Op == "" {
		return nil, nil
	}
	cp, digest, found, err := s.findOperation(ctx, rec, chain, call.Op)
	if err != nil || !found {
		return nil, err
	}
	if digest != call.Digest {
		return nil, refusal("", "operation_id %s was already used for a different call; choose a new id (nothing was changed)", call.Op)
	}
	return &Applied{Checkpoint: *cp, Replayed: true}, nil
}

// findOperation looks for the checkpoint of the session that carries
// operation id op: it reads the messages of the whole chain (newest first,
// the base last) with one git log. Operation ids of an earlier session are
// not on this chain and are not found.
func (s *Sessions) findOperation(ctx context.Context, rec *sessionRecord, chain []string, op string) (cp *Checkpoint, digest string, found bool, err error) {
	if len(chain) < 2 {
		return nil, "", false, nil
	}
	ws := s.reader()
	out, err := ws.git(ctx, 64<<20, "log", "--first-parent", "--no-color", "--format=%H%x1f%B%x1e", chain[0], "^"+chain[len(chain)-1])
	if err != nil {
		return nil, "", false, err
	}
	for _, entry := range strings.Split(string(out), "\x1e") {
		id, msg, ok := strings.Cut(strings.TrimLeft(entry, "\n"), "\x1f")
		if !ok {
			continue
		}
		if _, _, o, d := checkpointKind(msg); o != op {
			continue
		} else {
			digest = d
		}
		i := slices.Index(chain, id)
		if i < 0 || i+1 >= len(chain) {
			return nil, "", false, errors.New("git log named a commit that is not on the session's chain")
		}
		pol, err := s.policy(ctx, rec)
		if err != nil {
			return nil, "", false, err
		}
		cp, err := ws.describeCheckpoint(ctx, pol, id, chain[i+1])
		if err != nil {
			return nil, "", false, err
		}
		return cp, digest, true, nil
	}
	return nil, "", false, nil
}

const (
	operationsFile = "operations.json"
	maxReceipts    = 64
	receiptStarted = "started"
	receiptDone    = "done"
)

// receipt is what start_session, finish_session and abandon_session leave
// behind for a retry to find: these calls leave no checkpoint to carry a
// trailer. A finish writes "started" right before it merges into the owner's
// branch and "done" once the session is closed; start and abandon write
// "done" only.
type receipt struct {
	ID       string       `json:"id"`
	Tool     string       `json:"tool"`
	Digest   string       `json:"digest"`
	State    string       `json:"state"`
	Time     time.Time    `json:"time"`
	Finished *Finished    `json:"finished,omitempty"`
	Info     *SessionInfo `json:"info,omitempty"`
}

// readReceipts returns the receipts, oldest first. A missing file, or one
// that is empty, gives none; a file that cannot be read or parsed is an error naming it, so
// that a call with an operation id is never run on a guess.
func (s *Sessions) readReceipts() ([]receipt, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, operationsFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("the operation receipts cannot be read, so the retry check cannot be made: %v", pathErrCause(err))
	}
	var rs []receipt
	if err := json.Unmarshal(data, &rs); err != nil {
		return nil, fmt.Errorf("%s in repochat's state directory is damaged (%v), so the retry check cannot be made; the owner can inspect or delete it", operationsFile, err)
	}
	return rs, nil
}

// findReceipt looks up the receipt for call, for the tool named. It returns
// nil, nil when the call has no operation id or the id is new; the lock is
// held. The same id with another digest or tool is refused, and so is an id
// whose finish began merging without reporting.
func (s *Sessions) findReceipt(tool string, call Call) (*receipt, error) {
	if call.Op == "" {
		return nil, nil
	}
	rs, err := s.readReceipts()
	if err != nil {
		return nil, fmt.Errorf("%w (nothing was changed)", err)
	}
	for i := range rs {
		r := &rs[i]
		if r.ID != call.Op {
			continue
		}
		if r.Digest != call.Digest || r.Tool != tool {
			return nil, refusal("", "operation_id %s was already used for a different call; choose a new id (nothing was changed)", call.Op)
		}
		if r.State != receiptDone {
			return nil, fmt.Errorf("the %s_session call with this operation_id began merging and did not report its outcome; the owner can check with `repochat session` and `git log` (nothing was run)", tool)
		}
		return r, nil
	}
	return nil, nil
}

// putReceipt records r, replacing an earlier receipt of the same id and
// keeping the newest maxReceipts. The lock is held.
func (s *Sessions) putReceipt(call Call, tool, state string, fin *Finished, info *SessionInfo) error {
	if call.Op == "" {
		return nil
	}
	rs, err := s.readReceipts()
	if err != nil {
		return err
	}
	rs = slices.DeleteFunc(rs, func(r receipt) bool { return r.ID == call.Op })
	rs = append(rs, receipt{ID: call.Op, Tool: tool, Digest: call.Digest, State: state, Time: time.Now().UTC(), Finished: fin, Info: info})
	// A "started" receipt is never evicted: it is what refuses a retry of a
	// merge that may have run. The oldest "done" ones go first.
	for len(rs) > maxReceipts {
		i := slices.IndexFunc(rs, func(r receipt) bool { return r.State == receiptDone })
		if i < 0 {
			break
		}
		rs = slices.Delete(rs, i, i+1)
	}
	data, err := json.Marshal(rs)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.dir, operationsFile, append(data, '\n')); err != nil {
		return fmt.Errorf("the operation receipt cannot be written: %v", pathErrCause(err))
	}
	return nil
}

// dropReceipt removes the receipt of call. The lock is held. A receipt that
// cannot be rewritten stays, which refuses the retry: the safe side.
func (s *Sessions) dropReceipt(call Call) {
	if call.Op == "" {
		return
	}
	rs, err := s.readReceipts()
	if err != nil {
		return
	}
	rs = slices.DeleteFunc(rs, func(r receipt) bool { return r.ID == call.Op })
	if data, err := json.Marshal(rs); err == nil {
		writeFileAtomic(s.dir, operationsFile, append(data, '\n'))
	}
}
