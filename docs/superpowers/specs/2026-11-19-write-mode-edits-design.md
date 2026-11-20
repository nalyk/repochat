# Write mode, project 1: edits in a session workspace

Project: [`nalyk/repochat`](https://github.com/nalyk/repochat) · 2026-11-19 · status: **approved by the owner on 2026-11-19 and built; not yet used from a real ChatGPT session**

Anything marked "my call" was decided by the author of this document, not by the owner.

## Intent

The owner wants to discuss a repository in ordinary ChatGPT and have the model change the code, without spending the Codex allowance that the terminal, desktop and IDE agents share. repochat serves a repository read-only today. This project lets the model propose and make edits, keeps every change undoable, and merges the result into the owner's branch at the end.

This reverses the first rule of the 1.0 design brief, "read-only forever". The owner decided that. Read-only stays the default; writing exists only when asked for.

Write mode is four projects, built one at a time. This document covers the first:

1. **Edits in a session workspace** (this document).
2. Running commands in a sandbox.
3. Language intelligence for any language.
4. One plugin for several repositories.

## What the owner decided

- repochat may change files.
- Changes land in a side copy on its own branch, never directly in the files being worked on.
- The tool makes commits on that branch and merges at the end; merging by hand stays possible.
- ChatGPT may remember an approval for the whole conversation; no approval step in the terminal.
- Protected paths: the default list, nothing added.
- Any language must work.
- Sandbox and everything else are built into repochat itself, not delegated to outside tools (matters for project 2).
- The account is ChatGPT Plus.

## Goals

- The model can create, change, move and delete text files in the session workspace.
- A whole change, across many files, is one call and is applied entirely or not at all.
- A change made against an outdated view of a file is refused, not applied wrongly.
- Every applied change is a checkpoint that can be returned to.
- The model sees its own edits through the existing read tools.
- Nothing is written outside the workspace until the owner's branch is merged, and that merge is one explicit call.
- Everything works identically for every programming language.

## Non-goals for this project

- Running commands, tests or builds (project 2).
- Edits addressed by symbol name, rename, references (project 3).
- Several repositories behind one plugin, and tying a session to one conversation (project 4).
- Resolving merge conflicts through the tools.
- Binary files, file modes, symbolic links, submodules.
- Windows.

## Anti-goals

- A write tool available without the owner having started repochat in write mode.
- A write that leaves a change set half applied.
- Fuzzy matching: guessing which text the model meant.
- An edit that changes or removes a value the model was only ever shown masked.
- An anchor that tells the model anything about a masked value.
- Any write tool that can reach a withheld file, a protected path, or anything through a symbolic link.
- Running a hook, filter or any other program configured by the repository, at any step, including the final merge.

## Constraints

- Go, standard library only, one static binary, Linux and macOS, git 2.25 or newer.
- All existing guarantees of read mode hold unchanged when write mode is off, and the tool list is the same 13 tools.
- The exposure policy still decides what the model can see; in write mode it also decides what it can touch.
- Every git command goes through the existing hardened runner.

## What is not known yet

A probe (branch `spike/chatgpt-probe`) is with the owner. Its results switch three things:

| Question | If yes | If no |
|---|---|---|
| May a Plus account call a tool that is not read-only? | build as designed | write mode cannot be used from ChatGPT; it would still work from other MCP clients, and the owner decides whether to continue |
| Does a request identify its conversation? | project 4 can tie a session to a conversation | one open session per repository, shared by every chat that uses the plugin (as designed here) |
| Does the client declare elicitation? | a later version may ask the owner questions in-band | not used; nothing here depends on it |

Nothing in this design requires a yes to the second or third question.

## Approaches

**How write mode is built overall.** Three were considered:

- A. Everything inside repochat, including the sandbox of project 2, on the kernel's own features. Nothing extra to install; the most dangerous code is ours.
- B. The same modules with the sandbox delegated to bubblewrap and Apple's sandbox. Less of our own security code; one more program required.
- C. Hand editing and running to Codex CLI. Dropped: it spends the allowance this tool exists to save.

The owner picked **A**.

**How an edit names the text it changes.** Four were considered:

- Search and replace: the model quotes the old text. Common, but a poor fit here. The model sees masked text, so a quoted line containing a placeholder never matches the file.
- Patch formats (unified diff, `apply_patch`): worst in both benchmarks found; mostly format errors.
- Whole-file rewrite: reliable, but costs output in proportion to file size.
- Line anchors: every line the model reads carries its number and a short hash of its real content; an edit cites anchors and gives only the new text.

**Line anchors** are used (my call, with the owner's agreement in conversation). They need no quoting of old text, detect a stale view by construction, and work for any language. The evidence is one benchmark by the format's author plus mixed independent checks; the masking argument does not depend on it.

## Design

### Turning it on

`repochat --write` starts sharing in write mode. Without the flag nothing below exists. The banner says so plainly, names the session branch, and repeats that ChatGPT can now change files in the workspace.

### The session workspace

- A session is a linked git worktree on a new branch `repochat/<date>-<short id>`, created from the commit the owner's checkout is on when the session starts. It lives under repochat's state directory, not inside the repository.
- It starts from that commit. Uncommitted changes in the owner's checkout are not part of it; the banner warns when there are any (my call).
- There is one open session per repository. Starting `repochat --write` again resumes it; `--new-session` starts another and leaves the old branch in place (my call).
- In write mode every tool, old and new, works on the workspace. The model therefore sees its own edits, and does not see the owner's untracked files.
- Tracked files that the exposure policy withholds are not materialised in the workspace, and their absence is never recorded as a deletion. Project 2 needs this (a command could otherwise print them); it is done here so there is one mechanism.
- The owner can inspect the workspace and the branch at any time with ordinary git.

### Anchors

In write mode, tools that show file lines (`read_file`, `read_files`, `search_code`) prefix each line with `<line>:<hash>|`. Read mode output is unchanged.

The hash is short (two or three characters) and is computed, with a key that exists only for the session, from the line **as the model is shown it**, that is after masking. An anchor therefore says "this is the line you saw" and carries no information about a hidden value: two different secrets that mask to the same text give the same anchor. A stale view is still detected for everything the model could see.

### Change sets

One change set is an ordered list of operations over any number of files:

| Operation | Addresses | Carries |
|---|---|---|
| replace | first and last anchor of a range | the new lines |
| insert | an anchor, before or after; or the start or end of the file | the new lines |
| delete | first and last anchor of a range | nothing |
| create | a new path | the whole content |
| remove | a path, with the file's whole-content hash | nothing |
| move | a path and a new path | nothing |

Rules:

- All anchors refer to the files as they are before the change set. Ranges in one file may not overlap.
- An anchor whose hash does not match the current line makes the whole change set fail. The error returns the current lines around each stale anchor, with fresh anchors, so the model can retry without re-reading the file.
- A file keeps its line-ending style and its final newline, or lack of one. Only UTF-8 text files can be edited.
- A line that carries a masked value is locked: it cannot be replaced or deleted, and no range may include it. Inserting before or after it is fine. The error says that the line holds a value the model was not shown and that the owner has to change it by hand. Whether a line is locked comes from the redactor, never from looking for the placeholder's spelling. Removing or moving a whole file is still allowed; its content stays in history. With `--no-redact` nothing is masked and nothing is locked (my call: refusing is the only rule that cannot destroy a secret nobody reviewed).
- Because locked lines cannot be overwritten, new text that happens to contain the spelling of a placeholder is ordinary text. Files that legitimately contain it, such as repochat's own sources, stay editable.

### Proposing and applying

- `propose_changes` (read-only): validates a change set completely and returns the diff it would produce, a summary, any warnings, and a plan identifier. It writes nothing to the workspace.
- `apply_changes` (write): takes a plan identifier or a change set directly. It validates again against the current files, applies everything or nothing, and records a checkpoint. It returns the checkpoint, a diff summary, and fresh anchors around every touched region.

Accepting a change set directly in `apply_changes` is my call: with approval remembered for the conversation nobody reviews the preview, and a forced second call would only cost a round trip. `propose_changes` remains for when the model or the owner wants to look first.

A plan is kept on the owner's machine for a limited time and is tied to the content of the files it touches. Applying a plan twice gives the same checkpoint the second time; applying a plan whose files have changed fails.

### Checks before anything is written

A change set is refused as a whole when any of these holds:

- a path is protected: `.git` and everything in it, continuous-integration definitions (`.github/workflows`, `.gitlab-ci.yml`, and the equivalents of other services), hook directories (`.husky`, `.githooks`), `.gitmodules`, `.gitattributes`, `.repochatignore`;
- a path is withheld by the exposure policy, or a new path would be;
- a path is, or passes through, a symbolic link or a submodule;
- a path has a `filter` attribute (Git LFS, git-crypt and similar). The workspace holds such files as git stores them, because no filter program is ever run, so an edit would be written to the owner's checkout in the wrong form (my call);
- a file would be left syntactically broken, for the formats repochat can check with the standard library: Go source must parse and JSON must be valid. Other languages are not checked in this project (project 3 extends this through language servers);
- a file would contain conflict markers, NUL bytes or invalid UTF-8;
- the size limits are exceeded (files per change set, bytes of new text, size of a resulting file).

Refusing rather than warning on a syntax error is my call. A change set is atomic across files, so there is no legitimate need for a broken intermediate state, and the one study found (SWE-agent) measured a clear gain from refusing.

Go files are not reformatted automatically; a file that `gofmt` would change produces a warning (my call: silent reformatting would move lines under the model's anchors).

### Applying

Files are written through the same no-follow file access that reads use, extended for writing: each new content goes to a temporary file in the same directory and is renamed into place. If any step fails, the workspace is returned to the previous checkpoint. Existing files keep their mode; new files are ordinary non-executable files.

### Checkpoints

- Every applied change set is one commit on the session branch, with the message the model gave and a trailer saying it was made through repochat. The author is the owner's git identity when one is configured, otherwise a fixed repochat identity (my call).
- `list_checkpoints` (read-only) shows them.
- `undo_to_checkpoint` (write) restores the files of an earlier checkpoint as a new checkpoint. Nothing is ever discarded from the session's history (my call).
- An update from the owner's branch (see Finishing) is listed as a checkpoint of its own kind. Undo cannot go back past the latest update: doing so would revert the owner's own commits and then merge that reversal into the owner's branch (my call).
- The existing `git_diff`, `git_log` and `git_show` work on the session branch, so the model and the owner can see what the session has done since it started.

### Finishing

- `session_status` (read-only): the branch, where it started, how far it is ahead, which files differ, and whether the owner's branch has moved since.
- `finish_session` (write, marked destructive): merges the session into the branch it started from, in the owner's checkout, then removes the workspace and the session branch.
  - Fast-forward only, or as a single squashed commit when asked.
  - It refuses when the owner's checkout is on another branch, when the owner's branch has moved (the session must first be brought up to date), or when git would have to overwrite uncommitted changes.
  - No hook runs, as everywhere else. A repository that relies on a post-merge hook has to run it by hand; the result says so.
  - The result, and the terminal, list changed files that get executed on the owner's machine later: build files, package scripts, shell scripts, container files. This cannot be prevented, only shown.
- `update_session` (write): merges the owner's branch into the session when it has moved. If that conflicts, it is abandoned and reported; resolving conflicts through the tools is out of scope.
- `abandon_session` (write, marked destructive): removes the workspace. The branch is kept unless the call says otherwise (my call).
- The owner can do all of this from the terminal as well: `repochat session` shows the state, and merging the branch by hand with git always works.

No terminal confirmation is asked for the merge, following the owner's choice. `finish_session` is a different tool from `apply_changes`, so ChatGPT asks for it separately at least once per conversation. `--confirm-merge` adds a terminal confirmation for owners who want one (my call).

### After a session ends

When `finish_session` or `abandon_session` has run, there is no workspace. From then on the read tools serve the owner's checkout exactly as read mode does, and every write tool except `start_session` refuses, saying that no session is open. `start_session` (write) opens a new one from the commit the owner's checkout is on. The model never gets a session without asking for one, and the owner sees each start in the terminal (my call: the alternative, making the owner restart repochat between tasks, costs more than it protects).

### Which exposure rules apply

A workspace is built from a commit, so it lacks whatever the owner has not committed. The rules are therefore taken from both places: the `.repochatignore` in the owner's checkout as it is on disk, and the one in the workspace. A path withheld by either is withheld. `--deny`, `--allow` and the other flags are those of the current run. When a session is resumed, the workspace is brought in line first: files that the rules now withhold are taken out of it before anything is served (my call, and the conservative one: every other choice can expose a file the owner has just decided to hide).

### Signed commits

Session commits are not signed: signing runs a program, and repochat runs none. When the owner's git configuration asks for signed commits, `finish_session` refuses and explains how to merge the branch by hand, so that unsigned commits never land on the owner's branch through the tool. Otherwise the result notes that the commits are unsigned (my call).

### Tools

Write mode adds nine tools to the 13: `start_session`, `propose_changes`, `apply_changes`, `list_checkpoints`, `undo_to_checkpoint`, `session_status`, `update_session`, `finish_session`, `abandon_session`. Read-only ones are labelled so; the others are labelled as writes, and the two that cannot be undone as destructive.

### What is recorded

Every applied change set, undo, update, finish and abandon is written to the local activity log with the checkpoint, the files and the line counts. The diffs themselves are in git.

## What changes in the guarantees

Read mode: nothing.

Write mode, stated as what repochat will guarantee and test:

- Writes happen only inside the session workspace, plus the git bookkeeping of the session branch, plus the one merge into the owner's branch when `finish_session` is called.
- No write reaches a protected path, a withheld file, or anything outside the workspace, including through symbolic links replaced during the call.
- A change set is applied completely or not at all.
- A change made against outdated content is refused.
- No program configured by the repository runs at any point.

What it will not guarantee:

- That an edit is correct or harmless. With approval remembered, text in the repository can steer the model into making changes nobody reviewed. Until project 2, the damage is limited to the session branch and to whatever the owner merges.
- That merged code is safe to run.
- That the session belongs to one conversation: any chat that can use the plugin works on the same session.

## Testing

- Each rule above gets a test that fails first.
- The exposure boundary test is extended to writes: canary files in protected, withheld and outside locations must be byte-identical after every write tool has been driven against them with hostile arguments.
- Symbolic-link swaps during a write, stale anchors, overlapping ranges, a failure injected in the middle of a multi-file apply, plan replay, concurrent applies, line endings and final newlines.
- Masking: locked lines cannot be changed by any operation or range; two different secrets give the same anchor; a guessed secret cannot be confirmed by writing it and reading the anchor back; files containing the placeholder's spelling stay editable.
- Rules from an uncommitted `.repochatignore` apply in the workspace; a resumed session drops files that have become withheld; undo cannot cross an update; filtered paths and signing configurations are refused as described.
- The git-version matrix (2.25, 2.34, 2.43, newest) covers worktree creation, withheld-file handling and merging.
- An adversarial review of the finished code, with every finding reproduced before it is fixed, before write mode is called anything but experimental.

## Left to the builder

The hash function and length of anchors, the form of plan identifiers and how long plans live, the size limits, the exact list of continuous-integration paths, the wording of messages and tool descriptions, how withheld tracked files are kept out of the workspace, and the layout of session state on disk.
