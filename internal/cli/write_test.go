package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// driveFromFiles is the stand-in tunnel-client's "drive" mode: it sends the
// request in <dir>/<n>.req to the MCP server, n = 1, 2, …, as each appears,
// and writes the reply to <dir>/<n>.resp.
func driveFromFiles(dir string, stdin io.Writer, reader *bufio.Reader) {
	for n := 1; ; n++ {
		req := filepath.Join(dir, fmt.Sprintf("%d.req", n))
		var line []byte
		for {
			if b, err := os.ReadFile(req); err == nil {
				line = b
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		io.WriteString(stdin, strings.TrimSpace(string(line))+"\n")
		resp, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		tmp := filepath.Join(dir, fmt.Sprintf("%d.tmp", n))
		os.WriteFile(tmp, []byte(resp), 0o600)
		os.Rename(tmp, filepath.Join(dir, fmt.Sprintf("%d.resp", n)))
	}
}

// driver sends tool calls through the stand-in tunnel-client to the MCP
// server the launcher started.
type driver struct {
	t   *testing.T
	dir string
	n   int
}

func newDriver(t *testing.T, e *testEnv) *driver {
	dir := filepath.Join(filepath.Dir(e.record), "drive")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return &driver{t: t, dir: dir}
}

// call runs one tool and returns its text and whether it is an error.
func (d *driver) call(tool string, args any) (string, bool) {
	d.t.Helper()
	d.n++
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": fmt.Sprintf("d%d", d.n), "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	tmp := filepath.Join(d.dir, fmt.Sprintf("%d.reqtmp", d.n))
	os.WriteFile(tmp, raw, 0o600)
	os.Rename(tmp, filepath.Join(d.dir, fmt.Sprintf("%d.req", d.n)))
	respPath := filepath.Join(d.dir, fmt.Sprintf("%d.resp", d.n))
	var resp []byte
	waitFor(d.t, "the reply to "+tool, func() bool {
		var err error
		resp, err = os.ReadFile(respPath)
		return err == nil
	})
	var reply struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &reply); err != nil {
		d.t.Fatalf("%s: malformed reply %q", tool, resp)
	}
	if reply.Error != nil {
		return reply.Error.Message, true
	}
	var text strings.Builder
	for _, c := range reply.Result.Content {
		text.WriteString(c.Text)
	}
	return text.String(), reply.Result.IsError
}

// startWriting starts the launcher in write mode with the driving stand-in
// and waits until the server has answered the stand-in's own requests.
func startWriting(t *testing.T, args ...string) (*testEnv, *driver, *lockedBuffer, chan int) {
	t.Helper()
	e := setup(t, "drive")
	saveProfile(t, testKey)
	_, errb, done := start(append([]string{"--root", e.repo, "--write"}, args...)...)
	waitFor(t, "the stand-in's requests to be served", func() bool { return e.readRecord().Responses["read"] != "" })
	t.Cleanup(func() {
		runCLI("stop")
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("the launcher did not stop")
		}
	})
	return e, newDriver(t, e), errb, done
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

var createNotes = map[string]any{
	"changes": []any{map[string]any{"op": "create", "path": "notes.md", "text": "secret-sauce-xyzzy\n"}},
	"message": "add notes",
}

func toolCount(t *testing.T, args ...string) int {
	t.Helper()
	out, errText, code := runCLI(append([]string{"tools", "--json"}, args...)...)
	if code != 0 {
		t.Fatalf("tools %v: %d %s", args, code, errText)
	}
	var payload struct {
		Tools []any `json:"tools"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatal(err)
	}
	return len(payload.Tools)
}

func TestWriteFlagAddsTheToolsAndNothingElseDoes(t *testing.T) {
	e := setup(t, "")
	if n := toolCount(t, "--root", e.repo); n != 13 {
		t.Errorf("tools: %d, want 13", n)
	}

	script := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}
{"jsonrpc":"2.0","id":1,"method":"tools/list"}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"apply_changes","arguments":{"changes":[{"op":"create","path":"x.txt","text":"x"}]}}}
`
	in := filepath.Join(t.TempDir(), "in")
	os.WriteFile(in, []byte(script), 0o600)
	f, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out, errb bytes.Buffer
	if code := Main([]string{"mcp", "--root", e.repo}, f, &out, &errb); code != 0 {
		t.Fatalf("mcp: %d %s", code, errb.String())
	}
	replies := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var r struct {
			ID json.RawMessage `json:"id"`
		}
		json.Unmarshal([]byte(line), &r)
		replies[string(r.ID)] = line
	}
	var list struct {
		Result struct {
			Tools []any `json:"tools"`
		} `json:"result"`
	}
	json.Unmarshal([]byte(replies["1"]), &list)
	if len(list.Result.Tools) != 13 {
		t.Errorf("mcp without --write lists %d tools, want 13", len(list.Result.Tools))
	}
	wantIn(t, replies["2"], "Unknown tool: apply_changes")
	if _, err := os.Stat(filepath.Join(e.repo, "x.txt")); err == nil {
		t.Error("a file was written without --write")
	}
	st, _ := stateDir()
	if _, err := os.Stat(filepath.Join(st, "sessions")); err == nil {
		t.Error("the session state was created without --write")
	}
	if n := toolCount(t, "--root", e.repo, "--write"); n != 22 {
		t.Errorf("tools --write: %d, want 22", n)
	}
}

func TestBannerSaysWriteModeAndNamesTheBranch(t *testing.T) {
	e := setup(t, "drive")
	saveProfile(t, testKey)
	_, errb, done := start("--root", e.repo, "--write")
	waitFor(t, "the stand-in's requests to be served", func() bool { return e.readRecord().Responses["read"] != "" })
	base := gitOut(t, e.repo, "rev-parse", "HEAD")[:7]
	waitFor(t, "the banner", func() bool { return strings.Contains(errb.String(), "merging") })
	banner := errb.String()
	wantIn(t, banner, "· WRITE MODE (experimental)",
		"from main @ "+base+" · 0 checkpoint(s)",
		"  audience    whoever can use this plugin in ChatGPT can read these files and change them in the session workspace while repochat runs",
		"  merging     nothing reaches main until finish_session is called")
	if strings.Contains(banner, "read-only") {
		t.Errorf("the write-mode banner says read-only:\n%s", banner)
	}
	if !regexp.MustCompile(`  session     repochat/\d{4}-\d{2}-\d{2}-[0-9a-f]{8} · from main`).MatchString(banner) {
		t.Errorf("the banner does not name the session branch:\n%s", banner)
	}
	first := regexp.MustCompile(`repochat/\d{4}-\d{2}-\d{2}-[0-9a-f]{8}`).FindString(banner)
	rec := e.readRecord()
	sess, err := decodeSession(rec.ChildArgv[3])
	if err != nil || !sess.Write || sess.NewSession {
		t.Errorf("the server's options: %+v %v", sess, err)
	}
	runCLI("stop")
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the first launcher did not stop")
	}

	// With --new-session: one new session, started by the launcher alone.
	os.Remove(e.record)
	_, errb2, done2 := start("--root", e.repo, "--write", "--new-session", "--confirm-merge")
	waitFor(t, "the stand-in's requests to be served", func() bool { return e.readRecord().Responses["read"] != "" })
	banner2 := errb2.String()
	wantIn(t, banner2, "  merging     nothing reaches main until you run: repochat session finish", "is kept; its workspace was removed")
	second := regexp.MustCompile(`repochat/\d{4}-\d{2}-\d{2}-[0-9a-f]{8}`).FindString(banner2)
	if second == "" || second == first {
		t.Errorf("--new-session did not start a new session: %q then %q", first, second)
	}
	sess, err = decodeSession(e.readRecord().ChildArgv[3])
	if err != nil || sess.NewSession || !sess.ConfirmMerge {
		t.Errorf("--new-session must not reach the server: %+v %v", sess, err)
	}
	worktrees := strings.Count(gitOut(t, e.repo, "worktree", "list", "--porcelain"), "worktree ")
	if worktrees != 2 {
		t.Errorf("%d worktrees, want the checkout and one session", worktrees)
	}
	st, _ := stateDir()
	dirs, _ := filepath.Glob(filepath.Join(st, "sessions", "*", "[0-9a-f]*[0-9a-f]"))
	if len(dirs) != 1 {
		t.Errorf("session directories: %v, want one", dirs)
	}
	if !strings.Contains(gitOut(t, e.repo, "branch", "--list", first), first) {
		t.Errorf("the first session's branch %s was not kept", first)
	}
	runCLI("stop")
	<-done2
}

