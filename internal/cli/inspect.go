package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/nalyk/repochat/internal/guidance"
	"github.com/nalyk/repochat/internal/mcp"
	"github.com/nalyk/repochat/internal/repo"
	"github.com/nalyk/repochat/internal/tools"
)

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// cmdTools lists the tools the server exposes.
func cmdTools(c *console, args []string) int {
	fs := flag.NewFlagSet("tools", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	var s session
	sessionFlags(fs, &s)
	writeFlags(fs, &s, false)
	asJSON := fs.Bool("json", false, "print the exact tools/list payload")
	if code, ok := parseOptions(c, fs, args); !ok {
		return code
	}
	if err := s.checkWrite(); err != nil {
		c.fail("%v", err)
		return 2
	}
	ctx, stop := signalContext()
	defer stop()
	cfg, err := loadConfig()
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	r, err := openRepo(ctx, &s, cfg)
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	defer r.Close()
	list := tools.New(r)
	if s.Write {
		ss, err := openSessions(r, s.repoOptions(cfg), func(msg string) { c.warn("%s", msg) })
		if err != nil {
			c.fail("%v", err)
			return 1
		}
		defer ss.Close()
		list = writeTools(r, ss, false, nil)
	}
	if *asJSON {
		var payload []any
		for _, t := range list {
			payload = append(payload, mcp.ToolJSON(t))
		}
		enc := json.NewEncoder(c.out)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]any{"tools": payload})
		return 0
	}
	for _, t := range list {
		fmt.Fprintf(c.out, "%s  %s\n    %s\n", c.bold(t.Name), t.Schema.Signature(), t.Description)
	}
	return 0
}

func flagExit(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}

// parseOptions parses the command line of a command that takes options only.
// ok is false when the command must end with the returned exit code.
//
// The flag package stops at the first word that is not an option and ignores
// the rest. An unquoted pattern the shell expanded into several words
// (`--deny *.sql --no-history`) would then lose --no-history without a word
// and serve more than was asked for, so a stray word is an error.
func parseOptions(c *console, fs *flag.FlagSet, args []string) (code int, ok bool) {
	if err := fs.Parse(args); err != nil {
		return flagExit(err), false
	}
	if fs.NArg() > 0 {
		c.fail("unexpected argument %q: repochat %s takes options only.\n  Quote patterns that contain * or ?, and give each pattern its own --deny or --allow", fs.Arg(0), fs.Name())
		return 2, false
	}
	return 0, true
}

// cmdCall runs one tool through the real server pipeline and prints exactly
// the text a model would receive.
func cmdCall(c *console, args []string) int {
	fs := flag.NewFlagSet("call", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	var s session
	sessionFlags(fs, &s)
	writeFlags(fs, &s, false)
	era := fs.String("protocol", "legacy", "protocol era to use: legacy or modern (2026-07-28)")
	raw := fs.Bool("raw", false, "print the raw JSON-RPC response instead of the text")
	fs.Usage = func() {
		c.say("usage: repochat call [options] <tool> ['{\"arg\": value}']")
		c.say("example: repochat call search_code '{\"query\":\"TODO\",\"include\":[\"*.go\"]}'")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return flagExit(err)
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 2
	}
	if err := s.checkWrite(); err != nil {
		c.fail("%v", err)
		return 2
	}
	tool := fs.Arg(0)
	arguments := json.RawMessage("{}")
	if fs.NArg() == 2 {
		arguments = json.RawMessage(fs.Arg(1))
		var probe map[string]any
		if err := json.Unmarshal(arguments, &probe); err != nil {
			c.fail("arguments must be a JSON object, e.g. '{\"path\":\"README.md\"}': %v", err)
			return 2
		}
	}
	ctx, stop := signalContext()
	defer stop()
	cfg, err := loadConfig()
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	r, err := openRepo(ctx, &s, cfg)
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	defer r.Close()

	toolName, _ := json.Marshal(tool)
	var script string
	switch *era {
	case "legacy":
		script = `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"repochat-call","version":"1"}}}` + "\n" +
			fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%s,"arguments":%s}}`, toolName, compactJSON(arguments)) + "\n"
	case "modern":
		script = fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%s,"arguments":%s,"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`,
			toolName, compactJSON(arguments)) + "\n"
	default:
		c.fail("--protocol must be legacy or modern")
		return 2
	}
	srv := newServer(r)
	if s.Write {
		ss, err := openSessions(r, s.repoOptions(cfg), func(msg string) { c.warn("%s", msg) })
		if err != nil {
			c.fail("%v", err)
			return 1
		}
		defer ss.Close()
		record := terminalRecorder(c, &s, r, "")
		srv = newWriteServer(r, ss, false, func(n sessionNote) {
			c.say("%s %s", c.amber("session"), repo.SanitizeLine(n.Note))
			record(n)
		})
	}
	out, err := serveScript(ctx, srv, script)
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	var last string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var envelope struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal([]byte(line), &envelope) == nil && string(envelope.ID) == "1" {
			last = line
		}
	}
	if last == "" {
		c.fail("the server returned no response")
		return 1
	}
	if *raw {
		fmt.Fprintln(c.out, last)
		return 0
	}
	var reply struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(last), &reply); err != nil {
		c.fail("malformed response: %v", err)
		return 1
	}
	if reply.Error != nil {
		c.fail("protocol error %d: %s", reply.Error.Code, reply.Error.Message)
		if strings.HasPrefix(reply.Error.Message, "Unknown tool") {
			c.say("  `repochat tools` lists the available tools.")
		}
		return 1
	}
	for _, part := range reply.Result.Content {
		fmt.Fprintln(c.out, part.Text)
	}
	if reply.Result.IsError {
		c.say(c.dim("(the tool reported an error — this is the text the model would see)"))
		return 1
	}
	return 0
}

