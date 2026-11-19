package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nalyk/repochat/internal/mcp"
	"github.com/nalyk/repochat/internal/repo"
)

const (
	projectURL = "https://github.com/nalyk/repochat"
	pluginsURL = "https://chatgpt.com/plugins"
	tunnelsURL = "https://platform.openai.com/settings/organization/tunnels"
	apiKeysURL = "https://platform.openai.com/settings/organization/api-keys"
)

type runOptions struct {
	session
	Profile        string
	Verbose        bool
	PrintCommand   bool
	AllowBroadRoot bool
}

// cmdRun is plain `repochat`: serve the current repository to ChatGPT.
// On first use it walks the user through everything that is missing.
func cmdRun(c *console, args []string) int {
	fs := flag.NewFlagSet("repochat", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	var o runOptions
	sessionFlags(fs, &o.session)
	fs.StringVar(&o.Profile, "profile", "", "use this saved tunnel `name` (default: the default profile)")
	fs.BoolVar(&c.yes, "yes", false, "answer yes to confirmations (install the helper, take over a running instance)")
	fs.BoolVar(&o.Verbose, "verbose", false, "show tunnel-client's own log output")
	fs.BoolVar(&o.PrintCommand, "print-command", false, "print the tunnel-client command that would be run, then exit")
	fs.BoolVar(&o.AllowBroadRoot, "allow-broad-root", false, "allow serving your home directory or / when that is the repository root")
	fs.Usage = func() { printHelp(c) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		c.fail("unexpected argument %q", fs.Arg(0))
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	// A process that writes to a broken pipe on stderr is killed on the spot
	// unless it handles SIGPIPE. `repochat 2>&1 | tee log` followed by Ctrl+C
	// is exactly that: tee is gone when "Stopping…" is printed, and repochat
	// would die before it had stopped tunnel-client. With the signal ignored
	// the write merely fails, and a display that has gone away is noticed
	// (see serve) instead of ending the launcher mid-flight.
	signal.Ignore(syscall.SIGPIPE)
	display := &watchedWriter{w: c.errw, gone: make(chan struct{})}
	c.errw = display

	l := &launcher{c: c, o: o, events: make(chan activity, 256), stopReq: make(chan struct{}, 1), started: time.Now(), displayGone: display.gone}
	code, err := l.run(ctx)
	if errors.Is(err, errInterrupted) {
		return 130
	}
	if err != nil {
		c.fail("%v", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// broadRoot refuses a repository root that is the home directory or the
// file system root: serving it would expose far more than a project.
func broadRoot(root string) error {
	home, _ := os.UserHomeDir()
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	if root == "/" || (home != "" && root == home) {
		return fmt.Errorf("the repository root is %s — serving that would expose far more than a project.\n  cd into a project folder, or pass --allow-broad-root if you really mean it", root)
	}
	return nil
}

type launcher struct {
	c *console
	o runOptions

	cfg      *Config
	profName string
	prof     *Profile
	creds    credentials
	r        *repo.Repo
	snap     *repo.Snapshot
	helper   string
	helperV  string

	dir      string
	lock     *os.File
	ln       net.Listener
	sockPath string
	started  time.Time

	events  chan activity
	stopReq chan struct{}
	// displayGone is closed when the terminal output can no longer be written.
	displayGone <-chan struct{}

	mu    sync.Mutex
	state string
	lost  bool // tunnel-client cannot reach OpenAI right now
}

func (l *launcher) setState(s string) {
	l.mu.Lock()
	l.state = s
	l.mu.Unlock()
}

const stateReconnecting = "reconnecting"

// setTrouble records whether tunnel-client currently fails to reach OpenAI.
func (l *launcher) setTrouble(lost bool) {
	l.mu.Lock()
	l.lost = lost
	l.mu.Unlock()
}

// reportedState is the state shown by `repochat status` and `repochat doctor`.
// How far the share got (authenticated, serving, …) says nothing about now:
// while the connection is down it is not working, and is reported as such.
func (l *launcher) reportedState() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lost {
		return stateReconnecting
	}
	return l.state
}

func (l *launcher) run(ctx context.Context) (int, error) {
	c := l.c

	// 1. The repository (and git) come first: nothing is downloaded or asked
	// for until we know there is something to serve.
	start := l.o.Root
	if start == "" {
		start = "."
	}
	root, err := repo.FindRoot(start)
	if errors.Is(err, repo.ErrNotRepo) {
		return 1, errors.New("this folder is not inside a Git repository.\n  cd into a project folder (one that has a .git directory) and run `repochat` again")
	}
	if err != nil {
		return 1, err
	}
	if !l.o.AllowBroadRoot {
		if err := broadRoot(root); err != nil {
			return 1, err
		}
	}
	if l.cfg, err = loadConfig(); err != nil {
		return 1, err
	}
	if l.r, err = repo.Open(ctx, root, l.o.repoOptions(l.cfg)); err != nil {
		return 1, err
	}
	defer l.r.Close()
	if l.snap, err = l.r.Snapshot(ctx); err != nil {
		return 1, err
	}

	// 2. Which tunnel.
	if l.profName, l.prof, err = l.cfg.selectProfile(l.o.Profile, l.r.Root); err != nil {
		return 1, err
	}
	creds, haveCreds := resolveCredentials(l.prof)
	firstRun := !haveCreds
	if firstRun {
		if !c.tty {
			return 1, errors.New("no tunnel is set up yet and there is no terminal to ask on.\n  Run `repochat` in a terminal once, or set CONTROL_PLANE_TUNNEL_ID and CONTROL_PLANE_API_KEY")
		}
		c.say("")
		c.say("%s  Let's connect this repository to ChatGPT. Two short steps, one time only.", c.bold("Welcome to RepoChat."))
	}

	// 3. The helper.
	if l.helper, l.helperV, err = ensureHelper(ctx, c); err != nil {
		return 1, err
	}

	// 4. Credentials.
	if firstRun {
		if l.prof, err = credentialWizard(ctx, c, l.cfg, l.profName, l.helper, wizardOptions{}); err != nil {
			return 1, err
		}
		creds, _ = resolveCredentials(l.prof)
	}
	if err := validateCredentials(creds); err != nil {
		return 1, fmt.Errorf("%v. Run `repochat setup` to enter them again", err)
	}
	l.creds = creds

	// 5. Prove the server works before anything is exposed.
	if err := selfTest(ctx, l.r); err != nil {
		return 1, fmt.Errorf("internal self-test failed, not starting: %v", err)
	}

	if l.o.PrintCommand {
		sess := l.o.session.freeze(l.cfg, l.r.Root)
		argv, err := l.helperArgs(sess, "<health-url-file>")
		if err != nil {
			return 1, err
		}
		fmt.Fprintln(c.out, shellJoin(append([]string{l.helper}, argv...)))
		c.say(c.dim("(the API key is passed in the environment as CONTROL_PLANE_API_KEY, never on the command line)"))
		return 0, nil
	}

	// 6. One repochat per tunnel.
	if err := l.acquire(ctx); err != nil {
		return 1, err
	}
	defer l.release()

	return l.serve(ctx)
}

// ---------------------------------------------------------------------------
// Tunnel ownership and the control socket
// ---------------------------------------------------------------------------

type ownerInfo struct {
	Repo    string `json:"repo"`
	Started string `json:"started"`
	Socket  string `json:"socket"`
}

func tunnelRunDir(tunnelID string) (string, error) {
	base, err := stateDir()
	if err != nil {
		return "", err
	}
	if err := privateDir(base); err != nil {
		return "", err
	}
	dir := filepath.Join(base, "run", tunnelKey(tunnelID))
	if err := privateDir(filepath.Join(base, "run")); err != nil {
		return "", err
	}
	if err := privateDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

func readOwner(dir string) ownerInfo {
	var info ownerInfo
	if raw, err := os.ReadFile(filepath.Join(dir, "owner.json")); err == nil {
		_ = json.Unmarshal(raw, &info)
	}
	return info
}

// ctlRequest sends one request to a running instance and returns its reply.
func ctlRequest(socket, op string, timeout time.Duration) (*ctlReply, error) {
	if socket == "" {
		return nil, errors.New("no control socket recorded")
	}
	conn, err := net.DialTimeout("unix", socket, timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	line, _ := json.Marshal(ctlMessage{Op: op})
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return nil, err
	}
	raw, err := bufio.NewReader(io.LimitReader(conn, 64<<10)).ReadBytes('\n')
	if err != nil && len(raw) == 0 {
		return nil, err
	}
	var reply ctlReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

func tryLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return false, err
}

// acquire makes this process the single owner of the tunnel on this machine.
// Ownership is an OS lock on a file that is never deleted. A running owner is
// only ever asked to stop over its private control socket — no process is
// signalled by PID.
func (l *launcher) acquire(ctx context.Context) error {
	c := l.c
	dir, err := tunnelRunDir(l.creds.TunnelID)
	if err != nil {
		return err
	}
	l.dir = dir
	lock, got, err := takeOwnership(dir)
	if err != nil {
		return err
	}
	if !got {
		owner := readOwner(dir)
		what := "another repository"
		if owner.Repo != "" {
			what = repo.SanitizeLine(owner.Repo)
		}
		if owner.Repo == l.r.Root {
			lock.Close()
			return fmt.Errorf("repochat is already running for this repository (in another terminal). Stop it there with Ctrl+C, or run `repochat stop`")
		}
		c.say("")
		c.say("This tunnel is already in use by another repochat, serving %s.", c.bold(what))
		if !c.confirm("Stop that one and serve this repository instead?", true) {
			lock.Close()
			if c.interrupted {
				return errInterrupted
			}
			if !c.tty && !c.yes {
				return errors.New("the tunnel is in use. Stop the other repochat, or pass --yes to take over")
			}
			return errors.New("left the other instance running")
		}
		reply, rerr := ctlRequest(owner.Socket, "stop", 3*time.Second)
		if rerr != nil || !reply.OK {
			lock.Close()
			return errors.New("the other instance holds the tunnel but does not answer.\n  Stop it yourself (Ctrl+C in its terminal; if a leftover `tunnel-client` process remains, end it), then run repochat again.\n  Not starting a second connection on the same tunnel")
		}
		deadline := time.Now().Add(15 * time.Second)
		for !got && time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				lock.Close()
				return errInterrupted
			case <-time.After(150 * time.Millisecond):
			}
			if got, err = retryOwnership(dir, lock); err != nil {
				lock.Close()
				return err
			}
		}
		if !got {
			lock.Close()
			return errors.New("the other instance did not stop in time; not starting a second connection on the same tunnel")
		}
		c.ok("the other instance stopped")
	}
	l.lock = lock

	// We own the tunnel now, so any socket file left behind is stale.
	sock := filepath.Join(dir, "ctl.sock")
	if len(sock) > 100 { // sun_path is ~104 bytes on macOS, 108 on Linux
		tmp, err := os.MkdirTemp("", "repochat-")
		if err != nil {
			return err
		}
		sock = filepath.Join(tmp, "ctl.sock")
	}
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("cannot create the control socket: %w", err)
	}
	_ = os.Chmod(sock, 0o600)
	l.ln, l.sockPath = ln, sock
	info, _ := json.Marshal(ownerInfo{Repo: l.r.Root, Started: l.started.UTC().Format(time.RFC3339), Socket: sock})
	if err := os.WriteFile(filepath.Join(dir, "owner.json"), info, 0o600); err != nil {
		return err
	}
	go l.acceptLoop()
	return nil
}

func (l *launcher) release() {
	if l.ln != nil {
		l.ln.Close()
		_ = os.Remove(l.sockPath)
		if dir := filepath.Dir(l.sockPath); dir != l.dir {
			_ = os.Remove(dir)
		}
	}
	if l.dir != "" {
		_ = os.Remove(filepath.Join(l.dir, "owner.json"))
		_ = os.Remove(filepath.Join(l.dir, "health.url"))
	}
	if l.lock != nil {
		l.lock.Close() // releases the lock; the file itself stays
	}
}

func (l *launcher) acceptLoop() {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			return
		}
		go l.handleConn(conn)
	}
}

