package cli

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"repochat/internal/repo"
)

// The test binary plays three roles: the tests themselves, a stand-in for
// tunnel-client (REPOCHAT_TEST_ROLE=tunnel-client), and `repochat mcp` when
// that stand-in starts the MCP server the way the real tunnel-client does.
func TestMain(m *testing.M) {
	switch {
	case os.Getenv("REPOCHAT_TEST_ROLE") == "tunnel-client":
		os.Exit(fakeTunnelClient(os.Args[1:]))
	case len(os.Args) > 1 && os.Args[1] == "mcp":
		os.Exit(Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

const (
	testTunnel = "tunnel_0123456789abcdef0123456789abcdef"
	testKey    = "sk-test-0123456789abcdefghijklmnop"
)

// upstreamSplit mirrors how tunnel-client v0.0.21 splits --mcp.command
// (pkg/runtimeconfig parseCommandArgv): single quotes are literal, double
// quotes allow backslash escapes, a backslash outside quotes escapes the next
// byte, unquoted whitespace separates words.
func upstreamSplit(input string) ([]string, error) {
	input = strings.TrimSpace(input)
	var args []string
	var b strings.Builder
	inSingle, inDouble, escaped, have := false, false, false, false
	for i := 0; i < len(input); i++ {
		ch := input[i]
		switch {
		case escaped:
			b.WriteByte(ch)
			escaped = false
		case inSingle:
			if ch == '\'' {
				inSingle = false
			} else {
				b.WriteByte(ch)
			}
		case inDouble:
			switch ch {
			case '\\':
				escaped = true
			case '"':
				inDouble = false
			default:
				b.WriteByte(ch)
			}
		case ch == '\\':
			escaped, have = true, true
		case ch == '\'':
			inSingle, have = true, true
		case ch == '"':
			inDouble, have = true, true
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r':
			if b.Len() > 0 || have {
				args = append(args, b.String())
				b.Reset()
				have = false
			}
		default:
			b.WriteByte(ch)
		}
	}
	if escaped || inSingle || inDouble {
		return nil, fmt.Errorf("unterminated quote or escape")
	}
	if b.Len() > 0 || have {
		args = append(args, b.String())
	}
	return args, nil
}

type fakeRecord struct {
	Argv      []string          `json:"argv"`
	Env       map[string]string `json:"env"`
	ChildArgv []string          `json:"child_argv"`
	Responses map[string]string `json:"responses"`
	LockFD    bool              `json:"lock_fd"`
}

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// fakeTunnelClient behaves like tunnel-client as far as repochat can
// observe: --version, `admin tunnels get`, and `run` with a health endpoint
// and a stdio MCP child that it drives the way a remote caller would.
func fakeTunnelClient(args []string) int {
	mode := os.Getenv("FAKE_MODE")
	if len(args) > 0 && args[0] == "--version" {
		fmt.Println(orDefault(os.Getenv("FAKE_VERSION"), "0.0.21") + " (git sha: fake)")
		return 0
	}
	if len(args) >= 5 && args[0] == "admin" && args[len(args)-2] == "get" {
		if os.Getenv("CONTROL_PLANE_API_KEY") != os.Getenv("FAKE_GOOD_KEY") {
			fmt.Fprintln(os.Stderr, "error: 401 Unauthorized: invalid api key")
			return 1
		}
		fmt.Printf("{\"id\":%q,\"name\":\"Test tunnel\",\"workspace_ids\":[\"ws_1\"]}\n", args[len(args)-1])
		return 0
	}
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprintln(os.Stderr, "fake tunnel-client: unsupported arguments", args)
		return 2
	}
	rec := fakeRecord{Argv: args, Env: map[string]string{}, Responses: map[string]string{}}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		rec.Env[k] = v
	}
	if f := os.NewFile(3, "lock"); f != nil {
		if _, err := f.Stat(); err == nil {
			rec.LockFD = true
		}
	}
	save := func() {
		raw, _ := json.Marshal(rec)
		os.WriteFile(os.Getenv("FAKE_RECORD"), raw, 0o600)
	}
	save()
	if mode == "crash" {
		fmt.Println("level=error msg=\"tunnel metadata lookup failed\" status=404 detail=\"tunnel not found\"")
		return 3
	}
	if flagValue(args, "--control-plane.api-key") != "env:CONTROL_PLANE_API_KEY" || os.Getenv("CONTROL_PLANE_API_KEY") == "" {
		fmt.Fprintln(os.Stderr, "fake tunnel-client: api key not supplied through the environment")
		return 2
	}

	ln, err := net.Listen("tcp", flagValue(args, "--health.listen-addr"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	var mu sync.Mutex
	polled := false
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ready") })
	mux.HandleFunc("/health/control-plane", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case mode == "auth401":
			io.WriteString(w, `{"schema_version":1,"component":"control-plane","status":"degraded","details":{"consecutive_failures":3,"failure_category":"http_error","http_status":401}}`)
		case polled:
			io.WriteString(w, `{"schema_version":1,"component":"control-plane","status":"ok","details":{"last_success":"2026-01-01T00:00:00Z","consecutive_failures":0}}`)
		default:
			io.WriteString(w, `{"schema_version":1,"component":"control-plane","status":"unknown","details":{"consecutive_failures":0}}`)
		}
	})
	go http.Serve(ln, mux)
	if err := os.WriteFile(flagValue(args, "--health.url-file"), []byte("http://"+ln.Addr().String()+"\n"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)

	var child *exec.Cmd
	if mode != "auth401" {
		argv, err := upstreamSplit(flagValue(args, "--mcp.command"))
		if err != nil || len(argv) == 0 {
			fmt.Fprintln(os.Stderr, "fake tunnel-client: cannot parse --mcp.command:", err)
			return 2
		}
		rec.ChildArgv = argv
		child = exec.Command(argv[0], argv[1:]...)
		for _, kv := range os.Environ() { // the real client passes its environment on
			if !strings.HasPrefix(kv, "REPOCHAT_TEST_ROLE=") {
				child.Env = append(child.Env, kv)
			}
		}
		child.Stderr = os.Stderr
		stdin, _ := child.StdinPipe()
		stdout, _ := child.StdoutPipe()
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "fake tunnel-client: cannot start the MCP command:", err)
			return 2
		}
		mu.Lock()
		polled = true
		mu.Unlock()
		reader := bufio.NewReaderSize(stdout, 1<<20)
		ask := func(id, line string) {
			io.WriteString(stdin, line+"\n")
			if id == "" {
				return
			}
			resp, _ := reader.ReadString('\n')
			rec.Responses[id] = resp
		}
		time.Sleep(300 * time.Millisecond) // let repochat observe "authenticated" first
		ask("initialize", `{"jsonrpc":"2.0","id":"i","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"fake-chatgpt","version":"1"}}}`)
		if strings.Contains(strings.Join(args, " "), "--mcp.stdio-send-initialized-notification") {
			ask("", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		}
		ask("list", `{"jsonrpc":"2.0","id":"l","method":"tools/list"}`)
		ask("call", `{"jsonrpc":"2.0","id":"c","method":"tools/call","params":{"name":"repo_overview","arguments":{}}}`)
		ask("read", `{"jsonrpc":"2.0","id":"r","method":"tools/call","params":{"name":"read_file","arguments":{"path":"README.md"}}}`)
		save()
		defer func() {
			stdin.Close()
			child.Process.Signal(syscall.SIGTERM)
			child.Wait()
		}()
	}
	<-sig
	return 0
}

// lockedBuffer is a bytes.Buffer safe to read while a launcher writes to it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type testEnv struct {
	t      *testing.T
	repo   string
	record string
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=a@example.com",
		"GIT_COMMITTER_NAME=Ada", "GIT_COMMITTER_EMAIL=a@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func makeRepo(t *testing.T, name string) string {
	t.Helper()
	base, _ := filepath.EvalSymlinks(t.TempDir())
	dir := filepath.Join(base, name)
	os.MkdirAll(dir, 0o755)
	gitRun(t, dir, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# "+name+"\nhello\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=NOPE\n"), 0o644)
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "first")
	return dir
}

