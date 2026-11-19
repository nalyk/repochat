package repo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Withheld-path categories. A path is either served or withheld for exactly
// one of these reasons; withheld content is never returned by any tool.
const (
	ReasonUnsupported = "unsupported-name"   // not UTF-8, or contains control characters
	ReasonIgnoreFile  = "repochatignore"     // matched by .repochatignore
	ReasonDenyFlag    = "deny"               // matched by a --deny pattern
	ReasonSecretName  = "secret-name"        // matched by a built-in secret-file rule
	ReasonOwnFiles    = "repochat-files"     // the serving program's own configuration, state or helper
	ReasonUncertain   = "policy-unavailable" // .repochatignore could not be evaluated for this path
)

// Glob is a gitignore-style pattern.
//
//   - no slash        → matches a file or directory name at any depth (`*.pem`, `build`)
//   - contains slash  → anchored at the repository root (`src/**/*.ts`, `/Makefile`)
//   - trailing slash  → matches directories only (`docs/`)
//   - `*` and `?` never cross a `/`; `**` does
//
// A pattern that matches a directory matches everything beneath it.
type Glob struct {
	src     string
	re      *regexp.Regexp
	dirOnly bool
}

func CompileGlob(pattern string) (*Glob, error) {
	src := pattern
	p := strings.TrimSpace(pattern)
	p = strings.TrimPrefix(p, "./")
	if p == "" || p == "/" {
		return nil, fmt.Errorf("empty pattern")
	}
	if len(p) > 512 {
		return nil, fmt.Errorf("pattern too long")
	}
	g := &Glob{src: src}
	if strings.HasSuffix(p, "/") {
		g.dirOnly = true
		p = strings.TrimRight(p, "/")
	}
	anchored := strings.Contains(p, "/")
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return nil, fmt.Errorf("empty pattern")
	}

	var b strings.Builder
	b.WriteString("^")
	if !anchored {
		b.WriteString("(?:.*/)?")
	}
	for i := 0; i < len(p); {
		c := p[i]
		switch c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				atStart := i == 0 || p[i-1] == '/'
				if atStart && i+2 < len(p) && p[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 3
				} else {
					b.WriteString(".*")
					i += 2
				}
				continue
			}
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		case '[':
			class, n, err := parseClass(p[i:])
			if err != nil {
				return nil, err
			}
			b.WriteString(class)
			i += n
			continue
		case '\\':
			if i+1 < len(p) {
				i++
				b.WriteString(regexp.QuoteMeta(string(p[i])))
			}
		default:
			if c < utf8.RuneSelf {
				b.WriteString(regexp.QuoteMeta(string(c)))
			} else {
				b.WriteByte(c)
			}
		}
		i++
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("invalid pattern %q", pattern)
	}
	g.re = re
	return g, nil
}

// Match reports whether the file path p (slash-separated, repo-relative)
// matches the pattern directly or lies beneath a matching directory.
func (g *Glob) Match(p string) bool {
	if !g.dirOnly && g.re.MatchString(p) {
		return true
	}
	for i := len(p) - 1; i > 0; i-- {
		if p[i] == '/' && g.re.MatchString(p[:i]) {
			return true
		}
	}
	return false
}

func (g *Glob) String() string { return g.src }

