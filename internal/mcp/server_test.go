package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

const modernMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

const legacyInit = `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

func testServer() *Server {
	return &Server{
		Name: "test", Version: "9.9", Instructions: "hello",
		Tools: []Tool{
			{Name: "echo", Description: "echo", Schema: Schema{{Name: "text", Kind: String, Required: true, Desc: "text"}},
				Handler: func(_ context.Context, a Args) (Result, error) { return Result{Text: a.String("text")}, nil }},
			{Name: "none", Description: "no params",
				Handler: func(context.Context, Args) (Result, error) { return Result{Text: "ok"}, nil }},
			{Name: "fail", Description: "fails",
				Handler: func(context.Context, Args) (Result, error) { return Result{}, errors.New("nope <b>") }},
			{Name: "boom", Description: "panics",
				Handler: func(context.Context, Args) (Result, error) { panic("kaboom") }},
			{Name: "wait", Description: "blocks until cancelled",
				Handler: func(ctx context.Context, _ Args) (Result, error) {
					<-ctx.Done()
					return Result{}, ctx.Err()
				}},
			{Name: "slow", Description: "sleeps", Schema: Schema{{Name: "ms", Kind: Integer, Desc: "ms"}},
				Handler: func(_ context.Context, a Args) (Result, error) {
					time.Sleep(time.Duration(a.Int("ms", 50)) * time.Millisecond)
					return Result{Text: "slept"}, nil
				}},
		},
	}
}

type reply struct {
	ID     json.RawMessage `json:"id"`
	Result map[string]any  `json:"result"`
	Error  *struct {
		Code    int            `json:"code"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	} `json:"error"`
	raw string
}

// run feeds the lines to a fresh server and returns every reply in order.
func run(t *testing.T, s *Server, lines ...string) []reply {
	t.Helper()
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var replies []reply
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			var batch []json.RawMessage
			if err := json.Unmarshal([]byte(line), &batch); err != nil {
				t.Fatalf("bad batch reply %q: %v", line, err)
			}
			for _, item := range batch {
				var r reply
				if err := json.Unmarshal(item, &r); err != nil {
					t.Fatal(err)
				}
				r.raw = "batch:" + string(item)
				replies = append(replies, r)
			}
			continue
		}
		var r reply
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("reply is not JSON: %q: %v", line, err)
		}
		r.raw = line
		replies = append(replies, r)
	}
	return replies
}

func byID(t *testing.T, replies []reply, id string) reply {
	t.Helper()
	for _, r := range replies {
		if strings.Trim(string(r.ID), `"`) == id {
			return r
		}
	}
	t.Fatalf("no reply with id %s in %d replies", id, len(replies))
	return reply{}
}

