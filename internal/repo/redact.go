package repo

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Redactor masks high-confidence secret patterns in text that is about to be
// disclosed. It is best-effort defense in depth, not a guarantee: a secret in
// a format it does not know is not detected.
type Redactor struct{ enabled bool }

func NewRedactor(enabled bool) *Redactor { return &Redactor{enabled: enabled} }

func (r *Redactor) Enabled() bool { return r != nil && r.enabled }

type redactRule struct {
	name string
	re   *regexp.Regexp
	repl string // "" → "[REDACTED:name]"
}

var redactRules = []redactRule{
	{name: "private-key", re: regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----.*?-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)},
	{name: "aws-access-key-id", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{name: "aws-secret-key", re: regexp.MustCompile(`(?i)(aws_?secret_?access_?key["']?\s*[:=]\s*["']?)[A-Za-z0-9/+=]{40}`), repl: "${1}[REDACTED:aws-secret-key]"},
	{name: "github-token", re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_[A-Za-z0-9_]{22,255})`)},
	{name: "gitlab-token", re: regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{20,}`)},
	{name: "slack-token", re: regexp.MustCompile(`\bxox[abeprs]-[A-Za-z0-9\-]{10,}`)},
	{name: "slack-webhook", re: regexp.MustCompile(`https://hooks\.slack\.com/services/[A-Za-z0-9/_\-]{20,}`)},
	{name: "api-key", re: regexp.MustCompile(`\bsk-(?:proj|svcacct|admin|ant)-[A-Za-z0-9_\-]{20,}|\bsk-[A-Za-z0-9]{32,}`)},
	{name: "google-api-key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}`)},
	{name: "stripe-key", re: regexp.MustCompile(`\b(?:sk|rk)_live_[0-9A-Za-z]{16,}`)},
	{name: "sendgrid-key", re: regexp.MustCompile(`\bSG\.[A-Za-z0-9_\-]{16,}\.[A-Za-z0-9_\-]{16,}`)},
	{name: "npm-token", re: regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`)},
	{name: "pypi-token", re: regexp.MustCompile(`\bpypi-AgEIcHlwaS5vcmc[A-Za-z0-9_\-]{50,}`)},
	{name: "huggingface-token", re: regexp.MustCompile(`\bhf_[A-Za-z0-9]{30,}\b`)},
	{name: "digitalocean-token", re: regexp.MustCompile(`\bdop_v1_[a-f0-9]{64}\b`)},
	{name: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`)},
	{name: "url-password", re: regexp.MustCompile(`\b([a-zA-Z][a-zA-Z0-9+.\-]{1,20}://[^\s:/@"'<>]{0,64}:)[^\s@/"'<>$%{}\[\]]{3,}@`), repl: "${1}[REDACTED:url-password]@"},
}

// Every rule above needs at least one of these literals to match, so text
// without any of them can skip the regular expressions entirely.
var redactTriggers = []string{
	"PRIVATE KEY", "AKIA", "ASIA", "ecret", "ECRET", "gh", "glpat-", "xox", "hooks.slack.com",
	"sk-", "AIza", "_live_", "SG.", "npm_", "pypi-", "hf_", "dop_v1_", "eyJ", "://",
}

var (
	pemBegin = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)
	pemEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)
	// A BEGIN marker opens a multi-line block only when nothing but closing
	// punctuation follows it on the line (so code that merely mentions the
	// marker, e.g. strings.HasPrefix(s, "-----BEGIN …-----"), is left alone).
	pemBeginTail = regexp.MustCompile(`^(?:\\n|\\r|["'` + "`" + `\s,+])*$`)
	pemBodyLine  = regexp.MustCompile(`^[+\- ]?\s*["'` + "`" + `]?(?:[A-Za-z0-9+/=]{16,}|Proc-Type:.*|DEK-Info:.*|Version:.*|Comment:.*)?(?:\\n|\\r)*["'` + "`" + `]?\s*[+,]?\s*$`)
	base64Line   = regexp.MustCompile(`^[+\- ]?\s*[A-Za-z0-9+/]{40,}={0,2}\s*$`)
)

const pemPlaceholder = "[REDACTED:private-key]"

// MayContain is a cheap prefilter: false means no rule can match data.
func (r *Redactor) MayContain(data string) bool {
	if !r.Enabled() {
		return false
	}
	for _, t := range redactTriggers {
		if strings.Contains(data, t) {
			return true
		}
	}
	return false
}

