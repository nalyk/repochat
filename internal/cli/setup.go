package cli

import (
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

type wizardOptions struct {
	TunnelID string // pre-supplied (flag or environment)
	APIKey   string // pre-supplied (environment or key file)
	NoVerify bool
	Bind     string // repository root to bind the profile to
}

var pasteLabels = map[string]bool{
	"export": true, "set": true, "control_plane_tunnel_id": true, "control_plane_api_key": true,
	"openai_api_key": true, "tunnel": true, "tunnel_id": true, "id": true, "key": true, "api": true,
	"api_key": true, "apikey": true, "and": true, "bearer": true,
}

// parseCredentialPaste finds one tunnel ID and one API key in pasted text,
// in any order and with any punctuation between them. It never returns the
// pasted text in an error.
func parseCredentialPaste(text string) (id, key string, err error) {
	tokens := strings.FieldsFunc(text, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
	})
	for _, tok := range tokens {
		switch {
		case pasteLabels[strings.ToLower(tok)]:
		case tunnelIDRe.MatchString(tok):
			if id != "" && id != tok {
				return "", "", errors.New("that contained two different tunnel IDs; paste exactly one")
			}
			id = tok
		case strings.HasPrefix(tok, "tunnel_"):
			return "", "", errors.New("that tunnel ID looks incomplete (expected tunnel_ followed by 32 lowercase letters/digits)")
		case apiKeyRe.MatchString(tok):
			if key != "" && key != tok {
				return "", "", errors.New("that contained two different API keys; paste exactly one")
			}
			key = tok
		default:
			return "", "", errors.New("part of that was neither a tunnel ID nor an API key; nothing was saved")
		}
	}
	return id, key, nil
}

// credentialWizard collects the tunnel ID and API key, verifies them live
// and saves them. It is the only place credentials are ever typed.
func credentialWizard(ctx context.Context, c *console, cfg *Config, profile, helper string, o wizardOptions) (*Profile, error) {
	id, key := o.TunnelID, o.APIKey
	interactive := id == "" || key == ""
	if interactive {
		if !c.tty {
			return nil, errors.New("credentials are missing and there is no terminal to ask on. Set CONTROL_PLANE_TUNNEL_ID and CONTROL_PLANE_API_KEY, or run `repochat setup` in a terminal")
		}
		c.say("")
		c.say("%s", c.bold("Step 1 of 2 — two values from OpenAI Platform"))
		c.say("  a) Create a tunnel and attach it to your ChatGPT workspace:")
		c.say("       %s", c.cyan(tunnelsURL))
		c.say("     Copy its ID (it starts with tunnel_).")
		c.say("  b) Create an API key — Restricted, with Tunnels: Read + Use:")
		c.say("       %s", c.cyan(apiKeysURL))
		c.say("")
		// No other question is asked before the hidden prompt: anything typed
		// from here on must not be echoed.
		for attempt := 0; attempt < 5 && (id == "" || key == ""); attempt++ {
			prompt := "Paste the tunnel ID and the API key (any order), then press Enter: "
			switch {
			case id != "" && key == "":
				prompt = "Got the tunnel ID. Now paste the API key, then press Enter: "
			case id == "" && key != "":
				prompt = "Got the API key. Now paste the tunnel ID, then press Enter: "
			}
			text, err := c.readSecret(prompt)
			if err != nil {
				return nil, err
			}
			gotID, gotKey, perr := parseCredentialPaste(text)
			if perr != nil {
				c.warn("%v", perr)
				continue
			}
			if gotID != "" {
				id = gotID
			}
			if gotKey != "" {
				key = gotKey
			}
			if gotID == "" && gotKey == "" {
				c.warn("nothing recognisable there — the input is hidden while you paste; try again.")
			}
		}
		if id == "" || key == "" {
			return nil, errors.New("did not get both values. Run `repochat` again when you have them")
		}
	}
	if err := validateCredentials(credentials{TunnelID: id, APIKey: key}); err != nil {
		return nil, err
	}

	if o.Bind != "" {
		if err := checkTunnelUnassigned(id, o.Bind); err != nil {
			return nil, err
		}
	}
	p := &Profile{TunnelID: id, APIKey: key}
	if old := cfg.Profiles[profile]; old != nil && old.TunnelID == id {
		p.Onboarded, p.BoundRepo, p.LastRepo = old.Onboarded, old.BoundRepo, old.LastRepo
	}
	if o.Bind != "" {
		p.BoundRepo = o.Bind
	}

	if !o.NoVerify && helper != "" {
		c.say("Checking the tunnel with OpenAI…")
		info, err := verifyTunnel(ctx, helper, id, key)
		switch {
		case err == nil:
			p.TunnelName = info.Name
			name := info.Name
			if name == "" {
				name = maskID(id)
			}
			c.ok("tunnel \"%s\" is readable with this key", name)
			if info.known && len(info.Workspaces) == 0 {
				c.warn("this tunnel is not attached to a ChatGPT workspace yet, so ChatGPT will not offer it.")
				c.say("  Attach it at %s (edit the tunnel → workspace).", tunnelsURL)
			}
		case errors.Is(err, errInterrupted):
			return nil, err
		default:
			c.fail("could not verify the tunnel: %v", err)
			explainVerify(c, err.Error())
			if !c.confirm("Save these values anyway?", false) {
				if c.interrupted {
					return nil, errInterrupted
				}
				return nil, errors.New("nothing was saved")
			}
		}
	}

	set := func(c *Config) {
		c.Profiles[profile] = p
		if c.DefaultProfile == "" {
			c.DefaultProfile = profile
		}
	}
	if _, err := updateConfig(set); err != nil {
		return nil, fmt.Errorf("cannot save the configuration: %w", err)
	}
	set(cfg) // keep the caller's copy in step
	path, _ := configPath()
	c.ok("saved to %s (readable only by you)", path)
	return p, nil
}

