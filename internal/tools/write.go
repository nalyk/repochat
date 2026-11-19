package tools

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nalyk/repochat/internal/mcp"
	"github.com/nalyk/repochat/internal/repo"
)

// WriteInstructions is the write-mode part of WriteModeInstructions, the
// server-level guidance of write mode: it follows the guidance both modes
// share, in place of read mode's statement that nothing can be modified.
const WriteInstructions = "Write mode is on. Files are edited in a session workspace on a side branch, never in the user's own checkout. Lines you read carry anchors like `12:abc|`; cite them in apply_changes instead of quoting old text. If an anchor is stale the call fails and returns the current lines: use those anchors and retry. One apply_changes call may change many files and is applied completely or not at all. Lines that hold a masked value cannot be changed; tell the user to edit them by hand. Nothing reaches the user's branch until finish_session; call it only when the user asks to merge. A replace or delete of three or more lines also needs the file's current file_tag (from read_file or from the last apply_changes result)."

const (
	maxChangeItems        = 500     // operations in one change set, as the engine allows
	maxChangesBytes       = 2 << 20 // encoded change set: 1 MiB of new text plus JSON escaping
	maxMessageChars       = 2000
	defaultCheckpointList = 30
	maxCheckpointList     = 200
	checkpointFilesShown  = 10
)

var planIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// served is the Source of write mode: the session workspace while a session
// is open, otherwise the owner's checkout (without anchors).
type served struct{ s *repo.Sessions }

func (v served) Repo(ctx context.Context) (*repo.Repo, error) { return v.s.Serve(ctx) }

// changesSchema is the JSON Schema of a change set: the members of
// repo.Change, all strings.
func changesSchema() map[string]any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	op := str("What to do. replace: lines from..to become text. insert: text goes before or after the line at, or at the start or end of the file. " +
		"delete: lines from..to are removed. create: a new file at path whose whole content is text. remove: delete the file at path (needs file_tag). move: rename path to to_path. " +
		"A replace or delete of three or more lines also needs file_tag.")
	op["enum"] = []string{"replace", "insert", "delete", "create", "remove", "move"}
	where := str(`insert at an anchor only: "before" or "after" that line (default "after").`)
	where["enum"] = []string{"before", "after"}
	return map[string]any{
		"type":     "array",
		"minItems": 1,
		"maxItems": maxChangeItems,
		"items": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"op", "path"},
			"properties": map[string]any{
				"op":       op,
				"path":     str("Repository-relative path of the file, e.g. src/app/main.go."),
				"from":     str("replace, delete: anchor of the first line of the range, e.g. 12:abc, copied from read_file or search_code output (the part before the |)."),
				"to":       str("replace, delete: anchor of the last line of the range (12:abc form); omit for a single line. A range of three or more lines also needs file_tag."),
				"at":       str(`insert: an anchor (12:abc form) of the line to insert next to, or "start" or "end" of the file.`),
				"where":    where,
				"text":     str("replace, insert, create: the new lines, separated by \\n, exactly as they should be in the file: without anchors or line numbers, indentation included."),
				"to_path":  str("move: the new repository-relative path."),
				"file_tag": str("remove, and replace or delete of three or more lines (required); any replace or delete (optional, checked when given): the file's current file_tag, from the @meta line of read_file for this file or from the [file_tag …] line the last apply_changes result gave for it. It shows you saw the file's current content, including the lines between from and to."),
			},
		},
	}
}

