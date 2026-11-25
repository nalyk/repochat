# repochat CLI-parity brainstorm — round 3 (final text)

Same reviewer, same rules: read-only, no code, web search for external claims, the code for internal ones. Read
`docs/design/cli-parity/round1-*` and `round2-*` first. Your round-2 answer accepted P1, P3, P4 and the order v2; it kept
P2 open on M13/M14 and added M15–M18. Your V3 correction is accepted: the Ubuntu restriction exists since 23.10 and is
enabled by default since 24.04.

## My responses to round 2

ACCEPT M13, M14, M15, M16, M17, M18, and your counters to P1, P3, P4. Concretely, the text below goes into the plan.

### C-P2 Canonical execution projection (closes P2 / M13)

The projection is defined at the content-policy layer, not from rendered output. Display transformations (line-number
headers, anchors, pagination, the 2000-byte line display cut, control-character escaping, the 32 KB result budget) are
NOT exposure rules and do not shape the projection. Exposure rules are: membership (git-listed, policy-allowed) and
value masking. One explicit decision follows and is written down as such: the full content of an allowed text line is
permitted content even where the display shortened it, because masking runs over the whole line (up to
`maxLineBytes`, 64 KiB, read.go) before any display cut. Lines longer than `maxLineBytes` make their file "unsupported".

Per file class, versioned as projection v1:

| Class | In the projection |
|---|---|
| valid UTF-8 text | original bytes, each masked span replaced by its placeholder; nothing else changed |
| UTF-16 text with BOM | decoded, masked, re-encoded with the same BOM and byte order; if the round trip is not byte-identical outside masked spans, unsupported |
| invalid UTF-8, NUL-bearing, binary | omitted (unsupported) by default; an owner-local projection allowlist may admit named binary classes, which is an explicit, recorded expansion |
| text file whose masker run exceeds a limit, or with a line > 64 KiB | omitted (unsupported) |
| symlink | recreated only if its target is a projected path; else omitted |
| submodule | empty directory |
| withheld, protected-from-reading, outside membership | absent |

Identity: `projection_digest` = SHA-256 over a sorted manifest of (path, mode, content SHA-256), plus
`policy_version` and `projection_version`. Independent of `Snapshot.Generation` (your F63). Omissions are recorded by
category count only (M16); the owner-local diagnostic with names stays out of model-facing output. A test asserts the
equivalence: for every projected text file, reading it through read_file with unlimited budget yields the same masked
content.

### C-M14 What a check means

Every receipt states: checkpoint id, projection digest and versions, transformations (N masked values in K files),
omissions by category, task id and definition hash, toolchain and dependency inventory ids, network profile, cache
mode, exit status, counts passed/failed/skipped/unsupported, whether sources in the job copy were modified during the
job (detected by comparing manifests after the job; not claimed as proof of no transient change). The model-facing label
is "checked on a sanitized projection of checkpoint C", never "verified". The acceptance suite includes the false-pass
case: two distinct credential-shaped constants compared for equality pass in the projection and fail on the original.

### C-M15 Network profile for G1b

Two profiles, both with no host and no external network: `none` (no sockets beyond what seccomp allows for
AF_UNIX socketpair) and `job-local` (a job-owned network namespace with loopback only; TCP/UDP/Unix sockets inside it,
abstract Unix namespace private). `job-local` is the Go profile default because repochat's own suite needs loopback TCP
and Unix sockets (your F66); the receipt names the profile. No profile reaches the host or the internet.

### C-M17 Resource control

cgroup v2 with a delegated subtree (memory.max, pids.max, cpu.max; `cgroup.kill` for descendant cleanup) plus a size
limit on the job's writable storage (tmpfs size= inside the mount namespace). If delegation is not usable, execution is
unavailable and doctor says why. Doctor runs the actual launcher steps (your P3 counter).

### C-M18 Caches and artifacts

Fresh private GOCACHE and test cache per job (or per projection digest, never shared with the host). `go test -count=1`
in the profile. The read-only module projection admits only modules in the resolved build graph (`go list -m -json
all` run inside the sandbox over the projection, against a read-only module cache copy restricted to those paths),
with local `replace` directories admitted only if they are inside the projection. Prebuilt binaries are not admitted.

### Final order (unchanged from v2, with your gate additions)

1. G0 real Plus editing task, measured; optional probes separate; seeds M8.
2. Edit-loop hardening: M3, M6, M9, G8 terminal diff/undo, explicit Go formatting with fresh anchors.
3. G3 owner-reviewed guidance snapshot.
4. G1a: C-P2 projection, C-M14 receipt meaning, Linux confinement (namespaces + Landlock + seccomp + cgroup v2,
   all required), supervisor (M5, P4 with the deadline-aware wait), C-M15 network profiles, C-M17, C-M18, adversarial
   acceptance tests (M11). Exit criteria include demonstrated containment and the false-pass test.
5. G1b one offline Go profile: owner-local task ids with typed arguments, readiness report (M7, M16), receipts.
6. G7 session brief. 7. G2 apply_and_check if measured. 8. Free-form sandboxed command tool.
9. G4 WIP + M10 conflicts. 10. G5 diagnostics by ecosystem, then semantic navigation for one. 11. G6 dependency
   sources over the inventory. 12. Hub; widgets, image/browser, publishing on demonstrated need.

Not built for parity: ChatGPT-native features (web search, history, Projects, memory) and subagent emulation;
macOS execution until a separate feasibility result; any raw-secret execution mode; a Landlock-only weaker mode.

## Your task (round 3)

For each of C-P2, C-M14, C-M15, C-M17, C-M18 and the final order: ACCEPT, MAINTAIN or ESCALATE, with evidence. Check
in particular: that masking really runs over the whole line before the display cut (internal/repo/read.go,
redact.go); that `go list -m all` inside the sandbox against a restricted cache is workable offline; that cgroup v2
delegation is realistic for an unprivileged user process (systemd user slices, WSL2). Set `consensus` to "yes" only if
you sign this as the plan. Respond only with JSON matching the provided schema.
