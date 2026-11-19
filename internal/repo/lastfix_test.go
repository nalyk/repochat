package repo

import (
	"fmt"
	"strings"
	"testing"
)

// A private key in a file with an unresolved merge conflict: the conflict
// markers git wrote into the middle of the block must not end the masking.
func TestKeyBlockMaskingSurvivesConflictMarkers(t *testing.T) {
	dir := newDir(t)
	body := strings.Split(strings.TrimSpace(fakePEM), "\n")
	conflicted := strings.Join([]string{
		body[0], body[1],
		"<<<<<<< HEAD", body[2],
		"||||||| base", body[2],
		"=======", body[3],
		">>>>>>> theirs", body[len(body)-1], "after the key",
	}, "\n") + "\n"
	write(t, dir, "deploy/notes.txt", conflicted)
	commit(t, dir, "c")
	r := open(t, dir, Options{})
	res, err := r.ReadFile(bg, "deploy/notes.txt", 1, 100, "", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	mustNotContain(t, res.Text, body[2], body[3])
	// "=======" is itself made of base64 characters and is masked with the
	// key, which errs on the safe side.
	mustContain(t, res.Text, "<<<<<<< HEAD", ">>>>>>> theirs", "after the key")
}

// A name that a listing shows must work in the git tools too, exactly as shown.
func TestGitToolsTakeNamesAsListed(t *testing.T) {
	dir := newDir(t)
	names := []string{`back\slash.txt`, "~tilde.txt", " lead.txt", "trail.txt "}
	for _, n := range names {
		write(t, dir, n, "one\n")
	}
	commit(t, dir, "first")
	for _, n := range names {
		write(t, dir, n, "one\ntwo\n")
	}
	r := open(t, dir, Options{})
	for _, n := range names {
		log, err := r.Log(bg, LogOptions{Path: n})
		if err != nil || log.Count != 1 {
			t.Errorf("git_log path=%q: %v %+v", n, err, log)
		}
		diff, err := r.Diff(bg, DiffOptions{Paths: []string{n}}, 1<<20)
		if err != nil || !strings.Contains(diff.Text, "+two") {
			t.Errorf("git_diff paths=[%q]: %v", n, err)
		}
		if _, err := r.Blame(bg, n, 1, 1, ""); err != nil {
			t.Errorf("git_blame path=%q: %v", n, err)
		}
		if _, err := r.Blame(bg, n, 1, 1, "HEAD"); err != nil {
			t.Errorf("git_blame path=%q rev=HEAD: %v", n, err)
		}
	}
	// The literal reading opens nothing new.
	for _, bad := range []string{"../x", "/etc/passwd", ".git/config", "a/../b"} {
		if _, err := r.Log(bg, LogOptions{Path: bad}); err == nil {
			t.Errorf("git_log path=%q must be refused", bad)
		}
	}
}

// A blank at either end of a name does not make a secret file an ordinary one.
func TestSecretRulesIgnoreBlanksAroundNames(t *testing.T) {
	for _, p := range []string{" id_rsa", ".env ", "deploy/ prod.pem", " .ssh /config", "secrets.yaml "} {
		if !builtinSecret(p) {
			t.Errorf("%q must be withheld by the built-in rules", p)
		}
	}
	for _, p := range []string{" main.go ", " ", "dir/  ", " /x.go"} {
		if builtinSecret(p) {
			t.Errorf("%q is an ordinary name and stays served", p)
		}
	}
}

// An outline that lists only its first definitions is not a complete outline.
func TestOutlineSaysWhenItIsCut(t *testing.T) {
	dir := newDir(t)
	var src strings.Builder
	src.WriteString("package big\n")
	for i := 0; i < maxOutlineItems+50; i++ {
		fmt.Fprintf(&src, "func F%d() {}\n", i)
	}
	write(t, dir, "big.go", src.String())
	write(t, dir, "small.go", "package big\nfunc One() {}\n")
	commit(t, dir, "c")
	r := open(t, dir, Options{})
	big, err := r.Outline(bg, "big.go")
	if err != nil || !big.Truncated {
		t.Fatalf("an outline cut at %d definitions must say so: %v %+v", maxOutlineItems, err, big)
	}
	if small, _ := r.Outline(bg, "small.go"); small.Truncated {
		t.Error("a whole outline is not cut")
	}
}
