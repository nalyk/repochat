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
	{name: "slack-webhook", re: regexp.MustCompile(`(?:https://)?hooks\.slack\.com/services/[A-Za-z0-9/_\-]{20,}`)},
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
// without any of them can skip the regular expressions entirely. A rule whose
// literal is missing here is not applied at all unless another rule's literal
// happens to be near: "github" does not contain "gh", so fine-grained tokens
// need their own entry. TestEveryMaskingRuleFiresOnItsOwn holds each rule to
// this list.
var redactTriggers = []string{
	"PRIVATE KEY", "AKIA", "ASIA", "gh", "github_pat_", "glpat-", "xox", "hooks.slack.com",
	"sk-", "AIza", "_live_", "SG.", "npm_", "pypi-", "hf_", "dop_v1_", "eyJ", "://",
}

var (
	pemBegin = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)
	pemEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)
	// A BEGIN marker opens a multi-line block only when nothing but closing
	// punctuation follows it on the line (so code that merely mentions the
	// marker, e.g. strings.HasPrefix(s, "-----BEGIN …-----"), is left alone).
	pemBeginTail = regexp.MustCompile(`^(?:\\n|\\r|["'` + "`" + `\s,+])*$`)
	// A line inside a key block: base64 data, or one of the headers a block
	// may carry (RFC 1421 for PEM, RFC 4880 for OpenPGP armor). The headers
	// are named one by one: any other line ends the masking, so text that
	// merely follows a marker is not swallowed.
	pemBodyLine = regexp.MustCompile(`^[+\- ]?\s*["'` + "`" + `]?(?:[A-Za-z0-9+/=]+|(?:Proc-Type|DEK-Info|Version|Comment|MessageID|Hash|Charset):.*)?(?:\\n|\\r)*["'` + "`" + `]?\s*[+,]?\s*$`)
	base64Line  = regexp.MustCompile(`^[+\- ]?\s*[A-Za-z0-9+/]{40,}={0,2}\s*$`)
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
	return containsFold(data, "secret") // the one case-insensitive rule
}

