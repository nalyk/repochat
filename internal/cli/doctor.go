package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nalyk/repochat/internal/repo"
)

// cmdDoctor checks every part of the setup separately and says, for each,
// whether it works, fails, or was not tested.
func cmdDoctor(c *console, args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	var s session
	sessionFlags(fs, &s)
	profile := fs.String("profile", "", "check this saved tunnel `name`")
	offline := fs.Bool("offline", false, "skip the check that contacts OpenAI")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	failed := 0
	pass := func(what, detail string) { c.say("%s %-22s %s", c.green("✓"), what, detail) }
	bad := func(what, detail, fix string) {
		failed++
		c.say("%s %-22s %s", c.red("✗"), what, detail)
		if fix != "" {
			c.say("  %-22s → %s", "", fix)
		}
	}
	skip := func(what, detail string) { c.say("%s %-22s %s", c.dim("–"), what, c.dim("not tested: "+detail)) }

	c.say("%s %s", c.bold("repochat doctor"), versionString())

	// git
	if path, err := exec.LookPath("git"); err != nil {
		bad("git", "not found on PATH", "install git")
	} else {
		pass("git", path)
	}

	// configuration
	cfg, cfgErr := loadConfig()
	cfgPath, _ := configPath()
	switch {
	case cfgErr != nil:
		bad("configuration", cfgErr.Error(), "")
		cfg = &Config{Profiles: map[string]*Profile{}}
	case len(cfg.Profiles) == 0:
		pass("configuration", "none saved yet ("+cfgPath+")")
	default:
		pass("configuration", fmt.Sprintf("%s · %d profile(s), private", cfgPath, len(cfg.Profiles)))
	}

	// repository
	var r *repo.Repo
	start := s.Root
	if start == "" {
		start = "."
	}
	if root, err := repo.FindRoot(start); err != nil {
		bad("repository", "this folder is not inside a Git repository", "cd into a project folder and run doctor again")
	} else if r, err = repo.Open(ctx, root, s.repoOptions(cfg)); err != nil {
		bad("repository", err.Error(), "")
		r = nil
	} else {
		defer r.Close()
		if snap, err := r.Snapshot(ctx); err != nil {
			bad("repository", err.Error(), "")
			r = nil
		} else {
			detail := fmt.Sprintf("%s · git %s · %d files served", r.Root, r.GitVersion(), snap.Visible)
			if n := snap.WithheldTotal(); n > 0 {
				detail += fmt.Sprintf(", %d withheld (%s)", n, snap.WithheldSummary())
			}
			pass("repository", detail)
			for _, w := range snap.Warnings {
				c.warn("%s", w)
			}
		}
	}

	// MCP server: a real subprocess, both protocol eras
	if r == nil {
		skip("MCP server", "no repository")
	} else if err := subprocessSelfTest(ctx, r.Root, &s); err != nil {
		bad("MCP server", err.Error(), "this is a bug in repochat; please report it at "+projectURL+"/issues")
	} else {
		pass("MCP server", "answers in a subprocess: handshake, tool list and a tool call, legacy and 2026-07-28 protocols")
	}

	// helper
	helper, source, err := findHelper()
	helperOK := false
	switch {
	case errors.Is(err, errHelperMissing):
		bad("tunnel-client", "not installed", "run `repochat install-helper` (or just `repochat`, which offers to install it)")
	case err != nil:
		bad("tunnel-client", err.Error(), "")
	default:
		text, v, verr := helperVersion(ctx, helper)
		switch {
		case verr != nil:
			bad("tunnel-client", verr.Error(), "reinstall it with `repochat install-helper`")
		case v != [3]int{} && versionOlder(v, helperMin):
			bad("tunnel-client", fmt.Sprintf("%s is version %s; %d.%d.%d or newer is required", helper, text, helperMin[0], helperMin[1], helperMin[2]),
				"run `repochat install-helper`")
		default:
			helperOK = true
			pass("tunnel-client", fmt.Sprintf("%s · %s (%s)", helper, text, source))
		}
	}

	// credentials: syntax
	root := ""
	if r != nil {
		root = r.Root
	}
	name, prof, perr := cfg.selectProfile(*profile, root)
	creds, have := resolveCredentials(prof)
	credsOK := false
	switch {
	case perr != nil:
		bad("credentials", perr.Error(), "")
	case !have:
		bad("credentials", "no tunnel ID and API key saved for profile \""+name+"\"", "run `repochat` in a terminal; it asks for them once")
	default:
		if err := validateCredentials(creds); err != nil {
			bad("credentials", err.Error(), "run `repochat setup`")
		} else {
			credsOK = true
			from := "profile " + name
			if creds.FromEnv {
				from = "environment"
			}
			pass("credentials", fmt.Sprintf("well-formed · tunnel %s · from %s", maskID(creds.TunnelID), from))
		}
	}

	// authentication: live metadata lookup
	switch {
	case *offline:
		skip("OpenAI: tunnel lookup", "--offline")
	case !helperOK || !credsOK:
		skip("OpenAI: tunnel lookup", "needs tunnel-client and credentials")
	default:
		info, err := verifyTunnel(ctx, helper, creds.TunnelID, creds.APIKey)
		if err != nil {
			bad("OpenAI: tunnel lookup", err.Error(), "")
			explainVerify(c, err.Error())
		} else {
			detail := "the key can read the tunnel"
			if info.Name != "" {
				detail += " \"" + info.Name + "\""
			}
			pass("OpenAI: tunnel lookup", detail)
			if info.known && len(info.Workspaces) == 0 {
				c.warn("the tunnel is not attached to a ChatGPT workspace; ChatGPT will not offer it until it is (%s)", tunnelsURL)
			}
		}
	}

	// polling / remote: only observable while repochat runs
	running := false
	if credsOK {
		if dir, err := tunnelRunDir(creds.TunnelID); err == nil {
			if reply, err := ctlRequest(readOwner(dir).Socket, "status", time.Second); err == nil && reply.OK {
				running = true
				detail := fmt.Sprintf("repochat is running for %s — state: %s", reply.Repo, reply.State)
				switch reply.State {
				case "authenticated", "remote activity", "serving":
					pass("OpenAI: tunnel polling", detail)
				default:
					skip("OpenAI: tunnel polling", detail)
				}
				if reply.State == "serving" {
					pass("ChatGPT: tool call", "at least one tool call was served in this run")
				} else {
					skip("ChatGPT: tool call", "no tool call served yet in this run — ask @RepoChat something in ChatGPT")
				}
			}
		}
	}
	if !running {
		skip("OpenAI: tunnel polling", "only observable while `repochat` is running")
		if prof != nil && prof.Onboarded {
			pass("ChatGPT: tool call", "a tool call was served through this tunnel in an earlier run")
		} else {
			skip("ChatGPT: tool call", "start `repochat` and ask @RepoChat something in ChatGPT")
		}
	}

	c.say("")
	if failed > 0 {
		c.say("%s", c.red(fmt.Sprintf("%d problem(s) found.", failed)))
		return 1
	}
	c.say("%s", c.green("Everything that could be tested works."))
	return 0
}

