# Owner-reviewed guidance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the owner promote one reviewed root AGENTS.md / CLAUDE.md text to standing guidance that the MCP server delivers in its instructions. The guidance is bound to one tunnel and one repository, and the guard invalidates it conservatively.

**Architecture:** A new package, `internal/guidance`, holds:
- the record, the lifecycle and the tunnel assignments, under the state directory with a flock;
- validation and digests;
- the instruction assembly;
- the guard.

The repository side lives in `internal/repo`:
- the one-shot policy-aware read;
- the display transform;
- the binding capture;
- the single-path exposure decision.

`internal/cli` holds `repochat trust`, the launch-path enforcement, and the wiring of the guard into the MCP server. `internal/mcp` gets one hook.

**Tech Stack:** Go 1.25, standard library only; git ≥ 2.25.

**Spec:** `docs/superpowers/specs/2026-11-25-owner-guidance-design.md`. The debate is in `docs/design/guidance/round1-3*`.

## Global Constraints

- Standard library only. Linux and macOS. Every git command goes through the existing hardened runner.
- Without a record, tools, instructions and results are byte-identical to today, in both modes and both protocol eras. `TestReadModeCatalogueIsUnchanged` and `TestReadModeInstructionsAreUnchanged` pass untouched.
- No MCP operation creates, changes or reads trust state.
- Fail closed: a malformed or unreadable record, a binding mismatch or an unsupported transformation version refuses to start the share, with the reason.
- Limits are 8 KiB of raw source and 2 KiB of display. Over the limit means refusal, never truncation.
- Unmasked source is never stored. The raw tunnel ID is never stored in guidance state.
- Every file the feature writes is 0600 and every directory 0700. Writes are atomic, follow no symbolic links, and the state directory lies outside the repository and every session workspace.

## Review Focus

1. **Two profile names configured with the same tunnel.** `trust` on one and a share of the other repository on the second: the share is refused (Task 4: `TestAssignedTunnelRefusedThroughAnAlias`).
2. **The file is edited between the preview and the confirmation.** The stored snapshot is exactly what was shown, and the raw digest is that of the shown bytes (Task 2: `TestTrustStoresTheBytesItShowed`).
3. **`server/discover` right after `repochat trust --revoke`, before any tool call.** It is refused, not answered with the old instructions (Task 5: `TestDiscoverAfterRevokeIsRefused`).
4. **A `.repochatignore` edit that withholds AGENTS.md, made less than 3 s after the last call.** The next call is refused (Task 5: `TestWithholdingTheSourceInsideTheCacheWindowInvalidates`).
5. **Guidance text containing zero-width and bidi characters, and a fake `OWNER GUIDANCE` heading.** Shown visibly at review, and delivered as reviewed inside the segment only (Task 2: `TestDisplayRendersInvisibleCharacters`; Task 5: `TestFakeHeadingInsideToolResultsIsData`).

---

## File structure

| File | Responsibility |
|---|---|
| `internal/repo/guidance.go` (new) | `ReadGuidanceSource`, `GuidanceDisplay`, `CaptureBinding`, `PathServed` |
| `internal/guidance/state.go` (new) | record, lifecycle, assignments; load/validate/save under flock |
| `internal/guidance/deliver.go` (new) | `Instructions`, `Guard` |
| `internal/cli/trust.go` (new) | `repochat trust` |
| `internal/cli/run.go`, `serve.go`, `session.go`, `cli.go`, `config.go` (modify) | launch enforcement, wiring, session config `GuidanceKey` |
| `internal/mcp/server.go` (modify) | `Server.Guard func(method string) error` called before tool calls and instruction-bearing responses |
| `internal/tools/tools.go` (modify) | `repo_overview` drift line through an optional hook |

---

### Task 1: Repository side: one-shot read, display, binding, single-path decision

**Files:** create `internal/repo/guidance.go` and `internal/repo/guidance_test.go`.

**Interfaces, produced:**
- `type GuidanceSource struct{ Path string; Raw []byte; RawSHA string }`
- `func (r *Repo) ReadGuidanceSource(ctx context.Context, path string) (*GuidanceSource, error)`:
  - `path == ""` picks the first of `AGENTS.md`, `CLAUDE.md` that the snapshot lists and the policy serves.
  - The file must be root-level, read once through the existing no-follow opener, a regular file, valid UTF-8 and at most 8192 raw bytes.
  - Errors name the reason.
