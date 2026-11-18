package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRealTunnelClientContract runs repochat with the REAL tunnel-client
// binary against a local mock of OpenAI's tunnel control plane (the wire
// protocol documented in tunnel-client's docs/protocol.md). It proves what the
// stand-in used by the other tests cannot: that the real client accepts
// repochat's command line, starts the MCP server through its own argv parser,
// passes its initialization gate and carries tool calls end to end.
//
// It needs no OpenAI account. It is opt-in locally:
//
//	REPOCHAT_REAL_TUNNEL_CLIENT=/path/to/tunnel-client go test -run RealTunnelClient ./internal/cli/
//
// and mandatory for releases: with REPOCHAT_REQUIRE_TUNNEL_CLIENT=1 a missing
// binary is a failure, not a skip.
func TestRealTunnelClientContract(t *testing.T) {
	bin := os.Getenv("REPOCHAT_REAL_TUNNEL_CLIENT")
	if bin == "" {
		if os.Getenv("REPOCHAT_REQUIRE_TUNNEL_CLIENT") == "1" {
			t.Fatal("REPOCHAT_REQUIRE_TUNNEL_CLIENT=1 but REPOCHAT_REAL_TUNNEL_CLIENT is not set: the contract suite must run for a release")
		}
		t.Skip("set REPOCHAT_REAL_TUNNEL_CLIENT=/path/to/tunnel-client to run the contract suite against the real binary")
	}
	e := setup(t, "")
	t.Setenv("REPOCHAT_TUNNEL_CLIENT", bin)
	saveProfile(t, testKey)

	type command struct {
		id      string
		jsonrpc string
	}
	script := []command{
		{"req_init", `{"jsonrpc":"2.0","id":"rpc_init","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"contract-suite","version":"1"}}}`},
		{"req_list", `{"jsonrpc":"2.0","id":"rpc_list","method":"tools/list","params":{}}`},
		{"req_call", `{"jsonrpc":"2.0","id":"rpc_call","method":"tools/call","params":{"name":"repo_overview","arguments":{}}}`},
		{"req_read", `{"jsonrpc":"2.0","id":"rpc_read","method":"tools/call","params":{"name":"read_file","arguments":{"path":".env"}}}`},
	}
	var mu sync.Mutex
	next := 0
	outstanding := false
	responses := map[string]string{}
	var problems []string
	note := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/tunnels/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			note("%s %s: wrong or missing bearer token", r.Method, r.URL.Path)
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/v1/tunnels/"+testTunnel)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && rest == "":
			fmt.Fprintf(w, `{"id":%q,"name":"Contract tunnel","description":"mock","organization_ids":["org_1"],"workspace_ids":["ws_1"]}`, testTunnel)
		case r.Method == http.MethodGet && rest == "/poll":
			if outstanding || next >= len(script) {
				mu.Unlock()
				time.Sleep(200 * time.Millisecond)
				mu.Lock()
				w.WriteHeader(http.StatusNoContent)
				return
			}
			c := script[next]
			next++
			outstanding = true
			fmt.Fprintf(w, `{"commands":[{"request_id":%q,"shard_token":"shard-%s","command_type":"jsonrpc","channel":"main","created_at":%q,"response_timeout":"30s","headers":{},"jsonrpc":%s}]}`,
				c.id, c.id, time.Now().UTC().Format(time.RFC3339), c.jsonrpc)
		case r.Method == http.MethodPost && rest == "/response":
			body, _ := io.ReadAll(io.LimitReader(r.Body, 16<<20))
			var resp struct {
				RequestID string          `json:"request_id"`
				RespType  string          `json:"resp_type"`
				RespJSON  json.RawMessage `json:"resp_json"`
			}
			if err := json.Unmarshal(body, &resp); err != nil {
				note("malformed response body: %v", err)
			}
			if r.Header.Get("X-Tunnel-Shard-Token") != "shard-"+resp.RequestID {
				note("response for %s lacks its shard token header", resp.RequestID)
			}
			if resp.RespType == "" || resp.RespType == "jsonrpc_response" {
				responses[resp.RequestID] = string(resp.RespJSON)
				outstanding = false
			}
			io.WriteString(w, `{"status":"ok"}`)
		default:
			// Optional endpoints (e.g. managed Cloudflare runtime) are not part of this setup.
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("REPOCHAT_CONTROL_PLANE_URL", srv.URL)

	// The real client's own commands, as the wizard and doctor use them.
	text, v, err := helperVersion(context.Background(), bin)
	if err != nil || v == [3]int{} {
		t.Fatalf("cannot read the real tunnel-client's version: %q %v", text, err)
	}
	if versionOlder(v, helperMin) {
		t.Fatalf("tunnel-client %s is older than the supported minimum", text)
	}
	info, err := verifyTunnel(context.Background(), bin, testTunnel, testKey)
	if err != nil {
		t.Fatalf("tunnel lookup through the real client failed: %v", err)
	}
	if info.Name != "Contract tunnel" || !info.known || len(info.Workspaces) != 1 {
		t.Errorf("tunnel metadata was not understood: %+v", info)
	}
	if _, err := verifyTunnel(context.Background(), bin, testTunnel, "sk-wrong-0123456789abcdefghij"); err == nil {
		t.Error("a rejected key must fail the lookup")
	} else if strings.Contains(err.Error(), "sk-wrong") {
		t.Errorf("the key leaked into the error: %v", err)
	}
	problems = nil // the rejected lookup above is expected
	_, doctorOut, code := runCLI("doctor", "--root", e.repo)
	if code != 0 {
		t.Errorf("doctor with the real client:\n%s", doctorOut)
	}
	wantIn(t, doctorOut, "✓ tunnel-client", "✓ OpenAI: tunnel lookup", "Contract tunnel")

	_, errb, done := start("--root", e.repo)
	defer func() {
		runCLI("stop")
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("the launcher did not stop")
		}
	}()
	waitFor(t, "all four commands to be answered through the real tunnel-client", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(responses) == len(script)
	})
	mu.Lock()
	defer mu.Unlock()
	for _, p := range problems {
		t.Error(p)
	}
	wantIn(t, responses["req_init"], `"protocolVersion":"2025-11-25"`, `"name":"repochat"`)
	wantIn(t, responses["req_list"], `"name":"search_code"`, `"readOnlyHint":true`)
	wantIn(t, responses["req_call"], "REPOSITORY project", `"isError":false`)
	wantIn(t, responses["req_read"], "withheld", `"isError":true`)
	if strings.Contains(responses["req_read"], "NOPE") {
		t.Error("withheld content crossed the tunnel")
	}
	log := errb.String()
	t.Logf("launcher output with the real tunnel-client:\n%s", log)
	wantIn(t, log, "is running", "connected to OpenAI", "remote MCP activity detected", "contract-suite", "repo_overview")
	if strings.Contains(log, testKey) {
		t.Error("the API key was printed")
	}
}