func text(r reply) string {
	content, _ := r.Result["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	first, _ := content[0].(map[string]any)
	s, _ := first["text"].(string)
	return s
}

func TestSchemaIsValidJSONSchema(t *testing.T) {
	for _, tool := range testServer().Tools {
		schema := tool.Schema.JSON()
		raw, _ := json.Marshal(schema)
		if bytes.Contains(raw, []byte("null")) {
			t.Errorf("%s: schema contains null: %s", tool.Name, raw)
		}
		if schema["type"] != "object" || schema["additionalProperties"] != false {
			t.Errorf("%s: bad schema %s", tool.Name, raw)
		}
		if req, ok := schema["required"]; ok {
			if list, ok := req.([]string); !ok || len(list) == 0 {
				t.Errorf("%s: required must be a non-empty array when present: %s", tool.Name, raw)
			}
		}
		if _, ok := schema["properties"].(map[string]any); !ok {
			t.Errorf("%s: properties must be an object: %s", tool.Name, raw)
		}
	}
	listed := ToolJSON(testServer().Tools[1])
	ann := listed["annotations"].(map[string]any)
	if ann["readOnlyHint"] != true || ann["destructiveHint"] != false {
		t.Errorf("tools must be annotated read-only: %v", ann)
	}
}

func TestLegacyHandshakeNegotiation(t *testing.T) {
	cases := map[string]string{
		"2025-11-25": "2025-11-25",
		"2025-06-18": "2025-06-18",
		"2024-11-05": "2024-11-05",
		"2099-01-01": "2025-11-25", // unknown versions are never echoed
		"":           "2025-11-25",
		"2026-07-28": "2025-11-25", // the stateless revision has no initialize
	}
	for requested, want := range cases {
		line := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q}}`, requested)
		r := run(t, testServer(), line)[0]
		if r.Error != nil {
			t.Fatalf("%q: %s", requested, r.raw)
		}
		if got := r.Result["protocolVersion"]; got != want {
			t.Errorf("requested %q: negotiated %v, want %s", requested, got, want)
		}
		if _, has := r.Result["resultType"]; has {
			t.Errorf("legacy results must not carry resultType: %s", r.raw)
		}
		info := r.Result["serverInfo"].(map[string]any)
		if info["name"] != "test" || r.Result["instructions"] != "hello" {
			t.Errorf("bad initialize result: %s", r.raw)
		}
	}
}

func TestLegacySession(t *testing.T) {
	rs := run(t, testServer(),
		legacyInit,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo","arguments":{"text":"<a&b>"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"server/discover"}`,
		`{"jsonrpc":"2.0","id":6,"method":"resources/list"}`,
	)
	if len(rs) != 6 {
		t.Fatalf("want 6 replies (notification gets none), got %d", len(rs))
	}
	if tools := byID(t, rs, "3").Result["tools"].([]any); len(tools) != 6 {
		t.Errorf("tools/list returned %d tools", len(tools))
	}
	call := byID(t, rs, "4")
	if text(call) != "<a&b>" || call.Result["isError"] != false {
		t.Errorf("bad call result: %s", call.raw)
	}
	if !strings.Contains(call.raw, "<a&b>") {
		t.Errorf("HTML characters must not be \\u-escaped on the wire: %s", call.raw)
	}
	if r := byID(t, rs, "5"); r.Error == nil || r.Error.Code != -32601 {
		t.Errorf("server/discover in a legacy session: %s", r.raw)
	}
	if r := byID(t, rs, "6"); r.Error == nil || r.Error.Code != -32601 {
		t.Errorf("unknown method: %s", r.raw)
	}
}

