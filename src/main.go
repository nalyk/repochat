// RepoChat: a deliberately small, read-only, local MCP server for one Git repository.
// Use with OpenAI Secure MCP Tunnel (tunnel-client) to connect ChatGPT Web.
// No third-party Go dependencies. The MCP subprocess has no network listener.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const version = "0.1.0"
const maxReadBytes = 64 * 1024
const maxSearchBytes = 32 * 1024 * 1024

var ignoredDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".next": true,
	".nuxt": true, "dist": true, "build": true, "target": true, ".venv": true,
	"venv": true, "__pycache__": true, ".cache": true, ".idea": true,
	".terraform": true, "coverage": true, ".turbo": true, ".gradle": true,
}

var sensitiveFile = regexp.MustCompile(`(?i)(^\.env($|\.)|^\.npmrc$|^\.pypirc$|^id_(rsa|ed25519|ecdsa|dsa)(\.pub)?$|(^|[._-])(secret|secrets|credential|credentials|private[-_]?key|password|passwd|token|api[-_]?key)([._-]|$)|\.(pem|p12|pfx|key|keystore|jks)$|^\.netrc$|^\.aws$)`)

var textExtBlacklist = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	".ico": true, ".pdf": true, ".zip": true, ".gz": true, ".7z": true,
	".rar": true, ".tar": true, ".mp4": true, ".mp3": true, ".ogg": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".class": true,
	".so": true, ".dylib": true, ".exe": true, ".sqlite": true, ".db": true,
	".lockb": true,
}

type savedConfig struct {
	TunnelID   string `json:"tunnel_id"`
	RuntimeKey string `json:"runtime_key"`
}

func configFile() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "repochat", "config.json"), nil
}

