# repochat — "one ChatGPT plugin, many repositories" design, for adversarial review

You are an independent senior reviewer (protocol design, local IPC, product UX). Review a DESIGN; do not write code and
do not modify files. The code base is the Go project in the current directory (read README.md, internal/cli/run.go,
internal/mcp/server.go, internal/tools/tools.go for how it works today). Use web search to check external claims
(OpenAI Apps SDK / custom MCP docs, MCP spec 2026-07-28, openai/tunnel-client docs). Verify, don't trust me.

## Today (shipped, tested)
`repochat` in a repository starts OpenAI's tunnel-client with `repochat mcp` as its stdio child. One tunnel = one ChatGPT
plugin = one repository. A second `repochat` on the same tunnel takes the tunnel over. To work on several repositories
in parallel the user must create one tunnel + one ChatGPT plugin per repository. The owner wants: ONE plugin, any
number of repositories shared at the same time, nothing to add in ChatGPT when a new repository is started.

## Verified facts (measured on the owner's real ChatGPT session and in tunnel-client v0.0.21 docs)
- V1 ChatGPT reaches the server as client "openai-mcp 1.0.0" speaking MCP 2026-07-28 (stateless: no initialize, no
  session; every request stands alone).
- V2 tunnel-client: exactly one active instance per tunnel id; one shared stdio child per channel; channels are fixed
  at start. So everything behind one plugin goes through ONE MCP child process.
- V3 ChatGPT caches the tool list of a plugin (the user must press "Refresh tools" to see changes), so the tool list
  must not depend on which repositories are shared.
- V4 All tools are read-only (readOnlyHint), text results end with an "@meta {json}" line naming the repository.
- UNKNOWN: whether ChatGPT sends any per-conversation identifier in `_meta` (e.g. "openai/session") to a custom MCP
  server behind a tunnel. Please research this; I have not measured it yet.

## The naive hub and its three losses (the owner demands that ALL THREE be solved)
Naive: one process opens all repositories and tools get a `repo` name argument.
- L1 Isolation: any conversation using the plugin can read every repository that is shared at that moment.
- L2 Wrong repository: the model can name the wrong repository; with no conversation identity the server cannot tell.
- L3 The "one repository per process" rule (which the security guarantees and tests rely on) is inverted.

## Proposed design
P1 ROUTER WITH NO REPOSITORY ACCESS. tunnel-client's stdio child becomes `repochat mcp --router`: a small process that
   answers server/discover, initialize and tools/list itself (the same 13 tools, generated from the same catalogue,
   each with one extra REQUIRED string argument `repo`) and forwards tools/call to a per-repository process over a
   Unix socket. The router never opens a repository, never runs git, holds no policy. (solves L3: the rule holds;
   repo/tools packages and their tests are unchanged.)
P2 ONE SERVER PROCESS PER REPOSITORY, as today. Each `repochat` terminal runs today's MCP server for ITS repository,
   listening on a private Unix socket inside the per-tunnel 0700 run directory, and shows only its own activity.
   Ctrl+C detaches only that repository. Exposure flags (--deny, --tracked-only, …) stay per repository.