// setup isolates configuration, state and data, installs the stand-in
// tunnel-client and creates a repository.
func setup(t *testing.T, mode string) *testEnv {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("REPOCHAT_CONFIG_DIR", filepath.Join(home, "cfg"))
	t.Setenv("REPOCHAT_STATE_DIR", filepath.Join(home, "st"))
	t.Setenv("REPOCHAT_DATA_DIR", filepath.Join(home, "data"))
	t.Setenv("CONTROL_PLANE_TUNNEL_ID", "")
	t.Setenv("CONTROL_PLANE_API_KEY", "")
	t.Setenv("NO_COLOR", "1")
	e := &testEnv{t: t, repo: makeRepo(t, "project"), record: filepath.Join(home, "record.json")}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(home, "tunnel-client")
	script := fmt.Sprintf("#!/bin/sh\nREPOCHAT_TEST_ROLE=tunnel-client FAKE_RECORD=%s FAKE_GOOD_KEY=%s FAKE_MODE=%s exec %s \"$@\"\n",
		quoteArg(e.record), quoteArg(testKey), quoteArg(mode), quoteArg(self))
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REPOCHAT_TUNNEL_CLIENT", wrapper)
	return e
}

func saveProfile(t *testing.T, key string) {
	t.Helper()
	cfg := &Config{Profiles: map[string]*Profile{"default": {TunnelID: testTunnel, APIKey: key, TunnelName: "Test tunnel"}}, DefaultProfile: "default"}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

// start runs the launcher in the background and returns its output and a
// channel carrying its exit code.
func start(args ...string) (*lockedBuffer, *lockedBuffer, chan int) {
	out, errb := &lockedBuffer{}, &lockedBuffer{}
	done := make(chan int, 1)
	go func() { done <- Main(args, nil, out, errb) }()
	return out, errb, done
}

func runCLI(args ...string) (string, string, int) {
	var out, errb bytes.Buffer
	code := Main(args, nil, &out, &errb)
	return out.String(), errb.String(), code
}

func (e *testEnv) readRecord() fakeRecord {
	var rec fakeRecord
	raw, err := os.ReadFile(e.record)
	if err == nil {
		json.Unmarshal(raw, &rec)
	}
	return rec
}

func wantIn(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Errorf("expected %q in:\n%s", w, text)
		}
	}
}