func loadConfig() (savedConfig, error) {
	var c savedConfig
	path, err := configFile()
	if err != nil {
		return c, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return c, err
	}
	if info.Mode().Perm()&0077 != 0 {
		return c, errors.New("config permissions are too open: chmod 600 " + path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(b, &c)
	return c, err
}

func setup() error {
	path, err := configFile()
	if err != nil {
		return err
	}
	reader := bufio.NewReader(os.Stdin)
	tunnelID := strings.TrimSpace(os.Getenv("CONTROL_PLANE_TUNNEL_ID"))
	if tunnelID == "" {
		fmt.Fprint(os.Stderr, "OpenAI tunnel ID (tunnel_...): ")
		s, e := reader.ReadString('\n')
		if e != nil {
			return fmt.Errorf("reading tunnel ID: %w", e)
		}
		tunnelID = strings.TrimSpace(s)
	}
	if !strings.HasPrefix(tunnelID, "tunnel_") || len(tunnelID) < 15 {
		return errors.New("invalid tunnel ID")
	}
	apiKey := strings.TrimSpace(os.Getenv("CONTROL_PLANE_API_KEY"))
	if apiKey == "" {
		fmt.Fprint(os.Stderr, "Restricted runtime API key (hidden): ")
		hide := exec.Command("stty", "-echo")
		hide.Stdin = os.Stdin
		if e := hide.Run(); e != nil {
			return errors.New("cannot hide terminal input; set CONTROL_PLANE_API_KEY in environment instead")
		}
		// Restore echo even if reading the secret fails.
		defer func() {
			restore := exec.Command("stty", "echo")
			restore.Stdin = os.Stdin
			_ = restore.Run()
			fmt.Fprintln(os.Stderr)
		}()
		s, e := reader.ReadString('\n')
		if e != nil {
			return fmt.Errorf("reading runtime key: %w", e)
		}
		apiKey = strings.TrimSpace(s)
	}
	if len(apiKey) < 12 {
		return errors.New("runtime API key is too short")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(savedConfig{TunnelID: tunnelID, RuntimeKey: apiKey}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "config-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := tmp.Chmod(0600); err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Saved %s (0600). Run repochat doctor, then repochat from a repository.\n", path)
	return nil
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mcpCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type repository struct{ root string }

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--help", "-h", "help":
			printHelp()
			return
		case "--version", "version":
			fmt.Printf("repochat v%s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
			return
		case "doctor":
			doctor()
			return
		case "setup":
			if err := setup(); err != nil {
				fmt.Fprintln(os.Stderr, "repochat:", err)
				os.Exit(1)
			}
			return
		case "mcp":
			root := os.Getenv("REPOCHAT_ROOT")
			if root == "" {
				root = "."
			}
			repo, err := newRepository(root)
			if err != nil {
				fmt.Fprintln(os.Stderr, "repochat:", err)
				os.Exit(1)
			}
			if err := repo.serve(os.Stdin, os.Stdout); err != nil {
				fmt.Fprintln(os.Stderr, "repochat:", err)
				os.Exit(1)
			}
			return
		case "run": // Alias for plain invocation.
		default:
			fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", os.Args[1])
			printHelp()
			os.Exit(2)
		}
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "repochat:", err)
		os.Exit(1)
	}
}

func printHelp() {
	fmt.Println(`RepoChat v` + version + ` — read-only repository access from ChatGPT Web

Usage:
  cd /path/to/repo && repochat      Start bridge for the current repository
  repochat doctor                   Verify configuration
  repochat setup                    Save tunnel credentials securely (one time)
  repochat mcp                      Run MCP stdio server (for diagnostics)

First-time prerequisites:
  1. Install official OpenAI tunnel-client (one time).
  2. Create an OpenAI Secure MCP Tunnel; associate it with your ChatGPT workspace.
  3. In ChatGPT Plugins, create a private custom MCP plugin using Connection: Tunnel.
  4. Run repochat setup once to store tunnel credentials (0600) locally.

Security: read-only, no public listener, scoped to the current repository.
See README.md for the exact setup and limitations.`)
}

func doctor() {
	r, err := newRepository(".")
	if err != nil {
		fmt.Println("Repository:", err)
	} else {
		fmt.Println("Repository:", r.root)
	}
	if p, err := exec.LookPath("tunnel-client"); err == nil {
		fmt.Println("tunnel-client:", p)
	} else {
		fmt.Println("tunnel-client: NOT INSTALLED")
	}
	c, e := loadConfig()
	if e == nil {
		fmt.Println("Local config: present (permissions checked)")
	} else {
		fmt.Println("Local config: unavailable:", e)
	}
	if os.Getenv("CONTROL_PLANE_TUNNEL_ID") != "" || c.TunnelID != "" {
		fmt.Println("Tunnel ID: present")
	} else {
		fmt.Println("Tunnel ID: NOT SET")
	}
	if os.Getenv("CONTROL_PLANE_API_KEY") != "" || c.RuntimeKey != "" {
		fmt.Println("Runtime key: present (redacted)")
	} else {
		fmt.Println("Runtime key: NOT SET")
	}
}

func newRepository(path string) (*repository, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("not a directory: %s", abs)
	}
	// Require repository root. It also prevents unintentional access to $HOME.
	c := exec.Command("git", "-C", abs, "rev-parse", "--show-toplevel")
	c.Env = safeGitEnv()
	output, err := c.Output()
	if err != nil {
		return nil, errors.New("run inside a Git repository (git rev-parse --show-toplevel failed)")
	}
	top, err := filepath.EvalSymlinks(strings.TrimSpace(string(output)))
	if err != nil {
		return nil, err
	}
	if top != abs {
		return nil, fmt.Errorf("run from repository root (%s), not %s", top, abs)
	}
	return &repository{root: abs}, nil
}

