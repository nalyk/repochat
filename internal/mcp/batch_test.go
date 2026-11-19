package mcp

import (
	"strings"
	"testing"
)

// A batch is answered with one array, but the calls in it are calls like any
// others: while they run the session must go on reading, so that a ping is
// answered, a call can be cancelled, and the end of input is noticed.
func TestBatchedCallsDoNotBlockTheSession(t *testing.T) {
	p := startPipe(testServer())
	p.send(t, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	p.next(t)
	p.send(t, `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wait"}},`+
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"slow","arguments":{"ms":200}}},`+
		`{"jsonrpc":"2.0","id":4,"method":"ping"}]`)
	p.send(t, `{"jsonrpc":"2.0","id":3,"method":"ping"}`)
	if line := p.next(t); strings.HasPrefix(line, "[") || !strings.Contains(line, `"id":3`) {
		t.Fatalf("a ping sent while a batch runs must be answered before the batch is: %s", line)
	}

	// Call 1 waits until it is cancelled. Cancelling it lets the batch end,
	// with no entry for the withdrawn request and the others in order.
	p.send(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
	line := p.next(t)
	if !strings.HasPrefix(line, "[") || strings.Contains(line, `"id":1,`) {
		t.Fatalf("the batch must be answered as one array without the cancelled call: %s", line)
	}
	if two, four := strings.Index(line, `"id":2`), strings.Index(line, `"id":4`); two < 0 || four < 0 || two > four {
		t.Fatalf("the answers must keep the order of the requests: %s", line)
	}
	p.in.Close()
	if err := <-p.done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
}

// The 2025-03-26 lifecycle forbids `initialize` inside a batch. Accepting it
// would let one array renegotiate the session half way through itself.
func TestInitializeInsideABatchIsRefused(t *testing.T) {
	old := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`
	rs := run(t, testServer(), old,
		`[{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}},{"jsonrpc":"2.0","id":2,"method":"ping"}]`,
		`[{"jsonrpc":"2.0","id":3,"method":"ping"}]`)
	one := byID(t, rs, "1")
	if one.Error == nil || one.Error.Code != -32600 {
		t.Fatalf("initialize inside a batch must be refused with -32600: %s", one.raw)
	}
	if byID(t, rs, "2").Error != nil {
		t.Error("the other elements of that batch are answered normally")
	}
	// Had the session been renegotiated to 2025-11-25, this batch would be refused.
	if three := byID(t, rs, "3"); three.Error != nil || !strings.HasPrefix(three.raw, "batch:") {
		t.Errorf("the refused initialize must not change the negotiated revision: %s", three.raw)
	}
}