func TestLauncherEndToEnd(t *testing.T) {
	e := setup(t, "")
	saveProfile(t, testKey)
	t.Setenv("MCP_SERVER_URL", "http://evil.example/mcp") // must not reach tunnel-client
	t.Setenv("OPENAI_ADMIN_KEY", "sk-admin-should-not-leak")

	_, errb, done := start("--root", e.repo)
	waitFor(t, "the first served tool call", func() bool { return strings.Contains(errb.String(), "setup is complete") })
	waitFor(t, "the fake to finish its script", func() bool { return e.readRecord().Responses["read"] != "" })
	waitFor(t, "the read_file activity line", func() bool { return strings.Contains(errb.String(), "README.md") })
	log := errb.String()
	if strings.Index(log, "connected to OpenAI") > strings.Index(log, "remote MCP activity detected") {
		t.Errorf("the connection state must be reported before the first remote activity:\n%s", log)
	}
	wantIn(t, log, "RepoChat", "repository  project", "1 withheld by policy (1 secret-name)", "Test tunnel",
		"tunnel-client 0.0.21 (git sha: fake) is running", "connected to OpenAI", "add the plugin in ChatGPT",
		"remote MCP activity detected", "fake-chatgpt", "repo_overview", "read_file", "README.md")
	if strings.Contains(log, testKey) {
		t.Error("the API key must never be printed")
	}

	rec := e.readRecord()
	argv := strings.Join(rec.Argv, "\x00")
	if strings.Contains(argv, testKey) {
		t.Error("the API key must not be on tunnel-client's command line")
	}
	for _, want := range []string{"run", "--control-plane.tunnel-id\x00" + testTunnel, "--control-plane.api-key\x00env:CONTROL_PLANE_API_KEY",
		"--mcp.stdio-send-initialized-notification", "--health.listen-addr\x00127.0.0.1:0", "--health.url-file\x00"} {
		if !strings.Contains(argv, want) {
			t.Errorf("tunnel-client argv lacks %q: %v", want, rec.Argv)
		}
	}
	if rec.Env["CONTROL_PLANE_API_KEY"] != testKey {
		t.Error("the key must reach tunnel-client through its environment")
	}
	for _, leaked := range []string{"MCP_SERVER_URL", "OPENAI_ADMIN_KEY", "REPOCHAT_TUNNEL_CLIENT"} {
		if _, ok := rec.Env[leaked]; ok {
			t.Errorf("%s leaked into tunnel-client's environment", leaked)
		}
	}
	if !rec.LockFD {
		t.Error("tunnel-client must inherit the tunnel ownership lock")
	}
	if len(rec.ChildArgv) != 4 || rec.ChildArgv[1] != "mcp" || rec.ChildArgv[2] != "--session" {
		t.Fatalf("unexpected MCP command: %v", rec.ChildArgv)
	}
	sess, err := decodeSession(rec.ChildArgv[3])
	if err != nil || sess.Root != e.repo || sess.Events == "" {
		t.Errorf("session blob: %+v %v", sess, err)
	}
	if strings.Contains(rec.ChildArgv[3], testKey) || strings.Contains(string(mustDecode(t, rec.ChildArgv[3])), "sk-") {
		t.Error("the session must not contain credentials")
	}
	wantIn(t, rec.Responses["initialize"], `"protocolVersion":"2025-11-25"`, `"name":"repochat"`)
	wantIn(t, rec.Responses["list"], `"name":"search_code"`, `"readOnlyHint":true`)
	wantIn(t, rec.Responses["call"], "REPOSITORY project", `"isError":false`)
	wantIn(t, rec.Responses["read"], "# project")
	if strings.Contains(rec.Responses["call"]+rec.Responses["read"], "NOPE") {
		t.Error("withheld content was served")
	}

	out, _, code := runCLI("status")
	if code != 0 || !strings.Contains(out, "sharing "+e.repo) || !strings.Contains(out, "serving") {
		t.Errorf("status: %d %q", code, out)
	}
	if _, errText, code := runCLI("--root", e.repo); code == 0 || !strings.Contains(errText, "already running for this repository") {
		t.Errorf("a second launcher for the same repository must refuse: %d %s", code, errText)
	}

	if _, _, code := runCLI("stop"); code != 0 {
		t.Fatal("stop failed")
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("launcher exit code %d\n%s", code, errb.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the launcher did not stop")
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.Profiles["default"]; !p.Onboarded || p.LastRepo != e.repo {
		t.Errorf("profile after first success: %+v", p)
	}
	if out, _, _ := runCLI("status"); !strings.Contains(out+"", "") || strings.Contains(out, "sharing") {
		t.Errorf("status after stop: %q", out)
	}

	// The activity log is private and metadata-only by default.
	st, _ := stateDir()
	logFile := filepath.Join(st, auditFile)
	fi, err := os.Stat(logFile)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("activity log: %v %v", fi, err)
	}
	raw, _ := os.ReadFile(logFile)
	wantIn(t, string(raw), `"tool":"read_file"`, `"kind":"call"`)
	if strings.Contains(string(raw), "README.md") || strings.Contains(string(raw), `"args"`) {
		t.Errorf("the activity log must not record arguments by default:\n%s", raw)
	}
	if out, _, code := runCLI("log"); code != 0 || !strings.Contains(out, "read_file") {
		t.Errorf("repochat log: %d %q", code, out)
	}

	// Second start: onboarded, so the short hint is shown instead of the steps.
	_, errb2, done2 := start("--root", e.repo, "--quiet", "--audit-args")
	waitFor(t, "second run to connect", func() bool { return strings.Contains(errb2.String(), "connected to OpenAI") })
	waitFor(t, "second run activity", func() bool { return e.readRecord().Responses["read"] != "" })
	if strings.Contains(errb2.String(), "add the plugin in ChatGPT") {
		t.Error("the one-time steps must not be repeated once a tool call has been served")
	}
	wantIn(t, errb2.String(), "@RepoChat")
	time.Sleep(300 * time.Millisecond)
	if strings.Contains(errb2.String(), "read_file") {
		t.Error("--quiet must hide live activity lines")
	}
	runCLI("stop")
	<-done2
	raw, _ = os.ReadFile(logFile)
	if !strings.Contains(string(raw), `"args":{"path":"README.md"}`) {
		t.Errorf("--audit-args must record arguments:\n%s", raw)
	}
}

func mustDecode(t *testing.T, blob string) []byte {
	t.Helper()
	s, err := decodeSession(blob)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(s)
	return raw
}

func TestTakeover(t *testing.T) {
	e := setup(t, "")
	saveProfile(t, testKey)
	second := makeRepo(t, "other")

	_, errA, doneA := start("--root", e.repo)
	waitFor(t, "first instance", func() bool { return strings.Contains(errA.String(), "connected to OpenAI") })

	// Without consent the newcomer leaves the owner alone.
	_, errText, code := runCLI("--root", second)
	if code == 0 || !strings.Contains(errText, "already in use") || !strings.Contains(errText, e.repo) {
		t.Fatalf("expected a refusal naming the other repository: %d %s", code, errText)
	}
	select {
	case <-doneA:
		t.Fatal("the owner must keep running when the newcomer was not allowed to take over")
	default:
	}

	_, errB, doneB := start("--root", second, "--yes")
	select {
	case code := <-doneA:
		if code != 0 {
			t.Errorf("owner exit code %d", code)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the owner did not stop on takeover")
	}
	wantIn(t, errA.String(), "taking over")
	waitFor(t, "newcomer to serve", func() bool { return strings.Contains(errB.String(), "connected to OpenAI") })
	wantIn(t, errB.String(), "the other instance stopped", "repository  other", "this tunnel last served "+e.repo)
	out, _, _ := runCLI("status")
	if !strings.Contains(out, "sharing "+second) {
		t.Errorf("status: %q", out)
	}
	runCLI("stop")
	<-doneB
}

func TestUnresponsiveOwnerIsNeverDoubled(t *testing.T) {
	e := setup(t, "")
	saveProfile(t, testKey)
	dir, err := tunnelRunDir(testTunnel)
	if err != nil {
		t.Fatal(err)
	}
	// Something holds the tunnel lock but offers no control socket.
	holder, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "owner.json"), []byte(`{"repo":"/somewhere/else","socket":"`+filepath.Join(dir, "stale.sock")+`"}`), 0o600)

	_, errText, code := runCLI("--root", e.repo, "--yes")
	if code == 0 || !strings.Contains(errText, "does not answer") || !strings.Contains(errText, "Not starting a second connection") {
		t.Fatalf("expected a refusal: %d %s", code, errText)
	}
	if rec := e.readRecord(); len(rec.Argv) != 0 {
		t.Fatal("tunnel-client must not be started while another owner holds the tunnel")
	}
}