- `func (r *Repo) GuidanceDisplay(raw []byte) (display string, err error)`:
  - the masked text, through the redactor;
  - `Sanitize`, plus zero-width characters (U+200B–U+200D, U+2060, U+FEFF) shown as `\uNNNN`;
  - an error when the result is over 2048 bytes.
- `type Binding struct{ Root, CommonDir string; RootDev, RootIno, CommonDev, CommonIno uint64; Roots []string }`
- `func (r *Repo) CaptureBinding(ctx context.Context) (*Binding, error)`:
  - canonical paths;
  - stat device and inode through `syscall.Stat_t`, built on Linux and darwin only;
  - `rev-list --max-parents=0 HEAD`, sorted;
  - refuses a shallow repository (`rev-parse --is-shallow-repository`) and an unborn HEAD.
- `func (b *Binding) Equal(o *Binding) bool`
- `func (r *Repo) PathServed(ctx context.Context, path string) (served bool, inputs string, err error)`:
  - takes a fresh decision for one path. It bypasses the 3 s snapshot: a fresh listing plus the policy evaluator for that path.
  - `inputs` is the SHA-256 of the owner `.repochatignore`, or "absent", joined with the options digest.

- [ ] **Step 1: Write the failing tests.**
  - `TestReadGuidanceSourceDefaultsAndRefusals`: AGENTS.md is preferred over CLAUDE.md. Refused, each with its reason: a nested path, a symlink, an 8193-byte file, invalid UTF-8, a withheld file, and an untracked file under `--tracked-only`.
  - `TestDisplayRendersInvisibleCharacters`: U+200B, U+202E and ESC are rendered visibly, and a masked AWS key appears as `[REDACTED:...]`.
  - `TestDisplayOverTwoKiBIsRefused`.
  - `TestCaptureBindingRootsAndRefusals`: the root set of a repository with two orphan roots, sorted. A shallow clone and an unborn HEAD are refused.
  - `TestPathServedSeesAFreshRepochatignore`: write `.repochatignore` withholding AGENTS.md immediately after a `Policy` call; `PathServed` reports false with different `inputs`.
- [ ] **Step 2:** The tests fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test ./internal/repo/ -run 'Guidance|Binding|PathServed' -count=1` passes, and so does the package once.
- [ ] **Step 5:** Commit: `Guidance: read the source once, render it visibly, and capture the repository binding`.

### Task 2: Guidance state: record, lifecycle, assignments

**Files:** create `internal/guidance/state.go` and `internal/guidance/state_test.go`.

**Interfaces, consumed:** Task 1's `Binding`.

**Interfaces, produced:**
- `type Record struct{ Version, Transform int; Key string; Binding repo.Binding; Path, RawSHA, Display, DisplaySHA string; Reviewed time.Time; Generation uint64; Checksum string }`
- `const RecordVersion = 1` and `const TransformVersion = 1`
- `type Store struct{ dir string }`
- `func Open(stateDir, repoRoot string) (*Store, error)`:
  - creates `<stateDir>/guidance` with mode 0700;
  - refuses a directory inside `repoRoot`;
  - refuses a symbolic link anywhere on the path.
- `func (s *Store) Lock() (unlock func(), err error)`: an exclusive flock on `<dir>/.lock`.
- `func (s *Store) Save(r *Record) error`: under the lock. It bumps `lifecycle.json[key]`, sets `r.Generation` to the new value, computes the checksum (SHA-256 over the canonical JSON with `Checksum` empty), and writes atomically with mode 0600. It also writes `assignments.json[key] = r.Binding`.
- `func (s *Store) Load(key string) (*Record, error)`: `(nil, nil)` when absent. Otherwise it validates the version, the transform, the raw-digest syntax (64 hex), the display digest recomputed from the display, the checksum, the generation (≤ the lifecycle value) and the 2048-byte cap. Any failure returns an error.
- `func (s *Store) Revoke(key string) error`: under the lock; bumps the lifecycle and removes the record.
- `func (s *Store) Assignment(key string) (*repo.Binding, error)`
- `func (s *Store) Release(key string) error`: removes the assignment and the record, and bumps the lifecycle.
- `func Key(tunnelID string) string`: the same derivation as the existing `tunnelKey` in `internal/cli/run.go`. Move that function here and call it from `run.go`.

- [ ] **Step 1: Write the failing tests.**
  - `TestSaveLoadRoundTripAndPermissions`: files 0600, directory 0700.
  - `TestLoadRefusesDamage`: a flipped display byte, a bad checksum, a non-hex raw digest, transform 2 and a truncated file are each an error.
  - `TestGenerationsNeverRepeat`: save, revoke, save gives generations 1, 2, 3.
  - `TestAssignmentSurvivesRevoke`.
  - `TestOpenRefusesInsideRepoAndSymlinks`.
  - `TestConcurrentSavesSerialize`: two goroutines, under `-race`.
  - `TestTrustStoresTheBytesItShowed` belongs to Task 3, which passes in the display it showed.
- [ ] **Step 2:** The tests fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test -race ./internal/guidance/ -count=1` passes.
- [ ] **Step 5:** Commit: `Guidance: state with digests, monotonic generations and tunnel assignments`.