// NewWrite returns the tools of write mode: the 13 read tools, served from
// the session workspace while a session is open, then the nine that edit.
func NewWrite(owner *repo.Repo, s *repo.Sessions) []mcp.Tool {
	c := &catalog{src: served{s}, owner: owner, s: s}
	S, I, B, O := mcp.String, mcp.Integer, mcp.Boolean, mcp.Object
	// Write tool results are about the session, not about files served: they
	// are assembled with the owner's repository (masking, budget).
	w := func(h func(*catalog, context.Context, mcp.Args) (mcp.Result, error)) mcp.Handler {
		return func(ctx context.Context, a mcp.Args) (mcp.Result, error) { return h(c.at(owner), ctx, a) }
	}
	changes := mcp.Param{Name: "changes", Kind: O, Raw: changesSchema(), MaxBytes: maxChangesBytes,
		Desc: "The change set: an array of operations over any number of files, applied as one step. Anchors (12:abc) come from read_file or search_code output in this session and refer to the files as they are before the change set; ranges in one file may not overlap."}

	return append(c.readTools(), []mcp.Tool{
		{
			Name: "start_session", Title: "Start an editing session", Write: true,
			Description: "Open a session for editing, or resume the open one. A session is a workspace on a new side branch, made from the commit the user's checkout is on; from then on every tool reads and edits that workspace, never the user's own checkout. Uncommitted changes in the user's checkout are not part of it.",
			Schema: mcp.Schema{
				{Name: "fresh", Kind: B, Desc: "Remove the open session's workspace (its branch is kept) and start a new session. Default false: resume the open session when there is one."},
			},
			Handler: w((*catalog).startSession),
		},
		{
			Name: "session_status", Title: "Session status",
			Description: "Where the open session stands: its branch, the commit it started from, checkpoints since, the files that differ from the user's branch, whether the user's branch has moved (then call update_session before finish_session), and which changed files will run on the user's machine later.",
			Handler:     w((*catalog).sessionStatus),
		},
		{
			Name: "propose_changes", Title: "Check a change set",
			Description: "Validate a change set against the session's current files without applying it. Returns the diff it would produce, warnings, and a plan id that apply_changes accepts. Nothing is written. Optional: apply_changes also takes the change set directly.",
			Schema:      mcp.Schema{withRequired(changes)},
			Handler:     w((*catalog).propose),
		},
		{
			Name: "apply_changes", Title: "Apply a change set", Write: true,
			Description: "Apply a change set to the session workspace, completely or not at all, and record it as one checkpoint. Give exactly one of `changes` and `plan` (an id from propose_changes). Returns the checkpoint, line counts, each changed file's new file_tag and the changed regions with fresh anchors. A stale anchor refuses the whole change set; the error shows the current lines with fresh anchors to retry with.",
			Schema: mcp.Schema{
				changes,
				{Name: "plan", Kind: S, MaxLen: 16, Desc: "A plan id from propose_changes (16 hex characters), instead of changes."},
				{Name: "message", Kind: S, MaxLen: maxMessageChars, Desc: "Checkpoint message: one line saying what the change does, optionally a blank line and more (default: a generic message)."},
			},
			Handler: w((*catalog).apply),
		},
		{
			Name: "list_checkpoints", Title: "List checkpoints",
			Description: "List the session's checkpoints, newest first, ending with the start (the commit the session was made from): id, kind, time, message, files and line counts. The ids are what undo_to_checkpoint takes.",
			Schema: mcp.Schema{
				{Name: "limit", Kind: I, Min: 1, Max: maxCheckpointList, Desc: "Checkpoints to list (default 30): the newest ones, and always the start."},
			},
			Handler: w((*catalog).checkpoints),
		},
		{
			Name: "undo_to_checkpoint", Title: "Undo to a checkpoint", Write: true,
			Description: "Restore the files of an earlier checkpoint, recorded as a new checkpoint: nothing is removed from the session's history, so an undo can be undone too. It cannot go back past the latest update_session. Read the files again afterwards for fresh anchors.",
			Schema: mcp.Schema{
				{Name: "checkpoint", Kind: S, Required: true, MaxLen: 64, Desc: "Checkpoint id from list_checkpoints (at least 7 characters)."},
			},
			Handler: w((*catalog).undo),
		},
		{
			Name: "update_session", Title: "Bring the session up to date", Write: true,
			Description: "Merge the commits the user's branch gained since the session started into the session, as a checkpoint. A conflict abandons the merge and changes nothing; resolving conflicts through the tools is not supported. Read files again afterwards for fresh anchors.",
			Handler:     w((*catalog).update),
		},
		{
			Name: "finish_session", Title: "Merge the session into the user's branch", Write: true, Destructive: true,
			Description: "Merge the session into the user's branch, in the user's own checkout, then close the session. Call it only when the user asks to merge. Fast-forward, or one squashed commit with squash=true. Refused, changing nothing, when the user's checkout is on another branch, when their branch has moved (call update_session first), or when the merge would overwrite their uncommitted changes. The result lists changed files that will run on the user's machine: tell the user.",
			Schema: mcp.Schema{
				{Name: "squash", Kind: B, Desc: "Land the whole session as one new commit on the user's branch instead of fast-forwarding to its checkpoints (default false)."},
				{Name: "message", Kind: S, MaxLen: maxMessageChars, Desc: "Message of the squashed commit (squash only; default: a generic message)."},
			},
			Handler: w((*catalog).finishSession),
		},
		{
			Name: "abandon_session", Title: "Abandon the session", Write: true, Destructive: true,
			Description: "Close the session without merging: its workspace is removed and nothing reaches the user's branch. The session branch is kept, so the user can still merge it with git, unless delete_branch is true. The read tools then serve the user's checkout again.",
			Schema: mcp.Schema{
				{Name: "delete_branch", Kind: B, Desc: "Also delete the session branch (default false: it is kept so the work can still be merged by hand)."},
			},
			Handler: w((*catalog).abandon),
		},
	}...)
}

