# Edit-loop hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make write mode's existing edit loop safe to retry and bound to the revision being acted on, reviewable and undoable from the terminal, and able to gofmt a Go file through a change set. This is item 2 of the parity plan.

**Architecture:**
- Every mutating call may carry a model-chosen `operation_id`. For calls that make a checkpoint, the id and a digest of the call's arguments go into the commit as a trailer, so the record of the operation is atomic with the branch move and survives restarts. `finish_session`, `abandon_session` and `start_session` keep a small receipt file next to `session.json`.
- An `expected_checkpoint` makes a call refuse when the session has moved; `finish_session` requires it.
- The terminal gains `repochat session diff` and `repochat session undo`.
- A new change-set operation `format` runs `go/format` on a `.go` file as the last step of that file's edit.

**Tech Stack:** Go 1.25, standard library only; git ≥ 2.25.

**Spec:** `docs/superpowers/specs/2026-11-25-cli-parity-design.md`, item 2 (M3, M6, M9, G8, explicit Go formatting); the background is in `docs/design/cli-parity/round1-codex.json` (M3, M6, M9, G8, F57).

## Global Constraints

- Standard library only; one static binary; Linux and macOS; git 2.25 or newer.
- Read mode is unchanged: `TestReadModeCatalogueIsUnchanged` and `TestReadModeInstructionsAreUnchanged` pass untouched.
- Every git command goes through the existing hardened runner. No program configured by the repository runs.
- New tool parameters are optional, except `expected_checkpoint` on `finish_session`. ChatGPT keeps cached tool lists, so a call without the new parameters must work as today; `finish_session` is the deliberate exception (M9).
- An operation is never applied twice. Reusing an `operation_id` with different arguments is refused, and nothing is changed.
- Old checkpoints (two trailers) keep parsing as their kind.
- The `format` operation never changes a line that holds a masked value, and never changes a file's line-ending style or BOM.

## Review Focus

1. Two chats send the same `operation_id` at the same time. Exactly one applies; the other waits for the lock and reports the replay (Task 2: `TestConcurrentSameOperationAppliesOnce`).
2. ChatGPT with a cached, pre-upgrade tool list calls `finish_session` without `expected_checkpoint`. It is refused with a message that says to pass the head from `session_status` and to refresh the plugin's tools, not a bare schema error (Task 4: `TestFinishWithoutExpectedCheckpointExplainsWhy`).
3. An `operation_id` recorded in an abandoned session is retried after a new session started. It is not "replayed" into the new session; the call is processed normally and its anchors refuse it (Task 2: `TestOperationFromAnAbandonedSessionIsNotReplayed`).
4. `format` on a CRLF file, a file with a BOM, or a file holding a masked value is refused and the file is untouched (Task 5: `TestFormatRefusesCRLFBOMAndMaskedFiles`).
5. `expected_checkpoint` given as 7 hex characters, 12, the full 40, or in upper case: each is accepted when it names the head (Task 1: `TestExpectedCheckpointAcceptsAnyPrefixForm`).

---

## File structure

| File | Responsibility |
|---|---|
| `internal/repo/operation.go` (new) | `Call`; checking the expected checkpoint; looking operations up in the chain; the receipt file |
| `internal/repo/operation_test.go` (new) | Tasks 1–3 |
| `internal/repo/apply.go`, `checkpoint.go`, `finish.go`, `session.go` (modify) | the `…Call` entry points; trailer writing and parsing; `NoResync` |
| `internal/repo/changeset.go` (modify) | the `format` operation |
| `internal/tools/write.go` (modify) | parameters, argument digest, replay and handoff texts, instructions |
| `internal/cli/sessioncmd.go` (modify) | `session diff`, `session undo` |
| `README.md`, `README.ro.md`, `CHANGELOG.md`, the spec (modify) | documentation |

---

### Task 1: `Call` and the expected checkpoint (M6)

**Files:** Create `internal/repo/operation.go`, `internal/repo/operation_test.go`. Modify `apply.go`, `checkpoint.go`, `finish.go`.