func TestLauncherReportsAuthAndCrashFailures(t *testing.T) {
	e := setup(t, "auth401")
	saveProfile(t, testKey)
	_, errText, code := runCLI("--root", e.repo)
	if code == 0 {
		t.Fatal("an authentication failure must fail the run")
	}
	wantIn(t, errText, "rejected the tunnel connection (HTTP 401)", "API key was not accepted", "repochat setup")
	if strings.Contains(errText, "connected to OpenAI") {
		t.Error("must not claim to be connected")
	}

	e = setup(t, "crash")
	saveProfile(t, testKey)
	_, errText, code = runCLI("--root", e.repo)
	if code != 3 {
		t.Errorf("tunnel-client's exit code must be propagated, got %d", code)
	}
	wantIn(t, errText, "stopped unexpectedly (exit code 3)", "tunnel not found", "Check the tunnel ID")
}

func TestFirstRunWithoutTerminal(t *testing.T) {
	e := setup(t, "")
	_, errText, code := runCLI("--root", e.repo)
	if code == 0 || !strings.Contains(errText, "CONTROL_PLANE_TUNNEL_ID") || !strings.Contains(errText, "terminal") {
		t.Fatalf("expected instructions for non-interactive use: %d %s", code, errText)
	}
	if rec := e.readRecord(); len(rec.Argv) != 0 {
		t.Error("nothing may be started")
	}

	// Outside a repository the message says so before anything else happens.
	_, errText, code = runCLI("--root", t.TempDir())
	if code == 0 || !strings.Contains(errText, "not inside a Git repository") {
		t.Errorf("non-repository: %d %s", code, errText)
	}

	// Credentials from the environment need no saved configuration.
	t.Setenv("CONTROL_PLANE_TUNNEL_ID", testTunnel)
	t.Setenv("CONTROL_PLANE_API_KEY", testKey)
	out, errText, code := runCLI("--root", e.repo, "--print-command")
	if code != 0 || !strings.Contains(out, "--control-plane.api-key env:CONTROL_PLANE_API_KEY") {
		t.Fatalf("--print-command: %d %s %s", code, out, errText)
	}
	if strings.Contains(out+errText, testKey) {
		t.Error("--print-command must not reveal the key")
	}
}

func TestBroadRootIsRefused(t *testing.T) {
	setup(t, "")
	saveProfile(t, testKey)
	home, _ := filepath.EvalSymlinks(os.Getenv("HOME"))
	gitRun(t, home, "init", "-q")
	_, errText, code := runCLI("--root", home)
	if code == 0 || !strings.Contains(errText, "--allow-broad-root") {
		t.Fatalf("serving the home directory must need an explicit flag: %d %s", code, errText)
	}
}

