package mcp

import "testing"

// One description drives both the schema a client is shown and the checks
// applied to what it sends; the two must mean the same thing.
func TestValidationMatchesTheAdvertisedSchema(t *testing.T) {
	s := Schema{
		{Name: "query", Kind: String, Required: true, MaxLen: 5},
		{Name: "paths", Kind: StringList, MaxLen: 3},
	}
	ok := func(what string, raw map[string]any) {
		t.Helper()
		if _, err := s.Validate(raw); err != nil {
			t.Errorf("%s must be accepted: %v", what, err)
		}
	}
	refused := func(what string, raw map[string]any) {
		t.Helper()
		if _, err := s.Validate(raw); err == nil {
			t.Errorf("%s must be refused", what)
		}
	}
	// JSON Schema's maxLength counts characters, not bytes: five Cyrillic
	// letters are five characters and ten bytes.
	ok("a non-ASCII string within maxLength", map[string]any{"query": "яяяяя"})
	refused("a non-ASCII string over maxLength", map[string]any{"query": "яяяяяя"})
	ok("a non-ASCII list item within maxLength", map[string]any{"query": "a", "paths": []any{"ééé"}})
	refused("a list item over maxLength", map[string]any{"query": "a", "paths": []any{"éééé"}})

	// The schema says a required string is a string of at least one
	// character. A tab is one: searching for it is a legitimate search.
	ok("a whitespace-only required string", map[string]any{"query": "\t"})
	refused("an empty required string", map[string]any{"query": ""})
	props := s.JSON()["properties"].(map[string]any)
	if got := props["query"].(map[string]any)["minLength"]; got != 1 {
		t.Errorf("a required string must be advertised with minLength 1, got %v", got)
	}
	if _, has := props["paths"].(map[string]any)["minLength"]; has {
		t.Error("an optional parameter has no minimum length")
	}
}

// JSON-RPC member names are case-sensitive. A message whose only id is
// spelled "ID" has no id: it is a notification and gets no answer.
func TestEnvelopeMemberNamesAreExact(t *testing.T) {
	rs := run(t, testServer(), legacyInit,
		`{"jsonrpc":"2.0","ID":17,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":18,"Method":"ping"}`,
		`{"jsonrpc":"2.0","id":19,"method":"ping"}`)
	if len(rs) != 3 {
		t.Fatalf("expected answers to init, 18 and 19 only, got %d: %v", len(rs), rs)
	}
	if r := byID(t, rs, "18"); r.Error == nil || r.Error.Code != -32600 {
		t.Errorf(`"Method" is not "method": %s`, r.raw)
	}
	if r := byID(t, rs, "19"); r.Error != nil {
		t.Errorf("a correct message is answered: %s", r.raw)
	}
}
