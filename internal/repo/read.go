package repo

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	maxLineBytes     = 64 << 10 // bytes of one line considered at all
	maxLineDisplay   = 2000     // bytes of one line shown
	maxScanBytes     = 256 << 20
	maxRevBlobBytes  = 32 << 20
	maxUTF16Bytes    = 16 << 20
	DefaultReadLines = 500
	MaxReadLines     = 2000
)

// ReadResult describes one rendered slice of a file.
type ReadResult struct {
	Path       string
	Rev        string // full commit SHA when read from history, else ""
	Text       string // header + numbered lines + notes
	First      int    // first line shown (0 when nothing was shown)
	Last       int    // last line shown
	Total      int    // total lines (valid when TotalKnown)
	TotalKnown bool
	NextStart  int // 0 when the end of the file was reached
	Size       int64
	Binary     bool
	Redactions int
}

// ReadFile renders lines [start, start+maxLines) of a working-tree file, or
// of the file as it was at rev when rev is not empty. budget bounds the size
// of the rendered text.
func (r *Repo) ReadFile(ctx context.Context, p string, start, maxLines int, rev string, budget int) (*ReadResult, error) {
	if start < 1 {
		start = 1
	}
	if maxLines < 1 {
		maxLines = DefaultReadLines
	}
	maxLines = min(maxLines, MaxReadLines)

	if rev != "" {
		return r.readAtRev(ctx, p, start, maxLines, rev, budget)
	}
	rel, entry, _, err := r.authorizeFile(ctx, p)
	if err != nil {
		return nil, err
	}
	switch entry.Kind {
	case KindSymlink:
		return nil, fmt.Errorf("%s is a symbolic link; repochat never follows symlinks. Read the target file by its own path", rel)
	case KindSubmodule:
		return nil, fmt.Errorf("%s is a Git submodule; its files are not part of this repository. Run repochat inside the submodule to read it", rel)
	}
	op := r.newOpener()
	defer op.Close()
	f, fi, err := op.Open(rel)
	if err != nil {
		return nil, describeErr(rel, err)
	}
	defer f.Close()
	state := "tracked"
	if !entry.Tracked {
		state = "untracked"
	}
	if entry.Conflict {
		state = "tracked, has merge conflicts"
	}
	res := &ReadResult{Path: rel, Size: fi.Size()}
	r.render(res, f, start, maxLines, budget, state)
	return res, nil
}

func (r *Repo) readAtRev(ctx context.Context, p string, start, maxLines int, rev string, budget int) (*ReadResult, error) {
	rel, err := CheckPath(p)
	if err != nil {
		return nil, err
	}
	sha, err := r.ResolveCommit(ctx, rev)
	if err != nil {
		return nil, err
	}
	pol, err := r.Policy(ctx)
	if err != nil {
		return nil, err
	}
	if reason := pol.Check(rel); reason != "" {
		return nil, withheldError(rel, reason)
	}
	data, err := r.blobAt(ctx, sha, rel)
	if err != nil {
		return nil, err
	}
	res := &ReadResult{Path: rel, Rev: sha, Size: int64(len(data))}
	r.render(res, bytes.NewReader(data), start, maxLines, budget, "at "+sha[:10])
	return res, nil
}

// blobAt returns the content of path at commit sha (size-capped).
func (r *Repo) blobAt(ctx context.Context, sha, rel string) ([]byte, error) {
	object := sha + ":" + rel
	kind, code, err := r.gitText(ctx, "cat-file", "-t", object)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("%s does not exist at revision %s", rel, sha[:10])
	}
	switch kind {
	case "blob":
	case "tree":
		return nil, fmt.Errorf("%s is a directory at revision %s", rel, sha[:10])
	case "commit":
		return nil, fmt.Errorf("%s is a submodule at revision %s", rel, sha[:10])
	default:
		return nil, fmt.Errorf("%s is not a file at revision %s", rel, sha[:10])
	}
	sizeText, code, err := r.gitText(ctx, "cat-file", "-s", object)
	if err != nil || code != 0 {
		return nil, fmt.Errorf("cannot read %s at %s (the object may be missing from a shallow or partial clone)", rel, sha[:10])
	}
	if size, _ := strconv.ParseInt(sizeText, 10, 64); size > maxRevBlobBytes {
		return nil, fmt.Errorf("%s at %s is %s; files larger than %s cannot be read from history", rel, sha[:10], humanSize(size), humanSize(maxRevBlobBytes))
	}
	data, err := r.git(ctx, maxRevBlobBytes+1, "cat-file", "blob", object)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s at %s (the object may be missing from a shallow or partial clone)", rel, sha[:10])
	}
	return data, nil
}

