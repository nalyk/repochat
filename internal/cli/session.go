package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nalyk/repochat/internal/mcp"
	"github.com/nalyk/repochat/internal/repo"
	"github.com/nalyk/repochat/internal/tools"
)

// Version and Commit are set at build time (-ldflags -X).
var (
	Version = "1.0.0-dev"
	Commit  = ""
)

func versionString() string {
	// An untagged build is named after its commit already.
	if Commit != "" && !strings.HasPrefix(Version, Commit) {
		return Version + " (" + Commit + ")"
	}
	return Version
}

// session is everything the MCP server needs to know. The launcher freezes
// it and hands it to the `mcp` child; it never contains credentials.
type session struct {
	Root              string   `json:"root"`
	TrackedOnly       bool     `json:"tracked_only,omitempty"`
	NoHistory         bool     `json:"no_history,omitempty"`
	NoRedact          bool     `json:"no_redact,omitempty"`
	ShowWithheldNames bool     `json:"show_withheld_names,omitempty"`
	Allow             []string `json:"allow,omitempty"`
	Deny              []string `json:"deny,omitempty"`
	MaxOutputKB       int      `json:"max_output_kb,omitempty"`
	// Own is repochat's own directories (see repo.Options.Own), fixed by the
	// launcher so that the server withholds exactly what the launcher saw.
	Own       []string `json:"own,omitempty"`
	Quiet     bool     `json:"quiet,omitempty"`
	NoAudit   bool     `json:"no_audit,omitempty"`
	AuditArgs bool     `json:"audit_args,omitempty"`
	// Events is the launcher's control socket; the server reports activity
	// there instead of printing it.
	Events string `json:"events,omitempty"`
	ID     string `json:"id,omitempty"`
}

type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*l = append(*l, part)
		}
	}
	return nil
}

// sessionFlags registers the exposure options shared by every command that
// opens a repository.
func sessionFlags(fs *flag.FlagSet, s *session) {
	fs.StringVar(&s.Root, "root", "", "repository to serve (default: the one containing the current directory)")
	fs.BoolVar(&s.TrackedOnly, "tracked-only", false, "serve only files tracked by git (hide untracked files)")
	fs.BoolVar(&s.NoHistory, "no-history", false, "serve only the working tree, no past revisions")
	fs.BoolVar(&s.NoRedact, "no-redact", false, "do not mask secret-looking values")
	fs.BoolVar(&s.ShowWithheldNames, "show-withheld-names", false, "show the names of withheld files (their content stays withheld)")
	fs.Var((*listFlag)(&s.Allow), "allow", "serve paths matching `pattern` even if a built-in secret-file rule matches (repeatable)")
	fs.Var((*listFlag)(&s.Deny), "deny", "withhold paths matching `pattern` (repeatable, .gitignore syntax)")
	fs.IntVar(&s.MaxOutputKB, "max-output-kb", 0, "size budget per tool result in KB (16-2048, default 32)")
	fs.BoolVar(&s.Quiet, "quiet", false, "do not print live activity")
	fs.BoolVar(&s.NoAudit, "no-audit-log", false, "do not write the local activity log")
	fs.BoolVar(&s.AuditArgs, "audit-args", false, "record tool arguments (paths, queries) in the local activity log")
}

func (s *session) repoOptions(cfg *Config) repo.Options {
	o := repo.Options{
		TrackedOnly: s.TrackedOnly, NoRedact: s.NoRedact, NoHistory: s.NoHistory,
		ShowWithheldNames: s.ShowWithheldNames, MaxOutputBytes: s.MaxOutputKB << 10,
		Allow: append([]string{}, s.Allow...), Deny: append([]string{}, s.Deny...),
		Own: s.Own,
	}
	if len(o.Own) == 0 {
		o.Own = ownDirs()
	}
	if cfg != nil {
		o.Allow = append(o.Allow, cfg.Allow...)
		o.Deny = append(o.Deny, cfg.Deny...)
	}
	return o
}

