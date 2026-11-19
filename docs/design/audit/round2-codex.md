- **H1 — CONSENSUS.** The explicit names and `LC_*` exception are acceptable for the `mcp` environment. I see no required addition. Executable/exec failure must terminate before serving, as proposed.

- **H2 — CONSENSUS.** “Stops when its stderr is broken” accurately limits the claim.

- **H3 — CONSENSUS.** The separate coordination lock resolves the probe/acquisition race, provided **every** launcher ownership acquisition attempt takes it, including retries. Release it immediately after `tryLock`; keep the ownership lock separate. The proposed status wording and “unknown” error handling are appropriate.

- **H4 — CONSENSUS.** Accept rejecting batched `initialize` with `-32600` and no initialization state change, while retaining standalone renegotiation for compatibility. Repeated initialization does not change repository authorization, so I withdraw the demand to reject it. Describe this as deliberate compatibility behavior; existing tests establish behavior, rather than protocol compliance.

- **H5 — Not addressed in your message.** I cannot mark an omitted point CONSENSUS or reconstruct its disposition from the numbering.

- **H6 — CONSENSUS.** The revised comment states what the check establishes. Recommending branch/tag protection separately is appropriate.

- **H7 — CONSENSUS.** The broad-root guard and frozen exclusions address the concern, including plain `mcp`, aliases, component boundaries, case folding, precedence over `--allow`, and historical access.

- **H8 — CONSENSUS.** Accept 32 KiB now and defer per-call `max_bytes` to the planned release. Lowering the default without that argument is preferable to retaining 200 KiB: it reduces default context consumption, while existing narrowing and continuation controls remain available. Update all default descriptions consistently and retain the explicit “provisional and unmeasured” qualification. This mitigates the budget concern; measurement remains necessary for the release verdict.

- **H9 — CONSENSUS.** “Scope complete; candidate under validation” and `v1.0.0-rc.1` fit the remaining evidence gap. Stable release remains conditional on the real ChatGPT run and result-budget evaluation against the fixed build.

These are agreements on the proposed changes, not confirmation that they have been implemented. No files modified; no tests run.
