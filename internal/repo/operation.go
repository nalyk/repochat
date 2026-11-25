package repo

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Call is what a mutating call says about itself, beyond its arguments.
type Call struct {
	// Op is the operation id the caller chose for this call; "" for none.
	Op string
	// Digest is the digest of the call's arguments, recorded with Op.
	Digest string
	// Expect is the checkpoint the session must be at, as a hexadecimal
	// prefix of 7 to 40 characters of its ID; "" for no check.
	Expect string
}

var (
	// OperationIDRe is the form of an operation id.
	OperationIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)
	// ExpectRe is the form of an expected checkpoint.
	ExpectRe = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)
)

// validateCall refuses a Call whose fields are not in their forms. It reads
// no session state, so it runs before the lock is taken.
func validateCall(call Call) error {
	if call.Expect != "" && !ExpectRe.MatchString(call.Expect) {
		return fmt.Errorf("the expected checkpoint %q is not a checkpoint ID: give 7 to 40 hexadecimal characters of the ID that session_status shows (nothing was changed)", SanitizeLine(truncate(call.Expect, 40)))
	}
	if call.Op != "" {
		if !OperationIDRe.MatchString(call.Op) {
			return fmt.Errorf("the operation_id %q is not an operation id: give 8 to 64 letters, digits, dots, underscores or hyphens (nothing was changed)", SanitizeLine(truncate(call.Op, 40)))
		}
		if call.Digest == "" || strings.ContainsFunc(call.Digest, unicode.IsSpace) {
			return errors.New("an operation id needs a digest of the call's arguments without whitespace (nothing was changed)")
		}
	}
	return nil
}

// checkExpected refuses a call made for a session that is no longer at the
// checkpoint it names: head is the session branch's head (the full ID),
// expect the hexadecimal prefix the caller gave, or "" for none.
func checkExpected(head, expect string) error {
	if expect == "" || strings.HasPrefix(head, strings.ToLower(expect)) {
		return nil
	}
	return refusal("", "the session moved: it is at checkpoint %s, not %s; another conversation may be editing it. Call session_status, read the files again and retry with the new checkpoint", head[:12], SanitizeLine(expect))
}

// replayOperation answers a call whose operation id the session has already
// recorded: the same arguments give the earlier result (Replayed, nothing is
// written), other arguments are refused. It returns nil, nil when the call
// has no operation id or the id is new. chain is the session's checkpoints,
// newest first, and the lock is held.
func (s *Sessions) replayOperation(ctx context.Context, chain []string, call Call) (*Applied, error) {
	if call.Op == "" {
		return nil, nil
	}
	cp, digest, found, err := s.findOperation(ctx, chain, call.Op)
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
func (s *Sessions) findOperation(ctx context.Context, chain []string, op string) (cp *Checkpoint, digest string, found bool, err error) {
	if len(chain) < 2 {
		return nil, "", false, nil
	}
	ws := s.reader()
	out, err := ws.git(ctx, 64<<20, "log", "--first-parent", "--no-color", "--format=%H%x1f%B%x1e", chain[0], "^"+chain[len(chain)-1])
	if err != nil {
		return nil, "", false, err
	}
	for _, rec := range strings.Split(string(out), "\x1e") {
		id, msg, ok := strings.Cut(strings.TrimLeft(rec, "\n"), "\x1f")
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
		pol, err := ws.Policy(ctx)
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