// freeze resolves the session against the user configuration, so the child
// process needs nothing but the blob.
func (s *session) freeze(cfg *Config, root string) session {
	out := *s
	out.Root = root
	if cfg != nil {
		out.Allow = append(append([]string{}, s.Allow...), cfg.Allow...)
		out.Deny = append(append([]string{}, s.Deny...), cfg.Deny...)
	}
	out.Own = ownDirs()
	out.ID = newID()
	return out
}

// ownDirs lists the directories repochat keeps its own files in.
func ownDirs() []string {
	var dirs []string
	for _, get := range []func() (string, error){configDir, stateDir, dataDir} {
		if d, err := get(); err == nil && d != "" {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

func (s session) encode() string {
	raw, _ := json.Marshal(s)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeSession(blob string) (*session, error) {
	raw, err := base64.RawURLEncoding.DecodeString(blob)
	if err != nil {
		return nil, fmt.Errorf("invalid --session value")
	}
	var s session
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("invalid --session value")
	}
	return &s, nil
}

func newID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// openRepo opens the repository for a session.
func openRepo(ctx context.Context, s *session, cfg *Config) (*repo.Repo, error) {
	start := s.Root
	if start == "" {
		start = "."
	}
	r, err := repo.Open(ctx, start, s.repoOptions(cfg))
	if err == nil {
		r.Logf = stderrLogf(os.Stderr)
	}
	return r, err
}

// newServer builds the MCP server for a repository.
func newServer(r *repo.Repo) *mcp.Server {
	return &mcp.Server{
		Name:         "repochat",
		Title:        "RepoChat",
		Version:      Version,
		Instructions: tools.Instructions,
		Tools:        tools.New(r),
	}
}

// ---------------------------------------------------------------------------
// Activity rendering
// ---------------------------------------------------------------------------

// activity is one event as shown to the user and written to the audit log.
type activity struct {
	Time       time.Time      `json:"ts"`
	Session    string         `json:"session,omitempty"`
	Repo       string         `json:"repo,omitempty"`
	Kind       string         `json:"kind"`
	Client     string         `json:"client,omitempty"`
	Protocol   string         `json:"protocol,omitempty"`
	Tool       string         `json:"tool,omitempty"`
	OK         bool           `json:"ok"`
	Cancelled  bool           `json:"cancelled,omitempty"`
	Millis     int64          `json:"ms"`
	Bytes      int            `json:"bytes"`
	Redactions int            `json:"redactions,omitempty"`
	Error      string         `json:"error,omitempty"`
	Args       map[string]any `json:"args,omitempty"`
	// Summary is a short, masked, single-line rendering of the arguments for
	// the live display. It is never written to the audit log.
	Summary string `json:"summary,omitempty"`
}

func activityFrom(e mcp.Event, s *session, r *repo.Repo) activity {
	a := activity{
		Time: e.Time, Session: s.ID, Repo: r.ID, Kind: e.Kind, Client: repo.SanitizeLine(e.Client), Protocol: e.Protocol,
		Tool: e.Tool, OK: e.Err == "", Cancelled: e.Cancelled, Millis: e.Duration.Milliseconds(),
		Bytes: e.Bytes, Redactions: e.Redactions,
	}
	if e.Err != "" {
		msg, _ := r.Redactor().Line(firstLine(e.Err))
		a.Error = clip(repo.SanitizeLine(msg), 200)
	}
	if e.Kind == "call" {
		a.Summary = summarizeArgs(e.Args, r.Redactor())
		if s.AuditArgs {
			a.Args = boundedArgs(e.Args)
		}
	}
	return a
}

// Limits for arguments kept with --audit-args. They are the caller's raw
// arguments, megabytes if the caller likes; a record must stay small enough
// for the launcher's control socket and for `repochat log` to read back.
const (
	maxRecordedArgs  = 24
	maxRecordedName  = 80
	maxRecordedValue = 1024
	maxRecordedItems = 20
	maxRecordedItem  = 256
)

func boundedArgs(args map[string]any) map[string]any {
	if len(args) == 0 {
		return args
	}
	names := make([]string, 0, len(args))
	for k := range args {
		names = append(names, k)
	}
	// Real argument names are short; when there are too many, the short ones
	// are the ones worth keeping.
	sort.Slice(names, func(i, j int) bool {
		if len(names[i]) != len(names[j]) {
			return len(names[i]) < len(names[j])
		}
		return names[i] < names[j]
	})
	out := make(map[string]any, min(len(names), maxRecordedArgs)+1)
	for i, k := range names {
		if i == maxRecordedArgs {
			out["(more)"] = fmt.Sprintf("%d more argument(s) not recorded", len(names)-i)
			break
		}
		out[cut(k, maxRecordedName)] = boundedValue(args[k])
	}
	return out
}

func boundedValue(v any) any {
	switch t := v.(type) {
	case string:
		return cut(t, maxRecordedValue)
	case []any:
		items := make([]any, 0, min(len(t), maxRecordedItems)+1)
		for i, item := range t {
			if i == maxRecordedItems {
				items = append(items, fmt.Sprintf("(%d more item(s) not recorded)", len(t)-i))
				break
			}
			items = append(items, boundedScalar(item))
		}
		return items
	default:
		return boundedScalar(v)
	}
}

// boundedScalar keeps numbers, booleans and null; anything nested is not an
// argument any tool takes and is recorded only as its kind.
func boundedScalar(v any) any {
	switch t := v.(type) {
	case nil, bool, float64, int:
		return v
	case string:
		return cut(t, maxRecordedItem)
	case map[string]any:
		return "(object, not recorded)"
	case []any:
		return "(list, not recorded)"
	}
	return "(value, not recorded)"
}

// cut shortens s to at most n bytes of its beginning and says how much is
// missing.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + fmt.Sprintf("…(%d more bytes)", len(s)-n)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}

// summarizeArgs renders tool arguments on one line for the live display:
// masked, with control characters and line breaks escaped, and length-capped.
func summarizeArgs(args map[string]any, red *repo.Redactor) string {
	if len(args) == 0 {
		return ""
	}
	priority := map[string]int{"path": 0, "paths": 1, "query": 2, "rev": 3, "mode": 4, "base": 5, "target": 6, "range": 7}
	keys := make([]string, 0, len(args))
	for k := range args {
		if k != "cursor" {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		pi, oki := priority[keys[i]]
		pj, okj := priority[keys[j]]
		switch {
		case oki && okj:
			return pi < pj
		case oki != okj:
			return oki
		}
		return keys[i] < keys[j]
	})
	var parts []string
	for _, k := range keys {
		var v string
		switch t := args[k].(type) {
		case string:
			v = t
		case []any:
			items := make([]string, 0, len(t))
			for _, it := range t {
				items = append(items, fmt.Sprint(it))
			}
			v = strings.Join(items, ",")
		case float64:
			v = strconv.FormatFloat(t, 'f', -1, 64)
		default:
			v = fmt.Sprint(t)
		}
		v, _ = red.Line(v)
		v = clip(repo.SanitizeLine(v), 60)
		if k == "path" || k == "query" {
			if strings.ContainsAny(v, " ") {
				v = `"` + v + `"`
			}
			parts = append(parts, v)
		} else {
			// The name is the caller's text as much as the value is: these
			// are the raw arguments, reported even when the call is rejected
			// for carrying a name no tool has.
			name, _ := red.Line(k)
			parts = append(parts, clip(repo.SanitizeLine(name), 40)+"="+v)
		}
	}
	if _, ok := args["cursor"]; ok {
		parts = append(parts, "(continued)")
	}
	return clip(strings.Join(parts, " "), 140)
}

// formatActivity renders one live line.
func formatActivity(c *console, a activity) string {
	ts := a.Time.Local().Format("15:04:05")
	switch a.Kind {
	case "hello":
		who := a.Client
		if who == "" {
			who = "a client"
		}
		return fmt.Sprintf("%s  %s connected (MCP %s)", c.dim(ts), who, a.Protocol)
	case "list":
		return fmt.Sprintf("%s  %s", c.dim(ts), c.dim("tool list requested"))
	}
	status := c.green("→")
	tail := fmt.Sprintf("%s  %dms", humanBytes(a.Bytes), a.Millis)
	switch {
	case a.Cancelled:
		status, tail = c.amber("×"), "cancelled"
	case !a.OK:
		status, tail = c.red("✗"), a.Error
	}
	line := fmt.Sprintf("%s  %-13s %s  %s %s", c.dim(ts), c.cyan(a.Tool), a.Summary, status, tail)
	if a.Redactions > 0 {
		line += c.dim(fmt.Sprintf("  (%d masked)", a.Redactions))
	}
	return line
}

func humanBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
}

// stderrLogf is the server's diagnostic logger.
func stderrLogf(w io.Writer) func(string, ...any) {
	return func(format string, args ...any) {
		fmt.Fprintf(w, "repochat: "+format+"\n", args...)
	}
}

// isSecretVar reports whether an environment variable name looks like it
// holds a credential.
func isSecretVar(name string) bool {
	up := strings.ToUpper(name)
	return strings.HasPrefix(up, "CONTROL_PLANE_") || up == "OPENAI_API_KEY" || up == "OPENAI_ADMIN_KEY" ||
		strings.HasSuffix(up, "_API_KEY") || strings.HasSuffix(up, "_TOKEN") || strings.HasSuffix(up, "_SECRET")
}

// scrubSecretsFromEnv removes credentials from this process's environment.
// The MCP server never needs them, and nothing it starts should inherit them.
func scrubSecretsFromEnv() {
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); isSecretVar(k) {
			os.Unsetenv(k)
		}
	}
}