func TestSessionCommandShowsAndFinishes(t *testing.T) {
	e, d, errb, _ := startWriting(t)
	if text, isErr := d.call("start_session", map[string]any{}); isErr || !strings.Contains(text, "Resumed the open session") {
		t.Fatalf("start_session: %s", text)
	}
	if text, isErr := d.call("apply_changes", createNotes); isErr {
		t.Fatalf("apply_changes: %s", text)
	}
	waitFor(t, "the apply in the activity display", func() bool { return strings.Contains(errb.String(), "applied checkpoint") })

	out, errText, code := runCLI("session", "--root", e.repo)
	if code != 0 {
		t.Fatalf("session: %d %s", code, errText)
	}
	wantIn(t, out, "on branch repochat/", "checkpoints  1 since the start", "notes.md")

	before := gitOut(t, e.repo, "rev-parse", "HEAD")
	finish := func(answer string) (string, int) {
		var out, errb bytes.Buffer
		c := &console{rd: bufio.NewReader(strings.NewReader(answer)), out: &out, errw: &errb, tty: true}
		code := cmdSession(c, []string{"finish", "--root", e.repo})
		return out.String() + errb.String(), code
	}
	text, code := finish("n\n")
	if code == 0 || !strings.Contains(text, "Merge into main? [y/N]") || !strings.Contains(text, "Not merged") {
		t.Errorf("finish answered n: %d\n%s", code, text)
	}
	if gitOut(t, e.repo, "rev-parse", "HEAD") != before {
		t.Fatal("finish merged although the answer was no")
	}
	text, code = finish("y\n")
	if code != 0 || !strings.Contains(text, "Merged into main") {
		t.Fatalf("finish answered y: %d\n%s", code, text)
	}
	if gitOut(t, e.repo, "rev-parse", "HEAD") == before {
		t.Error("main did not move")
	}
	if raw, err := os.ReadFile(filepath.Join(e.repo, "notes.md")); err != nil || !strings.Contains(string(raw), "xyzzy") {
		t.Errorf("the merged file is not in the checkout: %v", err)
	}
	if out, _, code := runCLI("session", "--root", e.repo); code != 0 || !strings.Contains(out, "No session is open") {
		t.Errorf("session after finish: %d %s", code, out)
	}
	// The serving process sees that the session is gone.
	if text, _ := d.call("session_status", map[string]any{}); !strings.Contains(text, "No session is open") {
		t.Errorf("session_status after a terminal finish: %s", text)
	}
}