func withRequired(p mcp.Param) mcp.Param {
	p.Required = true
	return p
}

// ---------------------------------------------------------------------------
// Text helpers
// ---------------------------------------------------------------------------

// clip cuts text to at most room bytes, at a line end when there is one,
// ending with note. It reports whether it cut.
func clip(text string, room int, note string) (string, bool) {
	if len(text) <= room {
		return text, false
	}
	keep := max(room-len(note)-1, 0)
	cut := strings.LastIndexByte(text[:keep], '\n') + 1
	if cut == 0 {
		for cut = keep; cut > 0 && !utf8.RuneStart(text[cut]); cut-- {
		}
	}
	out := text[:cut]
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out + note, true
}

// writeFail shapes the error of a write tool: masked, sanitised, and cut to
// the result budget unless whole. The engine's texts say whether anything was
// changed; they are passed on, not reworded.
func (c *catalog) writeFail(err error, whole bool) (mcp.Result, error) {
	msg, _ := c.r.Redactor().Text(err.Error(), false)
	msg = repo.Sanitize(msg)
	if !whole {
		note := "[error cut at the size budget]"
		var ce *repo.ChangeError
		if errors.As(err, &ce) && ce.Fresh != "" {
			note = "[error cut at the size budget: read the files again for the current lines and their anchors]"
		}
		msg, _ = clip(msg, c.r.Budget()-len(newSessionHint)-1, note)
	}
	return mcp.Result{}, errors.New(withSessionHint(msg))
}

// newSessionHint translates the engine's advice to restart with
// --new-session, a command-line flag, into what the model can do.
const newSessionHint = "From here: call start_session with fresh=true."

