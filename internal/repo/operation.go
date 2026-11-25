package repo

import (
	"fmt"
	"regexp"
	"strings"
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
