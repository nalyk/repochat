package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const authCacheTTL = 3 * time.Second

var (
	revRe = regexp.MustCompile(`^[A-Za-z0-9_@][A-Za-z0-9._/@^~{}+\-]*$`)
	// revOpsRe is everything that may follow the name in a revision: parent
	// and ancestor steps (^, ^2, ~3) and the two peels that can only yield
	// the commit itself. Neither ^ nor ~ can occur in a ref name, so the
	// first one ends the name. What this leaves out is deliberate: ^{/text}
	// makes git search commit messages, which would let a caller test guesses
	// about masked or unserved messages (a match resolves, a miss does not);
	// ^{tree}, ^{object}, ^@, ^! and ^-n name other objects or sets.
	revOpsRe = regexp.MustCompile(`^(?:\^\{commit\}|\^\{\}|[\^~][0-9]{0,9})*$`)
	dateRe   = regexp.MustCompile(`^[0-9A-Za-z .,:+/\-]{1,40}$`)
	oidRe    = regexp.MustCompile(`^[0-9a-f]{40}(?:[0-9a-f]{24})?$`)

	// ErrNoHistory is returned by every history tool when the repository was
	// opened with NoHistory.
	ErrNoHistory = errors.New("access to past revisions is disabled for this session (repochat was started with --no-history)")
)

