package repo

import (
	"os"
	"testing"
)

func TestProtectedPathPatterns(t *testing.T) {
	protected := []string{
		".git", ".git/config", "sub/.git/hooks/pre-commit", ".GIT/config", "a/.Git",
		".github/workflows/ci.yml", ".github/workflows", ".GitHub/Workflows/x.yml", ".github/WORKFLOWS/deep/x.yaml",
		".gitlab-ci.yml", ".GitLab-CI.yml",
		".gitlab/ci/build.yml", ".gitlab/CI/x/y.yml",
		".circleci/config.yml", ".CircleCI/config.yml",
		".buildkite/pipeline.yml",
		".woodpecker/build.yml", ".woodpecker.yml",
		".drone.yml", ".travis.yml", "azure-pipelines.yml", "bitbucket-pipelines.yml",
		"Jenkinsfile", "JENKINSFILE",
		".husky/pre-commit", ".husky", ".githooks/pre-push",
		".gitmodules", "sub/.gitmodules", ".gitattributes", "a/b/.gitattributes", ".GitAttributes",
		".repochatignore", "deep/dir/.repochatignore", ".RepoChatIgnore",
		// Characters a case-insensitive or HFS+ file system treats as the
		// same name: the long s folds to s; a zero-width joiner is ignored.
		".hu\u017fky/pre-commit", ".git\u200c/config", ".gith\u200dub/workflows/x.yml",
	}
	for _, p := range protected {
		if !protectedPath(p) {
			t.Errorf("protectedPath(%q) = false", p)
		}
	}
	allowed := []string{
		"README.md", ".github/CODEOWNERS", ".github/ISSUE_TEMPLATE/bug.md", "docs/.github/workflows/x.yml",
		"sub/.gitlab-ci.yml", "sub/Jenkinsfile", ".gitlab/issue_templates/x.md", ".gitignore",
		"src/husky/x", "my.gitattributes", ".gitattributes.bak", "workflows/ci.yml", ".travis.yml.example",
	}
	for _, p := range allowed {
		if protectedPath(p) {
			t.Errorf("protectedPath(%q) = true", p)
		}
	}
}

// The workspace reads .repochatignore from its git index; that is only sound
// while no change set can write one, at any depth.
func TestRepochatignoreStaysProtectedAtAnyDepth(t *testing.T) {
	for _, p := range []string{".repochatignore", "a/.repochatignore", "a/b/c/.REPOCHATIGNORE", ".repochatignore/x"} {
		if !protectedPath(p) {
			t.Errorf("protectedPath(%q) = false", p)
		}
	}
}

func TestRunsLater(t *testing.T) {
	later := []struct {
		path string
		mode os.FileMode
	}{
		{"Makefile", 0o644}, {"sub/GNUmakefile", 0o644}, {"justfile", 0o644}, {"Taskfile.yml", 0o644},
		{"Rakefile", 0o644}, {"web/package.json", 0o644}, {"setup.py", 0o644}, {"pyproject.toml", 0o644},
		{"build.rs", 0o644}, {"Cargo.toml", 0o644}, {"CMakeLists.txt", 0o644}, {"pom.xml", 0o644},
		{"docker-compose.yml", 0o644}, {"compose.yml", 0o644}, {"rules.mk", 0o644}, {"x/run.sh", 0o644},
		{"a.bash", 0o644}, {"b.ps1", 0o644}, {"Dockerfile", 0o644}, {"Dockerfile.dev", 0o644},
		{"build.gradle", 0o644}, {"build.gradle.kts", 0o644}, {"App.csproj", 0o644},
		{"MAKEFILE", 0o644}, {"tools/thing", 0o755}, {"notes.txt", 0o744},
	}
	for _, c := range later {
		if !runsLater(c.path, c.mode) {
			t.Errorf("runsLater(%q, %v) = false", c.path, c.mode)
		}
	}
	for _, p := range []string{"README.md", "main.go", "package-lock.json", "Makefile.txt/x", "docs/compose.md"} {
		if runsLater(p, 0o644) {
			t.Errorf("runsLater(%q) = true", p)
		}
	}
}
