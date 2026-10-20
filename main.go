// Command bracer is the Bracer CLI: init, backup, drill, fetch, restore, ping,
// schedule, rotate-key. The monitoring server is a separate program.
package main

import (
	"os"

	"bracer.dev/bracer/internal/cli"
)

// version is set at build time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

func main() {
	cli.Version = version
	os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
