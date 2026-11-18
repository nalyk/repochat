package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"repochat/internal/mcp"
	"repochat/internal/repo"
)

// cmdMCP runs the MCP server on stdin/stdout. It is what tunnel-client (or
// any other stdio MCP client) starts.
func cmdMCP(c *console, args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	var s session
	sessionFlags(fs, &s)
	blob := fs.String("session", "", "frozen session passed by the repochat launcher (internal)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// The server never needs credentials; make sure nothing it starts can
	// inherit them either.
	scrubSecretsFromEnv()

	var cfg *Config
	if *blob != "" {
		decoded, err := decodeSession(*blob)
		if err != nil {
			fmt.Fprintln(c.errw, "repochat:", err)
			return 2
		}
		s = *decoded
	} else {
		// User-level allow/deny patterns are part of the exposure policy: if
		// they cannot be read, refuse to serve rather than serve more.
		var err error
		if cfg, err = loadConfig(); err != nil {
			fmt.Fprintln(c.errw, "repochat:", err)
			return 1
		}
		s.ID = newID()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	r, err := openRepo(ctx, &s, cfg)
	if err != nil {
		fmt.Fprintln(c.errw, "repochat:", err)
		return 1
	}
	defer r.Close()
	if _, err := r.Snapshot(ctx); err != nil {
		fmt.Fprintln(c.errw, "repochat:", err)
		return 1
	}

	srv := newServer(r)
	srv.Logf = stderrLogf(c.errw)
	sink := newEventSink(c, &s, r)
	defer sink.close()
	srv.OnEvent = sink.handle

	err = srv.Serve(ctx, c.in, c.out)
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(c.errw, "repochat:", err)
		return 1
	}
	return 0
}

// eventSink delivers server events to the launcher (when there is one), the
// terminal and the local activity log.
type eventSink struct {
	mu    sync.Mutex
	c     *console
	s     *session
	r     *repo.Repo
	conn  net.Conn
	audit *auditLog
	warn  sync.Once
}

func newEventSink(c *console, s *session, r *repo.Repo) *eventSink {
	e := &eventSink{c: c, s: s, r: r}
	if s.Events != "" {
		if conn, err := net.DialTimeout("unix", s.Events, 2*time.Second); err == nil {
			e.conn = conn
		}
	}
	if !s.NoAudit {
		if a, err := openAudit(); err == nil {
			e.audit = a
		} else {
			fmt.Fprintln(c.errw, "repochat: activity log disabled:", err)
		}
	}
	return e
}

func (e *eventSink) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.conn != nil {
		e.conn.Close()
	}
}

func (e *eventSink) handle(ev mcp.Event) {
	a := activityFrom(ev, e.s, e.r)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.conn != nil {
		line, _ := json.Marshal(ctlMessage{Op: "event", Activity: &a})
		e.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if _, err := e.conn.Write(append(line, '\n')); err != nil {
			e.conn.Close()
			e.conn = nil
		}
	} else if !e.s.Quiet {
		fmt.Fprintln(e.c.errw, formatActivity(e.c, a))
	}
	if e.audit != nil && (a.Kind == "call" || a.Kind == "hello") {
		if err := e.audit.write(a); err != nil {
			e.warn.Do(func() { fmt.Fprintln(e.c.errw, "repochat: cannot write the activity log:", err) })
		}
	}
}

// ctlMessage is one line on the launcher's control socket.
type ctlMessage struct {
	Op       string    `json:"op"` // event | stop | status
	Activity *activity `json:"a,omitempty"`
}

// ctlReply answers stop and status requests.
type ctlReply struct {
	OK      bool   `json:"ok"`
	Repo    string `json:"repo,omitempty"`
	Since   string `json:"since,omitempty"`
	State   string `json:"state,omitempty"`
	Version string `json:"version,omitempty"`
}