func (l *launcher) handleConn(conn net.Conn) {
	defer conn.Close()
	// An oversized line is skipped, not fatal: the MCP server reports every
	// call on this connection, and losing it would blind the live view for
	// the rest of the session.
	eachLine(conn, maxLogLine, func(line []byte) {
		var msg ctlMessage
		if json.Unmarshal(line, &msg) != nil {
			return
		}
		switch msg.Op {
		case "event":
			if msg.Activity != nil {
				select {
				case l.events <- *msg.Activity:
				default: // never block the server on a slow terminal
				}
			}
		case "stop":
			reply, _ := json.Marshal(ctlReply{OK: true})
			conn.Write(append(reply, '\n'))
			select {
			case l.stopReq <- struct{}{}:
			default:
			}
			conn.Close() // one request per connection
		case "status":
			reply, _ := json.Marshal(ctlReply{OK: true, Repo: l.r.Root, Since: l.started.UTC().Format(time.RFC3339), State: l.reportedState(), Version: Version})
			conn.Write(append(reply, '\n'))
			conn.Close()
		}
	})
}

// ---------------------------------------------------------------------------
// Running tunnel-client
// ---------------------------------------------------------------------------

// quoteArg quotes one word for tunnel-client's --mcp.command parser
// (single quotes are literal; a quote itself is written as '\”).
func quoteArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if a != "" && strings.IndexFunc(a, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@%+,", r))
		}) < 0 {
			parts[i] = a
		} else {
			parts[i] = quoteArg(a)
		}
	}
	return strings.Join(parts, " ")
}

func selfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if strings.ContainsAny(exe, "\n\r\x00") {
		return "", errors.New("repochat is installed at a path containing control characters; move it")
	}
	return exe, nil
}

// helperArgs builds the tunnel-client command line. The API key is never
// part of it: tunnel-client is told to read it from its environment.
func (l *launcher) helperArgs(sess session, healthURLFile string) ([]string, error) {
	exe, err := selfPath()
	if err != nil {
		return nil, err
	}
	command := quoteArg(exe) + " mcp --session " + quoteArg(sess.encode())
	argv := []string{
		"run",
		"--control-plane.tunnel-id", l.creds.TunnelID,
		"--control-plane.api-key", "env:CONTROL_PLANE_API_KEY",
		"--mcp.command", command,
		"--mcp.stdio-send-initialized-notification",
		"--health.listen-addr", "127.0.0.1:0",
		"--health.url-file", healthURLFile,
	}
	override, err := controlPlaneOverride()
	if err != nil {
		return nil, err
	}
	argv = append(argv, override...)
	return argv, nil
}

// controlPlaneOverride is a test hook for the contract suite: it points
// tunnel-client at a mock control plane. Only loopback addresses are
// accepted, so it cannot be used to send a repository anywhere else.
func controlPlaneOverride() ([]string, error) {
	raw := os.Getenv("REPOCHAT_CONTROL_PLANE_URL")
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return nil, errors.New("REPOCHAT_CONTROL_PLANE_URL must be an http(s) URL")
	}
	if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() {
		return nil, errors.New("REPOCHAT_CONTROL_PLANE_URL is a test hook and only accepts loopback addresses")
	}
	return []string{"--control-plane.base-url", raw}, nil
}