func TestConfirmMergeKeepsTheToolFromMerging(t *testing.T) {
	e, d, errb, _ := startWriting(t, "--confirm-merge")
	if text, isErr := d.call("apply_changes", createNotes); isErr {
		t.Fatalf("apply_changes: %s", text)
	}
	before := gitOut(t, e.repo, "rev-parse", "HEAD")
	text, isErr := d.call("finish_session", map[string]any{})
	want := "The session is ready to merge. The user chose to confirm merges in the terminal: ask them to run `repochat session finish`."
	if isErr || text != want {
		t.Errorf("finish_session with --confirm-merge: %v %q", isErr, text)
	}
	if gitOut(t, e.repo, "rev-parse", "HEAD") != before {
		t.Error("finish_session merged with --confirm-merge")
	}
	if _, err := os.Stat(filepath.Join(e.repo, "notes.md")); err == nil {
		t.Error("the session's file reached the checkout")
	}
	waitFor(t, "the request in the activity display", func() bool { return strings.Contains(errb.String(), "run `repochat session finish` to confirm") })
	if out, _, _ := runCLI("session", "--root", e.repo); !strings.Contains(out, "checkpoints  1 since the start") {
		t.Errorf("the session must still be open:\n%s", out)
	}
	// The tool keeps its name, schema and annotations.
	out, _, _ := runCLI("tools", "--root", e.repo, "--write", "--json")
	wantIn(t, out, `"name": "finish_session"`, `"destructiveHint": true`, `"squash"`)
}

func TestWritesAreRecordedWithoutContent(t *testing.T) {
	_, d, errb, _ := startWriting(t)
	text, isErr := d.call("apply_changes", createNotes)
	if isErr {
		t.Fatalf("apply_changes: %s", text)
	}
	cp := regexp.MustCompile(`checkpoint ([0-9a-f]{12})`).FindStringSubmatch(text)
	if cp == nil {
		t.Fatalf("no checkpoint in %s", text)
	}
	waitFor(t, "the apply in the activity display", func() bool { return strings.Contains(errb.String(), "applied checkpoint "+cp[1]) })
	wantIn(t, errb.String(), "1 file(s), +1 -0 lines")

	st, _ := stateDir()
	var line string
	waitFor(t, "the apply in the activity log", func() bool {
		raw, _ := os.ReadFile(filepath.Join(st, auditFile))
		for _, l := range strings.Split(string(raw), "\n") {
			if strings.Contains(l, `"kind":"session"`) && strings.Contains(l, `"tool":"apply_changes"`) {
				line = l
				return true
			}
		}
		return false
	})
	wantIn(t, line, `"checkpoint":"`+cp[1]+`"`, `"files":1`, `"added":1`, `"note":"applied checkpoint`)
	raw, _ := os.ReadFile(filepath.Join(st, auditFile))
	if strings.Contains(string(raw), "xyzzy") || strings.Contains(string(raw), "notes.md") {
		t.Errorf("the activity log holds what was written:\n%s", raw)
	}
	if out, _, _ := runCLI("log"); !strings.Contains(out, "applied checkpoint "+cp[1]) {
		t.Errorf("repochat log does not show the write:\n%s", out)
	}
}

