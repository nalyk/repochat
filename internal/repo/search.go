package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"regexp/syntax"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// searchTimeBudget is how long one call may scan before it returns what it
// has with a cursor. A variable so a test can make the budget run out.
var searchTimeBudget = 10 * time.Second

const (
	searchFileLimit   = 4 << 20
	searchLineDisplay = 300
	DefaultSearchHits = 50
	MaxSearchHits     = 200
	MaxSearchContext  = 5
)

// SearchOptions selects what to look for and where.
type SearchOptions struct {
	Query         string
	Regex         bool
	CaseSensitive bool
	WholeWord     bool
	Include       []string // gitignore-style patterns; empty = everything
	Exclude       []string
	Context       int // lines of context around each match
	MaxResults    int // matching lines
	// Resume position from a previous incomplete search over the same
	// snapshot and options: index into the candidate file list and the last
	// line already reported in that file.
	FromFile int
	FromLine int
	// Fingerprint of the file at FromFile as reported by the previous call;
	// required when FromLine > 0 so a line position is never reused after the
	// file changed.
	FromFingerprint string
}

// SearchResult is the rendered outcome plus the facts needed to continue it.
type SearchResult struct {
	Text      string
	Matches   int
	Files     int // files with at least one match
	Complete  bool
	NextFile  int // resume position when !Complete and Resumable
	NextLine  int
	Resumable bool
	// NextFingerprint identifies the content state of the file at NextFile
	// when the search stopped inside it (NextLine > 0).
	NextFingerprint string
	Generation      string
	Redactions      int
}

type searchLine struct {
	no    int
	text  string
	match bool
}

type fileScan struct {
	lines      []searchLine // matches with context, in order
	matches    int
	lastMatch  int
	more       bool   // stopped early because the per-call hit limit was reached
	skip       string // "", "binary", "oversized", "unreadable", "missing", "changed", "cancelled"
	redactions int
	failed     bool // the scan panicked; the whole call is reported as an error
}

