package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestWriteToolsAreAnnotatedAsWrites(t *testing.T) {
	noop := func(context.Context, Args) (Result, error) { return Result{Text: "ok"}, nil }
	s := &Server{Name: "t", Version: "1", Tools: []Tool{
		{Name: "plain", Description: "d", Handler: noop},
		{Name: "write", Description: "d", Write: true, Handler: noop},
		{Name: "destroy", Description: "d", Write: true, Destructive: true, Handler: noop},
	}}
	r := run(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{`+modernMeta+`}}`)[0]
	tools, _ := r.Result["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("want 3 tools, got %d: %s", len(tools), r.raw)
	}
	want := map[string][4]bool{ // readOnly, destructive, idempotent, openWorld
		"plain":   {true, false, true, false},
		"write":   {false, false, false, false},
		"destroy": {false, true, false, false},
	}
	for _, it := range tools {
		tool := it.(map[string]any)
		name := tool["name"].(string)
		ann := tool["annotations"].(map[string]any)
		got := [4]bool{}
		for i, k := range []string{"readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"} {
			b, ok := ann[k].(bool)
			if !ok {
				t.Fatalf("%s: %s missing or not a bool: %v", name, k, ann)
			}
			got[i] = b
		}
		if got != want[name] {
			t.Errorf("%s: annotations %v, want %v", name, got, want[name])
		}
	}
}

func TestObjectArgumentIsAdvertisedAndBounded(t *testing.T) {
	raw := map[string]any{"type": "array"}
	s := Schema{{Name: "changes", Kind: Object, Desc: "the changes", Required: true, MaxBytes: 64, Raw: raw}}

	prop := s.JSON()["properties"].(map[string]any)["changes"]
	want := map[string]any{"type": "array", "description": "the changes"}
	if !reflect.DeepEqual(prop, want) {
		t.Errorf("advertised %v, want %v", prop, want)
	}
	if _, polluted := raw["description"]; polluted {
		t.Error("Param.Raw must not be mutated by JSON()")
	}

	var ok map[string]any
	if err := json.Unmarshal([]byte(`{"changes":[1,2,3]}`), &ok); err != nil {
		t.Fatal(err)
	}
	args, err := s.Validate(ok)
	if err != nil {
		t.Fatalf("small value refused: %v", err)
	}
	if got := string(args.Raw("changes")); got != "[1,2,3]" {
		t.Errorf("Raw round trip = %q", got)
	}
	if _, isRaw := args["changes"].(json.RawMessage); !isRaw {
		t.Errorf("stored as %T, want json.RawMessage", args["changes"])
	}
	if args.Raw("absent") != nil {
		t.Error("Raw of an absent key must be nil")
	}

	big := map[string]any{"changes": []any{strings.Repeat("x", 100)}}
	if _, err := s.Validate(big); err == nil || !strings.Contains(err.Error(), "changes: too large") {
		t.Errorf("oversized value: err = %v, want \"changes: too large\"", err)
	}

	if _, err := s.Validate(map[string]any{}); err == nil {
		t.Error("missing required Object must fail")
	}

	// Default cap is 1 MiB.
	d := Schema{{Name: "v", Kind: Object}}
	if _, err := d.Validate(map[string]any{"v": strings.Repeat("y", 1<<20)}); err == nil {
		t.Error("value over the 1 MiB default must fail")
	}
	if _, err := d.Validate(map[string]any{"v": "small"}); err != nil {
		t.Errorf("small value under default cap: %v", err)
	}

	if sig := s.Signature(); !strings.Contains(sig, "changes: object") {
		t.Errorf("Signature = %q", sig)
	}
}