func compactJSON(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return "{}"
	}
	out, _ := json.Marshal(v)
	return string(out)
}

// cmdFiles shows, locally, exactly which files a session would serve and
// which it would withhold (with the reason). Nothing leaves the machine.
func cmdFiles(c *console, args []string) int {
	fs := flag.NewFlagSet("files", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	var s session
	sessionFlags(fs, &s)
	withheld := fs.Bool("withheld", false, "list the withheld files and why, instead of the served ones")
	if code, ok := parseOptions(c, fs, args); !ok {
		return code
	}
	ctx, stop := signalContext()
	defer stop()
	cfg, err := loadConfig()
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	r, err := openRepo(ctx, &s, cfg)
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	defer r.Close()
	snap, err := r.Snapshot(ctx)
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	for _, e := range snap.Entries {
		switch {
		case *withheld && e.Withheld != "":
			fmt.Fprintf(c.out, "%s\t%s\n", repo.SanitizeLine(e.Path), e.Withheld)
		case !*withheld && e.Withheld == "":
			fmt.Fprintln(c.out, repo.SanitizeLine(e.Path))
		}
	}
	summary := fmt.Sprintf("%d served (%d tracked, %d untracked)", snap.Visible, snap.Tracked, snap.Untracked)
	if n := snap.WithheldTotal(); n > 0 {
		summary += fmt.Sprintf(" · %d withheld (%s)", n, snap.WithheldSummary())
	}
	c.say(c.dim("%s · %s"), repo.SanitizeLine(r.Root), summary)
	for _, w := range append(append([]string{}, r.Warnings...), snap.Warnings...) {
		c.warn("%s", w)
	}
	if !*withheld && snap.WithheldTotal() > 0 {
		c.say(c.dim("`repochat files --withheld` shows what is held back; --allow / --deny / .repochatignore change it"))
	}
	return 0
}

// cmdMCPConfig prints configuration snippets for other stdio MCP clients.
func cmdMCPConfig(c *console, args []string) int {
	fs := flag.NewFlagSet("mcp-config", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	root := fs.String("root", "", "repository to serve (default: the one containing the current directory)")
	if code, ok := parseOptions(c, fs, args); !ok {
		return code
	}
	start := *root
	if start == "" {
		start = "."
	}
	repoRoot, err := repo.FindRoot(start)
	if err != nil {
		c.fail("this folder is not inside a Git repository")
		return 1
	}
	exe, err := selfPath()
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	snippet := map[string]any{"mcpServers": map[string]any{"repochat": map[string]any{
		"command": exe,
		"args":    []string{"mcp", "--root", repoRoot},
	}}}
	enc := json.NewEncoder(c.out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	enc.Encode(snippet)
	c.say(c.dim("Paste into any stdio MCP client (Claude Desktop, Claude Code .mcp.json, Cursor, VS Code…)."))
	c.say(c.dim("For Claude Code:  claude mcp add repochat -- %s"), repo.SanitizeLine(shellJoin([]string{exe, "mcp", "--root", repoRoot})))
	c.say(c.dim("This does not use the ChatGPT tunnel; the client starts repochat itself. No credentials are involved."))
	return 0
}

// cmdInstall copies the running binary into ~/.local/bin.
func cmdInstall(c *console, args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	dirFlag := fs.String("dir", "", "target directory (default: ~/.local/bin)")
	if code, ok := parseOptions(c, fs, args); !ok {
		return code
	}
	exe, err := selfPath()
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	dir := *dirFlag
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			c.fail("%v", err)
			return 1
		}
		dir = filepath.Join(home, ".local", "bin")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		c.fail("%v", err)
		return 1
	}
	dest := filepath.Join(dir, "repochat")
	if same, _ := sameFile(exe, dest); same {
		c.ok("already installed at %s", dest)
	} else {
		src, err := os.Open(exe)
		if err != nil {
			c.fail("%v", err)
			return 1
		}
		defer src.Close()
		tmp, err := os.CreateTemp(dir, ".repochat-*.tmp")
		if err != nil {
			c.fail("%v", err)
			return 1
		}
		defer os.Remove(tmp.Name())
		_, err = io.Copy(tmp, src)
		if err == nil {
			err = tmp.Chmod(0o755)
		}
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(tmp.Name(), dest)
		}
		if err != nil {
			c.fail("%v", err)
			return 1
		}
		c.ok("installed %s", dest)
	}
	onPath := false
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if p == dir {
			onPath = true
		}
	}
	if !onPath {
		c.warn("%s is not on your PATH. Add this line to your shell profile (~/.bashrc, ~/.zshrc):", dir)
		c.say("    export PATH=\"%s:$PATH\"", dir)
	} else {
		c.say("Now: cd into a repository and run %s.", c.bold("repochat"))
	}
	return 0
}

