package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testRepo(t *testing.T) *repository {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q", dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, output)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc hello() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("password=NOPE\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".env", filepath.Join(dir, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "external.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	repo, err := newRepository(dir)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}
func TestRootAndRead(t *testing.T) {
	r := testRepo(t)
	body, err := r.readFile("main.go", 1, 20)
	if err != nil || !strings.Contains(body, "func hello") {
		t.Fatalf("unexpected read: %v %q", err, body)
	}
	for _, name := range []string{"../etc/passwd", "/etc/passwd", ".env", "alias.txt", "external.txt", ".git/config"} {
		if _, err = r.readFile(name, 1, 20); err == nil {
			t.Errorf("read should block %q", name)
		}
	}
}
func TestSearchAndTree(t *testing.T) {
	r := testRepo(t)
	found, err := r.search("hello", 30, false)
	if err != nil || !strings.Contains(found, "main.go:2:") {
		t.Fatalf("search: %v %s", err, found)
	}
	leaked, err := r.search("NOPE", 30, false)
	if err != nil || !strings.Contains(leaked, "No matches") {
		t.Fatalf("secret search: %v %s", err, leaked)
	}
	tree, err := r.tree(3, 250)
	if err != nil || strings.Contains(tree, ".env") || strings.Contains(tree, ".git/") {
		t.Fatalf("tree: %v %s", err, tree)
	}
}
func TestMCP(t *testing.T) {
	r := testRepo(t)
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"main.go"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"read_file","arguments":{"path":".env"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"git_status","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"nonexistent","arguments":{}}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := r.serve(strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n"))
	if len(lines) != 6 {
		t.Fatalf("want 6 JSON RPC replies, got %d: %s", len(lines), out.String())
	}
	for i, line := range lines {
		var data map[string]any
		if err := json.Unmarshal(line, &data); err != nil {
			t.Fatalf("reply %d malformed: %v", i, err)
		}
		if data["jsonrpc"] != "2.0" {
			t.Fatalf("bad envelope %v", data)
		}
	}
	if !bytes.Contains(lines[1], []byte("search_code")) {
		t.Fatal("tools not listed")
	}
	if !bytes.Contains(lines[2], []byte("func hello")) {
		t.Fatal("read_file result missing")
	}
	if !bytes.Contains(lines[3], []byte(`"isError":true`)) {
		t.Fatal("secret path not blocked")
	}
	if !bytes.Contains(lines[5], []byte(`"isError":true`)) {
		t.Fatal("unknown tool should fail")
	}
}

func TestSetupConfigPermissions(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CONTROL_PLANE_TUNNEL_ID", "tunnel_0123456789abcdef0123456789abcdef")
	t.Setenv("CONTROL_PLANE_API_KEY", "sk-test-placeholder-not-real")
	if err := setup(); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TunnelID != "tunnel_0123456789abcdef0123456789abcdef" || cfg.RuntimeKey != "sk-test-placeholder-not-real" {
		t.Fatalf("bad config")
	}
	p, err := configFile()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("permissions not restrictive: %v %v", info, err)
	}
	if err := os.Chmod(p, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(); err == nil {
		t.Fatal("insecure config should be rejected")
	}
}
