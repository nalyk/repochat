# Hub multi-repository + code intelligence — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One ChatGPT plugin serves any number of repositories at once (capabilities, conversation binding, router, hub), and the server gains measured code-intelligence tools — all in one release, as the owner asked.

**Architecture:** A background hub owns the tunnel and starts tunnel-client with `repochat mcp --router` as its child. The router serves the static tool list and forwards each call, through an atomic admit step in the hub, to the per-repository server process that today's `repochat` terminal runs on a private Unix socket. Code-intelligence tools live in the per-repository process and reuse the existing policy, masking and budget pipeline.

**Tech Stack:** Go 1.24, standard library only; git ≥ 2.25; OpenAI tunnel-client ≥ 0.0.21.

**Spec:** `docs/superpowers/specs/2026-11-18-un-plugin-mai-multe-repository-uri-design.md` (design agreed with Codex: `docs/design/hub/`).

## Global Constraints

- Single static binary; `cd repo && repochat`; first run stays one hidden paste.
- Read-only; no program configured by a repository is ever executed; no language servers; no third-party modules.
- Every new tool obeys the exposure policy: withheld paths never read or indexed; matching and ranking run on the disclosed (masked) text.
- Router: no repository operation, no exposure-policy evaluation, never receives the API key.
- Capability: 128 bits from `crypto/rand`, fail closed; required on every tunnel call; removed before every log/event/error sink.
- No open mode, no repository-listing tool, no global "active repository".
- Local stdio paths (`repochat mcp`, `call`, `doctor`, `mcp-config`) stay single-repository without a code.
- Claims in docs: capability = permission, not intent; binding = "after the initial binding, a conversation cannot access another repository without an explicit local owner grant"; without a verified conversation identity, binding is documented as not active.
- `get_context`/`repo_map` claims only what the benchmark shows; results labelled heuristic; occurrences labelled "lexical, not proven".

## Review Focus

1. A caller with a cached pre-upgrade tool list (no `repo` argument) → refused with "refresh the plugin's tools", never routed to a default repository. (Task 5)
2. Two first calls of one conversation racing with different valid codes → exactly one binds. (Task 4)
3. A repository terminal killed with SIGKILL → its repository stops being served within the lease time; no stale socket serves it. (Task 6)
4. A capability pasted inside a `query` or `path` argument → absent from terminal lines, activity log and errors. (Task 3)
5. A file edited between `repo_map`/`find_symbol` and the follow-up read → positions re-derived, never served stale. (Task 9)

---

## File structure

| File | Responsibility |
|---|---|
| `internal/mcp/server.go` (modify) | pass request `_meta` and caller identity to handlers and events |
| `internal/hub/capability.go` | generate, format, parse, constant-time verify; scrub from strings |
| `internal/hub/ipc.go` | versioned envelope, framing, backend listener for one repository |
| `internal/hub/store.go` | associations, grants, tombstones, epoch; private atomic persistence |
| `internal/hub/admit.go` | atomic bind/check/admit; pending approvals and pairing phrases |
| `internal/hub/router.go` | static catalogue with `repo`, forwarding, correlation, cancellation |
| `internal/hub/daemon.go` | hub process: lock, tunnel-client, leases, control socket, idle exit |
| `internal/cli/run.go`, `serve.go`, `inspect.go`, `cli.go` (modify) | attach flow, `--router`, `allow`, `revoke`, `chats`, `stop --all`, `--new-code` |
| `internal/intel/symbols.go` | definition records per file (Go parsed, others heuristic), content-fingerprinted, in memory |
| `internal/intel/occurrences.go` | language-aware identifier occurrences, classified, labelled |
| `internal/intel/repomap.go`, `context.go`, `changes.go`, `archmap.go`, `stats.go` | the five tools' logic |
| `internal/tools/tools.go` (modify) | register new tools, `max_bytes` on every tool |
| `bench/` | frozen fixtures, tasks, deterministic baseline, runner, gate |

---

### Task 1: Request metadata reaches handlers

**Files:** Modify `internal/mcp/server.go`, `internal/mcp/server_test.go`.

**Interfaces — Produces:** `type Caller struct{ Session, Subject, Client, Protocol string; HasMeta bool }`; `func CallerFrom(ctx context.Context) Caller`; `Event.Session string` (salted digest set by the CLI, never the raw id).

- [ ] Test `TestCallerMetadataReachesHandler`: a modern request with `_meta["openai/session"]="s1"` → handler sees `Session=="s1"`; legacy request → `HasMeta==false`; a `session` key inside `arguments` is ignored.
- [ ] Implement; run `go test ./internal/mcp/`; commit.

### Task 2: Capabilities

**Files:** Create `internal/hub/capability.go`, `capability_test.go`.