func TestSetupNonInteractive(t *testing.T) {
	setup(t, "")
	t.Setenv("CONTROL_PLANE_TUNNEL_ID", testTunnel)
	t.Setenv("CONTROL_PLANE_API_KEY", testKey)
	_, errText, code := runCLI("setup")
	if code != 0 {
		t.Fatalf("setup failed: %s", errText)
	}
	wantIn(t, errText, `tunnel "Test tunnel" is readable`, "saved to")
	path, _ := configPath()
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions: %v %v", fi, err)
	}
	t.Setenv("CONTROL_PLANE_TUNNEL_ID", "")
	t.Setenv("CONTROL_PLANE_API_KEY", "")
	cfg, _ := loadConfig()
	if p := cfg.Profiles["default"]; p == nil || p.TunnelID != testTunnel || p.APIKey != testKey || p.TunnelName != "Test tunnel" {
		t.Fatalf("saved profile: %+v", p)
	}
	out, _, _ := runCLI("setup", "--list")
	if !strings.Contains(out, "default") || strings.Contains(out, testKey) || strings.Contains(out, testTunnel) {
		t.Errorf("--list must mask identifiers: %q", out)
	}

	// A key OpenAI rejects is not saved silently.
	t.Setenv("CONTROL_PLANE_TUNNEL_ID", testTunnel)
	t.Setenv("CONTROL_PLANE_API_KEY", "sk-wrong-0123456789abcdefghij")
	_, errText, code = runCLI("setup", "--profile", "second")
	if code == 0 || !strings.Contains(errText, "could not verify") || !strings.Contains(errText, "API key was not accepted") {
		t.Fatalf("a rejected key must fail setup: %d %s", code, errText)
	}
	if strings.Contains(errText, "sk-wrong") {
		t.Error("the key must not be echoed")
	}
	if cfg, _ := loadConfig(); cfg.Profiles["second"] != nil {
		t.Error("an unverified profile must not be saved without consent")
	}
	if _, errText, code = runCLI("setup", "--profile", "bad name!"); code == 0 {
		t.Errorf("invalid profile names must be rejected: %s", errText)
	}
}

func TestBoundProfiles(t *testing.T) {
	e := setup(t, "")
	other := makeRepo(t, "other")
	cfg := &Config{Profiles: map[string]*Profile{
		"default": {TunnelID: testTunnel, APIKey: testKey},
		"pinned":  {TunnelID: "tunnel_ffffffffffffffffffffffffffffffff", APIKey: testKey, BoundRepo: other},
	}, DefaultProfile: "default"}
	name, p, err := cfg.selectProfile("", other)
	if err != nil || name != "pinned" || p == nil {
		t.Errorf("a bound profile is selected automatically in its repository: %s %v", name, err)
	}
	if name, _, _ := cfg.selectProfile("", e.repo); name != "default" {
		t.Errorf("other repositories use the default profile, got %s", name)
	}
	if _, _, err := cfg.selectProfile("pinned", e.repo); err == nil || !strings.Contains(err.Error(), "bound to") {
		t.Errorf("a bound profile must refuse other repositories: %v", err)
	}
}

func TestDoctor(t *testing.T) {
	e := setup(t, "")
	saveProfile(t, testKey)
	_, errText, code := runCLI("doctor", "--root", e.repo)
	if code != 0 {
		t.Fatalf("doctor should pass:\n%s", errText)
	}
	wantIn(t, errText, "✓ git", "✓ repository", "1 withheld", "✓ MCP server", "legacy and 2026-07-28", "✓ tunnel-client",
		"✓ credentials", "✓ OpenAI: tunnel lookup", "Test tunnel", "not tested: only observable while", "Everything that could be tested works")

	saveProfile(t, "sk-wrong-0123456789abcdefghij")
	_, errText, code = runCLI("doctor", "--root", e.repo)
	if code != 1 {
		t.Errorf("doctor must fail on a rejected key, got %d", code)
	}
	wantIn(t, errText, "✗ OpenAI: tunnel lookup", "API key was not accepted", "1 problem(s) found")
	_, errText, code = runCLI("doctor", "--root", e.repo, "--offline")
	if code != 0 || !strings.Contains(errText, "not tested: --offline") {
		t.Errorf("--offline: %d %s", code, errText)
	}
}

func TestHelperVersionGate(t *testing.T) {
	e := setup(t, "")
	saveProfile(t, testKey)
	wrapper := os.Getenv("REPOCHAT_TUNNEL_CLIENT")
	script, _ := os.ReadFile(wrapper)
	os.WriteFile(wrapper, []byte(strings.Replace(string(script), "REPOCHAT_TEST_ROLE=", "FAKE_VERSION=0.0.9 REPOCHAT_TEST_ROLE=", 1)), 0o755)
	_, errText, code := runCLI("--root", e.repo)
	if code == 0 || !strings.Contains(errText, "0.0.21 or newer") {
		t.Fatalf("an old tunnel-client must be refused: %d %s", code, errText)
	}
	if rec := e.readRecord(); len(rec.Argv) != 0 {
		t.Error("an old tunnel-client must not be run")
	}
}

