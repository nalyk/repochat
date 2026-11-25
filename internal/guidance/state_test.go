package guidance

import (
	"crypto/sha256"
	"encoding/hex"
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

func TestLoadRefusesDamage(t *testing.T) {
	cases := map[string]func(path string, data []byte){
		"flipped display byte": func(p string, d []byte) {
			os.WriteFile(p, []byte(strings.Replace(string(d), "Be kind.", "Be Kind.", 1)), 0o600)
		},
		"bad checksum": func(p string, d []byte) {
			i := strings.Index(string(d), `"Checksum": "`) + len(`"Checksum": "`)
			b := []byte(string(d))
			if b[i] == '0' {
				b[i] = '1'
			} else {
				b[i] = '0'
			}
			os.WriteFile(p, b, 0o600)
		},
		"non-hex raw digest": func(p string, d []byte) {
			os.WriteFile(p, []byte(strings.Replace(string(d), hx("raw"), strings.Repeat("z", 64), 1)), 0o600)
		},
		"transform 2": func(p string, d []byte) {
			os.WriteFile(p, []byte(strings.Replace(string(d), `"Transform": 1`, `"Transform": 2`, 1)), 0o600)
		},
		"truncated": func(p string, d []byte) { os.WriteFile(p, d[:len(d)/2], 0o600) },
		"damaged lifecycle": func(p string, d []byte) {
			os.WriteFile(filepath.Join(filepath.Dir(p), "lifecycle.json"), []byte("{"), 0o600)
		},
		"damaged assignments": func(p string, d []byte) {
			os.WriteFile(filepath.Join(filepath.Dir(p), "assignments.json"), []byte("{"), 0o600)
		},
		"generation beyond lifecycle": func(p string, d []byte) {
			os.WriteFile(filepath.Join(filepath.Dir(p), "lifecycle.json"), []byte(`{"k":0}`), 0o600)
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			s, state := newStore(t)
			if err := s.Save(rec("k")); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(state, "guidance", "k.json")
			d, _ := os.ReadFile(p)
			damage(p, d)
			got, err := s.Load("k")
			if err == nil || got != nil {
				t.Fatalf("want error, got %v %v", got, err)
			}
		})
	}
}

func TestLoadRefusesOversizeDisplay(t *testing.T) {
	s, _ := newStore(t)
	r := rec("k")
	r.Display = strings.Repeat("a", 2049)
	r.DisplaySHA = hx(r.Display)
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Load("k"); err == nil || got != nil {
		t.Fatalf("want error, got %v %v", got, err)
	}
}

func TestGenerationsNeverRepeat(t *testing.T) {
	s, _ := newStore(t)
	r := rec("k")
	s.Save(r)
	if r.Generation != 1 {
		t.Fatal(r.Generation)
	}
	if err := s.Revoke("k"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Load("k"); got != nil || err != nil {
		t.Fatalf("after revoke: %v %v", got, err)
	}
	r2 := rec("k")
	s.Save(r2)
	if r2.Generation != 3 {
		t.Fatalf("generation %d, want 3", r2.Generation)
	}
}

func TestAssignmentSurvivesRevoke(t *testing.T) {
	s, _ := newStore(t)
	r := rec("k")
	s.Save(r)
	s.Revoke("k")
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
	s.Save(r2)
	if r2.Generation != 4 {
		t.Fatalf("generation %d, want 4 (save, revoke, release, save)", r2.Generation)
	}
}

func TestOpenRefusesInsideRepoAndSymlinks(t *testing.T) {
	repoRoot := t.TempDir()
	if _, err := Open(filepath.Join(repoRoot, "state"), repoRoot); err == nil {
		t.Fatal("inside repo accepted")
	}
	state := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(state, "guidance")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(state, t.TempDir()); err == nil {
		t.Fatal("symlinked guidance dir accepted")
	}
	// symlink earlier on the path
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(real, link)
	if _, err := Open(link, t.TempDir()); err == nil {
		t.Fatal("symlinked state dir accepted")
	}
	// symlinked file inside the store
	s, st := newStore(t)
	os.Symlink(filepath.Join(target, "x"), filepath.Join(st, "guidance", "lifecycle.json"))
	if err := s.Save(rec("k")); err == nil {
		t.Fatal("symlinked lifecycle accepted")
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
