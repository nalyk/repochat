// Package cli implements the repochat command line.
package cli

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
)

// Main runs repochat with the given arguments (without the program name) and
// returns the process exit code.
func Main(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	c := newConsole(stdin, stdout, stderr)
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		if len(args) > 0 {
			switch args[0] {
			case "-h", "--help", "-help":
				printHelp(c)
				return 0
			case "--version", "-version", "-v":
				fmt.Fprintf(c.out, "repochat %s %s/%s\n", versionString(), runtime.GOOS, runtime.GOARCH)
				return 0
			}
		}
		return cmdRun(c, args)
	}
	rest := args[1:]
	switch args[0] {
	case "run", "start":
		return cmdRun(c, rest)
	case "mcp":
		return cmdMCP(c, rest)
	case "setup":
		return cmdSetup(c, rest)
	case "doctor":
		return cmdDoctor(c, rest)
	case "status":
		return cmdStatus(c, rest)
	case "stop":
		return cmdStop(c, rest)
	case "tools":
		return cmdTools(c, rest)
	case "call":
		return cmdCall(c, rest)
	case "session":
		return cmdSession(c, rest)
	case "files":
		return cmdFiles(c, rest)
	case "log":
		return cmdLog(c, rest)
	case "mcp-config":
		return cmdMCPConfig(c, rest)
	case "install":
		return cmdInstall(c, rest)
	case "install-helper":
		return cmdInstallHelper(c, rest)
	case "version":
		fmt.Fprintf(c.out, "repochat %s %s/%s\n", versionString(), runtime.GOOS, runtime.GOARCH)
		return 0
	case "help":
		if len(rest) > 0 && rest[0] == "chatgpt" {
			printChatGPTHelp(c)
			return 0
		}
		printHelp(c)
		return 0
	}
	c.fail("unknown command %q", args[0])
	c.say("Run `repochat help` for the list of commands.")
	return 2
}

func printHelp(c *console) {
	fmt.Fprintf(c.out, `%s — ask ChatGPT about the Git repository on your computer. Read-only
unless you start it with --write.

%s
  cd ~/projects/my-project
  repochat

  The first time, repochat installs what it needs, asks for two values from
  OpenAI Platform and shows the one-time ChatGPT step. After that it is just
  "repochat" in any repository. Ctrl+C stops sharing.

%s
  repochat                 share the current repository with ChatGPT
  repochat status          is anything being shared right now?
  repochat stop            stop sharing (from another terminal)
  repochat doctor          check each part of the setup
  repochat setup           enter or change the tunnel ID / API key, manage profiles
  repochat help chatgpt    the one-time ChatGPT plugin steps

%s
  repochat files [--withheld]      which files would be served / held back, and why
  repochat tools                   the tools ChatGPT gets
  repochat call <tool> [json]      run a tool yourself and see what the model would see
  repochat log                     recent activity (which tools were called, when)

%s
  --tracked-only           hide untracked files
  --no-history             working tree only, no past commits
  --deny PATTERN           hold back more files (.gitignore syntax; repeatable)
  --allow PATTERN          serve a file the built-in secret-file rules would hold back
  --show-withheld-names    let the model see the names (never the content) of held-back files
  --no-redact              do not mask secret-looking values
  --profile NAME           use another saved tunnel
  --quiet / --verbose      less / more terminal output
  A .repochatignore file in the repository (same syntax as .gitignore) holds files back for everyone.

%s
  --write                  ChatGPT can also change files: in a session workspace on a side branch
                           (repochat/<date>-<id>), never in your checkout. Nothing reaches your
                           branch until finish_session (or you) merge it. Not with --no-history.
  --new-session            with --write: start a new session instead of resuming the open one;
                           the old session's branch is kept
  --confirm-merge          with --write: finish_session does not merge; you merge in the terminal
  repochat session diff            what finish would land, or one checkpoint [--checkpoint ID] [--stat]
  repochat session                 the open session: branch, checkpoints, changed files
  repochat session undo ID         restore a checkpoint as a new one, after asking [--yes]
  repochat session finish          merge it into your branch, after asking [--squash] [--message TEXT] [--yes]
  repochat session abandon         remove its workspace; the branch is kept [--delete-branch]
  repochat tools --write / call --write   the write-mode tools, from the terminal

%s
  repochat mcp             the MCP server itself (stdio); what tunnel-client starts
  repochat mcp-config      configuration snippet for other MCP clients (Claude, Cursor, …)
  repochat install         copy this binary to ~/.local/bin
  repochat install-helper  install OpenAI's tunnel-client (checksum-verified)
  repochat version

What ChatGPT can see: the files git knows about in this repository (not the ones
ignored by .gitignore), minus files that look like secrets, plus the commit history.
Whatever it reads is sent to OpenAI. Nothing can be changed through repochat
unless you start it with --write.

Project: https://github.com/nalyk/repochat
`, c.bold("RepoChat "+versionString()), c.bold("Everyday use"), c.bold("Commands"), c.bold("See what is exposed"),
		c.bold("Control what is exposed (options for repochat, mcp, files, call, session)"),
		c.bold("Write mode (experimental; options for repochat and mcp)"), c.bold("Plumbing"))
}

func printChatGPTHelp(c *console) {
	c.say("")
	printChatGPTSteps(c, nil)
	c.say("")
	c.say("  Notes")
	c.say("    • repochat must be running while you create the plugin and whenever you use it.")
	c.say("    • The tunnel only shows up in ChatGPT if it is attached to your ChatGPT workspace:")
	c.say("        %s", tunnelsURL)
	c.say("    • Adding custom MCP servers can be restricted by a workspace admin.")
	c.say("    • ChatGPT calls these \"plugins\", \"apps\" or \"connectors\" depending on the screen.")
	c.say("    • Tools not showing up after an update? Plugin settings → Refresh tools.")
	c.say("")
}
