# Write Mode, Project 1 (Edits in a Session Workspace) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** With `repochat --write`, the model can edit files in a git worktree on a side branch, every change is an undoable commit there, and one explicit call merges the result into the owner's branch.

**Architecture:** A session is a linked git worktree under repochat's state directory, opened as a second `*repo.Repo`, so the 13 read tools serve it unchanged. Edits are change sets addressed by line anchors (keyed hashes of the masked line); a change set is validated completely in memory, then written through the no-follow file access and committed with git plumbing. `repo.Sessions` owns the lifecycle and is the only writer; the tool catalogue asks it which repository to serve.

**Tech Stack:** Go standard library only, git ≥ 2.25 through the existing hardened runner (`Repo.git`, `gitBaseConfig`, `gitEnv`), `os.Root` for file access.

**Spec:** `docs/superpowers/specs/2026-11-19-write-mode-edits-design.md`. Read it first; this plan argues from it.

## Global Constraints

- Go standard library only; zero third-party modules. One static binary, Linux and macOS.
- `go.mod` moves to `go 1.25` (needed for `os.Root.Rename` and `os.Root.MkdirAll`). README build requirement changes accordingly. Runtime requirements do not change.
- git ≥ 2.25. No git feature newer than 2.25 (no `merge-tree --write-tree`, no `git init -b`, no `worktree add --orphan`).
- Without `--write`: the same 13 tools, byte-identical output, every existing test unchanged and green.
- Every git command goes through `Repo.git`/`Repo.gitRaw`. No hook, filter, merge driver, signing program or any other repository-configured program runs at any step.
- Fail closed: when the policy, an anchor, a path check or a git answer is uncertain, the change set is refused.
- Test first: each behaviour gets a test that is seen to fail before the code exists. Do not weaken an existing test.
- Work on branch `feat/write-mode` (from `design/write-mode`). Merge only through a PR with green CI on the exact head commit.
- Code, CLI text and commit messages in English; `CHANGELOG.md` in Romanian; `README.md` English with `README.ro.md` mirroring it.
- Commit messages end with the two attribution lines used throughout this repository's history.
- Write mode ships labelled **experimental** until the adversarial review in Task 11 is done.

## Review Focus

Conditions the spec implies but does not spell out, most likely first. Each has a test in the task named.

1. The owner commits in their own checkout while a session is open → `finish_session` refuses and names `update_session`; nothing in the owner's checkout changes. (Task 8)
2. repochat is killed in the middle of an apply → on the next start the workspace has changes that belong to no checkpoint → resume restores the workspace to its last checkpoint before serving. (Task 4)
3. The owner deletes the workspace directory or the session branch by hand → resume says so and offers `--new-session`; it does not crash and does not recreate anything silently. (Task 4)
4. CRLF files, files without a final newline, empty files, and an insert into an empty file → line endings and the final-newline state are preserved exactly. (Task 5)
5. A case-insensitive file system (macOS): `.GitHub/workflows/x.yml` is protected like `.github/workflows/x.yml`, and creating `readme.md` beside `README.md` is refused. (Task 5)

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/mcp/schema.go` (modify) | new `Object` parameter kind carrying nested JSON |
| `internal/mcp/server.go` (modify) | `Tool.Write`, `Tool.Destructive` → annotations |
| `internal/repo/policy.go`, `fileset.go` (modify) | second ignore source (`Options.ExtraIgnoreFile`) |
| `internal/repo/anchor.go` (create) | anchor and file-tag computation, `shownLines` |
| `internal/repo/read.go`, `search.go` (modify) | anchors in output when `Options.AnchorKey` is set |
| `internal/repo/fswrite.go` (create) | no-follow atomic write, remove, mkdir on `opener` |
| `internal/repo/session.go` (create) | `Sessions`, `Session`, state file, lock, start/resume/sync |
| `internal/repo/protect.go` (create) | protected paths, run-later paths |
| `internal/repo/changeset.go` (create) | change-set types, parsing, validation → `Plan` |
| `internal/repo/apply.go` (create) | apply, rollback, checkpoint commit, plan store |
| `internal/repo/checkpoint.go` (create) | list, undo |
| `internal/repo/finish.go` (create) | status, update, finish, abandon |
| `internal/tools/tools.go` (modify) | catalogue serves `Source.Repo()` |
| `internal/tools/write.go` (create) | the nine write-mode tools |
| `internal/cli/session.go`, `serve.go`, `run.go`, `cli.go` (modify), `sessioncmd.go` (create) | flags, banner, `repochat session` |

---

### Task 1: Protocol engine: write annotations and nested arguments

**Files:**
- Modify: `internal/mcp/server.go` (the `Tool` struct and the annotations map near line 220), `internal/mcp/schema.go`
- Test: `internal/mcp/write_test.go` (create)

**Interfaces:**
- Produces:
  - `Tool.Write bool`, `Tool.Destructive bool`
  - `const Object Kind` (after `StringList`); `Param.Raw map[string]any` (the JSON Schema of the value, advertised as is); `Param.MaxBytes int` (size cap of the encoded value, default 1 MiB)
  - `func (a Args) Raw(k string) json.RawMessage` (the value re-encoded; nil when absent)

- [ ] **Step 1: Write the failing tests**

```go
func TestWriteToolsAreAnnotatedAsWrites(t *testing.T)
// tools/list over three tools: {}, {Write:true}, {Write:true, Destructive:true}
// → readOnlyHint true/false/false, destructiveHint false/false/true,
//   idempotentHint true/false/false, openWorldHint false for all.