func run() error {
	repo, err := newRepository(".")
	if err != nil {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return err
	}
	tunnelClient, err := exec.LookPath("tunnel-client")
	if err != nil {
		return errors.New("official tunnel-client not found in PATH; install it once from https://github.com/openai/tunnel-client/releases/latest")
	}
	tunnelID, apiKey := os.Getenv("CONTROL_PLANE_TUNNEL_ID"), os.Getenv("CONTROL_PLANE_API_KEY")
	if tunnelID == "" || apiKey == "" {
		cfg, e := loadConfig()
		if e != nil {
			return errors.New("tunnel credentials missing; run 'repochat setup' once (or export CONTROL_PLANE_TUNNEL_ID and CONTROL_PLANE_API_KEY)")
		}
		if tunnelID == "" {
			tunnelID = cfg.TunnelID
		}
		if apiKey == "" {
			apiKey = cfg.RuntimeKey
		}
	}
	if tunnelID == "" || apiKey == "" {
		return errors.New("tunnel credentials are incomplete; run 'repochat setup'")
	}
	fmt.Fprintf(os.Stderr, "RepoChat v%s | read-only | %s\n", version, repo.root)
	fmt.Fprintln(os.Stderr, "ChatGPT Web: use your installed custom MCP plugin (e.g. @RepoChat).")
	fmt.Fprintln(os.Stderr, "Ctrl+C stops this repository connection. One process per tunnel ID.")
	command := quoteCommand(binary) + " mcp"
	cmd := exec.Command(tunnelClient, "run", "--control-plane.tunnel-id", tunnelID, "--control-plane.api-key", "env:CONTROL_PLANE_API_KEY", "--mcp.command", command)
	cmd.Env = append(os.Environ(), "REPOCHAT_ROOT="+repo.root, "CONTROL_PLANE_API_KEY="+apiKey, "CONTROL_PLANE_TUNNEL_ID="+tunnelID)
	cmd.Dir = repo.root
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func quoteCommand(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func safeGitEnv() []string {
	// Avoid inheriting GIT_* variables that could redirect operations to other repositories.
	env := []string{}
	for _, v := range os.Environ() {
		key := strings.SplitN(v, "=", 2)[0]
		if strings.HasPrefix(key, "GIT_") {
			continue
		}
		if key == "PAGER" || key == "GIT_PAGER" {
			continue
		}
		env = append(env, v)
	}
	return append(env, "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat", "GIT_OPTIONAL_LOCKS=0", "GIT_CONFIG_NOSYSTEM=1")
}

func (r *repository) git(args ...string) (string, error) {
	params := []string{"--no-pager", "-C", r.root, "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}
	// --no-pager must be placed before subcommand; command list is trusted literals.
	// Move it to a valid position before -C in case of older Git versions.
	params = append(params, args...)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", params...)
	cmd.Env = safeGitEnv()
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("git command failed: %s", strings.TrimSpace(string(out)))
	}
	if len(out) > 24*1024 {
		return string(out[:24*1024]) + "\n[truncated]", nil
	}
	return strings.TrimSpace(string(out)), nil
}

func (r *repository) allowedPath(userPath string) (string, error) {
	if strings.TrimSpace(userPath) == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(userPath) {
		return "", errors.New("absolute paths are forbidden")
	}
	clean := filepath.Clean(userPath)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("path outside repository")
	}
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if part == ".git" || ignoredDirs[part] || sensitiveFile.MatchString(part) {
			return "", errors.New("path blocked by read-only safety policy")
		}
	}
	joined := filepath.Join(r.root, clean)
	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(r.root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("symlink escapes repository")
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == ".git" || ignoredDirs[part] || sensitiveFile.MatchString(part) {
			return "", errors.New("symlink resolves to blocked path")
		}
	}
	return resolved, nil
}

func (r *repository) readFile(p string, start, maxLines int) (string, error) {
	resolved, err := r.allowedPath(p)
	if err != nil {
		return "", err
	}
	if textExtBlacklist[strings.ToLower(filepath.Ext(resolved))] {
		return "", errors.New("binary file type blocked")
	}
	f, err := os.Open(resolved)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("only regular files can be read")
	}
	if start < 1 {
		start = 1
	}
	if maxLines < 1 {
		maxLines = 160
	}
	if maxLines > 300 {
		maxLines = 300
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4*1024), 1024*1024)
	var b strings.Builder
	line := 0
	n := 0
	for scanner.Scan() {
		line++
		if line < start {
			continue
		}
		raw := scanner.Bytes()
		if bytes.IndexByte(raw, 0) >= 0 || !utf8.Valid(raw) {
			return "", errors.New("not UTF-8 text")
		}
		if line >= start+maxLines {
			b.WriteString("[line limit reached]\n")
			break
		}
		s := fmt.Sprintf("%d: %s\n", line, string(raw))
		if b.Len()+len(s) > maxReadBytes {
			b.WriteString("[byte limit reached]\n")
			break
		}
		b.WriteString(s)
		n++
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if n == 0 {
		return fmt.Sprintf("%s: no lines from %d (possibly EOF)\n", p, start), nil
	}
	return fmt.Sprintf("FILE: %s (read-only; from line %d)\n%s", p, start, b.String()), nil
}

