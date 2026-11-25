package repo

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A session is a linked git worktree on a side branch, kept under repochat's
// state directory, in which a model's edits are made. Its files are those of
// the branch minus every tracked file the exposure policy withholds: those
// are left out of the checkout by git's sparse checkout (marked
// skip-worktree, so their absence is not a deletion) and their absence is
// verified on disk after every synchronisation.
//
// Layout, under SessionOptions.StateDir:
//
//	<owner ID>/lock          an exclusive flock around every operation
//	<owner ID>/session.json  the open session (0600)
//	<owner ID>/<id>/w        its workspace

// SessionOptions configure a Sessions.
type SessionOptions struct {
	StateDir string       // <repochat state>/sessions
	Repo     Options      // the exposure options of this run
	Notify   func(string) // one-line notices for the owner's terminal; may be nil
	// Observe makes a Sessions that never changes the workspace's files or
	// index (see errObserving).
	Observe bool
}

// SessionInfo describes the open session.
type SessionInfo struct {
	ID, Branch, StartBranch, Base, Head, Workspace string
	Created                                        time.Time
	// Replayed is set when the answer is the stored one of an earlier call
	// with the same operation id.
	Replayed bool
}

// ErrNoSession is returned when an operation needs a session and none is open.
var ErrNoSession = errors.New("no session is open: call start_session")

// sessionRecord is session.json.
type sessionRecord struct {
	ID          string    `json:"id"`
	Branch      string    `json:"branch"`
	StartBranch string    `json:"start_branch"`
	Base        string    `json:"base"`
	Workspace   string    `json:"workspace"`
	Created     time.Time `json:"created"`
	AnchorKey   string    `json:"anchor_key"`  // base64 of 32 random bytes
	LastUpdate  string    `json:"last_update"` // commit of the latest update from the owner's branch
	SyncedWith  string    `json:"synced_with,omitempty"`
}

// Sessions manages the session of one repository. Every exported method
// holds an exclusive lock shared with every other repochat process for this
// repository and starts from session.json as it is on disk.
type Sessions struct {
	owner *Repo
	opts  SessionOptions
	dir   string // <StateDir>/<owner ID>, symbolic links resolved

	mu   sync.Mutex // serialises this process's callers; the flock serialises processes
	lock *os.File   // nil once closed
	rec  *sessionRecord
	ws   *Repo // the open workspace, for rec

	plans   map[string]*storedPlan // Propose's plans, by ID (see apply.go)
	planSeq uint64
	// Test seams, nil in production: the clock for plan expiry, and a hook
	// called before the i-th file of an apply is written; an error it
	// returns fails the apply there.
	now        func() time.Time
	applyFault func(i int) error
	// Called right before the owner's `git merge --ff-only` in Finish, and
	// right after an update's merge is committed, before the resync.
	beforeOwnerMerge func()
	afterUpdateMerge func()
}

const (
	sessionFile    = "session.json"
	ownerIgnore    = ".repochatignore"
	sessionKeySize = 32
)

// errDamagedRecord marks a session.json that cannot be trusted.
var errDamagedRecord = errors.New("session.json is damaged; start a new session with --new-session")

var (
	sessionIDRe  = regexp.MustCompile(`^[0-9a-f]{8}$`)
	objectIDRe   = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	digestRe     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	sparseConfig = []string{"-c", "core.sparseCheckout=true"}
)

// NewSessions prepares session management for owner. It neither starts nor
// resumes a session.
func NewSessions(owner *Repo, o SessionOptions) (*Sessions, error) {
	if !filepath.IsAbs(o.StateDir) {
		return nil, errors.New("the session state directory must be an absolute path")
	}
	dir := filepath.Join(o.StateDir, owner.ID)
	// The workspace must lie outside the owner's checkout: checked before
	// anything is created, and again once the directory exists.
	inside := errors.New("the session state directory is inside the repository's checkout; workspaces must lie outside it")
	if within(owner.Root, resolveExisting(dir)) {
		return nil, inside
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("cannot create the session state directory: %w", err)
	}
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	if within(owner.Root, dir) {
		return nil, inside
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cannot open the session lock: %w", err)
	}
	return &Sessions{owner: owner, opts: o, dir: dir, lock: lock}, nil
}