func TestNewSessionWithoutWriteIsAnError(t *testing.T) {
	e := setup(t, "")
	for _, args := range [][]string{
		{"--root", e.repo, "--new-session"},
		{"mcp", "--root", e.repo, "--new-session"},
		{"--root", e.repo, "--confirm-merge"},
	} {
		_, errText, code := runCLI(args...)
		if code != 2 || !strings.Contains(errText, "only means something with --write") {
			t.Errorf("repochat %s: %d %s", strings.Join(args, " "), code, errText)
		}
	}
}

func TestWriteAndNoHistoryAreRefusedTogether(t *testing.T) {
	e := setup(t, "")
	for _, args := range [][]string{
		{"--root", e.repo, "--write", "--no-history"},
		{"mcp", "--root", e.repo, "--write", "--no-history"},
		{"tools", "--root", e.repo, "--write", "--no-history"},
	} {
		_, errText, code := runCLI(args...)
		if code != 2 || !strings.Contains(errText, "--write and --no-history cannot be used together") {
			t.Errorf("repochat %s: %d %s", strings.Join(args, " "), code, errText)
		}
	}
}

func TestTerminalCommandAndServerShareOneLock(t *testing.T) {
	e, d, _, _ := startWriting(t)
	if text, isErr := d.call("apply_changes", createNotes); isErr {
		t.Fatalf("apply_changes: %s", text)
	}
	out, errText, code := runCLI("session", "abandon", "--root", e.repo)
	if code != 0 || !strings.Contains(out+errText, "Session abandoned") {
		t.Fatalf("session abandon: %d %s %s", code, out, errText)
	}
	again := map[string]any{"changes": []any{map[string]any{"op": "create", "path": "more.md", "text": "more\n"}}}
	text, isErr := d.call("apply_changes", again)
	if !isErr || !strings.Contains(text, "no session is open") {
		t.Errorf("apply_changes after a terminal abandon: %v %s", isErr, text)
	}
	st, _ := stateDir()
	if found, _ := filepath.Glob(filepath.Join(st, "sessions", "*", "*", "w", "more.md")); len(found) > 0 {
		t.Errorf("written into a removed workspace: %v", found)
	}
	if _, err := os.Stat(filepath.Join(e.repo, "more.md")); err == nil {
		t.Error("written into the checkout")
	}
}

func TestSessionCommandWithoutASession(t *testing.T) {
	e := setup(t, "")
	out, errText, code := runCLI("session", "--root", e.repo)
	if code != 0 || !strings.Contains(out, "No session is open") {
		t.Errorf("session: %d %s %s", code, out, errText)
	}
	if _, errText, code := runCLI("session", "status", "--root", e.repo, "--squash"); code != 2 || !strings.Contains(errText, "--squash is an option of `repochat session finish`") {
		t.Errorf("session status --squash: %d %s", code, errText)
	}
	if _, _, code := runCLI("session", "--help"); code != 0 {
		t.Errorf("session --help: %d", code)
	}
	help, _, _ := runCLI("help")
	wantIn(t, help, "--write", "--new-session", "--confirm-merge", "repochat session finish", "repochat session abandon", "--delete-branch", "--squash")
}

// A client that starts `repochat mcp --write` itself gets what the launcher
// gives: the session is started (or resumed) before serving, and what the
// engine has to tell the owner goes to stderr.
func TestPlainMCPWriteStartsASession(t *testing.T) {
	e := setup(t, "")
	serve := func(args ...string) (string, string) {
		t.Helper()
		script := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}
{"jsonrpc":"2.0","id":1,"method":"tools/list"}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"session_status","arguments":{}}}
`
		in := filepath.Join(t.TempDir(), "in")
		os.WriteFile(in, []byte(script), 0o600)
		f, err := os.Open(in)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		var out, errb bytes.Buffer
		if code := Main(append([]string{"mcp", "--root", e.repo, "--write"}, args...), f, &out, &errb); code != 0 {
			t.Fatalf("mcp --write: %d %s", code, errb.String())
		}
		return out.String(), errb.String()
	}
	out, errText := serve()
	wantIn(t, errText, "WRITE MODE (experimental): session repochat/", "from main @ ")
	wantIn(t, out, `"name":"apply_changes"`, "checkpoint(s) since the start", "Write mode is on")
	first := regexp.MustCompile(`repochat/\d{4}-\d{2}-\d{2}-[0-9a-f]{8}`).FindString(errText)

	_, errText = serve("--new-session")
	second := ""
	if m := regexp.MustCompile(`WRITE MODE \(experimental\): session (repochat/\d{4}-\d{2}-\d{2}-[0-9a-f]{8})`).FindStringSubmatch(errText); m != nil {
		second = m[1]
	}
	if second == "" || second == first {
		t.Errorf("--new-session: %q then %q", first, second)
	}
	wantIn(t, errText, "session", "The previous session's branch "+first+" is kept")
}
