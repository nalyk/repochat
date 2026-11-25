// Package guidance holds the on-disk state of owner-reviewed guidance: the
// reviewed record per tunnel, a monotonic generation counter per key, and the
// repository each key is assigned to.
package guidance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/nalyk/repochat/internal/repo"
)

const (
	RecordVersion    = 1
	TransformVersion = 1

	maxDisplay = 2048

	lifecycleFile   = "lifecycle.json"
	assignmentsFile = "assignments.json"
	lockFile        = ".lock"
)

// Record is one owner-reviewed guidance text, bound to a repository.
type Record struct {
	Version, Transform                int
	Key                               string
	Binding                           repo.Binding
	Path, RawSHA, Display, DisplaySHA string
	Reviewed                          time.Time
	Generation                        uint64
	Checksum                          string
}

// Store is the guidance directory under the state directory.
type Store struct{ dir string }

var (
	hex64    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	keyChars = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

// Key is a filesystem-safe identifier for a tunnel's runtime and guidance files.
func Key(tunnelID string) string {
	sum := sha256.Sum256([]byte(tunnelID))
	return hex.EncodeToString(sum[:])[:16]
}

// SHA256Hex is the hex SHA-256 of s, as used for DisplaySHA.
func SHA256Hex(s string) string { return sha(s) }

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// checksum is the SHA-256 of the record's JSON with Checksum empty. A struct's
// field order is fixed, so the encoding is canonical.
func checksum(r Record) (string, error) {
	r.Checksum = ""
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// resolve makes p absolute and resolves symbolic links in its existing part;
// the part that does not exist yet is appended unchanged.
func resolve(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	existing := abs
	var rest []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		rest = append([]string{filepath.Base(existing)}, rest...)
		existing = parent
	}
	real, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{real}, rest...)...), nil
}

// insideRepo reports whether dir, or any existing parent of it, is the same
// file (device and inode) as the repository root. Comparing files rather than
// strings holds on case-insensitive file systems.
func insideRepo(dir string, repoInfo os.FileInfo) bool {
	for p := dir; ; p = filepath.Dir(p) {
		if fi, err := os.Stat(p); err == nil && os.SameFile(fi, repoInfo) {
			return true
		}
		if filepath.Dir(p) == p {
			return false
		}
	}
}

// Open creates <stateDir>/guidance (0700). stateDir is resolved through
// symbolic links, as is repoRoot; a resolved directory inside the repository
// is refused. The guidance directory itself must be a real directory owned by
// the current user with mode 0700, and its files are never followed through
// links.
func Open(stateDir, repoRoot string) (*Store, error) {
	state, err := resolve(stateDir)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(state, "guidance")
	var repoInfo os.FileInfo
	if repoRoot != "" {
		rr, err := resolve(repoRoot)
		if err != nil {
			return nil, err
		}
		if repoInfo, err = os.Stat(rr); err != nil {
			return nil, err
		}
	}
	inside := func() error {
		if repoInfo != nil && insideRepo(dir, repoInfo) {
			return fmt.Errorf("guidance state directory %s is inside the repository %s; the repository must not hold its own review state", dir, repoRoot)
		}
		return nil
	}
	if err := inside(); err != nil {
		return nil, err
	}
	created := false
	if fi, err := os.Lstat(dir); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s is a symbolic link; guidance state is never kept behind a link", dir)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		created = true
	} else {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := inside(); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if created && fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, err
		}
		if fi, err = os.Lstat(dir); err != nil {
			return nil, err
		}
	}
	if !fi.Mode().IsDir() {
		return nil, fmt.Errorf("%s is not a real directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
		return nil, fmt.Errorf("%s is not owned by the current user", dir)
	}
	if fi.Mode().Perm() != 0o700 {
		return nil, fmt.Errorf("%s has mode %o, want 0700", dir, fi.Mode().Perm())
	}
	s := &Store{dir: dir}
	if err := s.sweepTemps(); err != nil {
		return nil, err
	}
	return s, nil
}