func (l *launcher) serve(ctx context.Context) (int, error) {
	c := l.c
	sess := l.o.session.freeze(l.cfg, l.r.Root)
	sess.Events = l.sockPath
	healthFile := filepath.Join(l.dir, "health.url")
	_ = os.Remove(healthFile)
	argv, err := l.helperArgs(sess, healthFile)
	if err != nil {
		return 1, err
	}

	logPath := filepath.Join(l.dir, "tunnel-client.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return 1, err
	}
	defer logFile.Close()

	l.banner()

	cmd := exec.Command(l.helper, argv...)
	cmd.Dir = l.r.Root
	cmd.Env = helperEnv("CONTROL_PLANE_API_KEY=" + l.creds.APIKey)
	cmd.Stdin = nil
	var sink io.Writer = logFile
	if l.o.Verbose {
		sink = io.MultiWriter(logFile, prefixWriter{w: c.errw, prefix: c.dim("  tunnel-client │ "), secret: l.creds.APIKey})
	}
	cmd.Stdout, cmd.Stderr = sink, sink
	cmd.SysProcAttr = helperProcAttr()
	// tunnel-client inherits the ownership lock: if repochat is killed
	// outright, the tunnel still counts as in use until tunnel-client ends,
	// so a second connection can never be started next to a survivor.
	cmd.ExtraFiles = []*os.File{l.lock}
	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("cannot start tunnel-client: %w", err)
	}
	l.setState("starting")
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	stopHelper := func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(6 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-exited
		}
	}

	mon := &healthMonitor{file: healthFile}
	tick := time.NewTicker(400 * time.Millisecond)
	defer tick.Stop()
	var localOK, authOK, sawRemote, served, troubled bool
	startDeadline := time.Now().Add(45 * time.Second)

	// checkHealth reports state changes read from tunnel-client's health
	// endpoint. stop=true means the run must end with the returned code.
	checkHealth := func() (code int, stop bool) {
		st := mon.poll()
		switch {
		case !localOK && st.ready:
			localOK = true
			l.setState("local ready")
			c.ok("tunnel-client %s is running", l.helperV)
			tick.Reset(1 * time.Second)
		case !localOK && time.Now().After(startDeadline):
			c.fail("tunnel-client did not become ready in 45 seconds.")
			l.explainFailure(logPath)
			stopHelper()
			return 1, true
		}
		if localOK && !authOK {
			switch {
			case st.polled:
				authOK, troubled = true, false
				l.setTrouble(false)
				l.setState("authenticated")
				c.ok("connected to OpenAI — the tunnel is authenticated and polling")
				l.afterConnected()
				tick.Reset(5 * time.Second)
			case st.httpStatus == 401 || st.httpStatus == 403 || st.httpStatus == 404:
				c.fail("OpenAI rejected the tunnel connection (HTTP %d).", st.httpStatus)
				l.explainAuth(st.httpStatus)
				stopHelper()
				return 1, true
			case st.failure != "" && !troubled:
				troubled = true
				l.setTrouble(true)
				c.warn("cannot reach OpenAI yet (%s); tunnel-client keeps retrying…", st.failure)
			}
		} else if authOK {
			switch {
			case st.failures >= 3 && !troubled:
				troubled = true
				l.setTrouble(true)
				c.warn("lost contact with OpenAI (%s); tunnel-client keeps retrying…", orDefault(st.failure, "no successful poll"))
			case st.failures == 0 && st.polled && troubled:
				troubled = false
				l.setTrouble(false)
				c.ok("reconnected to OpenAI")
			}
		}
		return 0, false
	}

	for {
		select {
		case <-ctx.Done():
			c.say("")
			c.say("Stopping…")
			stopHelper()
			c.say("Stopped. ChatGPT can no longer reach this repository.")
			return 0, nil

		case <-l.stopReq:
			c.say("")
			c.say("Another repochat is taking over this tunnel — stopping.")
			stopHelper()
			return 0, nil

		case <-l.displayGone:
			// Nobody can see what is being read any more, and there is no
			// terminal left to stop it from: do not go on sharing unseen.
			stopHelper()
			return 1, nil

		case err := <-exited:
			code := 1
			var ee *exec.ExitError
			if errors.As(err, &ee) && ee.ExitCode() > 0 {
				code = ee.ExitCode()
			}
			c.say("")
			c.fail("tunnel-client stopped unexpectedly%s.", exitNote(err))
			l.explainFailure(logPath)
			return code, nil

		case a := <-l.events:
			if !authOK {
				// A request can only arrive through a working tunnel, so make
				// sure the connection state is reported before the activity.
				if code, stop := checkHealth(); stop {
					return code, nil
				}
			}
			if a.Kind == "hello" && !sawRemote {
				sawRemote = true
				l.setState("remote activity")
				c.ok("remote MCP activity detected — %s is discovering the tools", clientLabel(a.Client))
			}
			if !l.o.Quiet {
				c.say("%s", formatActivity(c, a))
			}
			if a.Kind == "call" && a.OK && !served {
				served = true
				l.setState("serving")
				if l.prof != nil && !l.prof.Onboarded && !l.creds.FromEnv {
					l.prof.Onboarded = true
					if err := updateProfile(l.profName, l.creds.TunnelID, func(p *Profile) { p.Onboarded = true }); err == nil {
						c.ok("first tool call served — setup is complete. From now on: %s in any repository.", c.bold("repochat"))
					}
				}
			}

		case <-tick.C:
			if code, stop := checkHealth(); stop {
				return code, nil
			}
		}
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func clientLabel(name string) string {
	if name == "" {
		return "a client"
	}
	return "\"" + name + "\" (self-reported)"
}

func exitNote(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Sprintf(" (exit code %d)", ee.ExitCode())
	}
	return ""
}

