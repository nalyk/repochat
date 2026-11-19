package repo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func mkfifo(path string) error { return syscall.Mkfifo(path, 0o644) }

var bg = context.Background()

// gitCmd runs plain git (not the hardened wrapper) to build fixtures.
func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@example.com",
		"GIT_COMMITTER_NAME=Ada", "GIT_COMMITTER_EMAIL=ada@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_DATE=2026-01-02T03:04:05Z", "GIT_COMMITTER_DATE=2026-01-02T03:04:05Z",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newDir creates an empty repository and returns its resolved path.
func newDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Keep the user's real git configuration out of the tests.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(os.Getenv("HOME"), ".config"))
	gitCmd(t, dir, "init", "-q")
	gitCmd(t, dir, "symbolic-ref", "HEAD", "refs/heads/main")
	return dir
}

func commit(t *testing.T, dir, msg string) string {
	t.Helper()
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-q", "--allow-empty", "-m", msg)
	return gitCmd(t, dir, "rev-parse", "HEAD")
}

func open(t *testing.T, dir string, opts Options) *Repo {
	t.Helper()
	r, err := Open(bg, dir, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// fresh drops the cached snapshot so a test sees its latest file changes.
func fresh(r *Repo) {
	r.mu.Lock()
	r.snap = nil
	r.mu.Unlock()
}

func mustContain(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Errorf("expected %q in:\n%s", w, text)
		}
	}
}

func mustNotContain(t *testing.T, text string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(text, b) {
			t.Errorf("did not expect %q in:\n%s", b, text)
		}
	}
}

const fakePEM = "-----BEGIN RSA PRIVATE KEY-----\n" +
	"MIIEowIBAAKCAQEAxQ3mG1kq0Tt0f8VhXw9aY7b2c1dE5fG6hI7jK8lM9nO0pQ1r\n" +
	"S2tU3vW4xY5zA6bC7dD8eE9fF0gG1hH2iI3jJ4kK5lL6mM7nN8oO9pP0qQ1rR2sS\n" +
	"T3uU4vV5wW6xX7yY8zZ9aA0bB1cC2dD3eE4fF5gG6hH7iI8jJ9kK0lL1mM2nN3oO\n" +
	"-----END RSA PRIVATE KEY-----\n"

const fakeAWSKey = "AKIAIOSFODNN7EXAMPLE"
