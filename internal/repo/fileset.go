package repo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// File kinds as recorded by git.
const (
	KindRegular = iota
	KindSymlink
	KindSubmodule
)

// Entry is one path git knows about in the working tree.
type Entry struct {
	Path     string
	Kind     uint8
	Tracked  bool
	Conflict bool   // has unmerged index stages
	Withheld string // "" when served, otherwise the reason category
}

// Snapshot is the authorised view of the working tree at one moment: every
// file tool works from it and nothing outside it is ever opened.
type Snapshot struct {
	Entries    []Entry // sorted by path
	Generation string  // changes whenever the set of paths or the policy changes
	Policy     *Policy
	Warnings   []string
	Visible    int
	Tracked    int
	Untracked  int
	Withheld   map[string]int // reason → count
	index      map[string]int
}

// Lookup returns the entry for a path, if git lists it.
func (s *Snapshot) Lookup(rel string) (Entry, bool) {
	i, ok := s.index[rel]
	if !ok {
		return Entry{}, false
	}
	return s.Entries[i], true
}

// WithheldTotal is the number of withheld paths.
func (s *Snapshot) WithheldTotal() int {
	n := 0
	for _, c := range s.Withheld {
		n += c
	}
	return n
}

// WithheldSummary renders the withheld counts per category, e.g.
// "3 secret-name, 1 repochatignore".
func (s *Snapshot) WithheldSummary() string { return summarizeCounts(s.Withheld) }

func summarizeCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k, n := range m {
		if n > 0 {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", m[k], k))
	}
	return strings.Join(parts, ", ")
}

// Snapshot returns the current view, rebuilding it when older than a few
// seconds. Any failure to enumerate fails closed: no stale or partial list is
// ever used as the authorisation set.
func (r *Repo) Snapshot(ctx context.Context) (*Snapshot, error) {
	r.mu.Lock()
	if r.snap != nil && time.Since(r.snapAt) < snapshotTTL {
		s := r.snap
		r.mu.Unlock()
		return s, nil
	}
	r.mu.Unlock()

	s, err := r.buildSnapshot(ctx)
	if err != nil {
		r.mu.Lock()
		r.snap = nil
		r.mu.Unlock()
		return nil, err
	}
	r.mu.Lock()
	r.snap, r.snapAt = s, time.Now()
	r.mu.Unlock()
	return s, nil
}

// Policy returns the current policy (from the current snapshot).
func (r *Repo) Policy(ctx context.Context) (*Policy, error) {
	s, err := r.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return s.Policy, nil
}

const ignoreFileName = ".repochatignore"

