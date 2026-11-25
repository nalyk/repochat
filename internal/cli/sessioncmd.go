package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/nalyk/repochat/internal/repo"
)

// cmdSession is `repochat session [status|diff|finish|abandon]`: the write-mode
// session of a repository, from the terminal. It works whether or not
// repochat is serving; the session engine serialises the two.
func cmdSession(c *console, args []string) int {
	sub := "status"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "status", "diff", "finish", "abandon":
	default:
		c.fail("unknown session command %q: use status, diff, finish or abandon", sub)
		return 2
	}
	fs := flag.NewFlagSet("session "+sub, flag.ContinueOnError)
	fs.SetOutput(c.errw)
	var s session
	sessionFlags(fs, &s)
	squash := fs.Bool("squash", false, "finish: land the whole session as one new commit instead of fast-forwarding")
	message := fs.String("message", "", "finish --squash: the message of that commit")
	deleteBranch := fs.Bool("delete-branch", false, "abandon: also delete the session branch (default: it is kept)")
	checkpoint := fs.String("checkpoint", "", "diff: show only this checkpoint's change")
	stat := fs.Bool("stat", false, "diff: list the changed files with their line counts instead of the patch")
	fs.BoolVar(&c.yes, "yes", false, "finish: merge without asking")
	if code, ok := parseOptions(c, fs, args); !ok {
		return code
	}
	// An option of another subcommand is a mistake, not something to ignore.
	only := map[string]string{"squash": "finish", "message": "finish", "yes": "finish", "delete-branch": "abandon", "checkpoint": "diff", "stat": "diff"}
	var wrong string
	fs.Visit(func(f *flag.Flag) {
		if want, ok := only[f.Name]; ok && want != sub && wrong == "" {
			wrong = fmt.Sprintf("--%s is an option of `repochat session %s`", f.Name, want)
		}
	})
	if wrong != "" {
		c.fail("%s", wrong)
		return 2
	}
	if *message != "" && !*squash {
		c.fail("--message is the message of a squashed commit: use it with --squash")
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
	ss, err := observeSessions(r, s.repoOptions(cfg), func(msg string) { c.warn("%s", msg) })
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	defer ss.Close()
	record := terminalRecorder(c, &s, r, "repochat session "+sub)

	switch sub {
	case "abandon":
		info, err := ss.Current(ctx)
		switch {
		case errors.Is(err, repo.ErrNoSession):
			c.say("No session is open for %s; there is nothing to abandon.", repo.SanitizeLine(r.Root))
			return 1
		case err != nil:
			// A broken session is still abandoned: that is how it is cleaned up.
			c.warn("%v", err)
		}
		if err := ss.Abandon(ctx, *deleteBranch); err != nil {
			c.fail("%v", err)
			return 1
		}
		branch := ""
		if info != nil {
			branch = repo.SanitizeLine(info.Branch)
		}
		if *deleteBranch {
			c.ok("Session abandoned: its workspace and the branch %s were removed. Nothing reached your branch.", branch)
			record(sessionNote{Note: fmt.Sprintf("session abandoned in the terminal: workspace removed, branch %s deleted", branch)})
		} else {
			c.ok("Session abandoned: its workspace was removed. Nothing reached your branch.")
			if branch != "" {
				c.say("  The branch %s is kept; to merge it by hand: git merge --ff-only %s", branch, branch)
			}
			record(sessionNote{Note: fmt.Sprintf("session abandoned in the terminal: workspace removed, branch %s kept", branch)})
		}
		return 0
	}

	st, err := ss.Status(ctx)
	if errors.Is(err, repo.ErrNoSession) {
		fmt.Fprintf(c.out, "No session is open for %s.\n", repo.SanitizeLine(r.Root))
		if sub == "finish" {
			return 1
		}
		return 0
	}
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	if sub == "diff" {
		return printSessionDiff(ctx, c, r, st, *checkpoint, *stat)
	}
	printSessionStatus(c, st)
	if sub == "status" {
		return 0
	}

	into := repo.SanitizeLine(st.Info.StartBranch)
	if !c.confirm(fmt.Sprintf("Merge into %s?", into), false) {
		switch {
		case c.interrupted:
			return 130
		case !c.tty && !c.yes:
			c.say("Not merged: there is no terminal to confirm on. Run it in a terminal, or pass --yes.")
		default:
			c.say("Not merged; the session stays open.")
		}
		return 1
	}
	fin, err := ss.Finish(ctx, *squash, *message)
	if err != nil {
		// The engine's text says what was changed and, when the checkout
		// may be partly updated, how to recover: it is shown whole.
		c.fail("%v", err)
		return 1
	}
	var warnings, notes []string
	for _, n := range fin.Notes {
		if strings.HasPrefix(n, "WARNING") {
			warnings = append(warnings, n)
		} else {
			notes = append(notes, n)
		}
	}
	for _, w := range warnings {
		c.warn("%s", w)
	}
	how := "fast-forward"
	if fin.Squashed {
		how = "one squashed commit"
	}
	if len(warnings) == 0 {
		c.ok("Merged into %s (%s): it is now at %s. %d file(s) changed.", into, how, shortID(fin.Commit), len(fin.Files))
	} else {
		c.say("%s: the merged commit is %s.", into, shortID(fin.Commit))
	}
	if len(fin.RunsLater) > 0 {
		c.warn("these changed files get executed on this machine later (build files, package scripts, shell scripts, container files); review them before you build or run anything:")
		listLines(c, fin.RunsLater)
	}
	for _, n := range notes {
		c.say("  note: %s", repo.SanitizeLine(n))
	}
	if len(fin.Files) > 0 {
		c.say("  Files changed:")
		listLines(c, fin.Files)
	}
	paths, more := notePaths(fin.Files, len(fin.Files), r.Redactor())
	note := withPaths(fmt.Sprintf("merged into %s in the terminal: now at %s, %d file(s) changed", into, shortID(fin.Commit), len(fin.Files)), paths, more)
	if len(warnings) > 0 {
		note += " · WARNING: the merge did not end cleanly"
	}
	record(sessionNote{Note: note, Checkpoint: fin.Commit, Files: len(fin.Files), Paths: paths, More: more})
	return 0
}

