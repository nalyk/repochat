package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nalyk/repochat/internal/mcp"
)

// A scripted run (`repochat call`, the start-up self-test) reaches the end of
// its input while its calls are still running. The few seconds the server
// grants a client that hangs up must not decide whether those calls are
// answered: a search over a large repository with a cold cache takes longer.
func TestScriptedRunWaitsForSlowCalls(t *testing.T) {
	srv := &mcp.Server{
		Name: "test", Version: "1",
		DrainTimeout: 20 * time.Millisecond, // stands for "shorter than the call"
		Tools: []mcp.Tool{{Name: "slow", Description: "answers late", Handler: func(ctx context.Context, _ mcp.Args) (mcp.Result, error) {
			select {
			case <-time.After(400 * time.Millisecond):
				return mcp.Result{Text: "finished"}, nil
			case <-ctx.Done():
				return mcp.Result{}, ctx.Err()
			}
		}}},
	}
	script := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}` + "\n" +
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow","arguments":{}}}` + "\n"
	out, err := serveScript(context.Background(), srv, script)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "finished") {
		t.Fatalf("the answer of a call that outlasts the shutdown grace was dropped; the server wrote:\n%s", out)
	}
}

// The same through the real command, with a git that is slow the way it is on
// a large working tree: `repochat call` must print the result, not "the server
// returned no response".
func TestCallWaitsForAToolSlowerThanTheShutdownGrace(t *testing.T) {
	if testing.Short() {
		t.Skip("takes more than five seconds")
	}
	e := setup(t, "")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	slow := "#!/bin/sh\nfor a in \"$@\"; do if [ \"$a\" = status ]; then sleep 5.5; break; fi; done\nexec " + quoteArg(realGit) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(slow), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, errText, code := runCLI("call", "--root", e.repo, "git_status")
	if code != 0 || !strings.Contains(out, "BRANCH main") {
		t.Fatalf("call git_status with a slow git: exit %d\nstdout: %s\nstderr: %s", code, out, errText)
	}
}

// hastyServer is a stdio server that answers each line after a delay and
// exits the moment its input ends, dropping what it has not answered yet.
func hastyServer() int {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := sc.Text()
		go func() {
			time.Sleep(300 * time.Millisecond)
			fmt.Println("answer to " + line)
		}()
	}
	return 0
}

// muteServer reads its input and never answers.
func muteServer() int {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
	}
	time.Sleep(time.Minute)
	return 0
}

func roleCommand(ctx context.Context, t *testing.T, role string) *exec.Cmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, self)
	cmd.Env = append(os.Environ(), "REPOCHAT_TEST_ROLE="+role)
	return cmd
}

// doctor talks to `repochat mcp` as a client. A client keeps its side open
// until it has its answers; closing it right after sending starts the server's
// shutdown while the last call may still be running.
func TestSubprocessClientKeepsItsSideOpenUntilAnswered(t *testing.T) {
	lines, err := askSubprocess(roleCommand(context.Background(), t, "hasty-server"), "one\ntwo\n", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("expected both answers, got %d: %q", len(lines), lines)
	}
}

func TestSubprocessClientGivesUpAtTheDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	lines, err := askSubprocess(roleCommand(ctx, t, "mute-server"), "one\n", 1)
	if time.Since(start) > 10*time.Second {
		t.Fatalf("waited %s for a server that never answers", time.Since(start))
	}
	if err == nil || len(lines) != 0 {
		t.Fatalf("a server that never answers must be reported as an error, got %q, %v", lines, err)
	}
}