**Interfaces — Produces:**
- `type Call struct{ Op, Digest, Expect string }`. `Op` is the operation id ("" for none), `Digest` is the argument digest recorded with it, and `Expect` is the checkpoint the session must be at ("" for no check).
- `var OperationIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)` and `var ExpectRe = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)`.
- `func checkExpected(head, expect string) error`: nil when `expect == ""` or `head` starts with `strings.ToLower(expect)`. Otherwise it returns a `*ChangeError` refusal: "the session moved: it is at checkpoint <head[:12]>, not <expect>; another conversation may be editing it. Call session_status, read the files again and retry with the new checkpoint".
- `func (s *Sessions) ApplyCall(ctx context.Context, call Call, planID string, changes []Change, message string) (*Applied, error)`
- `func (s *Sessions) UndoToCall(ctx context.Context, call Call, id string) (*Applied, error)`
- `func (s *Sessions) UpdateCall(ctx context.Context, call Call) (*Applied, error)`
- `func (s *Sessions) FinishCall(ctx context.Context, call Call, squash bool, message string) (*Finished, error)`

The existing `Apply`, `UndoTo`, `Update` and `Finish` become one-line wrappers passing `Call{}`, so no existing test changes. Each `…Call` validates `call.Expect` with `ExpectRe` before taking the lock. Under the lock, it compares against the branch head from `s.chain` before anything is written.

