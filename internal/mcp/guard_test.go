package mcp

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
)

const guardNotice = "the owner changed this repository's guidance"

// guarded is testServer with a guard that fails once fail is set, recording
// every method it was asked about.
func guarded(fail *bool) (*Server, *[]string) {
	s := testServer()
	var mu sync.Mutex
	var seen []string
	s.Guard = func(_ context.Context, method string) error {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, method)
		if *fail {
			return errors.New(guardNotice)
		}
		return nil
	}
	return s, &seen
}

func TestGuardRefusesInstructionBearingResponses(t *testing.T) {
	fail := true
	s, seen := guarded(&fail)
	replies := run(t, s,
		legacyInit,
		`{"jsonrpc":"2.0","id":2,"method":"server/discover","params":{`+modernMeta+`}}`,
	)
	for _, id := range []string{"init", "2"} {
		r := byID(t, replies, id)
		if r.Error == nil || r.Error.Code != codeInternal || !strings.Contains(r.Error.Message, guardNotice) {
			t.Errorf("%s: want error -32603 with the notice, got %s", id, r.raw)
		}
		if strings.Contains(r.raw, "hello") {
			t.Errorf("%s: the instructions were sent: %s", id, r.raw)
		}
	}
	if got := strings.Join(*seen, ","); got != "initialize,server/discover" {
		t.Errorf("guard saw %q", got)
	}
}

func TestGuardRefusesToolCallsAsToolErrors(t *testing.T) {
	fail := false
	s, seen := guarded(&fail)
	ran := false
	s.Tools[1].Handler = func(context.Context, Args) (Result, error) { ran = true; return Result{Text: "ok"}, nil }
	fail = true
	replies := run(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"none",`+modernMeta+`}}`)
	r := byID(t, replies, "1")
	if r.Error != nil || r.Result["isError"] != true || !strings.Contains(text(r), guardNotice) {
		t.Fatalf("want an isError result with the notice, got %s", r.raw)
	}
	if ran {
		t.Error("the tool ran despite the guard")
	}
	if got := strings.Join(*seen, ","); got != "tools/call" {
		t.Errorf("guard saw %q", got)
	}
}

func TestGuardPassingChangesNothing(t *testing.T) {
	lines := []string{
		legacyInit,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"x"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"server/discover","params":{` + modernMeta + `}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{` + modernMeta + `}}`,
	}
	plain := run(t, testServer(), lines...)
	fail := false
	s, seen := guarded(&fail)
	withGuard := run(t, s, lines...)
	if len(plain) != len(withGuard) {
		t.Fatalf("%d replies vs %d", len(plain), len(withGuard))
	}
	for _, id := range []string{"init", "1", "2", "3"} {
		if a, b := byID(t, plain, id).raw, byID(t, withGuard, id).raw; a != b {
			t.Errorf("reply %s differs:\n%s\n%s", id, a, b)
		}
	}
	got := append([]string(nil), *seen...)
	sort.Strings(got)
	if strings.Join(got, ",") != "initialize,server/discover,tools/call" {
		t.Errorf("guard saw %q (tools/list carries no instructions)", got)
	}
}
