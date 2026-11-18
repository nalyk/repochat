package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	searchFileLimit   = 4 << 20
	searchTimeBudget  = 10 * time.Second
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
	skip       string // "", "binary", "oversized", "unreadable", "missing", "changed"
	redactions int
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

	re, literal, err := compileQuery(o)
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

	var files []string
	withheldInScope, notFiles := 0, 0
	for _, e := range snap.Entries {
		if len(include) > 0 && !matchAny(include, e.Path) {
			continue
		}
		if matchAny(exclude, e.Path) {
			continue
		}
		switch {
		case e.Withheld != "":
			withheldInScope++
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
				slots[i].res = r.scanFile(op, files[i], re, literal, o, from)
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
	for i := o.FromFile; i < len(files) && stop == ""; i++ {
		select {
		case <-slots[i].done:
		case <-ctx.Done():
			stop = "cancelled"
			continue
		case <-time.After(time.Until(deadline)):
			stop = "time budget"
			res.NextFile, res.NextLine, res.Resumable = i, 0, true
			continue
		}
		fs := slots[i].res
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
			fs = r.scanFile(op, files[i], re, literal, o2, o.fromLineFor(i))
			op.Close()
			if fs.skip != "" || fs.matches == 0 {
				break
			}
			block = renderFileHits(files[i], fs.lines)
		}
		if fs.skip != "" || fs.matches == 0 {
			continue // the file changed between the two scans
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
	if withheldInScope > 0 {
		details = append(details, fmt.Sprintf("%d withheld file(s) not searched", withheldInScope))
	}
	if notFiles > 0 {
		details = append(details, fmt.Sprintf("%d symlink/submodule entr(ies) not searched", notFiles))
	}
	if res.Redactions > 0 {
		details = append(details, fmt.Sprintf("%d secret-looking value(s) masked", res.Redactions))
	}
	if res.Complete {
		note := ""
		if skipped["binary"]+skipped["oversized"]+skipped["missing"]+skipped["unreadable"]+skipped["changed"]+withheldInScope > 0 {
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

func compileQuery(o SearchOptions) (*regexp.Regexp, []byte, error) {
	if !o.Regex && !o.WholeWord && o.CaseSensitive {
		return nil, []byte(o.Query), nil // plain substring search
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
		return nil, nil, fmt.Errorf("invalid regular expression (RE2 syntax, no look-around or backreferences): %v", err)
	}
	return re, nil, nil
}

// scanFile finds matches in one file. With redaction on, a file that may
// contain secret spans is masked first and the predicate runs on the masked
// text, so hidden bytes can never influence whether or where a match occurs.
func (r *Repo) scanFile(op *opener, rel string, re *regexp.Regexp, literal []byte, o SearchOptions, afterLine int) fileScan {
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
	masked := r.red.MayContain(text)
	if !masked {
		// Fast reject on the raw bytes: it is the disclosed representation.
		if literal != nil {
			if !bytes.Contains(data, literal) {
				return fileScan{}
			}
		} else if !re.Match(data) {
			return fileScan{}
		}
	}

	rawLines := strings.Split(text, "\n")
	if n := len(rawLines); n > 0 && rawLines[n-1] == "" {
		rawLines = rawLines[:n-1]
	}
	var fs fileScan
	lines := rawLines
	if masked {
		st := r.red.NewState()
		lines = make([]string, len(rawLines))
		for i, l := range rawLines {
			lines[i] = st.Line(strings.TrimSuffix(l, "\r"))
		}
	}

	matchLine := func(s string) (int, bool) {
		if literal != nil {
			i := strings.Index(s, string(literal))
			return i, i >= 0
		}
		loc := re.FindStringIndex(s)
		if loc == nil {
			return 0, false
		}
		return loc[0], true
	}

	var hits []int // 0-based line indexes
	for i, l := range lines {
		if i+1 <= afterLine {
			continue
		}
		if len(l) > maxLineBytes {
			l = l[:maxLineBytes]
		}
		if _, ok := matchLine(l); ok {
			if len(hits) >= o.MaxResults {
				fs.more = true
				break
			}
			hits = append(hits, i)
		}
	}
	fs.redactions = 0
	if len(hits) == 0 {
		return fs
	}
	fs.matches = len(hits)
	fs.lastMatch = hits[len(hits)-1] + 1

	shown := map[int]bool{}
	isHit := map[int]bool{}
	for _, h := range hits {
		isHit[h] = true
		for c := h - o.Context; c <= h+o.Context; c++ {
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
		l := strings.TrimSuffix(lines[i], "\r")
		at := 0
		if isHit[i] {
			at, _ = matchLine(l)
		}
		shownText := windowLine(l, at)
		fs.redactions += strings.Count(shownText, "[REDACTED:")
		fs.lines = append(fs.lines, searchLine{no: i + 1, text: shownText, match: isHit[i]})
	}
	return fs
}

// windowLine shortens a long line to a window around the match position.
func windowLine(s string, at int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= searchLineDisplay {
		return strings.TrimRight(s, " \t")
	}
	start := max(at-80, 0)
	if start > len(s) {
		start = 0
	}
	for start > 0 && !utf8.RuneStart(s[start]) {
		start--
	}
	end := min(start+searchLineDisplay, len(s))
	for end < len(s) && !utf8.RuneStart(s[end]) {
		end--
	}
	out := s[start:end]
	if start > 0 {
		out = "…" + out
	}
	if end < len(s) {
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
