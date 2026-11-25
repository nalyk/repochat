package repo

import (
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
// most 8192 bytes. The listing and policy are taken afresh, not from the
// cached snapshot.
func (r *Repo) ReadGuidanceSource(ctx context.Context, path string) (*GuidanceSource, error) {
	r.mu.Lock()
	r.snap = nil
	r.mu.Unlock()
	s, err := r.Snapshot(ctx)
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

// GuidanceDisplay renders raw guidance as the owner reviews it and as it is
// later delivered: secrets masked, control, bidirectional and tag characters
// shown as escapes (Sanitize), and zero-width characters shown the same way.
// A result over 2048 bytes is refused, never truncated.
func (r *Repo) GuidanceDisplay(raw []byte) (string, error) {
	masked, _ := r.red.Text(string(raw), false)
	var b strings.Builder
	for _, c := range Sanitize(masked) {
		switch c {
		case 0x200B, 0x200C, 0x200D, 0x2060, 0xFEFF:
			fmt.Fprintf(&b, `\u%04x`, c)
		default:
			b.WriteRune(c)
		}
	}
	out := b.String()
	if len(out) > maxGuidanceDisplay {
		return "", fmt.Errorf("the reviewed text is %d bytes once masked and made visible; the limit is %d bytes and nothing is truncated", len(out), maxGuidanceDisplay)
	}
	return out, nil
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
	out, err := r.git(ctx, 1<<20, "rev-list", "--max-parents=0", "HEAD")
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
// the cached snapshot: a new listing and the policy as the rules files are
// now. inputs names what the decision rested on: the SHA-256 of the owner's
// .repochatignore ("absent" when there is none), then a digest of the
// options that bear on exposure. The same inputs mean the same decision.
func (r *Repo) PathServed(ctx context.Context, path string) (served bool, inputs string, err error) {
	before, err := r.ignoreInput()
	if err != nil {
		return false, "", err
	}
	s, err := r.buildSnapshot(ctx)
	if err != nil {
		return false, "", err
	}
	after, err := r.ignoreInput()
	if err != nil {
		return false, "", err
	}
	if before != after {
		return false, "", errors.New(".repochatignore changed while the decision was being taken; try again")
	}
	inputs = after + "|" + r.optionsDigest()
	if s.Policy.Check(path) != "" {
		return false, inputs, nil
	}
	e, ok := s.Lookup(path)
	return ok && e.Withheld == "" && e.Kind == KindRegular, inputs, nil
}

// ignoreInput is the SHA-256 of the owner checkout's .repochatignore, or
// "absent".
func (r *Repo) ignoreInput() (string, error) {
	op := r.newOpener()
	defer op.Close()
	data, _, truncated, err := op.ReadAll(ignoreFileName, maxIgnoreFile)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "absent", nil
	case err != nil:
		return "", describeErr(ignoreFileName, err)
	case truncated:
		return "", errors.New(".repochatignore is larger than 256 KB; refusing to guess at the policy")
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// optionsDigest covers the options that decide what is exposed, including
// the content of the second rules file.
func (r *Repo) optionsDigest() string {
	h := sha256.New()
	fmt.Fprintf(h, "tracked=%t\nnoredact=%t\n", r.Opts.TrackedOnly, r.Opts.NoRedact)
	for _, k := range []struct {
		tag  string
		list []string
	}{{"allow", r.Opts.Allow}, {"deny", r.Opts.Deny}, {"own", r.Opts.Own}} {
		for _, v := range k.list {
			fmt.Fprintf(h, "%s=%q\n", k.tag, v)
		}
	}
	if r.Opts.ExtraIgnoreFile != "" {
		text, found, err := readExtraIgnore(r.Opts.ExtraIgnoreFile)
		fmt.Fprintf(h, "extra=%q found=%t err=%t text=%q\n", r.Opts.ExtraIgnoreFile, found, err != nil, text)
	}
	return hex.EncodeToString(h.Sum(nil))
}