// watchedWriter passes output through and reports, once, that its
// destination is gone (the reader of a pipe has exited).
type watchedWriter struct {
	w    io.Writer
	once sync.Once
	gone chan struct{}
}

func (d *watchedWriter) Write(p []byte) (int, error) {
	n, err := d.w.Write(p)
	if errors.Is(err, syscall.EPIPE) {
		d.once.Do(func() { close(d.gone) })
	}
	return n, err
}

type prefixWriter struct {
	w      io.Writer
	prefix string
	secret string // never echoed
}

func (p prefixWriter) Write(b []byte) (int, error) {
	text := string(b)
	if p.secret != "" {
		text = strings.ReplaceAll(text, p.secret, "[api key]")
	}
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		fmt.Fprintln(p.w, p.prefix+repo.SanitizeLine(line))
	}
	return len(b), nil
}

func (l *launcher) banner() {
	c := l.c
	c.say("")
	c.say("%s %s · read-only", c.bold("RepoChat"), versionString())
	c.say("  repository  %s  %s", c.bold(l.r.Name), c.dim(repo.SanitizeLine(l.r.Root)))
	serving := fmt.Sprintf("%d files", l.snap.Visible)
	if n := l.snap.WithheldTotal(); n > 0 {
		serving += fmt.Sprintf(" · %d withheld by policy (%s)", n, l.snap.WithheldSummary())
	}
	if l.o.TrackedOnly {
		serving += " · tracked files only"
	}
	if l.o.NoHistory {
		serving += " · no history"
	}
	if l.o.NoRedact {
		serving += " · secret masking OFF"
	}
	c.say("  serving     %s", serving)
	tunnel := maskID(l.creds.TunnelID)
	if l.prof != nil && l.prof.TunnelName != "" && !l.creds.FromEnv {
		tunnel = l.prof.TunnelName + " (" + tunnel + ")"
	}
	source := "profile " + l.profName
	if l.creds.FromEnv {
		source = "from environment"
	}
	c.say("  tunnel      %s · %s", tunnel, source)
	c.say("  audience    whoever can use this plugin in ChatGPT can read these files while repochat runs")
	for _, w := range append(append([]string{}, l.r.Warnings...), l.snap.Warnings...) {
		c.warn("%s", w)
	}
	if l.prof != nil && !l.creds.FromEnv {
		if last := l.prof.LastRepo; last != "" && last != l.r.Root {
			c.say("  %s this tunnel last served %s — chats that used it will now see THIS repository", c.amber("note"), repo.SanitizeLine(last))
		}
		if l.prof.LastRepo != l.r.Root {
			l.prof.LastRepo = l.r.Root
			root := l.r.Root
			_ = updateProfile(l.profName, l.creds.TunnelID, func(p *Profile) { p.LastRepo = root })
		}
	}
	c.say("")
}

