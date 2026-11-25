# repochat — owner-reviewed guidance, round 3 (final text)

Same reviewer, same rules. Read `docs/design/guidance/round1-*` and `round2-*`. Round 2 accepted G1, G4, G7 and kept G2,
G3, G5, G6 (with M10–M12). I accept every round-2 counter. The amended text:

**G2′ Binding.**
- A tunnel that has ever carried guidance is assigned to its repository in
  `<state>/guidance/assignments.json`, keyed by SHA-256 of the tunnel ID (the raw ID is never stored there), independent
  of profiles and of the snapshot: it survives `--revoke`, profile deletion and re-creation, `--unbind`/rebind and
  credential aliases (two profile names with one tunnel resolve to one assignment).
- Every launch path (share, setup, takeover, `repochat mcp` with tunnel credentials) refuses that tunnel on another
  repository.
- The only release is `repochat trust --release-tunnel`. It confirms in the terminal (default no; `--yes` for
  scripts) and states that the plugin must be deleted and created again in ChatGPT before the tunnel serves another
  repository; otherwise use another tunnel.
- Repository binding has four parts:
  1. the canonical owner root;
  2. the canonical common dir;
  3. device+inode of both;
  4. the sorted, complete set of root commits of the owner's HEAD (`rev-list --max-parents=0 HEAD`, hardened runner).
- A shallow repository is refused (`rev-parse --is-shallow-repository`); an unborn one too.
- A changed root set, root/common-dir identity, or path refuses the share until `repochat trust` is run again. It is
  never a silent rebind, and never "omit guidance and keep serving".
- A rebase that keeps the roots stays valid. Linked worktrees have distinct owner-root bindings; a session worktree
  never stands in for the owner.
- The docs say that shared ancestry does not authenticate a project's identity.

**G3′ Storage.**
- Load validates:
  - the raw-digest syntax (the raw digest is used only to detect drift);
  - the display digest, recomputed from the stored display bytes;
  - a SHA-256 checksum over the canonical versioned record, called corruption detection and never authentication.
- Unmasked source is not stored.
- Generations are monotonic and never reused. They live in `<state>/guidance/lifecycle.json`, keyed like the
  assignments, and a revoke or a replace increments them.
- All state changes are serialized by an exclusive flock on the guidance directory.
- An unsupported transformation version refuses the share, with the reason; the text is never re-transformed.

**G5′ Guard.**
- One guard runs before every tool call and before every `initialize` and `server/discover` response that carries
  instructions. It:
  1. reads the complete bounded record and validates it as in G3′;
  2. compares the generation and the snapshot identity (display digest) with what the process started with;
  3. obtains a current exposure decision for the source path. The guard keeps the decision cached together with the
     inputs it depends on: the SHA-256 of the owner checkout's `.repochatignore`, the process's fixed options, and
     the built-in rules. It reuses the cached decision only while the stat and hash of that file match those
     inputs; otherwise it re-evaluates the policy for that one path through the existing evaluator.
- Any mismatch, damage, unreadable state or withheld source latches invalidation for the process. Every later tool
  call and every instruction-bearing response is refused with the restart / Refresh tools / new conversation notice.
- Documented: calls already admitted may finish, and nothing recalls earlier context or effects.

**G6′ Drift line.**
- Four states:
  - **unchanged:** the hash matches;
  - **changed:** the hash differs;
  - **removed:** only on a definite not-exist result;
  - **could not be compared:** permission, I/O or unsafe file type, never reported as unchanged or removed.
- Exposure denial or uncertainty goes to G5′.
- Ordinary removal never revokes. At start, an absent source whose path the policy still permits keeps the reviewed
  snapshot served.

**G7′** `docs/guidance-test.md` carries the full accepted M5 protocol plus:
- deterministic automated cases: tunnel aliases and rebinding, shallow history, revoke and re-trust generation reuse,
  discovery right after revocation, a policy change within the cache window, comparison failures;
- success thresholds fixed before any trial: guidance adherence in at least 8 of 10 matched trials per placement and
  length, and zero provenance-promotion failures across all hostile cases.

Any provenance-promotion failure keeps the feature experimental.

## Your task (round 3)

For G2′, G3′, G5′, G6′ and G7′, and for the cumulative design (G1, G4 and G7 as accepted, plus these): ACCEPT,
MAINTAIN or ESCALATE. Set `consensus` to "yes" only if you sign it. If anything still blocks, name the exact sentence
to change. Respond only with JSON matching the schema.