// classLiteral writes one byte as a literal member of a regular-expression
// character class. Inside a class the characters \ ] [ ^ and - are special
// (a hyphen would otherwise form a range), so they are escaped.
func classLiteral(c byte) string {
	switch c {
	case '\\', ']', '[', '^', '-':
		return `\` + string(c)
	}
	return string(c)
}

var posixClasses = set("alnum", "alpha", "blank", "cntrl", "digit", "graph", "lower", "print", "punct", "space", "upper", "xdigit")

// parseClass translates a bracket expression starting at s[0] == '[' into a
// regular-expression class and returns how many bytes of s it consumed.
// Supported: negation (! or ^), ranges, a literal ] in first position,
// backslash escapes and POSIX classes such as [:digit:]. As in .gitignore, a
// class never matches a slash. Anything else is an error rather than a guess:
// a pattern that silently matched nothing would expose what it meant to hide.
func parseClass(s string) (string, int, error) {
	var b strings.Builder
	b.WriteString("[")
	i := 1
	negated := false
	if i < len(s) && (s[i] == '!' || s[i] == '^') {
		negated = true
		b.WriteString("^")
		i++
	}
	first := true
	for i < len(s) {
		c := s[i]
		switch {
		case c == ']' && !first:
			if negated {
				b.WriteString("/")
			}
			b.WriteString("]")
			return b.String(), i + 1, nil
		case c == '[' && i+1 < len(s) && s[i+1] == ':':
			end := strings.Index(s[i+2:], ":]")
			if end < 0 || !posixClasses[s[i+2:i+2+end]] {
				return "", 0, fmt.Errorf("unsupported character class in pattern")
			}
			b.WriteString("[:" + s[i+2:i+2+end] + ":]")
			i += end + 4
		case c == '\\' && i+1 < len(s):
			b.WriteString(classLiteral(s[i+1]))
			i += 2
		case c == '[' || c == ']' || c == '^' || c == '\\':
			b.WriteString(classLiteral(c))
			i++
		default:
			b.WriteByte(c)
			i++
		}
		first = false
	}
	return "", 0, fmt.Errorf("unterminated character class in pattern")
}

// CompileGlobs compiles a list of patterns, naming the offending one on error.
func CompileGlobs(patterns []string) ([]*Glob, error) {
	var out []*Glob
	for _, p := range patterns {
		if strings.TrimSpace(p) == "" {
			continue
		}
		g, err := CompileGlob(p)
		if err != nil {
			return nil, fmt.Errorf("pattern %q: %w", p, err)
		}
		out = append(out, g)
	}
	return out, nil
}

func matchAny(globs []*Glob, p string) bool {
	for _, g := range globs {
		if g.Match(p) {
			return true
		}
	}
	return false
}

// ignoreRule is one line of .repochatignore in this package's own, lenient
// reading (see Glob). What the rules mean is decided by git (ignoreOracle);
// this reading only ever adds to it: a path it withholds stays withheld even
// where git would serve it, as it was before git was asked at all.
type ignoreRule struct {
	glob   *Glob
	negate bool
}

// Policy decides, from the path alone, whether a file may be served. Because
// it is a pure function of the path it applies identically to the working
// tree and to every historical revision.
//
// Precedence (first match wins):
//  1. unsupported name            → withheld
//  2. .repochatignore             → withheld (the repository can only tighten)
//  3. --deny                      → withheld
//  4. --allow                     → served (overrides rule 5 only)
//  5. built-in secret-file rules  → withheld
type Policy struct {
	ignore []ignoreRule
	oracle *ignoreOracle // git's reading of the same rules; nil when there are none
	deny   []*Glob
	allow  []*Glob
	hash   string
	own    []string // see Options.Own
}

// NewPolicy builds a policy. ignoreFile is the content of .repochatignore
// ("" if absent). A line that cannot be understood is an error: skipping it
// would serve the files it was meant to withhold.
func NewPolicy(ignoreFile string, deny, allow []*Glob) (*Policy, error) {
	p := &Policy{deny: deny, allow: allow}
	h := sha256.New()
	// git skips a UTF-8 byte order mark at the start of an ignore file; kept,
	// it would become part of the first pattern, which then matches nothing.
	ignoreFile = strings.TrimPrefix(ignoreFile, "\xef\xbb\xbf")
	// Anything that is not UTF-8 text (UTF-16, binary data) would parse as
	// patterns that match nothing, i.e. as no rules at all.
	if strings.IndexByte(ignoreFile, 0) >= 0 || !utf8.ValidString(ignoreFile) {
		return nil, errors.New(".repochatignore is not UTF-8 text (it contains NUL bytes or invalid UTF-8; is it saved as UTF-16?). Save it as UTF-8; repochat will not guess what it should withhold")
	}
	for n, raw := range strings.Split(ignoreFile, "\n") {
		raw = strings.TrimSuffix(raw, "\r")
		line := strings.TrimRight(raw, "\r \t")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		negate := false
		if strings.HasPrefix(line, "!") {
			negate, line, raw = true, line[1:], raw[1:]
		} else if strings.HasPrefix(line, `\#`) || strings.HasPrefix(line, `\!`) {
			line = line[1:] // an escaped leading # or !
		}
		g, err := CompileGlob(line)
		if err != nil {
			return nil, fmt.Errorf(".repochatignore line %d cannot be understood (%v). Fix or remove that line; repochat will not guess what it should withhold", n+1, err)
		}
		p.ignore = append(p.ignore, ignoreRule{glob: g, negate: negate})
		fmt.Fprintf(h, "i:%v:%s\n", negate, raw)
	}
	if len(p.ignore) > 0 {
		p.oracle = newIgnoreOracle(ignoreFile)
		// Answers depend on the git that gives them.
		if _, id, err := gitIdentity(); err == nil {
			fmt.Fprintf(h, "git:%s\n", id)
		}
	}
	for _, g := range deny {
		fmt.Fprintf(h, "d:%s\n", g.src)
	}
	for _, g := range allow {
		fmt.Fprintf(h, "a:%s\n", g.src)
	}
	p.hash = hex.EncodeToString(h.Sum(nil))[:16]
	return p, nil
}