func sameFile(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(fa, fb), nil
}

// cmdInstallHelper installs (or reinstalls) tunnel-client privately.
func cmdInstallHelper(c *console, args []string) int {
	fs := flag.NewFlagSet("install-helper", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	version := fs.String("version", "", "tunnel-client `version` to install (default: "+helperPinnedVersion+", verified against checksums built into repochat)")
	fs.BoolVar(&c.yes, "yes", false, "do not ask for confirmation")
	if code, ok := parseOptions(c, fs, args); !ok {
		return code
	}
	ctx, stop := signalContext()
	defer stop()
	if _, err := installHelper(ctx, c, *version); err != nil {
		c.fail("%v", err)
		return 1
	}
	return 0
}

// instance is one tunnel in use on this machine.
type instance struct {
	Profile string    // saved profile name, or a description when there is none
	Owner   ownerInfo // what the launcher recorded when it took the tunnel
	Reply   *ctlReply // nil when nothing answers on the control socket
}

const (
	profileFromEnv = "(environment)"
	profileUnsaved = "(not a saved profile)"
)

// findInstances reports every tunnel that is in use on this machine. It goes
// by the runtime files, not by the saved profiles: a share started from
// environment variables in another shell, or from a profile removed since, is
// still a share, and "nothing is being shared" would be false.
//
// A launcher that answers on its control socket is running. When nothing
// answers but the tunnel's lock is still held, the launcher was killed and its
// tunnel-client (which inherits the lock) is still serving: that is reported
// too, with Reply == nil. Files left behind by an instance that is gone are
// ignored.
func findInstances(cfg *Config) []instance {
	base, err := stateDir()
	if err != nil {
		return nil
	}
	runDir := filepath.Join(base, "run")
	entries, err := os.ReadDir(runDir)
	if err != nil {
		return nil
	}
	names := map[string]string{}
	for name, p := range cfg.Profiles {
		names[guidance.Key(p.TunnelID)] = name
	}
	if id := strings.TrimSpace(os.Getenv("CONTROL_PLANE_TUNNEL_ID")); id != "" {
		if _, saved := names[guidance.Key(id)]; !saved {
			names[guidance.Key(id)] = profileFromEnv
		}
	}
	var found []instance
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(runDir, e.Name())
		raw, err := os.ReadFile(filepath.Join(dir, "owner.json"))
		if err != nil {
			continue
		}
		inst := instance{Profile: names[e.Name()]}
		if inst.Profile == "" {
			inst.Profile = profileUnsaved
		}
		if json.Unmarshal(raw, &inst.Owner) != nil {
			continue
		}
		if reply, err := ctlRequest(inst.Owner.Socket, "status", time.Second); err == nil && reply.OK {
			inst.Reply = reply
		} else if held, err := lockHeld(dir); err == nil && !held {
			continue // files left behind by an instance that is gone
		}
		// Held, or impossible to tell: reported, never passed over in silence.
		found = append(found, inst)
	}
	sort.Slice(found, func(i, j int) bool {
		return found[i].Profile+found[i].Owner.Repo < found[j].Profile+found[j].Owner.Repo
	})
	return found
}