// Search scans the served working-tree files for the query.
func (r *Repo) Search(ctx context.Context, o SearchOptions, budget int) (*SearchResult, error) {
	if utf8.RuneCountInString(o.Query) < 1 {
		return nil, errors.New("query must not be empty")
	}
	if o.MaxResults < 1 {
		o.MaxResults = DefaultSearchHits
	}
	o.MaxResults = min(o.MaxResults, MaxSearchHits)
	o.Context = min(max(o.Context, 0), MaxSearchContext)

	m, err := compileQuery(o)
	if err != nil {
		return nil, err
	}
	include, err := CompileGlobs(o.Include)
	if err != nil {
		return nil, fmt.Errorf("include %w", err)
	}
	exclude, err := CompileGlobs(o.Exclude)
	if err != nil {
		return nil, fmt.Errorf("exclude %w", err)
	}
	snap, err := r.Snapshot(ctx)
	if err != nil {
		return nil, err
	}

	// A count of the withheld files that include/exclude select would let a
	// caller test patterns against withheld names and recover them one
	// character at a time. The number reported is therefore the repository's
	// total, whatever the filters are. Only when the owner lets the model see
	// withheld names anyway is the count the one for the selected scope.
	var files []string
	withheld, notFiles := snap.WithheldTotal(), 0
	if r.Opts.ShowWithheldNames {
		withheld = 0
	}
	for _, e := range snap.Entries {
		if e.Withheld != "" && !r.Opts.ShowWithheldNames {
			continue
		}
		if len(include) > 0 && !matchAny(include, e.Path) {
			continue
		}
		if matchAny(exclude, e.Path) {
			continue
		}
		switch {
		case e.Withheld != "":
			withheld++
		case e.Kind != KindRegular:
			notFiles++
		default:
			files = append(files, e.Path)
		}
	}
	if o.FromFile < 0 || o.FromFile > len(files) || o.FromLine < 0 {
		return nil, errors.New("cursor does not match this search; restart without cursor")
	}
	if o.FromLine > 0 {
		if o.FromFile >= len(files) || r.fingerprint(files[o.FromFile]) != o.FromFingerprint {
			return nil, errors.New("the file this search stopped in has changed since; restart the search without cursor")
		}
	}

	caller := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	deadline := time.Now().Add(searchTimeBudget)

	// Workers scan ahead in file order; results are consumed strictly in
	// order, so the output is deterministic and resumable.
	type slot struct {
		done chan struct{}
		res  fileScan
	}
	slots := make([]*slot, len(files))
	for i := o.FromFile; i < len(files); i++ {
		slots[i] = &slot{done: make(chan struct{})}
	}
	jobs := make(chan int)
	workers := min(max(runtime.NumCPU(), 2), 8)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op := r.newOpener()
			defer op.Close()
			for i := range jobs {
				from := 0
				if i == o.FromFile {
					from = o.FromLine
				}
				slots[i].res = r.scan(ctx, op, files[i], m, o, from)
				close(slots[i].done)
			}
		}()
	}
	go func() {
		defer close(jobs)
		for i := o.FromFile; i < len(files); i++ {
			select {
			case jobs <- i:
			case <-ctx.Done():
				return
			}
		}
	}()

	res := &SearchResult{Generation: snap.Generation, Complete: true}
	skipped := map[string]int{}
	var body strings.Builder
	scanned := 0
	stop := ""
	failed := ""
	for i := o.FromFile; i < len(files) && stop == ""; i++ {
		select {
		case <-slots[i].done:
		case <-ctx.Done():
			stop = "cancelled"
			continue
		case <-time.After(time.Until(deadline)):
			// Still inside the file this call resumed in: the line position
			// must survive, or the next call reports its matches again.
			stop = "time budget"
			res.NextFile, res.NextLine, res.Resumable = i, o.fromLineFor(i), true
			continue
		}
		fs := slots[i].res
		if fs.failed {
			stop, failed = "error", files[i]
			continue
		}
		scanned++
		if fs.skip != "" {
			skipped[fs.skip]++
			continue
		}
		if fs.matches == 0 {
			continue
		}
		// Enforce the hit limit and the size budget exactly: when this file
		// would overshoot either, re-scan it with a smaller per-file limit so
		// the result ends on a match boundary that can be resumed.
		block := renderFileHits(files[i], fs.lines)
		limit := o.MaxResults - res.Matches
		room := budget - 1024 - body.Len()
		if len(block) > room && res.Matches > 0 {
			stop = "size budget"
			res.NextFile, res.NextLine, res.Resumable = i, o.fromLineFor(i), true
			continue
		}
		for fs.matches > limit || (len(block) > room && limit > 1) {
			if len(block) > room {
				limit = max(1, min(limit, fs.matches)/2)
			}
			o2 := o
			o2.MaxResults = limit
			op := r.newOpener()
			fs = r.scan(ctx, op, files[i], m, o2, o.fromLineFor(i))
			op.Close()
			if fs.failed || fs.skip != "" || fs.matches == 0 {
				break
			}
			block = renderFileHits(files[i], fs.lines)
		}
		// The file changed between the two scans. One that can no longer be
		// read was not searched, and the result has to say so.
		if fs.failed {
			stop, failed = "error", files[i]
			continue
		}
		if fs.skip != "" {
			skipped[fs.skip]++
			continue
		}
		if fs.matches == 0 {
			continue
		}
		body.WriteString(block)
		res.Matches += fs.matches
		res.Files++
		res.Redactions += fs.redactions
		switch {
		case fs.more && len(block) > room:
			stop = "size budget"
			res.NextFile, res.NextLine, res.Resumable = i, fs.lastMatch, true
		case fs.more:
			stop = "result limit"
			res.NextFile, res.NextLine, res.Resumable = i, fs.lastMatch, true
		case res.Matches >= o.MaxResults && i+1 < len(files):
			stop = "result limit"
			res.NextFile, res.NextLine, res.Resumable = i+1, 0, true
		}
	}
	cancel()
	wg.Wait()
	if failed != "" {
		return nil, fmt.Errorf("internal error while searching %s; the search was abandoned (the details are in repochat's local log on the user's machine)", SanitizeLine(failed))
	}
	// A caller that gave up gets its own error: files skipped because of the
	// cancellation must never be presented as a finished search.
	if err := caller.Err(); err != nil {
		return nil, err
	}
	if stop != "" {
		res.Complete = false
	}
	if res.Resumable && res.NextLine > 0 && res.NextFile < len(files) {
		res.NextFingerprint = r.fingerprint(files[res.NextFile])
	}

	mode := "literal"
	if o.Regex {
		mode = "regex"
	}
	if o.WholeWord {
		mode += ", whole word"
	}
	if o.CaseSensitive {
		mode += ", case-sensitive"
	} else {
		mode += ", case-insensitive"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "SEARCH %q (%s): %d matching line(s) in %d file(s)\n", o.Query, mode, res.Matches, res.Files)
	b.WriteString(body.String())

	var details []string
	details = append(details, fmt.Sprintf("scanned %d of %d candidate file(s)", scanned+o.FromFile, len(files)))
	if n := skipped["binary"]; n > 0 {
		details = append(details, fmt.Sprintf("%d binary skipped", n))
	}
	if n := skipped["oversized"]; n > 0 {
		details = append(details, fmt.Sprintf("%d larger than %s skipped", n, humanSize(searchFileLimit)))
	}
	if n := skipped["missing"]; n > 0 {
		details = append(details, fmt.Sprintf("%d not present in the working tree", n))
	}
	if n := skipped["unreadable"] + skipped["changed"]; n > 0 {
		details = append(details, fmt.Sprintf("%d unreadable", n))
	}
	switch {
	case withheld > 0 && r.Opts.ShowWithheldNames:
		details = append(details, fmt.Sprintf("%d withheld file(s) not searched", withheld))
	case withheld > 0:
		details = append(details, fmt.Sprintf("%d withheld file(s) not searched (the repository's total, whatever include/exclude select)", withheld))
	}
	if notFiles > 0 {
		details = append(details, fmt.Sprintf("%d symlink/submodule entr(ies) not searched", notFiles))
	}
	if res.Redactions > 0 {
		details = append(details, fmt.Sprintf("%d secret-looking value(s) masked", res.Redactions))
	}
	if res.Complete {
		note := ""
		if skipped["binary"]+skipped["oversized"]+skipped["missing"]+skipped["unreadable"]+skipped["changed"]+withheld > 0 {
			note = " (of the files that could be searched)"
		}
		fmt.Fprintf(&b, "[complete%s for file set %s (file contents are read live): %s]\n", note, snap.Generation, strings.Join(details, "; "))
		if res.Matches == 0 {
			b.WriteString("[no match. Ignored (.gitignore) files are not part of the repository view; try a shorter query, regex=true, or a different include pattern]\n")
		}
	} else {
		fmt.Fprintf(&b, "[INCOMPLETE — stopped by %s: %s. More matches may exist", stop, strings.Join(details, "; "))
		if res.Resumable {
			b.WriteString("; pass the cursor from @meta to continue]\n")
		} else {
			b.WriteString("]\n")
		}
	}
	res.Text = b.String()
	return res, nil
}

