// Package cli holds the subcommands of bracer; main.go only calls Main.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
)

// Version is set by main from ldflags.
var Version = "dev"

const usage = `bracer — backups proven, crons watched, nothing held.

Usage:
  bracer <command> <app> [flags]
  bracer status
  bracer restore

Every file of an app is in ~/.config/bracer/<app>/, and every command names
its app; only status, without a name, shows every app on this machine, and
restore takes no app name: it starts from the bucket.

Commands:
  init        create an app: find Postgres, pick databases, prove a backup (test-restored), install the timer
  init --web  the same, picking the Postgres in the browser; every run then reports to https://app.bracer.dev
  remove      remove an app from this machine (its backups stay in the bucket)
  status      every app on this machine: last run, next run
  backup      dump, encrypt and upload every database in app.yaml
  restore     load a backup from the bucket into a Postgres of your choice
  rotate-key  switch to a new age key; old backups stay readable with the old key
  version     print the version
`

// Exit codes per conventions: 0 ok, 1 runtime error, 2 usage error
// (ADR-0022, ADR-0029: a warning does not change the exit).
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// Main runs the subcommand and returns the exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	switch cmd := args[0]; cmd {
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, Version)
		return exitOK
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return exitOK
	case "init":
		return runInit(ctx, args[1:], defaultInitDeps(stdin, stdout, stderr))
	case "backup":
		return runBackup(ctx, args[1:], defaultBackupDeps(stdin, stdout, stderr))
	case "restore":
		return runRestore(ctx, args[1:], defaultBackupDeps(stdin, stdout, stderr))
	case "status":
		return runStatus(ctx, args[1:], defaultScheduleDeps(stdin, stdout, stderr))
	case "remove":
		return runRemove(ctx, args[1:], defaultScheduleDeps(stdin, stdout, stderr))
	case "rotate-key":
		return runRotateKey(ctx, args[1:], stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "bracer: unknown command %q\n\n%s", cmd, usage)
		return exitUsage
	}
}