func (l *launcher) afterConnected() {
	c := l.c
	onboarded := l.creds.FromEnv || (l.prof != nil && l.prof.Onboarded)
	c.say("")
	if onboarded {
		c.say("  In ChatGPT, type %s in a chat and ask about this repository.", c.bold("@RepoChat"))
		c.say("  %s", c.dim("Plugin not there yet? Run `repochat help chatgpt` for the one-time steps. Ctrl+C stops sharing."))
	} else {
		printChatGPTSteps(c, l.prof)
		c.say("  %s", c.dim("Leave this running. I'll confirm here when the first request arrives. Ctrl+C stops sharing."))
	}
	c.say("")
}

func printChatGPTSteps(c *console, p *Profile) {
	tunnel := "your tunnel"
	if p != nil && p.TunnelName != "" {
		tunnel = "\"" + p.TunnelName + "\""
	}
	c.say("  %s", c.bold("Last step — add the plugin in ChatGPT (one time):"))
	c.say("    1. Open %s  →  +  →  Add custom MCP server", c.cyan(pluginsURL))
	c.say("    2. Name: RepoChat   Connection: %s → pick %s   Authentication: No authentication", c.bold("Tunnel"), tunnel)
	c.say("    3. Create as a plugin (personal, not workspace-wide), then Install")
	c.say("    4. In a new chat type:  %s", c.bold("@RepoChat give me an overview of this repository"))
}