// fingerprint identifies the current content state of a working-tree file
// (size and modification time); "" when it cannot be examined.
func (r *Repo) fingerprint(rel string) string {
	op := r.newOpener()
	defer op.Close()
	fi, err := op.Lstat(rel)
	if err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	return fmt.Sprintf("%d-%d", fi.Size(), fi.ModTime().UnixNano())
}

func (o SearchOptions) fromLineFor(i int) int {
	if i == o.FromFile {
		return o.FromLine
	}
	return 0
}

// matcher is the compiled query. One matcher decides whether a line matches
// and where its excerpt goes, always on the line in the form it is shown in.
type matcher struct {
	re  *regexp.Regexp // nil: plain substring search for lit
	lit string
	// wholeFile says that a match on some line implies a match on the file
	// taken as one text, so a file without any match can be rejected before
	// it is split into lines. It does not hold for a pattern with ^, $, \A
	// or \z: those mean the edges of a line here, not of the file.
	wholeFile bool
}

func compileQuery(o SearchOptions) (*matcher, error) {
	if !o.Regex && !o.WholeWord && o.CaseSensitive {
		return &matcher{lit: o.Query, wholeFile: true}, nil // plain substring search
	}
	pattern := o.Query
	if !o.Regex {
		pattern = regexp.QuoteMeta(pattern)
	}
	if o.WholeWord {
		pattern = `\b(?:` + pattern + `)\b`
	}
	if !o.CaseSensitive {
		pattern = `(?i)` + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regular expression (RE2 syntax, no look-around or backreferences): %v", err)
	}
	return &matcher{re: re, wholeFile: !anchored(pattern)}, nil
}

