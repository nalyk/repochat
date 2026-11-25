# ChatGPT + repochat as a coding agent: what is missing, and in what order

Project: [`nalyk/repochat`](https://github.com/nalyk/repochat) · 2026-11-25 · status: **consensus with Codex CLI after four rounds; pushed to main at the owner's request on 2026-11-25**

The debate is in [`docs/design/cli-parity/`](../../design/cli-parity/): each round's prompt and Codex's answer, as JSON validated against a schema (`codex exec`, read-only sandbox, live web search). Codex signed in round 4: "I sign the cumulative rounds 1–4 plan, including every accepted counter and the final order." That covers the design. Implementation and whether each platform can support it stay subject to the gates below.

## The question

What does repochat lack for ChatGPT on the web to work on a local repository the way Claude Code CLI or Codex CLI does: explore, plan, edit, run, verify, iterate, and hand back reviewable work?

## The answer in one paragraph

In the CLIs, the model and its tools are one product. Here ChatGPT owns the model and the loop, and repochat owns the hands. The hands read well and edit (experimentally), but cannot run anything, so the model cannot check its own work. That is the largest gap. It still does not come first. First, the editing that already exists has to be proven in a real ChatGPT Plus session and made dependable. Command execution then arrives as verification against an isolated, sanitized copy of a checkpoint: Linux only, with every isolation layer required, and Go first.

## Facts this rests on (selected; all with sources in the transcripts)

- ChatGPT supports write tools on custom MCP servers and asks for confirmation per tool. It can remember an approval for the conversation, per tool, not per argument (round 1, F11–F13). Whether this Plus account may call them was **unverified** when the debate ran (F14); the owner's write test on 2026-11-25 settled it (see G0).
- Secure MCP Tunnel: 120 s per request including queueing, and progress notifications do not reset it; 10 MiB per message; 600 requests/min (F18–F20). No documented ChatGPT-side tool timeout or tool calls per turn (F21, F22).
- MCP Tasks are an optional extension in 2026-07-28, not the 2025-11-25 mechanism (F25, F26). Support in ChatGPT is unverified (F27). Image results and widgets through the tunnel are unverified (F24, F34).
- Codex: separate sandbox and approval policies; Linux uses bwrap + seccomp, macOS uses Seatbelt through `sandbox-exec` (F37, F39, F40). Apple does not support custom Seatbelt policies for third parties (F53).
- Landlock cannot stop `stat`/`access` probes (F51, F61); seccomp is not filesystem isolation (F52).
- Ubuntu restricts unprivileged user namespaces through AppArmor by default since 24.04 (round 2, F59). The owner's WSL2 kernel 6.18 reports Landlock ABI 7 and `unshare -Urm` works (measured locally, not by Codex).
- repochat today: a session worktree shares the owner's object database (F03); only stored plans are replay-protected (F09); the 60 s deadline is a context, not a forced stop (F07, F65). Masking runs over the whole line before the 2000-byte display cut (round 3, F70).

## Order

1. **G0: real-session proof.** One disposable real task from a Plus account: discover tools, start a session, read anchors, a multi-file edit, recovery from a stale anchor, checkpoints, undo, finish. Measure confirmations, calls, latency, truncation, interruptions, and whether the Codex allowance moved. Record account, model, protocol, client and date. Probes for Tasks, progress, elicitation, images and a minimal widget run separately and block nothing. **If Plus cannot call write tools, the parity work stops and goes back to the owner.** The run seeds a small repeatable task set (M8).
   **2026-11-25: the gate is passed.** A write test from the owner's real ChatGPT session, with write mode as built, succeeded. The measured task and the side probes are still to do.
2. **Harden the edit loop.** (Built on 2026-11-25; not yet used from a real ChatGPT session.)
   - Durable operation ids and receipts (M3), so a lost response can be queried and a retry cannot apply twice.
   - The expected session generation and checkpoint on every mutating call (M6).
   - A handoff bound to a revision: `finish_session` names the checkpoint it expects (M9).
   - `repochat session diff` and `repochat session undo` with explicit checkpoint ids, plus a complete-diff path (G8).
   - Explicit Go formatting through `go/format` with fresh anchors.
3. **G3: owner-reviewed guidance.** A snapshot of one root AGENTS.md / CLAUDE.md is stored outside the repository, bound to the repository and its path, and served with a provenance header. Drift from the source is reported, nothing is trusted recursively, and the server instructions make the exception explicit.
4. **G1a: the execution contract and Linux confinement.** See "Execution boundary". Exit criteria: confinement and resource limits demonstrated, adversarial tests passing, the false-pass test present.
5. **G1b: one offline Go verification profile.** Task ids declared by the owner, with typed arguments. A readiness report (M7, M16), demonstrated offline resolution, one useful check, unsupported checks reported as such, and receipts (C-M14).
6. **G7: session brief.** Objective, acceptance criteria, steps, decisions, blockers, current checkpoint and verification receipts. Facts come from the server, prose from the model, and the brief is shown by `session_status`.
7. **G2: `apply_and_check`.** A separate tool with its own approval, built only if measured round trips justify it. A failing test leaves the edit applied; the result returns the checkpoint plus the receipt.
8. **A free-form sandboxed command tool.** A separate tool, behind the same boundary, after that boundary passes the same tests.
9. **G4 + M10: the owner's uncommitted work and conflicts.** Policy-filtered capture of the work in progress (HEAD, index, files, selected untracked files). The handoff carries only the agent's changes, against the owner's current files. Then structured conflict resolution inside a disposable transaction.
10. **G5: language intelligence.** Compiler and type-checker diagnostics, one ecosystem at a time, as tasks. Semantic navigation for one ecosystem only when the measured tasks call for it.
11. **G6: dependency sources.** Read through the dependency inventory that execution already authorizes, with package, version and path provenance.
12. **Later, on demonstrated need:** the multi-repository hub (project 4), widgets, image/browser checks, publishing (push/PR).

## Execution boundary (G1a)

**Isolation.** Linux user, mount and network namespaces, Landlock, seccomp and delegated cgroup v2 are **all required**. Where one is missing, execution is unavailable and `repochat doctor` says why and what the owner can change, for example a narrow AppArmor profile. Doctor runs the real launcher steps, not version checks. There is no weaker Landlock-only mode. macOS execution waits for a separate feasibility result.

**Inputs (C-P2, projection v1).** Each job runs in a fresh, writable, disposable copy of one checkpoint's projection. Nothing flows back. A full copy is fine first; an overlay is an optimization, never on top of the live worktree or the owner's checkout. The projection is defined by the exposure rules (membership + masking), not by how the read tools display content.

| Class | In the projection |
|---|---|
| valid UTF-8 text | original bytes, every masked span replaced by its placeholder |
| UTF-16 with BOM | decoded, masked, re-encoded with the same BOM; unsupported if the round trip changes any byte outside the masked spans |
| invalid UTF-8, NUL-bearing, binary | omitted by default; an owner-local allowlist may admit named classes, as a recorded expansion |
| a line > 64 KiB, or the masker over its limits | omitted |
| symlink | recreated only if its target is projected |
| submodule | empty directory |
| withheld, outside membership | absent |

There is no `.git`, no owner checkout and no host environment; the network is per profile.

**Explicit decision:** the whole of an allowed line is permitted content, even where the display shortened it.

`projection_digest` = SHA-256 over the manifest (path, mode, content hash), plus the policy and projection versions.

Equivalence test: for every projected text file, the decoded logical lines (after reader normalization) equal the shared reader's masked lines before display transformations. Separate assertions check that every unmasked byte range is preserved, including line endings and BOM.

Omissions are counted by category only; names of withheld files never reach the model.

**What a check means (C-M14).** Each receipt records:
- the checkpoint, the projection digest and versions;
- transformations (N masked values in K files) and omissions by category;
- the task id, the hash of its definition and the digest of the concrete arguments;
- the toolchain and both dependency inventories, the network profile and the cache mode;
- the exit status, and counts of passed, failed, skipped and unsupported checks;
- whether sources in the copy changed during the job.

The model-facing label is "checked on a sanitized projection of checkpoint C", never "verified". The acceptance suite includes the false-pass case: two distinct credential-shaped constants compare equal once masked.

**Network (C-M15).** Two profiles, neither of which reaches the host or the internet. `none` allows only AF_UNIX socketpairs. `job-local` gives the job its own network namespace: loopback only, plus a private abstract Unix socket namespace. `job-local` is the Go default, because repochat's own tests need loopback TCP and Unix sockets. The denial tests cover host pathname sockets and inherited descriptors.

**Resources (C-M17).** A delegated cgroup v2 subtree with `memory.max`, `memory.swap.max`, `pids.max`, `cpu.max`, and `cgroup.kill` for cleaning up descendants, plus a size and inode limit on the writable storage. The supervisor sits outside the job's subtree, and repository code cannot reach the cgroup controls.

**Jobs (M5, P4).** The supervisor owns each job, not the request. A call waits at most 45 s for the result, capped by the time left on the request deadline minus a margin for delivering the response. After that it returns a handle. The link between operation and job is persisted before the call is acknowledged. Cancelling the request ends only the waiting; ending the job is an explicit tool call or a terminal action. Restart, abandon and revoke have defined outcomes.

**Go dependencies (C-M18).** Two recorded inventories:
1. A resolution-metadata closure authorized by the owner: `.info`, `.mod`, checksum evidence and `.ziphash` for every version the resolver needs, including versions it does not select. Go's own 1.25 test suite has a case that needs exactly this. It is staged before resolution.
2. The selected and authorized source modules, which are the only ones admitted for execution.

Beyond the inventories:
- Missing inputs make the task unsupported; no mount is widened and no network is turned on to fill them.
- The toolchain is pinned and recorded.
- Workspace and vendor modes are each explicitly supported or rejected.
- Build and test caches are private.
- Tests run with `go test -count=1`.
- Local `replace` directories are admitted only if they are inside the projection.
- Prebuilt binaries are rejected.

**What changes in the guarantees.** The write-mode rule "no program configured by the repository runs" stays true for every tool except the execution tools. For those it becomes: repository code runs only inside this boundary, and receives no repository content the model could not already read.

## Not built for parity

- What ChatGPT already provides: web search, conversation history, Projects, memory.
- Imitations of subagents, a conversation store of our own, delegating to another model.
- An execution mode that sees real secret values; mounting the whole of HOME or whole caches.
- A weaker execution mode; macOS execution before a feasibility result.
- A universal LSP layer, persistent indexes, and `repo_map`/`get_context` in the first milestone.
- Annotations chosen only to make ChatGPT ask for more confirmations.
- Push/PR as part of the core.

## Decisions taken in the debate (author's calls, accepted by Codex)

- A single execution mode, the disposable copy (P1).
- Execution input = the model's view (P2 → C-P2).
- No execution where namespaces are refused (P3).
- 45 s wait cap with a handle (P4).

The owner has not decided any of these yet.

## Left to the builder

The format of operation ids and receipts, numeric resource limits, the readiness report format, the location of the guidance snapshot, the formats of the task definition and the projection allowlist.