type tunnelInfo struct {
	Name       string
	Workspaces []string
	known      bool // the response was understood well enough to trust Workspaces
}

// verifyTunnel asks OpenAI for the tunnel's metadata through tunnel-client,
// with the key supplied only through the environment.
func verifyTunnel(ctx context.Context, helper, id, key string) (*tunnelInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	args := []string{"admin", "--json"}
	override, err := controlPlaneOverride()
	if err != nil {
		return nil, err
	}
	args = append(append(args, override...), "tunnels", "get", id)
	cmd := exec.CommandContext(ctx, helper, args...)
	cmd.Env = helperEnv("CONTROL_PLANE_API_KEY=" + key)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, errors.New("no answer from OpenAI within 25 seconds (network or proxy problem?)")
	}
	if ctx.Err() != nil {
		return nil, errInterrupted
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		msg = strings.ReplaceAll(msg, key, "[api key]")
		return nil, errors.New(clip(repo.SanitizeLine(firstLine(msg)), 300))
	}
	info := &tunnelInfo{}
	var doc map[string]any
	if json.Unmarshal(out, &doc) == nil {
		if name, ok := doc["name"].(string); ok {
			info.Name = clip(repo.SanitizeLine(name), 80)
		}
		for _, k := range []string{"workspace_ids", "workspaceIds", "WorkspaceIDs"} {
			if list, ok := doc[k].([]any); ok {
				info.known = true
				for _, w := range list {
					info.Workspaces = append(info.Workspaces, fmt.Sprint(w))
				}
			}
		}
	}
	return info, nil
}

func explainVerify(c *console, msg string) {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "401") || strings.Contains(lower, "unauthorized") || strings.Contains(lower, "invalid api key"):
		c.say("  → The API key was not accepted. Check that you copied all of it: %s", apiKeysURL)
	case strings.Contains(lower, "403") || strings.Contains(lower, "forbidden") || strings.Contains(lower, "permission"):
		c.say("  → The key may not read this tunnel. It needs Tunnels: Read + Use, in the tunnel's organization.")
	case strings.Contains(lower, "404") || strings.Contains(lower, "not found"):
		c.say("  → No such tunnel for this key. Check the tunnel ID: %s", tunnelsURL)
	default:
		c.say("  → This may be a network problem; `repochat doctor` tests each part separately.")
	}
}

