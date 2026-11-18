// Command repochat lets ChatGPT (or any MCP client) read the Git repository
// you are standing in — read-only, over OpenAI's Secure MCP Tunnel.
package main

import (
	"os"

	"repochat/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
