package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A directory name is repository-controlled text like any file name: cloning
// creates it from the URL. It must not reach the terminal with its control
// characters intact.
func TestRepositoryPathIsEscapedOnDisplay(t *testing.T) {
	e := setup(t, "")
	saveProfile(t, testKey)
	base, _ := filepath.EvalSymlinks(t.TempDir())
	root := filepath.Join(base, "pro\x1b]0;TITLE\x07\x1b[2Jject")
	if err := os.Rename(e.repo, root); err != nil {
		t.Fatal(err)
	}
	check := func(what, text string) {
		t.Helper()
		if strings.ContainsAny(text, "\x1b\x07") {
			t.Errorf("%s prints the path with raw control characters: %q", what, text)
		}
		if !strings.Contains(text, `pro\x1b]0;TITLE`) {
			t.Errorf("%s should show the path in escaped form: %q", what, text)
		}
	}
	_, errText, _ := runCLI("files", "--root", root)
	check("files", errText)
	_, errText, _ = runCLI("doctor", "--root", root, "--offline")
	check("doctor", errText)
	out, errText, _ := runCLI("mcp-config", "--root", root)
	check("mcp-config", out+errText)

	_, errA, done := start("--root", root)
	waitFor(t, "the share to start", func() bool { return strings.Contains(errA.String(), "connected to OpenAI") })
	check("the banner", errA.String())
	out, errText, _ = runCLI("status")
	check("status", out+errText)

	// A second start in the same repository names it in its refusal.
	_, errText, _ = runCLI("--root", makeRepo(t, "other"))
	check("the takeover question", errText)
	_, errText, _ = runCLI("stop")
	check("stop", errText)
	<-done
}

// An override given as a relative path must mean one binary. It is checked
// from the current directory and started from the repository root: left
// relative, those can be two different files.
func TestHelperOverrideIsMadeAbsolute(t *testing.T) {
	e := setup(t, "")
	wrapper := os.Getenv("REPOCHAT_TUNNEL_CLIENT")
	t.Chdir(filepath.Dir(wrapper))
	t.Setenv("REPOCHAT_TUNNEL_CLIENT", "./"+filepath.Base(wrapper))
	path, _, err := findHelper()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("findHelper returned the relative path %q (repository at %s)", path, e.repo)
	}
}

// An untagged build is named after its commit; saying the commit twice
// ("74d46e7 (74d46e7)") is noise.
func TestVersionStringDoesNotRepeatTheCommit(t *testing.T) {
	v, c := Version, Commit
	defer func() { Version, Commit = v, c }()
	for _, tc := range []struct{ version, commit, want string }{
		{"v1.2.0", "abc1234", "v1.2.0 (abc1234)"},
		{"abc1234", "abc1234", "abc1234"},
		{"abc1234-dirty", "abc1234", "abc1234-dirty"},
		{"1.0.0-dev", "", "1.0.0-dev"},
	} {
		Version, Commit = tc.version, tc.commit
		if got := versionString(); got != tc.want {
			t.Errorf("versionString(%q, %q) = %q, want %q", tc.version, tc.commit, got, tc.want)
		}
	}
}

// With --verbose tunnel-client's own output is echoed. Whatever it prints,
// the API key it was given must not appear on the terminal.
func TestVerboseEchoNeverShowsTheKey(t *testing.T) {
	var out lockedBuffer
	w := prefixWriter{w: &out, prefix: "| ", secret: testKey}
	w.Write([]byte("poll failed: bearer " + testKey + " rejected\nsecond line\n"))
	if strings.Contains(out.String(), testKey) || !strings.Contains(out.String(), "[api key]") || !strings.Contains(out.String(), "| second line") {
		t.Fatalf("%q", out.String())
	}
}

// A home directory under git contains repochat's own configuration, API key
// included. It is never served, and a home directory is not served through
// plain `mcp` either without being asked for explicitly.
func TestOwnConfigurationIsNeverServed(t *testing.T) {
	e := setup(t, "")
	t.Setenv("REPOCHAT_CONFIG_DIR", filepath.Join(e.repo, ".config", "repochat"))
	t.Setenv("REPOCHAT_STATE_DIR", filepath.Join(e.repo, ".local", "state", "repochat"))
	saveProfile(t, testKey)
	out, _, _ := runCLI("files", "--root", e.repo, "--allow", "**")
	if strings.Contains(out, "config.json") || !strings.Contains(out, "README.md") {
		t.Fatalf("files lists the credentials file as served:\n%s", out)
	}
	out, _, _ = runCLI("files", "--root", e.repo, "--withheld")
	wantIn(t, out, ".config/repochat/config.json\trepochat-files")
	out, errText, code := runCLI("call", "--root", e.repo, "--no-redact", "read_file", `{"path":".config/repochat/config.json"}`)
	if code == 0 || strings.Contains(out+errText, testKey) {
		t.Fatalf("the API key was served: %s%s", out, errText)
	}
}

func TestPlainServerRefusesTheHomeDirectory(t *testing.T) {
	e := setup(t, "")
	t.Setenv("HOME", e.repo)
	_, errText, code := runCLI("mcp", "--root", e.repo, "--quiet")
	if code == 0 || !strings.Contains(errText, "--allow-broad-root") {
		t.Fatalf("mcp must refuse a home directory as root: exit %d %q", code, errText)
	}
	for _, cmd := range []string{"files", "tools"} {
		if _, errText, code := runCLI(cmd, "--root", e.repo); code != 0 {
			t.Errorf("%s only looks, locally, and must still work: %s", cmd, errText)
		}
	}
}
