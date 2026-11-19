# repochat

[Română](README.ro.md) · Project: <https://github.com/nalyk/repochat>

Talk to ChatGPT about the Git repository on your computer, without uploading it anywhere and without opening a port. repochat is read-only: nothing can be changed through it, unless you start it with `--write` ([write mode](#write-mode-experimental), experimental and off by default).

```bash
cd ~/projects/my-project
repochat
```

Then, in ChatGPT: `@RepoChat what does AuthenticationService do and where is it used?`

It is one Go binary with no third-party dependencies. It speaks MCP and reaches ChatGPT through OpenAI's [Secure MCP Tunnel](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels), using OpenAI's own `tunnel-client`, which repochat installs for you on first run.

> Read this before anything else. Whatever ChatGPT reads from your repository is sent to OpenAI. "Local" here means you upload nothing in advance and the connection starts from your machine. It does not mean the data stays on it. Only use repochat with code you are allowed to show.

## Why this exists

I wrote it because of how OpenAI meters things.

Codex in the terminal, in the desktop app and in the IDE all draw on one allowance. OpenAI's [pricing page](https://learn.chatgpt.com/docs/pricing) puts it plainly: "ChatGPT Work and Codex share pricing, credits, and usage limits." On Plus that allowance is a five-hour window with a weekly limit on top, and the estimate for the default model is somewhere between 15 and 160 local messages per window, depending on how heavy each one is. Pro plans have no five-hour window, but the weekly limit stays.

That is fine when the agent is editing code. It is a poor deal for the other half of the job, which is talking: "why is this module shaped like this", "what would break if I changed that", "read these four files and tell me what you would do". An agent answers those by pulling half the repository into its context, and every such question comes out of the same budget I need later for the actual work. More than once I ran out mid-week having mostly asked questions.

Ordinary ChatGPT, the chat on the web, is a separate surface. The same pricing page lists the agentic models as available "in Work and Codex, not Chat", and Chat is not part of the shared allowance it describes. Chat is also where I do my reading: web search, long back-and-forth, half-formed ideas. What Chat cannot do is see my code, unless I paste it in or push it somewhere it can reach.

repochat closes that gap. The repository stays where it is, ChatGPT asks for the files it needs, and the conversation happens in Chat instead of in an agent session. I plan and research in the browser, with the real code in front of the model, and keep the Codex allowance for the sessions that change things.

Three caveats, because I would rather be straight about this:

- I have not measured how tool calls from a plugin count against Chat's own limits, and OpenAI does not spell it out. Third-party guides say the accounting between ChatGPT and Codex is changing again soon. Check your [usage dashboard](https://chatgpt.com/codex/settings/usage) rather than trusting this paragraph.
- OpenAI documents custom MCP plugins for ChatGPT on the web. I have not tried the mobile or desktop ChatGPT apps and make no claim about them.
- Secure MCP Tunnel was announced for enterprise customers. If your plan does not offer it for personal plugins, repochat cannot change that.

If you already live in Codex or Claude Code and never hit a limit, you probably do not need this.

## Quick start

### 1. Install

From source (Go 1.25+ and git 2.25+):

```bash
git clone https://github.com/nalyk/repochat.git
cd repochat
make install          # builds and copies the binary to ~/.local/bin
```

Or with Go directly (the repository is private, so Go needs access to it):

```bash
GOPRIVATE=github.com/nalyk/repochat go install github.com/nalyk/repochat@latest
```

There is no published release yet. A `v*` tag on `main` builds one.

### 2. First run

Go into a repository and run `repochat`. It walks you through what is missing, in this order:

1. It checks git and the repository first. Nothing is downloaded or asked for until it knows there is something to serve.
2. It installs `tunnel-client` if it cannot find it. It shows where the download comes from and what it verifies, then asks. The version is pinned and the archive is checked against SHA-256 sums compiled into repochat.
3. It asks once for two values from OpenAI Platform, in a single hidden field (paste them in any order):
   - the ID of a tunnel created at <https://platform.openai.com/settings/organization/tunnels> and attached to your ChatGPT workspace;
   - a Restricted API key with Tunnels Read + Use, from <https://platform.openai.com/settings/organization/api-keys>.

   It verifies them with OpenAI and saves them in a file only you can read.
4. It starts the tunnel and shows the one step that has to happen in ChatGPT: <https://chatgpt.com/plugins> → `+` → Add custom MCP server → Connection: Tunnel → pick the tunnel → No authentication → Create as a plugin (personal) → Install. You choose the plugin's name; the examples here use "RepoChat".
5. It confirms in the terminal when the first call from ChatGPT has been served.

No local program can do the ChatGPT step for you. The tunnel and the plugin live in your account.

### 3. Day to day

```bash
cd other-project && repochat      # Ctrl+C stops sharing
```

You can start it from any subdirectory. If the tunnel is already in use by another repochat, it asks whether to stop that one and take over.

Without a terminal (scripts, CI) it asks nothing: set `CONTROL_PLANE_TUNNEL_ID` and `CONTROL_PLANE_API_KEY`, and add `--yes` where you want confirmations answered.

## Several repositories at once

One tunnel serves one repository at a time. To work in several at once, give each its own tunnel:

```bash
cd ~/projects/api && repochat                    # default profile, first tunnel
cd ~/projects/web && repochat --profile web      # asks once for the "web" tunnel and key
```

- Each profile has its own tunnel and its own plugin in ChatGPT. Tunnel permissions are per organization, so one API key should work for all of them; that has not been checked with a real account.
- `repochat setup --profile web --bind`, run inside the repository, ties the profile to it. After that plain `repochat` is enough there, and the profile refuses any other repository.
- `repochat status` lists every share on this machine. `repochat stop --profile web` stops one.

How a single conversation behaves with two plugins that expose the same tool names is ChatGPT's business and has not been tested. One conversation per plugin is the safe way.

A design for one plugin serving several repositories exists in [`docs/superpowers/`](docs/superpowers/). None of it is built.

## Write mode (experimental)

Write mode is off by default. Without `--write`, repochat serves the 13 read-only tools described below and nothing in this section applies. Write mode passes its automated tests, but it has not yet been used from a real ChatGPT session.

```bash
repochat --write                    # share, and let ChatGPT edit files in a session
repochat --write --new-session      # start a new session instead of resuming the open one
repochat --write --confirm-merge    # finish_session does not merge; you do, with repochat session finish
repochat session                    # from any terminal: the open session, its checkpoints, the changed files
repochat session finish             # merge it yourself, after a question [--squash] [--message TEXT] [--yes]
repochat session abandon            # remove its workspace; the branch is kept [--delete-branch]
```

`--write` cannot be combined with `--no-history`. `repochat mcp --write` does the same for other MCP clients, and `repochat tools --write` and `repochat call --write` show and run the write tools from the terminal.

### What a session is

A session is a second copy of the repository, made with `git worktree` from the commit your checkout is on, on a new branch named `repochat/<date>-<id>`. It lives under repochat's state directory (`~/.local/state/repochat/sessions/`), not inside your repository. Your uncommitted changes are not in it; the banner says so when you have some.

While a session is open, every tool works on that copy, the 13 read tools included. ChatGPT sees its own edits, and does not see your checkout. Files the exposure policy withholds are not written into the copy at all.

Each change ChatGPT applies becomes a commit on the session branch, called a checkpoint. Nothing reaches your branch until `finish_session` merges the session, as a fast-forward or as one squashed commit. You can look at the session branch with ordinary git at any time, and merge it by hand instead. There is one open session per repository: starting `repochat --write` again resumes it.

In a session, every line the model reads carries its number and a short hash, like `12:abc|`. An edit names those anchors instead of quoting the old text. If the file changed after the model read it, the anchor no longer matches and the edit is refused.

### The nine tools

| Tool | Role |
|---|---|
| `start_session` | open a session, or resume the open one; `fresh` starts another and keeps the old branch |
| `session_status` | branch, start, checkpoints, changed files, whether your branch has moved, changed files that run on your machine later (read-only) |
| `propose_changes` | check a change set and show the diff it would make, writing nothing (read-only) |
| `apply_changes` | apply a change set over any number of files, completely or not at all, as one checkpoint |
| `list_checkpoints` | the checkpoints, newest first (read-only) |
| `undo_to_checkpoint` | restore the files of an earlier checkpoint as a new checkpoint; history keeps everything |
| `update_session` | merge the commits your branch gained into the session; a conflict changes nothing |
| `finish_session` | merge the session into your branch, in your checkout, then close it (marked destructive) |
| `abandon_session` | close the session without merging; the branch is kept unless asked otherwise (marked destructive) |

A change set is a list of operations: replace, insert or delete lines by anchor; create, remove or move a whole file. Only UTF-8 text files can be edited. Go and JSON files must still parse after the change; other languages are not checked.

### What it guarantees

Each point is covered by automated tests. One of them drives every write tool with hostile paths and values (`..`, absolute paths, `.git` in any letter case, protected and withheld paths, symbolic links committed in the repository or planted in the session copy) against canary files in every place a write must not reach, and checks after each call that every canary is unchanged.

- Writes happen only inside the session copy and the git bookkeeping of its branch, plus the one merge into your branch when `finish_session` is called.
- No write reaches a protected path, a withheld file, or anything outside the session copy, including through symbolic links replaced while the call runs. Protected paths are `.git` and everything in it, continuous-integration definitions (`.github/workflows`, `.gitlab-ci.yml` and the equivalents of other services), hook directories (`.husky`, `.githooks`), `.gitmodules`, `.gitattributes` and `.repochatignore`. Withheld files are those of [What ChatGPT can see](#what-chatgpt-can-see); the `.repochatignore` in your checkout counts as it is on disk, committed or not.
- A change set is applied completely or not at all.
- A change made against outdated content is refused.
- A line that holds a masked value cannot be changed or deleted. The model is told that you have to edit it by hand. With `--no-redact` nothing is masked and nothing is locked.
- No program configured by the repository runs at any step, the final merge included: no hook, filter, merge driver or signing program. Session commits are therefore unsigned. If your git configuration asks for signed commits, `finish_session` refuses and says how to merge by hand.
- `finish_session` refuses, changing nothing, when your checkout is on another branch, when your branch has moved (then `update_session` comes first), and when the merge would overwrite your uncommitted changes, ignored files included.
- Without `--write`, the tools, their descriptions and their results are exactly those of read mode.

### What it does not guarantee

- That an edit is correct or harmless. ChatGPT can remember your approval for the whole conversation. After that, text in the repository (a comment, a README, a pasted issue) can steer the model into changes nobody reviewed. Until something is merged the damage stays on the session branch, so read the diff before you merge: `repochat session` lists the files, and `git diff <your branch>...<session branch>` shows them.
- That merged code is safe to run. Build files, package scripts, shell scripts and container files run later on your machine. `finish_session` and `repochat session` list the ones the session changed; that is all repochat can do about them.
- That a session belongs to one conversation. Every chat that can use the plugin works on the same open session.
- That a merge cut off halfway leaves your checkout clean. The merge is not stopped when the call is cancelled or times out, but if git itself fails in the middle, the result says the checkout may be partly updated and prints the commands that put it back, or complete the merge. Run them as printed, in that order.
- That hooks your repository relies on still happen. `post-merge` and the others do not run; the result names the ones the repository has, so you can run them by hand.
- Binary files, file modes, symbolic links and submodules cannot be edited.

Building needs Go 1.25 or newer, as for the rest of repochat.

## What ChatGPT can see

| Sees | Does not see |
|---|---|
| files git knows about: tracked and untracked | files ignored by `.gitignore` |
| history reachable from `HEAD`, branches, remote branches and tags | stash, reflog, orphaned commits |
| how many files are withheld in the whole repository, by category | the names and content of withheld files |
| | the `.git` directory, files outside the repository, symlink targets |
| | the content of submodules (a changed submodule shows only as a commit change) |
| | repochat's own files (the configuration with the API key, state, `tunnel-client`) |

Files that look like secrets are withheld by default: `.env`, `*.pem`, `*.key`, `id_rsa`, `terraform.tfstate`, `*.tfvars`, `credentials.json`, `secrets.yaml`, `.npmrc`, `service-account.json` and similar. The keyword rule (`token`, `secret`, `password`…) applies only to data and configuration files, so `token.go` stays visible. The model is told that withheld files exist and how many there are in total. That number never depends on what it searches for, because a count per query would let it guess names letter by letter.

Values that look like secrets (AWS keys, GitHub, GitLab and Slack tokens, OpenAI, Anthropic, Google and Stripe keys, JWTs, passwords in URLs, PEM private keys) appear as `[REDACTED:kind]`. Search runs on the masked text. Masking is best effort: a secret in a format repochat does not know is not caught.

See exactly what would be served, without starting anything:

```bash
repochat files                 # served files
repochat files --withheld      # withheld files and why
repochat call read_file '{"path":"README.md"}'    # the exact text the model would get
```

## Controlling exposure

| Option | Effect |
|---|---|
| `--tracked-only` | hide untracked files |
| `--no-history` | working tree only: history tools and `git_diff` are refused, and `git_status` and `repo_overview` show no commit ids |
| `--deny PATTERN` | withhold more (`.gitignore` syntax, repeatable) |
| `--allow PATTERN` | serve a file the built-in secret rules would withhold |
| `--show-withheld-names` | the model sees the names of withheld files, never their content |
| `--no-redact` | turn value masking off |
| `--max-output-kb N` | size of one result, `@meta` line included (16–2048, default 32) |
| `--profile NAME` | another saved tunnel |

A `.repochatignore` file in the repository withholds files for everyone who serves it, with the same syntax as `.gitignore`. Your own git decides what a rule means: repochat asks `git check-ignore` in an empty scratch repository, isolated from your configuration, the system's and the served repository's. You can test a rule with the same command. Names are compared exactly, including case. If git cannot be asked, nothing is served. `.repochatignore` can only tighten; `--allow` does not override it.

Order of the rules: repochat's own files → unsupported name → `.repochatignore` → `--deny` → `--allow` → built-in secret rules.

Quote your patterns and give each its own `--deny` or `--allow`. An unquoted pattern can be expanded by the shell into several words, and commands that take only options stop with an error on a stray word instead of silently dropping the options after it.

## Commands

| Command | What it does |
|---|---|
| `repochat` | share the current repository |
| `repochat status` / `stop` | is anything shared right now, from any terminal? / stop it from another one |
| `repochat doctor` | check each part separately and say what it could not test |
| `repochat setup` | enter or change the tunnel and key; `--list`, `--remove`, `--bind` |
| `repochat files`, `tools`, `call` | what is served, which tools the model gets, run a tool yourself |
| `repochat log` | recent activity (which tool, when, how long) |
| `repochat session` | the open write-mode session; `finish`, `abandon` |
| `repochat mcp` | the MCP server itself (stdio) |
| `repochat mcp-config` | configuration for other MCP clients |
| `repochat install`, `install-helper` | install the binary, or `tunnel-client` |
| `repochat help chatgpt` | the ChatGPT steps |

## Tools the model gets

All 13 are marked `readOnlyHint`, so ChatGPT does not ask for confirmation on every call. Write mode adds nine more, listed under [write mode](#write-mode-experimental).

| Tool | Role |
|---|---|
| `repo_overview` | branch, HEAD, uncommitted changes, layout, file types, what the session can see |
| `list_files`, `find_files` | browse the tree; find by name or pattern |
| `read_file`, `read_files` | read with line numbers and a continuation hint; also at an old revision |
| `search_code` | literal or RE2 search with context, filters and a cursor; says when it is incomplete |
| `file_outline` | definitions in a file (Go: real parser; other languages: heuristic, labelled as such) |
| `git_status`, `git_log`, `git_diff`, `git_show`, `git_blame`, `git_refs` | state and history |

Every result ends with an `@meta {json}` line: whether it is complete, the cursor or next line, how many values were masked.

## Security

What repochat guarantees, each point covered by automated tests:

- Without `--write`, no tool writes. No tool runs arbitrary commands, and the model never supplies raw git arguments. What write mode adds is under [write mode](#write-mode-experimental).
- A file is served only if git lists it and the policy allows it. Symlinks are never followed, even if a directory is swapped for one during access.
- A diff or commit that touches only withheld files produces no content. Git never enters a submodule.
- A revision is a branch, tag or commit name followed at most by parent steps. Forms that search commit messages are refused, and a missing revision and an unreachable one get the same answer.
- Repository configuration cannot make a read run a program (filters, `textconv`, diff drivers, `fsmonitor`, `diff.external`, lazy fetches). Each case is tested with a positive control that shows plain git does run it.
- Git's error text never reaches the model.
- The API key is in no process's arguments and never reaches git. `tunnel-client` hands its environment, key included, to the server it starts, so the server restarts itself with an allowlisted environment before it serves anything.
- repochat never serves its own files, and it refuses a home directory or `/` as the root without `--allow-broad-root`.
- One repochat per tunnel on this machine. Takeover is cooperative over a private socket; no process is signalled by PID. Ctrl+C stops sharing even when output goes through a pipe.
- Repository text and the caller's text have their control characters neutralised before they reach the model or your terminal.

What it does not guarantee:

- It is not a sandbox. Hardening git means switching off known dangerous settings one by one, and a new git release can add one I have not seen. Your user and system git configuration are trusted.
- Masking is not a guarantee. Check with `repochat files` and `repochat call`.
- Anyone who can use the plugin can read the repository while repochat runs. Create it as personal. The tunnel does not know which conversation is using it: switch repositories and old conversations see the new one.
- Old commits are served by default, and a secret removed from the working tree may still be in them. Use `--no-history` if that worries you.
- Instructions hidden in code (prompt injection) can influence the model. repochat labels repository text as data; what the model does with it is up to the model.
- The 32 KB default result size is provisional and has not been measured in real ChatGPT sessions.
- `tunnel-client` opens a diagnostics port on `127.0.0.1`. repochat itself listens on nothing.

The local log (`repochat log`) keeps metadata only. Arguments are recorded only with `--audit-args`. It is a history for you and can be edited by anyone with your account.

## Troubleshooting

Start with `repochat doctor`.

| Symptom | Likely cause |
|---|---|
| the tunnel does not show up in ChatGPT | it is not attached to the ChatGPT workspace, or an admin restricted custom MCP servers |
| "OpenAI rejected the tunnel connection (HTTP 401/403/404)" | wrong key, missing Tunnels Read + Use, or wrong tunnel ID; run `repochat setup` |
| tools missing after an update | plugin settings → Refresh tools |
| "does not answer" on takeover or in `repochat status` | the launcher was killed and a leftover `tunnel-client` still holds the tunnel; end that process |
| the model cannot find a file | it is ignored by `.gitignore` or withheld; see `repochat files --withheld` |

`repochat --verbose` also shows `tunnel-client`'s own log.

## Other MCP clients

`repochat mcp` is an ordinary stdio MCP server and needs no tunnel or credentials. `repochat mcp-config` prints configuration for Claude Desktop, Claude Code, Cursor or VS Code. It speaks the handshake revisions (2024-11-05 to 2025-11-25) and the stateless revision 2026-07-28.

## Development and where it stands

```bash
make check            # vet, race-detector tests, first run through a pseudo-terminal
make fuzz             # short fuzzing of the input validators
make dist             # archives for linux/darwin × amd64/arm64, with checksums
make test-contract    # contract suite with the REAL tunnel-client and a mock control plane
```

CI runs the full suite on Linux and macOS, and the repository and tool tests again with git 2.25, 2.34 and 2.43.

The 1.0 feature set is complete. As for maturity, this is a candidate under validation, and I would not put a repository I cannot afford to leak behind it yet. An audit on 19 November 2026 found about 50 real defects behind a green CI, including one that leaked withheld files from submodules. They are fixed, each with a test that failed first, and the list is in [`CHANGELOG.md`](CHANGELOG.md). The first version of one fix was itself wrong on newer git, which CI caught. The design reviews and the audit exchange with Codex CLI are in [`docs/design/`](docs/design/).

Still not done:

1. A real ChatGPT session on the fixed build. It needs a tunnel and key from your account; the mock control plane follows the documented protocol, and OpenAI's servers may have rules it lacks.
2. A measurement of result sizes in real conversations.
3. Interactive use on macOS. CI passes there, but nobody has sat in front of it.
4. Write mode from a real ChatGPT session. Whether a ChatGPT Plus account may call a tool that is not read-only at all is still being checked.

## License

MIT. `tunnel-client` is a separate program by OpenAI under Apache-2.0.