func withSessionHint(msg string) string {
	if strings.Contains(msg, "--new-session") {
		return msg + "\n" + newSessionHint
	}
	return msg
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 200 {
		cut := 200
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

// bullets writes one "  - item" line per item.
func bullets(b *strings.Builder, items []string) {
	for _, it := range items {
		fmt.Fprintf(b, "  - %s\n", repo.SanitizeLine(it))
	}
}

const runsLaterHeading = "WILL RUN ON THE USER'S MACHINE: these changed files get executed later (build files, package scripts, shell scripts, container files). Tell the user to review them before they build or run anything:\n"

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (c *catalog) startSession(ctx context.Context, a mcp.Args) (mcp.Result, error) {
	fresh := a.Bool("fresh")
	prev, perr := c.s.Current(ctx)
	info, err := c.s.Start(ctx, fresh)
	if err != nil {
		return c.writeFail(err, false)
	}
	resumed := perr == nil && !fresh && prev.ID == info.ID
	var b strings.Builder
	if resumed {
		fmt.Fprintf(&b, "Resumed the open session %s.\n", info.ID)
	} else {
		fmt.Fprintf(&b, "Started session %s.\n", info.ID)
	}
	fmt.Fprintf(&b, "Branch %s, made from %s at %s; the session is at %s.\n", info.Branch, info.StartBranch, short(info.Base), short(info.Head))
	b.WriteString("Every tool now reads and edits the session workspace, not the user's checkout. Read files (again) before editing: their lines carry anchors like `12:abc|`. Nothing reaches the user's branch until finish_session.\n")
	return c.finish("start_session", b.String(), 0, meta{
		"session": info.ID, "branch": info.Branch, "start_branch": info.StartBranch,
		"base": info.Base, "head": info.Head, "resumed": resumed, "complete": true,
	}), nil
}

func (c *catalog) sessionStatus(ctx context.Context, _ mcp.Args) (mcp.Result, error) {
	st, err := c.s.Status(ctx)
	if errors.Is(err, repo.ErrNoSession) {
		return c.finish("session_status", "No session is open. The read tools serve the user's checkout as it is, without anchors. Call start_session to open one before editing.\n", 0,
			meta{"open": false, "complete": true}), nil
	}
	if err != nil {
		return c.writeFail(err, false)
	}
	in := st.Info
	var b strings.Builder
	fmt.Fprintf(&b, "Session %s on branch %s, made from %s at %s; now at %s, %d checkpoint(s) since the start.\n",
		in.ID, in.Branch, in.StartBranch, short(in.Base), short(in.Head), st.Ahead)
	if len(st.RunsLater) > 0 {
		b.WriteString(runsLaterHeading)
		bullets(&b, st.RunsLater)
	}
	if st.OwnerMoved {
		fmt.Fprintf(&b, "%s has commits the session lacks: call update_session before finish_session.\n", in.StartBranch)
	} else {
		fmt.Fprintf(&b, "%s has not moved since: finish_session can fast-forward it.\n", in.StartBranch)
	}
	switch {
	case st.OwnerBranch == "":
		fmt.Fprintf(&b, "The user's checkout is on a detached HEAD: finish_session is refused until the user checks out %s.\n", in.StartBranch)
	case st.OwnerBranch != in.StartBranch:
		fmt.Fprintf(&b, "The user's checkout is on %s: finish_session is refused until the user checks out %s.\n", st.OwnerBranch, in.StartBranch)
	}
	if len(st.Files) == 0 {
		fmt.Fprintf(&b, "No file differs from %s.\n", in.StartBranch)
	} else {
		fmt.Fprintf(&b, "Files that differ from %s (%d):\n", in.StartBranch, len(st.Files))
		bullets(&b, st.Files)
	}
	m := meta{
		"open": true, "session": in.ID, "branch": in.Branch, "start_branch": in.StartBranch, "head": in.Head,
		"ahead": st.Ahead, "owner_moved": st.OwnerMoved, "owner_branch": st.OwnerBranch, "files": len(st.Files), "complete": true,
	}
	footerList(m, "runs_later", st.RunsLater)
	return c.finish("session_status", b.String(), 0, m), nil
}

func (c *catalog) propose(ctx context.Context, a mcp.Args) (mcp.Result, error) {
	changes, err := repo.ParseChanges(a.Raw("changes"))
	if err != nil {
		return c.writeFail(err, false)
	}
	plan, err := c.s.Propose(ctx, changes)
	if err != nil {
		return c.writeFail(err, false)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Plan %s: %d file(s), +%d -%d lines. Nothing was written. Apply it with apply_changes plan=%s while the files stay as they are (a plan expires after 30 minutes).\n",
		plan.ID, len(plan.Files), plan.Added, plan.Removed, plan.ID)
	for _, f := range plan.Files {
		what := "changed"
		switch {
		case f.Before == nil:
			what = "new"
		case f.After == nil:
			what = "removed"
		}
		fmt.Fprintf(&b, "  %s (%s)\n", repo.SanitizeLine(f.Path), what)
	}
	if len(plan.Warnings) > 0 {
		b.WriteString("Warnings:\n")
		bullets(&b, plan.Warnings)
	}
	b.WriteString("Diff:\n")
	diff, cut := clip(plan.Diff, c.bodyBudget()-b.Len(),
		"[diff cut at the size budget; the plan is complete and can be applied as it is. Read the files for the rest]\n")
	b.WriteString(diff)
	m := meta{"plan": plan.ID, "files": len(plan.Files), "added": plan.Added, "removed": plan.Removed, "complete": !cut}
	if cut {
		m["diff_truncated"] = true
	}
	return c.finish("propose_changes", b.String(), 0, m), nil
}

func (c *catalog) apply(ctx context.Context, a mcp.Args) (mcp.Result, error) {
	if a.Has("changes") == a.Has("plan") {
		return c.writeFail(errors.New("apply_changes takes exactly one of changes (the operations) and plan (an id from propose_changes); nothing was changed"), false)
	}
	var changes []repo.Change
	planID := a.String("plan")
	if a.Has("plan") {
		if !planIDRe.MatchString(planID) {
			return c.writeFail(errors.New("plan must be the 16 hexadecimal characters propose_changes returned; nothing was changed"), false)
		}
	} else {
		var err error
		if changes, err = repo.ParseChanges(a.Raw("changes")); err != nil {
			return c.writeFail(err, false)
		}
	}
	ap, err := c.s.Apply(ctx, planID, changes, a.String("message"))
	if err != nil {
		return c.writeFail(err, false)
	}
	intro := fmt.Sprintf("Applied as checkpoint %s: %d file(s), +%d -%d lines.\n", ap.Checkpoint.ID, len(ap.Checkpoint.Files), ap.Checkpoint.Added, ap.Checkpoint.Removed)
	return c.appliedResult("apply_changes", intro,
		"Changed regions with fresh anchors, and each file's new file_tag (elsewhere in these files line numbers may have shifted; read again before editing there):\n", ap), nil
}

// appliedResult reports a new (or current) checkpoint with the engine's
// Fresh text, cut to the budget.
func (c *catalog) appliedResult(tool, intro, freshHeading string, ap *repo.Applied) mcp.Result {
	var b strings.Builder
	b.WriteString(intro)
	if len(ap.Warnings) > 0 {
		b.WriteString("Warnings:\n")
		bullets(&b, ap.Warnings)
	}
	b.WriteString(freshHeading)
	fresh, cut := clip(ap.Fresh, c.bodyBudget()-b.Len(), "[cut at the size budget: read the files again for fresh anchors]\n")
	b.WriteString(fresh)
	cp := ap.Checkpoint
	m := meta{"checkpoint": cp.ID, "kind": cp.Kind, "added": cp.Added, "removed": cp.Removed, "complete": !cut}
	footerList(m, "files", cp.Files)
	if cut {
		m["fresh_truncated"] = true
	}
	return c.finish(tool, b.String(), 0, m)
}

func (c *catalog) checkpoints(ctx context.Context, a mcp.Args) (mcp.Result, error) {
	all, err := c.s.Checkpoints(ctx)
	if err != nil {
		return c.writeFail(err, false)
	}
	limit := a.Int("limit", defaultCheckpointList)
	shown, omitted := all, 0
	if len(all) > limit {
		// The newest, and always the start (the last entry).
		shown = append(slices.Clone(all[:limit-1]), all[len(all)-1])
		omitted = len(all) - limit
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d checkpoint(s), newest first:\n", len(all))
	for i, cp := range shown {
		if omitted > 0 && i == len(shown)-1 {
			fmt.Fprintf(&b, "[%d older checkpoint(s) are not shown; call again with a larger limit (at most %d)]\n", omitted, maxCheckpointList)
		}
		fmt.Fprintf(&b, "%s  %-7s %s", cp.ID, cp.Kind, cp.Time.UTC().Format("2006-01-02 15:04:05Z"))
		if cp.Kind != "start" {
			fmt.Fprintf(&b, "  +%d -%d", cp.Added, cp.Removed)
			files := cp.Files
			more := ""
			if len(files) > checkpointFilesShown {
				more = fmt.Sprintf(" and %d more", len(files)-checkpointFilesShown)
				files = files[:checkpointFilesShown]
			}
			if len(files) > 0 {
				fmt.Fprintf(&b, "  %s%s", repo.SanitizeLine(strings.Join(files, ", ")), more)
			}
		}
		fmt.Fprintf(&b, "\n    %s\n", repo.SanitizeLine(firstLine(cp.Message)))
	}
	return c.finish("list_checkpoints", b.String(), 0, meta{"shown": len(shown), "total": len(all), "omitted": omitted, "complete": omitted == 0}), nil
}

func (c *catalog) undo(ctx context.Context, a mcp.Args) (mcp.Result, error) {
	ap, err := c.s.UndoTo(ctx, a.String("checkpoint"))
	if err != nil {
		return c.writeFail(err, false)
	}
	cp := ap.Checkpoint
	intro := fmt.Sprintf("The session is at checkpoint %s (%s: %s): %d file(s), +%d -%d lines. Earlier checkpoints stay in the history.\n",
		cp.ID, cp.Kind, firstLine(cp.Message), len(cp.Files), cp.Added, cp.Removed)
	return c.appliedResult("undo_to_checkpoint", intro, "Files:\n", ap), nil
}

func (c *catalog) update(ctx context.Context, _ mcp.Args) (mcp.Result, error) {
	ap, err := c.s.Update(ctx)
	if err != nil {
		return c.writeFail(err, false)
	}
	cp := ap.Checkpoint
	intro := fmt.Sprintf("The session is at checkpoint %s (%s: %s): %d file(s), +%d -%d lines.\n",
		cp.ID, cp.Kind, firstLine(cp.Message), len(cp.Files), cp.Added, cp.Removed)
	return c.appliedResult("update_session", intro, "", ap), nil
}

func (c *catalog) finishSession(ctx context.Context, a mcp.Args) (mcp.Result, error) {
	branch := "the user's branch"
	if info, err := c.currentInfo(ctx); err == nil {
		branch = info.StartBranch
	}
	fin, err := c.s.Finish(ctx, a.Bool("squash"), a.String("message"))
	if err != nil {
		// Some of these say the checkout may be partly updated and how to
		// recover: they reach the user whole.
		return c.writeFail(err, true)
	}
	open, known := c.sessionOpen(ctx)
	warnings := 0
	for _, n := range fin.Notes {
		if strings.HasPrefix(n, "WARNING") {
			warnings++
		}
	}
	m := meta{"commit": fin.Commit, "squashed": fin.Squashed, "files": len(fin.Files),
		"warnings": warnings, "complete": true}
	if known {
		m["session_open"] = open
	} else {
		m["session_state"] = "unknown"
	}
	footerList(m, "runs_later", fin.RunsLater)
	footerList(m, "file_list", fin.Files) // "files" is their number here
	return c.finish("finish_session", finishText(branch, fin, open, known), 0, m), nil
}

// currentInfo is Sessions.Current, or the test seam.
func (c *catalog) currentInfo(ctx context.Context) (*repo.SessionInfo, error) {
	if c.current != nil {
		return c.current(ctx)
	}
	return c.s.Current(ctx)
}

// sessionOpen reports whether a session is open, and whether that is known.
// It is asked after a merge that may have run past the call's deadline, so
// it does not take the call's cancellation; only "no session" means closed.
func (c *catalog) sessionOpen(ctx context.Context) (open, known bool) {
	_, err := c.currentInfo(context.WithoutCancel(ctx))
	switch {
	case err == nil:
		return true, true
	case errors.Is(err, repo.ErrNoSession):
		return false, true
	}
	return false, false
}

// finishText is the result of a Finish: what needs telling first, first.
func finishText(branch string, fin *repo.Finished, open, known bool) string {
	var warnings, notes []string
	for _, n := range fin.Notes {
		if strings.HasPrefix(n, "WARNING") {
			warnings = append(warnings, n)
		} else {
			notes = append(notes, n)
		}
	}
	var b strings.Builder
	if len(warnings) > 0 {
		b.WriteString("WARNING: the merge did not end cleanly. Tell the user this first:\n")
		bullets(&b, warnings)
		// When where the branch is could not be read, Commit is the commit
		// that was being landed, not the branch's position.
		positionUnknown := false
		for _, w := range warnings {
			if strings.HasPrefix(w, "WARNING: git reported the merge of ") {
				positionUnknown = true
			}
		}
		if positionUnknown {
			fmt.Fprintf(&b, "Where %s is now is not known; the merged commit is %s.\n", branch, short(fin.Commit))
		} else {
			fmt.Fprintf(&b, "%s is at %s.\n", branch, short(fin.Commit))
		}
	} else {
		how := "fast-forward"
		if fin.Squashed {
			how = "one squashed commit"
		}
		fmt.Fprintf(&b, "Merged the session into %s (%s): it is now at %s. %d file(s) changed.\n", branch, how, short(fin.Commit), len(fin.Files))
	}
	if len(fin.RunsLater) > 0 {
		b.WriteString(runsLaterHeading)
		bullets(&b, fin.RunsLater)
	} else {
		b.WriteString("No changed file was recognised as one that runs on the user's machine later (build files, package scripts, shell scripts, container files).\n")
	}
	if len(notes) > 0 {
		b.WriteString("Notes:\n")
		bullets(&b, notes)
	}
	switch {
	case !known:
		b.WriteString("Whether the session is still open could not be determined; call session_status to find out.\n")
	case open:
		b.WriteString("The session is still open.\n")
	default:
		b.WriteString("The session is closed: the read tools serve the user's checkout again, without anchors; start_session opens a new session.\n")
	}
	if len(fin.Files) > 0 {
		b.WriteString("Files changed:\n")
		bullets(&b, fin.Files)
	}
	return b.String()
}

func (c *catalog) abandon(ctx context.Context, a mcp.Args) (mcp.Result, error) {
	branch := ""
	if info, err := c.currentInfo(ctx); err == nil {
		branch = info.Branch
	}
	del := a.Bool("delete_branch")
	if err := c.s.Abandon(ctx, del); err != nil {
		return c.writeFail(err, false)
	}
	var b strings.Builder
	b.WriteString("The session was abandoned: its workspace was removed and nothing reached the user's branch.\n")
	switch {
	case del && branch != "":
		fmt.Fprintf(&b, "The session branch %s was deleted.\n", branch)
	case del:
		b.WriteString("The session branch was deleted.\n")
	case branch != "":
		fmt.Fprintf(&b, "The session branch %s is kept; the user can still merge it by hand: git merge --ff-only %s\n", branch, branch)
	default:
		b.WriteString("The session branch is kept; the user can still merge it by hand with git.\n")
	}
	b.WriteString("The read tools serve the user's checkout again, without anchors; start_session opens a new session.\n")
	return c.finish("abandon_session", b.String(), 0, meta{"branch": branch, "branch_deleted": del, "complete": true}), nil
}