func (r *repository) tree(depth, limit int) (string, error) {
	if depth < 1 {
		depth = 3
	}
	if depth > 8 {
		depth = 8
	}
	if limit < 1 {
		limit = 250
	}
	if limit > 600 {
		limit = 600
	}
	var entries []string
	err := filepath.WalkDir(r.root, func(path string, de fs.DirEntry, e error) error {
		if e != nil {
			return nil
		}
		if path == r.root {
			return nil
		}
		rel, _ := filepath.Rel(r.root, path)
		d := strings.Count(rel, string(filepath.Separator)) + 1
		if ignoredDirs[de.Name()] && de.IsDir() {
			return filepath.SkipDir
		}
		if sensitiveFile.MatchString(de.Name()) {
			if de.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if de.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d > depth {
			if de.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if len(entries) >= limit {
			return io.EOF
		}
		name := rel
		if de.IsDir() {
			name += "/"
		}
		entries = append(entries, name)
		return nil
	})
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	sort.Strings(entries)
	if errors.Is(err, io.EOF) {
		entries = append(entries, "[entry limit reached]")
	}
	return strings.Join(entries, "\n"), nil
}

func (r *repository) search(q string, limit int, caseSensitive bool) (string, error) {
	if len(q) < 2 || len(q) > 200 {
		return "", errors.New("query length must be 2..200 characters")
	}
	if limit < 1 {
		limit = 60
	}
	if limit > 120 {
		limit = 120
	}
	needle := q
	if !caseSensitive {
		needle = strings.ToLower(q)
	}
	results := []string{}
	totalBytes := int64(0)
	totalFiles := 0
	deadline := time.Now().Add(8 * time.Second)
	err := filepath.WalkDir(r.root, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return nil
		}
		if p == r.root {
			return nil
		}
		if d.IsDir() {
			if ignoredDirs[d.Name()] || sensitiveFile.MatchString(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 || sensitiveFile.MatchString(d.Name()) || textExtBlacklist[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		if totalFiles >= 5000 || totalBytes >= maxSearchBytes || time.Now().After(deadline) || len(results) >= limit {
			return io.EOF
		}
		info, e := d.Info()
		if e != nil || !info.Mode().IsRegular() || info.Size() > 2*1024*1024 {
			return nil
		}
		f, e := os.Open(p)
		if e != nil {
			return nil
		}
		totalFiles++
		totalBytes += info.Size()
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 4*1024), 512*1024)
		lineNo := 0
		rel, _ := filepath.Rel(r.root, p)
		for scanner.Scan() {
			lineNo++
			raw := scanner.Bytes()
			if bytes.IndexByte(raw, 0) >= 0 || !utf8.Valid(raw) {
				break
			}
			s := string(raw)
			searchIn := s
			if !caseSensitive {
				searchIn = strings.ToLower(s)
			}
			if strings.Contains(searchIn, needle) {
				if len(s) > 280 {
					s = s[:280] + "…"
				}
				results = append(results, fmt.Sprintf("%s:%d: %s", rel, lineNo, strings.TrimSpace(s)))
				if len(results) >= limit {
					break
				}
			}
		}
		_ = f.Close()
		return nil
	})
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if len(results) == 0 {
		return "No matches within scanned text files.", nil
	}
	if errors.Is(err, io.EOF) {
		results = append(results, "[search capped by result/time/size limit]")
	}
	return strings.Join(results, "\n"), nil
}

func intArg(m map[string]any, key string, fallback int) int {
	if n, ok := m[key].(float64); ok {
		return int(n)
	}
	return fallback
}
func strArg(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func schema(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}
func strField(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func intField(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func (r *repository) tools() []tool {
	readonly := map[string]any{"readOnlyHint": true, "destructiveHint": false, "openWorldHint": false}
	return []tool{
		{"repo_info", "Git repository metadata and current branch. No files changed.", schema(map[string]any{}), readonly},
		{"tree", "List repository files (filtered, with depth and entry limits).", schema(map[string]any{"depth": intField("Max directory depth, 1..8"), "limit": intField("Max entries, 1..600")}), readonly},
		{"read_file", "Read UTF-8 source code, with line numbers and limits; blocks sensitive paths.", schema(map[string]any{"path": strField("Relative path within repository"), "start_line": intField("1-based first line"), "max_lines": intField("Maximum 300 lines")}, "path"), readonly},
		{"search_code", "Search a literal text substring in source files; case-insensitive by default.", schema(map[string]any{"query": strField("Literal string to find"), "limit": intField("Max matches, 1..120"), "case_sensitive": map[string]any{"type": "boolean"}}, "query"), readonly},
		{"git_status", "Read git status (working tree metadata, not file content).", schema(map[string]any{}), readonly},
		{"git_log", "Read recent Git commit metadata (hash, date, author, subject).", schema(map[string]any{"limit": intField("Number of commits 1..30")}), readonly},
	}
}

func (r *repository) call(name string, a map[string]any) (string, error) {
	switch name {
	case "repo_info":
		branch, err := r.git("branch", "--show-current")
		if err != nil {
			return "", err
		}
		head, err := r.git("rev-parse", "--short", "HEAD")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Repository: %s\nBranch: %s\nHEAD: %s\nMode: READ ONLY; secret-looking paths excluded.", r.root, branch, head), nil
	case "tree":
		return r.tree(intArg(a, "depth", 3), intArg(a, "limit", 250))
	case "read_file":
		return r.readFile(strArg(a, "path"), intArg(a, "start_line", 1), intArg(a, "max_lines", 160))
	case "search_code":
		return r.search(strArg(a, "query"), intArg(a, "limit", 60), a["case_sensitive"] == true)
	case "git_status":
		return r.git("status", "--short", "--untracked-files=normal")
	case "git_log":
		n := intArg(a, "limit", 10)
		if n < 1 {
			n = 10
		}
		if n > 30 {
			n = 30
		}
		return r.git("log", "-n", strconv.Itoa(n), "--date=short", "--format=%h | %ad | %an | %s")
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

func (r *repository) handle(req rpcRequest) any {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		return map[string]any{"protocolVersion": p.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}}, "serverInfo": map[string]any{"name": "repochat", "version": version}, "instructions": "Read-only local repository. Start with repo_info and tree; use search_code and read_file for evidence. Never assume an unread file was inspected. Paths that appear secret are blocked."}
	case "ping":
		return map[string]any{}
	case "tools/list":
		return map[string]any{"tools": r.tools()}
	case "tools/call":
		var p mcpCall
		if e := json.Unmarshal(req.Params, &p); e != nil {
			return toolResult("invalid tool arguments: "+e.Error(), true)
		}
		out, err := r.call(p.Name, p.Arguments)
		if err != nil {
			return toolResult(err.Error(), true)
		}
		if out == "" {
			out = "(empty)"
		}
		return toolResult(out, false)
	default:
		return nil
	}
}

func toolResult(s string, isErr bool) map[string]any {
	return map[string]any{"content": []map[string]string{{"type": "text", "text": s}}, "isError": isErr}
}

func (r *repository) serve(input io.Reader, output io.Writer) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4*1024), 8*1024*1024)
	enc := json.NewEncoder(output)
	for scanner.Scan() {
		var req rpcRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		if req.Method == "notifications/initialized" || strings.HasPrefix(req.Method, "notifications/") || len(req.ID) == 0 {
			continue
		}
		result := r.handle(req)
		if result == nil {
			if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "Method not found"}}); err != nil {
				return err
			}
			continue
		}
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}); err != nil {
			return err
		}
	}
	return scanner.Err()
}