- [ ] **Step 1: Write the failing tests** in `operation_test.go`, using the helpers of `apply_test.go`:
  - `TestExpectedCheckpointRefusesAMovedSession`:
    1. Start a session, apply change A, and note the head H1.
    2. Apply change B (head H2).
    3. Call `ApplyCall(Call{Expect: H1[:12]}, …C…)`. The error contains "the session moved", the branch head is still H2, and the files hold no part of C.
  - The same with `UndoToCall`, `UpdateCall` and `FinishCall` (Finish: the owner's branch tip is unchanged).
  - `TestExpectedCheckpointAcceptsAnyPrefixForm`: a matching head given as 7 hex characters, 12, 40 and upper-case 12 is accepted.
  - `TestExpectedCheckpointRejectsMalformed`: `"zzzzzzz"`, `"abc"` and 41 hex characters are refused before the lock, and nothing changes.
- [ ] **Step 2:** `go test ./internal/repo/ -run 'ExpectedCheckpoint' -count=1` fails (undefined `ApplyCall`).
- [ ] **Step 3:** Implement as specified.
- [ ] **Step 4:** `go test ./internal/repo/ -count=1` passes.
- [ ] **Step 5:** Commit: `Write mode: a call can name the checkpoint it expects the session to be at`.

### Task 2: Operation ids on checkpoints (M3)

**Files:** Modify `internal/repo/operation.go`, `apply.go` (`commitMessage`, `checkpoint`), `checkpoint.go` (`checkpointKind`, `describeCheckpoint`, `UndoToCall`), `finish.go` (`UpdateCall`, `commitUpdate`). Test `internal/repo/operation_test.go`.

**Interfaces:**
- Consumes `Call` (Task 1).
- Produces:
  - `commitMessage(message, kind string, call Call) (text string, full []byte)`. When `call.Op != ""`, a third trailer line `Repochat-Operation: <op> <digest>` follows `Repochat-Kind`.
  - `checkpointKind(msg string) (kind, text, op, digest string)`, which accepts either the two trailers or the three, and nothing else.
  - `Checkpoint` gains `Operation string`.
  - `Applied` gains `Replayed bool`.
  - `func (s *Sessions) findOperation(ctx context.Context, chain []string, op string) (cp *Checkpoint, digest string, found bool, err error)`: one `git log --format=%H%x1f%B%x1e` over the chain (first parent), through the hardened runner.

Order inside each `…Call`, under the lock:
1. If `call.Op != ""`, look it up. Found with the same digest: return `&Applied{Checkpoint: *cp, Replayed: true}`, nothing written. Found with another digest: refuse ("operation_id … was already used for a different call; choose a new id"), nothing written.
2. Otherwise check `Expect` (Task 1), then run as today, with `call` passed down to `commitMessage`.

`OperationIDRe` is checked before the lock.

- [ ] **Step 1: Write the failing tests:**
  - `TestOperationReplayReturnsTheSameCheckpoint`: an `ApplyCall` with `Op "op-aaaa1111"` and digest d1, twice. Both return the same `Checkpoint.ID`, the second has `Replayed == true`, and the branch moved once (`Checkpoints` has one change).
  - `TestOperationReuseWithOtherArgumentsIsRefused`: the same op with d2 → the error contains "different call", and the head is unchanged.
  - `TestOperationSurvivesRestart`: after the first apply, close the `Sessions` and build a new one on the same state directory; the replay is still found.
  - `TestOperationReplayWinsOverExpect`: the retry carries `Expect` = the head before the first call. It is replayed, not refused as moved.
  - `TestConcurrentSameOperationAppliesOnce`: two goroutines call the same `ApplyCall`. Exactly one `Replayed == false`, and one new checkpoint.
  - `TestOperationFromAnAbandonedSessionIsNotReplayed`:
    1. Apply with op X, abandon, start a new session.
    2. The same `ApplyCall` with the old anchors is refused for stale anchors, not replayed.
    3. A call with op X and fresh anchors applies normally.
  - `TestUndoAndUpdateCarryOperations`: `UndoToCall` and `UpdateCall` with ops replay the same way.
  - `TestCheckpointMessagesHideTheOperationTrailer`: `Checkpoints()[0].Message` equals the message given, and `.Operation` equals the op.
  - `TestTwoTrailerCheckpointsStillParse`: a commit made with the old two-trailer message (built by hand in the test) lists as kind `change`.
- [ ] **Step 2:** The tests fail (`Replayed` undefined).
- [ ] **Step 3:** Implement as specified.
- [ ] **Step 4:** `go test ./internal/repo/ -count=1` passes, including the existing checkpoint, undo and update tests.
- [ ] **Step 5:** Commit: `Write mode: an operation id makes apply, undo and update safe to retry`.

### Task 3: Receipts for start, finish and abandon; revision-bound handoff (M3, M9)

**Files:** Modify `internal/repo/operation.go`, `finish.go`, `session.go`. Test `operation_test.go`.

**Interfaces:**
- Consumes `Call` and `OperationIDRe` (Tasks 1–2).
- Produces:
  - `<s.dir>/operations.json`: a JSON array of `receipt{ID, Tool, Digest, State string; Time time.Time; Finished *Finished; Info *SessionInfo}`. `State` is `started` or `done`. Written with `writeFileAtomic` under the lock, keeping the newest 64 entries.
  - `func (s *Sessions) StartCall(ctx context.Context, call Call, fresh bool) (*SessionInfo, error)` and `func (s *Sessions) AbandonCall(ctx context.Context, call Call, deleteBranch bool) (replayed bool, err error)`, with `Start` and `Abandon` as wrappers.
  - `Finished` gains `Base, Head string` (the reviewed revision) and `Replayed bool`. `SessionInfo` gains `Replayed bool`.

Behaviour:
- `FinishCall` writes a `started` receipt immediately before the owner's merge (where `beforeOwnerMerge` is called) and a `done` receipt with `Finished` after the session is closed.
- A retry that finds `done` with the same digest returns the stored `Finished` with `Replayed = true`, even though no session is open any more.
- A retry that finds `started` is refused: "the finish_session call with this operation_id began merging and did not report its outcome; the owner can check with `repochat session` and `git log`". Nothing runs.
- `StartCall` and `AbandonCall` record `done` only, with the same replay rule.
- A different digest is refused as in Task 2.

- [ ] **Step 1: Write the failing tests:**
  - `TestFinishReplayAfterTheSessionClosed`: a `FinishCall` with an op, then the same call again. The same `Commit`, `Replayed == true`, no error, and the owner's branch is unchanged by the second call.
  - `TestFinishStartedReceiptRefuses`: the test writes a `started` receipt for op X into `operations.json`. `FinishCall` with op X → the error contains "did not report its outcome", and the owner's branch is unchanged.
  - `TestFinishReportsTheReviewedRevision`: `Finished.Head` is the session head the call expected and `Finished.Base` is the session's base.
  - `TestStartFreshReplayDoesNotStartAThirdSession`: `StartCall(op, fresh=true)` twice → the same `SessionInfo.ID`, and the second has `Replayed == true`.
  - `TestAbandonReplay`: the second call returns `replayed == true`, no error.
  - `TestReceiptsKeepTheNewest64`: 65 abandon/start pairs with distinct ops → 64 entries, and the first op is gone.
- [ ] **Step 2:** The tests fail.
- [ ] **Step 3:** Implement as specified.
- [ ] **Step 4:** `go test ./internal/repo/ -count=1` passes.
- [ ] **Step 5:** Commit: `Write mode: finish, start and abandon keep receipts; finish reports the revision it merged`.

### Task 4: The tools carry `operation_id` and `expected_checkpoint` (M3, M6, M9)

**Files:** Modify `internal/tools/write.go`. Test `internal/tools/write_test.go`, `internal/tools/writeboundary_test.go`.

**Interfaces:**
- Consumes the `…Call` methods, `OperationIDRe`, `ExpectRe`, and the `Replayed` flags (Tasks 1–3).
- Produces:
  - `func argsDigest(tool string, a mcp.Args) string`: the first 16 hex characters of SHA-256 over `tool + "\x00" + json.Marshal(a without "operation_id")`. `encoding/json` sorts map keys, which makes the encoding canonical.
  - Parameter `operation_id` (String, MaxLen 64) on `start_session`, `apply_changes`, `undo_to_checkpoint`, `update_session`, `finish_session` and `abandon_session`. Description: "An id you choose for this call, 8–64 letters, digits, `.`, `_` or `-`, new for every call. If the result does not arrive, call again with the same operation_id and exactly the same arguments: work already done is reported, not repeated."
  - Parameter `expected_checkpoint` (String, MaxLen 40): optional on `apply_changes`, `undo_to_checkpoint` and `update_session`. On `finish_session` it is required, but enforced by the handler, not by `Param.Required`: the server validates the schema before the handler runs, so a schema-level `Required` could only ever produce the generic missing-argument error (Review Focus 2). There its description begins "Required." Description: "The checkpoint the session is at, as the last session_status, start_session or apply_changes result showed it. Refused if the session has moved since."

Results and instructions:
- A replayed result begins "Operation <id> was already done: <what>. Nothing was done again." and carries `"replayed": true` in `@meta`. For checkpoint results, add "Read the files again for fresh anchors."
- The `finish_session` result adds:
  - "Merged revision: session at <Head[:12]>, from <Base[:12]>."
  - "Full diff: `git diff <start_branch>...<session_branch>` or `repochat session diff`."
  - "No checks were run: repochat cannot run commands yet."
  - `@meta` gains `head` and `base`.
- `WriteInstructions` gains one sentence at the end: "Pass a new operation_id with every call that changes something, and expected_checkpoint (the latest checkpoint you saw) with finish_session."
- `TestWriteInstructionsAreExact` is updated to the new text. Read-mode texts are untouched.

- [ ] **Step 1: Write the failing tests:**
  - `TestApplyReplayThroughTheTool`: `apply_changes` with the same `operation_id` twice. The second text starts "Operation", `@meta` has `"replayed":true`, and `list_checkpoints` shows one change.
  - `TestOperationIDAndExpectedCheckpointAreValidated`: `operation_id "short"`, `"../../etc"` and 65 characters, and `expected_checkpoint "xyz1234"`, are each refused, and nothing changes.
  - `TestFinishWithoutExpectedCheckpointExplainsWhy`: a `finish_session` call without the parameter returns an error text that names `expected_checkpoint`, `session_status` and "Refresh tools".
  - `TestFinishHandoffNamesTheRevision`: the result contains "Merged revision", "repochat session diff" and "No checks were run".
  - `TestArgsDigestIgnoresOperationIDAndKeyOrder`: a digest of the same arguments in another key order, or with another `operation_id`, is equal; it differs when the tool or any other argument differs.
  - In `writeboundary_test.go`, every `finish_session` case gains a valid `expected_checkpoint`, and the hostile-value loop also tries the new parameters with `..`, absolute paths, NUL and 10 KB values.
- [ ] **Step 2:** The tests fail.
- [ ] **Step 3:** Implement. `finishSession` refuses a call without `expected_checkpoint` with the explanatory text, before calling `FinishCall`.
- [ ] **Step 4:** `go test ./internal/tools/ -count=1` passes, including both read-mode golden tests unchanged.
- [ ] **Step 5:** Commit: `Write tools: operation_id and expected_checkpoint; finish_session names the revision it merges`.

### Task 5: The `format` operation (explicit gofmt)

**Files:** Modify `internal/repo/changeset.go` (`checkShape`, `gather`, `editFile`, `checkContent`), `internal/tools/write.go` (`changesSchema`). Test `internal/repo/changeset_test.go`.

**Interfaces:** Produces `Change{Op: "format", Path: p}`, which takes no other field; `checkShape` refuses any other field set.

Rules:
- `p` ends in `.go`.
- At most one `format` per path.
- Refused when the same change set removes or moves `p`.
- Applied after every other operation on `p`, or alone on an existing file.
- `format.Source` runs on the file's final content.
- Refused, as the whole change set, when:
  - the final content holds a masked line: "holds a masked value; gofmt could move it: format it by hand";
  - the file has CRLF line endings or a BOM;
  - `format.Source` fails (with `goMessage`);
  - the file is already formatted and `format` is its only operation: "already formatted as gofmt formats it; nothing to apply".
- For a formatted file, `Regions`, the file's part of `Plan.Diff`, `Added` and `Removed` come from one hunk between `Before` and the final `After`, after trimming the common leading and trailing lines.
- The "not formatted as gofmt would format it" warning is not given for a file with `format`.

The schema enum gains `format`. The op description gains: "format: run gofmt on the .go file at path after the other operations on it (needs no other field)."

- [ ] **Step 1: Write the failing tests:**
  - `TestFormatAloneReformatsAGoFile`: a file with `func f( ){}` and a format-only set → its content equals `format.Source` of the original, `Plan.Warnings` is empty, and the fresh anchors cover the changed lines.
  - `TestFormatAfterEditsInTheSameSet`: a replace plus a format on the same file → the result is formatted and contains the replacement.
  - `TestFormatRefusesCRLFBOMAndMaskedFiles`: each case is refused and the file is byte-identical.
  - `TestFormatRefusesNonGoRemovedMovedAndRepeated`: each case is refused.
  - `TestFormatOfAFormattedFileIsRefused`.
- [ ] **Step 2:** The tests fail (unknown op).
- [ ] **Step 3:** Implement as specified.
- [ ] **Step 4:** `go test ./internal/repo/ ./internal/tools/ -count=1` passes.
- [ ] **Step 5:** Commit: `Change sets: a format operation runs gofmt on a Go file`.

### Task 6: `repochat session diff` (G8)

**Files:** Modify `internal/cli/sessioncmd.go`. Test `internal/cli/write_test.go`.

**Interfaces:** Consumes `Sessions.Status` and `Repo.Diff`.

`repochat session diff [--checkpoint ID] [--stat]`:
- Without `--checkpoint`: `r.Diff(ctx, repo.DiffOptions{Mode: "range", Base: st.Info.StartBranch, Target: st.Info.Head, MergeBase: true, StatOnly: stat}, terminalDiffBudget)`, with `const terminalDiffBudget = 64 << 20`.
- With `--checkpoint ID`: `Base: ID + "^", Target: ID`.
- Prints `res.Text`. When `len(res.Omitted) + len(res.Cut) > 0` or `res.Incomplete`, ends with "Not everything fit: `git diff <start>...<branch>` shows the rest."
- No session open: "No session is open for …" and exit 0.
- `--checkpoint` and `--stat` are options of `diff` only, in the same `only` map as today's.

- [ ] **Step 1: Write the failing tests:**
  - `TestSessionDiffShowsWhatFinishWouldLand`: apply a change, then `repochat session diff` → the output names the file and contains the new line.
  - `--stat` lists the file without patch lines.
  - `--checkpoint <id>` shows only that checkpoint's change.
  - No session → exit 0 with the message.
  - `--stat` given to `session finish` → exit 2.
- [ ] **Step 2:** The tests fail.
- [ ] **Step 3:** Implement as specified.
- [ ] **Step 4:** `go test ./internal/cli/ -count=1` passes.
- [ ] **Step 5:** Commit: `repochat session diff: the session's whole change, or one checkpoint, in the terminal`.

### Task 7: `repochat session undo` (G8)

**Files:** Modify `internal/repo/session.go` (`SessionOptions`, `ensure`), `internal/cli/session.go`, `internal/cli/sessioncmd.go`. Test `internal/cli/write_test.go`, `internal/repo/session_test.go`.

**Interfaces:**
- Consumes `UndoToCall` (Task 1).
- Produces `SessionOptions.NoResync bool`. In `ensure`, when `rec.SyncedWith` differs from this process's digest and `NoResync` is set, return an error and do not sync: "the session was last brought in line by a repochat run with other exposure options; run this with the same options as the share, or undo from ChatGPT".
- `func editSessions(r *repo.Repo, opts repo.Options, notify func(string)) (*repo.Sessions, error)` in `internal/cli/session.go`: non-observing, with `NoResync: true`.

`repochat session undo <checkpoint> [--yes]`:
- The checkpoint is the positional word after `undo`.
- Prints the session status, asks "Undo to <id> as a new checkpoint?" (default no; `--yes` skips the question; without a terminal and without `--yes` it does nothing, like finish).
- Then `UndoToCall(ctx, repo.Call{Expect: st.Info.Head}, id)`.
- Prints "The session is at checkpoint <new> (undo to <id>)" and records a `sessionNote`.
- Without a checkpoint argument: exit 2.

- [ ] **Step 1: Write the failing tests:**
  - `TestSessionUndoFromTheTerminal`: apply A then B, then `repochat session undo <A's id> --yes`. A new checkpoint of kind `undo`, and the workspace file holds A's content.
  - `TestSessionUndoRefusesOtherOptions`: the session was synced with `--deny 'x/*'`, and the terminal runs without it → error, head and files unchanged.
  - `TestSessionUndoNeedsConfirmation`: no terminal, no `--yes` → nothing changes.
  - `TestSessionUndoNeedsACheckpoint` → exit 2.
  - In `session_test.go`, `TestNoResyncRefusesInsteadOfSyncing`.
- [ ] **Step 2:** The tests fail.
- [ ] **Step 3:** Implement as specified.
- [ ] **Step 4:** `go test ./internal/... -count=1` passes.
- [ ] **Step 5:** Commit: `repochat session undo: restore a checkpoint from the terminal`.

### Task 8: Documentation and the full check

**Files:** `README.md`, `README.ro.md`, `CHANGELOG.md`, `docs/superpowers/specs/2026-11-25-cli-parity-design.md`.

- [ ] **Step 1:** Update the README write-mode section (both languages):
  - the two parameters and what a retry does;
  - `finish_session` needs `expected_checkpoint`;
  - the `format` operation;
  - `repochat session diff` and `repochat session undo` in the command lines and the commands table;
  - one line in "What it guarantees": "A call repeated with the same operation_id is not applied twice."
- [ ] **Step 2:** Add a CHANGELOG entry under "Nelansat" (Romanian, in the file's style). In the spec, item 2 gets "built on <date>; not yet used from a real ChatGPT session".
- [ ] **Step 3:** `make check` passes (vet, race-detector tests, the onboarding run).
- [ ] **Step 4:** Commit: `Docs: operation ids, expected checkpoints, format, session diff and undo`.
- [ ] **Step 5:** Push the branch and open a PR. CI must be green on the head commit. The PR says the new parameters need "Refresh tools" in ChatGPT.
