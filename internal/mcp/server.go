// Package mcp is a small, dependency-free Model Context Protocol server for
// tools-only servers speaking newline-delimited JSON-RPC (the stdio binding).
//
// It is "dual-era": it answers the initialize handshake of the legacy protocol
// revisions (2024-11-05 … 2025-11-25) and the stateless per-request model of
// revision 2026-07-28 (server/discover, _meta protocol version, resultType).
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// ModernVersion is the stateless protocol revision this server implements.
const ModernVersion = "2026-07-28"

// LegacyVersions are the handshake-based revisions, newest first.
var LegacyVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// SupportedVersions lists every revision the server can speak, newest first.
func SupportedVersions() []string {
	return append([]string{ModernVersion}, LegacyVersions...)
}

const (
	metaProtocolVersion = "io.modelcontextprotocol/protocolVersion"
	metaClientInfo      = "io.modelcontextprotocol/clientInfo"
	metaClientCaps      = "io.modelcontextprotocol/clientCapabilities"
	metaServerInfo      = "io.modelcontextprotocol/serverInfo"

	codeParse              = -32700
	codeInvalidRequest     = -32600
	codeMethodNotFound     = -32601
	codeInvalidParams      = -32602
	codeInternal           = -32603
	codeUnsupportedVersion = -32022

	cacheTTLMillis = 3600000
)

// Result is what a tool hands back to the model.
type Result struct {
	Text       string
	Redactions int // secret-looking values removed from Text (for the activity log)
}

// Handler runs one tool call. A returned error is shown to the model as a tool
// execution error (isError: true) so that it can correct itself.
type Handler func(ctx context.Context, args Args) (Result, error)

// Tool is one callable tool.
type Tool struct {
	Name        string
	Title       string
	Description string
	Schema      Schema
	Handler     Handler
}

// Event is reported to Server.OnEvent for the activity log.
type Event struct {
	Time       time.Time
	Kind       string // "hello" (handshake/discovery), "list", "call"
	Client     string
	Protocol   string
	Tool       string
	Args       map[string]any
	Bytes      int
	Duration   time.Duration
	Err        string
	Redactions int
	Cancelled  bool
}

// Server is an MCP server. Configure the exported fields, then call Serve once.
type Server struct {
	Name         string
	Title        string
	Version      string
	Instructions string
	Tools        []Tool

	MaxConcurrent   int           // parallel tool calls (default 8)
	MaxPending      int           // queued + running tool calls (default 256)
	MaxPendingBytes int           // total request bytes held by those calls (default 16 MiB)
	CallTimeout     time.Duration // per tool call (default 60s)
	MaxMessageBytes int           // per inbound message (default 4 MiB)
	DrainTimeout    time.Duration // wait for admitted calls after EOF (default 5s)

	OnEvent func(Event)
	Logf    func(format string, args ...any)

	byName map[string]*Tool
	listed []any

	wmu  sync.Mutex
	w    io.Writer
	werr error

	mu       sync.Mutex
	inflight map[string]*call
	pending  int // request bytes held by admitted calls
	client   string
	legacy   string // negotiated legacy revision; "" until initialize is received
	sem      chan struct{}
	wg       sync.WaitGroup
}

