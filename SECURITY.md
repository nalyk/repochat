# Security policy

repochat stands between a language model and the files on your computer, so a hole in it matters. Reports are welcome.

## Reporting a vulnerability

Report privately through GitHub: on the repository page open **Security**, then **Report a vulnerability**. Please do not open a public issue for something that leaks withheld files, masked values, or lets a tool write where it should not.

Include the repochat version (`repochat version`), your git version and operating system, and the smallest repository layout and tool call that show the problem. A failing test is the best report.

This is a personal project maintained in spare time. Expect a first answer within about a week. A confirmed hole gets a fix with a test that failed before it, an entry in [`CHANGELOG.md`](CHANGELOG.md), and credit if you want it.

## What counts

- A tool result that shows content of a withheld file, or a value that should have been masked.
- A read or write outside the shared repository, or in write mode outside the session workspace: through symlinks, submodules, git configuration, hooks, filters, or crafted arguments.
- Write mode changing the owner's checkout, branch or git configuration other than through `finish_session` as documented.
- repochat running a command or fetching from the network because of something in the repository or in a tool call.
- Credentials (the API key, the tunnel) reaching the model, the logs, or another local user.

## What does not

- A secret in a format repochat does not know. Masking is best effort and the README says so.
- Anything that needs a switch the owner turned on to show more: `--no-redact`, `--allow`, `--show-withheld-names`.
- What the model does with content it was allowed to read, and OpenAI's side of the tunnel.
- An attacker who already runs code as your user on your machine.

## Supported versions

Only the latest commit on `main` and the latest release. Write mode is experimental.