func TestObjectArgumentIsAdvertisedAndBounded(t *testing.T)
// Param{Name:"changes", Kind:Object, Required:true, MaxBytes:64, Raw: map[string]any{"type":"array"}}
// → Schema.JSON()["properties"]["changes"] equals Raw plus "description";
// → Validate with a 10-byte array succeeds and Args.Raw("changes") round-trips it;
// → Validate with a value encoding to more than 64 bytes fails with "changes: too large";
// → Validate with the member missing fails (required).
```

- [ ] **Step 2:** Run `go test -count=1 -run 'WriteTools|ObjectArgument' ./internal/mcp/`. Expected: FAIL (unknown fields).
- [ ] **Step 3:** Implement. `Validate` keeps an `Object` value as `json.RawMessage` in `Args`; it checks only presence and size. The handler owns the structure.
- [ ] **Step 4:** Run `go test -count=1 ./internal/mcp/`. Expected: PASS, including every existing test.
- [ ] **Step 5:** Commit: `Engine: write annotations and nested JSON arguments`.

---

### Task 2: A second source of ignore rules

The workspace is built from a commit, so the owner's uncommitted `.repochatignore` must still apply there. A path withheld by either file is withheld.

**Files:**
- Modify: `internal/repo/repo.go` (`Options`), `internal/repo/policy.go`, `internal/repo/fileset.go` (`buildSnapshot`)
- Test: `internal/repo/extraignore_test.go` (create)

**Interfaces:**
- Produces: `Options.ExtraIgnoreFile string`: absolute path of a second rules file, read at every snapshot build; missing file = no extra rules; unreadable for any other reason = snapshot build fails.
- `NewPolicy(ignoreFile string, deny, allow []*Glob)` keeps its signature; add `func (p *Policy) AddIgnore(rules string) error`, which gives the policy a second `ignoreOracle` and second lenient rule list. `decide`, `lenient` and `Prime` consult both; `hash` covers both texts.

- [ ] **Step 1: Failing tests**

```go
func TestEitherIgnoreFileWithholds(t *testing.T)
// repo with tracked a.txt, b.txt, c.txt; its .repochatignore = "a.txt";
// extra file = "b.txt" → Check("a.txt") and Check("b.txt") == ReasonIgnoreFile, Check("c.txt") == "".

func TestExtraIgnoreCannotReinclude(t *testing.T)
// .repochatignore = "*.txt"; extra = "!a.txt" → a.txt stays withheld.

func TestExtraIgnoreIsRereadOnChange(t *testing.T)
// rewrite the extra file, wait past snapshotTTL → new rules apply.

func TestUnreadableExtraIgnoreFailsClosed(t *testing.T)
// extra file is a directory → Snapshot returns an error.
```

- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement. The extra oracle is adopted across snapshots the same way `r.oracle` is (reuse while the rule text is equal).
- [ ] **Step 4:** `go test -count=1 ./internal/repo/`. Expected: PASS.
- [ ] **Step 5:** Commit: `Policy: a second ignore source, withheld by either`.

---

### Task 3: Anchors

**Files:**
- Create: `internal/repo/anchor.go`
- Modify: `internal/repo/repo.go` (`Options`), `internal/repo/read.go` (`render`, `ReadResult`), `internal/repo/search.go`
- Test: `internal/repo/anchor_test.go` (create)

**Interfaces:**
- Produces:
  - `Options.AnchorKey []byte`: when non-empty, working-tree reads and searches carry anchors.
  - `func anchorOf(key []byte, shown string) string`: first 15 bits of HMAC-SHA256(key, shown) as 3 characters of `abcdefghijklmnopqrstuvwxyz234567`.
  - `type shownLine struct { Text string; Locked bool; Long bool }`: `Text` is the line as the model is shown it (masked, sanitised, **not** truncated); `Locked` when the redactor masked anything on it (count > 0 from `RedactState.Masked`, including continuation lines of a multi-line secret); `Long` when it exceeds the display limit and is shown cut.
  - `func (r *Repo) shownLines(ctx context.Context, content []byte) ([]shownLine, error)`: the single source of truth for what the model saw. `render` is changed to produce its lines through the same per-line function, so the two cannot drift.
  - `func fileTagOf(key []byte, lines []shownLine) string`: 8 characters, same alphabet, over all shown lines.
  - `ReadResult.FileTag string`, set when the scan reached the end of the file.
  - Output format in write mode: `<line>:<anchor>|<text>` (line number right-aligned as today). Read mode output is byte-identical to today.

- [ ] **Step 1: Failing tests**

```go
func TestAnchorsAppearOnlyWithAKey(t *testing.T)
// same file read with and without AnchorKey: without → output equals the golden of today;
// with → every content line matches ^\s*\d+:[a-z2-7]{3}\|

func TestAnchorDoesNotDependOnTheSecret(t *testing.T)
// two files identical except the secret value on line 2 (both masked to the same text)
// → line 2 has the same anchor in both, and the same FileTag.

func TestAnchorChangesWithTheKeyAndTheText(t *testing.T)

func TestLinesWithMaskedValuesAreLocked(t *testing.T)
// shownLines over: plain line, `password = "…real…"`, a PEM private key block (5 lines)
// → Locked == false, true, and true for every line of the block.

func TestNoRedactLocksNothing(t *testing.T)   // Options.NoRedact

func TestHistoryReadsCarryNoAnchors(t *testing.T)
// ReadFile(..., rev: "HEAD") with AnchorKey set → no anchors: only the workspace can be edited.

func TestSearchResultsCarryAnchors(t *testing.T)
// a search hit on line 7 shows the same anchor that ReadFile shows for line 7.
```

- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test -count=1 ./internal/repo/ ./internal/tools/`. Expected: PASS.
- [ ] **Step 5:** Commit: `Anchors: keyed hashes of the line as shown`.

---

### Task 4: Sessions: start, resume, sync

**Files:**
- Create: `internal/repo/session.go`, `internal/repo/fswrite.go`
- Modify: `go.mod` (`go 1.25`), `internal/repo/repo.go` (workspace git prefix)
- Test: `internal/repo/session_test.go`, `internal/repo/fswrite_test.go` (create)

**Interfaces:**
- Produces:

```go
type SessionOptions struct {
    StateDir string        // <repochat state>/sessions
    Repo     Options       // the exposure options of this run
    Notify   func(string)  // one-line notices for the owner's terminal; may be nil
}
func NewSessions(owner *Repo, o SessionOptions) (*Sessions, error)

// Serve returns the repository the read tools must use right now:
// the workspace when a session is open, otherwise the owner's checkout.
func (s *Sessions) Serve(ctx context.Context) (*Repo, error)
func (s *Sessions) Start(ctx context.Context, fresh bool) (*SessionInfo, error) // fresh: leave an open session's branch and start another
func (s *Sessions) Current(ctx context.Context) (*SessionInfo, error)           // ErrNoSession when none
func (s *Sessions) Close() error

type SessionInfo struct {
    ID, Branch, StartBranch, Base, Head, Workspace string
    Created time.Time
}
var ErrNoSession = errors.New("no session is open: call start_session")

// on opener (fswrite.go); every call refuses a symbolic link at any path element
func (o *opener) WriteAtomic(rel string, data []byte, mode os.FileMode) error // temp file in the same directory, then rename
func (o *opener) Remove(rel string) error                                     // removes the file, then empty parent directories
```

- Decisions:
  - Layout: `<StateDir>/<owner.ID>/session.json` (0600), `<StateDir>/<owner.ID>/lock`, workspace at `<StateDir>/<owner.ID>/<id>/w`. `id` = 8 random hex characters. Branch `repochat/<YYYY-MM-DD>-<id>`.
  - `session.json`: `id, branch, start_branch, base, workspace, created, anchor_key (base64, 32 random bytes), last_update` (commit of the latest update, empty at start).
  - Every exported `Sessions` method takes an exclusive `flock` on `lock` and re-reads `session.json` first, so the terminal command (Task 10) and the serving process never act on stale state.
  - Start refuses when the owner's `HEAD` is detached or unborn (a session needs a branch to merge back into).
  - Creation: `git worktree add --no-checkout -b <branch> <workspace> <base>`; then **sync**; then open the workspace with `repo.Open(ctx, workspace, opts)` where `opts` = `SessionOptions.Repo` plus `ExtraIgnoreFile = <owner root>/.repochatignore` and `AnchorKey`.
  - **Sync** (run at creation, at resume, and after every update): list the tracked paths of the workspace's `HEAD`; ask the policy (both ignore sources, primed in one batch) which are withheld; write the worktree's `info/sparse-checkout` as `/*` followed by one escaped `!/<path>` line per withheld path; run `read-tree -mu HEAD` with `-c core.sparseCheckout=true`. Then verify on disk that no withheld tracked path exists in the workspace; if one does, remove the workspace and fail. Every later git command in the workspace carries `-c core.sparseCheckout=true` (add it to the workspace `Repo`'s `gitPre`).
  - Resume, in order: workspace directory or branch missing → error naming `--new-session`, nothing recreated; workspace differs from its `HEAD` (changes that belong to no checkpoint) → restore tracked files from `HEAD` and delete untracked non-ignored files, with a `Notify` line; then sync.
  - Uncommitted changes in the owner's checkout at start → `Notify`: they are not part of the session.

- [ ] **Step 1: Failing tests**

```go
func TestWriteAtomicRefusesSymlinks(t *testing.T)
// target is a symlink; a parent directory is a symlink; a parent is swapped for a
// symlink between two writes → error each time, and the link's target is byte-identical.
func TestWriteAtomicLeavesNoTempFileOnFailure(t *testing.T)
func TestRemovePrunesEmptyParents(t *testing.T)

func TestStartCreatesWorktreeOnASideBranch(t *testing.T)
// Workspace is outside owner.Root; branch matches ^repochat/\d{4}-\d{2}-\d{2}-[0-9a-f]{8}$;
// base == owner HEAD; owner's `git status --porcelain` is unchanged; Serve() returns a Repo
// whose Root is the workspace.
func TestWithheldTrackedFilesAreAbsentFromTheWorkspace(t *testing.T)
// tracked .env (built-in rule), secret/x.txt (.repochatignore, committed) and
// later.txt (withheld only by the owner's UNCOMMITTED .repochatignore):
// none exists on disk in the workspace; `git status --porcelain` there is empty
// (their absence is not a deletion); `git ls-files` there still lists them.
func TestPathsWithPatternCharactersAreWithheldExactly(t *testing.T)
// tracked withheld files named "a*b.key", "!x.pem", " lead.key", "t#.key", "b[1].key" and
// a non-withheld "ab.txt", "x.txt" → exactly the withheld ones are absent.
func TestResumeKeepsBranchAnchorsAndCheckpoints(t *testing.T)
// second NewSessions on the same state dir → same ID, same AnchorKey (same anchors).
func TestResumeDropsFilesThatBecameWithheld(t *testing.T)
// resume with an added Deny pattern → the file is gone from the workspace before Serve returns.
func TestResumeRestoresAnInterruptedApply(t *testing.T)            // Review Focus 2
// modify a tracked file and add an untracked one in the workspace behind the session's back
// → after resume both are gone/restored and a notice was sent.
func TestResumeWithMissingWorkspaceOrBranchSaysSo(t *testing.T)    // Review Focus 3
// delete the directory; separately delete the branch → error contains "--new-session";
// no worktree and no branch were created.
func TestFreshStartLeavesTheOldBranch(t *testing.T)
func TestStartRefusesDetachedHead(t *testing.T)
func TestNoRepositoryHookRunsOnStart(t *testing.T)
// post-checkout hook and a smudge filter that touch a marker file → marker absent.
```

- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test -count=1 ./internal/repo/`. Expected: PASS.
- [ ] **Step 5:** Commit: `Sessions: a worktree on a side branch, synced with the exposure policy`.

---

### Task 5: Change sets: parsing and validation

Nothing is written in this task. The output is a `Plan` holding the complete new content of every touched file.

**Files:**
- Create: `internal/repo/changeset.go`, `internal/repo/protect.go`
- Test: `internal/repo/changeset_test.go`, `internal/repo/protect_test.go` (create)

**Interfaces:**
- Consumes: `shownLines`, `anchorOf`, `fileTagOf` (Task 3); `Sessions`, `opener` (Task 4).
- Produces:

```go
type Change struct {
    Op     string `json:"op"`      // replace | insert | delete | create | remove | move
    Path   string `json:"path"`
    From   string `json:"from"`    // anchor "<line>:<hash>": first line of a range (replace, delete)
    To     string `json:"to"`      // anchor: last line of the range; empty = same as From
    At     string `json:"at"`      // insert: an anchor, or "start", or "end"
    Where  string `json:"where"`   // insert at an anchor: "before" | "after" (default "after")
    Text   string `json:"text"`    // new lines, "\n"-separated; one trailing "\n" is ignored
    ToPath string `json:"to_path"` // move
    FileTag string `json:"file_tag"` // remove: the FileTag the model was shown
}
func ParseChanges(raw json.RawMessage) ([]Change, error) // unknown members are an error

type Plan struct {
    ID       string            // 16 random hex characters
    Files    []PlannedFile     // sorted by path
    Diff     string            // unified-style, built from the operations with 3 context lines, old lines as shown (masked)
    Warnings []string
    Added, Removed int         // line counts
}
type PlannedFile struct {
    Path    string
    Before  []byte      // nil when the file did not exist
    After   []byte      // nil when the file is removed
    Regions [][2]int    // line ranges of After that changed, for fresh anchors
}
func (s *Sessions) Propose(ctx context.Context, changes []Change) (*Plan, error)

type ChangeError struct {
    Path, Reason string
    Fresh string // for a stale anchor: the current lines around it, with fresh anchors
}
func (e *ChangeError) Error() string

func protectedPath(rel string) bool // protect.go
func runsLater(rel string, mode os.FileMode) bool
```

- Decisions (each is a refusal of the whole change set unless marked warning):
  - Limits: 500 operations, 100 files, 1 MiB of new text in total, 2 MiB per resulting file.
  - Anchors refer to the file before the change set. Ranges and insertion points in one file may not overlap or touch the same line twice. Operations are applied bottom-up per file.
  - Stale anchor (line out of range, or hash differs): `ChangeError.Fresh` carries lines `n-3 … n+3` with fresh anchors.
  - Any `Locked` line inside a replace or delete range → refused: "line N holds a value you were not shown; the owner has to change it by hand". Insert beside it is allowed. The same refusal, with its own wording, for a `Long` line (shown cut).
  - `create`: the path must not exist, also not under another case (compare case-folded against the snapshot). `remove`: `FileTag` must equal the current tag. `move`: target must not exist; content unchanged. A change set may not create or move onto a path it removes.
  - Path checks, for every path including `ToPath`: `CheckPath`; `protectedPath`; policy `Check` must return `""` for the path (for a new path too); no path element is a symbolic link or a submodule (`Lstat` through `opener`); `git check-attr -z --stdin filter` in the workspace must answer `unspecified` for it (one batch for the whole change set).
  - `protectedPath`, case-insensitive: any element equal to `.git`; `.github/workflows/**`; `.gitlab-ci.yml`; `.gitlab/ci/**`; `.circleci/**`; `.buildkite/**`; `.woodpecker/**`; `.woodpecker.yml`; `.drone.yml`; `.travis.yml`; `azure-pipelines.yml`; `bitbucket-pipelines.yml`; `Jenkinsfile`; `.husky/**`; `.githooks/**`; and at any depth `.gitmodules`, `.gitattributes`, `.repochatignore`.
  - Content: must be UTF-8 without NUL (before and after). The file's line ending (`\n` or `\r\n`, by majority of the existing lines; `\n` for a new file) is applied to new lines. The final-newline state of an existing file is preserved; a new file ends with a newline.
  - After content containing a conflict-marker line (`<<<<<<< `, `=======`, `>>>>>>> ` at line start) that the Before content did not contain → refused.
  - Syntax gate, "do not make it worse": a `.go` file that parsed before (or is new) must parse after (`go/parser.ParseFile`); a `.json` file that was valid before (or is new, except `tsconfig*.json`, `jsconfig*.json` and files under `.vscode/`) must be valid after (`json.Valid`). The error carries the parser's first message with its line.
  - Warning: a `.go` file that `go/format.Source` would change.
  - New text is ordinary text even when it spells a masking placeholder.

- [ ] **Step 1: Failing tests** (table-driven; one row per rule above). The names:

```go
func TestProposeBuildsTheNewContent(t *testing.T)       // replace, insert before/after/start/end, delete, create, remove, move in one set over three files
func TestProposeWritesNothing(t *testing.T)             // workspace tree hash and `git status` identical before and after
func TestStaleAnchorRefusesEverythingAndReturnsFreshLines(t *testing.T)
func TestOverlappingRangesAreRefused(t *testing.T)
func TestLockedLinesCannotBeReplacedOrDeleted(t *testing.T) // single line, inside a range, PEM block; insert beside is accepted
func TestGuessingASecretLearnsNothing(t *testing.T)
// create a file whose line 1 is a guessed secret assignment, propose, read it back:
// its anchor equals the anchor of the real secret line (both masked alike), for a right and a wrong guess.
func TestPlaceholderSpellingIsOrdinaryText(t *testing.T) // a file containing "[REDACTED:kind]" literally stays editable
func TestProtectedPathsAreRefused(t *testing.T)          // every pattern, plus ".GitHub/Workflows/x.yml" (Review Focus 5)
func TestCreateBesideACaseVariantIsRefused(t *testing.T) // README.md exists → create readme.md refused (Review Focus 5)
func TestWithheldPathsCannotBeTouchedOrCreated(t *testing.T) // .env, a .repochatignore path, a --deny path, as path and as to_path
func TestSymlinkAndSubmodulePathsAreRefused(t *testing.T)
func TestFilteredPathsAreRefused(t *testing.T)           // .gitattributes: "*.bin filter=lfs" → refused; the filter program never ran
func TestSyntaxGateDoesNotMakeItWorse(t *testing.T)      // go: valid→invalid refused, invalid→invalid accepted; json likewise; tsconfig.json with comments accepted
func TestGofmtDifferenceIsAWarning(t *testing.T)
func TestLineEndingsAndFinalNewlineSurvive(t *testing.T) // CRLF file, file without final newline, empty file, insert into an empty file (Review Focus 4)
func TestLimitsAreEnforced(t *testing.T)
func TestRemoveNeedsTheCurrentFileTag(t *testing.T)
func TestConflictMarkersAndNulAreRefused(t *testing.T)
func FuzzParseChanges(f *testing.F)                      // never panics; add to `make fuzz`
```

- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test -count=1 ./internal/repo/`. Expected: PASS.
- [ ] **Step 5:** Commit: `Change sets: complete validation before anything is written`.

---

### Task 6: Applying and checkpoints

**Files:**
- Create: `internal/repo/apply.go`
- Test: `internal/repo/apply_test.go` (create)

**Interfaces:**
- Consumes: `Plan`, `Propose` (Task 5); `WriteAtomic`, `Remove` (Task 4).
- Produces:

```go
type Checkpoint struct {
    ID      string    // first 12 hex characters of the commit
    Kind    string    // start | change | undo | update
    Message string
    Time    time.Time
    Files   []string
    Added, Removed int
}
type Applied struct {
    Checkpoint Checkpoint
    Fresh      string   // touched regions ±3 lines with fresh anchors, per file
    Warnings   []string
}
// Exactly one of planID and changes is given.
func (s *Sessions) Apply(ctx context.Context, planID string, changes []Change, message string) (*Applied, error)
```

- Decisions:
  - `Propose` keeps its plans in memory: at most 32, each for 30 minutes. `Apply(planID)` re-runs the validation of the stored changes against the current files; it succeeds only if every `Before` is byte-identical to the stored one. A plan already applied returns the same `Applied` again without writing.
  - One mutex serialises every write in `Sessions`.
  - Writing: keep each `Before` in memory; write files in path order; on any error write every `Before` back (and remove created files), then return the error. A test seam `applyFault func(i int) error` (unexported field, nil in production) injects a failure before the i-th file.
  - Existing files keep their mode; new files are `0644`.
  - Commit with plumbing, in the workspace: `update-index --add --remove -z --stdin` for exactly the touched paths; `write-tree`; `commit-tree <tree> -p HEAD --no-gpg-sign -F -`; `update-ref refs/heads/<branch> <new> <old>`. All with `-c commit.gpgsign=false -c user.name=… -c user.email=…`.
  - Identity: the owner's `user.name`/`user.email` as git reports them for the owner's repository; if either is empty, `repochat` / `repochat@localhost`.
  - Message: the given one (first line cut to 100 characters, default `Changes through repochat`), then a blank line and the trailers `Made-through: repochat` and `Repochat-Kind: change`.
  - After the commit the workspace `Repo`'s snapshot is dropped so the next read sees the new files.

- [ ] **Step 1: Failing tests**

```go
func TestApplyWritesCommitsAndReturnsFreshAnchors(t *testing.T)
// three files changed in one call → one new commit on the session branch whose tree
// contains exactly those changes; `git status --porcelain` in the workspace is empty;
// Fresh contains the new lines with anchors that ReadFile confirms; the owner's
// checkout and branch are untouched.
func TestApplyIsAllOrNothing(t *testing.T)
// fault before file 2 of 3 → error; workspace tree hash, HEAD and `git status` as before.
func TestApplyingAPlanTwiceGivesTheSameCheckpoint(t *testing.T)
func TestPlanIsRefusedWhenAFileChangedSince(t *testing.T)
func TestPlansExpire(t *testing.T)                    // clock injected through an unexported `now func() time.Time`
func TestConcurrentAppliesDoNotInterleave(t *testing.T) // 8 goroutines, disjoint files → 8 commits in a line, every tree consistent; run with -race
func TestWithheldFilesSurviveACommit(t *testing.T)
// after an apply, `git ls-tree -r HEAD` in the workspace still lists the withheld tracked files with their original blob ids.
func TestCommitRunsNoHookAndIsUnsigned(t *testing.T)
// pre-commit, commit-msg, post-commit hooks and commit.gpgsign=true with gpg.program=<marker script> → no marker, commit exists.
func TestCommitCarriesIdentityAndTrailers(t *testing.T)
func TestSymlinkSwappedInDuringApplyIsNotFollowed(t *testing.T)
// between validation and write (use applyFault as the hook) replace a parent directory with a
// symlink to a directory outside → the outside directory is unchanged and the apply fails cleanly.
```

- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test -race -count=1 ./internal/repo/`. Expected: PASS.
- [ ] **Step 5:** Commit: `Apply: all or nothing, one checkpoint per change set`.

---

### Task 7: Listing checkpoints and undoing

**Files:**
- Create: `internal/repo/checkpoint.go`
- Test: `internal/repo/checkpoint_test.go` (create)

**Interfaces:**
- Consumes: the write path of Task 6 (undo goes through the same file writing and the same commit code, with `Repochat-Kind: undo`).
- Produces:

```go
func (s *Sessions) Checkpoints(ctx context.Context) ([]Checkpoint, error) // newest first; the last entry is the start
func (s *Sessions) UndoTo(ctx context.Context, id string) (*Applied, error) // id: a checkpoint ID or a unique prefix of ≥ 7 characters
```

- Decisions: checkpoints are the first-parent commits from `base` to `HEAD`; `Kind` comes from the `Repochat-Kind` trailer. Undo computes the paths that differ between `HEAD` and the target (`diff-tree -r -z --name-status --no-renames`), writes the target's blobs (or removes), and commits. The target must be on the first-parent chain at or after `last_update` (or `base`); otherwise: "cannot undo past the update from <branch>". Undo to `HEAD` is a no-op that returns the current checkpoint.

- [ ] **Step 1: Failing tests**

```go
func TestCheckpointsListEveryChangeNewestFirst(t *testing.T)
func TestUndoRestoresTheTreeAsANewCheckpoint(t *testing.T)
// after three applies, UndoTo(first) → HEAD^{tree} equals first^{tree}; four change commits + one undo
// are all still in history; workspace `git status` empty.
func TestUndoOfACreateRemovesTheFileAndOfARemoveBringsItBack(t *testing.T)
func TestUndoCannotCrossAnUpdate(t *testing.T)   // uses Update from Task 8; mark t.Skip until then, remove the skip in Task 8
func TestUndoToUnknownOrForeignCommitIsRefused(t *testing.T) // a commit of the owner's branch that is not in the session
```

- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test -count=1 ./internal/repo/`. Expected: PASS.
- [ ] **Step 5:** Commit: `Checkpoints: list and undo, never discarding history`.

---

### Task 8: Status, update, finish, abandon

**Files:**
- Create: `internal/repo/finish.go`
- Test: `internal/repo/finish_test.go` (create)

**Interfaces:**
- Produces:

```go
type Status struct {
    Info        SessionInfo
    Ahead       int       // checkpoints since the start
    Files       []string  // paths that differ from the owner's branch tip
    OwnerMoved  bool      // the start branch has commits the session lacks
    OwnerBranch string    // the branch the owner's checkout is on now ("" when detached)
    RunsLater   []string  // changed files that get executed on the owner's machine
}
func (s *Sessions) Status(ctx context.Context) (*Status, error)
func (s *Sessions) Update(ctx context.Context) (*Applied, error)
type Finished struct {
    Commit    string   // the commit the owner's branch is on now
    Squashed  bool
    Files     []string
    RunsLater []string
    Notes     []string // unsigned commits; hooks that did not run
}
func (s *Sessions) Finish(ctx context.Context, squash bool, message string) (*Finished, error)
func (s *Sessions) Abandon(ctx context.Context, deleteBranch bool) error
```

- Decisions:
  - `runsLater`: mode with any execute bit, or base name (case-insensitive) one of `Makefile`, `GNUmakefile`, `justfile`, `Taskfile.yml`, `Rakefile`, `package.json`, `setup.py`, `pyproject.toml`, `build.rs`, `Cargo.toml`, `CMakeLists.txt`, `pom.xml`, `docker-compose.yml`, `compose.yml`, or matching `*.mk`, `*.sh`, `*.bash`, `*.ps1`, `Dockerfile*`, `build.gradle*`, `*.csproj`.
  - **Update**: refused when any `merge.<name>.driver` is configured for the repository (`config --get-regexp`), since a merge driver is a program. Otherwise in the workspace: `merge --no-ff --no-commit --no-verify-signatures <start branch>` with `-c rerere.enabled=false -c commit.gpgsign=false`. On conflict or any failure: `merge --abort`, verify the workspace equals `HEAD`, report the conflicting paths. On success: commit with plumbing (two parents, `Repochat-Kind: update`), record `last_update`, then **sync** (Task 4), so newly arrived withheld files never stay on disk. Nothing to merge → no-op.
  - **Finish**, checks in this order, each a refusal that changes nothing: no checkpoints beyond the start; the owner's checkout is not on `StartBranch`; `commit.gpgsign` is true in the owner's effective configuration (message explains `git merge --ff-only <branch>` by hand); the start branch tip is not an ancestor of the session head (message names `update_session`).
  - Then: for `squash`, create one commit with plumbing (tree of the session head, parent = start branch tip, message from the argument, trailer `Made-through: repochat`); the commit to land is that one, otherwise the session head. In the owner's checkout: `merge --ff-only <commit>`. If git refuses because local changes would be overwritten, report those paths; nothing changed.
  - After a successful merge: `worktree remove --force`, delete the session branch, delete `session.json`. A failure in this cleanup is a `Note`, not an error: the merge has happened.
  - `Notes`: "commits are unsigned"; and, when the owner's hooks directory contains an executable `post-merge` or `post-checkout`, "these hooks did not run".
  - **Abandon**: `worktree remove --force`, delete `session.json`; the branch is deleted only with `deleteBranch`.
  - After Finish or Abandon, `Serve` returns the owner's repository and every write method returns `ErrNoSession`.

- [ ] **Step 1: Failing tests**

```go
func TestStatusReportsAheadFilesAndRunsLater(t *testing.T)
func TestFinishFastForwardsTheOwnersBranch(t *testing.T)
// two applies → Finish(false,"") → owner's branch == former session head, owner's files changed
// on disk, owner's `git status --porcelain` empty, workspace directory and session branch gone.
func TestFinishSquashLandsOneCommit(t *testing.T)
func TestFinishRefusesWhenTheOwnersBranchMoved(t *testing.T)        // Review Focus 1: message contains "update_session"; owner's HEAD, index and files unchanged
func TestFinishRefusesOnAnotherBranchOrDetachedHead(t *testing.T)
func TestFinishRefusesToOverwriteUncommittedChanges(t *testing.T)   // owner edited a file the session also changed
func TestFinishKeepsUnrelatedUncommittedChanges(t *testing.T)       // owner's edit to an untouched file survives the merge
func TestFinishRefusesWhenSigningIsRequired(t *testing.T)
func TestFinishRunsNoHook(t *testing.T)                             // post-merge and post-checkout markers absent; Notes mention them
func TestFinishDoesNotMaterialiseWithheldFilesDifferently(t *testing.T)
// a withheld tracked file in the owner's checkout is byte-identical and still tracked after Finish.
func TestUpdateMergesTheOwnersBranchAndResyncs(t *testing.T)
// the owner commits a new file and a new .env → after Update the file is in the workspace, .env is not on disk there.
func TestUpdateConflictIsAbandonedCleanly(t *testing.T)
func TestUpdateRefusesWithAMergeDriverConfigured(t *testing.T)      // the driver's marker file never appears
func TestAbandonKeepsTheBranchUnlessTold(t *testing.T)
func TestAfterTheEndReadsServeTheOwnerAndWritesRefuse(t *testing.T)
```

Also remove the `t.Skip` from `TestUndoCannotCrossAnUpdate` (Task 7).

- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test -race -count=1 ./internal/repo/`. Expected: PASS.
- [ ] **Step 5:** Commit: `Finish: fast-forward into the owner's branch, or refuse`.

---

### Task 9: The tools

**Files:**
- Modify: `internal/tools/tools.go` (`catalog`, `New`, `Instructions`)
- Create: `internal/tools/write.go`
- Test: `internal/tools/write_test.go` (create)

**Interfaces:**
- Consumes: everything exported by Tasks 4–8; `mcp.Object`, `Tool.Write`, `Tool.Destructive` (Task 1).
- Produces:

```go
type Source interface{ Repo(ctx context.Context) (*repo.Repo, error) }
func New(r *repo.Repo) []mcp.Tool                        // unchanged signature and result: the 13 tools
func NewWrite(owner *repo.Repo, s *repo.Sessions) []mcp.Tool // the 13 tools served from s.Serve, plus the nine below
const WriteInstructions string                           // appended to Instructions in write mode
```

`catalog.r` becomes a call to the source at the start of every handler.

| Tool | Annotation | Arguments | Calls |
|---|---|---|---|
| `start_session` | write | `fresh` (bool) | `Start` |
| `session_status` | read-only | none | `Status` |
| `propose_changes` | read-only | `changes` (Object, required) | `Propose` |
| `apply_changes` | write | `changes` (Object) or `plan` (string, 16 hex); `message` (string, ≤ 2000) | `Apply` |
| `list_checkpoints` | read-only | none | `Checkpoints` |
| `undo_to_checkpoint` | write | `checkpoint` (string, required) | `UndoTo` |
| `update_session` | write | none | `Update` |
| `finish_session` | write, destructive | `squash` (bool), `message` (string, ≤ 2000) | `Finish` |
| `abandon_session` | write, destructive | `delete_branch` (bool) | `Abandon` |

- Decisions:
  - `changes` advertises a JSON Schema for an array (max 500 items) of objects with exactly the members of `repo.Change`, `op` as an enum, `additionalProperties: false`.
  - `apply_changes` with both or neither of `changes` and `plan` is an error.
  - Results follow the existing `finish` shape (text plus the `@meta` line) and the existing size budget. `apply_changes` returns the checkpoint, the line counts and the fresh anchors, and does not return the diff (the model already has its own text). `propose_changes` returns the diff, cut to the budget with a note.
  - Every text that comes from a file goes through the redactor as in the read tools.
  - `ChangeError` is returned as a tool error whose text is the reason followed by `Fresh`.
  - `read_file` in write mode adds `file_tag` to its `@meta` line.
  - `WriteInstructions` (exact copy): "Write mode is on. Files are edited in a session workspace on a side branch, never in the user's own checkout. Lines you read carry anchors like `12:abc|`; cite them in apply_changes instead of quoting old text. If an anchor is stale the call fails and returns the current lines: use those anchors and retry. One apply_changes call may change many files and is applied completely or not at all. Lines that hold a masked value cannot be changed; tell the user to edit them by hand. Nothing reaches the user's branch until finish_session; call it only when the user asks to merge."

- [ ] **Step 1: Failing tests**

```go
func TestReadModeCatalogueIsUnchanged(t *testing.T)
// New(r): names, descriptions and schemas equal a golden captured from main before this branch.
func TestWriteCatalogueAddsNineToolsWithHonestAnnotations(t *testing.T)
// 22 tools; the 13 read tools read-only; annotations exactly as in the table.
func TestEditRoundTripThroughTheTools(t *testing.T)
// start_session → read_file (anchors, file_tag) → apply_changes using those anchors →
// read_file shows the new text → list_checkpoints has two entries → git_diff against the
// start shows the change → finish_session → the owner's file on disk has the new text.
func TestStaleAnchorErrorCarriesFreshAnchorsThatWork(t *testing.T)
// take the anchors out of the error text, retry, succeed.
func TestWriteToolsRefuseWithoutASession(t *testing.T)   // each of the seven that need one; message names start_session
func TestReadToolsFollowTheSession(t *testing.T)
// before start: an untracked file of the owner is visible; during the session it is not and
// the model's new file is; after abandon the first state returns.
func TestApplyNeedsExactlyOneOfChangesAndPlan(t *testing.T)
func TestProposeDiffIsMaskedAndBudgeted(t *testing.T)
```

- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test -count=1 ./internal/tools/`. Expected: PASS.
- [ ] **Step 5:** Commit: `Tools: nine write-mode tools over the session`.

---

### Task 10: The command line

**Files:**
- Modify: `internal/cli/session.go` (struct `session`, `sessionFlags`), `internal/cli/serve.go`, `internal/cli/run.go` (`banner`, the activity display), `internal/cli/cli.go`, `internal/cli/audit.go`, `internal/mcp/server.go` (`Event`)
- Create: `internal/cli/sessioncmd.go`
- Test: `internal/cli/write_test.go` (create)

**Interfaces:**
- Consumes: `repo.NewSessions`, `tools.NewWrite`, `tools.WriteInstructions`.
- Produces:
  - Flags on the sharing commands and on `mcp`: `--write`, `--new-session` (implies nothing without `--write`: an error), `--confirm-merge`.
  - `session.Write`, `session.NewSession`, `session.ConfirmMerge` (JSON `write`, `new_session`, `confirm_merge`), passed to the serving process like the other options.
  - `mcp.Event.Note string`: a one-line statement for the owner; the serving process sends an event of `Kind: "session"` for start, apply, undo, update, finish and abandon. The activity display prints it; the audit log stores it as `note` with the checkpoint and the file and line counts (never content).
  - `repochat session [status|finish|abandon] [--root …] [--squash] [--message …] [--delete-branch]`.

- Decisions:
  - Sessions live under `<state dir>/sessions`.
  - With `--write`, sharing starts or resumes the session before the banner, so the banner can name the branch.
  - Banner (exact copy): the first line says `· WRITE MODE (experimental)` in place of `· read-only`; a line `  session     <branch> · from <start branch> @ <7-char base> · <n> checkpoint(s)`; the `audience` line reads `whoever can use this plugin in ChatGPT can read these files and change them in the session workspace while repochat runs`; and a final line `  merging     nothing reaches <start branch> until finish_session is called` or, with `--confirm-merge`, `… until you run: repochat session finish`.
  - With `--confirm-merge`, the `finish_session` tool does not merge. It returns: "The session is ready to merge. The user chose to confirm merges in the terminal: ask them to run `repochat session finish`." `repochat session finish` shows the status (checkpoints, files, run-later files), asks `Merge into <branch>? [y/N]` through the existing `confirm`, and calls `Finish`.
  - `repochat session` with no subcommand is `status`. Without an open session it says so and exits 0.
  - `--write` together with `--no-history` is refused (checkpoints are history).

- [ ] **Step 1: Failing tests**

```go
func TestWriteFlagAddsTheToolsAndNothingElseDoes(t *testing.T)
// `repochat tools` → 13; `repochat tools --write` → 22; `mcp` without --write answers
// tools/list with 13 and refuses tools/call apply_changes as an unknown tool.
func TestBannerSaysWriteModeAndNamesTheBranch(t *testing.T)
func TestSessionCommandShowsAndFinishes(t *testing.T)
// through the stand-in client: start, apply; then `repochat session` prints the branch and
// 1 checkpoint; `repochat session finish` with "y" on stdin merges; with "n" it does not.
func TestConfirmMergeKeepsTheToolFromMerging(t *testing.T)
func TestWritesAreRecordedWithoutContent(t *testing.T)
// the audit log has a line for the apply with checkpoint and counts; the new text is not in it.
func TestNewSessionWithoutWriteIsAnError(t *testing.T)
func TestWriteAndNoHistoryAreRefusedTogether(t *testing.T)
func TestTerminalCommandAndServerShareOneLock(t *testing.T)
// `repochat session abandon` while the server runs → the server's next apply_changes
// answers with the no-session error instead of writing into a removed workspace.
```

- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `make check` (vet, gofmt, tests) and `make test-race`. Expected: PASS.
- [ ] **Step 5:** Commit: `CLI: --write, the session command, and what the banner says`.

---

### Task 11: The boundary, the git matrix, the documents, the review

**Files:**
- Modify: `internal/tools/boundary_test.go`, `.github/workflows/ci.yml`, `Makefile` (`fuzz`), `README.md`, `README.ro.md`, `CHANGELOG.md`, `docs/design/README.md`
- Create: `internal/tools/writeboundary_test.go`, `docs/design/audit/write-mode-prompt.txt`, `docs/design/audit/write-mode-codex.md`

- [ ] **Step 1: The write boundary test (failing first where a hole exists)**

```go
func TestWriteBoundaryThroughEveryTool(t *testing.T)
```

Canary files, each with unique content, in: the owner's checkout (tracked, untracked, and uncommitted edits); a directory outside both the repository and the state directory; every protected path that exists in the fixture; withheld tracked files (built-in rule, committed `.repochatignore`, uncommitted `.repochatignore`, `--deny`); the `.git` directory of the owner (`config`, `hooks/pre-commit`) and the worktree's own git directory. Then drive every write tool with hostile arguments: paths with `..`, absolute paths, `.git/…` in every letter case, paths through a symbolic link that points at each canary, `to_path` variants of all of these, `create` over each canary, `remove` and `move` of each. Assert after every call that every canary is byte-identical and that no new file exists outside the workspace. The positive control: a legitimate edit in the same fixture succeeds.

- [ ] **Step 2:** `go test -count=1 -run WriteBoundary ./internal/tools/`. Fix anything it finds in the owning file, test first. Expected at the end: PASS.

- [ ] **Step 3: CI.** The `git-versions` job already runs `./internal/repo/ ./internal/tools/` on git 2.25, 2.34 and 2.43; confirm the new tests run there and pass. Add `FuzzParseChanges` to `make fuzz`. Push the branch, open the PR, and get every job green; fix by changing code, never by skipping a test on an old git.

- [ ] **Step 4: Documents.**
  - `README.md` and `README.ro.md`: a section "Write mode (experimental)": how to start it, what a session is, the nine tools in one table, the guarantees and the non-guarantees copied from the spec's "What changes in the guarantees", the Go 1.25 build requirement.
  - `CHANGELOG.md` (Romanian): the entry.
  - `docs/design/README.md` (Romanian): note that rule K2 ("read-only forever") was reversed by the owner on 2026-11-19 for an opt-in mode, with a link to the spec.

- [ ] **Step 5: Adversarial review.** Write `docs/design/audit/write-mode-prompt.txt`: the guarantees from the spec, and the instruction to break each one against the built binary and report a reproduction for every claim. Run it read-only:

```bash
codex exec -s read-only --skip-git-repo-check "$(cat docs/design/audit/write-mode-prompt.txt)" < /dev/null > docs/design/audit/write-mode-codex.md 2>&1
```

Reproduce every finding before believing it. For each real one: a failing test, the fix, the suite. Record which findings were real in the audit file.

- [ ] **Step 6:** `make check && make test-race && make fuzz && make test-contract`. Expected: all PASS. Commit: `Write mode: boundary test, documents, review findings`.

- [ ] **Step 7:** PR with green CI on the head commit. The PR text states that write mode is experimental and that it has not yet been exercised from a real ChatGPT session.
