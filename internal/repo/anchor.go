package repo

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"unicode/utf8"
)

// Anchors are keyed hashes of a line exactly as the model is shown it: after
// masking, sanitising and UTF-8 repair, before any shortening for display.
// They therefore say "this is the line you saw" and nothing about a value
// that was masked: two secrets that mask to the same text give the same
// anchor.

const anchorAlphabet = "abcdefghijklmnopqrstuvwxyz234567"

var anchorEncoding = base32.NewEncoding(anchorAlphabet).WithPadding(base32.NoPadding)

const (
	anchorChars  = 3 // 15 bits
	fileTagChars = 8 // 40 bits
)

// anchorOf returns the first 15 bits of HMAC-SHA256(key, shown) as three
// characters of anchorAlphabet.
func anchorOf(key []byte, shown string) string {
	m := hmac.New(sha256.New, key)
	io.WriteString(m, shown)
	return anchorEncoding.EncodeToString(m.Sum(nil))[:anchorChars]
}

// fileTagger computes a file tag one line at a time, so render can tag a
// file while it streams it.
type fileTagger struct{ h hash.Hash }

func newFileTagger(key []byte) *fileTagger {
	h := hmac.New(sha256.New, key)
	io.WriteString(h, "repochat file tag\x00")
	return &fileTagger{h: h}
}

// add takes the next line's shown text. Each line is length-prefixed, so
// moving text across a line boundary changes the tag.
func (f *fileTagger) add(text string) {
	var n [binary.MaxVarintLen64]byte
	f.h.Write(n[:binary.PutUvarint(n[:], uint64(len(text)))])
	io.WriteString(f.h, text)
}

func (f *fileTagger) sum() string {
	return anchorEncoding.EncodeToString(f.h.Sum(nil))[:fileTagChars]
}

// fileTagOf returns 8 characters of anchorAlphabet that identify all the
// shown lines of a file, in order.
func fileTagOf(key []byte, lines []shownLine) string {
	f := newFileTagger(key)
	for _, l := range lines {
		f.add(l.Text)
	}
	return f.sum()
}

// shownLine is one line of a file as the model is shown it.
type shownLine struct {
	Text   string // masked, sanitised, UTF-8 repaired; not shortened for display
	Locked bool   // the redactor masked something on this line (or in its unread tail)
	Long   bool   // longer than the display limit: shown cut
}

// maskedLine is one line after the per-line steps every reader of a file
// shares: line terminator removed, invalid UTF-8 replaced, masked with the
// state carried from the lines before it.
type maskedLine struct {
	text    string // masked; not sanitised, not cut
	cut     bool   // longer than maxLineBytes: the rest was not read
	invalid bool   // invalid UTF-8 was replaced
	locked  bool
}

// maskLine is the one function that turns a raw line into what is shown.
// key is the private-key evidence readLongLine found in the unread part of
// a cut line.
func maskLine(st *RedactState, raw []byte, cut bool, key int) maskedLine {
	text := strings.TrimSuffix(string(raw), "\r")
	ml := maskedLine{cut: cut}
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "�")
		ml.invalid = true
	}
	text, masks := st.Masked(text)
	st.Force(key)
	ml.text = text
	// A key marker in the unread tail is a value the model was not shown.
	ml.locked = masks > 0 || (key != 0 && st.r.Enabled())
	return ml
}

// long reports that the line is shown cut.
func (ml maskedLine) long() bool { return ml.cut || len(ml.text) > maxLineDisplay }

func (ml maskedLine) shown() shownLine {
	return shownLine{Text: Sanitize(ml.text), Locked: ml.locked, Long: ml.long()}
}

// lineSource yields the lines of a content stream through maskLine.
type lineSource struct {
	br *bufio.Reader
	st *RedactState
}

// next returns the next line and the number of bytes it took. n == 0 with an
// error means there are no more lines.
func (s *lineSource) next(ctx context.Context) (ml maskedLine, n int, err error) {
	line, n, cut, key, err := readLongLine(ctx, s.br)
	if n == 0 && err != nil {
		return maskedLine{}, 0, err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return maskedLine{}, n, err
	}
	return maskLine(s.st, line, cut, key), n, err
}

// textKind says how the start of a content stream decides whether and how
// it is shown as lines.
type textKind int

const (
	textPlain textKind = iota
	textUTF16
	textUTF16TooLarge
	textBinary
	textLFSPointer
)

// prepareText looks at the start of br and returns the reader to take lines
// from: br itself, or the content decoded from UTF-16.
func prepareText(br *bufio.Reader, size int64) (*bufio.Reader, textKind) {
	head, _ := br.Peek(8000)
	switch {
	case hasUTF16BOM(head):
		raw, _ := io.ReadAll(io.LimitReader(br, maxUTF16Bytes+1))
		if len(raw) > maxUTF16Bytes {
			return nil, textUTF16TooLarge
		}
		return bufio.NewReaderSize(strings.NewReader(decodeUTF16(raw)), 64<<10), textUTF16
	case bytes.IndexByte(head, 0) >= 0:
		return nil, textBinary
	case bytes.HasPrefix(head, []byte("version https://git-lfs.github.com/spec/")) && size < 1024:
		return br, textLFSPointer
	}
	return br, textPlain
}

// shownLines returns every line of content as read_file shows it, through
// the same steps render takes. It is what an edit is checked against: an
// anchor the model was shown always equals the one computed here.
func (r *Repo) shownLines(ctx context.Context, content []byte) ([]shownLine, error) {
	br, kind := prepareText(bufio.NewReaderSize(bytes.NewReader(content), 64<<10), int64(len(content)))
	switch kind {
	case textBinary:
		return nil, errors.New("binary file: it has no lines")
	case textUTF16TooLarge:
		return nil, fmt.Errorf("UTF-16 text larger than %s: not shown", humanSize(maxUTF16Bytes))
	}
	src := &lineSource{br: br, st: r.red.NewState()}
	var out []shownLine
	for {
		if len(out)&255 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		ml, n, err := src.next(ctx)
		if n == 0 && errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		out = append(out, ml.shown())
		if err != nil {
			return out, nil
		}
	}
}
