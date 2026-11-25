package repo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

const (
	// maxGuidanceRaw is the most raw bytes of a guidance source file.
	maxGuidanceRaw = 8192
	// maxGuidanceDisplay is the most bytes of the masked, visible rendering.
	maxGuidanceDisplay = 2048
)

// guidanceCandidates are the root files `repochat trust` considers by
// default, in order of preference.
var guidanceCandidates = []string{"AGENTS.md", "CLAUDE.md"}

// GuidanceSource is the one reading of a guidance file: its path, its exact
// bytes and the SHA-256 of those bytes.
type GuidanceSource struct {
	Path   string
	Raw    []byte
	RawSHA string
}

// ReadGuidanceSource reads the guidance file once. An empty path picks the
// first of AGENTS.md and CLAUDE.md that git lists and the policy serves. The
// file must be at the root, a regular file (never a link), valid UTF-8, and at
// most 8192 bytes. The listing and policy are taken afresh (a snapshot built
// for this call), not from the cached one.
func (r *Repo) ReadGuidanceSource(ctx context.Context, path string) (*GuidanceSource, error) {
	s, err := r.buildSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	rel := path
	if rel == "" {
		for _, c := range guidanceCandidates {
			if e, ok := s.Lookup(c); ok && e.Withheld == "" && s.Policy.Check(c) == "" {
				rel = c
				break
			}
		}
		if rel == "" {
			return nil, errors.New("no AGENTS.md or CLAUDE.md at the repository root is listed by git and served by the policy")
		}
	}
	if rel == "" || strings.ContainsAny(rel, "/\\\x00") || rel == "." || rel == ".." {
		return nil, fmt.Errorf("%q is not a root-level file: guidance must be a file at the repository root", path)
	}
	if reason := s.Policy.Check(rel); reason != "" {
		return nil, withheldError(rel, reason)
	}
	e, ok := s.Lookup(rel)
	if !ok {
		if r.Opts.TrackedOnly {
			return nil, fmt.Errorf("%s is not a tracked file (untracked files are not served with --tracked-only)", rel)
		}
		return nil, fmt.Errorf("%s is not listed by git in this repository's working tree", rel)
	}
	switch e.Kind {
	case KindSymlink:
		return nil, fmt.Errorf("%s is a symbolic link; guidance is never read through a link", rel)
	case KindSubmodule:
		return nil, fmt.Errorf("%s is a Git submodule, not a regular file", rel)
	}
	if e.Conflict {
		return nil, fmt.Errorf("%s has unresolved merge conflicts", rel)
	}
	op := r.newOpener()
	defer op.Close()
	data, _, truncated, err := op.ReadAll(rel, maxGuidanceRaw)
	if err != nil {
		return nil, describeErr(rel, err)
	}
	if truncated {
		return nil, fmt.Errorf("%s is larger than %d bytes; guidance is never truncated", rel, maxGuidanceRaw)
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%s is not valid UTF-8", rel)
	}
	sum := sha256.Sum256(data)
	return &GuidanceSource{Path: rel, Raw: data, RawSHA: hex.EncodeToString(sum[:])}, nil
}