func TestRequestsBeforeHandshakeAreRejected(t *testing.T) {
	for _, method := range []string{"tools/list", "tools/call", "server/discover"} {
		r := run(t, testServer(), fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":{"name":"none"}}`, method))[0]
		if r.Error == nil || r.Error.Code != -32602 {
			t.Fatalf("%s without initialize or _meta must be -32602: %s", method, r.raw)
		}
		if _, ok := r.Error.Data["supported"]; !ok {
			t.Errorf("%s: error should list supported versions: %s", method, r.raw)
		}
	}
}

func TestModernEra(t *testing.T) {
	rs := run(t, testServer(),
		`{"jsonrpc":"2.0","id":"d","method":"server/discover","params":{`+modernMeta+`}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{`+modernMeta+`}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"none","arguments":{},`+modernMeta+`}}`,
		`{"jsonrpc":"2.0","id":4,"method":"ping","params":{`+modernMeta+`}}`,
	)
	d := byID(t, rs, "d")
	versions, _ := d.Result["supportedVersions"].([]any)
	if len(versions) == 0 || versions[0] != ModernVersion {
		t.Errorf("discover: %s", d.raw)
	}
	for _, id := range []string{"d", "2", "3"} {
		r := byID(t, rs, id)
		if r.Result["resultType"] != "complete" {
			t.Errorf("%s: modern results need resultType: %s", id, r.raw)
		}
		meta, _ := r.Result["_meta"].(map[string]any)
		if _, ok := meta["io.modelcontextprotocol/serverInfo"]; !ok {
			t.Errorf("%s: modern results should identify the server: %s", id, r.raw)
		}
	}
	list := byID(t, rs, "2")
	if list.Result["cacheScope"] != "private" || list.Result["ttlMs"] == nil {
		t.Errorf("tools/list needs ttlMs and cacheScope: %s", list.raw)
	}
	if r := byID(t, rs, "4"); r.Error == nil || r.Error.Code != -32601 {
		t.Errorf("ping was removed in the modern revision: %s", r.raw)
	}
}

func TestModernMetadataValidation(t *testing.T) {
	cases := []struct {
		name, meta string
		code       int
	}{
		{"missing capabilities", `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}`, -32602},
		{"version not a string", `"_meta":{"io.modelcontextprotocol/protocolVersion":7}`, -32602},
		{"capabilities not an object", `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":"yes"}`, -32602},
		{"capabilities null", `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":null}`, -32602},
		{"unsupported version", `"_meta":{"io.modelcontextprotocol/protocolVersion":"1900-01-01","io.modelcontextprotocol/clientCapabilities":{}}`, -32022},
	}
	for _, tc := range cases {
		r := run(t, testServer(), `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{`+tc.meta+`}}`)[0]
		if r.Error == nil || r.Error.Code != tc.code {
			t.Errorf("%s: want %d, got %s", tc.name, tc.code, r.raw)
			continue
		}
		if tc.code == -32022 {
			if r.Error.Data["requested"] != "1900-01-01" || r.Error.Data["supported"] == nil {
				t.Errorf("%s: error data must carry requested and supported: %s", tc.name, r.raw)
			}
		}
	}
}

func TestToolErrors(t *testing.T) {
	rs := run(t, testServer(), legacyInit,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"missing","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"x","extra":1}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fail"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"boom"}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"none"}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"arguments":{}}}`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"echo","arguments":[1]}}`,
	)
	if r := byID(t, rs, "1"); r.Error == nil || r.Error.Code != -32602 || !strings.Contains(r.Error.Message, "Unknown tool") {
		t.Errorf("unknown tool must be a protocol error: %s", r.raw)
	}
	for _, id := range []string{"2", "3", "4", "5"} {
		r := byID(t, rs, id)
		if r.Error != nil || r.Result["isError"] != true {
			t.Errorf("request %s must be a tool execution error (isError), got %s", id, r.raw)
		}
	}
	if !strings.Contains(text(byID(t, rs, "2")), `missing required argument "text"`) {
		t.Errorf("validation errors must be actionable: %s", byID(t, rs, "2").raw)
	}
	if strings.Contains(byID(t, rs, "5").raw, "kaboom") {
		t.Errorf("panic details must not reach the client")
	}
	if text(byID(t, rs, "6")) != "ok" {
		t.Errorf("server must keep working after a panic: %s", byID(t, rs, "6").raw)
	}
	for _, id := range []string{"7", "8"} {
		if r := byID(t, rs, id); r.Error == nil || r.Error.Code != -32602 {
			t.Errorf("malformed call %s must be -32602: %s", id, r.raw)
		}
	}
}

func TestEnvelopeErrors(t *testing.T) {
	rs := run(t, testServer(), legacyInit,
		`{this is not json`,
		`"a string"`,
		`{"jsonrpc":"2.0","id":null,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":{"a":1},"method":"tools/list"}`,
		`{"jsonrpc":"1.0","id":9,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":10}`,
		`{"jsonrpc":"2.0","id":11,"method":"tools/list","params":[1,2]}`,
		`{"jsonrpc":"2.0","id":12,"result":{}}`,
		`{"jsonrpc":"2.0","method":"some/notification"}`,
	)
	// initialize + 7 errors; the client response and the notification get nothing.
	if len(rs) != 8 {
		t.Fatalf("want 8 replies, got %d: %v", len(rs), rs)
	}
	wantCodes := []int{-32700, -32600, -32600, -32600, -32600, -32600, -32602}
	for i, want := range wantCodes {
		r := rs[i+1]
		if r.Error == nil || r.Error.Code != want {
			t.Errorf("line %d: want code %d, got %s", i+1, want, r.raw)
		}
	}
	for _, i := range []int{1, 2, 3, 4} {
		if string(rs[i].ID) != "null" {
			t.Errorf("reply %d must have a null id: %s", i, rs[i].raw)
		}
	}
}

func TestIDsAreEchoedLosslessly(t *testing.T) {
	rs := run(t, testServer(), legacyInit,
		`{"jsonrpc":"2.0","id":12345678901234567890,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":"1","method":"ping"}`,
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
	)
	got := []string{string(rs[1].ID), string(rs[2].ID), string(rs[3].ID)}
	want := []string{"12345678901234567890", `"1"`, "1"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("id %d: got %s want %s", i, got[i], want[i])
		}
	}
}

