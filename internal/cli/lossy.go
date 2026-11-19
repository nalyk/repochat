package cli

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// lossyQueue is how many diagnostic lines may wait for a slow reader.
const lossyQueue = 512

// lossyWriter hands diagnostics to another writer from a goroutine of its
// own, so that writing them never blocks the caller.
//
// The MCP server's stderr belongs to its client. A client may pipe it and
// never read it; a direct write would then block for ever once the pipe is
// full, and with it the request loop and the tool workers that report through
// it. Lines that do not fit in the queue are dropped and counted, and a reader
// that comes back is told how many it missed.
type lossyWriter struct {
	mu      sync.Mutex
	queue   chan []byte
	closed  bool
	dropped int
	done    chan struct{}
}

func newLossyWriter(w io.Writer) *lossyWriter {
	l := &lossyWriter{queue: make(chan []byte, lossyQueue), done: make(chan struct{})}
	go func() {
		defer close(l.done)
		for line := range l.queue {
			w.Write(line)
			l.mu.Lock()
			missed := l.dropped
			l.dropped = 0
			l.mu.Unlock()
			if missed > 0 {
				fmt.Fprintf(w, "repochat: %d diagnostic line(s) were dropped because nothing was reading them\n", missed)
			}
		}
	}()
	return l
}

// Write queues p and returns at once. It never fails.
func (l *lossyWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return len(p), nil
	}
	select {
	case l.queue <- append([]byte(nil), p...):
	default:
		l.dropped++
	}
	return len(p), nil
}

// Close stops accepting lines and waits up to wait for the queued ones to be
// written. Later writes are discarded.
func (l *lossyWriter) Close(wait time.Duration) {
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.queue)
	}
	l.mu.Unlock()
	select {
	case <-l.done:
	case <-time.After(wait):
	}
}