**Interfaces — Produces:** `func New(name string) (Capability, error)` (128 bits, error when `crypto/rand` fails); `func (c Capability) String() string` → `name-` + 26 lower-case base32 chars; `func Parse(s string) (Capability, bool)`; `func (c Capability) Equal(other string) bool` (constant time); `func (c Capability) RouteID() string` (hex SHA-256 prefix, 32 chars); `func Scrub(s string) string` replaces anything matching `[A-Za-z0-9._-]*-[a-z2-7]{26}` with `[repo-code]`.

- [ ] Tests: format and length; two codes differ; `Parse` rejects 25/27 chars and upper case; `Equal` true only for the exact code; `Scrub` removes a code embedded in a longer string and leaves `token.go-abc` alone.
- [ ] Implement; test; commit.

### Task 3: IPC envelope and the per-repository backend

**Files:** Create `internal/hub/ipc.go`, `ipc_test.go`; modify `internal/cli/serve.go`, `session.go`.

**Interfaces — Produces:** `type Request struct{ V int; ID uint64; Capability string; Generation uint64; Caller mcp.Caller; Tool string; Arguments json.RawMessage; DeadlineMS int64 }`; `type Response struct{ V int; ID uint64; Text string; IsError bool; ProtocolError *ProtoErr; Redactions int }`; `const IPCVersion = 1`; `func ServeBackend(ctx, ln net.Listener, cap Capability, generation func() uint64, call func(ctx, tool string, args json.RawMessage, c mcp.Caller) Response) error` — newline-delimited JSON, one goroutine per request, cancel message `{"v":1,"cancel":ID}`.

- [ ] Tests: wrong capability → generic `ProtocolError` identical to unknown tool-less refusal; wrong version → explicit version error; stale generation refused; cancel stops the handler; a capability inside `Arguments.query` never appears in the recorded event (`Scrub` applied in the event sink, `--audit-args` included).
- [ ] Backend `call` runs today's tool pipeline (`mcp.Schema.Validate` then handler) so repository rules are unchanged.
- [ ] Test; commit.

### Task 4: Associations, grants and atomic admission

**Files:** Create `internal/hub/store.go`, `admit.go`, tests.

**Interfaces — Produces:** `type Store` persisted as one 0600 JSON file with `Epoch`; `func (s *Store) Admit(scope, session, routeID string, generation uint64) Decision` where `Decision` ∈ `Allow`, `RefuseBound{Name, Phrase}`, `NeedOwner{Phrase}`, `RefuseNoIdentity`, `RefuseFull`; `func (s *Store) Approve(phrase string, routeID string) error`; `func (s *Store) Rotate(routeID string)`; `func (s *Store) Revoke(session string)`; `func (s *Store) Prune(olderThan time.Duration) int`. Sessions are stored as salted SHA-256 digests. Values: association idle expiry 30 days → tombstone; table cap 10 000; pairing phrase three random words from a built-in 256-word list plus two digits, single use, 5-minute expiry, 5 wrong attempts.

- [ ] Tests: first call binds; second repository refused with a phrase; `Approve` grants exactly that pending request; replayed/expired/unknown phrase refused; 50 goroutines racing two codes for one session → one binding; `Rotate` drops grants but keeps the association; lost store file → new `Epoch` → every session gets `NeedOwner`; tombstoned session gets `NeedOwner`, not a fresh binding; full table → `RefuseFull`, nothing evicted; `--confirm-new-chats` → first call `NeedOwner`; no session + binding required → `RefuseNoIdentity`; no session + `AllowUnidentified` → `Allow`.
- [ ] Implement with one mutex around bind/check/admit; atomic file replace; commit.

### Task 5: Router

**Files:** Create `internal/hub/router.go`, `router_test.go`; modify `internal/tools/tools.go` (export the catalogue without handlers), `internal/cli/serve.go` (`mcp --router`).

**Interfaces — Consumes:** Tasks 1–4. **Produces:** `func NewRouter(cat []mcp.Tool, dir string, admit func(...) Decision) *mcp.Server` — same tools, each schema with required `repo` first.

- [ ] Tests (two fake backends holding sentinels A and B): each code reaches only its backend; missing `repo` → "refresh the plugin's tools"; unknown, offline and revoked codes give byte-identical errors; cancellation propagates; a late response after cancel is dropped; duplicate upstream id rejected; backend `IsError` and protocol errors preserved; tool list identical with 0, 1 and 2 backends attached; the router process environment contains no `CONTROL_PLANE_*`.
- [ ] Implement; commit.

### Task 6: Hub daemon and attach flow

**Files:** Create `internal/hub/daemon.go`; modify `internal/cli/run.go`, `inspect.go`, `cli.go`, `config.go`, tests in `internal/cli/`.

**Interfaces — Produces:** hidden command `repochat hub --profile P`; control socket ops `attach{routeID,name,socket,lease}`, `renew`, `detach`, `status`, `stop`, `approve{phrase}`, `events`; lease 10 s, renewed every 3 s; idle exit 20 s after the last detach. New user commands: `repochat allow <phrase>`, `repochat revoke`, `repochat chats [--prune]`, `repochat stop --all`, flags `--new-code`, `--confirm-new-chats`, `--allow-unidentified`. Capability stored per (worktree identity, exposure profile) in the state dir, 0600; widening exposure issues a new code.