func TestBatchingOnlyIn2025_03_26(t *testing.T) {
	batch := `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/x"},{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"none"}}]`
	old := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`
	rs := run(t, testServer(), old, batch)
	if len(rs) != 3 || !strings.HasPrefix(rs[1].raw, "batch:") || text(rs[2]) != "ok" {
		t.Fatalf("2025-03-26 must answer a batch with one array: %v", rs)
	}
	// A batch is bounded, and its calls count against admission together.
	big := "[" + strings.Repeat(`{"jsonrpc":"2.0","id":1,"method":"ping"},`, maxBatch) + `{"jsonrpc":"2.0","id":2,"method":"ping"}]`
	if rs := run(t, testServer(), old, big); rs[1].Error == nil || rs[1].Error.Code != -32600 {
		t.Errorf("an oversized batch must be rejected: %s", rs[1].raw)
	}
	limited := testServer()
	limited.MaxPending = 2
	calls := "[" + strings.TrimSuffix(strings.Repeat(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"none"}},`, 4), ",") + "]"
	rs = run(t, limited, old, fmt.Sprintf(calls, 11, 12, 13, 14))
	busy := 0
	for _, r := range rs[1:] {
		if r.Error != nil && strings.Contains(r.Error.Message, "busy") {
			busy++
		}
	}
	if busy != 2 {
		t.Errorf("calls in one batch must hold their admission until the batch is answered; %d of 4 refused", busy)
	}
	for _, opening := range []string{legacyInit, ""} {
		lines := []string{batch}
		if opening != "" {
			lines = []string{opening, batch}
		}
		rs := run(t, testServer(), lines...)
		last := rs[len(rs)-1]
		if last.Error == nil || last.Error.Code != -32600 || strings.HasPrefix(last.raw, "batch:") {
			t.Errorf("batches outside 2025-03-26 must be rejected with -32600: %s", last.raw)
		}
	}
}

// pipeServer runs a server on pipes so a test can interleave reads and writes.
type pipeServer struct {
	in   *io.PipeWriter
	out  chan string
	done chan error
}

func startPipe(s *Server) *pipeServer {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	p := &pipeServer{in: inW, out: make(chan string, 64), done: make(chan error, 1)}
	go func() {
		p.done <- s.Serve(context.Background(), inR, outW)
		outW.Close()
	}()
	go func() {
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 4096)
		for {
			n, err := outR.Read(tmp)
			buf = append(buf, tmp[:n]...)
			for {
				i := bytes.IndexByte(buf, '\n')
				if i < 0 {
					break
				}
				p.out <- string(buf[:i])
				buf = buf[i+1:]
			}
			if err != nil {
				close(p.out)
				return
			}
		}
	}()
	return p
}

func (p *pipeServer) send(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(p.in, line+"\n"); err != nil {
		t.Fatal(err)
	}
}

func (p *pipeServer) next(t *testing.T) string {
	t.Helper()
	select {
	case line, ok := <-p.out:
		if !ok {
			t.Fatal("server closed its output")
		}
		return line
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a reply")
	}
	return ""
}

