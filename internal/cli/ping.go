package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"bracer.dev/bracer/internal/ping"
)

// runPing is `bracer ping <id> [--start|--fail|--exit N] [--server URL] [-- cmd args]`
// (spec backup/ping). Does not read app.yaml, does not need Docker.
func runPing(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// Split off the part after "--" before parsing flags.
	var wrapped []string
	for i, a := range args {
		if a == "--" {
			wrapped = args[i+1:]
			args = args[:i]
			break
		}
	}
	fs := flag.NewFlagSet("ping", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage:
  bracer ping <id> [--start | --fail | --exit N] [--server URL]
  bracer ping <id> [--server URL] -- <command> [args...]

Sends a heartbeat for any job to a Bracer server (hosted by default, or
BRACER_SERVER / --server). With "--", runs the command, reports /start,
its exit code and the last 1 KB of output, and exits with the same code.

`)
		fs.PrintDefaults()
	}
	start := fs.Bool("start", false, "report that the job started")
	failFlag := fs.Bool("fail", false, "report that the job failed")
	exitCode := fs.Int("exit", -1, "report the job's exit code (0-255)")
	server := fs.String("server", "", "Bracer server URL (default: $BRACER_SERVER, then "+ping.DefaultServer+")")
	verbose := fs.Bool("verbose", false, "print each request")
	// Allow flags after the id: parse, take the id, parse the rest again.
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return exitUsage
	}
	id := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return exitUsage
	}
	fail := func(code int, format string, a ...interface{}) int {
		fmt.Fprintf(stderr, "bracer ping: "+format+"\n", a...)
		return code
	}
	if fs.NArg() != 0 {
		return fail(exitUsage, "unexpected argument %q (put the command after \"--\")", fs.Arg(0))
	}
	if !ping.ValidID(id) {
		return fail(exitUsage, "invalid heartbeat id %q (letters, digits, - and _ only)", id)
	}
	srv, err := ping.ResolveServer(*server, os.Getenv)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	c := &ping.Client{Server: srv, UserAgent: "bracer/" + Version}
	if *verbose {
		c.Log = func(format string, a ...any) { fmt.Fprintf(stderr, "bracer ping: "+format+"\n", a...) }
	} else {
		c.Log = func(format string, a ...any) {
			// Do not print the URL (it contains the id) at the default level; print errors only.
			if len(a) > 0 {
				if _, isErr := a[len(a)-1].(error); isErr {
					fmt.Fprintf(stderr, "bracer ping: "+format+"\n", a...)
				}
			}
		}
	}

	if len(wrapped) > 0 {
		if *start || *failFlag || *exitCode >= 0 {
			return fail(exitUsage, "--start/--fail/--exit cannot be combined with a wrapped command")
		}
		return c.Wrap(ctx, id, wrapped, stdin, stdout, stderr)
	}

	variant := ""
	n := 0
	if *start {
		variant, n = "start", n+1
	}
	if *failFlag {
		variant, n = "fail", n+1
	}
	if *exitCode >= 0 {
		if *exitCode > 255 {
			return fail(exitUsage, "--exit must be 0-255")
		}
		variant, n = strconv.Itoa(*exitCode), n+1
	}
	if n > 1 {
		return fail(exitUsage, "use only one of --start, --fail, --exit")
	}
	var body []byte
	if f, ok := stdin.(*os.File); ok {
		body = ping.ReadStdinBody(f)
	}
	if err := c.Send(ctx, id, variant, body); err != nil {
		return fail(exitError, "%v", err)
	}
	return exitOK
}
