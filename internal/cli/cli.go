// Package cli holds the subcommands of bracer; main.go only calls Main.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"

	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/storage"
)

// Version is set by main from ldflags.
var Version = "dev"

const usage = `bracer — backups proven, crons watched, nothing held.

Usage:
  bracer <command> [flags]

Commands:
  init      find Postgres on this machine, pick databases, write app.yaml
  backup    dump, encrypt and upload every database in app.yaml
  drill     restore the latest backup into a temp Postgres and verify it
  fetch     download a backup (decrypted) to a file or stdout
  restore   restore a backup into a real Postgres with pg_restore
  ping      send a heartbeat for any job, or wrap a command: bracer ping <id> -- <cmd>
  cron-example  print a cron / systemd timer schedule for app.yaml
  rotate-key  switch to a new age key; old backups stay readable with the old key
  version   print the version
`

// Exit codes per conventions: 0 ok, 1 runtime error, 2 usage error.
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
	case "drill":
		return runDrill(ctx, args[1:], defaultBackupDeps(stdin, stdout, stderr))
	case "fetch":
		return runFetch(ctx, args[1:], defaultBackupDeps(stdin, stdout, stderr))
	case "restore":
		return runRestore(ctx, args[1:], defaultBackupDeps(stdin, stdout, stderr))
	case "ping":
		return runPing(ctx, args[1:], stdin, stdout, stderr)
	case "cron-example":
		return runCronExample(ctx, args[1:], stdout, stderr)
	case "rotate-key":
		return runRotateKey(ctx, args[1:], stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "bracer: unknown command %q\n\n%s", cmd, usage)
		return exitUsage
	}
}

func defaultInitDeps(stdin io.Reader, stdout, stderr io.Writer) initDeps {
	return initDeps{
		shell:    pg.ExecShell{},
		probe:    pg.ProbeSSLRequest,
		newStore: func(c storage.S3Config) (storage.Store, error) { return storage.NewS3(c) },
		goos:     runtime.GOOS,
		stdin:    stdin,
		stdout:   stdout,
		stderr:   stderr,
	}
}