// explainFailure shows the end of tunnel-client's log.
func (l *launcher) explainFailure(logPath string) {
	c := l.c
	raw, err := os.ReadFile(logPath)
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		c.say("  (tunnel-client printed nothing; log: %s)", logPath)
		return
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > 12 {
		lines = lines[len(lines)-12:]
	}
	c.say("  Last lines of its log:")
	for _, line := range lines {
		line = strings.ReplaceAll(line, l.creds.APIKey, "[api key]")
		c.say("    %s", c.dim(clip(repo.SanitizeLine(line), 400)))
	}
	c.say("  Full log: %s", logPath)
	if status := rejectionInLog(string(raw)); status != 0 {
		l.explainAuth(status)
	} else {
		c.say("  `repochat doctor` checks each part of the setup separately.")
	}
}

// A rejection is an HTTP status that is named as one (`status=401`,
// `"status":403`, `HTTP 404`, `status code 404`) or that comes with its reason
// phrase (`401 Unauthorized`). Bare digits are not evidence: timestamps and
// port numbers are full of them, and "not found" is said about files too.
var rejectionRe = regexp.MustCompile(`(?i)(?:\b(?:http|status|code)\w*\b[^0-9a-z\n]{0,8}(401|403|404)\b)|(?:\b(401)\s+unauthorized\b)|(?:\b(403)\s+forbidden\b)|(?:\b(404)\s+not\s+found\b)`)

// rejectionInLog returns the HTTP status (401, 403 or 404) with which the
// control plane rejected tunnel-client according to its log, or 0. The last
// one mentioned wins: it is the one tunnel-client gave up on.
func rejectionInLog(log string) int {
	status := 0
	for _, m := range rejectionRe.FindAllStringSubmatch(log, -1) {
		for _, group := range m[1:] {
			if group != "" {
				status, _ = strconv.Atoi(group)
			}
		}
	}
	return status
}