// anchored reports whether a pattern refers to the start or the end of the
// text it is matched against. When in doubt it says yes: the caller then
// takes the slower path that looks at every line.
func anchored(pattern string) bool {
	tree, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return true
	}
	var walk func(*syntax.Regexp) bool
	walk = func(re *syntax.Regexp) bool {
		switch re.Op {
		case syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText, syntax.OpEndText:
			return true
		}
		for _, sub := range re.Sub {
			if walk(sub) {
				return true
			}
		}
		return false
	}
	return walk(tree)
}

// find returns the first match in s as byte offsets into s.
func (m *matcher) find(s string) (start, end int, ok bool) {
	if m.re == nil {
		i := strings.Index(s, m.lit)
		return i, i + len(m.lit), i >= 0
	}
	loc := m.re.FindStringIndex(s)
	if loc == nil {
		return 0, 0, false
	}
	return loc[0], loc[1], true
}

// inText reports whether the query matches anywhere in data taken as one text.
func (m *matcher) inText(data []byte) bool {
	if m.re == nil {
		return bytes.Contains(data, []byte(m.lit))
	}
	return m.re.Match(data)
}

// scan is scanFile made safe to run in a goroutine of its own: a panic there
// would end the whole server, so it is turned into a failed result (and the
// call into an error). What exactly went wrong stays in the local log.
func (r *Repo) scan(ctx context.Context, op *opener, rel string, m *matcher, o SearchOptions, afterLine int) (fs fileScan) {
	defer func() {
		if p := recover(); p != nil {
			if r.Logf != nil {
				r.Logf("panic while searching %s: %v\n%s", SanitizeLine(rel), p, debug.Stack())
			}
			fs = fileScan{failed: true}
		}
	}()
	return r.scanFile(ctx, op, rel, m, o, afterLine)
}

