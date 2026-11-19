package cli

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// stuckWriter is a stderr nobody reads: every write blocks until released.
type stuckWriter struct{ release chan struct{} }

func (s stuckWriter) Write(p []byte) (int, error) {
	<-s.release
	return len(p), nil
}

// gatedWriter collects what it is given, but only once its gate is open.
type gatedWriter struct {
	gate chan struct{}
	mu   sync.Mutex
	buf  bytes.Buffer
}

func (g *gatedWriter) Write(p []byte) (int, error) {
	<-g.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.Write(p)
}

func (g *gatedWriter) String() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.String()
}

func TestLossyWriterNeverBlocksTheCaller(t *testing.T) {
	stuck := stuckWriter{release: make(chan struct{})}
	defer close(stuck.release)
	w := newLossyWriter(stuck)
	start := time.Now()
	for i := 0; i < 10_000; i++ {
		fmt.Fprintf(w, "line %d\n", i)
	}
	w.Close(50 * time.Millisecond)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("writing to a reader that never reads took %s", took)
	}
	// Writing after Close is harmless too (late diagnostics during shutdown).
	fmt.Fprintln(w, "late")
}

func TestLossyWriterDeliversInOrder(t *testing.T) {
	sink := &gatedWriter{gate: make(chan struct{})}
	close(sink.gate)
	w := newLossyWriter(sink)
	for i := 0; i < 100; i++ {
		fmt.Fprintf(w, "line %d\n", i)
	}
	w.Close(5 * time.Second)
	var want strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&want, "line %d\n", i)
	}
	if sink.String() != want.String() {
		t.Fatalf("lines lost or reordered:\n%s", sink.String())
	}
}

func TestLossyWriterSaysWhenItDroppedLines(t *testing.T) {
	sink := &gatedWriter{gate: make(chan struct{})}
	w := newLossyWriter(sink)
	for i := 0; i < lossyQueue+500; i++ {
		fmt.Fprintf(w, "line %d\n", i)
	}
	close(sink.gate) // the reader comes back
	w.Close(5 * time.Second)
	got := sink.String()
	if !strings.HasPrefix(got, "line 0\n") || !strings.Contains(got, "dropped") {
		t.Fatalf("a reader that fell behind must be told that lines are missing; got %d bytes ending in %q", len(got), got[max(0, len(got)-200):])
	}
}

// A stdio client may pipe the server's stderr and never read it. The server
// must go on answering, and must still end when its input closes.
func TestServerSurvivesAClientThatNeverReadsStderr(t *testing.T) {
	e := setup(t, "")
	in, feed, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out := &lockedBuffer{}
	stuck := stuckWriter{release: make(chan struct{})}
	defer close(stuck.release)
	done := make(chan int, 1)
	go func() { done <- Main([]string{"mcp", "--root", e.repo, "--no-audit-log"}, in, out, stuck) }()

	const requests = 50
	fmt.Fprintln(feed, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`)
	for i := 1; i <= requests; i++ {
		fmt.Fprintf(feed, `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"git_refs","arguments":{}}}`+"\n", i)
	}
	answers := func() int { return strings.Count(out.String(), "\n") }
	for deadline := time.Now().Add(20 * time.Second); answers() < requests+1 && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if got := answers(); got != requests+1 {
		t.Fatalf("the server answered %d of %d requests while its stderr was not being read", got, requests+1)
	}
	feed.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code %d", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the server did not end after its input closed")
	}
}
