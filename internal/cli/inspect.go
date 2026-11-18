package cli

import (
	"bytes"
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

	"repochat/internal/mcp"
	"repochat/internal/repo"
	"repochat/internal/tools"
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
	asJSON := fs.Bool("json", false, "print the exact tools/list payload")
	if err := fs.Parse(args); err != nil {
		return flagExit(err)
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

// cmdCall runs one tool through the real server pipeline and prints exactly
// the text a model would receive.
func cmdCall(c *console, args []string) int {
	fs := flag.NewFlagSet("call", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	var s session
	sessionFlags(fs, &s)
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
	var out bytes.Buffer
	if err := srv.Serve(ctx, strings.NewReader(script), &out); err != nil {
		c.fail("%v", err)
		return 1
	}
	var last string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
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
	if err := fs.Parse(args); err != nil {
		return flagExit(err)
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
	c.say(c.dim("%s · %s"), r.Root, summary)
	for _, w := range snap.Warnings {
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
	if err := fs.Parse(args); err != nil {
		return flagExit(err)
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
	c.say(c.dim("For Claude Code:  claude mcp add repochat -- %s"), shellJoin([]string{exe, "mcp", "--root", repoRoot}))
	c.say(c.dim("This does not use the ChatGPT tunnel; the client starts repochat itself. No credentials are involved."))
	return 0
}

// cmdInstall copies the running binary into ~/.local/bin.
func cmdInstall(c *console, args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	dirFlag := fs.String("dir", "", "target directory (default: ~/.local/bin)")
	if err := fs.Parse(args); err != nil {
		return flagExit(err)
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
	if err := fs.Parse(args); err != nil {
		return flagExit(err)
	}
	ctx, stop := signalContext()
	defer stop()
	if _, err := installHelper(ctx, c, *version); err != nil {
		c.fail("%v", err)
		return 1
	}
	return 0
}

// runningInstances finds live repochat launchers for the saved profiles.
func runningInstances(cfg *Config, only string) map[string]*ctlReply {
	found := map[string]*ctlReply{}
	ids := map[string]string{}
	for name, p := range cfg.Profiles {
		if only == "" || only == name {
			ids[name] = p.TunnelID
		}
	}
	if id := strings.TrimSpace(os.Getenv("CONTROL_PLANE_TUNNEL_ID")); id != "" && only == "" {
		ids["(environment)"] = id
	}
	for name, id := range ids {
		dir, err := tunnelRunDir(id)
		if err != nil {
			continue
		}
		if reply, err := ctlRequest(readOwner(dir).Socket, "status", time.Second); err == nil && reply.OK {
			found[name] = reply
		}
	}
	return found
}

// cmdStatus reports whether repochat is currently sharing a repository.
func cmdStatus(c *console, args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	profile := fs.String("profile", "", "only this saved tunnel `name`")
	if err := fs.Parse(args); err != nil {
		return flagExit(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	found := runningInstances(cfg, *profile)
	if len(found) == 0 {
		c.say("repochat is not running. Nothing is being shared.")
		return 0
	}
	names := make([]string, 0, len(found))
	for n := range found {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		r := found[n]
		fmt.Fprintf(c.out, "sharing %s · profile %s · state: %s · since %s\n", r.Repo, n, r.State, r.Since)
	}
	return 0
}

// cmdStop asks running repochat instances to stop sharing.
func cmdStop(c *console, args []string) int {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	profile := fs.String("profile", "", "only this saved tunnel `name`")
	if err := fs.Parse(args); err != nil {
		return flagExit(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	stopped := 0
	ids := map[string]string{}
	for name, p := range cfg.Profiles {
		if *profile == "" || *profile == name {
			ids[name] = p.TunnelID
		}
	}
	if id := strings.TrimSpace(os.Getenv("CONTROL_PLANE_TUNNEL_ID")); id != "" && *profile == "" {
		ids["(environment)"] = id
	}
	for name, id := range ids {
		dir, err := tunnelRunDir(id)
		if err != nil {
			continue
		}
		owner := readOwner(dir)
		if reply, err := ctlRequest(owner.Socket, "stop", 3*time.Second); err == nil && reply.OK {
			stopped++
			c.ok("asked the repochat sharing %s (profile %s) to stop", owner.Repo, name)
		}
	}
	if stopped == 0 {
		c.say("repochat is not running. Nothing to stop.")
	}
	return 0
}