// GuidanceFileSHA is the SHA-256 of the current bytes of the root file rel,
// read once through the same no-follow opener as ReadGuidanceSource, and
// nothing else: no listing, no policy, no UTF-8 check. It is for drift only,
// so the errors are the raw ones (errors.Is(err, fs.ErrNotExist) means the
// file is gone). A file over the guidance limit returns "" and no error: it
// cannot be the reviewed text.
func (r *Repo) GuidanceFileSHA(rel string) (string, error) {
	if rel == "" || strings.ContainsAny(rel, "/\\\x00") || rel == "." || rel == ".." {
		return "", fmt.Errorf("%q is not a root-level file", rel)
	}
	op := r.newOpener()
	defer op.Close()
	data, _, truncated, err := op.ReadAll(rel, maxGuidanceRaw)
	if err != nil {
		return "", err
	}
	if truncated {
		return "", nil
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// guidanceRedactor masks whatever the run's options say: guidance is always
// shown masked.
var guidanceRedactor = NewRedactor(true)

// hiddenRune reports the runes GuidanceDisplay never shows as themselves. It
// is an allowlist: a rune is shown only when it is a newline or tab or
// graphic (so unassigned, format, control and private-use runes are all
// escaped), is not a variation selector or another default-ignorable
// code point, and is not one of the graphic runes that render blank.
func hiddenRune(r rune) bool {
	if r == '\n' || r == '\t' {
		return false
	}
	if !unicode.IsGraphic(r) ||
		unicode.Is(unicode.Variation_Selector, r) ||
		unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) {
		return true
	}
	switch r {
	case 0x115F, 0x1160, 0x3164, 0xFFA0, 0x2800, 0x034F, 0x17B4, 0x17B5:
		return true
	}
	return r >= 0x180B && r <= 0x180F
}

func stripHidden(s string) string {
	return strings.Map(func(r rune) rune {
		if hiddenRune(r) {
			return -1
		}
		return r
	}, s)
}

// GuidanceDisplay renders raw guidance as the owner reviews it and as it is
// later delivered. Secrets are masked always, whatever the run's options; a
// secret split by invisible characters is still found, because masking looks
// at each line with those characters removed (a line that gets masked is shown
// without them). Everything else is shown with control, bidirectional, tag
// (Sanitize) and invisible characters as \uNNNN or \UNNNNNNNN escapes. A
// result over 2048 bytes is refused, never truncated.
func (r *Repo) GuidanceDisplay(raw []byte) (string, error) {
	st := guidanceRedactor.NewState()
	lines := strings.Split(string(raw), "\n")
	for i, l := range lines {
		stripped := stripHidden(l)
		if m := st.Line(stripped); m != stripped {
			lines[i] = m
		}
	}
	var b strings.Builder
	for _, c := range Sanitize(strings.Join(lines, "\n")) {
		switch {
		case !hiddenRune(c):
			b.WriteRune(c)
		case c > 0xFFFF:
			fmt.Fprintf(&b, `\U%08x`, c)
		default:
			fmt.Fprintf(&b, `\u%04x`, c)
		}
	}
	out := b.String()
	if len(out) > maxGuidanceDisplay {
		return "", fmt.Errorf("the reviewed text is %d bytes once masked and made visible; the limit is %d bytes and nothing is truncated", len(out), maxGuidanceDisplay)
	}
	if n := SegmentMarkerLine(out); n > 0 {
		return "", SegmentMarkerError(n)
	}
	return out, nil
}

// SegmentMarkerLine is the 1-based number of the first line of display that
// reads as the guidance segment's own end marker or heading: its trimmed,
// case-folded text equals "end of owner guidance" or starts with "owner
// guidance". It is 0 when there is none. Such a line could close the segment
// early and make what follows read as server rules.
func SegmentMarkerLine(display string) int {
	for i, l := range strings.Split(display, "\n") {
		t := strings.ToLower(strings.TrimSpace(l))
		if t == "end of owner guidance" || strings.HasPrefix(t, "owner guidance") {
			return i + 1
		}
	}
	return 0
}

// SegmentMarkerError is the refusal for a marker on line n.
func SegmentMarkerError(n int) error {
	return fmt.Errorf("line %d of the guidance reads as the OWNER GUIDANCE heading or its END OF OWNER GUIDANCE marker, which could close the guidance section early; reword that line and run `repochat trust` again", n)
}

// Binding identifies the owner's repository: canonical paths, device and
// inode of both, and the sorted set of root commits of HEAD.
type Binding struct {
	Root, CommonDir      string
	RootDev, RootIno     uint64
	CommonDev, CommonIno uint64
	Roots                []string
}

// CaptureBinding records the binding of this repository. A shallow
// repository, whose root commits cannot be known, and an unborn HEAD are
// refused.
func (r *Repo) CaptureBinding(ctx context.Context) (*Binding, error) {
	if err := r.checkRoot(); err != nil {
		return nil, err
	}
	b := &Binding{}
	var err error
	if b.Root, err = filepath.EvalSymlinks(r.Root); err != nil {
		return nil, fmt.Errorf("cannot resolve the repository root: %v", err)
	}
	dir, code, err := r.gitText(ctx, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	if code != 0 || dir == "" {
		return nil, errors.New("cannot locate the git common directory")
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(r.Root, dir)
	}
	if b.CommonDir, err = filepath.EvalSymlinks(dir); err != nil {
		return nil, fmt.Errorf("cannot resolve the git common directory: %v", err)
	}
	if b.RootDev, b.RootIno, err = devIno(b.Root); err != nil {
		return nil, err
	}
	if b.CommonDev, b.CommonIno, err = devIno(b.CommonDir); err != nil {
		return nil, err
	}
	shallow, code, err := r.gitText(ctx, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return nil, err
	}
	if code != 0 || (shallow != "true" && shallow != "false") {
		return nil, errors.New("cannot tell whether the repository is shallow; refusing to bind it")
	}
	if shallow == "true" {
		return nil, errors.New("the repository is shallow, so its root commits are unknown; guidance cannot be bound to it")
	}
	if _, code, err = r.gitText(ctx, "rev-parse", "--verify", "-q", "HEAD^{commit}"); err != nil {
		return nil, err
	} else if code != 0 {
		return nil, errors.New("the repository has no commits yet (HEAD is unborn); guidance cannot be bound to it")
	}
	out, err := r.git(ctx, 1<<20, "-c", "core.commitGraph=false", "rev-list", "--max-parents=0", "HEAD")
	if err != nil {
		return nil, err
	}
	for _, f := range strings.Fields(string(out)) {
		if !objectIDRe.MatchString(f) {
			return nil, errors.New("unexpected output from git rev-list; refusing to bind")
		}
		b.Roots = append(b.Roots, f)
	}
	if len(b.Roots) == 0 {
		return nil, errors.New("the repository has no root commit; guidance cannot be bound to it")
	}
	sort.Strings(b.Roots)
	return b, nil
}

func devIno(path string) (dev, ino uint64, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, errors.New("the file system does not report device and inode numbers")
	}
	return uint64(st.Dev), uint64(st.Ino), nil
}

// Equal reports whether both bindings describe the same repository.
func (b *Binding) Equal(o *Binding) bool {
	if b == nil || o == nil {
		return b == o
	}
	if b.Root != o.Root || b.CommonDir != o.CommonDir ||
		b.RootDev != o.RootDev || b.RootIno != o.RootIno ||
		b.CommonDev != o.CommonDev || b.CommonIno != o.CommonIno ||
		len(b.Roots) != len(o.Roots) {
		return false
	}
	for i := range b.Roots {
		if b.Roots[i] != o.Roots[i] {
			return false
		}
	}
	return true
}

// PathServed takes a fresh decision for one root-relative path, never using
// the cached snapshot: a snapshot is built for this call, from the rules files
// as they are now.
//
// inputs states exactly what the decision rested on, and equal inputs mean an
// equal decision: the SHA-256 of the .repochatignore text the policy was built
// from ("absent" when there is none); a digest of the options that bear on
// exposure (tracked-only, no-redact, and the policy hash, which covers allow,
// deny, own prefixes and the second rules file's content as read); the identity of the git program that judges the
// rules; the snapshot's entry for the path (whether git lists it, tracked or
// not, kind, merge conflict, the index object id of a tracked file); and
// whether the file is present in the working tree and as what.
func (r *Repo) PathServed(ctx context.Context, path string) (served bool, inputs string, err error) {
	d, err := r.PathDecision(ctx, path)
	return d.Served, d.Inputs, err
}

// PathDecision is PathServed with what the decision means kept apart: Served
// as PathServed; Permitted when the policy allows the path itself, whether or
// not git lists it; Missing when the working tree definitely has no file
// there. A source that is Missing but Permitted is absent, not withheld.
type PathDecision struct {
	Served, Permitted, Missing bool
	Inputs                     string
}

// PathDecision takes the fresh decision PathServed reports.
func (r *Repo) PathDecision(ctx context.Context, path string) (PathDecision, error) {
	s, err := r.buildSnapshot(ctx)
	if err != nil {
		return PathDecision{}, err
	}
	e, listed := s.Lookup(path)
	in, err := r.guidanceInputs(s.IgnoreText, s.Policy, e, listed, path)
	if err != nil {
		return PathDecision{}, err
	}
	d := PathDecision{Permitted: s.Policy.Check(path) == "", Missing: in.present == presentMissing}
	d.Served = d.Permitted && listed && e.Withheld == "" && e.Kind == KindRegular
	d.Inputs = in.String()
	return d, nil
}

// GuidanceInputs states the inputs PathServed would report for path, without
// building a snapshot or asking git to judge any rule: it reads the rules
// files, parses them for the policy's hash, and asks git about this one path
// only. Equal results mean PathServed would decide as it did.
//
// It skips refreshFilterArgs and the fileset caps, and that is safe. The
// filter neutralisation is for git commands that read working-tree contents
// and so may run clean/smudge drivers; the only git commands here are
// ls-files with --stage or --others, which read the index and directory
// names and run no filter. The fileset caps bound the cost of listing the
// whole repository; this lists one path, so there is no size to bound, and
// the rules file is read under its own limit. A repository too large to
// serve fails at start, in the full snapshot, not here.
func (r *Repo) GuidanceInputs(ctx context.Context, path string) (string, error) {
	text, err := r.readIgnoreFile(ctx)
	if err != nil {
		return "", err
	}
	pol, err := r.newPolicy(text)
	if err != nil {
		return "", err
	}
	e, listed, err := r.entryOf(ctx, path)
	if err != nil {
		return "", err
	}
	in, err := r.guidanceInputs(text, pol, e, listed, path)
	if err != nil {
		return "", err
	}
	return in.String(), nil
}

// entryOf is the snapshot's entry for one path, read from git for that path
// alone, the way buildSnapshot reads it for all of them.
func (r *Repo) entryOf(ctx context.Context, path string) (Entry, bool, error) {
	cached, err := r.git(ctx, 64<<10, "ls-files", "-z", "--stage", "--", path)
	if err != nil {
		return Entry{}, false, fmt.Errorf("cannot list tracked files: %w", err)
	}
	var e Entry
	listed := false
	for _, rec := range bytes.Split(cached, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, p, ok := strings.Cut(string(rec), "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return Entry{}, false, errors.New("unexpected output from git ls-files; refusing to continue")
		}
		if p != path { // a directory of that name lists its files
			continue
		}
		if listed {
			e.Conflict = true
			continue
		}
		listed = true
		e = Entry{Path: p, Tracked: true, Conflict: fields[2] != "0", Object: fields[1]}
		switch fields[0] {
		case "120000":
			e.Kind = KindSymlink
		case "160000":
			e.Kind = KindSubmodule
		}
	}
	if listed || r.Opts.TrackedOnly {
		return e, listed, nil
	}
	others, err := r.git(ctx, 64<<10, "ls-files", "-z", "--others", "--exclude-standard", "--", path)
	if err != nil {
		return Entry{}, false, fmt.Errorf("cannot list untracked files: %w", err)
	}
	for _, rec := range bytes.Split(others, []byte{0}) {
		if string(rec) == path {
			return Entry{Path: path}, true, nil
		}
	}
	return Entry{}, false, nil
}

const (
	presentMissing    = "missing"
	presentRegular    = "regular"
	presentUnreadable = "unreadable"
)

// decisionInputs is what one exposure decision rests on; String is the
// inputs PathServed and GuidanceInputs report.
type decisionInputs struct {
	ignore, options, gitID string
	listed                 bool
	entry                  Entry
	present                string
}

func (d decisionInputs) String() string {
	e := d.entry
	return fmt.Sprintf("%s|%s|git=%s|entry=%t,%t,%d,%t,%s|file=%s",
		d.ignore, d.options, d.gitID, d.listed, e.Tracked, e.Kind, e.Conflict, e.Object, d.present)
}

// guidanceInputs is the one place the inputs of a decision about path are
// put together, from the .repochatignore text and policy it rests on and the
// path's entry (listed says whether git lists it).
func (r *Repo) guidanceInputs(ignoreText string, pol *Policy, e Entry, listed bool, path string) (decisionInputs, error) {
	d := decisionInputs{ignore: "absent", options: optionsDigest(r, pol), listed: listed, entry: e, present: presentMissing}
	if ignoreText != "" {
		sum := sha256.Sum256([]byte(ignoreText))
		d.ignore = hex.EncodeToString(sum[:])
	}
	var err error
	if _, d.gitID, err = gitIdentity(); err != nil {
		return decisionInputs{}, err
	}
	if fi, lerr := r.root.Lstat(path); lerr == nil {
		d.present = fi.Mode().Type().String()
		if fi.Mode().IsRegular() {
			d.present = presentRegular
		}
	} else if !errors.Is(lerr, os.ErrNotExist) {
		d.present = presentUnreadable
	}
	return d, nil
}

// optionsDigest covers the options that decide what is exposed. The policy's
// own hash stands for the rules, deny and allow patterns, own prefixes and the
// second rules file, as they were read.
func optionsDigest(r *Repo, p *Policy) string {
	h := sha256.New()
	fmt.Fprintf(h, "tracked=%t\nnoredact=%t\npolicy=%s\n", r.Opts.TrackedOnly, r.Opts.NoRedact, p.hash)
	return hex.EncodeToString(h.Sum(nil))
}

// SafeName is the rule that turns a directory name into a repository's Name:
// only letters, digits, '.', '_' and '-', at most 60 bytes.
func SafeName(s string) string { return safeName(s) }