// Line applies the single-line rules and returns the masked line and the
// number of values masked.
func (r *Redactor) Line(s string) (string, int) {
	if !r.MayContain(s) {
		return s, 0
	}
	n := 0
	for _, rule := range redactRules {
		locs := rule.re.FindAllStringIndex(s, -1)
		if len(locs) == 0 {
			continue
		}
		n += len(locs)
		repl := rule.repl
		if repl == "" {
			repl = "[REDACTED:" + rule.name + "]"
		}
		s = rule.re.ReplaceAllString(s, repl)
	}
	return s, n
}

// RedactState masks a stream of consecutive lines, tracking multi-line
// private-key blocks. Feed it every line from the start of the content, in
// order, including lines that will not be displayed.
type RedactState struct {
	r     *Redactor
	inPEM bool
	count int
}

func (r *Redactor) NewState() *RedactState { return &RedactState{r: r} }

// Count is the number of values masked so far.
func (st *RedactState) Count() int { return st.count }

func (st *RedactState) Line(s string) string {
	if !st.r.Enabled() {
		return s
	}
	if st.inPEM {
		if pemEnd.MatchString(s) {
			st.inPEM = false
			return diffPrefix(s) + pemPlaceholder
		}
		if pemBodyLine.MatchString(s) {
			return diffPrefix(s) + pemPlaceholder
		}
		st.inPEM = false // not key material after all: stop masking
	}
	out, n := st.r.Line(s)
	st.count += n
	if loc := pemBegin.FindStringIndex(out); loc != nil && pemBeginTail.MatchString(out[loc[1]:]) {
		st.inPEM = true
		st.count++
		return out[:loc[0]] + pemPlaceholder
	}
	return out
}

func diffPrefix(s string) string {
	if s != "" && (s[0] == '+' || s[0] == '-' || s[0] == ' ') {
		return s[:1]
	}
	return ""
}

// Text masks a whole multi-line text. With hunks=true (diff/show/blame
// output, where a key body can appear without its BEGIN line) it additionally
// masks runs of two or more lines that consist only of base64 data.
func (r *Redactor) Text(s string, hunks bool) (string, int) {
	if !r.Enabled() || s == "" {
		return s, 0
	}
	lines := strings.Split(s, "\n")
	st := r.NewState()
	for i, l := range lines {
		lines[i] = st.Line(l)
	}
	count := st.Count()
	if hunks {
		for i := 0; i < len(lines); {
			j := i
			for j < len(lines) && base64Line.MatchString(lines[j]) {
				j++
			}
			if j-i >= 2 {
				for k := i; k < j; k++ {
					lines[k] = diffPrefix(lines[k]) + "[REDACTED:base64-block]"
				}
				count++
			}
			if j == i {
				j++
			}
			i = j
		}
	}
	return strings.Join(lines, "\n"), count
}

// Sanitize makes text safe to show to a model and to print on a terminal:
// invalid UTF-8 becomes U+FFFD and control characters other than newline and
// tab (including ESC, DEL and the C1 range) become visible \xNN escapes.
func Sanitize(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= utf8.RuneSelf || (c < 0x20 && c != '\n' && c != '\t') || c == 0x7f {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	const hex = "0123456789abcdef"
	var b strings.Builder
	b.Grow(len(s) + 16)
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == utf8.RuneError:
			b.WriteRune('\uFFFD')
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			b.WriteString(`\x`)
			b.WriteByte(hex[(r>>4)&0xf])
			b.WriteByte(hex[r&0xf])
		case r == '\u2028' || r == '\u2029' || r == '\u202e' || r == '\u2066' || r == '\u2067' || r == '\u2068' || r == '\u2069' || r == '\u202a' || r == '\u202b' || r == '\u202d' || r == '\u202c':
			b.WriteString(`\u`)
			for shift := 12; shift >= 0; shift -= 4 {
				b.WriteByte(hex[(r>>shift)&0xf])
			}
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// SanitizeLine is Sanitize for a single-line value: newlines and tabs are
// escaped too, so repository-derived text can never forge extra lines.
func SanitizeLine(s string) string {
	s = Sanitize(s)
	if strings.ContainsAny(s, "\n\t") {
		s = strings.NewReplacer("\n", `\n`, "\t", `\t`).Replace(s)
	}
	return s
}