// terminalDiffBudget is the size of diff the terminal shows: the owner's
// own screen, not a model's context, so it is effectively unlimited.
const terminalDiffBudget = 64 << 20

// printSessionDiff shows what finish would land, or one checkpoint's change.
func printSessionDiff(ctx context.Context, c *console, r *repo.Repo, st *repo.SessionStatus, checkpoint string, stat bool) int {
	o := repo.DiffOptions{Mode: "range", Base: st.Info.StartBranch, Target: st.Info.Head, MergeBase: true, StatOnly: stat}
	if checkpoint != "" {
		o = repo.DiffOptions{Mode: "range", Base: checkpoint + "^", Target: checkpoint, StatOnly: stat}
	}
	res, err := r.Diff(ctx, o, terminalDiffBudget)
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	fmt.Fprintln(c.out, strings.TrimRight(res.Text, "\n"))
	if len(res.Omitted)+len(res.Cut) > 0 || res.Incomplete {
		fmt.Fprintf(c.out, "Not everything fit: `git diff %s...%s` shows the rest.\n", st.Info.StartBranch, st.Info.Branch)
	}
	return 0
}

func printSessionStatus(c *console, st *repo.SessionStatus) {
	in := st.Info
	start := repo.SanitizeLine(in.StartBranch)
	fmt.Fprintf(c.out, "Session %s on branch %s\n", in.ID, repo.SanitizeLine(in.Branch))
	fmt.Fprintf(c.out, "  from         %s @ %s, now at %s\n", start, shortID(in.Base), shortID(in.Head))
	fmt.Fprintf(c.out, "  checkpoints  %d since the start\n", st.Ahead)
	if st.OwnerMoved {
		fmt.Fprintf(c.out, "  your branch  %s has commits the session lacks: ask ChatGPT to update the session before merging, or merge the session branch by hand with git\n", start)
	} else {
		fmt.Fprintf(c.out, "  your branch  %s has not moved since the session started\n", start)
	}
	switch {
	case st.OwnerBranch == "":
		fmt.Fprintf(c.out, "  checkout     detached HEAD: check out %s before merging\n", start)
	case st.OwnerBranch != in.StartBranch:
		fmt.Fprintf(c.out, "  checkout     on %s: check out %s before merging\n", repo.SanitizeLine(st.OwnerBranch), start)
	}
	fmt.Fprintf(c.out, "  workspace    %s\n", repo.SanitizeLine(in.Workspace))
	if len(st.Files) == 0 {
		fmt.Fprintf(c.out, "  No file differs from %s.\n", start)
	} else {
		fmt.Fprintf(c.out, "  Files that differ from %s (%d):\n", start, len(st.Files))
		for _, f := range st.Files {
			fmt.Fprintf(c.out, "    %s\n", repo.SanitizeLine(f))
		}
	}
	if len(st.RunsLater) > 0 {
		fmt.Fprintf(c.out, "  WILL RUN ON THIS MACHINE LATER (build files, package scripts, shell scripts, container files); review before building:\n")
		for _, f := range st.RunsLater {
			fmt.Fprintf(c.out, "    %s\n", repo.SanitizeLine(f))
		}
	}
}

func listLines(c *console, items []string) {
	for _, it := range items {
		c.say("    %s", repo.SanitizeLine(it))
	}
}

// terminalRecorder writes what a write done from the terminal did to the
// activity log (unless --no-audit-log), as the serving process does for
// the writes it makes.
func terminalRecorder(c *console, s *session, r *repo.Repo, tool string) func(sessionNote) {
	if s.NoAudit {
		return func(sessionNote) {}
	}
	if s.ID == "" {
		s.ID = newID()
	}
	return func(n sessionNote) {
		a := sessionActivity(n, orDefault(n.Tool, tool), s, r)
		log, err := openAudit()
		if err == nil {
			err = log.write(a)
		}
		if err != nil {
			c.warn("cannot write the activity log: %v", err)
		}
	}
}