func TestParseCredentialPaste(t *testing.T) {
	good := []string{
		testTunnel + " " + testKey,
		testKey + "\t" + testTunnel,
		"CONTROL_PLANE_TUNNEL_ID=" + testTunnel + "\nexport CONTROL_PLANE_API_KEY=\"" + testKey + "\"\r",
		"tunnel id: " + testTunnel + ", key: " + testKey,
		testTunnel + " " + testTunnel + " " + testKey,
	}
	for _, in := range good {
		id, key, err := parseCredentialPaste(in)
		if err != nil || id != testTunnel || key != testKey {
			t.Errorf("paste %q: %q %q %v", in, id, key, err)
		}
	}
	if id, key, err := parseCredentialPaste(testTunnel); err != nil || id == "" || key != "" {
		t.Errorf("a lone tunnel id is accepted as a partial answer: %v", err)
	}
	bad := map[string]string{
		"two ids":    testTunnel + " tunnel_ffffffffffffffffffffffffffffffff " + testKey,
		"two keys":   testKey + " sk-other-0123456789abcdefghijkl",
		"short id":   "tunnel_abc " + testKey,
		"stray text": testTunnel + " hunter2 " + testKey,
	}
	for name, in := range bad {
		_, _, err := parseCredentialPaste(in)
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		for _, secret := range []string{"hunter2", "sk-other", testKey, "tunnel_abc"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: the error echoes pasted input: %v", name, err)
			}
		}
	}
}

func TestConfigIsPrivate(t *testing.T) {
	setup(t, "")
	saveProfile(t, testKey)
	path, _ := configPath()
	if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o700 {
		t.Errorf("config directory mode %v", fi.Mode().Perm())
	}
	os.Chmod(path, 0o644)
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("a world-readable config must be rejected with a fix: %v", err)
	}
	os.Chmod(path, 0o600)
	real := path + ".real"
	os.Rename(path, real)
	os.Symlink(real, path)
	if _, err := loadConfig(); err == nil {
		t.Error("a symlinked config must be rejected")
	}
	os.Remove(path)
	os.WriteFile(path, []byte("{not json"), 0o600)
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("corrupt config: %v", err)
	}
	os.WriteFile(path, []byte(`{"version":99,"profiles":{}}`), 0o600)
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "newer repochat") {
		t.Errorf("future config version: %v", err)
	}
	if err := validateCredentials(credentials{TunnelID: "tunnel_short", APIKey: testKey}); err == nil {
		t.Error("malformed tunnel ids must be rejected")
	}
	if err := validateCredentials(credentials{TunnelID: testTunnel, APIKey: "has spaces in it 0123456789"}); err == nil {
		t.Error("malformed keys must be rejected")
	}
}

func zipWith(t *testing.T, build func(w *zip.Writer)) (*os.File, int64) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "*.zip")
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	build(w)
	w.Close()
	fi, _ := f.Stat()
	return f, fi.Size()
}

func addFile(w *zip.Writer, name, content string, mode os.FileMode) {
	h := &zip.FileHeader{Name: name, Method: zip.Deflate}
	h.SetMode(mode)
	fw, _ := w.CreateHeader(h)
	io.WriteString(fw, content)
}

func TestExtractHelper(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "tunnel-client")

	f, size := zipWith(t, func(w *zip.Writer) {
		addFile(w, "LICENSE", "license", 0o644)
		addFile(w, "tunnel-client", "#!/bin/sh\necho hi\n", 0o755)
	})
	if err := extractHelper(f, size, dir, dest); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dest)
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("installed binary: %v %v", fi, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("no temporary files may be left behind: %v", entries)
	}

	bad := map[string]func(w *zip.Writer){
		"missing":   func(w *zip.Writer) { addFile(w, "README", "x", 0o644) },
		"duplicate": func(w *zip.Writer) { addFile(w, "tunnel-client", "a", 0o755); addFile(w, "tunnel-client", "b", 0o755) },
		"symlink":   func(w *zip.Writer) { addFile(w, "tunnel-client", "/bin/sh", os.ModeSymlink|0o777) },
		"nested":    func(w *zip.Writer) { addFile(w, "bin/tunnel-client", "a", 0o755) },
		"traversal": func(w *zip.Writer) { addFile(w, "../tunnel-client", "a", 0o755) },
		"too many": func(w *zip.Writer) {
			for i := 0; i < maxHelperEntries+1; i++ {
				addFile(w, fmt.Sprintf("f%d", i), "x", 0o644)
			}
			addFile(w, "tunnel-client", "a", 0o755)
		},
	}
	for name, build := range bad {
		os.Remove(dest)
		f, size := zipWith(t, build)
		if err := extractHelper(f, size, dir, dest); err == nil {
			t.Errorf("%s: the archive must be rejected", name)
		}
		if _, err := os.Lstat(dest); err == nil {
			t.Errorf("%s: nothing may be installed from a rejected archive", name)
		}
	}
}