// subprocessSelfTest starts `repochat mcp` exactly as a client would and
// talks to it over real pipes, in both protocol eras.
func subprocessSelfTest(ctx context.Context, root string, s *session) error {
	exe, err := selfPath()
	if err != nil {
		return err
	}
	frozen := *s
	frozen.Root = root
	frozen.NoAudit, frozen.Quiet = true, true
	frozen.ID = "doctor"
	const meta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"repochat-doctor","version":"1"}}`
	scripts := []struct{ era, input string }{
		{"legacy", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"repochat-doctor","version":"1"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"repo_overview","arguments":{}}}
`},
		{"2026-07-28", `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{` + meta + `}}
{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{` + meta + `}}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"repo_overview","arguments":{},` + meta + `}}
`},
	}
	for _, sc := range scripts {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		cmd := exec.CommandContext(cctx, exe, "mcp", "--session", frozen.encode())
		cmd.Dir = root
		cmd.Stdin = strings.NewReader(sc.input)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		cancel()
		if err != nil {
			return fmt.Errorf("%s: the server exited with an error: %v %s", sc.era, err, firstLine(stderr.String()))
		}
		lines := bytes.Split(bytes.TrimSpace(stdout.Bytes()), []byte("\n"))
		if len(lines) != 3 {
			return fmt.Errorf("%s: expected 3 replies, got %d", sc.era, len(lines))
		}
		for i, line := range lines {
			var reply struct {
				Error  json.RawMessage `json:"error"`
				Result struct {
					IsError bool `json:"isError"`
				} `json:"result"`
			}
			if err := json.Unmarshal(line, &reply); err != nil {
				return fmt.Errorf("%s: reply %d is not valid JSON", sc.era, i+1)
			}
			if len(reply.Error) > 0 || reply.Result.IsError {
				return fmt.Errorf("%s: request %d failed: %s", sc.era, i+1, clip(string(line), 200))
			}
		}
	}
	return nil
}