// serverEnvironment returns the part of env the MCP server may keep, and
// whether anything was left out. It is a list of what the server and the git
// it starts need, not a list of what to remove: nobody can enumerate the names
// under which people keep credentials.
func serverEnvironment(env []string) (kept []string, removed bool) {
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		// Names are listed one by one; a prefix such as XDG_ or REPOCHAT_
		// would be no boundary at all. LC_ is the exception: those are the
		// locale categories, which git reads.
		if serverEnvNames[name] || strings.HasPrefix(name, "LC_") {
			kept = append(kept, kv)
		} else {
			removed = true
		}
	}
	return kept, removed
}

var serverEnvNames = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
	"LANG": true, "LANGUAGE": true, "TZ": true, "TMPDIR": true, "TERM": true, "NO_COLOR": true,
	"XDG_CONFIG_HOME": true, "XDG_STATE_HOME": true, "XDG_DATA_HOME": true, "XDG_RUNTIME_DIR": true,
	"REPOCHAT_CONFIG_DIR": true, "REPOCHAT_STATE_DIR": true, "REPOCHAT_DATA_DIR": true,
}

// CleanServerStart must be the first thing the program does. When this
// process is about to be the MCP server and was started with more in its
// environment than the server needs, it replaces itself with a copy that has
// only that.
//
// tunnel-client needs the API key and hands its whole environment, key
// included, to the server it starts. Unsetting the variable later only
// changes what the server's own children inherit: the value stays in this
// process's memory and in its environment as the system shows it
// (/proc/<pid>/environ). The server reads untrusted repository content and
// answers a remote caller; it should not be holding the key at all. If the
// program cannot start itself again it does not serve: carrying on would
// quietly give up the very protection this exists for.
func CleanServerStart() {
	if len(os.Args) < 2 || os.Args[1] != "mcp" {
		return
	}
	clean, removed := serverEnvironment(os.Environ())
	if !removed {
		return
	}
	exe, err := os.Executable()
	if err == nil {
		err = syscall.Exec(exe, os.Args, clean)
	}
	fmt.Fprintf(os.Stderr, "repochat: cannot restart the MCP server with a clean environment (%v); not serving. Reinstall repochat or start it by its full path\n", err)
	os.Exit(1)
}
