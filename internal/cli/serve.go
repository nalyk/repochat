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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nalyk/repochat/internal/guidance"
	"github.com/nalyk/repochat/internal/mcp"
	"github.com/nalyk/repochat/internal/repo"
)

// cmdMCP runs the MCP server on stdin/stdout. It is what tunnel-client (or
// any other stdio MCP client) starts.
func cmdMCP(c *console, args []string) int {
	// stderr belongs to the client, which may never read it. Everything this
	// command reports goes through a writer that cannot block, so an unread
	// pipe cannot stall the request loop or the tool workers.
	errw := newLossyWriter(c.errw)
	defer errw.Close(2 * time.Second)
	c.errw = errw

	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	var s session
	sessionFlags(fs, &s)
	writeFlags(fs, &s, true)
	blob := fs.String("session", "", "frozen session passed by the repochat launcher (internal)")
	allowBroad := fs.Bool("allow-broad-root", false, "allow serving your home directory or / when that is the repository root")
	if code, ok := parseOptions(c, fs, args); !ok {
		return code
	}
	// The server never needs credentials; make sure nothing it starts can
	// inherit them either.
	tunnelID := strings.TrimSpace(os.Getenv("CONTROL_PLANE_TUNNEL_ID"))
	if tunnelID == "" {
		tunnelID = strings.TrimSpace(os.Getenv(mcpTunnelVar))
	}
	guidanceKey := ""
	if tunnelID != "" {
		guidanceKey = guidance.Key(tunnelID)
	}
	os.Unsetenv(mcpTunnelVar)
	scrubSecretsFromEnv()

	var cfg *Config
	if *blob != "" {
		decoded, err := decodeSession(*blob)
		if err != nil {
			fmt.Fprintln(c.errw, "repochat:", err)
			return 2
		}
		s = *decoded
		if s.GuidanceKey != "" {
			guidanceKey = s.GuidanceKey
		}
		// The launcher has started or resumed the session already; this
		// process resumes that one and never starts another.
		s.NewSession = false
	} else {
		if err := s.checkWrite(); err != nil {
			fmt.Fprintln(c.errw, "repochat:", err)
			return 2
		}
		// User-level allow/deny patterns are part of the exposure policy: if
		// they cannot be read, refuse to serve rather than serve more.
		var err error
		if cfg, err = loadConfig(); err != nil {
			fmt.Fprintln(c.errw, "repochat:", err)
			return 1
		}
		s.ID = newID()
		// A client that starts this server itself passes what it reads on to
		// a model just as ChatGPT does, so the launcher's guard applies here
		// too. (A session from the launcher has been through it already.)
		if !*allowBroad {
			start := s.Root
			if start == "" {
				start = "."
			}
			if root, err := repo.FindRoot(start); err == nil {
				if err := broadRoot(root); err != nil {
					fmt.Fprintln(c.errw, "repochat:", repo.Sanitize(err.Error()))
					return 1
				}
			}
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	r, err := openRepo(ctx, &s, cfg)
	if err != nil {
		fmt.Fprintln(c.errw, "repochat:", err)
		return 1
	}
	defer r.Close()
	r.Logf = stderrLogf(c.errw)
	if guidanceKey != "" {
		if err := checkTunnelKey(ctx, r, guidanceKey); err != nil {
			fmt.Fprintln(c.errw, "repochat:", repo.Sanitize(err.Error()))
			return 1
		}
	}
	if _, err := r.Snapshot(ctx); err != nil {
		fmt.Fprintln(c.errw, "repochat:", err)
		return 1
	}

	sink := newEventSink(c, &s, r)
	defer sink.close()
	srv := newServer(r)
	if s.Write {
		ss, err := openSessions(r, s.repoOptions(cfg), sink.notice)
		if err != nil {
			fmt.Fprintln(c.errw, "repochat:", err)
			return 1
		}
		defer ss.Close()
		if *blob == "" {
			// Started by a client directly: like the launcher, start or
			// resume the session before serving, and say which it is.
			info, note, err := startSession(ctx, ss, s.NewSession)
			if err != nil {
				fmt.Fprintln(c.errw, "repochat:", repo.Sanitize(err.Error()))
				return 1
			}
			fmt.Fprintf(c.errw, "repochat: WRITE MODE (experimental): session %s, from %s @ %s\n",
				repo.SanitizeLine(info.Branch), repo.SanitizeLine(info.StartBranch), shortID(info.Base))
			sink.session(note)
		}
		srv = newWriteServer(r, ss, s.ConfirmMerge, sink.session)
	}
	srv.Logf = stderrLogf(c.errw)
	srv.OnEvent = sink.handle
	if guidanceKey != "" {
		if err := deliverGuidance(ctx, srv, r, guidanceKey, stderrLogf(c.errw)); err != nil {
			fmt.Fprintln(c.errw, "repochat:", repo.Sanitize(err.Error()))
			return 1
		}
	}

	err = srv.Serve(ctx, c.in, c.out)
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(c.errw, "repochat:", err)
		return 1
	}
	return 0
}

// deliverGuidance gives srv the owner's reviewed guidance for key, if there
// is any: the instructions that carry it and the guard that refuses every
// call once it no longer holds. With no guidance state, or no record for the
// key, srv is left exactly as it was. An error means the process must not
// serve.
func deliverGuidance(ctx context.Context, srv *mcp.Server, r *repo.Repo, key string, logf func(string, ...any)) error {
	store, err := openAssignments(r.Root)
	if err != nil || store == nil {
		return err
	}
	instr, g, err := guidance.Deliver(ctx, store, key, r.Name, srv.Instructions, r)
	if err != nil || g == nil {
		return err
	}
	g.Logf = logf
	srv.Instructions = instr
	srv.Guard = func(ctx context.Context, _ string) error { return g.Check(ctx) }
	return nil
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
	e.deliver(activityFrom(ev, e.s, e.r))
}

// notice passes on a notice from the session engine.
func (e *eventSink) notice(msg string) {
	e.session(sessionNote{Note: msg})
}

// session reports what a write did.
func (e *eventSink) session(n sessionNote) {
	e.deliver(sessionActivity(n, n.Tool, e.s, e.r))
}

// deliver sends one activity to the launcher, or prints it when there is
// none, and writes it to the activity log. Notes about writes are printed
// even with --quiet: they are what the owner has to know.
func (e *eventSink) deliver(a activity) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.conn != nil {
		line, _ := json.Marshal(ctlMessage{Op: "event", Activity: &a})
		e.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if _, err := e.conn.Write(append(line, '\n')); err != nil {
			e.conn.Close()
			e.conn = nil
		}
	} else if !e.s.Quiet || a.Kind == "session" {
		fmt.Fprintln(e.c.errw, formatActivity(e.c, a))
	}
	if e.audit != nil && (a.Kind == "call" || a.Kind == "hello" || a.Kind == "session") {
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
