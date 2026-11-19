package cli

import (
	"strings"
	"testing"
)

// Asking a command for its options is not a usage error.
func TestHelpExitsZeroForEveryCommand(t *testing.T) {
	setup(t, "")
	for _, cmd := range []string{"run", "mcp", "setup", "doctor", "status", "stop", "tools", "call", "files", "log", "mcp-config", "install", "install-helper"} {
		if _, errText, code := runCLI(cmd, "--help"); code != 0 {
			t.Errorf("repochat %s --help: exit %d\n%s", cmd, code, errText)
		}
	}
}

// Go's flag package stops at the first word that is not an option and ignores
// everything after it. An unquoted pattern the shell expanded to two files
// (`--deny *.sql --no-history`) would then drop --no-history without a word and
// serve more than was asked for. Commands that take no arguments refuse them.
func TestStrayArgumentsAreRefused(t *testing.T) {
	e := setup(t, "")
	for _, args := range [][]string{
		{"mcp", "--root", e.repo, "--deny", "a.sql", "b.sql", "--no-history"},
		{"files", "--root", e.repo, "--deny", "a.sql", "b.sql", "--tracked-only"},
		{"tools", "--root", e.repo, "extra"},
		{"doctor", "--root", e.repo, "--offline", "extra"},
		{"mcp-config", "--root", e.repo, "extra"},
		{"status", "extra"},
		{"stop", "extra"},
		{"log", "extra"},
		{"setup", "--list", "extra"},
		{"install", "--dir", t.TempDir(), "extra"},
		{"install-helper", "extra"},
	} {
		_, errText, code := runCLI(args...)
		if code != 2 || !strings.Contains(errText, `unexpected argument`) {
			t.Errorf("repochat %s: exit %d, want 2 with an explanation\n%s", strings.Join(args, " "), code, errText)
		}
	}
}