// sweepTemps removes temporary files a crashed write left behind.
func (s *Store) sweepTemps() error {
	unlock, err := s.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	// os.ReadDir, not filepath.Glob: the directory's own path may hold
	// pattern characters such as '['.
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".tmp-") {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Lock takes an exclusive flock on <dir>/.lock. Never call Load or
// Assignment while holding it: they take their own lock on the same file
// through another descriptor, and flock would deadlock.
func (s *Store) Lock() (func(), error) { return s.flock(syscall.LOCK_EX) }

// rlock takes a shared flock, for readers that want one consistent snapshot.
func (s *Store) rlock() (func(), error) { return s.flock(syscall.LOCK_SH) }

func (s *Store) flock(how int) (func(), error) {
	p := filepath.Join(s.dir, lockFile)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("guidance lock %s: %w", p, err)
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, fmt.Errorf("guidance lock %s: %w", p, err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

func refuseLink(p string) error {
	fi, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symbolic link; refusing to use it", p)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", p)
	}
	return nil
}

// writeAtomic writes a temporary file beside the target, fsyncs it and renames it.
func (s *Store) writeAtomic(name string, data []byte) error {
	p := filepath.Join(s.dir, name)
	if err := refuseLink(p); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	ok = true
	return s.syncDir()
}

// syncDir fsyncs the guidance directory so renames and removals are durable.
func (s *Store) syncDir() error {
	d, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

func (s *Store) readFile(name string) ([]byte, error) {
	f, err := os.OpenFile(filepath.Join(s.dir, name), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// readMap reads a JSON map file; a missing file is an empty map.
func readMap[V any](s *Store, name string) (map[string]V, error) {
	b, err := s.readFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]V{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("guidance %s: %w", filepath.Join(s.dir, name), err)
	}
	m := map[string]V{}
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		return nil, fmt.Errorf("guidance %s is damaged: %v", filepath.Join(s.dir, name), err)
	}
	return m, nil
}

func writeMap[V any](s *Store, name string, m map[string]V) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return s.writeAtomic(name, b)
}

func validKey(k string) error {
	if !keyChars.MatchString(k) {
		return fmt.Errorf("invalid guidance key %q", k)
	}
	return nil
}

func (s *Store) bump(key string) (uint64, error) {
	lc, err := readMap[uint64](s, lifecycleFile)
	if err != nil {
		return 0, err
	}
	lc[key]++
	if err := writeMap(s, lifecycleFile, lc); err != nil {
		return 0, err
	}
	return lc[key], nil
}

// Save stores r under r.Key: it bumps the key's generation, sets r.Generation
// and r.Checksum, writes the record and records the key's assignment. A key
// assigned to another canonical root is refused; one assigned to the same
// root has its assignment replaced by r's binding.
func (s *Store) Save(r *Record) error {
	if err := validKey(r.Key); err != nil {
		return err
	}
	if err := checkContent(r); err != nil {
		return fmt.Errorf("guidance record for %s refused: %w", r.Key, err)
	}
	unlock, err := s.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	as, err := readMap[repo.Binding](s, assignmentsFile)
	if err != nil {
		return err
	}
	// The same canonical root may be reviewed again after the rest of its
	// binding changed (an unrelated merge, a re-clone at that path): the
	// owner reviewed it, so the assignment follows. Another root never.
	if a, ok := as[r.Key]; ok && a.Root != r.Binding.Root {
		return fmt.Errorf("this tunnel is assigned to %s; to give it to another repository run `repochat trust --release-tunnel` from that profile first", a.Root)
	}
	// Lifecycle first: a crash afterwards leaves a skipped generation, never a
	// repeated one.
	gen, err := s.bump(r.Key)
	if err != nil {
		return err
	}
	c := *r
	c.Generation = gen
	sum, err := checksum(c)
	if err != nil {
		return err
	}
	c.Checksum = sum
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := s.writeAtomic(r.Key+".json", b); err != nil {
		return err
	}
	as[r.Key] = r.Binding
	if err := writeMap(s, assignmentsFile, as); err != nil {
		return err
	}
	r.Generation, r.Checksum = gen, sum
	return nil
}

// checkContent validates what does not depend on the store: versions, the
// raw digest syntax, the display cap and the display digest.
func checkContent(r *Record) error {
	switch {
	case r.Version != RecordVersion:
		return fmt.Errorf("version %d, want %d", r.Version, RecordVersion)
	case r.Transform != TransformVersion:
		return fmt.Errorf("transform %d, want %d", r.Transform, TransformVersion)
	case !hex64.MatchString(r.RawSHA):
		return errors.New("raw digest is not 64 hex characters")
	case len(r.Display) > maxDisplay:
		return fmt.Errorf("display is %d bytes, over %d", len(r.Display), maxDisplay)
	case sha(r.Display) != r.DisplaySHA:
		return errors.New("display does not match its digest")
	}
	return nil
}

// Load returns the record for key, (nil, nil) when there is none, and an
// error naming the file for anything damaged, superseded or unassigned. It
// reads under a shared lock, one consistent snapshot of the three files.
func (s *Store) Load(key string) (*Record, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	unlock, err := s.rlock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	name := key + ".json"
	path := filepath.Join(s.dir, name)
	b, rerr := s.readFile(name)
	lc, err := readMap[uint64](s, lifecycleFile)
	if err != nil {
		return nil, err
	}
	as, err := readMap[repo.Binding](s, assignmentsFile)
	if err != nil {
		return nil, err
	}
	if errors.Is(rerr, os.ErrNotExist) {
		return nil, nil
	}
	if rerr != nil {
		return nil, fmt.Errorf("guidance record %s: %w", path, rerr)
	}
	bad := func(format string, a ...any) (*Record, error) {
		return nil, fmt.Errorf("guidance record %s is damaged: %s", path, fmt.Sprintf(format, a...))
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return bad("%v", err)
	}
	if err := checkContent(&r); err != nil {
		return bad("%v", err)
	}
	if r.Key != key {
		return bad("key %q does not match its file", r.Key)
	}
	if r.Generation == 0 {
		return bad("generation 0 is not a valid generation")
	}
	want, err := checksum(r)
	if err != nil || want != r.Checksum {
		return bad("checksum mismatch")
	}
	if r.Generation != lc[key] {
		return nil, fmt.Errorf("guidance record %s is superseded: it is generation %d but the current generation is %d (revoked, released or replaced); it is not honoured", path, r.Generation, lc[key])
	}
	a, ok := as[key]
	if !ok {
		return nil, fmt.Errorf("guidance record %s has no assignment in %s; it is not honoured", path, filepath.Join(s.dir, assignmentsFile))
	}
	if !a.Equal(&r.Binding) {
		return nil, fmt.Errorf("guidance record %s is bound to a different repository than %s assigns; it is not honoured", path, filepath.Join(s.dir, assignmentsFile))
	}
	return &r, nil
}

// Revoke bumps the generation and removes the record; the assignment stays.
func (s *Store) Revoke(key string) error {
	if err := validKey(key); err != nil {
		return err
	}
	unlock, err := s.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := s.bump(key); err != nil {
		return err
	}
	return s.remove(key + ".json")
}

func (s *Store) remove(name string) error {
	p := filepath.Join(s.dir, name)
	if err := refuseLink(p); err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.syncDir()
}

// Assignment returns the binding key is assigned to, or (nil, nil).
func (s *Store) Assignment(key string) (*repo.Binding, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	unlock, err := s.rlock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	as, err := readMap[repo.Binding](s, assignmentsFile)
	if err != nil {
		return nil, err
	}
	b, ok := as[key]
	if !ok {
		return nil, nil
	}
	return &b, nil
}

// Release removes the assignment and the record and bumps the generation.
func (s *Store) Release(key string) error {
	if err := validKey(key); err != nil {
		return err
	}
	unlock, err := s.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := s.bump(key); err != nil {
		return err
	}
	if err := s.remove(key + ".json"); err != nil {
		return err
	}
	as, err := readMap[repo.Binding](s, assignmentsFile)
	if err != nil {
		return err
	}
	delete(as, key)
	return writeMap(s, assignmentsFile, as)
}
