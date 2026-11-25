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

func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// noSymlinks refuses a symbolic link at any component of an absolute path
// that exists.
func noSymlinks(p string) error {
	cur := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(p, cur), cur) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link; guidance state is never kept behind a link", cur)
		}
	}
	return nil
}

// Open creates <stateDir>/guidance (0700). It refuses a directory inside the
// repository and any symbolic link on the path.
func Open(stateDir, repoRoot string) (*Store, error) {
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(abs, "guidance")
	if repoRoot != "" {
		rabs, err := filepath.Abs(repoRoot)
		if err != nil {
			return nil, err
		}
		roots := []string{rabs}
		if real, err := filepath.EvalSymlinks(rabs); err == nil {
			roots = append(roots, real)
		}
		for _, r := range roots {
			if within(r, dir) {
				return nil, fmt.Errorf("guidance state directory %s is inside the repository %s; the repository must not hold its own review state", dir, r)
			}
		}
	}
	if err := noSymlinks(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := noSymlinks(dir); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Lock takes an exclusive flock on <dir>/.lock.
func (s *Store) Lock() (func(), error) {
	p := filepath.Join(s.dir, lockFile)
	if err := refuseLink(p); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("guidance lock %s: %w", p, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
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
	if d, err := os.Open(s.dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

func (s *Store) readFile(name string) ([]byte, error) {
	p := filepath.Join(s.dir, name)
	if err := refuseLink(p); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	return b, nil
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
// and r.Checksum, writes the record and records the key's assignment.
func (s *Store) Save(r *Record) error {
	if err := validKey(r.Key); err != nil {
		return err
	}
	unlock, err := s.Lock()
	if err != nil {
		return err
	}
	defer unlock()
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
	as, err := readMap[repo.Binding](s, assignmentsFile)
	if err != nil {
		return err
	}
	as[r.Key] = r.Binding
	if err := writeMap(s, assignmentsFile, as); err != nil {
		return err
	}
	r.Generation, r.Checksum = gen, sum
	return nil
}

// Load returns the record for key, (nil, nil) when there is none, and an
// error naming the file for anything damaged.
func (s *Store) Load(key string) (*Record, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	name := key + ".json"
	path := filepath.Join(s.dir, name)
	b, err := s.readFile(name)
	if errors.Is(err, os.ErrNotExist) {
		// Still refuse a damaged lifecycle or assignments file.
		if _, err := readMap[uint64](s, lifecycleFile); err != nil {
			return nil, err
		}
		if _, err := readMap[repo.Binding](s, assignmentsFile); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("guidance record %s: %w", path, err)
	}
	bad := func(format string, a ...any) (*Record, error) {
		return nil, fmt.Errorf("guidance record %s is damaged: %s", path, fmt.Sprintf(format, a...))
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return bad("%v", err)
	}
	lc, err := readMap[uint64](s, lifecycleFile)
	if err != nil {
		return nil, err
	}
	if _, err := readMap[repo.Binding](s, assignmentsFile); err != nil {
		return nil, err
	}
	switch {
	case r.Version != RecordVersion:
		return bad("version %d, want %d", r.Version, RecordVersion)
	case r.Transform != TransformVersion:
		return bad("transform %d, want %d", r.Transform, TransformVersion)
	case r.Key != key:
		return bad("key %q does not match its file", r.Key)
	case !hex64.MatchString(r.RawSHA):
		return bad("raw digest is not 64 hex characters")
	case len(r.Display) > maxDisplay:
		return bad("display is %d bytes, over %d", len(r.Display), maxDisplay)
	case sha(r.Display) != r.DisplaySHA:
		return bad("display does not match its digest")
	case r.Generation == 0 || r.Generation > lc[key]:
		return bad("generation %d is beyond the lifecycle value %d", r.Generation, lc[key])
	}
	want, err := checksum(r)
	if err != nil || want != r.Checksum {
		return bad("checksum mismatch")
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
	return nil
}

// Assignment returns the binding key is assigned to, or (nil, nil).
func (s *Store) Assignment(key string) (*repo.Binding, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
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
