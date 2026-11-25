# repochat CLI-parity brainstorm — round 2

You are the same independent reviewer as in round 1. Do not write code; do not modify files. Your round-1 answer is in
`docs/design/cli-parity/round1-codex.json` and my round-1 brief in `docs/design/cli-parity/round1-prompt.md`; read both
first. Use web search for any external claim you rely on, and the code in the current directory for internal ones.

## What I verified locally after round 1

- V1 Your F07 and F09 hold: the 60 s deadline is a context deadline that `runTool` waits on (internal/mcp/server.go
  around line 717), and only stored plans have replay protection (internal/repo/apply.go storedPlan.applied).
- V2 The owner's machine: Ubuntu 24.04.5 on WSL2, kernel 6.18. `landlock_create_ruleset(NULL,0,VERSION)` returns ABI 7.
  `unshare -Urm true` succeeds (unprivileged user + mount namespaces work here). CONFIG_LSM includes landlock.
- V3 Known: native Ubuntu 23.10+ ships `kernel.apparmor_restrict_unprivileged_userns=1`, so unprivileged user
  namespaces are refused there unless an AppArmor profile allows the binary. Please confirm or refute with a source.

## My responses to round 1

ACCEPT (I take your counter-proposal as written, unless noted):
- G0 — a disposable real editing task first, measured; Tasks / progress / elicitation / image / widget probes run
  separately and do not block. If Plus writes fail, the parity work stops and goes back to the owner.
- G1 split into G1a (contract + Linux confinement feasibility, adversarial acceptance tests) and G1b (one offline Go
  verification profile). Owner-local task IDs with narrowly typed arguments first; free-form commands only after the
  boundary passes the same tests. Linux only; macOS execution stays unavailable until a separate feasibility result
  (your C2). Bounded wait with a durable handle for longer work (your C3).
- G2 as a distinct `apply_and_check` tool with operation receipts, only if measurements justify it.
- G3 as an owner-reviewed snapshot stored outside the repository, one root file, provenance header, drift reported,
  no recursive trust; the server-instruction exception made explicit.
- G4 as policy-filtered WIP capture with delta-only handoff; after the ordinary loop is dependable; paired with M10.
- G5: compiler diagnostics as G1 tasks; semantic navigation later, one ecosystem at a time; the hub spec's blanket
  rejection of language servers becomes a scoped deferral (I will edit the spec after consensus).
- G6 through the dependency inventory that G1 needs anyway.
- G7 as a bounded session brief (objective, acceptance criteria, steps, decisions, blockers, checkpoint, verification
  receipts), server facts separate from model prose.
- G8 before G1: aggregate diff and undo from the terminal with explicit checkpoint ids; widget tested early.
- T3 and all cuts C1–C10, with the amendments below.
- M1, M3, M4, M5, M6, M7, M8, M9, M10, M11, M12 as proposed.

PARTIALLY ACCEPT / PUSH BACK:

- **P1 (on G1 / M1 / C1) One input mechanism, not two.** You propose an immutable read-only projection, plus a
  disposable writable copy for projects that need source writes. I propose a single mode: every job runs in a
  disposable writable copy of one checkpoint's projection; nothing flows back. Reasons: read-only source turns ordinary
  suites (golden-file updates, tools writing caches or bytecode next to sources) into infrastructure failures that look
  like code failures (your M4 concern); two modes double the adversarial test matrix; flow-back is already refused in
  both of your variants. Cost: an overlay (overlayfs inside the user namespace, kernel ≥ 5.11) where available, else a
  full copy bounded by a size limit. Formatter/generator flow-back stays the later, validated import you described.
- **P2 (on M2) Execution input = the model's view, exactly.** The projection holds, for every file, the text the read
  tools would serve: masked values replaced by their `[REDACTED:kind]` placeholders, withheld files absent, no `.git`,
  no owner checkout, scrubbed environment, no network, no host IPC. The contract becomes: "executed code receives no
  repository content the model could not already read." A test that needs a real secret fails, and the readiness report
  (M7) names it. No raw-secret mode in this design. Toolchains and the dependency projection are the only other inputs,
  so the dependency inventory is where any remaining exposure lives.
- **P3 (on M11) What happens where namespaces are refused.** Mount + user namespaces give a projected filesystem that
  also hides names (stat/existence probes cannot see the owner's tree). Landlock alone cannot stop stat of a guessed path
  (your F51), and the 1.0 design already treats name guessing as a real threat. Proposal: namespaces + Landlock + seccomp
  are REQUIRED; where unprivileged user namespaces are refused (native Ubuntu 23.10+ default), execution is unavailable
  and `repochat doctor` says exactly why and what the owner can change. No silently weaker Landlock-only mode.
  Challenge this if you think a documented weaker mode is better for adoption.
- **P4 (on C3) Wait budget numbers.** Bounded wait 45 s, under the 60 s server context and the 120 s tunnel deadline;
  after that a handle. The supervisor (M5) — not the request context — owns the job, so request cancellation never
  kills or orphans it; cancellation is an explicit tool call or the terminal.

## Consolidated order v2

1. G0 — real Plus editing task, measured; optional probes on the side. Seeds the M8 task set.
2. Edit-loop hardening: M3 durable operation ids and receipts; M6 expected checkpoint/session generation on every
   mutating call; M9 revision-bound handoff (finish names the expected checkpoint); G8 terminal diff and undo; explicit
   Go formatting with fresh anchors (go/format, no subprocess).
3. G3 owner-reviewed guidance snapshot.
4. G1a — execution contract (P1, P2), Linux confinement (P3), supervisor (M5, P4), adversarial acceptance tests (M11).
5. G1b — Go verification profile: owner-local task ids, readiness report (M7), attributable receipts (M4).
6. G7 session brief.
7. G2 `apply_and_check`, if measured round trips justify it.
8. Free-form sandboxed command tool, same boundary, separate tool.
9. G4 WIP capture + M10 conflict resolution.
10. G5 more ecosystems' diagnostics; semantic navigation for one ecosystem when measured.
11. G6 dependency sources over the inventory.
12. Project 4 (hub); widgets, image/browser verification, publishing only on demonstrated need.

## Your task (round 2)

For every id I pushed back on (P1–P4) and for the order v2: ACCEPT, MAINTAIN (say why, with evidence) or ESCALATE
(clarify what I misunderstood). Then list anything new that this version breaks or still misses. Set `consensus` to
"yes" only if you would sign this order and these boundaries as the plan; otherwise list what remains. Agreeing to end
the debate is not helping; agree only where the position is actually solid.

Respond only with JSON matching the provided schema.