func TestHelperDownloadRules(t *testing.T) {
	for _, ok := range [][2]string{{"https", "github.com"}, {"https", "objects.githubusercontent.com"}, {"https", "release-assets.githubusercontent.com"}} {
		if err := checkHelperURL(ok[0], ok[1]); err != nil {
			t.Errorf("%v should be allowed: %v", ok, err)
		}
	}
	for _, bad := range [][2]string{{"http", "github.com"}, {"https", "github.com.evil.example"}, {"https", "example.com"}, {"file", ""}, {"https", "raw.githubusercontent.com"}} {
		if err := checkHelperURL(bad[0], bad[1]); err == nil {
			t.Errorf("%v must be refused", bad)
		}
	}
	client := helperHTTPClient()
	req, _ := http.NewRequest("GET", "https://evil.example/x.zip", nil)
	if err := client.CheckRedirect(req, nil); err == nil {
		t.Error("redirects to other hosts must be refused")
	}
	req, _ = http.NewRequest("GET", "http://github.com/x.zip", nil)
	if err := client.CheckRedirect(req, nil); err == nil {
		t.Error("redirects to plain http must be refused")
	}
	if len(helperPinnedSHA256) != 4 {
		t.Errorf("expected four pinned platforms, got %d", len(helperPinnedSHA256))
	}
	for platform, sum := range helperPinnedSHA256 {
		if len(sum) != 64 || strings.ToLower(sum) != sum {
			t.Errorf("%s: malformed pinned checksum", platform)
		}
	}
	if got := helperAssetURL("0.0.21", helperAssetName("0.0.21")); !strings.HasPrefix(got, "https://github.com/openai/tunnel-client/releases/download/v0.0.21/tunnel-client-v0.0.21-") {
		t.Errorf("asset url: %s", got)
	}
	// Without consent nothing is downloaded.
	t.Setenv("REPOCHAT_DATA_DIR", t.TempDir())
	var errb bytes.Buffer
	c := newConsole(nil, io.Discard, &errb)
	if _, err := installHelper(context.Background(), c, ""); err == nil || !strings.Contains(err.Error(), "install-helper --yes") {
		t.Errorf("installing needs consent: %v", err)
	}
	if _, err := installHelper(context.Background(), c, "../../x"); err == nil {
		t.Error("invalid versions must be rejected")
	}
}

func TestCommandQuotingMatchesUpstreamParser(t *testing.T) {
	paths := []string{
		"/usr/local/bin/repochat", "/home/o'brien/bin/repochat", "/opt/my tools/repo chat", `/weird/"quoted"/path`,
		`/back\slash/x`, "/tab\there", "/a,channel=main/b", "/uni/çödé/répochat", "/dollar/$HOME/`id`",
	}
	blob := session{Root: "/some/repo with space/it's", Deny: []string{"*.sql"}}.encode()
	for _, p := range paths {
		command := quoteArg(p) + " mcp --session " + quoteArg(blob)
		argv, err := upstreamSplit(command)
		if err != nil {
			t.Fatalf("%q: %v", p, err)
		}
		if len(argv) != 4 || argv[0] != p || argv[1] != "mcp" || argv[2] != "--session" || argv[3] != blob {
			t.Errorf("%q does not survive tunnel-client's parser: %q", p, argv)
		}
		for _, prefix := range []string{"url=", "command=", "channel=", "unix-socket="} {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(command)), prefix) {
				t.Errorf("%q would be read as a qualified entry", command)
			}
		}
	}
	back, err := decodeSession(blob)
	if err != nil || back.Root != "/some/repo with space/it's" || len(back.Deny) != 1 {
		t.Errorf("session round trip: %+v %v", back, err)
	}
	if _, err := decodeSession("not base64!"); err == nil {
		t.Error("garbage sessions must be rejected")
	}
}

func TestControlPlaneOverrideIsLoopbackOnly(t *testing.T) {
	for _, bad := range []string{"https://evil.example", "http://10.0.0.5:8080", "http://localhost.evil.example", "ftp://127.0.0.1", "http://user:pw@127.0.0.1"} {
		t.Setenv("REPOCHAT_CONTROL_PLANE_URL", bad)
		if args, err := controlPlaneOverride(); err == nil {
			t.Errorf("%s must be refused, got %v", bad, args)
		}
	}
	t.Setenv("REPOCHAT_CONTROL_PLANE_URL", "http://127.0.0.1:9999")
	if args, err := controlPlaneOverride(); err != nil || len(args) != 2 {
		t.Errorf("loopback must be accepted: %v %v", args, err)
	}
	t.Setenv("REPOCHAT_CONTROL_PLANE_URL", "")
	if args, err := controlPlaneOverride(); err != nil || args != nil {
		t.Errorf("unset means no override: %v %v", args, err)
	}
}

func TestHelperEnvironmentIsAnAllowlist(t *testing.T) {
	for k, v := range map[string]string{
		"CONTROL_PLANE_BASE_URL": "http://evil", "MCP_COMMAND": "evil", "HEALTH_LISTEN_ADDR": "0.0.0.0:1", "LOG_HTTP_RAW_UNSAFE": "true",
		"OPENAI_ADMIN_KEY": "sk-admin", "OPENAI_API_KEY": "sk-user", "AWS_SECRET_ACCESS_KEY": "x", "HTTPS_PROXY": "http://proxy:8080",
	} {
		t.Setenv(k, v)
	}
	env := strings.Join(helperEnv("CONTROL_PLANE_API_KEY=only-this"), "\n")
	for _, leaked := range []string{"CONTROL_PLANE_BASE_URL", "MCP_COMMAND", "HEALTH_LISTEN_ADDR", "LOG_HTTP_RAW_UNSAFE", "OPENAI_ADMIN_KEY", "OPENAI_API_KEY", "AWS_SECRET"} {
		if strings.Contains(env, leaked) {
			t.Errorf("%s must not reach tunnel-client", leaked)
		}
	}
	wantIn(t, env, "CONTROL_PLANE_API_KEY=only-this", "HTTPS_PROXY=http://proxy:8080", "PATH=")
}

