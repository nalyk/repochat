package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Withheld-path categories. A path is either served or withheld for exactly
// one of these reasons; withheld content is never returned by any tool.
const (
	ReasonUnsupported = "unsupported-name" // not UTF-8, or contains control characters
	ReasonIgnoreFile  = "repochatignore"   // matched by .repochatignore
	ReasonDenyFlag    = "deny"             // matched by a --deny pattern
	ReasonSecretName  = "secret-name"      // matched by a built-in secret-file rule
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
			end := strings.IndexByte(p[i+1:], ']')
			if end <= 0 {
				b.WriteString(`\[`)
				break
			}
			class := p[i+1 : i+1+end]
			neg := false
			if class[0] == '!' || class[0] == '^' {
				neg, class = true, class[1:]
			}
			if class == "" {
				b.WriteString(`\[`)
				break
			}
			b.WriteString("[")
			if neg {
				b.WriteString("^")
			}
			b.WriteString(strings.NewReplacer(`\`, `\\`, `[`, `\[`).Replace(class))
			b.WriteString("]")
			i += end + 2
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
	deny   []*Glob
	allow  []*Glob
	hash   string
}

// NewPolicy builds a policy. ignoreFile is the content of .repochatignore
// ("" if absent); invalid lines in it are skipped and returned as warnings.
func NewPolicy(ignoreFile string, deny, allow []*Glob) (*Policy, []string) {
	p := &Policy{deny: deny, allow: allow}
	var warnings []string
	h := sha256.New()
	for n, line := range strings.Split(ignoreFile, "\n") {
		line = strings.TrimRight(line, "\r \t")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		negate := false
		if strings.HasPrefix(line, "!") {
			negate, line = true, line[1:]
		}
		line = strings.TrimPrefix(line, `\`) // "\#name" / "\!name" escapes
		g, err := CompileGlob(line)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf(".repochatignore line %d ignored: %v", n+1, err))
			continue
		}
		p.ignore = append(p.ignore, ignoreRule{glob: g, negate: negate})
		fmt.Fprintf(h, "i:%v:%s\n", negate, line)
	}
	for _, g := range deny {
		fmt.Fprintf(h, "d:%s\n", g.src)
	}
	for _, g := range allow {
		fmt.Fprintf(h, "a:%s\n", g.src)
	}
	p.hash = hex.EncodeToString(h.Sum(nil))[:16]
	return p, warnings
}

// Check returns "" when the path may be served, otherwise the withholding reason.
func (p *Policy) Check(rel string) string {
	if !SupportedName(rel) {
		return ReasonUnsupported
	}
	ignored := false
	for _, r := range p.ignore {
		if r.glob.Match(rel) {
			ignored = !r.negate
		}
	}
	if ignored {
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

// SupportedName reports whether a path can be represented safely: valid
// UTF-8, no control characters, no empty/dot segments, reasonable length.
func SupportedName(rel string) bool {
	if rel == "" || len(rel) > 4096 || !utf8.ValidString(rel) {
		return false
	}
	for _, r := range rel {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == '\u2028' || r == '\u2029' {
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
	lp := strings.ToLower(rel)
	segs := strings.Split(lp, "/")
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
