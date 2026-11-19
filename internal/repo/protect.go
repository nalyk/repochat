package repo

import (
	"os"
	"path"
	"strings"
	"unicode"
)

// Protected paths are those whose content git or a hosting service turns
// into the execution of a program, or that decide what repochat itself
// withholds. No change set may touch one. The workspace reads .repochatignore
// from its git index, which is sound only because no change set can write a
// .repochatignore at any depth.

// protectedAnywhere are names protected as any element of a path.
var protectedAnywhere = set(".git", ".gitmodules", ".gitattributes", ".repochatignore")

// protectedRootFiles are paths protected exactly, at the root.
var protectedRootFiles = set(
	".gitlab-ci.yml", ".woodpecker.yml", ".drone.yml", ".travis.yml",
	"azure-pipelines.yml", "bitbucket-pipelines.yml", "jenkinsfile",
)

// protectedRootTrees are directories protected with everything beneath them.
var protectedRootTrees = []string{
	".github/workflows", ".gitlab/ci", ".circleci", ".buildkite", ".woodpecker", ".husky", ".githooks",
}

// protectedPath reports whether a change set must leave rel alone. Names are
// compared as a case-insensitive file system (macOS) would: case-folded, and
// without the code points HFS+ ignores in a name.
func protectedPath(rel string) bool {
	p := foldName(rel)
	segs := strings.Split(p, "/")
	for _, s := range segs {
		if protectedAnywhere[s] {
			return true
		}
	}
	if protectedRootFiles[p] {
		return true
	}
	for _, tree := range protectedRootTrees {
		if p == tree || strings.HasPrefix(p, tree+"/") {
			return true
		}
	}
	return false
}

// foldName returns the form of a name under which a case-insensitive file
// system finds it: every rune replaced by one representative of its simple
// case-folding orbit (so "ſ" and "K" join "s" and "k"), lower case, with the
// code points HFS+ ignores in names removed.
func foldName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if hfsIgnorable(r) {
			continue
		}
		b.WriteRune(foldRune(r))
	}
	return b.String()
}

// foldRune maps r to the lower case of the smallest rune in its orbit.
func foldRune(r rune) rune {
	if r < 0x80 {
		return unicode.ToLower(r)
	}
	least := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		if f < least {
			least = f
		}
	}
	return unicode.ToLower(least)
}

// hfsIgnorable lists the code points HFS+ drops when it compares names (the
// set git's own protection for .git on HFS+ uses).
func hfsIgnorable(r rune) bool {
	switch {
	case r >= 0x200c && r <= 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x206a && r <= 0x206f, r == 0xfeff:
		return true
	}
	return false
}

// laterNames are build and task definitions that something runs on the
// owner's machine as a matter of course (lower case).
var laterNames = set(
	"makefile", "gnumakefile", "justfile", "taskfile.yml", "rakefile", "package.json", "setup.py",
	"pyproject.toml", "build.rs", "cargo.toml", "cmakelists.txt", "pom.xml", "docker-compose.yml", "compose.yml",
)

// runsLater reports whether a changed file is one that will be executed on
// the owner's machine: anything with an execute bit, or a build or task
// definition.
func runsLater(rel string, mode os.FileMode) bool {
	if mode&0o111 != 0 {
		return true
	}
	base := strings.ToLower(path.Base(rel))
	if laterNames[base] {
		return true
	}
	for _, ext := range []string{".mk", ".sh", ".bash", ".ps1", ".csproj"} {
		if strings.HasSuffix(base, ext) {
			return true
		}
	}
	return strings.HasPrefix(base, "dockerfile") || strings.HasPrefix(base, "build.gradle")
}