// cmdSetup runs the credential wizard on demand, and manages profiles.
func cmdSetup(c *console, args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	profile := fs.String("profile", "", "profile `name` to create or update (default: the default profile)")
	tunnelID := fs.String("tunnel-id", "", "tunnel ID (otherwise asked for, or read from CONTROL_PLANE_TUNNEL_ID)")
	keyFile := fs.String("api-key-file", "", "read the API key from this `file` (otherwise asked for, or read from CONTROL_PLANE_API_KEY)")
	noVerify := fs.Bool("no-verify", false, "save without checking the tunnel with OpenAI")
	bind := fs.Bool("bind", false, "bind this profile to the current repository (it will then refuse to serve any other)")
	unbind := fs.Bool("unbind", false, "remove the profile's repository binding")
	list := fs.Bool("list", false, "list saved profiles")
	remove := fs.Bool("remove", false, "delete the profile")
	fs.BoolVar(&c.yes, "yes", false, "answer yes to confirmations")
	if code, ok := parseOptions(c, fs, args); !ok {
		return code
	}
	cfg, err := loadConfig()
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	name := *profile
	if name == "" {
		name = cfg.defaultName()
	}
	if !profileRe.MatchString(name) {
		c.fail("invalid profile name %q (letters, digits, '.', '_' and '-')", name)
		return 2
	}

	switch {
	case *list:
		if len(cfg.Profiles) == 0 {
			c.say("No profiles yet. Run `repochat` inside a repository to set one up.")
			return 0
		}
		for _, n := range cfg.profileNames() {
			p := cfg.Profiles[n]
			line := fmt.Sprintf("%s  %s", n, maskID(p.TunnelID))
			if p.TunnelName != "" {
				line += "  \"" + p.TunnelName + "\""
			}
			if n == cfg.defaultName() {
				line += "  (default)"
			}
			if p.BoundRepo != "" {
				line += "  bound to " + p.BoundRepo
			}
			if !p.Onboarded {
				line += "  — ChatGPT side not confirmed yet"
			}
			fmt.Fprintln(c.out, line)
		}
		return 0
	case *remove:
		if cfg.Profiles[name] == nil {
			c.fail("no profile named %q", name)
			return 1
		}
		if _, err := updateConfig(func(c *Config) {
			delete(c.Profiles, name)
			if c.DefaultProfile == name {
				c.DefaultProfile = ""
			}
		}); err != nil {
			c.fail("%v", err)
			return 1
		}
		c.ok("removed profile %q (the tunnel and key still exist at OpenAI; revoke the key there if it is no longer needed)", name)
		return 0
	case *unbind:
		p := cfg.Profiles[name]
		if p == nil {
			c.fail("no profile named %q", name)
			return 1
		}
		if p.TunnelID != "" {
			if err := checkTunnelUnassigned(p.TunnelID, ""); err != nil {
				c.fail("%v", err)
				return 1
			}
		}
		if _, err := updateConfig(func(c *Config) {
			if p := c.Profiles[name]; p != nil {
				p.BoundRepo = ""
			}
		}); err != nil {
			c.fail("%v", err)
			return 1
		}
		c.ok("profile %q is no longer bound to a repository", name)
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	o := wizardOptions{NoVerify: *noVerify, TunnelID: strings.TrimSpace(*tunnelID)}
	if o.TunnelID == "" {
		o.TunnelID = strings.TrimSpace(os.Getenv("CONTROL_PLANE_TUNNEL_ID"))
	}
	if *keyFile != "" {
		raw, err := os.ReadFile(*keyFile)
		if err != nil {
			c.fail("cannot read the key file: %v", err)
			return 1
		}
		o.APIKey = strings.TrimSpace(string(raw))
	} else {
		o.APIKey = strings.TrimSpace(os.Getenv("CONTROL_PLANE_API_KEY"))
	}
	if *bind {
		root, err := repo.FindRoot(".")
		if err != nil {
			c.fail("--bind must be run inside the repository to bind to")
			return 1
		}
		o.Bind = root
	}
	if *bind && o.TunnelID == "" && o.APIKey == "" {
		if p := cfg.Profiles[name]; p != nil { // bind an existing profile without re-entering credentials
			if err := checkTunnelUnassigned(p.TunnelID, o.Bind); err != nil {
				c.fail("%v", err)
				return 1
			}
			bound := o.Bind
			if _, err := updateConfig(func(c *Config) {
				if p := c.Profiles[name]; p != nil {
					p.BoundRepo = bound
				}
			}); err != nil {
				c.fail("%v", err)
				return 1
			}
			c.ok("profile %q is now bound to %s", name, o.Bind)
			return 0
		}
	}

	helper := ""
	if !o.NoVerify {
		var err error
		if helper, _, err = ensureHelper(ctx, c); err != nil {
			c.fail("%v", err)
			c.say("  (use --no-verify to save the credentials without the helper)")
			return 1
		}
	}
	if _, err := credentialWizard(ctx, c, cfg, name, helper, o); err != nil {
		if errors.Is(err, errInterrupted) {
			return 130
		}
		c.fail("%v", err)
		return 1
	}
	c.say("")
	c.say("Next: cd into a repository and run %s.", c.bold("repochat"))
	return 0
}
