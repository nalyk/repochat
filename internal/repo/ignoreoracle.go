package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ignoreOracle answers one question: under the rules of .repochatignore, is
// this path ignored? The answer is git's own. The file is documented as
// having the syntax of a .gitignore, and what that syntax means is whatever
// the user's git does with it: `git check-ignore` is how they would check a
// rule themselves. An earlier version re-implemented git's matcher here; it
// agreed with one git release and served files that a newer one ignores.
//
// git is asked about path strings, not files: `check-ignore --no-index`
// evaluates rules against names whether or not anything exists there, so a
// path that occurs only in an old commit is judged exactly like one in the
// working tree. It runs in a scratch repository made for the purpose, whose
// only ignore source is the rules, with the user's, the system's and the
// served repository's configuration out of reach.
//
// Paths are asked about in batches and the answers are kept for as long as
// the rules and the git program stay the same. When git cannot be asked, or
// answers in a form that is not understood, the path is not known to be
// served: callers treat it as withheld.
type ignoreOracle struct {
	rules []byte

	mu       sync.Mutex
	gitID    string // identity of the git program the answers come from
	known    map[string]bool
	failed   error // the last failure, remembered briefly (see decide)
	failedAt time.Time
	runs     int // git processes started, for tests
}

const (
	oracleBatch       = 20_000           // paths per git process
	oracleMaxKnown    = 1_000_000        // answers kept; beyond that the cache starts over
	oracleRetryAfter  = 5 * time.Second  // how long a failure stands before git is asked again
	oracleTimeout     = 30 * time.Second // per git process
	oracleExcludeFile = ".git/info/exclude"
)

func newIgnoreOracle(rules string) *ignoreOracle {
	return &ignoreOracle{rules: []byte(rules), known: map[string]bool{}}
}