- [ ] Tests with the stand-in tunnel-client: two repositories attach to one tunnel and both answer; SIGKILL of one terminal → refused within 12 s while the other keeps working; hub exits after the last detach; second start attaches (no takeover prompt); old-version owner on the lock → refusal telling the user to stop it; `stop --all` not undone by a live attachment; `--bind` profile refuses a second repository; exposure widened → new code, old one dead.
- [ ] Update `test/onboarding_pty.py`: first run prints one copyable starter line containing the code; nothing asked before the hidden prompt.
- [ ] Real tunnel-client contract test: two repositories, ONE tunnel, sentinel isolation, wrong code refused, binding refusal + `allow`.
- [ ] Commit.

### Task 7: Byte budgets and `context_stats`

**Files:** Create `internal/intel/stats.go`; modify `internal/tools/tools.go`, `internal/repo/repo.go`.

- [ ] Every tool accepts `max_bytes` (4 096–2 097 152); default budget becomes 32 KiB; the final encoded result including `@meta` never exceeds it (test with every tool).
- [ ] `context_stats`: per-attachment calls, bytes, latency percentiles, repeated-span bytes; states its scope; no token or cost claims.
- [ ] Commit.

### Task 8: Benchmark harness (before the retrieval tools)

**Files:** Create `bench/fixtures/{go,ts,php}/` (frozen small repositories), `bench/tasks.json` (8 tasks per language: 2 tuning, 6 sealed; gold evidence as path + line spans), `bench/baseline.go` (deterministic policy over `find_files`, `search_code`, `file_outline`, `read_files`), `bench/run_test.go`.

- [ ] Gate, predeclared in `bench/GATE.md`: at 4, 16 and 64 KiB — ≥ 30 % fewer returned bytes or fewer calls than the baseline at recall within 5 points; no baseline-success task with zero recall; zero withheld or masked canary in any output.
- [ ] `make bench` prints a per-task table and exits non-zero when the gate fails. Commit.

### Task 9: Symbol records, `find_symbol`, `symbol_context`, occurrences

**Files:** Create `internal/intel/symbols.go`, `occurrences.go`, tests; modify `internal/repo/outline.go` to return records.

**Interfaces — Produces:** `type Def struct{ Path, Name, Kind, Container, Signature string; Line, EndLine int; Exact bool }`; `func (ix *Index) Defs(ctx, path) ([]Def, error)` cached by (path, size, mtime, policy hash), built from masked text; `func (ix *Index) Find(ctx, name string, limit int) []Def`; `func (ix *Index) Occurrences(ctx, name string, limit int) []Occ` with `Occ.Class` ∈ `definition`, `call-like`, `other`.

- [ ] Tests: Go method found with receiver; TS/PHP/Python heuristics labelled `Exact=false`; `$var` and Unicode identifiers respect language boundaries; a withheld file contributes nothing; a masked token cannot be found by name; edit a file → next call reflects it; output says "lexical, not proven".
- [ ] Commit.

### Task 10: `repo_map` and `get_context`

**Files:** Create `internal/intel/repomap.go`, `context.go`, tests.

- [ ] `repo_map`: ranked definitions with file diversity, bounded scan, reports coverage ("N of M files examined").
- [ ] `get_context(query, max_bytes)`: split query into terms, score paths/definitions/lines, select excerpts with dedup, add likely tests by naming convention; output lists selected evidence, what was omitted and that selection is heuristic.
- [ ] Run `make bench`; record the result in `bench/RESULT.md` as measured, pass or fail. Commit.

### Task 11: `analyze_changes`

**Files:** Create `internal/intel/changes.go`, tests.

- [ ] From `git_diff` modes or a range: changed definitions (hunk → enclosing `Def`), their occurrences elsewhere, likely tests; withheld changes counted only. Tests on the history fixture. Commit.

### Task 12: `architecture_map`

**Files:** Create `internal/intel/archmap.go`, tests.

- [ ] Import/manifest graph: Go imports via `go/parser`; JS/TS/PHP/Python import statements by pattern; manifests parsed as data; edges labelled `observed`, components labelled `inferred`; never runs `go list` or a package manager. Commit.

### Task 13: Documentation, review and release checks

- [ ] README, `repochat help`, CHANGELOG: new flow, exact guarantees and residuals from the spec, benchmark result as measured.
- [ ] `repochat doctor` reports hub state and whether a conversation identity has been observed on this installation.
- [ ] Full suite with race detector, real tunnel-client contract suite, pty onboarding, CI on Linux and macOS.
- [ ] Adversarial review with Codex until no critical or major finding remains; record under `docs/design/hub/`.
