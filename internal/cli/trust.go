package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nalyk/repochat/internal/guidance"
	"github.com/nalyk/repochat/internal/repo"
)

// trustAfterPreview is a test seam, called once the preview is printed and
// before the owner is asked.
var trustAfterPreview = func() {}

func sha256Hex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// cmdTrust is `repochat trust [PATH]`: the owner reviews a root AGENTS.md or
// CLAUDE.md and stores exactly what was shown as standing guidance.
func cmdTrust(c *console, args []string) int {
	path := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		path, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("trust", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	var s session
	sessionFlags(fs, &s)
	profile := fs.String("profile", "", "profile (tunnel) to give the guidance to")
	status := fs.Bool("status", false, "show the stored guidance and whether the file changed since")
	revoke := fs.Bool("revoke", false, "remove the stored guidance")
	release := fs.Bool("release-tunnel", false, "forget which repository this tunnel is assigned to (after asking)")
	fs.BoolVar(&c.yes, "yes", false, "do not ask for confirmation")
	if err := fs.Parse(args); err != nil {
		return flagExit(err)
	}
	if fs.NArg() > 1 || (fs.NArg() == 1 && path != "") {
		c.fail("trust takes one path: repochat trust [PATH] [options]")
		return 2
	}
	if fs.NArg() == 1 {
		path = fs.Arg(0)
	}
	modes := 0
	for _, b := range []bool{*status, *revoke, *release} {
		if b {
			modes++
		}
	}
	if modes > 1 {
		c.fail("use only one of --status, --revoke and --release-tunnel")
		return 2
	}
	if modes > 0 && path != "" {
		c.fail("a path only goes with reviewing guidance, not with --status, --revoke or --release-tunnel")
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

	name, prof, err := cfg.selectProfile(*profile, r.Root)
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	creds, have := resolveCredentials(prof)
	if !have {
		c.fail("no tunnel ID and API key saved for profile %q: run `repochat setup --profile %s`", name, name)
		return 1
	}
	if prof == nil || prof.BoundRepo != r.Root {
		c.fail("profile %q is not bound to this repository: bind it: repochat setup --profile %s --bind", name, name)
		return 1
	}
	key := guidance.Key(creds.TunnelID)
	sd, err := stateDir()
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	if err := privateDir(sd); err != nil {
		c.fail("%v", err)
		return 1
	}
	store, err := guidance.Open(sd, r.Root)
	if err != nil {
		c.fail("%v", err)
		return 1
	}

	switch {
	case *status:
		return trustStatus(ctx, c, r, store, key)
	case *revoke:
		if err := store.Revoke(key); err != nil {
			c.fail("%v", err)
			return 1
		}
		c.ok("the standing guidance for %s is revoked", repo.SanitizeLine(filepath.Base(r.Root)))
		c.say("Restart repochat, press Refresh tools in ChatGPT, and start a new conversation.")
		return 0
	case *release:
		return trustRelease(c, store, key, name)
	}
	return trustReview(ctx, c, r, store, key, name, path)
}

func trustStatus(ctx context.Context, c *console, r *repo.Repo, store *guidance.Store, key string) int {
	rec, err := store.Load(key)
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	if rec == nil {
		c.say("No guidance is stored for this tunnel.")
		return 0
	}
	state := "unchanged"
	if _, lerr := os.Lstat(filepath.Join(r.Root, rec.Path)); errors.Is(lerr, os.ErrNotExist) {
		state = "removed"
	} else if src, rerr := r.ReadGuidanceSource(ctx, rec.Path); rerr != nil {
		state = "changed (it can no longer be read as guidance: " + repo.SanitizeLine(rerr.Error()) + ")"
	} else if src.RawSHA != rec.RawSHA {
		state = "changed"
	}
	c.say("Path:       %s", repo.SanitizeLine(rec.Path))
	c.say("Reviewed:   %s", rec.Reviewed.Local().Format(time.RFC1123))
	c.say("Generation: generation %d", rec.Generation)
	c.say("File:       %s", state)
	if state != "unchanged" {
		c.say("The stored text is still what ChatGPT gets. Review the file again with `repochat trust`.")
	}
	return 0
}

func trustRelease(c *console, store *guidance.Store, key, profile string) int {
	b, err := store.Assignment(key)
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	if b == nil {
		c.say("This tunnel is not assigned to any repository; there is nothing to release.")
		return 0
	}
	c.say("Tunnel of profile %s is assigned to %s.", repo.SanitizeLine(profile), repo.SanitizeLine(b.Root))
	if !c.confirm("Release it and remove its stored guidance?", false) {
		c.say("Nothing was released.")
		if !c.tty && !c.yes {
			c.say("There is no terminal to ask on: use --yes.")
		}
		return 1
	}
	if err := store.Release(key); err != nil {
		c.fail("%v", err)
		return 1
	}
	c.ok("the tunnel is released")
	c.say("In ChatGPT, delete the plugin and create it again; then trust a repository for this tunnel.")
	return 0
}

func trustReview(ctx context.Context, c *console, r *repo.Repo, store *guidance.Store, key, profile, path string) int {
	if !c.tty && !c.yes {
		c.fail("repochat trust needs a terminal to ask for your confirmation; use --yes to confirm without one")
		return 1
	}
	src, err := r.ReadGuidanceSource(ctx, path)
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	display, err := r.GuidanceDisplay(src.Raw)
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	bind, err := r.CaptureBinding(ctx)
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	if a, err := store.Assignment(key); err != nil {
		c.fail("%v", err)
		return 1
	} else if a != nil && !a.Equal(bind) {
		c.fail("this tunnel is assigned to another repository (%s). To give it to this one, run `repochat trust --release-tunnel`, then delete the plugin in ChatGPT and create it again", repo.SanitizeLine(a.Root))
		return 1
	}

	name := repo.SanitizeLine(filepath.Base(r.Root))
	c.say("Repository: %s (%s)", name, repo.SanitizeLine(r.Root))
	c.say("Profile:    %s", repo.SanitizeLine(profile))
	c.say("File:       %s", repo.SanitizeLine(src.Path))
	c.say("----- begin guidance -----")
	c.say("%s", display)
	c.say("----- end guidance -----")
	c.say("This text is sent to OpenAI with the plugin's metadata whenever ChatGPT loads the plugin.")
	trustAfterPreview()

	if !c.confirm(fmt.Sprintf("Give this to ChatGPT as your standing instructions for %s?", name), false) {
		c.say("Nothing was stored.")
		return 1
	}
	rec := &guidance.Record{
		Version: guidance.RecordVersion, Transform: guidance.TransformVersion,
		Key: key, Binding: *bind, Path: src.Path, RawSHA: src.RawSHA,
		Display: display, DisplaySHA: sha256Hex([]byte(display)),
		Reviewed: time.Now().UTC(),
	}
	if err := store.Save(rec); err != nil {
		c.fail("%v", err)
		return 1
	}
	c.ok("stored as generation %d", rec.Generation)
	c.say("Restart repochat, press Refresh tools in ChatGPT, and start a new conversation.")
	return 0
}