func (r *Repo) buildSnapshot(ctx context.Context) (*Snapshot, error) {
	if err := r.checkRoot(); err != nil {
		return nil, err
	}
	if err := r.refreshFilterArgs(ctx); err != nil {
		return nil, err
	}

	s := &Snapshot{Withheld: map[string]int{}, index: map[string]int{}}

	// .repochatignore is read through the same no-follow path as every file.
	ignoreText := ""
	op := r.newOpener()
	data, _, truncated, err := op.ReadAll(ignoreFileName, 256<<10)
	op.Close()
	switch {
	case err == nil && truncated:
		return nil, errors.New(".repochatignore is larger than 256 KB; refusing to guess at the policy")
	case err == nil:
		ignoreText = string(data)
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, fmt.Errorf("cannot read .repochatignore (%v); refusing to run without the policy it defines", describeErr(ignoreFileName, err))
	}
	if s.Policy, err = NewPolicy(ignoreText, r.deny, r.allow); err != nil {
		return nil, err
	}
	s.Policy.own = r.own
	s.Policy.hash += "+" + strings.Join(r.own, "|")

	const listLimit = 256 << 20
	cached, err := r.git(ctx, listLimit, "ls-files", "-z", "--stage")
	if err != nil {
		return nil, fmt.Errorf("cannot list tracked files: %w", err)
	}
	pathBytes := 0
	add := func(e Entry) error {
		pathBytes += len(e.Path)
		if len(s.Entries) >= maxFilesetEntries || pathBytes > maxFilesetBytes {
			return fmt.Errorf("repository has more than %d files (or %d MB of path names); repochat refuses to serve a partial list", maxFilesetEntries, maxFilesetBytes>>20)
		}
		s.index[e.Path] = len(s.Entries)
		s.Entries = append(s.Entries, e)
		return nil
	}
	for _, rec := range bytes.Split(cached, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		// "<mode> <object> <stage>\t<path>"
		meta, path, ok := strings.Cut(string(rec), "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return nil, errors.New("unexpected output from git ls-files; refusing to continue")
		}
		if i, dup := s.index[path]; dup {
			s.Entries[i].Conflict = true
			continue
		}
		e := Entry{Path: path, Tracked: true, Conflict: fields[2] != "0"}
		switch fields[0] {
		case "120000":
			e.Kind = KindSymlink
		case "160000":
			e.Kind = KindSubmodule
		}
		if err := add(e); err != nil {
			return nil, err
		}
	}
	if !r.Opts.TrackedOnly {
		others, err := r.git(ctx, listLimit, "ls-files", "-z", "--others", "--exclude-standard")
		if err != nil {
			return nil, fmt.Errorf("cannot list untracked files: %w", err)
		}
		for _, rec := range bytes.Split(others, []byte{0}) {
			path := string(rec)
			if path == "" || strings.HasSuffix(path, "/") { // a nested repository
				continue
			}
			if _, dup := s.index[path]; dup {
				continue
			}
			if err := add(Entry{Path: path}); err != nil {
				return nil, err
			}
		}
	}

	sort.Slice(s.Entries, func(i, j int) bool { return s.Entries[i].Path < s.Entries[j].Path })
	h := sha256.New()
	h.Write([]byte(s.Policy.hash))
	for i := range s.Entries {
		e := &s.Entries[i]
		s.index[e.Path] = i
		e.Withheld = s.Policy.Check(e.Path)
		if e.Withheld != "" {
			s.Withheld[e.Withheld]++
		} else {
			s.Visible++
			if e.Tracked {
				s.Tracked++
			} else {
				s.Untracked++
			}
		}
		h.Write([]byte(e.Path))
		h.Write([]byte{0, e.Kind})
	}
	s.Generation = hex.EncodeToString(h.Sum(nil))[:12]
	return s, nil
}