// Check returns "" when the path may be served, otherwise the withholding reason.
func (p *Policy) Check(rel string) string {
	// First of all, and beyond the reach of --allow: the serving program's
	// own files.
	if len(p.own) > 0 {
		lower := strings.ToLower(rel)
		for _, pre := range p.own {
			if pre == "" || lower == pre || strings.HasPrefix(lower, pre+"/") {
				return ReasonOwnFiles
			}
		}
	}
	if !SupportedName(rel) {
		return ReasonUnsupported
	}
	switch ignored, err := p.decide(context.Background(), rel); {
	case err != nil:
		return ReasonUncertain
	case ignored:
		return ReasonIgnoreFile
	}
	if matchAny(p.deny, rel) {
		return ReasonDenyFlag
	}
	if matchAny(p.allow, rel) {
		return ""
	}
	if builtinSecret(rel) {
		return ReasonSecretName
	}
	return ""
}

// lenient is the package's own reading of the rules: the last pattern that
// matches the file or a directory above it decides.
func (p *Policy) lenient(rel string) bool {
	withheld := false
	for _, r := range p.ignore {
		if r.glob.Match(rel) {
			withheld = !r.negate
		}
	}
	return withheld
}

// decide reports whether .repochatignore withholds the path: when git says
// so under these rules, or when the lenient reading does. An error means
// that git could not be asked; the path is then not known to be served.
func (p *Policy) decide(ctx context.Context, rel string) (bool, error) {
	if len(p.ignore) == 0 {
		return false, nil
	}
	if p.lenient(rel) {
		return true, nil
	}
	return p.oracle.ignored(ctx, rel)
}

// ignored is decide for callers that only need to know whether the path is
// withheld; not knowing counts as withheld.
func (p *Policy) ignored(rel string) bool {
	ignored, err := p.decide(context.Background(), rel)
	return ignored || err != nil
}

// Prime has git judge a whole list of paths at once, so that the checks
// which follow find their answers ready instead of each asking on its own.
// Paths that never reach git (unsupported names, the lenient reading's own
// verdicts) are left out. An error means the answers are not available;
// every check of those paths then reports them as withheld.
func (p *Policy) Prime(ctx context.Context, paths []string) error {
	if len(p.ignore) == 0 {
		return nil
	}
	ask := make([]string, 0, len(paths))
	for _, rel := range paths {
		if rel != "" && SupportedName(rel) && !p.lenient(rel) {
			ask = append(ask, rel)
		}
	}
	return p.oracle.decide(ctx, ask)
}