// within reports whether p is dir or lies beneath it.
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveExisting resolves the symbolic links of the longest part of p that
// exists and appends the rest unchanged.
func resolveExisting(p string) string {
	rest := ""
	for {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// acquire takes both locks and returns the function that releases them.
func (s *Sessions) acquire() (func(), error) {
	s.mu.Lock()
	if s.lock == nil {
		s.mu.Unlock()
		return nil, errors.New("sessions are closed")
	}
	for {
		err := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_EX)
		if err == nil {
			break
		}
		if err != syscall.EINTR {
			s.mu.Unlock()
			return nil, fmt.Errorf("cannot lock the session: %w", err)
		}
	}
	return func() {
		syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		s.mu.Unlock()
	}, nil
}

// Close releases the workspace and the lock file.
func (s *Sessions) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeWorkspace()
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}

func (s *Sessions) notify(msg string) {
	if s.opts.Notify != nil {
		s.opts.Notify(msg)
	}
}

func (s *Sessions) closeWorkspace() {
	if s.ws != nil {
		s.ws.Close()
	}
	s.ws, s.rec = nil, nil
}

// Serve returns the repository the read tools must use right now:
// the workspace when a session is open, otherwise the owner's checkout.
func (s *Sessions) Serve(ctx context.Context) (*Repo, error) {
	if s.opts.Observe {
		return nil, errObserving
	}
	release, err := s.acquire()
	if err != nil {
		return nil, err
	}
	defer release()
	rec, err := s.ensure(ctx)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return s.owner, nil
	}
	return s.ws, nil
}

// Current describes the open session, or returns ErrNoSession.
func (s *Sessions) Current(ctx context.Context) (*SessionInfo, error) {
	release, err := s.acquire()
	if err != nil {
		return nil, err
	}
	defer release()
	rec, err := s.readRecord()
	if err != nil {
		return nil, err
	}
	if rec == nil {
		s.closeWorkspace()
		return nil, ErrNoSession
	}
	return s.info(ctx, rec)
}

// Start opens a session, or resumes the open one. With fresh, an open
// session's workspace is removed, its branch is left in place, and a new
// session is started.
func (s *Sessions) Start(ctx context.Context, fresh bool) (*SessionInfo, error) {
	return s.StartCall(ctx, Call{}, fresh)
}

