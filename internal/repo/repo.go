// Package repo gives strictly read-only, policy-filtered access to one Git
// working tree and its reachable history.
package repo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Options configure what a Repo exposes. They are fixed for its lifetime.
type Options struct {
	TrackedOnly       bool     // do not expose untracked files
	NoRedact          bool     // do not mask secret-looking values
	NoHistory         bool     // refuse tools that read past revisions
	ShowWithheldNames bool     // list withheld paths by name instead of counts
	Allow             []string // patterns exempt from the built-in secret-file rules
	Deny              []string // extra patterns to withhold
	MaxOutputBytes    int      // per tool result (default DefaultMaxOutput)
}

const (
	DefaultMaxOutput  = 200 << 10
	MinOutput         = 16 << 10
	MaxOutput         = 2 << 20
	maxFilesetEntries = 500_000
	maxFilesetBytes   = 64 << 20 // total bytes of path names
	snapshotTTL       = 3 * time.Second
	gitTimeout        = 30 * time.Second
)

// MinGit is the oldest git this package is written against.
var MinGit = [3]int{2, 25, 0}

var (
	ErrNotRepo  = errors.New("not inside a Git working tree")
	ErrWithheld = errors.New("withheld by policy")
)

// Repo is one opened repository. It is safe for concurrent use.
type Repo struct {
	Root   string // absolute path of the working tree, symlinks resolved
	GitDir string // absolute path of the git directory
	Name   string // sanitized directory name
	ID     string // stable identifier: name plus a short hash of the root path
	Opts   Options
	// Logf, when set, receives diagnostics that must stay on this machine
	// (git's own error output). It is never part of a tool result.
	Logf func(format string, args ...any)

	root     *os.Root
	rootInfo os.FileInfo
	red      *Redactor
	allow    []*Glob
	deny     []*Glob
	version  [3]int

	mu         sync.Mutex
	snap       *Snapshot
	snapAt     time.Time
	filterArgs []string
	authorized map[string]time.Time
	emptyTree  string
}

// FindRoot returns the nearest ancestor of start (inclusive) that contains a
// `.git` entry. repochat locates the working tree itself instead of asking
// git, so a repository's own `core.worktree` setting cannot redirect it.
func FindRoot(start string) (string, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", start)
	}
	for dir := abs; ; {
		if looksLikeGitDir(filepath.Join(dir, ".git")) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrNotRepo
		}
		dir = parent
	}
}

// looksLikeGitDir reports whether path is a repository's .git: a directory
// holding HEAD, or a "gitdir:" file (linked worktrees, submodules). A stray
// entry that merely happens to be called .git is skipped.
func looksLikeGitDir(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if fi.IsDir() {
		_, err := os.Stat(filepath.Join(path, "HEAD"))
		return err == nil
	}
	if !fi.Mode().IsRegular() {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 8)
	n, _ := f.Read(head)
	return string(head[:n]) == "gitdir: "
}

// Open opens the repository whose working tree contains start.
func Open(ctx context.Context, start string, opts Options) (*Repo, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, errors.New("git is not installed (it must be on PATH)")
	}
	root, err := FindRoot(start)
	if err != nil {
		return nil, err
	}
	if opts.MaxOutputBytes <= 0 {
		opts.MaxOutputBytes = DefaultMaxOutput
	}
	opts.MaxOutputBytes = min(max(opts.MaxOutputBytes, MinOutput), MaxOutput)

	r := &Repo{Root: root, Opts: opts, red: NewRedactor(!opts.NoRedact), authorized: map[string]time.Time{}}
	if r.allow, err = CompileGlobs(opts.Allow); err != nil {
		return nil, fmt.Errorf("--allow %w", err)
	}
	if r.deny, err = CompileGlobs(opts.Deny); err != nil {
		return nil, fmt.Errorf("--deny %w", err)
	}

	if out, _, code, err := runGit(ctx, root, nil, 1024, "version"); err != nil || code != 0 {
		return nil, fmt.Errorf("cannot run git: %v", firstErr(err, out))
	} else {
		r.version = parseGitVersion(string(out))
	}
	if versionLess(r.version, MinGit) {
		return nil, fmt.Errorf("git %d.%d.%d is too old; repochat needs git %d.%d or newer",
			r.version[0], r.version[1], r.version[2], MinGit[0], MinGit[1])
	}

	// Hold the directory itself from here on: file access goes through this
	// handle, and every git call first checks that the path still leads to it.
	if r.root, err = os.OpenRoot(root); err != nil {
		return nil, err
	}
	if r.rootInfo, err = r.root.Stat("."); err != nil {
		r.root.Close()
		return nil, err
	}
	fail := func(err error) (*Repo, error) {
		r.root.Close()
		return nil, err
	}

	out, _, code, err := runGit(ctx, root, nil, 8192, "rev-parse", "--absolute-git-dir")
	if err != nil || code != 0 {
		return fail(fmt.Errorf("%s is not a usable Git repository: %s", root, firstErr(err, out)))
	}
	r.GitDir = strings.TrimRight(string(out), "\r\n")

	// With git-dir and work-tree pinned, git must agree that root is the top.
	top, code, err := r.gitText(ctx, "rev-parse", "--show-toplevel")
	if err != nil || code != 0 {
		return fail(fmt.Errorf("%s has no working tree (bare repository?): %s", root, firstErr(err, []byte(top))))
	}
	if resolved, err := filepath.EvalSymlinks(top); err != nil || resolved != root {
		return fail(fmt.Errorf("git reports working tree %q but repochat located %q; refusing to continue", top, root))
	}
	r.Name = safeName(filepath.Base(root))
	sum := sha256.Sum256([]byte(root))
	r.ID = r.Name + "-" + hex.EncodeToString(sum[:])[:8]
	return r, nil
}

