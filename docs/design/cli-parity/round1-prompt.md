# repochat — what it lacks to feel like Claude Code CLI / Codex CLI, and in what order to build it

You are an independent senior reviewer (coding agents, sandboxing, MCP, ChatGPT custom MCP / Apps SDK). This is a
BRAINSTORM that must end in CONSENSUS with me (Claude, the author), over several rounds. Do not write code and do not
modify files. Read the code base in the current directory: README.md, docs/design/README.md,
docs/superpowers/specs/*.md (write mode is built; the hub spec is proposed, not built), internal/tools/tools.go,
internal/tools/write.go, internal/repo/session.go, internal/mcp/server.go. Use web search to check every external claim
(OpenAI docs for ChatGPT developer mode / custom MCP / Apps SDK / Secure MCP Tunnel, MCP spec 2025-11-25 and 2026-07-28,
Codex CLI docs, Claude Code docs, Linux Landlock and seccomp, macOS Seatbelt). Verify, don't trust me. Label what you
cannot verify as unverified.

## What repochat is today (on main, tested)

- One Go binary, standard library only. `repochat` in a repository starts OpenAI's `tunnel-client` (Secure MCP Tunnel)
  with `repochat mcp` as its stdio child; ChatGPT on the web uses it as a custom MCP plugin. The owner's motive: hold
  coding conversations in ChatGPT Chat instead of spending the allowance that Codex CLI/desktop/IDE share. Account:
  ChatGPT Plus.
- 13 read-only tools: repo_overview, list_files, find_files, read_file, read_files, search_code, file_outline,
  git_status, git_log, git_diff, git_show, git_blame, git_refs. Exposure policy: only files git lists; secret-like files
  withheld (counted, never named); secret-like values masked; `.repochatignore`; hardened git (no program configured by
  the repository ever runs).
- `--write` (experimental; built and tested, never used from a real ChatGPT session): 9 more tools. Edits happen in a
  session = a git worktree on branch `repochat/<date>-<id>` under `~/.local/state/repochat/sessions`, created from HEAD
  (the owner's uncommitted work is NOT in it); withheld files are not materialised (sparse checkout); line-anchored
  edits (`12:abc|`); atomic multi-file change sets, each one a checkpoint commit; undo; update from the owner's branch;
  finish = fast-forward or squash merge into the owner's checkout. Only Go and JSON are syntax-checked.
- Limits: server per-call timeout 60 s (`DefaultCallTimeout` in internal/mcp/server.go); tunnel 120 s per request,
  10 MiB per message, 600 requests/min; result budget 32 KB by default.
- ChatGPT asks for confirmation per non-read-only TOOL and can remember the answer for the whole conversation, per tool,
  not per argument.
- The owner's decided roadmap: write mode is four projects: (1) edits in a session — built; (2) running commands in a
  sandbox built into repochat itself on kernel features (the owner chose this over delegating to bubblewrap/Seatbelt and
  over handing work to Codex); (3) language intelligence for any language; (4) one plugin for several repositories (the
  hub spec, proposed).
- Unknowns: a probe on branch `spike/chatgpt-probe` (not yet run by the owner) answers whether Plus may call
  non-read-only tools, whether `_meta["openai/session"]` arrives, and which client capabilities ChatGPT declares.
- Contradiction in the docs: the hub spec REJECTS language servers ("they run tools the project defines") while the
  write-mode spec says project 3 extends checking "through language servers".

## Goal of this brainstorm

The owner's question: "what does this app lack to be a near Claude Code CLI or Codex CLI experience?" — that is,
ChatGPT web + repochat should let the model do on a local repository what those CLIs do: explore, plan, edit, run,
verify, iterate, and hand back reviewable work. The consensus must give (a) the gap list, (b) the build order, (c) what
NOT to build for parity, (d) for the top items, the shape of the design.

## CLAUDE'S OPENING POSITION (v1)

Framing: in the CLIs the model and its tools are one product. Here ChatGPT owns the model and the loop; repochat owns
the hands. The hands read well and edit (experimentally). They cannot run anything, so the model cannot check its own
work, and checking its own work is what makes the CLIs feel like agents.

**G0 Gate: real-session validation first.** Extend the probe to also measure: the per-call timeout ChatGPT enforces;
how many tool calls one assistant turn makes before ChatGPT stops; whether image content in a tool result reaches the
model; whether MCP Tasks (2025-11-25), progress notifications and elicitation are supported; how large results can be
in practice. Nothing below is worth building if Plus cannot call write tools.

Tier 1 (defines the CLI experience):

**G1 Command execution (project 2).** What this surface forces:
- Jobs, not calls: start → poll → tail/cancel, because of the 60 s / 120 s caps. One job at a time per session (every
  chat shares the session).
- ChatGPT's approval is per tool and remembered, so the sandbox must be sufficient on its own. Two tools: `run_task`
  (owner-declared tasks such as test/build/lint, from owner-local configuration, not from the repository) and
  `run_command` (free-form, marked destructive/open-world so it gets its own approval). Network off by default.
- The sandbox is first an EXPOSURE boundary, unlike Codex's (whose read-only mode still reads the whole disk).
  Commands must not read withheld files, the owner's checkout, the shared `.git` object store (a worktree shares
  objects: `git show HEAD:.env` would leak a withheld file), repochat's configuration (API key), or `$HOME`. Read
  allowlist = the session workspace + toolchains + caches. Output goes through the masker.
- The write-mode anti-goal "no program configured by the repository runs" must be restated: running tests IS running
  repository code. New guarantee: repository code runs only inside the sandbox, only through these tools.
- Dependencies: the worktree lacks ignored artifacts (node_modules, .venv, target/, generated code) and withheld test
  fixtures (testdata/*.pem). Go first: read-only module cache, GOPROXY=off, GOFLAGS=-mod=readonly, a private GOCACHE.
  Other ecosystems need an owner-approved install step with network for the package manager only.
- Output reduction: failures + tail inside 32 KB.

**G2 Compound calls.** A ChatGPT round trip costs more than a CLI one (latency, confirmations, the turn may stop).
`apply_changes` gets an optional `then_run` task; the result is the diff summary + failures only.

**G3 Trusted repository instructions.** The CLIs load AGENTS.md / CLAUDE.md as instructions. repochat lists AGENTS.md
as a key file but tells the model never to follow repository text (internal/tools/tools.go). Proposal:
`repochat trust AGENTS.md` pins a hash; while the file is unchanged it is served as owner-endorsed guidance (at the top of
repo_overview); when it changes it falls back to data and the terminal says so.

**G4 The owner's work in progress.** On request, the session base is a snapshot of the working tree (like
`git stash create`), so "finish what I started" works; finish must cope with the owner's tree still holding that WIP.

Tier 2:

**G5 Language intelligence** = the project's own compiler / type checker / language server inside the G1 sandbox. This
resolves the contradiction: the objection to language servers was "they run project-defined tools", which the sandbox
answers. Replaces the Go/JSON-only syntax checks with real diagnostics.

**G6 Dependency source reading** (opt-in, read-only, masked): the Go module cache for go.sum modules, node_modules for
package.json dependencies, site-packages.

**G7 A plan that survives the conversation:** an `update_plan`-like tool stored with the session and shown by
session_status, so a new chat continues the previous one's intent.

**G8 Review in the terminal:** a live coloured diff per checkpoint, `repochat session diff`, `repochat session undo`.

Tier 3 (later, not needed for parity): the multi-repository hub (the CLIs work on one directory too), push/PR
(outward-facing), screenshots/browser for front-end work, post-edit formatters (after G1), the repo_map/get_context
benchmark from the hub spec (the CLIs have no such thing; grep + read is the parity bar).

Already covered by ChatGPT, do not build: web search, conversation history/resume, Projects instructions and memory,
the stop button. Subagents are not reachable on this surface.

Proposed order: G0 → G3 → G1 (jobs, run_task first, Linux first) → G2 → G4 → G8 → G7 → G5 → G6 → tier 3.

Confidence: medium. Weaknesses I see: I have not verified ChatGPT's per-call timeout, its tool-calls-per-turn
behaviour, MCP Tasks support or image support; Landlock + seccomp in standard-library-only Go is substantial work; the
macOS sandbox story (Seatbelt through the deprecated `sandbox-exec`, which Codex still uses) is uncertain; I may
underweight Apps SDK UI (a diff/test widget inside ChatGPT could beat a terminal pane).

Assumptions: the owner's decisions above stand (side copy, sandbox built in, Plus, remembered approvals are acceptable);
parity means the loop, not an identical UI.

## Your task (round 1)

1. Research and verify, with sources: ChatGPT custom MCP / developer mode behaviour on Plus (write tools, confirmation,
   timeouts, tool calls per turn, image results, MCP Tasks / progress / elicitation, Apps SDK widgets through a tunnel);
   and what Claude Code and Codex CLI actually provide that defines their experience (tools, sandbox modes, approval
   policies, AGENTS.md, plan tools, background processes, checkpoints/undo). One entry per claim in `facts`.
2. A verdict on each of G0–G8 and on the tier-3 list (id "T3"); critique hard; counter-propose.
3. What I missed (M1, M2, …), with severity.
4. Your build order with reasons, and what to cut.
5. If you agree too easily you are not helping. Push on: is "commands in ChatGPT" really the top priority, or should
   the edit loop be made excellent first? Is an exposure-boundary sandbox realistic in standard-library Go on Linux and
   macOS? Is the job model right given how ChatGPT turns behave?

Respond only with JSON matching the provided schema.