func TestMCPServerScrubsCredentials(t *testing.T) {
	for k := range map[string]string{"CONTROL_PLANE_API_KEY": "", "OPENAI_API_KEY": "", "CONTROL_PLANE_TUNNEL_ID": "", "GITHUB_TOKEN": "", "MY_SECRET": ""} {
		t.Setenv(k, "value")
	}
	t.Setenv("HARMLESS", "stays")
	scrubSecretsFromEnv()
	for _, k := range []string{"CONTROL_PLANE_API_KEY", "OPENAI_API_KEY", "CONTROL_PLANE_TUNNEL_ID", "GITHUB_TOKEN", "MY_SECRET"} {
		if _, ok := os.LookupEnv(k); ok {
			t.Errorf("%s must be removed from the MCP server's environment", k)
		}
	}
	if os.Getenv("HARMLESS") != "stays" {
		t.Error("unrelated variables must be left alone")
	}
}

func TestInspectionCommands(t *testing.T) {
	e := setup(t, "")
	out, _, code := runCLI("files", "--root", e.repo)
	if code != 0 || !strings.Contains(out, "README.md") || strings.Contains(out, ".env") {
		t.Errorf("files: %d %q", code, out)
	}
	out, _, _ = runCLI("files", "--root", e.repo, "--withheld")
	if !strings.Contains(out, ".env\tsecret-name") {
		t.Errorf("files --withheld: %q", out)
	}
	out, _, _ = runCLI("files", "--root", e.repo, "--allow", ".env", "--deny", "README.md")
	if !strings.Contains(out, ".env") || strings.Contains(out, "README.md") {
		t.Errorf("--allow/--deny: %q", out)
	}

	out, _, code = runCLI("tools", "--root", e.repo, "--json")
	var listed struct {
		Tools []map[string]any `json:"tools"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &listed) != nil || len(listed.Tools) != 13 || strings.Contains(out, "null") {
		t.Errorf("tools --json: %d (%d tools)", code, len(listed.Tools))
	}

	for _, protocol := range []string{"legacy", "modern"} {
		out, errText, code := runCLI("call", "--root", e.repo, "--protocol", protocol, "read_file", `{"path":"README.md"}`)
		if code != 0 || !strings.Contains(out, "1| # project") || !strings.Contains(out, "@meta ") {
			t.Errorf("call (%s): %d %q %q", protocol, code, out, errText)
		}
	}
	out, _, code = runCLI("call", "--root", e.repo, "read_file", `{"path":".env"}`)
	if code != 1 || !strings.Contains(out, "withheld") || strings.Contains(out, "NOPE") {
		t.Errorf("call on a withheld file: %d %q", code, out)
	}
	if _, errText, code := runCLI("call", "--root", e.repo, "nope"); code != 1 || !strings.Contains(errText, "Unknown tool") {
		t.Errorf("unknown tool: %d %q", code, errText)
	}
	if _, _, code := runCLI("call", "--root", e.repo, "read_file", "{not json"); code != 2 {
		t.Errorf("bad JSON arguments must be a usage error, got %d", code)
	}

	out, _, code = runCLI("mcp-config", "--root", e.repo)
	var snippet map[string]map[string]map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &snippet) != nil || snippet["mcpServers"]["repochat"]["command"] == "" {
		t.Errorf("mcp-config: %d %q", code, out)
	}

	out, _, code = runCLI("version")
	if code != 0 || !strings.HasPrefix(out, "repochat ") {
		t.Errorf("version: %q", out)
	}
	out, _, code = runCLI("help")
	if code != 0 || !strings.Contains(out, "repochat doctor") || !strings.Contains(out, "sent to OpenAI") {
		t.Errorf("help: %q", out)
	}
	if _, errText, code := runCLI("frobnicate"); code != 2 || !strings.Contains(errText, "unknown command") {
		t.Errorf("unknown command: %d %q", code, errText)
	}
	if out, _, _ := runCLI("status"); !strings.Contains(out+"x", "x") {
		t.Error("unreachable")
	}

	dest := t.TempDir()
	_, errText, code := runCLI("install", "--dir", dest)
	if fi, err := os.Stat(filepath.Join(dest, "repochat")); code != 0 || err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("install: %d %v %s", code, err, errText)
	}
}

func TestActivityLineIsSafeToPrint(t *testing.T) {
	red := repo.NewRedactor(true)
	summary := summarizeArgs(map[string]any{
		"path":   "src/evil\n12:00:00  read_file FORGED\x1b[2J",
		"query":  "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB",
		"cursor": "abc",
		"paths":  []any{"a", "b"},
		"limit":  float64(5),
	}, red)
	for _, bad := range []string{"\n", "\x1b", "ghp_0123456789"} {
		if strings.Contains(summary, bad) {
			t.Errorf("activity summary contains %q: %q", bad, summary)
		}
	}
	wantIn(t, summary, `src/evil\n12:00:00`, "[REDACTED:github-token]", "(continued)", "limit=5")
	if len(summary) > 150 {
		t.Errorf("summary too long: %d", len(summary))
	}
}

func TestAuditRotation(t *testing.T) {
	setup(t, "")
	a, err := openAudit()
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(a.path, bytes.Repeat([]byte("x"), auditMaxBytes+1), 0o600)
	if err := a.write(activity{Kind: "call", Tool: "read_file", Summary: "must not be stored"}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(a.path + ".1"); err != nil || fi.Size() <= auditMaxBytes {
		t.Errorf("the full log must be rotated: %v", err)
	}
	raw, _ := os.ReadFile(a.path)
	if !strings.Contains(string(raw), `"tool":"read_file"`) || strings.Contains(string(raw), "must not be stored") {
		t.Errorf("new log: %s", raw)
	}
}

func TestSelfTestCoversBothEras(t *testing.T) {
	e := setup(t, "")
	r, err := repo.Open(context.Background(), e.repo, repo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := selfTest(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}