// Close releases the directory handle.
func (r *Repo) Close() error {
	if r.root != nil {
		return r.root.Close()
	}
	return nil
}

// GitVersion returns the detected git version as "X.Y.Z".
func (r *Repo) GitVersion() string {
	return fmt.Sprintf("%d.%d.%d", r.version[0], r.version[1], r.version[2])
}

// Redactor exposes the repository's redactor (never nil).
func (r *Repo) Redactor() *Redactor { return r.red }

// Budget is the byte budget for one tool result.
func (r *Repo) Budget() int { return r.Opts.MaxOutputBytes }

var nameUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeName(s string) string {
	s = nameUnsafe.ReplaceAllString(s, "_")
	if len(s) > 60 {
		s = s[:60]
	}
	if s == "" || s == "." {
		s = "repository"
	}
	return s
}

func firstErr(err error, out []byte) string {
	msg := strings.TrimSpace(string(out))
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	if msg == "" && err != nil {
		msg = err.Error()
	}
	if msg == "" {
		msg = "unknown error"
	}
	return SanitizeLine(msg)
}

var gitVersionRe = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

func parseGitVersion(s string) [3]int {
	var v [3]int
	m := gitVersionRe.FindStringSubmatch(s)
	for i := 1; i < len(m) && i <= 3; i++ {
		v[i-1], _ = strconv.Atoi(m[i])
	}
	return v
}

func versionLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Hardened git execution
// ---------------------------------------------------------------------------

// Settings that could make a read command execute a program are overridden at
// command scope, which outranks every configuration file. Repository-local
// configuration is therefore untrusted for execution; system and user
// configuration stay in effect (ignore rules, line-ending settings,
// safe.directory). This is defense in depth, not a sandbox.
var gitBaseConfig = []string{
	"-c", "core.fsmonitor=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.pager=cat",
	"-c", "core.quotepath=false",
	"-c", "color.ui=false",
	"-c", "log.showSignature=false",
	"-c", "protocol.allow=never",
	"-c", "submodule.recurse=false",
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
	// Settings that make git read a file named by the repository's own
	// configuration (possibly outside the repository) into its output.
	"-c", "mailmap.file=/dev/null",
	"-c", "mailmap.blob=",
	"-c", "log.mailmap=false",
}

// gitEnv builds the environment for git from an allowlist. Notably absent:
// every GIT_* variable from the caller (GIT_DIR, GIT_EXTERNAL_DIFF, …) and
// all credentials (CONTROL_PLANE_API_KEY, OPENAI_API_KEY, …).
func gitEnv() []string {
	env := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_PAGER=cat",
		"PAGER=cat",
		"GIT_EDITOR=true",
		"GIT_ASKPASS=true",
		"SSH_ASKPASS=true",
		"GIT_LITERAL_PATHSPECS=1",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_ALLOW_PROTOCOL=", // set but empty: no transport is permitted
		"GIT_NO_REPLACE_OBJECTS=1",
		"LC_MESSAGES=C",
		"LANGUAGE=C",
	}
	for _, k := range []string{"PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "TZ", "XDG_CONFIG_HOME"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	ctype := os.Getenv("LC_ALL")
	if ctype == "" {
		ctype = os.Getenv("LC_CTYPE")
	}
	if ctype == "" {
		ctype = os.Getenv("LANG")
	}
	if ctype != "" {
		env = append(env, "LC_CTYPE="+ctype)
	}
	return env
}

// runGit runs git in dir and returns at most limit bytes of stdout. When the
// output is larger the process group is killed and truncated=true. A non-zero
// exit is reported through code with stderr as out; err is reserved for
// failures to run git at all and for timeouts/cancellation.
func runGit(ctx context.Context, dir string, pre []string, limit int, args ...string) (out []byte, truncated bool, code int, err error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	full := append([]string{"--no-pager"}, pre...)
	full = append(full, gitBaseConfig...)
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	var stderr cappedBuffer
	stderr.limit = 8 << 10
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, -1, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, -1, err
	}
	buf := make([]byte, 0, min(limit+1, 64<<10))
	tmp := make([]byte, 32<<10)
	for len(buf) <= limit {
		n, rerr := stdout.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if rerr != nil {
			break
		}
	}
	if len(buf) > limit {
		truncated = true
		buf = buf[:limit]
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	werr := cmd.Wait()
	if ctx.Err() != nil && !truncated {
		return nil, false, -1, fmt.Errorf("git %s: %w", args[0], ctx.Err())
	}
	if truncated {
		return buf, true, 0, nil
	}
	if werr != nil {
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			return stderr.Bytes(), false, ee.ExitCode(), nil
		}
		return nil, false, -1, werr
	}
	return buf, false, 0, nil
}