// render fills res from the content in src.
func (r *Repo) render(res *ReadResult, src io.Reader, start, maxLines, budget int, state string) {
	br := bufio.NewReaderSize(src, 64<<10)
	var notes []string

	head, _ := br.Peek(8000)
	switch {
	case hasUTF16BOM(head):
		raw, _ := io.ReadAll(io.LimitReader(br, maxUTF16Bytes+1))
		if len(raw) > maxUTF16Bytes {
			res.Binary = true
			res.Text = fmt.Sprintf("FILE %s · %s · UTF-16 text larger than %s: not shown\n", res.Path, humanSize(res.Size), humanSize(maxUTF16Bytes))
			return
		}
		br = bufio.NewReaderSize(strings.NewReader(decodeUTF16(raw)), 64<<10)
		notes = append(notes, "decoded from UTF-16")
	case bytes.IndexByte(head, 0) >= 0:
		res.Binary = true
		res.Text = fmt.Sprintf("FILE %s · %s · binary file: content not shown\n", res.Path, humanSize(res.Size))
		return
	case bytes.HasPrefix(head, []byte("version https://git-lfs.github.com/spec/")) && res.Size < 1024:
		notes = append(notes, "this is a Git LFS pointer; the real content is stored outside the repository and is not available")
	}

	st := r.red.NewState()
	var body strings.Builder
	lineNo, scanned := 0, int64(0)
	invalidUTF8, longLines, budgetHit, scanCapped := false, 0, false, false
	width := len(strconv.Itoa(start + maxLines))

	for {
		line, n, long, key, err := readLongLine(br)
		if n == 0 && err != nil {
			break
		}
		scanned += int64(n)
		lineNo++
		// Redaction state must see every line, shown or not.
		text := strings.TrimSuffix(string(line), "\r")
		if !utf8.ValidString(text) {
			text = strings.ToValidUTF8(text, "\uFFFD")
			invalidUTF8 = true
		}
		text = st.Line(text)
		st.Force(key)

		if lineNo >= start && lineNo < start+maxLines && !budgetHit {
			if long || len(text) > maxLineDisplay {
				longLines++
			}
			shown := truncateLine(text, long)
			entry := fmt.Sprintf("%*d| %s\n", width, lineNo, Sanitize(shown))
			if body.Len()+len(entry) > budget-512 && res.First != 0 {
				budgetHit = true
				res.NextStart = lineNo
			} else {
				if res.First == 0 {
					res.First = lineNo
				}
				res.Last = lineNo
				body.WriteString(entry)
			}
		}
		if err != nil {
			break
		}
		if scanned > maxScanBytes {
			scanCapped = true
			break
		}
	}
	res.Total, res.TotalKnown = lineNo, !scanCapped
	res.Redactions = st.Count()
	if res.NextStart == 0 && res.Last != 0 && (res.Last < res.Total || scanCapped) {
		res.NextStart = res.Last + 1
	}

	total := strconv.Itoa(res.Total)
	if !res.TotalKnown {
		total = "more than " + total
	}
	var b strings.Builder
	switch {
	case res.Total == 0:
		fmt.Fprintf(&b, "FILE %s · empty file · %s\n", res.Path, state)
	case res.First == 0:
		fmt.Fprintf(&b, "FILE %s · %s lines · %s · %s\n[start_line %d is past the end of the file]\n", res.Path, total, humanSize(res.Size), state, start)
	default:
		fmt.Fprintf(&b, "FILE %s · lines %d-%d of %s · %s · %s\n", res.Path, res.First, res.Last, total, humanSize(res.Size), state)
		b.WriteString(body.String())
	}
	if res.NextStart > 0 {
		fmt.Fprintf(&b, "[more lines: call read_file again with start_line=%d]\n", res.NextStart)
	}
	if longLines > 0 {
		notes = append(notes, fmt.Sprintf("%d very long line(s) shortened to %d bytes", longLines, maxLineDisplay))
	}
	if invalidUTF8 {
		notes = append(notes, "bytes that are not valid UTF-8 are shown as \uFFFD")
	}
	if res.Redactions > 0 {
		notes = append(notes, fmt.Sprintf("%d secret-looking value(s) masked as [REDACTED:…]", res.Redactions))
	}
	for _, n := range notes {
		fmt.Fprintf(&b, "[note: %s]\n", n)
	}
	res.Text = b.String()
}

// readLongLine reads one line of any length, keeping at most maxLineBytes of
// it. n is the number of bytes consumed; long reports that the line was cut.
// key reports private-key markers found anywhere in a cut line, including
// the part that is not returned (see keyEdge), so masking cannot be defeated
// by pushing a marker past the cut.
func readLongLine(br *bufio.Reader) (line []byte, n int, long bool, key int, err error) {
	var carry []byte // tail of the previous chunk, for markers split across chunks
	for {
		chunk, e := br.ReadSlice('\n')
		n += len(chunk)
		if room := maxLineBytes - len(line); room > 0 {
			if len(chunk) > room {
				long = true
				line = append(line, chunk[:room]...)
			} else {
				line = append(line, chunk...)
			}
		} else if len(chunk) > 0 {
			long = true
		}
		if long {
			window := append(append([]byte(nil), carry...), chunk...)
			if k := keyEdge(window); k != 0 {
				key = k
			}
			carry = append(carry[:0], window[max(0, len(window)-96):]...)
		}
		if errors.Is(e, bufio.ErrBufferFull) {
			continue
		}
		line = bytes.TrimSuffix(line, []byte("\n"))
		return line, n, long, key, e
	}
}

func truncateLine(s string, alreadyCut bool) string {
	if len(s) <= maxLineDisplay {
		if alreadyCut {
			return s + " …[line continues]"
		}
		return s
	}
	cut := maxLineDisplay
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf(" …[+%d bytes]", len(s)-cut)
}

func hasUTF16BOM(b []byte) bool {
	return len(b) >= 2 && ((b[0] == 0xFF && b[1] == 0xFE) || (b[0] == 0xFE && b[1] == 0xFF))
}

func decodeUTF16(b []byte) string {
	little := b[0] == 0xFF
	b = b[2:]
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if little {
			u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
		} else {
			u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
		}
	}
	return string(utf16.Decode(u))
}

func humanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	}
}
