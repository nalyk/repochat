package cli

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// `repochat 2>&1 | tee log`, then Ctrl+C: tee dies with the same signal, and
// the launcher's "Stopping…" goes into a pipe nobody reads. A process that
// writes to a broken pipe on stderr is killed on the spot unless it says
// otherwise — here before it had stopped tunnel-client, which then goes on
// serving (on macOS nothing else ends it).
func TestInterruptWithABrokenStderrStillStopsTheHelper(t *testing.T) {
	e := setup(t, "")
	saveProfile(t, testKey)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "--root", e.repo)
	cmd.Env = append(os.Environ(), "REPOCHAT_TEST_ROLE=repochat")
	cmd.Stderr = w
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	t.Cleanup(func() { cmd.Process.Kill() })

	connected := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "connected to OpenAI") {
				connected <- true
				return
			}
		}
		connected <- false
	}()
	select {
	case ok := <-connected:
		if !ok {
			t.Fatal("the launcher ended before it connected")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the launcher did not connect")
	}
	dir, err := tunnelRunDir(testTunnel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "owner.json")); err != nil {
		t.Fatalf("the launcher should own the tunnel by now: %v", err)
	}

	r.Close() // the reader of stderr is gone
	cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				t.Fatalf("the launcher was killed by %v instead of stopping the share", ws.Signal())
			}
			t.Fatalf("the launcher exited with %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the launcher did not stop")
	}
	if _, err := os.Stat(filepath.Join(dir, "owner.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the launcher did not release the tunnel (owner.json: %v)", err)
	}
	if held, err := lockHeld(dir); held || err != nil {
		t.Error("something still holds the tunnel's lock: tunnel-client was not stopped")
	}
}

// Ctrl+C at a yes/no question must abort, not wait for Enter and then take
// the default answer.
func TestInterruptAtAQuestionAborts(t *testing.T) {
	// Keep the signal from ending the test binary whatever confirm does.
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, os.Interrupt)
	defer signal.Stop(guard)

	in, feed, err := os.Pipe() // input that never arrives
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	defer feed.Close()
	var shown lockedBuffer
	c := &console{in: in, rd: bufio.NewReader(in), out: io.Discard, errw: &shown, tty: true}

	answer := make(chan bool, 1)
	go func() { answer <- c.confirm("Stop that one and serve this repository instead?", true) }()
	waitFor(t, "the question", func() bool { return bytes.Contains([]byte(shown.String()), []byte("[Y/n]")) })
	syscall.Kill(os.Getpid(), syscall.SIGINT)
	select {
	case yes := <-answer:
		if yes || !c.interrupted {
			t.Fatalf("Ctrl+C at a question must mean no and be reported as an interruption (answer %v, interrupted %v)", yes, c.interrupted)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Ctrl+C at a question was swallowed: still waiting for Enter")
	}
}

// brokenPipe is a stderr whose reader exits when told to.
type brokenPipe struct {
	lockedBuffer
	broken chan struct{}
}

func (b *brokenPipe) Write(p []byte) (int, error) {
	select {
	case <-b.broken:
		return 0, syscall.EPIPE
	default:
		return b.lockedBuffer.Write(p)
	}
}

// When the reader of the launcher's output goes away on its own, nobody can
// see what is being read and no terminal is left to stop it from. The share
// ends instead of going on unseen.
func TestShareEndsWhenItsDisplayIsGone(t *testing.T) {
	e := setup(t, "")
	saveProfile(t, testKey)
	display := &brokenPipe{broken: make(chan struct{})}
	done := make(chan int, 1)
	go func() { done <- Main([]string{"--root", e.repo}, nil, io.Discard, display) }()
	waitFor(t, "the share to start", func() bool { return strings.Contains(display.String(), "connected to OpenAI") })
	dir, err := tunnelRunDir(testTunnel)
	if err != nil {
		t.Fatal(err)
	}

	close(display.broken)
	// The next thing the launcher prints is the next activity line.
	conn, err := net.Dial("unix", readOwner(dir).Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte(`{"op":"event","a":{"kind":"call","tool":"read_file","ok":true}}` + "\n"))

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the launcher went on sharing after its display was gone")
	}
	if held, err := lockHeld(dir); held || err != nil {
		t.Error("something still holds the tunnel's lock: tunnel-client was not stopped")
	}
}