// StartCall is Start for a call with an operation id: a retry of a call that
// has answered returns that answer (Replayed) and starts nothing.
func (s *Sessions) StartCall(ctx context.Context, call Call, fresh bool) (*SessionInfo, error) {
	if s.opts.Observe {
		return nil, errObserving
	}
	release, err := s.acquire()
	if err != nil {
		return nil, err
	}
	defer release()
	if r, err := s.findReceipt("start", call); err != nil {
		return nil, err
	} else if r != nil && r.Info != nil {
		info := *r.Info
		info.Replayed = true
		return &info, nil
	}
	rec, err := s.readRecord()
	damaged := fresh && errors.Is(err, errDamagedRecord)
	if err != nil && !damaged {
		return nil, err
	}
	if rec != nil && !fresh {
		if rec, err = s.ensure(ctx); err != nil {
			return nil, err
		}
		if rec != nil {
			info, err := s.info(ctx, rec)
			if err == nil {
				s.recordStart(call, info)
			}
			return info, err
		}
	}
	if err := s.checkOwnerIgnore(); err != nil {
		return nil, err
	}

	// A session needs a branch to merge back into.
	if err := s.owner.refreshFilterArgs(ctx); err != nil {
		return nil, err
	}
	ref, code, err := s.owner.gitText(ctx, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return nil, err
	}
	if code != 0 || !strings.HasPrefix(ref, "refs/heads/") {
		return nil, errors.New("the repository's checkout is not on a branch (detached HEAD); a session needs a branch to merge back into: check out a branch and start again")
	}
	startBranch := strings.TrimPrefix(ref, "refs/heads/")
	base, code, err := s.owner.gitText(ctx, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	if err != nil {
		return nil, err
	}
	if code != 0 || !objectIDRe.MatchString(base) {
		return nil, fmt.Errorf("the branch %s has no commits yet; a session starts from a commit", SanitizeLine(startBranch))
	}

	if damaged {
		// Nothing in a damaged record is trusted, so nothing it names is
		// removed: it is set aside for the owner to look at.
		s.closeWorkspace()
		if err := os.Rename(filepath.Join(s.dir, sessionFile), filepath.Join(s.dir, sessionFile+".damaged")); err != nil {
			return nil, fmt.Errorf("cannot set the damaged session.json aside: %v", pathErrCause(err))
		}
		s.notify("The session record was damaged; it was kept as session.json.damaged and a new session was started. An earlier workspace may remain under repochat's state directory.")
	}
	if rec != nil { // fresh: the old session's branch stays, its workspace goes
		s.closeWorkspace()
		werr := s.removeWorkspace(ctx, rec.Workspace)
		if err := os.Remove(filepath.Join(s.dir, sessionFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if werr != nil {
			s.notify(fmt.Sprintf("The previous session's branch %s is kept. Its workspace could not be removed (%v); what is left of it lies under repochat's state directory.", rec.Branch, werr))
		} else {
			s.notify(fmt.Sprintf("The previous session's branch %s is kept; its workspace was removed.", rec.Branch))
		}
	}

	status, err := s.owner.git(ctx, 64<<20, "status", "--porcelain", "-z", "--untracked-files=normal", "--ignore-submodules=dirty")
	if err != nil {
		return nil, err
	}
	if len(status) > 0 {
		s.notify("The checkout has uncommitted changes; they are not part of the session, which starts from the last commit.")
	}

	var id [4]byte
	key := make([]byte, sessionKeySize)
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	rec = &sessionRecord{
		ID:          hex.EncodeToString(id[:]),
		StartBranch: startBranch,
		Base:        base,
		Created:     time.Now().UTC().Truncate(time.Second),
		AnchorKey:   base64.StdEncoding.EncodeToString(key),
	}
	rec.Branch = "repochat/" + time.Now().Format("2006-01-02") + "-" + rec.ID
	rec.Workspace = filepath.Join(s.dir, rec.ID, "w")
	if err := os.Mkdir(filepath.Dir(rec.Workspace), 0o700); err != nil {
		return nil, err
	}

	created := false
	ok := false
	defer func() {
		if ok {
			return
		}
		s.closeWorkspace()
		if created {
			s.removeWorkspace(ctx, rec.Workspace)
			s.owner.gitRaw(ctx, 4096, "update-ref", "-d", "refs/heads/"+rec.Branch, rec.Base)
		} else {
			os.RemoveAll(filepath.Dir(rec.Workspace))
		}
	}()
	if _, err := s.owner.git(ctx, 64<<10, "worktree", "add", "--no-checkout", "-b", rec.Branch, rec.Workspace, rec.Base); err != nil {
		return nil, err
	}
	created = true
	ws, err := s.openWorkspace(ctx, rec)
	if err != nil {
		return nil, err
	}
	s.ws, s.rec = ws, rec
	// The digest of what the sync below works from, taken before it: a rule
	// that changes while it runs is then seen by the next call.
	if rec.SyncedWith, err = s.optionsDigest(ctx, rec); err != nil {
		return nil, err
	}
	if err := s.sync(ctx); err != nil {
		return nil, err
	}
	if err := s.writeRecord(rec); err != nil {
		return nil, err
	}
	ok = true
	info, err := s.info(ctx, rec)
	if err == nil {
		s.recordStart(call, info)
	}
	return info, err
}

// recordStart keeps the answer of a start call that has an operation id. The
// session is open by now, so a receipt that cannot be written is a notice.
func (s *Sessions) recordStart(call Call, info *SessionInfo) {
	stored := *info
	if err := s.putReceipt(call, "start", receiptDone, nil, &stored); err != nil {
		s.notify("The session was started, but " + err.Error() + "; a retry of this call is not recognised.")
	}
}

// ensure makes the open workspace match session.json: none when there is no
// session, and otherwise the recorded one, resumed if this process has not
// opened it yet, and synced with this process's exposure options and the
// rules files as they are now when the record says it was last synced with
// others (see optionsDigest). An observer only checks that
// the workspace and the branch are there. The caller holds the locks.
func (s *Sessions) ensure(ctx context.Context) (*sessionRecord, error) {
	rec, err := s.readRecord()
	if err != nil {
		return nil, err
	}
	if rec == nil {
		s.closeWorkspace()
		return nil, nil
	}
	if err := s.checkOwnerIgnore(); err != nil {
		return nil, err
	}
	if s.opts.Observe {
		if _, err := s.present(ctx, rec); err != nil {
			return nil, err
		}
		return rec, nil
	}
	mine, err := s.optionsDigest(ctx, rec)
	if err != nil {
		s.closeWorkspace()
		return nil, err
	}
	// The open handle outlives its directory. A workspace removed behind
	// this process's back goes through resume like after a restart, which
	// reports it missing.
	reuse := s.ws != nil && s.rec != nil && sameSession(s.rec, rec)
	if reuse {
		if fi, err := os.Lstat(rec.Workspace); err != nil || !fi.IsDir() {
			reuse = false
		}
	}
	switch {
	case !reuse:
		s.closeWorkspace()
		if err := s.resume(ctx, rec); err != nil {
			s.closeWorkspace()
			return nil, err
		}
	case rec.SyncedWith != mine:
		// Synced by a process with other options: nothing is served or
		// written from it until it matches this one's.
		if err := s.sync(ctx); err != nil {
			s.closeWorkspace()
			return nil, err
		}
	}
	if rec.SyncedWith != mine {
		s.plans = nil
		rec.SyncedWith = mine
		if err := s.writeRecord(rec); err != nil {
			s.closeWorkspace()
			return nil, err
		}
	}
	s.rec = rec
	return rec, nil
}

// freshPolicy makes the next look at the workspace build its file list and
// policy anew, rules files re-read, instead of using a snapshot of a few
// seconds ago: whatever validates a write judges it by the rules as they
// are now. The caller holds the locks.
func (s *Sessions) freshPolicy() {
	if ws := s.ws; ws != nil {
		ws.mu.Lock()
		ws.snap = nil
		ws.mu.Unlock()
	}
}

// missing is the error for a session whose workspace or branch is gone.
func missing(rec *sessionRecord, what string) error {
	return fmt.Errorf("the session's %s is missing; nothing was recreated. Start a new session with --new-session (the branch %s, if it still exists, is kept)", what, rec.Branch)
}

// present checks that the workspace directory and the branch still exist.
func (s *Sessions) present(ctx context.Context, rec *sessionRecord) (head string, err error) {
	if fi, err := os.Lstat(rec.Workspace); err != nil || !fi.IsDir() {
		return "", missing(rec, "workspace")
	}
	head, code, err := s.owner.gitText(ctx, "rev-parse", "--verify", "-q", "refs/heads/"+rec.Branch+"^{commit}")
	if err != nil {
		return "", err
	}
	if code != 0 || !objectIDRe.MatchString(head) {
		return "", missing(rec, "branch")
	}
	return head, nil
}

// resume opens a recorded session's workspace: it must still exist, on its
// branch; changes that belong to no checkpoint are undone; then it is synced.
func (s *Sessions) resume(ctx context.Context, rec *sessionRecord) error {
	if _, err := s.present(ctx, rec); err != nil {
		return err
	}
	ws, err := s.openWorkspace(ctx, rec)
	if err != nil {
		return err
	}
	s.ws, s.rec = ws, rec
	ref, code, err := ws.gitText(ctx, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return err
	}
	if code != 0 || ref != "refs/heads/"+rec.Branch {
		return fmt.Errorf("the session workspace is no longer on its branch %s; start a new session with --new-session", rec.Branch)
	}
	if err := s.restore(ctx); err != nil {
		return err
	}
	return s.sync(ctx)
}

// restore returns the workspace to its HEAD when it differs: tracked files
// are restored and untracked files that are not ignored are deleted. Such
// changes belong to no checkpoint (an apply that was interrupted).
func (s *Sessions) restore(ctx context.Context) error {
	ws := s.ws
	dirty := func() (bool, error) {
		if err := ws.beforeWorktreeRead(ctx); err != nil {
			return false, err
		}
		out, err := ws.git(ctx, 64<<20, "status", "--porcelain", "-z", "--untracked-files=all", "--ignore-submodules=dirty")
		return len(out) > 0, err
	}
	d, err := dirty()
	if err != nil || !d {
		return err
	}
	if err := s.checkIndexLock(ws); err != nil {
		return err
	}
	if _, err := ws.git(ctx, 1<<20, "read-tree", "--reset", "-u", "HEAD"); err != nil {
		return err
	}
	if _, err := ws.git(ctx, 1<<20, "clean", "-f", "-d", "-q"); err != nil {
		return err
	}
	if d, err := dirty(); err != nil || d {
		if err == nil {
			err = errors.New("the session workspace has changes that could not be undone; start a new session with --new-session")
		}
		return err
	}
	s.notify("The session workspace had changes that belong to no checkpoint (an interrupted apply); they were undone.")
	return nil
}

// openWorkspace opens a session's workspace with the exposure options of
// this run, the owner's .repochatignore as a second rules source, and the
// session's anchor key.
func (s *Sessions) openWorkspace(ctx context.Context, rec *sessionRecord) (*Repo, error) {
	key, err := base64.StdEncoding.DecodeString(rec.AnchorKey)
	if err != nil || len(key) != sessionKeySize {
		return nil, errors.New("session.json is damaged (anchor key); start a new session with --new-session")
	}
	opts := s.opts.Repo
	opts.ExtraIgnoreFile = filepath.Join(s.owner.Root, ownerIgnore)
	opts.AnchorKey = key
	ws, err := Open(ctx, rec.Workspace, opts)
	if err != nil {
		return nil, err
	}
	if ws.Root != rec.Workspace {
		ws.Close()
		return nil, errors.New("the session workspace is not a repository of its own; start a new session with --new-session")
	}
	// It must be a worktree of the owner's repository.
	common := func(r *Repo) (string, error) {
		dir, code, err := r.gitText(ctx, "rev-parse", "--git-common-dir")
		if err != nil || code != 0 {
			return "", fmt.Errorf("cannot locate the git directory: %v", err)
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(r.Root, dir)
		}
		return filepath.EvalSymlinks(dir)
	}
	mine, err1 := common(ws)
	theirs, err2 := common(s.owner)
	if err1 != nil || err2 != nil || mine != theirs {
		ws.Close()
		return nil, errors.New("the session workspace does not belong to this repository; start a new session with --new-session")
	}
	ws.Logf = s.owner.Logf
	// The program's own directories are named by absolute path and lie, if
	// anywhere, in the owner's checkout: the workspace withholds the same
	// repository-relative paths, not those relative to its own directory.
	ws.own = s.ownPrefixes(ws.own)
	ws.extraPre = sparseConfig
	ws.ignoreFromIndex = true
	// Results name the repository, not the workspace directory.
	ws.Name = s.owner.Name
	sum := sha256.Sum256([]byte(ws.Root))
	ws.ID = ws.Name + "-" + hex.EncodeToString(sum[:])[:8]
	return ws, nil
}

// sync brings the open workspace in line with the exposure policy: every
// tracked file of its HEAD that the policy withholds is left out of the
// checkout, every other one is checked out. It runs when a session starts,
// when it is resumed, and after every update from the owner's branch. If a
// withheld file is still on disk afterwards, the workspace is removed.
func (s *Sessions) sync(ctx context.Context) error {
	err := s.syncFiles(ctx)
	if !errors.Is(err, errWithheldOnDisk) {
		return err
	}
	path := s.ws.Root
	s.closeWorkspace()
	s.removeWorkspace(ctx, path)
	return errors.New("a file the policy withholds could not be kept out of the session workspace, so the workspace was removed")
}

// errWithheldOnDisk is a withheld file found in the workspace after a sync.
var errWithheldOnDisk = errors.New("a file the policy withholds is in the session workspace")

// syncFiles is sync without the removal: it returns errWithheldOnDisk when
// a withheld file is still on disk afterwards.
func (s *Sessions) syncFiles(ctx context.Context) error {
	ws := s.ws
	if ws == nil {
		return ErrNoSession
	}
	if err := ws.refreshFilterArgs(ctx); err != nil {
		return err
	}
	withheld, err := ws.withheldAt(ctx, "HEAD")
	if err != nil {
		return err
	}
	if err := ws.writeSparse(withheld); err != nil {
		return err
	}
	if _, err := ws.git(ctx, 1<<20, "read-tree", "-mu", "HEAD"); err != nil {
		return err
	}
	ws.mu.Lock()
	ws.snap = nil
	ws.mu.Unlock()

	// The guarantee is what is on disk, not what git was asked to do.
	for _, p := range withheld {
		if onDisk(ws.Root, p) {
			if s.owner.Logf != nil {
				s.owner.Logf("session sync: withheld %q present after the sparse checkout", p)
			}
			return errWithheldOnDisk
		}
	}
	return nil
}

// withheldAt lists the tracked files of rev that the exposure policy
// withholds, the policy being the one of this run around rev's own
// .repochatignore.
func (ws *Repo) withheldAt(ctx context.Context, rev string) ([]string, error) {
	tree, err := ws.git(ctx, 256<<20, "ls-tree", "-r", "-z", "--full-tree", rev)
	if err != nil {
		return nil, fmt.Errorf("cannot list the session's files: %w", err)
	}
	var paths []string
	ignoreText := ""
	for _, rec := range bytes.Split(tree, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, path, ok := strings.Cut(string(rec), "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 || path == "" {
			return nil, errors.New("unexpected output from git ls-tree; refusing to continue")
		}
		if len(paths) >= maxFilesetEntries {
			return nil, fmt.Errorf("the repository has more than %d files; repochat refuses to build a partial workspace", maxFilesetEntries)
		}
		paths = append(paths, path)
		if path == ignoreFileName {
			if ignoreText, err = ws.ignoreBlob(ctx, f[0], f[2]); err != nil {
				return nil, err
			}
		}
	}
	pol, err := ws.newPolicy(ignoreText)
	if err != nil {
		return nil, err
	}
	if err := pol.Prime(ctx, paths); err != nil {
		return nil, fmt.Errorf(".repochatignore could not be evaluated (%v); refusing to build the workspace without the policy it defines", err)
	}
	var withheld []string
	for _, p := range paths {
		if pol.Check(p) != "" {
			withheld = append(withheld, p)
		}
	}
	return withheld, nil
}

// writeSparse makes the workspace's sparse-checkout file leave exactly the
// given paths out of the checkout. git applies it at the next command that
// updates the files.
func (ws *Repo) writeSparse(withheld []string) error {
	var sparse strings.Builder
	sparse.WriteString("/*\n")
	for _, p := range withheld {
		esc, err := sparseEscape(p)
		if err != nil {
			return err
		}
		sparse.WriteString("!/" + esc + "\n")
	}
	if err := writeFileAtomic(filepath.Join(ws.GitDir, "info"), "sparse-checkout", []byte(sparse.String())); err != nil {
		return fmt.Errorf("cannot write the workspace's sparse-checkout file: %w", err)
	}
	return nil
}

// sparseEscape writes a path as a pattern that matches exactly that path,
// in the syntax of .gitignore (the leading "/" and "!" are added by the
// caller). A name that ends a line early cannot be written at all.
func sparseEscape(p string) (string, error) {
	if strings.Contains(p, "\n") || strings.HasSuffix(p, "\r") {
		return "", fmt.Errorf("the withheld file %q has a name that git's sparse checkout cannot express; refusing to build a workspace that would contain it", SanitizeLine(p))
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '\\', '*', '?', '[', '!', '#', ' ':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

// errNotOurWorkspace is the refusal to remove what is at a recorded
// workspace path.
var errNotOurWorkspace = errors.New("what is at the session's workspace path is not the session's own workspace (a symbolic link, something that is not a directory, or a directory git does not list as that worktree), so nothing was removed; the owner can look under repochat's state directory and remove it by hand")

// ownWorkspace checks that path may be removed as a session's workspace:
// it is <session directory>/<id>/w, neither <id> nor w is a symbolic link or
// anything but a directory, and git lists a worktree of the owner's
// repository at exactly that path. present is false, with no error, when
// the workspace directory, or the one it lies in, is not there at all.
//
// The record names the path, and a record is only a file: git resolves
// symbolic links, so `worktree remove` given a link to another worktree
// removes that one, with everything in it.
func (s *Sessions) ownWorkspace(ctx context.Context, path string) (present bool, err error) {
	parent := filepath.Dir(path)
	if filepath.Base(path) != "w" || filepath.Dir(parent) != s.dir || !sessionIDRe.MatchString(filepath.Base(parent)) {
		return false, errNotOurWorkspace
	}
	for _, p := range []string{parent, path} {
		fi, err := os.Lstat(p)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return false, nil
		case err != nil:
			return false, fmt.Errorf("cannot check the session's workspace path: %v", pathErrCause(err))
		case !fi.IsDir(): // Lstat: a symbolic link is not a directory
			return false, errNotOurWorkspace
		}
	}
	out, err := s.owner.git(ctx, 16<<20, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	for _, l := range strings.Split(string(out), "\n") {
		if l == "worktree "+path {
			return true, nil
		}
	}
	return false, errNotOurWorkspace
}

// removeWorkspace removes a workspace and its registration in the owner's
// repository; the branch stays. It removes nothing unless the path is the
// session's own workspace (see ownWorkspace). Failures are logged, and
// returned for the callers that report them: what remains is a directory
// under repochat's state and, at worst, a stale worktree entry that
// `git worktree prune` clears.
func (s *Sessions) removeWorkspace(ctx context.Context, path string) error {
	present, err := s.ownWorkspace(ctx, path)
	if err != nil {
		if s.owner.Logf != nil {
			s.owner.Logf("session: the workspace was not removed: %v", err)
		}
		return err
	}
	var first error
	removed := false
	if present {
		out, _, code, err := s.owner.gitRaw(ctx, 64<<10, "worktree", "remove", "--force", "--force", path)
		removed = err == nil && code == 0
		if !removed {
			if s.owner.Logf != nil {
				s.owner.Logf("session: git worktree remove failed: %v %s", err, firstErr(nil, out))
			}
			first = errors.New("git worktree remove failed")
		}
	}
	// The session's own directory, checked above to be one: what it holds is
	// removed, never followed.
	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		if s.owner.Logf != nil {
			s.owner.Logf("session: cannot remove the workspace: %v", err)
		}
		return pathErrCause(err)
	}
	if !removed {
		// git refuses a worktree whose directory is gone (older versions):
		// its registration is removed here, that one only, never by a prune.
		if s.removeWorktreeEntry(ctx, path) {
			first = nil
		}
	}
	return first
}

// removeWorktreeEntry removes the registration, in the owner's git
// directory, of the session worktree at path, once its directory is gone:
// the entry under <common dir>/worktrees whose gitdir file names path/.git.
// path must lie in the session state directory. It reports whether no such
// entry remains.
func (s *Sessions) removeWorktreeEntry(ctx context.Context, path string) bool {
	if !within(s.dir, path) || path == s.dir {
		return false
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	common, code, err := s.owner.gitText(ctx, "rev-parse", "--git-common-dir")
	if err != nil || code != 0 || common == "" {
		return false
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(s.owner.Root, common)
	}
	admin := filepath.Join(common, "worktrees")
	entries, err := os.ReadDir(admin)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	want := filepath.Join(path, ".git")
	clean := true
	for _, e := range entries {
		if !e.IsDir() { // never through a symbolic link
			continue
		}
		data, err := os.ReadFile(filepath.Join(admin, e.Name(), "gitdir"))
		if err != nil || filepath.Clean(strings.TrimRight(string(data), "\r\n")) != want {
			continue
		}
		if err := os.RemoveAll(filepath.Join(admin, e.Name())); err != nil {
			if s.owner.Logf != nil {
				s.owner.Logf("session: cannot remove the stale worktree entry: %v", err)
			}
			clean = false
		}
	}
	return clean
}

// checkOwnerIgnore refuses an owner's .repochatignore that is not a regular
// file: it is read following links, and a dangling link would read as no
// rules at all.
func (s *Sessions) checkOwnerIgnore() error {
	fi, err := os.Lstat(filepath.Join(s.owner.Root, ownerIgnore))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("cannot check the repository's .repochatignore (%v); refusing to run a session without the policy it defines", pathErrCause(err))
	case !fi.Mode().IsRegular():
		return errors.New("the repository's .repochatignore is not a regular file (a symbolic link, or a directory); refusing to run a session without the policy it defines")
	}
	return nil
}

// info describes rec, with the branch's current commit.
func (s *Sessions) info(ctx context.Context, rec *sessionRecord) (*SessionInfo, error) {
	head, err := s.present(ctx, rec)
	if err != nil {
		return nil, err
	}
	return &SessionInfo{
		ID: rec.ID, Branch: rec.Branch, StartBranch: rec.StartBranch, Base: rec.Base,
		Head: head, Workspace: rec.Workspace, Created: rec.Created,
	}, nil
}

// readRecord reads session.json; nil when there is none. A record that does
// not hold together is an error, never a guess.
func (s *Sessions) readRecord() (*sessionRecord, error) {
	f, err := os.OpenFile(filepath.Join(s.dir, sessionFile), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if errors.Is(err, syscall.ELOOP) {
		return nil, errDamagedRecord
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read session.json: %v", pathErrCause(err))
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, errDamagedRecord
	}
	data, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return nil, err
	}
	var rec sessionRecord
	damaged := errDamagedRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, damaged
	}
	key, kerr := base64.StdEncoding.DecodeString(rec.AnchorKey)
	branchRe := `^repochat/\d{4}-\d{2}-\d{2}-` + regexp.QuoteMeta(rec.ID) + `$`
	if !sessionIDRe.MatchString(rec.ID) || !regexp.MustCompile(branchRe).MatchString(rec.Branch) ||
		rec.Workspace != filepath.Join(s.dir, rec.ID, "w") || !objectIDRe.MatchString(rec.Base) ||
		kerr != nil || len(key) != sessionKeySize || rec.StartBranch == "" || !SupportedName(rec.StartBranch) ||
		strings.HasPrefix(rec.StartBranch, "-") || (rec.LastUpdate != "" && !objectIDRe.MatchString(rec.LastUpdate)) ||
		(rec.SyncedWith != "" && !digestRe.MatchString(rec.SyncedWith)) {
		return nil, damaged
	}
	return &rec, nil
}

// writeRecord replaces session.json.
func (s *Sessions) writeRecord(rec *sessionRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.dir, sessionFile, append(data, '\n'))
}

// writeFileAtomic writes dir/name (0600) through a temporary file in dir.
func writeFileAtomic(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+name+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(dir, name))
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// errObserving is what an observing Sessions answers to anything that would
// change the workspace.
var errObserving = errors.New("this command does not change the workspace: it can show the session, finish it or abandon it")

// ownPrefixes are the repository-relative prefixes of the program's own
// directories (Options.Own) as the owner's checkout has them: those of the
// owner's repository and those of this run's options against its root, then
// extra, each once.
func (s *Sessions) ownPrefixes(extra []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range [][]string{s.owner.own, ownPrefixes(s.owner.Root, s.opts.Repo.Own), extra} {
		for _, p := range l {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// optionsDigest identifies everything that decides which files sync leaves
// out of the workspace: the exposure options of this run, the content of the
// owner's .repochatignore as it is on disk now, and the .repochatignore the
// session branch holds. The record keeps the digest the workspace was last
// synced with, so a change to any of them, an edit of the owner's rules file
// while a server runs among them, brings the workspace in line at the next
// call. What cannot be read makes a digest of its own: the sync that follows
// then fails on it, closed.
func (s *Sessions) optionsDigest(ctx context.Context, rec *sessionRecord) (string, error) {
	o := s.opts.Repo
	ownerRules := "absent"
	switch text, found, err := readExtraIgnore(filepath.Join(s.owner.Root, ownerIgnore)); {
	case err != nil:
		ownerRules = "unreadable"
	case found:
		sum := sha256.Sum256([]byte(text))
		ownerRules = hex.EncodeToString(sum[:])
	}
	// The blob the session branch has at .repochatignore, "" for none.
	sessionRules, code, err := s.owner.gitText(ctx, "rev-parse", "--verify", "-q", "refs/heads/"+rec.Branch+":"+ignoreFileName)
	if err != nil {
		return "", err
	}
	if code != 0 {
		sessionRules = ""
	}
	raw, _ := json.Marshal(struct {
		TrackedOnly              bool
		Allow, Deny, Own         []string
		ExtraIgnore, Owner       string
		OwnPrefixes              []string
		OwnerRules, SessionRules string
	}{o.TrackedOnly, o.Allow, o.Deny, o.Own, filepath.Join(s.owner.Root, ownerIgnore), s.owner.Root, s.ownPrefixes(nil), ownerRules, sessionRules})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// sameSession reports whether two records describe the same session in the
// same state, whatever options its workspace was synced with.
func sameSession(a, b *sessionRecord) bool {
	x, y := *a, *b
	x.SyncedWith, y.SyncedWith = "", ""
	return x == y
}

// reader is the repository that git questions about the session go to: the
// open workspace, or, for an observer, the owner's repository, which shares
// its objects and branches.
func (s *Sessions) reader() *Repo {
	if s.opts.Observe {
		return s.owner
	}
	return s.ws
}

// policy is the exposure policy of the session's files: that of the open
// workspace, or, for an observer, the one sync would build from the session
// branch's .repochatignore and the owner's, with the owner repository's
// options.
func (s *Sessions) policy(ctx context.Context, rec *sessionRecord) (*Policy, error) {
	if !s.opts.Observe {
		return s.ws.Policy(ctx)
	}
	out, err := s.owner.git(ctx, 64<<10, "ls-tree", "-z", "--full-tree", "refs/heads/"+rec.Branch, "--", ignoreFileName)
	if err != nil {
		return nil, fmt.Errorf("cannot read the session's .repochatignore: %w", err)
	}
	text := ""
	if entry := strings.TrimSuffix(string(out), "\x00"); entry != "" {
		meta, path, ok := strings.Cut(entry, "\t")
		f := strings.Fields(meta)
		if !ok || path != ignoreFileName || len(f) != 3 || strings.Contains(entry, "\x00") {
			return nil, errors.New("unexpected output from git ls-tree; refusing to guess at the policy")
		}
		if f[1] != "blob" {
			return nil, fmt.Errorf(".repochatignore %v; refusing to run without the policy it defines", errNotRegular)
		}
		if text, err = s.owner.ignoreBlob(ctx, f[0], f[2]); err != nil {
			return nil, err
		}
	}
	pol, err := s.owner.newPolicy(text)
	if err != nil {
		return nil, err
	}
	pol.own = s.ownPrefixes(nil)
	extra, found, err := readExtraIgnore(filepath.Join(s.owner.Root, ownerIgnore))
	if err != nil {
		return nil, err
	}
	if found {
		if err := pol.AddIgnore(extra); err != nil {
			return nil, err
		}
	}
	return pol, nil
}