// gitIdentity names the git program in use. If it is replaced (an upgrade
// while repochat runs), its answers may differ and the old ones are dropped.
func gitIdentity() (path, id string, err error) {
	path, err = exec.LookPath("git")
	if err != nil {
		return "", "", errors.New("git is not installed (it must be on PATH)")
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	fi, err := os.Stat(path)
	if err != nil {
		return "", "", err
	}
	return path, fmt.Sprintf("%s|%d|%d", path, fi.Size(), fi.ModTime().UnixNano()), nil
}

// decide makes sure an answer is known for every path, asking git once per
// batch of paths it has no answer for yet.
func (o *ignoreOracle) decide(ctx context.Context, paths []string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	git, id, err := gitIdentity()
	if err != nil {
		return err
	}
	if id != o.gitID {
		o.gitID, o.known, o.failed = id, map[string]bool{}, nil
	}
	var ask []string
	seen := map[string]bool{}
	for _, p := range paths {
		if _, ok := o.known[p]; !ok && !seen[p] {
			seen[p] = true
			ask = append(ask, p)
		}
	}
	if len(ask) == 0 {
		return nil
	}
	// A failure is not retried path by path: that would start one process
	// per file exactly when something is wrong.
	if o.failed != nil && time.Since(o.failedAt) < oracleRetryAfter {
		return o.failed
	}
	if len(o.known)+len(ask) > oracleMaxKnown {
		o.known = map[string]bool{}
	}
	for len(ask) > 0 {
		n := min(len(ask), oracleBatch)
		answers, err := o.ask(ctx, git, ask[:n])
		if err != nil {
			o.failed, o.failedAt = err, time.Now()
			return err
		}
		for i, p := range ask[:n] {
			o.known[p] = answers[i]
		}
		ask = ask[n:]
	}
	o.failed = nil
	return nil
}

// ignored returns git's answer for one path.
func (o *ignoreOracle) ignored(ctx context.Context, p string) (bool, error) {
	if err := o.decide(ctx, []string{p}); err != nil {
		return false, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	answer, ok := o.known[p]
	if !ok {
		return false, errors.New("no answer for the path")
	}
	return answer, nil
}

// ask runs one git process over paths and returns, in order, whether each is
// ignored.
func (o *ignoreOracle) ask(ctx context.Context, git string, paths []string) ([]bool, error) {
	o.runs++
	scratch, err := os.MkdirTemp("", "repochat-ignore-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	// A repository made by hand rather than by `git init`, which would copy
	// the user's template directory (hooks included) into it.
	work := filepath.Join(scratch, "w")
	gitDir := filepath.Join(work, ".git")
	for _, d := range []string{"objects", "refs", "info"} {
		if err := os.MkdirAll(filepath.Join(gitDir, d), 0o700); err != nil {
			return nil, err
		}
	}
	files := map[string][]byte{
		"HEAD":         []byte("ref: refs/heads/main\n"),
		"config":       []byte("[core]\n\trepositoryformatversion = 0\n\tbare = false\n"),
		"info/exclude": o.rules,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(gitDir, name), data, 0o600); err != nil {
			return nil, err
		}
	}

	ctx, cancel := context.WithTimeout(ctx, oracleTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, git, "--no-pager",
		// The rules are the only ignore source: no per-user file, and no
		// folding of case that a file system or a configuration might ask
		// for (the rest of the policy compares names exactly).
		"-c", "core.excludesFile=/dev/null",
		"-c", "core.ignoreCase=false",
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=/dev/null",
		"check-ignore", "--no-index", "-v", "-n", "-z", "--stdin")
	cmd.Dir = work
	// Nothing of the caller's environment: no GIT_* variable, no
	// configuration of the user or the system, an empty home.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + scratch,
		"XDG_CONFIG_HOME=" + filepath.Join(scratch, "xdg"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_DIR=" + gitDir,
		"GIT_WORK_TREE=" + work,
		"GIT_CEILING_DIRECTORIES=" + scratch,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"LC_ALL=C",
	}
	// "./" in front of every path: a name that starts with ":" would
	// otherwise be read as pathspec magic, which this command either
	// misreads or refuses.
	var in bytes.Buffer
	for _, p := range paths {
		in.WriteString("./")
		in.WriteString(p)
		in.WriteByte(0)
	}
	cmd.Stdin = &in
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	var stderr cappedBuffer
	stderr.limit = 4 << 10
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	// Exit status 1 is not a failure: it says that none of the paths is
	// ignored. Anything else is.
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// git's own words may quote a path; they are not passed on.
		return nil, fmt.Errorf("git could not evaluate .repochatignore (%v)", err)
	}
	// One record per path, in order: source, line number, pattern, path.
	// For a path no rule matches the first three are empty; a pattern that
	// starts with "!" is a rule that re-includes the path.
	fields := strings.Split(string(out), "\x00")
	if len(fields) != 4*len(paths)+1 || fields[len(fields)-1] != "" {
		return nil, fmt.Errorf("git answered about %d path(s) when asked about %d", (len(fields)-1)/4, len(paths))
	}
	// The rules file as git may name it: relative to the scratch repository,
	// or by the path it was given, with or without symbolic links resolved.
	exclude := filepath.Join(gitDir, "info", "exclude")
	sources := map[string]bool{oracleExcludeFile: true, exclude: true}
	if resolved, err := filepath.EvalSymlinks(exclude); err == nil {
		sources[resolved] = true
	}
	answers := make([]bool, len(paths))
	for i, p := range paths {
		source, line, pattern, path := fields[4*i], fields[4*i+1], fields[4*i+2], fields[4*i+3]
		if path != "./"+p {
			return nil, errors.New("git answered about a different path than it was asked about")
		}
		switch {
		case source == "" && line == "" && pattern == "":
			answers[i] = false
		case sources[source] && pattern != "":
			answers[i] = !strings.HasPrefix(pattern, "!")
		default:
			return nil, errors.New("git reported a rule from an unexpected source")
		}
	}
	return answers, nil
}