// containsFold reports whether s contains the lower-case ASCII word,
// ignoring case, without allocating.
func containsFold(s, word string) bool {
	lo, up := word[0], word[0]-'a'+'A'
	for i := 0; i+len(word) <= len(s); i++ {
		if s[i] != lo && s[i] != up {
			continue
		}
		if strings.EqualFold(s[i:i+len(word)], word) {
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

// Force sets the key-block state from evidence the caller found in text it
// did not pass to Line (the cut-off tail of a very long line): open > 0 means
// a key block begins there, open < 0 that one ends there.
func (st *RedactState) Force(open int) {
	if !st.r.Enabled() || open == 0 {
		return
	}
	if open > 0 && !st.inPEM {
		st.count++
	}
	st.inPEM = open > 0
}

// keyEdge looks for private-key markers in b: +1 when the last marker opens a
// block, -1 when it closes one, 0 when there is none.
func keyEdge(b []byte) int {
	begin, end := -1, -1
	if locs := pemBegin.FindAllIndex(b, -1); len(locs) > 0 {
		begin = locs[len(locs)-1][0]
	}
	if locs := pemEnd.FindAllIndex(b, -1); len(locs) > 0 {
		end = locs[len(locs)-1][0]
	}
	switch {
	case begin < 0 && end < 0:
		return 0
	case begin > end:
		return 1
	default:
		return -1
	}
}

func (st *RedactState) Line(s string) string {
	out, _ := st.Masked(s)
	return out
}

// Masked is Line that also reports how many masks the returned line carries
// (a line inside a key block carries one). Callers that show only some lines
// count from this instead of looking for the marker's spelling in their
// output, which ordinary source text can contain.
func (st *RedactState) Masked(s string) (string, int) {
	if !st.r.Enabled() {
		return s, 0
	}
	if st.inPEM {
		if pemEnd.MatchString(s) {
			st.inPEM = false
			return diffPrefix(s) + pemPlaceholder, 1
		}
		if pemBodyLine.MatchString(s) {
			return diffPrefix(s) + pemPlaceholder, 1
		}
		// The markers git writes into an unresolved merge conflict are not
		// key material, but they do not end the block either: both sides of
		// the conflict are still the key.
		if keyConflictMarker.MatchString(s) {
			return s, 0
		}
		st.inPEM = false // not key material after all: stop masking
	}
	out, n := st.r.Line(s)
	st.count += n
	if loc := pemBegin.FindStringIndex(out); loc != nil && pemBeginTail.MatchString(out[loc[1]:]) {
		st.inPEM = true
		st.count++
		return out[:loc[0]] + pemPlaceholder, n + 1
	}
	return out, n
}

// keyConflictMarker matches a line git inserts around the sides of a merge
// conflict, optionally behind a diff or blame prefix.
var keyConflictMarker = regexp.MustCompile(`^[ +\-]?(?:<{7}|={7}|>{7}|\|{7})(?: .*)?\r?$`)

// maskMarker matches a marker as the rules above write it.
var maskMarker = regexp.MustCompile(`\[REDACTED:[a-z0-9-]{1,40}\]`)

// maskMarkerMax bounds the length of a marker, so looking for one around a
// position never has to examine the whole line.
const maskMarkerMax = len("[REDACTED:]") + 40

// shownMasks is the number of masked values visible in an excerpt of a line
// on which the redactor masked n values. The redactor's count is the upper
// limit, so text that merely spells a marker out is never counted on its
// own; the excerpt lowers it when it leaves a marker out.
func shownMasks(excerpt string, n int) int {
	if n == 0 {
		return 0
	}
	return min(n, len(maskMarker.FindAllStringIndex(excerpt, -1)))
}

// cutPoint returns where to cut already masked text to at most limit bytes:
// on a character boundary and never inside a mask marker. Text must be
// masked before it is shortened, never after: a secret that loses its end to
// the cut is no longer recognised and would be shown in part.
func cutPoint(s string, limit int) int {
	if limit >= len(s) {
		return len(s)
	}
	cut := max(limit, 0)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if start, _, ok := markerAround(s, cut); ok {
		cut = start
	}
	return cut
}

// markerAround returns the bounds of the mask marker that pos falls strictly
// inside, so text is never cut through a marker (half a marker reads like
// leftover secret text). ok is false when pos is not inside one.
func markerAround(s string, pos int) (start, end int, ok bool) {
	if pos <= 0 || pos >= len(s) {
		return 0, 0, false
	}
	lo, hi := max(0, pos-maskMarkerMax), min(len(s), pos+maskMarkerMax)
	for _, loc := range maskMarker.FindAllStringIndex(s[lo:hi], -1) {
		if lo+loc[0] < pos && pos < lo+loc[1] {
			return lo + loc[0], lo + loc[1], true
		}
	}
	return 0, 0, false
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

// invisibleControl reports the characters beyond the C0/C1 controls that
// Sanitize shows as an escape: line and paragraph separators, the
// bidirectional embedding, override and isolate controls (they reorder what a
// reader sees), and the Unicode tag characters U+E0000–U+E007F (invisible, and
// a known way to carry a hidden copy of ASCII text to a model). Zero-width
// joiners and variation selectors are left alone: ordinary text needs them.
func invisibleControl(r rune) bool {
	switch {
	case r == 0x2028 || r == 0x2029: // line and paragraph separator
		return true
	case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069: // embeddings, overrides, isolates
		return true
	case r >= 0xE0000 && r <= 0xE007F: // tags
		return true
	}
	return false
}

// Sanitize makes text safe to show to a model and to print on a terminal:
// invalid UTF-8 becomes U+FFFD, control characters other than newline and
// tab (including ESC, DEL and the C1 range) become visible \xNN escapes, and
// the characters of invisibleControl become \uNNNN or \UNNNNNNNN escapes.
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
		case invisibleControl(r):
			shift := 12
			if r > 0xFFFF {
				b.WriteString(`\U`)
				shift = 28
			} else {
				b.WriteString(`\u`)
			}
			for ; shift >= 0; shift -= 4 {
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