// scanFile finds matches in one file. Every line is first brought into the
// form read_file shows (line terminator removed, invalid UTF-8 replaced,
// secret-looking values masked) and only that form is used from then on: the
// predicate runs on it, so hidden bytes can never influence whether or where
// a match occurs, and the excerpt is cut from it, so match offsets always
// refer to the text they are applied to.
func (r *Repo) scanFile(ctx context.Context, op *opener, rel string, m *matcher, o SearchOptions, afterLine int) fileScan {
	if ctx.Err() != nil {
		return fileScan{skip: "cancelled"}
	}
	data, _, truncated, err := op.ReadAll(rel, searchFileLimit)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fileScan{skip: "missing"}
	case errors.Is(err, errChanged):
		return fileScan{skip: "changed"}
	case err != nil:
		return fileScan{skip: "unreadable"}
	case truncated:
		return fileScan{skip: "oversized"}
	}
	if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
		return fileScan{skip: "binary"}
	}
	text := string(data)
	valid := utf8.Valid(data)
	masked := r.red.MayContain(text)
	// Fast reject on the raw bytes. Sound only when they are what the lines
	// will show (nothing to mask, nothing to replace) and the pattern cannot
	// tell a line from the whole file.
	if !masked && valid && m.wholeFile && !m.inText(data) {
		return fileScan{}
	}

	lines := strings.Split(text, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	st := r.red.NewState()
	masks := map[int]int{} // line index → values masked on that line
	for i, l := range lines {
		if i&1023 == 0 && ctx.Err() != nil {
			return fileScan{skip: "cancelled"}
		}
		l = strings.TrimSuffix(l, "\r")
		if !valid {
			l = strings.ToValidUTF8(l, string(utf8.RuneError))
		}
		if masked {
			var n int
			if l, n = st.Masked(l); n > 0 {
				masks[i] = n
			}
		}
		lines[i] = l
	}

	// Lines are searched whole, however long: a line cut short would hide
	// matches past the cut and make patterns such as `word$` match at it.
	type hit struct{ line, start, end int }
	var fs fileScan
	var hits []hit
	for i, l := range lines {
		if i+1 <= afterLine {
			continue
		}
		if i&1023 == 0 && ctx.Err() != nil {
			return fileScan{skip: "cancelled"}
		}
		start, end, ok := m.find(l)
		if !ok {
			continue
		}
		if len(hits) >= o.MaxResults {
			fs.more = true
			break
		}
		hits = append(hits, hit{i, start, end})
	}
	if len(hits) == 0 {
		return fs
	}
	fs.matches = len(hits)
	fs.lastMatch = hits[len(hits)-1].line + 1

	shown := map[int]bool{}
	found := make(map[int]hit, len(hits))
	for _, h := range hits {
		found[h.line] = h
		for c := h.line - o.Context; c <= h.line+o.Context; c++ {
			if c >= 0 && c < len(lines) {
				shown[c] = true
			}
		}
	}
	order := make([]int, 0, len(shown))
	for i := range shown {
		order = append(order, i)
	}
	sort.Ints(order)
	for _, i := range order {
		h, isHit := found[i] // the zero hit puts a context line's excerpt at its start
		text := windowLine(lines[i], h.start, h.end)
		fs.lines = append(fs.lines, searchLine{no: i + 1, text: text, match: isHit})
		// Counted from what the redactor did to this line, not by looking
		// for "[REDACTED:" alone: ordinary source text can spell that out.
		fs.redactions += shownMasks(text, masks[i])
	}
	return fs
}

// windowLine shortens a long line to an excerpt that begins shortly before
// the match s[start:end] and therefore contains it (as much of it as fits).
// s is the line in the form the match was found in, valid UTF-8. The excerpt
// never begins or ends inside a mask marker.
func windowLine(s string, start, end int) string {
	start = min(max(start, 0), len(s))
	end = min(max(end, start), len(s))
	if len(s) <= searchLineDisplay {
		// Trailing blanks are dropped, unless the match reaches into them.
		return s[:max(end, len(strings.TrimRight(s, " \t")))]
	}
	from := max(start-80, 0)
	for from > 0 && !utf8.RuneStart(s[from]) {
		from--
	}
	if a, _, ok := markerAround(s, from); ok {
		from = a
	}
	to := min(from+searchLineDisplay, len(s))
	for to < len(s) && !utf8.RuneStart(s[to]) {
		to--
	}
	if _, b, ok := markerAround(s, to); ok {
		to = b
	}
	out := s[from:to]
	if from > 0 {
		out = "…" + out
	}
	if to < len(s) {
		out += "…"
	}
	return out
}

func renderFileHits(rel string, lines []searchLine) string {
	var b strings.Builder
	b.WriteString(rel)
	b.WriteString("\n")
	prev := 0
	for _, l := range lines {
		if prev != 0 && l.no > prev+1 {
			b.WriteString("  ⋯\n")
		}
		sep := "-"
		if l.match {
			sep = ":"
		}
		fmt.Fprintf(&b, "  %d%s %s\n", l.no, sep, Sanitize(l.text))
		prev = l.no
	}
	return b.String()
}