func TestCancellationSuppressesTheResponse(t *testing.T) {
	var events []Event
	var mu sync.Mutex
	s := testServer()
	s.OnEvent = func(e Event) { mu.Lock(); events = append(events, e); mu.Unlock() }
	p := startPipe(s)
	p.send(t, legacyInit)
	p.next(t)
	p.send(t, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"wait"}}`)
	p.send(t, `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"none"}}`)
	if line := p.next(t); !strings.Contains(line, `"id":8`) {
		t.Fatalf("a blocked call must not delay other calls; got %s", line)
	}
	p.send(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`)
	p.send(t, `{"jsonrpc":"2.0","id":9,"method":"ping"}`)
	if line := p.next(t); !strings.Contains(line, `"id":9`) {
		t.Fatalf("expected the ping reply, got %s", line)
	}
	p.in.Close()
	if err := <-p.done; err != nil {
		t.Fatal(err)
	}
	for line := range p.out {
		if strings.Contains(line, `"id":7`) {
			t.Fatalf("a cancelled request must get no response, got %s", line)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	cancelled := false
	for _, e := range events {
		if e.Kind == "call" && e.Tool == "wait" && e.Cancelled {
			cancelled = true
		}
	}
	if !cancelled {
		t.Error("the cancellation must be reported to the activity log")
	}
}

func TestDuplicateInflightID(t *testing.T) {
	p := startPipe(testServer())
	p.send(t, legacyInit)
	p.next(t)
	p.send(t, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"wait"}}`)
	p.send(t, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"none"}}`)
	line := p.next(t)
	if !strings.Contains(line, "-32600") || !strings.Contains(line, "still in progress") {
		t.Fatalf("a duplicate in-flight id must be rejected: %s", line)
	}
	p.send(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`)
	p.in.Close()
	<-p.done
}

func TestEOFLetsAdmittedCallsFinish(t *testing.T) {
	rs := run(t, testServer(), legacyInit,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow","arguments":{"ms":150}}}`)
	if len(rs) != 2 || text(byID(t, rs, "1")) != "slept" {
		t.Fatalf("a call admitted before EOF must still be answered: %v", rs)
	}
}

func TestOversizedFrameIsRejectedAndServerContinues(t *testing.T) {
	s := testServer()
	s.MaxMessageBytes = 1024
	big := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"` + strings.Repeat("x", 5000) + `"}}}`
	rs := run(t, s, legacyInit, big, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if len(rs) != 3 || rs[1].Error == nil || rs[1].Error.Code != -32600 {
		t.Fatalf("oversized frame must yield -32600: %v", rs)
	}
	if byID(t, rs, "2").Error != nil {
		t.Fatal("server must keep serving after an oversized frame")
	}
}

func TestPendingBudget(t *testing.T) {
	s := testServer()
	s.MaxPending = 2
	p := startPipe(s)
	p.send(t, legacyInit)
	p.next(t)
	p.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wait"}}`)
	p.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"wait"}}`)
	p.send(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"none"}}`)
	if line := p.next(t); !strings.Contains(line, `"id":3`) || !strings.Contains(line, "busy") {
		t.Fatalf("over-budget call must be refused immediately: %s", line)
	}
	p.send(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
	p.send(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2}}`)
	p.in.Close()
	<-p.done
}

func TestPendingByteBudget(t *testing.T) {
	s := testServer()
	s.MaxPendingBytes = 600
	p := startPipe(s)
	p.send(t, legacyInit)
	p.next(t)
	p.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wait"}}`)
	p.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"`+strings.Repeat("x", 700)+`"}}}`)
	if line := p.next(t); !strings.Contains(line, `"id":2`) || !strings.Contains(line, "busy") {
		t.Fatalf("a call that would exceed the queued-bytes budget must be refused: %s", line)
	}
	p.send(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
	p.in.Close()
	<-p.done
}

// A client that stops reading must not be able to wedge the server: the
// reader loop keeps processing cancellations and the session can be ended.
func TestStalledClientCannotBlockTheServer(t *testing.T) {
	s := testServer()
	s.Tools = append(s.Tools, Tool{Name: "big", Description: "big output",
		Handler: func(context.Context, Args) (Result, error) { return Result{Text: strings.Repeat("x", 1<<20)}, nil }})
	inR, inW := io.Pipe()
	outR, outW := io.Pipe() // nobody reads outR: every write blocks
	defer outR.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, inR, outW) }()
	send := func(line string) {
		t.Helper()
		sent := make(chan struct{})
		go func() { io.WriteString(inW, line+"\n"); close(sent) }()
		select {
		case <-sent:
		case <-time.After(5 * time.Second):
			t.Fatalf("the reader loop is blocked; could not deliver %q", line)
		}
	}
	send(legacyInit)
	for i := 0; i < 20; i++ {
		send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"big"}}`, i+10))
	}
	send(`{"jsonrpc":"2.0","id":99,"method":"tools/list"}`)
	send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":10}}`)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the server did not stop while its client was not reading")
	}
}