func (l *launcher) explainAuth(status int) {
	c := l.c
	switch status {
	case 401:
		c.say("  → The API key was not accepted. Create a new one at %s and run `repochat setup`.", apiKeysURL)
	case 403:
		c.say("  → The API key is valid but may not use this tunnel. It needs the Tunnels permissions Read + Use")
		c.say("    (restricted key), in the same organization as the tunnel: %s", apiKeysURL)
	case 404:
		c.say("  → The tunnel was not found for this key. Check the tunnel ID at %s and run `repochat setup`.", tunnelsURL)
	}
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

type healthState struct {
	ready      bool   // /readyz answered 200
	polled     bool   // the control plane accepted at least one poll
	failures   int    // consecutive poll failures
	failure    string // failure category of the last failed poll
	httpStatus int
}

// healthMonitor reads tunnel-client's loopback health endpoint. It only ever
// talks to a loopback address, uses no proxy and follows no redirects.
type healthMonitor struct {
	file   string
	base   string
	client *http.Client
}

func (h *healthMonitor) resolve() bool {
	if h.base != "" {
		return true
	}
	raw, err := os.ReadFile(h.file)
	if err != nil {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(string(raw)))
	if err != nil || u.Scheme != "http" || u.User != nil {
		return false
	}
	if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() {
		return false
	}
	h.base = "http://" + u.Host
	h.client = &http.Client{
		Timeout:       2 * time.Second,
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return true
}

func (h *healthMonitor) get(path string) (int, []byte) {
	resp, err := h.client.Get(h.base + path)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	return resp.StatusCode, body
}

func (h *healthMonitor) poll() healthState {
	var st healthState
	if !h.resolve() {
		return st
	}
	code, _ := h.get("/readyz")
	st.ready = code == http.StatusOK
	code, body := h.get("/health/control-plane")
	if code != http.StatusOK {
		return st
	}
	var cp struct {
		Details struct {
			LastSuccess         string `json:"last_success"`
			ConsecutiveFailures int    `json:"consecutive_failures"`
			FailureCategory     string `json:"failure_category"`
			HTTPStatus          int    `json:"http_status"`
		} `json:"details"`
	}
	if json.Unmarshal(body, &cp) != nil {
		return st
	}
	st.polled = cp.Details.LastSuccess != ""
	st.failures = cp.Details.ConsecutiveFailures
	st.failure = cp.Details.FailureCategory
	st.httpStatus = cp.Details.HTTPStatus
	if st.failure == "http_error" && st.httpStatus != 0 {
		st.failure = fmt.Sprintf("HTTP %d", st.httpStatus)
	}
	return st
}

// ---------------------------------------------------------------------------
// Self-test
// ---------------------------------------------------------------------------

// selfTest drives the real server pipeline in memory: handshake, tool list
// and one tool call, in both protocol eras.
func selfTest(ctx context.Context, r *repo.Repo) error {
	const meta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`
	scripts := map[string]string{
		"legacy": `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"repochat-selftest","version":"1"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"repo_overview","arguments":{}}}
`,
		"modern": `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{` + meta + `}}
{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{` + meta + `}}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"repo_overview","arguments":{},` + meta + `}}
`,
	}
	for era, script := range scripts {
		replies, err := runScript(ctx, r, script)
		if err != nil {
			return fmt.Errorf("%s era: %w", era, err)
		}
		if len(replies) != 3 {
			return fmt.Errorf("%s era: expected 3 replies, got %d", era, len(replies))
		}
		for id, reply := range replies {
			if reply.Error != nil {
				return fmt.Errorf("%s era: request %s failed: %s", era, id, reply.Error.Message)
			}
		}
		var call struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(replies["3"].Result, &call); err != nil || call.IsError || len(call.Content) == 0 {
			text := ""
			if len(call.Content) > 0 {
				text = firstLine(call.Content[0].Text)
			}
			return fmt.Errorf("%s era: repo_overview failed: %s", era, text)
		}
	}
	return nil
}

type scriptReply struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// serveScript feeds newline-delimited requests to srv in memory and returns
// everything the server wrote.
//
// The end of a script is not a client hanging up: its calls are still running
// when the input runs out. They are waited for as long as a call may take, not
// for the few seconds the server grants a departing client; otherwise a call
// slower than that grace (a search over a large working tree with a cold
// cache) would end without an answer.
func serveScript(ctx context.Context, srv *mcp.Server, script string) ([]byte, error) {
	call := srv.CallTimeout
	if call <= 0 {
		call = mcp.DefaultCallTimeout
	}
	srv.DrainTimeout = call + 5*time.Second
	var out bytes.Buffer
	if err := srv.Serve(ctx, strings.NewReader(script), &out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// runScript feeds newline-delimited requests to a fresh in-memory server and
// returns the replies keyed by request id.
func runScript(ctx context.Context, r *repo.Repo, script string) (map[string]scriptReply, error) {
	out, err := serveScript(ctx, newServer(r), script)
	if err != nil {
		return nil, err
	}
	replies := map[string]scriptReply{}
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var reply scriptReply
		if err := json.Unmarshal(line, &reply); err != nil {
			return nil, fmt.Errorf("malformed reply: %v", err)
		}
		replies[strings.Trim(string(reply.ID), `"`)] = reply
	}
	return replies, nil
}