type cappedBuffer struct {
	bytes.Buffer
	limit int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.limit - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// gitPre pins the git directory and the working tree for every command.
func (r *Repo) gitPre() []string {
	pre := []string{"--git-dir=" + r.GitDir, "--work-tree=" + r.Root}
	r.mu.Lock()
	pre = append(pre, r.filterArgs...)
	r.mu.Unlock()
	return pre
}

// gitRaw runs a git command in this repository (see runGit for the contract).
// git addresses the working tree by path, so the path must still lead to the
// directory that was opened; otherwise nothing is run.
func (r *Repo) gitRaw(ctx context.Context, limit int, args ...string) ([]byte, bool, int, error) {
	if err := r.checkRoot(); err != nil {
		return nil, false, -1, err
	}
	return runGit(ctx, r.Root, r.gitPre(), limit, args...)
}

// git runs a command that must succeed and whose output must be complete.
func (r *Repo) git(ctx context.Context, limit int, args ...string) ([]byte, error) {
	out, truncated, code, err := r.gitRaw(ctx, limit, args...)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, r.gitFailure(args[0], out)
	}
	if truncated {
		return nil, fmt.Errorf("git %s produced more than %d bytes; refusing to work from incomplete data", args[0], limit)
	}
	return out, nil
}

// gitFailure reports a failed git command. git's error text can quote files
// that the repository's configuration points at (for example the first line
// of a configured ignore-revs file), so it goes to the local log only and the
// caller gets a message with nothing repository-controlled in it.
func (r *Repo) gitFailure(subcommand string, stderr []byte) error {
	if r.Logf != nil {
		r.Logf("git %s failed: %s", subcommand, firstErr(nil, stderr))
	}
	return fmt.Errorf("git %s failed; the reason is in repochat's local log on the user's machine (ask the user to run `repochat doctor`)", subcommand)
}

// gitText runs a small query and returns trimmed stdout (or stderr when the
// exit code is non-zero).
func (r *Repo) gitText(ctx context.Context, args ...string) (string, int, error) {
	out, _, code, err := r.gitRaw(ctx, 64<<10, args...)
	return strings.TrimRight(string(out), "\r\n"), code, err
}

// refreshFilterArgs neutralises every configured clean/smudge/process filter
// driver, whichever configuration file defines it. A driver whose name cannot
// be expressed as a command-line override makes the repository unusable
// rather than leaving a program that git might run.
func (r *Repo) refreshFilterArgs(ctx context.Context) error {
	out, truncated, code, err := runGit(ctx, r.Root, []string{"--git-dir=" + r.GitDir, "--work-tree=" + r.Root}, 1<<20,
		"config", "-z", "--get-regexp", `^filter\..*\.(clean|smudge|process|required)$`)
	if err != nil {
		return err
	}
	if truncated {
		return errors.New("git filter configuration is implausibly large; refusing to continue")
	}
	var args []string
	if code == 0 {
		seen := map[string]bool{}
		for _, rec := range bytes.Split(out, []byte{0}) {
			key, _, _ := strings.Cut(string(rec), "\n")
			if !strings.HasPrefix(key, "filter.") {
				continue
			}
			name := key[len("filter."):]
			dot := strings.LastIndexByte(name, '.')
			if dot < 0 {
				continue
			}
			name = name[:dot] // may be empty: "filter..clean" is a valid key
			if seen[name] {
				continue
			}
			seen[name] = true
			if strings.ContainsAny(name, "=\n\r\x00") {
				return fmt.Errorf("git filter driver %q has a name repochat cannot neutralise; refusing to run git here", SanitizeLine(name))
			}
			args = append(args,
				"-c", "filter."+name+".clean=",
				"-c", "filter."+name+".smudge=",
				"-c", "filter."+name+".process=",
				"-c", "filter."+name+".required=false")
		}
	}
	r.mu.Lock()
	r.filterArgs = args
	r.mu.Unlock()
	return nil
}

// beforeWorktreeRead must be called before any git command that reads
// working-tree file contents (status, working-tree diff, blame of the working
// file): those are the commands that run clean filters. It re-reads the list
// of configured filter drivers so one added since the last look is
// neutralised too. A driver added during the command itself is outside what
// repochat can prevent (see the README: not a sandbox).
func (r *Repo) beforeWorktreeRead(ctx context.Context) error {
	return r.refreshFilterArgs(ctx)
}

// checkRoot fails closed when the directory at Root is no longer the one that
// was opened (renamed away and replaced), because git would follow the path
// while the held handle follows the original directory.
func (r *Repo) checkRoot() error {
	fi, err := os.Stat(r.Root)
	if err != nil || !os.SameFile(fi, r.rootInfo) {
		return errors.New("the repository directory was moved or replaced while repochat was running; restart repochat")
	}
	return nil
}