// When the client has let the whole output queue fill up, the reader waits
// only briefly and then ends the session by itself: it must not hang until
// someone kills the process.
func TestSaturatedOutputQueueEndsTheSession(t *testing.T) {
	s := testServer()
	s.QueueWait = 150 * time.Millisecond
	inR, inW := io.Pipe()
	outR, outW := io.Pipe() // never read
	defer outR.Close()
	done := make(chan error, 1)
	go func() { done <- s.Serve(context.Background(), inR, outW) }()
	go func() {
		io.WriteString(inW, legacyInit+"\n")
		for i := 0; i < 600; i++ { // more immediate replies than the queue holds
			if _, err := io.WriteString(inW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"ping"}`+"\n", i+10)); err != nil {
				return
			}
		}
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "stopped reading") {
			t.Fatalf("expected the session to end with an output-overload error, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a saturated output queue blocked the server")
	}
	inW.Close()
}

// Results waiting for a client that is not reading keep their admission
// slot, so a stalled client cannot pile up unbounded finished calls.
func TestFinishedCallsWaitingForOutputStayAdmitted(t *testing.T) {
	s := testServer()
	s.MaxPending = 4
	s.QueueWait = 200 * time.Millisecond
	s.Tools = append(s.Tools, Tool{Name: "big", Description: "big output",
		Handler: func(context.Context, Args) (Result, error) { return Result{Text: strings.Repeat("x", 256<<10)}, nil }})
	inR, inW := io.Pipe()
	outR, outW := io.Pipe() // never read
	defer outR.Close()
	done := make(chan error, 1)
	go func() { done <- s.Serve(context.Background(), inR, outW) }()
	go func() {
		io.WriteString(inW, legacyInit+"\n")
		for i := 0; i < 2000; i++ {
			if _, err := io.WriteString(inW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"big"}}`+"\n", i+10)); err != nil {
				return
			}
		}
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "stopped reading") {
			t.Fatalf("expected an output-overload end, got %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the server neither bounded nor ended a session whose client stopped reading")
	}
	s.mu.Lock()
	admitted := len(s.inflight)
	s.mu.Unlock()
	if admitted > 4 {
		t.Errorf("admission limit exceeded: %d calls held", admitted)
	}
	inW.Close()
}

func TestCallTimeout(t *testing.T) {
	s := testServer()
	s.CallTimeout = 80 * time.Millisecond
	rs := run(t, s, legacyInit, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wait"}}`)
	r := byID(t, rs, "1")
	if r.Result["isError"] != true || !strings.Contains(text(r), "timed out") {
		t.Fatalf("a timed-out call must say so: %s", r.raw)
	}
}

func TestWriterFailureStopsTheServer(t *testing.T) {
	s := testServer()
	err := s.Serve(context.Background(), strings.NewReader(legacyInit+"\n"+`{"jsonrpc":"2.0","id":2,"method":"ping"}`+"\n"), failingWriter{})
	if err == nil {
		t.Fatal("Serve must return the write error")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestArgumentCoercion(t *testing.T) {
	schema := Schema{
		{Name: "n", Kind: Integer, Min: 1, Max: 10, Desc: "n"},
		{Name: "flag", Kind: Boolean, Desc: "f"},
		{Name: "list", Kind: StringList, MaxItems: 2, Desc: "l"},
		{Name: "mode", Kind: String, Enum: []string{"a", "b"}, Desc: "m"},
		{Name: "rev", Kind: String, Desc: "r"},
	}
	args, err := schema.Validate(map[string]any{"n": "7", "flag": "true", "list": "one", "mode": "b", "rev": float64(1234567)})
	if err != nil {
		t.Fatal(err)
	}
	if args.Int("n", 0) != 7 || !args.Bool("flag") || len(args.Strings("list")) != 1 || args.String("rev") != "1234567" {
		t.Errorf("coercion failed: %v", args)
	}
	if a, _ := schema.Validate(map[string]any{"n": float64(999)}); a.Int("n", 0) != 10 {
		t.Errorf("out-of-range integers are clamped, got %v", a)
	}
	for name, bad := range map[string]map[string]any{
		"float":     {"n": 1.5},
		"enum":      {"mode": "c"},
		"too many":  {"list": []any{"a", "b", "c"}},
		"bad item":  {"list": []any{1}},
		"bool type": {"flag": 1.0},
		"nul byte":  {"rev": "a\x00b"},
	} {
		if _, err := schema.Validate(bad); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}
