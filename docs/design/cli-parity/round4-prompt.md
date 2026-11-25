# repochat CLI-parity brainstorm — round 4 (signature)

Same reviewer, same rules: read-only, no code. Read `docs/design/cli-parity/round3-prompt.md` and
`round3-codex.json`. Round 3 accepted C-M14, C-M15, C-M17 and the final order, and kept C-P2 and C-M18 open on two
text corrections. I accept both corrections and every counter of round 3 verbatim. The amended contract text:

**C-P2, last sentence replaced by:** "For every projected text file, decoded logical lines, after reader
normalization, equal the shared reader's masked lines before display transformations. Separately assert preservation
of every unmasked byte range, including line endings and BOM. Reader display tests apply the same rendering
transformations to both representations." The policy, file classes, explicit long-line decision and digest stand.

**C-M18, the module-cache part replaced by:** two recorded inventories. (1) An owner-authorized resolution-metadata
closure: the cache metadata (`.info`, `.mod`, checksum evidence, `.ziphash`) for every module version the resolver
needs, including non-selected versions (your F73/F74), staged before any sandboxed resolution. (2) The selected
dependency-source projection: only selected, authorized source modules, admitted for execution. Missing inputs make the
task unsupported; mounts are never broadened and network is never enabled to fill them. The toolchain version is
pinned and recorded; workspace (`go.work`) and vendor modes are each explicitly supported or rejected in G1b, never
silently mixed. Private caches, `-count=1`, projected local replacements and rejection of prebuilt binaries stand.

**Also folded in from your round-3 counters:** receipts bind the canonical digest of the concrete typed arguments next
to the task definition hash (C-M14); M11 denial tests cover host pathname sockets and inherited socket descriptors
(C-M15); doctor exercises controller enablement, real limit enforcement and descendant cleanup, the supervisor stays
outside the job subtree with cgroup controls unreachable from repository code, and the limits include memory.swap.max
and an inode/storage limit (C-M17); G1a's contract carries both corrections, G1b demonstrates offline resolution and one
useful check (final order).

## Your task (round 4)

For C-P2 and C-M18 as amended, and for the plan as a whole (rounds 1–4): ACCEPT, MAINTAIN or ESCALATE. Set
`consensus` to "yes" only if you sign it. If anything still blocks, name the exact sentence to change. Respond only with
JSON matching the provided schema.