P3 ACCESS CODE = CAPABILITY. Each repository gets an access code such as `api-k7m2qx` (repository name + 6 random
   base32 chars = 30 bits), printed in its terminal: "In ChatGPT: @RepoChat api-k7m2qx <your question>". The `repo`
   argument must be that code. A conversation can reach exactly the repositories whose codes the USER put into it.
   No tool lists repositories or codes. Unknown and offline codes are indistinguishable ("no repository is shared
   under that code; ask the user"). (solves L1; and L2 because the model cannot address a repository it was not given.)
   - The socket path is derived from a hash of the code, so the router has no registry to enumerate; the
     per-repository server ALSO verifies the code (constant time), so the router is not trusted for authorization.
   - Wrong-code attempts are rate limited globally (e.g. 10 per minute, then a cool-down) and shown as a warning in
     every attached terminal.
   - The code is stable per repository on this machine (stored 0600 in the user's state dir) so saved prompts, a
     ChatGPT Project's instructions and old conversations keep working after a restart; `repochat --new-code` rotates.
   - The code is masked in activity lines and never written to the local activity log (the repository name is).
   - Also fixes an existing weakness: today an old conversation silently reads whatever repository now owns the
     tunnel; with codes it gets "not shared right now" instead.
P4 HUB SUPERVISOR. tunnel-client + router are owned by a background "hub" process that the first `repochat` starts
   automatically (detached) and that exits by itself shortly after the last repository detaches. Every terminal is an
   equal attachment. The hub holds the per-tunnel lock (replacing today's takeover). `repochat status` shows hub and
   attachments; `repochat stop --all` ends the hub. With no repository attached the hub can read nothing.
P5 Profiles (separate tunnels/plugins) remain for hard separation between audiences.

## Alternatives I considered
A1 "Pairing/approval": the model calls open_repo(name) → the repository's terminal asks the user to approve →
   the tool result returns a conversation-scoped code. No code to paste, but a terminal keypress per new chat, and
   the name is guessable (approval is the control).
A2 Bind a conversation id (if ChatGPT sends one) to the first code used, and refuse other codes in that conversation.
A3 Tool names prefixed per repository — rejected (V3).
A4 An "active repository" switch — rejected (stateless, two chats would fight).
A5 No code when only one repository is shared ("open mode") — I lean against: it reintroduces L1 and the stale-chat
   weakness, at the price of one token per new chat.

## Questions
Q1 Does P1–P4 really solve L1, L2 and L3? Name any way a conversation can read a repository whose code it was not
   given, or any way the per-repository guarantees are weakened by the router.
Q2 Code design: entropy, stability vs rotation, where it may leak (it is sent to OpenAI inside the conversation by
   design), rate limiting without a caller identity. Better alternative?
Q3 Hub as an auto-started background process vs "the first terminal is the host". Which, and why? Failure modes
   (hub crash, router crash/restart by tunnel-client, stale sockets, upgrade with mixed versions).
Q4 Should A1 (approval) and/or A2 (conversation binding) be part of v1, optional, or left out? Base the A2 answer on
   what you can verify about `_meta` sent by ChatGPT.
Q5 UX: the owner insists first use must stay "ultra easy" (today: `repochat`, one hidden paste, done). What is the
   smallest-friction way to get the code into a chat, and should A5 exist?
Q6 Anything else that would make this fail in practice with ChatGPT (argument handling, confirmation prompts, tool
   cache, request concurrency through one child, response size, timeouts).

Return a verdict for each of P1..P5 and A1..A5 (agree/modify/disagree + severity + rationale + proposed change),
answers to Q1..Q6, missed items, and whether you consider the three losses solved.

# PART B — product direction ("repochat 2.0" proposal) to evaluate

The owner received a proposal (written by another assistant) arguing that repochat 1.0 "solves connectivity, not code
intelligence" and should become a local code-intelligence / context engine. The owner asks us to evaluate it and reach
a consensus on what to build and in what order. Its items, each with MY position — challenge both:

Hard constraints that stay (owner decisions): single static Go binary, `cd repo && repochat`; read-only; no program
configured by the repository is ever executed; no LLM or vector DB required; standard library only (K1) unless you can
show a specific exception is worth asking the owner for; every new tool obeys the existing exposure policy (withheld
paths, secret masking, "matching happens on the disclosed text").

R1 `get_context(query, max_tokens)`: returns a budgeted "context pack" (relevant definitions, references, tests, paths
   and line numbers) instead of the model issuing many search/read calls.
   My position: worth building, but as an honest heuristic retriever (lexical + symbol ranking, no LLM); it must ship
   with a reproducible retrieval benchmark, otherwise we cannot claim any saving.
R2 `repo_map`: compact ranked map of the repository (top symbols per file, budgeted). My position: yes; cheap; reuses
   the existing outline code.
R3 `context_stats` + a per-call size budget (`max_tokens`), lower default than today's 200 KiB.
   My position: yes; the activity log already records tool, bytes and duration per call.
R4 Persistent incremental symbol index. Proposal: exact for Go/TS/JS/PHP, optional language servers for the rest.
   My position: Go exact (go/parser); other languages via in-process line heuristics (ctags-like), labelled as
   heuristic; cache keyed by git blob id, stored outside the repository in the user's cache dir, containing names and
   positions only (no source text), never indexing withheld paths. NO language servers: they execute project-defined
   plugins/toolchains, which breaks the "no repository-configured program runs" guarantee. No tree-sitter (cgo).
R5 `find_symbol`, `find_references`, `symbol_context`, `impact_analysis`.
   My position: find_symbol yes; references = identifier occurrences with word boundaries, grouped as
   definition / call-like / other, each result labelled "lexical, not proven"; impact_analysis only as a composition
   of those with explicit confidence — never presented as a call graph.
R6 `analyze_changes`: from a diff, list changed symbols, where they are referenced, and likely related tests.
   My position: yes, high value, composes diff + outline + references; tests found by naming conventions only.
R7 Multi-repository through one plugin, "only within one trust zone". My position: PART A solves isolation with
   per-repository access codes, which is stronger than a trust-zone rule; cross-repository comparison = the user gives
   two codes to one conversation.
R8 "Agent Context Bridge" (read CLAUDE.md/AGENTS.md, plans, exported session summaries of other coding agents).
   My position: no new feature. Files inside the repository are already readable; agent transcripts live outside the
   repository (e.g. under ~/.claude) and reading them would break the repository boundary.
R9 `architecture_map`: components, entry points, dependencies, external integrations.
   My position: later; feasible as an import/manifest graph (Go exact, others by import-statement patterns), labelled.
R10 "Token intelligence": measure context efficiency. My position: we can measure bytes, calls, duplicate bytes and
   retrieval recall on a gold set; we cannot measure billed tokens or answer quality from inside the server.
R11 Reposition as "Local code intelligence for AI assistants / less code sent, more code understood".
   My position: not until the benchmark shows it.
R12 ORDER. The proposal: context engine + symbol index + measurement first; multi-repository last ("2.0").
   My position: (1) one-plugin multi-repository with access codes (PART A) — it is the friction the owner actually hit
   today, twice; (2) repo_map + get_context + context_stats with the benchmark; (3) symbol index, find_symbol,
   find_references, analyze_changes; (4) architecture_map. Agent bridge: dropped.

Questions for PART B:
QB1 Which of R1..R12 are right, wrong or overstated? Be concrete about what a model-driven client (ChatGPT) gains
    from a context pack versus doing its own search/read loop through a tunnel.
QB2 Is an on-disk index compatible with the security model (at-rest sensitivity, stale entries after policy changes,
    poisoning by a hostile repository, masking)? What must the design guarantee?
QB3 Design the smallest reproducible benchmark that could falsify R1's value (no LLM in the loop).
QB4 Your recommended order and the cut line for the next release.

In your JSON: put P1..P5, A1..A5 and R1..R12 in `verdicts`; QA answers Q1..Q6 and QB1..QB4 in `answers`.