// lockHeld reports whether some process holds the tunnel's ownership lock.
// The probe takes the lock itself for an instant, so it runs under the same
// coordination lock as a launcher's attempt to take ownership (takeOwnership):
// otherwise a launcher starting at that instant would find the lock busy and
// conclude that the tunnel is in use.
func lockHeld(dir string) (bool, error) {
	release, err := coordinate(dir)
	if err != nil {
		return false, err
	}
	defer release()
	f, err := os.Open(filepath.Join(dir, "lock"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	switch {
	case err == nil:
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false, nil
	case errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN):
		return true, nil
	}
	return false, err
}

// coordinate serialises looking at a tunnel's ownership lock with taking it.
// It is held only for the instant either takes, never while waiting for a
// person or for another process.
func coordinate(dir string) (release func(), err error) {
	f, err := os.OpenFile(filepath.Join(dir, "probe.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

// takeOwnership opens the tunnel's ownership lock and tries to take it. The
// file is returned either way; got says whether this process now owns the
// tunnel.
func takeOwnership(dir string) (lock *os.File, got bool, err error) {
	lock, err = os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if got, err = retryOwnership(dir, lock); err != nil {
		lock.Close()
		return nil, false, err
	}
	return lock, got, nil
}

// retryOwnership is one attempt to take an ownership lock that is already open.
func retryOwnership(dir string, lock *os.File) (bool, error) {
	release, err := coordinate(dir)
	if err != nil {
		return false, err
	}
	defer release()
	return tryLock(lock)
}

// selectInstances applies --profile. An unknown name is an error rather than
// an empty answer, which would read as "nothing is shared".
func selectInstances(c *console, cfg *Config, profile string) ([]instance, bool) {
	all := findInstances(cfg)
	if profile == "" {
		return all, true
	}
	if cfg.Profiles[profile] == nil {
		c.fail("no saved profile named %q. `repochat setup --list` shows the saved ones; without --profile every share is listed", profile)
		return nil, false
	}
	var out []instance
	for _, inst := range all {
		if inst.Profile == profile {
			out = append(out, inst)
		}
	}
	return out, true
}

const leftoverAdvice = "the tunnel may still be active: its lock is held (or could not be examined) although no launcher answers.\n  If a tunnel-client process is left over, end it yourself (repochat never signals a process it did not start)"

// cmdStatus reports whether repochat is currently sharing a repository.
func cmdStatus(c *console, args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	profile := fs.String("profile", "", "only this saved tunnel `name`")
	if code, ok := parseOptions(c, fs, args); !ok {
		return code
	}
	cfg, err := loadConfig()
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	found, ok := selectInstances(c, cfg, *profile)
	if !ok {
		return 1
	}
	if len(found) == 0 {
		c.say("repochat is not running. Nothing is being shared.")
		return 0
	}
	code := 0
	for _, inst := range found {
		if inst.Reply == nil {
			code = 1
			c.warn("the share of %s (profile %s) does not answer: %s", repo.SanitizeLine(inst.Owner.Repo), inst.Profile, leftoverAdvice)
			continue
		}
		fmt.Fprintf(c.out, "sharing %s · profile %s · state: %s · since %s\n", repo.SanitizeLine(inst.Reply.Repo), inst.Profile, inst.Reply.State, inst.Reply.Since)
	}
	return code
}

// cmdStop asks running repochat instances to stop sharing.
func cmdStop(c *console, args []string) int {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	profile := fs.String("profile", "", "only this saved tunnel `name`")
	if code, ok := parseOptions(c, fs, args); !ok {
		return code
	}
	cfg, err := loadConfig()
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	found, ok := selectInstances(c, cfg, *profile)
	if !ok {
		return 1
	}
	if len(found) == 0 {
		c.say("repochat is not running. Nothing to stop.")
		return 0
	}
	code := 0
	for _, inst := range found {
		if inst.Reply != nil {
			if reply, err := ctlRequest(inst.Owner.Socket, "stop", 3*time.Second); err == nil && reply.OK {
				c.ok("asked the repochat sharing %s (profile %s) to stop", repo.SanitizeLine(inst.Owner.Repo), inst.Profile)
				continue
			}
		}
		code = 1
		c.fail("cannot stop the share of %s (profile %s): %s", repo.SanitizeLine(inst.Owner.Repo), inst.Profile, leftoverAdvice)
	}
	return code
}