// CheckPath validates a model-supplied path and returns its cleaned form.
// It rejects absolute paths, parent references and anything inside .git.
func CheckPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimSuffix(p, "/")
	if p == "" || p == "." {
		return "", errors.New("path is required (repository-relative, e.g. src/main.go)")
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") {
		return "", errors.New("use a repository-relative path, not an absolute one")
	}
	if strings.Contains(p, `\`) && !strings.Contains(p, "/") {
		p = strings.ReplaceAll(p, `\`, "/")
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
			return "", fmt.Errorf("invalid path %q", SanitizeLine(p))
		case "..":
			return "", errors.New("paths may not contain '..'")
		case ".git":
			return "", errors.New("the .git directory is not accessible")
		}
	}
	if !SupportedName(p) {
		return "", errors.New("path contains unsupported characters")
	}
	return p, nil
}

// exactPath reports whether p, unchanged, is a well-formed repository-relative
// path that stays outside .git. Unlike CheckPath it repairs nothing, so a
// blank at either end, a backslash or a leading "~" is part of the name.
func exactPath(p string) bool {
	if strings.HasPrefix(p, "/") || !SupportedName(p) { // SupportedName rules out empty, "." and ".." segments
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".git" {
			return false
		}
	}
	return true
}

// pathForms returns the readings of a model-supplied path, most literal
// first: the string exactly as given, when CheckPath would change or reject
// it, and then its cleaned form. Listings print names as they are, and a
// name that a listing shows must work when it is passed back; cleaning alone
// turns " notes.txt" or `a\b.txt` into the name of a different file. Both
// readings are well-formed relative paths outside .git; neither can name
// anything that git does not list.
func pathForms(p string) ([]string, error) {
	clean, err := CheckPath(p)
	var forms []string
	if exactPath(p) && (err != nil || p != clean) {
		forms = append(forms, p)
	}
	if err == nil {
		forms = append(forms, clean)
	}
	if len(forms) == 0 {
		return nil, err
	}
	return forms, nil
}

// gitPath resolves a model-supplied path for a git command. The working tree
// decides between the readings: the string exactly as given when git lists a
// file or directory by that name (a listing printed it), otherwise the
// cleaned form, which is also what a path that exists only in history gets.
// A reading that the policy withholds is refused, whichever would be used.
func (r *Repo) gitPath(ctx context.Context, p string) (string, error) {
	forms, err := pathForms(p)
	if err != nil {
		return "", err
	}
	s, err := r.Snapshot(ctx)
	if err != nil {
		return "", err
	}
	for _, rel := range forms {
		if reason := s.Policy.Check(rel); reason != "" {
			return "", withheldError(rel, reason)
		}
	}
	for _, rel := range forms {
		if _, ok := s.Lookup(rel); ok || s.hasVisibleBelow(rel, false) {
			return rel, nil
		}
	}
	return forms[len(forms)-1], nil
}

// hasVisibleBelow reports whether a path the caller may see lies beneath dir.
func (s *Snapshot) hasVisibleBelow(dir string, withheldToo bool) bool {
	prefix := dir + "/"
	i := sort.Search(len(s.Entries), func(i int) bool { return s.Entries[i].Path >= prefix })
	for ; i < len(s.Entries) && strings.HasPrefix(s.Entries[i].Path, prefix); i++ {
		if withheldToo || s.Entries[i].Withheld == "" {
			return true
		}
	}
	return false
}

// authorizeFile resolves a model-supplied path to a served working-tree
// entry, distinguishing "withheld" from "not found". Whether a reading is
// withheld is decided from the string alone, before looking whether such a
// file exists, so the answer never confirms that a withheld file is there.
func (r *Repo) authorizeFile(ctx context.Context, p string) (string, Entry, *Snapshot, error) {
	forms, err := pathForms(p)
	if err != nil {
		return "", Entry{}, nil, err
	}
	s, err := r.Snapshot(ctx)
	if err != nil {
		return "", Entry{}, nil, err
	}
	for _, rel := range forms {
		if reason := s.Policy.Check(rel); reason != "" {
			return "", Entry{}, s, withheldError(rel, reason)
		}
		if e, ok := s.Lookup(rel); ok {
			return rel, e, s, nil
		}
	}
	// Not a listed file under any reading: say so for the reading that is a
	// directory, if one is. Otherwise, when the string could only be read
	// literally, the reason CheckPath rejects it is the useful answer.
	for _, rel := range forms {
		if s.hasVisibleBelow(rel, false) {
			return "", Entry{}, s, notFoundError(s, rel)
		}
	}
	if _, err := CheckPath(p); err != nil {
		return "", Entry{}, s, err
	}
	return "", Entry{}, s, notFoundError(s, forms[len(forms)-1])
}

func withheldError(rel, reason string) error {
	return fmt.Errorf("%s is withheld by the repository owner's policy (%s): %w. It exists but its content is not available; do not guess it — tell the user if you need it",
		rel, reason, ErrWithheld)
}

func notFoundError(s *Snapshot, rel string) error {
	// Only a served file makes its directory known: saying "is a directory"
	// for one that holds nothing but withheld files would confirm a guessed
	// name of something the policy hides.
	if s.hasVisibleBelow(rel, false) {
		return fmt.Errorf("%s is a directory; use list_files with path=\"%s\"", rel, rel)
	}
	return fmt.Errorf("%s is not a file in this repository's working tree (not tracked, or ignored by .gitignore). Use find_files or list_files to locate it", rel)
}