// SupportedName reports whether a path can be represented safely: valid
// UTF-8, no control characters, no empty/dot segments, reasonable length.
// Every character that Sanitize would show as an escape is excluded: a name
// is listed as Sanitize prints it, and a name printed with an escape could
// not be passed back to read the file.
func SupportedName(rel string) bool {
	if rel == "" || len(rel) > 4096 || !utf8.ValidString(rel) {
		return false
	}
	for _, r := range rel {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || invisibleControl(r) {
			return false
		}
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

var (
	secretDirs = set(".ssh", ".gnupg", ".aws")

	secretExact = set(
		".envrc", ".npmrc", ".pypirc", ".netrc", "_netrc", ".git-credentials",
		".htpasswd", "htpasswd", ".pgpass", ".my.cnf", ".dockercfg", ".s3cfg", ".boto",
		"kubeconfig", ".kubeconfig", "credentials", ".vault-token", "terraform.rc",
		".terraformrc", "secring.gpg", "wp-config.php", "master.key",
	)

	secretKeyPrefixes = []string{"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519"}

	secretExts = set(".pem", ".key", ".p12", ".pfx", ".jks", ".keystore", ".kdbx", ".ppk", ".p8", ".tfvars")

	// The keyword heuristic only applies to data/config-like files; it never
	// hides source code such as token.go or password.service.ts.
	dataExts = set("", ".json", ".yaml", ".yml", ".toml", ".ini", ".txt", ".conf", ".cfg", ".config",
		".properties", ".xml", ".env", ".csv", ".tsv", ".bak", ".old", ".orig", ".plist", ".sql", ".dat", ".list")

	secretWords = set("secret", "secrets", "credential", "credentials", "creds", "password", "passwords",
		"passwd", "token", "tokens", "apikey", "apikeys", "privatekey")

	secretPairs = [][2]string{{"api", "key"}, {"api", "keys"}, {"private", "key"}, {"access", "key"}, {"secret", "key"}, {"auth", "key"}}

	benignWords = set("example", "examples", "sample", "samples", "template", "templates", "tmpl", "dist",
		"schema", "spec", "test", "tests", "mock", "mocks", "fixture", "fixtures", "dummy", "fake", "demo", "defaults")

	serviceAccountRe = regexp.MustCompile(`(^|[._-])service[._-]?account|^client_secret|firebase-adminsdk|^gcp[-_].*key`)
)

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, i := range items {
		m[i] = true
	}
	return m
}

func nameTokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == '.' || r == '_' || r == '-' || r == ' ' })
}

func hasAny(tokens []string, words map[string]bool) bool {
	for _, t := range tokens {
		if words[t] {
			return true
		}
	}
	return false
}

// builtinSecret implements the built-in secret-file rules (case-insensitive).
func builtinSecret(rel string) bool {
	// Blanks around a name do not change what the file is (" id_rsa").
	segs := strings.Split(strings.ToLower(rel), "/")
	for i := range segs {
		segs[i] = strings.TrimSpace(segs[i])
	}
	lp := strings.Join(segs, "/")
	if segs[len(segs)-1] == "" { // a name made of blanks only is nobody's secret file
		return false
	}
	base := segs[len(segs)-1]
	for _, d := range segs[:len(segs)-1] {
		if secretDirs[d] {
			return true
		}
	}
	if lp == ".docker/config.json" || strings.HasSuffix(lp, "/.docker/config.json") ||
		lp == ".kube/config" || strings.HasSuffix(lp, "/.kube/config") {
		return true
	}
	if secretExact[base] {
		return true
	}
	tokens := nameTokens(base)
	if base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".env") {
		return !hasAny(tokens, benignWords)
	}
	for _, pre := range secretKeyPrefixes {
		if strings.HasPrefix(base, pre) && !strings.HasSuffix(base, ".pub") {
			return true
		}
	}
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if stem == "" { // dotfile without further extension, e.g. ".secrets"
		stem, ext = base[1:], ""
	}
	if secretExts[ext] || strings.Contains(base, ".tfstate") || strings.HasSuffix(base, ".tfvars.json") {
		return true
	}
	if ext == ".json" && serviceAccountRe.MatchString(base) {
		return true
	}
	if !dataExts[ext] {
		return false
	}
	stemTokens := nameTokens(stem)
	if hasAny(stemTokens, benignWords) {
		return false
	}
	if hasAny(stemTokens, secretWords) {
		return true
	}
	for i := 0; i+1 < len(stemTokens); i++ {
		for _, pair := range secretPairs {
			if stemTokens[i] == pair[0] && stemTokens[i+1] == pair[1] {
				return true
			}
		}
	}
	return false
}
