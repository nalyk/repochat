package guidance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nalyk/repochat/internal/repo"
)

func hx(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	state := t.TempDir()
	repoRoot := t.TempDir()
	s, err := Open(state, repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	return s, state
}

func rec(key string) *Record {
	disp := "Be kind.\n"
	return &Record{
		Version: RecordVersion, Transform: TransformVersion, Key: key,
		Binding: repo.Binding{Root: "/r", CommonDir: "/r/.git", RootDev: 1, RootIno: 2, CommonDev: 1, CommonIno: 3, Roots: []string{"abc"}},
		Path:    "AGENTS.md", RawSHA: hx("raw"), Display: disp, DisplaySHA: hx(disp),
		Reviewed: time.Unix(1700000000, 0).UTC(),
	}
}

func TestKeyMatchesLegacy(t *testing.T) {
	h := sha256.Sum256([]byte("tun"))
	if got, want := Key("tun"), hex.EncodeToString(h[:])[:16]; got != want {
		t.Fatalf("Key = %q, want %q", got, want)
	}
}

func TestSaveLoadRoundTripAndPermissions(t *testing.T) {
	s, state := newStore(t)
	r := rec("k1")
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	if r.Generation != 1 || r.Checksum == "" {
		t.Fatalf("generation %d checksum %q", r.Generation, r.Checksum)
	}
	got, err := s.Load("k1")
	if err != nil || got == nil {
		t.Fatalf("load: %v %v", got, err)
	}
	if got.Display != r.Display || got.Generation != 1 || !got.Binding.Equal(&r.Binding) {
		t.Fatalf("mismatch: %+v", got)
	}
	dir := filepath.Join(state, "guidance")
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", fi.Mode().Perm())
	}
	for _, n := range []string{"k1.json", "lifecycle.json", "assignments.json"} {
		fi, err := os.Stat(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", n, fi.Mode().Perm())
		}
	}
	if none, err := s.Load("absent"); none != nil || err != nil {
		t.Fatalf("absent: %v %v", none, err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestLoadRefusesDamage(t *testing.T) {
	type damage struct {
		reason string
		do     func(t *testing.T, p string, d []byte)
	}
	write := func(t *testing.T, p string, b []byte) { must(t, os.WriteFile(p, b, 0o600)) }
	cases := map[string]damage{
		"flipped display byte": {"display does not match", func(t *testing.T, p string, d []byte) {
			write(t, p, []byte(strings.Replace(string(d), "Be kind.", "Be Kind.", 1)))
		}},
		"bad checksum": {"checksum mismatch", func(t *testing.T, p string, d []byte) {
			i := strings.Index(string(d), `"Checksum": "`) + len(`"Checksum": "`)
			b := []byte(string(d))
			if b[i] == '0' {
				b[i] = '1'
			} else {
				b[i] = '0'
			}
			write(t, p, b)
		}},
		"non-hex raw digest": {"raw digest", func(t *testing.T, p string, d []byte) {
			write(t, p, []byte(strings.Replace(string(d), hx("raw"), strings.Repeat("z", 64), 1)))
		}},
		"transform 2": {"transform 2", func(t *testing.T, p string, d []byte) {
			write(t, p, []byte(strings.Replace(string(d), `"Transform": 1`, `"Transform": 2`, 1)))
		}},
		"truncated": {"is damaged", func(t *testing.T, p string, d []byte) { write(t, p, d[:len(d)/2]) }},
		"generation 0": {"generation 0", func(t *testing.T, p string, d []byte) {
			write(t, p, []byte(strings.Replace(string(d), `"Generation": 1`, `"Generation": 0`, 1)))
		}},
		"damaged lifecycle": {"lifecycle.json", func(t *testing.T, p string, d []byte) {
			write(t, filepath.Join(filepath.Dir(p), "lifecycle.json"), []byte("{"))
		}},
		"damaged assignments": {"assignments.json", func(t *testing.T, p string, d []byte) {
			write(t, filepath.Join(filepath.Dir(p), "assignments.json"), []byte("{"))
		}},
		"generation beyond lifecycle": {"superseded", func(t *testing.T, p string, d []byte) {
			write(t, filepath.Join(filepath.Dir(p), "lifecycle.json"), []byte(`{"k":0}`))
		}},
		"assignment missing": {"no assignment", func(t *testing.T, p string, d []byte) {
			write(t, filepath.Join(filepath.Dir(p), "assignments.json"), []byte(`{}`))
		}},
		"assignment differs": {"different repository", func(t *testing.T, p string, d []byte) {
			write(t, filepath.Join(filepath.Dir(p), "assignments.json"), []byte(`{"k":{"Root":"/other"}}`))
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s, state := newStore(t)
			must(t, s.Save(rec("k")))
			p := filepath.Join(state, "guidance", "k.json")
			d, err := os.ReadFile(p)
			must(t, err)
			c.do(t, p, d)
			got, err := s.Load("k")
			if err == nil || got != nil {
				t.Fatalf("want error, got %v %v", got, err)
			}
			if !strings.Contains(err.Error(), c.reason) {
				t.Fatalf("error %q does not mention %q", err, c.reason)
			}
		})
	}
}

func TestInterruptedRevokeAndSaveAreRefused(t *testing.T) {
	// Revoke bumps the lifecycle first; a crash leaves the record behind.
	s, _ := newStore(t)
	must(t, s.Save(rec("k")))
	unlock, err := s.Lock()
	must(t, err)
	_, err = s.bump("k")
	unlock()
	must(t, err)
	got, err := s.Load("k")
	if err == nil || got != nil || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("interrupted revoke: %v %v", got, err)
	}
	// A Save that crashed after the bump (before writing the new record) is the same state.
	s2, _ := newStore(t)
	must(t, s2.Save(rec("k")))
	must(t, s2.Save(rec("k")))
	unlock, err = s2.Lock()
	must(t, err)
	_, err = s2.bump("k")
	unlock()
	must(t, err)
	if got, err := s2.Load("k"); err == nil || got != nil || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("crashed save: %v %v", got, err)
	}
}

func TestSaveRefusesInvalidRecordBeforeBumping(t *testing.T) {
	s, state := newStore(t)
	r := rec("k")
	r.Display = strings.Repeat("a", 2049)
	r.DisplaySHA = hx(r.Display)
	if err := s.Save(r); err == nil {
		t.Fatal("oversize accepted")
	}
	if _, err := os.Stat(filepath.Join(state, "guidance", "lifecycle.json")); err == nil {
		t.Fatal("lifecycle bumped for a refused record")
	}
}

func TestLoadRefusesOversizeDisplay(t *testing.T) {
	s, state := newStore(t)
	must(t, s.Save(rec("k")))
	p := filepath.Join(state, "guidance", "k.json")
	d, err := os.ReadFile(p)
	must(t, err)
	var r Record
	must(t, json.Unmarshal(d, &r))
	r.Display = strings.Repeat("a", 2049)
	r.DisplaySHA = hx(r.Display)
	r.Checksum, err = checksum(r)
	must(t, err)
	b, err := json.Marshal(r)
	must(t, err)
	must(t, os.WriteFile(p, b, 0o600))
	if got, err := s.Load("k"); err == nil || got != nil || !strings.Contains(err.Error(), "over 2048") {
		t.Fatalf("want cap error, got %v %v", got, err)
	}
}

func TestOpenSweepsTempFiles(t *testing.T) {
	s, state := newStore(t)
	tmp := filepath.Join(state, "guidance", ".tmp-123")
	must(t, os.WriteFile(tmp, []byte("x"), 0o600))
	_, err := Open(state, t.TempDir())
	must(t, err)
	if _, err := os.Stat(tmp); err == nil {
		t.Fatal("temp file survived Open")
	}
	_ = s
}

func TestGenerationsNeverRepeat(t *testing.T) {
	s, _ := newStore(t)
	r := rec("k")
	must(t, s.Save(r))
	if r.Generation != 1 {
		t.Fatal(r.Generation)
	}
	must(t, s.Revoke("k"))
	if got, err := s.Load("k"); got != nil || err != nil {
		t.Fatalf("after revoke: %v %v", got, err)
	}
	r2 := rec("k")
	must(t, s.Save(r2))
	if r2.Generation != 3 {
		t.Fatalf("generation %d, want 3", r2.Generation)
	}
}

func TestAssignmentSurvivesRevoke(t *testing.T) {
	s, _ := newStore(t)
	r := rec("k")
	must(t, s.Save(r))
	must(t, s.Revoke("k"))
	b, err := s.Assignment("k")
	if err != nil || b == nil || !b.Equal(&r.Binding) {
		t.Fatalf("assignment: %v %v", b, err)
	}
	if err := s.Release("k"); err != nil {
		t.Fatal(err)
	}
	if b, err := s.Assignment("k"); b != nil || err != nil {
		t.Fatalf("after release: %v %v", b, err)
	}
	r2 := rec("k")
	must(t, s.Save(r2))
	if r2.Generation != 4 {
		t.Fatalf("generation %d, want 4 (save, revoke, release, save)", r2.Generation)
	}
}

func TestOpenRefusesInsideRepoAndSymlinks(t *testing.T) {
	repoRoot := t.TempDir()
	if _, err := Open(filepath.Join(repoRoot, "state"), repoRoot); err == nil {
		t.Fatal("inside repo accepted")
	}
	// inside the repo by way of a symlink
	must(t, os.Mkdir(filepath.Join(repoRoot, "sub"), 0o755))
	viaLink := filepath.Join(t.TempDir(), "l")
	must(t, os.Symlink(filepath.Join(repoRoot, "sub"), viaLink))
	if _, err := Open(viaLink, repoRoot); err == nil {
		t.Fatal("state reached through a link into the repo accepted")
	}
	// symlinked guidance directory
	state := t.TempDir()
	target := t.TempDir()
	must(t, os.Symlink(target, filepath.Join(state, "guidance")))
	if _, err := Open(state, t.TempDir()); err == nil {
		t.Fatal("symlinked guidance dir accepted")
	}
	// wrong mode on an existing guidance directory
	state2 := t.TempDir()
	must(t, os.Mkdir(filepath.Join(state2, "guidance"), 0o755))
	must(t, os.Chmod(filepath.Join(state2, "guidance"), 0o755))
	if _, err := Open(state2, t.TempDir()); err == nil {
		t.Fatal("mode 0755 guidance dir accepted")
	}
	// symlinked file inside the store
	s, st := newStore(t)
	must(t, os.Symlink(filepath.Join(target, "x"), filepath.Join(st, "guidance", "lifecycle.json")))
	if err := s.Save(rec("k")); err == nil {
		t.Fatal("symlinked lifecycle accepted")
	}
}

func TestOpenWorksThroughSymlinkedParent(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	must(t, os.Symlink(real, link))
	s, err := Open(filepath.Join(link, "state"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	must(t, s.Save(rec("k")))
	if r, err := s.Load("k"); err != nil || r == nil {
		t.Fatalf("%v %v", r, err)
	}
	if _, err := os.Stat(filepath.Join(real, "state", "guidance", "k.json")); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentSavesSerialize(t *testing.T) {
	s, _ := newStore(t)
	var wg sync.WaitGroup
	for _, k := range []string{"a", "b", "a", "b", "a", "b"} {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			if err := s.Save(rec(k)); err != nil {
				t.Error(err)
			}
		}(k)
	}
	wg.Wait()
	for _, k := range []string{"a", "b"} {
		r, err := s.Load(k)
		if err != nil || r == nil || r.Generation != 3 {
			t.Fatalf("%s: %+v %v", k, r, err)
		}
	}
}

func TestRevokeConcurrentWithSave(t *testing.T) {
	s, _ := newStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := s.Save(rec("a")); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := s.Revoke("a"); err != nil {
				t.Error(err)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Load("a"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	r, err := s.Load("a")
	if err != nil {
		t.Fatal(err)
	}
	if r != nil && r.Generation != 8 {
		t.Fatalf("surviving record has generation %d, want 8", r.Generation)
	}
}

func TestSaveRefusesAnotherAssignment(t *testing.T) {
	s, _ := newStore(t)
	a := rec("k1")
	if err := s.Save(a); err != nil {
		t.Fatal(err)
	}
	b := rec("k1")
	b.Binding = repo.Binding{Root: "/other", CommonDir: "/other/.git", RootDev: 1, RootIno: 9, CommonDev: 1, CommonIno: 10, Roots: []string{"abc"}}
	err := s.Save(b)
	if err == nil || !strings.Contains(err.Error(), "/r") || !strings.Contains(err.Error(), "--release-tunnel") {
		t.Fatalf("err %v", err)
	}
	got, err := s.Load("k1")
	if err != nil || got == nil || got.Binding.Root != "/r" || got.Generation != 1 {
		t.Fatalf("record %+v err %v", got, err)
	}
	// The same binding may be saved again, and a release clears the way.
	if err := s.Save(rec("k1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Release("k1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(b); err != nil {
		t.Fatal(err)
	}
}

// I1: a review of the same root replaces the assignment even when the rest of
// the binding changed (an unrelated merge, a re-clone at that path); another
// root is still refused.
func TestSaveReplacesTheAssignmentOfTheSameRoot(t *testing.T) {
	s, _ := newStore(t)
	must(t, s.Save(rec("k1")))
	changed := rec("k1")
	changed.Binding.Roots = []string{"abc", "def"}
	changed.Binding.RootIno, changed.Binding.CommonIno = 7, 8
	if err := s.Save(changed); err != nil {
		t.Fatalf("same root, changed binding: %v", err)
	}
	got, err := s.Load("k1")
	if err != nil || got == nil || got.Generation != 2 || !got.Binding.Equal(&changed.Binding) {
		t.Fatalf("record %+v err %v", got, err)
	}
	a, err := s.Assignment("k1")
	if err != nil || a == nil || !a.Equal(&changed.Binding) {
		t.Fatalf("assignment %+v err %v", a, err)
	}
	other := rec("k1")
	other.Binding.Root = "/elsewhere"
	if err := s.Save(other); err == nil || !strings.Contains(err.Error(), "--release-tunnel") {
		t.Fatalf("another root: %v", err)
	}
}

// m4: a '[' in the state path is a glob pattern to filepath.Glob; the sweep
// must not depend on it.
func TestOpenWithABracketInTheStatePath(t *testing.T) {
	state := filepath.Join(t.TempDir(), "st[ate")
	must(t, os.MkdirAll(state, 0o700))
	s, err := Open(state, t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	must(t, s.Save(rec("k")))
	tmp := filepath.Join(state, "guidance", ".tmp-9")
	keep := filepath.Join(state, "guidance", "k.json")
	must(t, os.WriteFile(tmp, []byte("x"), 0o600))
	if _, err := Open(state, t.TempDir()); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := os.Stat(tmp); err == nil {
		t.Fatal("temp file survived Open")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("the record was swept: %v", err)
	}
}