type call struct {
	cancel    context.CancelFunc
	cancelled bool
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  *string         `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

var nullID = json.RawMessage("null")

// outcome of processing one message: an immediate response, deferred work
// (a tool call) that yields a response later, or nothing (notifications).
type outcome struct {
	resp  *response
	async func() *response
}

func (s *Server) init(w io.Writer) {
	s.w = w
	if s.MaxConcurrent <= 0 {
		s.MaxConcurrent = 8
	}
	if s.MaxPending <= 0 {
		s.MaxPending = 256
	}
	if s.MaxPendingBytes <= 0 {
		s.MaxPendingBytes = 16 << 20
	}
	if s.CallTimeout <= 0 {
		s.CallTimeout = 60 * time.Second
	}
	if s.MaxMessageBytes <= 0 {
		s.MaxMessageBytes = 4 << 20
	}
	if s.DrainTimeout <= 0 {
		s.DrainTimeout = 5 * time.Second
	}
	s.sem = make(chan struct{}, s.MaxConcurrent)
	s.inflight = map[string]*call{}
	s.byName = map[string]*Tool{}
	s.listed = nil
	for i := range s.Tools {
		t := &s.Tools[i]
		s.byName[t.Name] = t
		s.listed = append(s.listed, ToolJSON(*t))
	}
}

// ToolJSON renders a tool definition as advertised by tools/list.
func ToolJSON(t Tool) map[string]any {
	title := t.Title
	if title == "" {
		title = t.Name
	}
	return map[string]any{
		"name":        t.Name,
		"title":       title,
		"description": t.Description,
		"inputSchema": t.Schema.JSON(),
		"annotations": map[string]any{
			"title":           title,
			"readOnlyHint":    true,
			"destructiveHint": false,
			"idempotentHint":  true,
			"openWorldHint":   false,
		},
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *Server) emit(e Event) {
	if s.OnEvent != nil {
		e.Time = time.Now()
		s.OnEvent(e)
	}
}

type inbound struct {
	line    []byte
	tooLong bool
	err     error
}

// Serve reads messages from in and writes responses to out until in reaches
// EOF (then it lets running calls finish and returns nil), ctx is cancelled,
// or out fails.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.init(out)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	lines := make(chan inbound)
	go func() {
		br := bufio.NewReaderSize(in, 64<<10)
		for {
			line, tooLong, err := readLine(br, s.MaxMessageBytes)
			select {
			case lines <- inbound{line, tooLong, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg := <-lines:
			if msg.err != nil {
				s.drain()
				if errors.Is(msg.err, io.EOF) {
					return s.writeError()
				}
				return msg.err
			}
			if msg.tooLong {
				s.write(errResp(nullID, codeInvalidRequest,
					fmt.Sprintf("Invalid Request: message exceeds %d bytes", s.MaxMessageBytes), nil))
			} else {
				s.handleLine(ctx, msg.line)
			}
			if err := s.writeError(); err != nil {
				return err
			}
		}
	}
}

func (s *Server) drain() {
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(s.DrainTimeout):
		s.logf("shutdown: gave up waiting for running tool calls")
	}
}

// readLine returns the next newline-terminated line. A line longer than max
// is consumed to its end and reported with tooLong=true instead of growing
// memory without bound.
func readLine(br *bufio.Reader, max int) (line []byte, tooLong bool, err error) {
	for {
		chunk, e := br.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > max {
				tooLong, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		switch {
		case e == nil:
			return line, tooLong, nil
		case errors.Is(e, bufio.ErrBufferFull):
			continue
		case errors.Is(e, io.EOF) && (len(line) > 0 || tooLong):
			return line, tooLong, nil // final line without a trailing newline
		default:
			return nil, false, e
		}
	}
}

func (s *Server) handleLine(ctx context.Context, raw []byte) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return
	}
	if raw[0] == '[' {
		s.handleBatch(ctx, raw)
		return
	}
	o := s.process(ctx, raw)
	if o.resp != nil {
		s.write(o.resp)
	}
	if o.async != nil {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if r := o.async(); r != nil {
				s.write(r)
			}
		}()
	}
}

// handleBatch supports JSON-RPC batches, which exist only in revision
// 2025-03-26: every element is processed in order and the responses are sent
// as one array. Under any other revision a batch is an invalid request.
func (s *Server) handleBatch(ctx context.Context, raw []byte) {
	if s.legacyVersion() != "2025-03-26" {
		s.write(errResp(nullID, codeInvalidRequest,
			"Invalid Request: JSON-RPC batching is only available after negotiating protocol revision 2025-03-26", nil))
		return
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		s.write(errResp(nullID, codeParse, "Parse error: "+err.Error(), nil))
		return
	}
	if len(items) == 0 {
		s.write(errResp(nullID, codeInvalidRequest, "Invalid Request: empty batch", nil))
		return
	}
	var out []*response
	for _, item := range items {
		o := s.process(ctx, item)
		if o.resp != nil {
			out = append(out, o.resp)
		}
		if o.async != nil {
			if r := o.async(); r != nil {
				out = append(out, r)
			}
		}
	}
	if len(out) > 0 {
		s.writeValue(out)
	}
}

func (s *Server) process(ctx context.Context, raw []byte) outcome {
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		var syn *json.SyntaxError
		if errors.As(err, &syn) || errors.Is(err, io.ErrUnexpectedEOF) {
			return outcome{resp: errResp(nullID, codeParse, "Parse error: "+err.Error(), nil)}
		}
		return outcome{resp: errResp(nullID, codeInvalidRequest, "Invalid Request: a JSON-RPC object is required", nil)}
	}
	idKind := classifyID(req.ID)
	if req.Method == nil {
		if len(req.Result) > 0 || len(req.Error) > 0 {
			return outcome{} // a response from the client; this server never sends requests
		}
		return outcome{resp: errResp(replyID(req.ID, idKind), codeInvalidRequest, "Invalid Request: method is required", nil)}
	}
	method := *req.Method
	if idKind == idAbsent {
		s.notification(method, req.Params)
		return outcome{}
	}
	if idKind == idInvalid {
		return outcome{resp: errResp(nullID, codeInvalidRequest, "Invalid Request: id must be a string or a number", nil)}
	}
	if req.JSONRPC != "2.0" {
		return outcome{resp: errResp(req.ID, codeInvalidRequest, `Invalid Request: jsonrpc must be "2.0"`, nil)}
	}

	params, err := parseObject(req.Params)
	if err != nil {
		return outcome{resp: errResp(req.ID, codeInvalidParams, "Invalid params: params must be an object", nil)}
	}
	meta, _ := parseObject(params["_meta"])

	// Era selection. Modern (stateless) semantics apply when the request names
	// revision 2026-07-28 in _meta. Legacy semantics apply only once this
	// process has received initialize. Anything else is rejected rather than
	// guessed at.
	modern := false
	protocol := s.legacyVersion()
	rawVersion, hasVersion := meta[metaProtocolVersion]
	if hasVersion {
		var v string
		if json.Unmarshal(rawVersion, &v) != nil {
			return outcome{resp: errResp(req.ID, codeInvalidParams, "Invalid params: _meta protocol version must be a string", nil)}
		}
		switch {
		case v == ModernVersion:
			modern, protocol = true, v
			if _, ok := meta[metaClientCaps]; !ok {
				return outcome{resp: errResp(req.ID, codeInvalidParams,
					"Invalid params: missing required _meta field "+metaClientCaps, nil)}
			}
		case protocol != "" || method == "initialize":
			// Legacy session: the handshake result governs, not this key.
		default:
			return outcome{resp: errResp(req.ID, codeUnsupportedVersion, "Unsupported protocol version",
				map[string]any{"supported": SupportedVersions(), "requested": v})}
		}
	}
	if !modern && protocol == "" && method != "initialize" && method != "ping" {
		return outcome{resp: errResp(req.ID, codeInvalidParams,
			"Invalid params: send initialize first (legacy revisions) or set _meta["+metaProtocolVersion+"] on every request",
			map[string]any{"supported": SupportedVersions()})}
	}
	client := clientName(meta[metaClientInfo])

	switch method {
	case "initialize":
		return outcome{resp: s.initialize(req)}
	case "ping":
		if modern {
			return outcome{resp: errResp(req.ID, codeMethodNotFound, "Method not found: ping was removed in "+ModernVersion, nil)}
		}
		return outcome{resp: s.result(req.ID, false, map[string]any{})}
	case "server/discover":
		if !modern {
			return outcome{resp: errResp(req.ID, codeMethodNotFound,
				"Method not found: server/discover belongs to revision "+ModernVersion+"; this session negotiated "+protocol, nil)}
		}
		s.emit(Event{Kind: "hello", Client: client, Protocol: protocol})
		return outcome{resp: s.result(req.ID, true, map[string]any{
			"supportedVersions": SupportedVersions(),
			"capabilities":      s.capabilities(),
			"instructions":      s.Instructions,
			"ttlMs":             cacheTTLMillis,
			"cacheScope":        "private",
		})}
	case "tools/list":
		s.emit(Event{Kind: "list", Client: s.clientOr(client), Protocol: protocol})
		res := map[string]any{"tools": s.listed}
		if modern {
			res["ttlMs"] = cacheTTLMillis
			res["cacheScope"] = "private"
		}
		return outcome{resp: s.result(req.ID, modern, res)}
	case "tools/call":
		return s.toolsCall(ctx, req, params, modern, s.clientOr(client), protocol, len(raw))
	default:
		return outcome{resp: errResp(req.ID, codeMethodNotFound, "Method not found: "+method, nil)}
	}
}

func (s *Server) notification(method string, rawParams json.RawMessage) {
	if method != "notifications/cancelled" {
		return
	}
	params, _ := parseObject(rawParams)
	id := bytes.TrimSpace(params["requestId"])
	if len(id) == 0 {
		return
	}
	s.mu.Lock()
	if c := s.inflight[string(id)]; c != nil {
		c.cancelled = true
		c.cancel()
	}
	s.mu.Unlock()
}

func (s *Server) capabilities() map[string]any {
	return map[string]any{"tools": map[string]any{"listChanged": false}}
}

func (s *Server) serverInfo() map[string]any {
	info := map[string]any{"name": s.Name, "version": s.Version}
	if s.Title != "" {
		info["title"] = s.Title
	}
	return info
}

func (s *Server) initialize(req request) *response {
	var p struct {
		ProtocolVersion string          `json:"protocolVersion"`
		ClientInfo      json.RawMessage `json:"clientInfo"`
	}
	_ = json.Unmarshal(req.Params, &p)
	version := LegacyVersions[0]
	if isLegacy(p.ProtocolVersion) {
		version = p.ProtocolVersion
	}
	client := clientName(p.ClientInfo)
	s.mu.Lock()
	s.client = client
	s.legacy = version
	s.mu.Unlock()
	s.emit(Event{Kind: "hello", Client: client, Protocol: version})
	return s.result(req.ID, false, map[string]any{
		"protocolVersion": version,
		"capabilities":    s.capabilities(),
		"serverInfo":      s.serverInfo(),
		"instructions":    s.Instructions,
	})
}

func (s *Server) legacyVersion() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.legacy
}

func (s *Server) clientOr(perRequest string) string {
	if perRequest != "" {
		return perRequest
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

func (s *Server) toolsCall(ctx context.Context, req request, params map[string]json.RawMessage, modern bool, client, protocol string, size int) outcome {
	var name string
	if raw, ok := params["name"]; !ok || json.Unmarshal(raw, &name) != nil || name == "" {
		return outcome{resp: errResp(req.ID, codeInvalidParams, "Invalid params: tools/call requires a string \"name\"", nil)}
	}
	tool := s.byName[name]
	if tool == nil {
		return outcome{resp: errResp(req.ID, codeInvalidParams, "Unknown tool: "+name, nil)}
	}
	rawArgs := map[string]any{}
	if raw, ok := params["arguments"]; ok && string(bytes.TrimSpace(raw)) != "null" {
		if err := json.Unmarshal(raw, &rawArgs); err != nil {
			return outcome{resp: errResp(req.ID, codeInvalidParams, "Invalid params: \"arguments\" must be an object", nil)}
		}
	}

	key := string(req.ID)
	callCtx, cancel := context.WithTimeout(ctx, s.CallTimeout)
	c := &call{cancel: cancel}
	s.mu.Lock()
	if _, dup := s.inflight[key]; dup {
		s.mu.Unlock()
		cancel()
		return outcome{resp: errResp(req.ID, codeInvalidRequest, "Invalid Request: a request with this id is still in progress", nil)}
	}
	if len(s.inflight) >= s.MaxPending || (len(s.inflight) > 0 && s.pending+size > s.MaxPendingBytes) {
		s.mu.Unlock()
		cancel()
		return outcome{resp: errResp(req.ID, codeInternal, "Server busy: too many tool calls in progress", nil)}
	}
	s.inflight[key] = c
	s.pending += size
	s.mu.Unlock()

	return outcome{async: func() *response {
		defer func() {
			cancel()
			s.mu.Lock()
			delete(s.inflight, key)
			s.pending -= size
			s.mu.Unlock()
		}()
		ev := Event{Kind: "call", Client: client, Protocol: protocol, Tool: name, Args: rawArgs}
		start := time.Now()
		text, isErr := s.runTool(callCtx, tool, rawArgs, &ev)
		ev.Duration = time.Since(start)
		ev.Bytes = len(text)

		s.mu.Lock()
		cancelled := c.cancelled
		s.mu.Unlock()
		if cancelled {
			// The client withdrew the request: no response may be sent for it.
			ev.Cancelled = true
			s.emit(ev)
			return nil
		}
		s.emit(ev)
		return s.result(req.ID, modern, map[string]any{
			"content": []map[string]string{{"type": "text", "text": text}},
			"isError": isErr,
		})
	}}
}

// runTool validates arguments, waits for a worker slot and runs the handler.
// It never panics and always returns text for the model.
func (s *Server) runTool(ctx context.Context, tool *Tool, rawArgs map[string]any, ev *Event) (text string, isErr bool) {
	args, err := tool.Schema.Validate(rawArgs)
	if err != nil {
		ev.Err = err.Error()
		return fmt.Sprintf("Invalid arguments for %s: %s", tool.Name, err), true
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		ev.Err = "cancelled before start"
		return tool.Name + " was cancelled or timed out before it could start", true
	}
	defer func() {
		if r := recover(); r != nil {
			s.logf("panic in tool %s: %v\n%s", tool.Name, r, debug.Stack())
			ev.Err = fmt.Sprint("panic: ", r)
			text, isErr = fmt.Sprintf("%s failed with an internal error; the server is still running", tool.Name), true
		}
	}()
	res, err := tool.Handler(ctx, args)
	if err != nil {
		msg := err.Error()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			msg = fmt.Sprintf("%s timed out after %s; narrow the request (smaller range, path filter, fewer results)", tool.Name, s.CallTimeout)
		}
		ev.Err = msg
		return msg, true
	}
	ev.Redactions = res.Redactions
	if res.Text == "" {
		res.Text = "(empty result)"
	}
	return res.Text, false
}

// result builds a success response. Modern-era results carry resultType and
// identify the server in _meta, as revision 2026-07-28 requires.
func (s *Server) result(id json.RawMessage, modern bool, body map[string]any) *response {
	if modern {
		body["resultType"] = "complete"
		body["_meta"] = map[string]any{metaServerInfo: s.serverInfo()}
	}
	raw, err := encode(body)
	if err != nil {
		return errResp(id, codeInternal, "Internal error: cannot encode result", nil)
	}
	return &response{JSONRPC: "2.0", ID: id, Result: bytes.TrimSpace(raw)}
}

func errResp(id json.RawMessage, code int, msg string, data any) *response {
	if len(id) == 0 {
		id = nullID
	}
	return &response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg, Data: data}}
}

func (s *Server) write(r *response) { s.writeValue(r) }

func (s *Server) writeValue(v any) {
	raw, err := encode(v)
	if err != nil {
		s.logf("cannot encode response: %v", err)
		return
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.werr != nil {
		return
	}
	if _, err := s.w.Write(raw); err != nil {
		s.werr = err
	}
}

func (s *Server) writeError() error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.werr
}

// encode marshals v as one line of JSON without HTML escaping (source code is
// full of <, > and &, which the default encoder would triple in size).
func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

const (
	idAbsent = iota
	idValid
	idInvalid
)

func classifyID(id json.RawMessage) int {
	id = bytes.TrimSpace(id)
	if len(id) == 0 {
		return idAbsent
	}
	if c := id[0]; c == '"' || c == '-' || (c >= '0' && c <= '9') {
		return idValid
	}
	return idInvalid // null, objects, arrays, booleans
}

func replyID(id json.RawMessage, kind int) json.RawMessage {
	if kind == idValid {
		return id
	}
	return nullID
}

func parseObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return out, nil
	}
	if err := json.Unmarshal(trimmed, &out); err != nil {
		return map[string]json.RawMessage{}, err
	}
	return out, nil
}

func isLegacy(v string) bool {
	for _, l := range LegacyVersions {
		if l == v {
			return true
		}
	}
	return false
}

func clientName(raw json.RawMessage) string {
	var info struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &info) != nil {
		return ""
	}
	name := strings.TrimSpace(info.Name + " " + info.Version)
	if len(name) > 80 {
		name = name[:80]
	}
	return name
}