### Task 3: `repochat trust`

**Files:** create `internal/cli/trust.go` and add tests to `internal/cli/trust_test.go`. Modify `cli.go` (dispatch and help).

**Interfaces, consumed:** Tasks 1 and 2, plus the existing profile and config resolution (`loadConfig`, the bound repo, the credentials' tunnel ID).

**Behaviour:**
- `repochat trust [PATH] [--yes] [--profile NAME]`:
  1. Resolves the profile and refuses one that is not bound to this repository ("bind it: repochat setup --profile NAME --bind").
  2. Refuses when the tunnel is assigned to another binding.
  3. Reads the source once, builds the display and captures the binding.
  4. Prints the repository name and root, the profile, the path, the display between clear delimiters, and the sentence "This text is sent to OpenAI with the plugin's metadata whenever ChatGPT loads the plugin."
  5. Asks "Give this to ChatGPT as your standing instructions for <name>?" (default no). Without a terminal and without `--yes` it does nothing and exits 1.
  6. Saves using the bytes it showed, and prints "Restart repochat, press Refresh tools in ChatGPT, and start a new conversation."
- `--status` prints the record: path, review time, generation, and the drift state from Task 6's `Drift` once that exists. Until then it prints unchanged, changed or removed by comparing the raw digest.
- `--revoke` revokes.
- `--release-tunnel` confirms and releases, printing the ChatGPT step (delete the plugin and create it again).

- [ ] **Step 1: Write the failing tests.**
  - `TestTrustNeedsABoundProfile`.
  - `TestTrustStoresTheBytesItShowed`: the file is rewritten between the preview and the confirmation, through a test seam called after printing. The stored display and raw digest are those of the first read.
  - `TestTrustWithoutTerminalNeedsYes`.
  - `TestTrustRevokeAndStatus`.
  - `TestReleaseTunnelConfirms`.
- [ ] **Step 2:** The tests fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test ./internal/cli/ -run Trust -count=1` passes, and so does the package once.
- [ ] **Step 5:** Commit: `repochat trust: review a guidance file and store exactly what was shown`.

### Task 4: Launch-path enforcement

**Files:** modify `internal/cli/run.go`, `config.go` (setup bind and unbind), and the takeover path. Tests in `internal/cli/trust_test.go`.

**Behaviour:** before a share, a takeover, `setup --bind`, `setup --unbind`, or `repochat mcp` with tunnel credentials:
- If `Assignment(Key(tunnelID))` exists and the repository's binding differs, refuse: "this tunnel carries owner guidance for <root>; use another tunnel, or `repochat trust --release-tunnel` there".
- If a record exists but the binding changed (roots, inode or path), refuse: "the repository changed since its guidance was reviewed; run repochat trust again".
- `--unbind` on an assigned tunnel is refused with the same message.

- [ ] **Step 1: Write the failing tests.**
  - `TestAssignedTunnelRefusedOnAnotherRepository`.
  - `TestAssignedTunnelRefusedThroughAnAlias`: two profiles share one tunnel ID.
  - `TestUnbindRefusedWhileAssigned`.
  - `TestChangedRootsRefuseTheShare`: rewrite history with a new orphan root.
- [ ] **Step 2:** The tests fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test ./internal/cli/ -count=1` passes.
- [ ] **Step 5:** Commit: `A tunnel that carries guidance serves only its repository`.

### Task 5: Delivery and guard

**Files:**
- Create `internal/guidance/deliver.go` and `deliver_test.go`.
- Modify `internal/mcp/server.go`, to add `Guard func(method string) error`. When non-nil it is called before `tools/call` and before building the `initialize` and `server/discover` results; a non-nil error becomes a JSON-RPC error response. The result for `tools/call` must stay an `isError` tool result.
- Modify `internal/cli/serve.go` and `session.go`:
  - the session JSON gains `GuidanceKey string \`json:"guidance_key,omitempty"\``, set by the launcher from the tunnel;
  - the MCP process loads the record at start, fails closed on error, builds the instructions, and installs the guard.

**Interfaces, produced:**
- `func Instructions(name, base string, r *Record) (string, error)`:
  - returns `lead(name) + base + "\n\nOWNER GUIDANCE (reviewed by the user, <date>, from <path>)\n" + r.Display + "\nEND OF OWNER GUIDANCE\n"`;
  - `lead` is the spec's lead text, checked to be at most 512 characters with the safe name;
  - returns an error when the whole exceeds `MaxInstructions = 8192`.
- `type Guard struct{ ... }` and `func NewGuard(store *Store, key string, started *Record, owner *repo.Repo) *Guard`
- `func (g *Guard) Check(ctx context.Context) error`:
  - loads and validates the record, compares the generation and the display SHA, and caches `PathServed` keyed by its `inputs`;
  - latches on the first failure, and returns `ErrInvalidated` with the notice text on every later call.
- With no record, `Guard` is nil and the instructions are the base text, byte-identical.

- [ ] **Step 1: Write the failing tests.**
  - `TestInstructionsLeadFitsAndSegmentIsLast`.
  - `TestNoRecordInstructionsAreByteIdentical` covers both modes.
  - `TestDiscoverAfterRevokeIsRefused`: through `mcp.Server` with the guard, revoke, then send `server/discover` and `initialize`, and expect errors.
  - `TestToolCallAfterReplaceIsRefusedAndLatched`.
  - `TestWithholdingTheSourceInsideTheCacheWindowInvalidates`.
  - `TestFakeHeadingInsideToolResultsIsData`: a repository file containing "OWNER GUIDANCE". `read_file` returns it as ordinary data, and the instructions carry the segment only once.
  - `TestMalformedRecordRefusesToStart`.
- [ ] **Step 2:** The tests fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test ./internal/guidance/ ./internal/mcp/ ./internal/cli/ ./internal/tools/ -count=1` passes, and the read-mode goldens are untouched.
- [ ] **Step 5:** Commit: `Guidance reaches ChatGPT in the server instructions, guarded before every call`.

### Task 6: Drift line, docs, full check

**Files:** `internal/guidance/deliver.go` (`Drift`), `internal/tools/tools.go` (an optional `OverviewNote func(ctx) string` on the catalogue, used only when non-nil), README.md, README.ro.md, CHANGELOG.md, the new `docs/guidance-test.md`, and the spec status.

**Interfaces, produced:** `func Drift(ctx, owner *repo.Repo, r *Record) (state string)`. It returns one of `unchanged`, `changed`, `removed` (only on `fs.ErrNotExist`) or `could not be compared`. It always reads the owner checkout.

The overview line reads: "Owner guidance: from <path>, reviewed <date>; <state> (the reviewed version is in the instructions)". The terminal shows a notice once.

- [ ] **Step 1: Write the failing tests.** `TestDriftStates`, covering the four states including a permission-denied file. `TestOverviewShowsDriftOnlyWithAGuard`: without a record, the overview is byte-identical.
- [ ] **Step 2:** Implement, then write the docs:
  - the README sections (both languages): what it is, the commands, the limits, the threat model (repository authors and model-controlled inputs are adversarial; the owner's account and binary are trusted), and "shared ancestry does not authenticate a project";
  - the CHANGELOG entry in Romanian;
  - `docs/guidance-test.md` with the spec's protocol and thresholds;
  - spec status: "built on <date>; experimental; not yet measured in ChatGPT".
- [ ] **Step 3:** `make check` passes.
- [ ] **Step 4:** Commit: `Guidance: drift in the overview, docs and the real-session protocol`.