// head returns the commit HEAD points to; ok is false when no commit exists
// yet (unborn branch).
func (r *Repo) head(ctx context.Context) (oid string, ok bool, err error) {
	out, code, err := r.gitText(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", "HEAD^{commit}")
	if err != nil {
		return "", false, err
	}
	if code != 0 || !oidRe.MatchString(out) {
		return "", false, nil
	}
	return out, true, nil
}

// ResolveCommit turns a model-supplied revision into a commit OID that this
// session is allowed to read.
//
// Authorised history is exactly the commits reachable from HEAD or from a
// ref under refs/heads/, refs/remotes/ or refs/tags/. Reflog syntax, the
// stash and unreachable objects are never served.
func (r *Repo) ResolveCommit(ctx context.Context, rev string) (string, error) {
	if r.Opts.NoHistory {
		return "", ErrNoHistory
	}
	rev = strings.TrimSpace(rev)
	switch {
	case rev == "":
		return "", errors.New("revision is required (a branch, tag, commit hash, or an expression such as HEAD~2)")
	case len(rev) > 200 || !revRe.MatchString(rev):
		return "", errors.New("invalid revision: use a branch, tag, commit hash, or an expression such as HEAD~2")
	case strings.Contains(rev, ".."):
		return "", errors.New("a single revision is expected here, not a range")
	case strings.Contains(rev, "@{"):
		return "", errors.New("reflog and upstream expressions (@{…}) are not available; name the branch or commit directly")
	}
	if i := strings.IndexAny(rev, "^~"); i >= 0 && !revOpsRe.MatchString(rev[i:]) {
		return "", errors.New("invalid revision: after a branch, tag or commit hash only parent and ancestor steps are available (^, ^2, ~3). Expressions that search commit messages or select other objects (^{/text}, ^{tree}, ^@) are not; use git_log with grep to find a commit by its message")
	}
	oid, code, err := r.gitText(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", err
	}
	// A name that resolves to nothing and one that resolves to a commit this
	// session does not serve (stash, reflog-only, dangling) get the same
	// answer after the same git commands. Otherwise the difference in text,
	// or in the time a refusal takes, would tell a caller which names exist
	// outside the served history.
	exists := code == 0 && oidRe.MatchString(oid)
	reachable, err := r.reachable(ctx, oid, exists)
	if err != nil {
		return "", err
	}
	if !exists || !reachable {
		return "", fmt.Errorf("revision %q is unknown or not reachable from HEAD or from any branch, remote branch or tag; unreachable history (stash, reflog, dangling commits) is not served. Use git_refs to list branches and tags, git_log to list commits", rev)
	}
	return oid, nil
}

// reachable reports whether the commit is part of the served history. With
// exists=false there is no commit to ask about: the same commands are run
// about an object id that cannot exist and the answer is always false (see
// ResolveCommit for why). The walks git does for a real commit still take as
// long as the history is deep; what is equal is the number of git processes,
// which dominates in practice.
func (r *Repo) reachable(ctx context.Context, oid string, exists bool) (bool, error) {
	if !exists {
		oid = strings.Repeat("0", 40)
	}
	// Reachability can change (a branch is deleted or moved), so a positive
	// answer is only reused for a few seconds.
	r.mu.Lock()
	checked, known := r.authorized[oid]
	r.mu.Unlock()
	if known && time.Since(checked) < authCacheTTL {
		return true, nil
	}
	unverified := errors.New("could not verify that the revision is reachable; refusing to serve it")
	reachable := false
	if _, ok, err := r.head(ctx); err != nil {
		return false, err
	} else if ok {
		_, _, code, err := r.gitRaw(ctx, 1024, "merge-base", "--is-ancestor", oid, "HEAD")
		if err != nil {
			return false, err
		}
		switch {
		case !exists:
		case code == 0:
			reachable = true
		case code != 1:
			return false, unverified
		}
	}
	if !reachable {
		out, code, err := r.gitText(ctx, "for-each-ref", "--count=1", "--contains="+oid, "--format=%(refname)",
			"refs/heads/", "refs/remotes/", "refs/tags/")
		if err != nil {
			return false, err
		}
		switch {
		case !exists:
		case code != 0:
			return false, unverified
		default:
			reachable = out != ""
		}
	}
	if !reachable {
		return false, nil
	}
	r.mu.Lock()
	if len(r.authorized) > 4096 {
		r.authorized = map[string]time.Time{}
	}
	r.authorized[oid] = time.Now()
	r.mu.Unlock()
	return true, nil
}

// emptyTreeOID is the OID of the empty tree in this repository's hash format.
func (r *Repo) emptyTreeOID(ctx context.Context) (string, error) {
	r.mu.Lock()
	cached := r.emptyTree
	r.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	out, code, err := r.gitText(ctx, "hash-object", "-t", "tree", "/dev/null")
	if err != nil {
		return "", err
	}
	if code != 0 || !oidRe.MatchString(out) {
		return "", errors.New("cannot determine the empty tree id")
	}
	r.mu.Lock()
	r.emptyTree = out
	r.mu.Unlock()
	return out, nil
}

func short(oid string) string {
	if len(oid) > 10 {
		return oid[:10]
	}
	return oid
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// StatusEntry is one changed path. X is the staged state, Y the working-tree
// state ('.' = unchanged), as in `git status --porcelain=v2`.
type StatusEntry struct {
	X, Y     byte
	Path     string
	Orig     string
	Unmerged bool
}

// Status is the parsed working-tree status.
type Status struct {
	HeadOID   string
	Branch    string
	Upstream  string
	Unborn    bool
	Detached  bool
	HasAB     bool
	Ahead     int
	Behind    int
	Entries   []StatusEntry
	Untracked []string
}

func (r *Repo) status(ctx context.Context) (*Status, error) {
	if err := r.beforeWorktreeRead(ctx); err != nil {
		return nil, err
	}
	untracked := "--untracked-files=all"
	if r.Opts.TrackedOnly {
		untracked = "--untracked-files=no"
	}
	out, err := r.git(ctx, 64<<20, "status", "--porcelain=v2", "--branch", "-z", untracked, "--ignore-submodules=dirty")
	if err != nil {
		return nil, err
	}
	st := &Status{}
	recs := bytes.Split(out, []byte{0})
	for i := 0; i < len(recs); i++ {
		rec := string(recs[i])
		if rec == "" {
			continue
		}
		switch rec[0] {
		case '#':
			f := strings.Fields(rec)
			if len(f) < 3 {
				continue
			}
			switch f[1] {
			case "branch.oid":
				if f[2] == "(initial)" {
					st.Unborn = true
				} else {
					st.HeadOID = f[2]
				}
			case "branch.head":
				if f[2] == "(detached)" {
					st.Detached = true
				} else {
					st.Branch = f[2]
				}
			case "branch.upstream":
				st.Upstream = f[2]
			case "branch.ab":
				if len(f) >= 4 {
					st.Ahead, _ = strconv.Atoi(strings.TrimPrefix(f[2], "+"))
					st.Behind, _ = strconv.Atoi(strings.TrimPrefix(f[3], "-"))
					st.HasAB = true
				}
			}
		case '1':
			f := strings.SplitN(rec, " ", 9)
			if len(f) == 9 && len(f[1]) == 2 {
				st.Entries = append(st.Entries, StatusEntry{X: f[1][0], Y: f[1][1], Path: f[8]})
			}
		case '2':
			f := strings.SplitN(rec, " ", 10)
			if len(f) == 10 && len(f[1]) == 2 && i+1 < len(recs) {
				i++
				st.Entries = append(st.Entries, StatusEntry{X: f[1][0], Y: f[1][1], Path: f[9], Orig: string(recs[i])})
			}
		case 'u':
			f := strings.SplitN(rec, " ", 11)
			if len(f) == 11 && len(f[1]) == 2 {
				st.Entries = append(st.Entries, StatusEntry{X: f[1][0], Y: f[1][1], Path: f[10], Unmerged: true})
			}
		case '?':
			if len(rec) > 2 {
				st.Untracked = append(st.Untracked, rec[2:])
			}
		}
	}
	if r.Opts.NoHistory {
		// Which commit HEAD is, and how many commits separate it from its
		// upstream, describe history. Dropped here so that no caller (status,
		// overview, result footers) can pass them on.
		st.HeadOID, st.HasAB, st.Ahead, st.Behind = "", false, 0, 0
	}
	return st, nil
}

// StatusResult is the rendered status.
type StatusResult struct {
	Text      string
	Staged    int
	Unstaged  int
	Untracked int
	Conflicts int
	Withheld  int
	Head      string
	Branch    string
	Truncated bool // a section listed fewer paths than it counted
}

const statusSectionLimit = 300

// Status renders branch information and the changed paths. Withheld paths
// are counted, not named (unless names were opted into).
func (r *Repo) Status(ctx context.Context) (*StatusResult, error) {
	st, err := r.status(ctx)
	if err != nil {
		return nil, err
	}
	pol, err := r.Policy(ctx)
	if err != nil {
		return nil, err
	}
	res := &StatusResult{Head: st.HeadOID, Branch: st.Branch}
	var staged, unstaged, untracked, conflicts []string
	visible := func(paths ...string) bool {
		for _, p := range paths {
			if p != "" && pol.Check(p) != "" {
				return false
			}
		}
		return true
	}
	label := func(e StatusEntry) string {
		// The old name of a staged rename is no longer in the index or the
		// working tree: without history it is not shown.
		if e.Orig != "" && !r.Opts.NoHistory {
			return SanitizeLine(e.Orig) + " -> " + SanitizeLine(e.Path)
		}
		return SanitizeLine(e.Path)
	}
	headOnly := 0
	for _, e := range st.Entries {
		if r.Opts.NoHistory && e.X == 'D' && !e.Unmerged {
			// A staged deletion names a file that only the last commit
			// still has.
			headOnly++
			continue
		}
		if !visible(e.Path, e.Orig) {
			res.Withheld++
			if !r.Opts.ShowWithheldNames {
				continue
			}
		}
		suffix := ""
		if !visible(e.Path, e.Orig) {
			suffix = "  [withheld]"
		}
		if e.Unmerged {
			conflicts = append(conflicts, fmt.Sprintf("  %c%c %s%s", e.X, e.Y, label(e), suffix))
			continue
		}
		if e.X != '.' {
			staged = append(staged, fmt.Sprintf("  %c  %s%s", e.X, label(e), suffix))
		}
		if e.Y != '.' {
			unstaged = append(unstaged, fmt.Sprintf("  %c  %s%s", e.Y, label(e), suffix))
		}
	}
	for _, p := range st.Untracked {
		if strings.HasSuffix(p, "/") {
			continue // nested repository
		}
		if !visible(p) {
			res.Withheld++
			if !r.Opts.ShowWithheldNames {
				continue
			}
			untracked = append(untracked, "  "+SanitizeLine(p)+"  [withheld]")
			continue
		}
		untracked = append(untracked, "  "+SanitizeLine(p))
	}
	res.Staged, res.Unstaged, res.Untracked, res.Conflicts = len(staged)+headOnly, len(unstaged), len(untracked), len(conflicts)

	var b strings.Builder
	b.WriteString(describeHead(st))
	b.WriteString("\n")
	section := func(title string, items []string, unnamed int) {
		fmt.Fprintf(&b, "%s (%d)\n", title, len(items)+unnamed)
		for i, it := range items {
			if i == statusSectionLimit {
				fmt.Fprintf(&b, "  … %d more\n", len(items)-i)
				res.Truncated = true
				break
			}
			b.WriteString(it)
			b.WriteString("\n")
		}
		if unnamed > 0 {
			fmt.Fprintf(&b, "  [%d staged deletion(s) not named: those files exist only in the last commit, and history is disabled (--no-history)]\n", unnamed)
		}
	}
	if len(conflicts) > 0 {
		section("MERGE CONFLICTS", conflicts, 0)
	}
	section("STAGED (index vs HEAD)", staged, headOnly)
	section("UNSTAGED (working tree vs index)", unstaged, 0)
	if r.Opts.TrackedOnly {
		b.WriteString("UNTRACKED: not exposed in this session (--tracked-only)\n")
	} else {
		section("UNTRACKED (not ignored)", untracked, 0)
	}
	if res.Withheld > 0 && !r.Opts.ShowWithheldNames {
		fmt.Fprintf(&b, "[%d changed path(s) withheld by policy are not listed]\n", res.Withheld)
	}
	switch {
	case len(staged)+len(unstaged)+len(untracked)+len(conflicts)+headOnly == 0 && res.Withheld == 0:
		b.WriteString("[working tree clean]\n")
	case r.Opts.NoHistory:
		b.WriteString("[codes: M modified, A added, D deleted, R renamed, C copied, T type changed, U unmerged. git_diff is unavailable: history is disabled (--no-history)]\n")
	default:
		b.WriteString("[codes: M modified, A added, D deleted, R renamed, C copied, T type changed, U unmerged. Use git_diff to see the changes]\n")
	}
	res.Text = b.String()
	return res, nil
}

func describeHead(st *Status) string {
	var b strings.Builder
	switch {
	case st.Unborn:
		fmt.Fprintf(&b, "BRANCH %s (no commits yet)", SanitizeLine(st.Branch))
	case st.HeadOID == "": // history is disabled: the commit is not named
		if st.Detached {
			b.WriteString("HEAD detached")
		} else {
			fmt.Fprintf(&b, "BRANCH %s", SanitizeLine(st.Branch))
		}
	case st.Detached:
		fmt.Fprintf(&b, "HEAD detached at %s", short(st.HeadOID))
	default:
		fmt.Fprintf(&b, "BRANCH %s at %s", SanitizeLine(st.Branch), short(st.HeadOID))
	}
	if st.Upstream != "" {
		fmt.Fprintf(&b, " · upstream %s", SanitizeLine(st.Upstream))
		if st.HasAB {
			fmt.Fprintf(&b, " (ahead %d, behind %d)", st.Ahead, st.Behind)
		}
	}
	return b.String()
}
