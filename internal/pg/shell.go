// Package pg discovers Postgres on the user's machine (Docker containers,
// Kubernetes pods, ports, sockets) and runs Postgres tools (psql, pg_dump…) with
// whatever the environment provides: docker exec, kubectl exec, host binaries, docker run.
// → ADR-0002, ADR-0009
package pg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

// Cmd is an external process to run. Env is KEY=VALUE pairs added to the
// process environment; secrets only travel through here, never through Args.
type Cmd struct {
	Name   string
	Args   []string
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer // nil: collected into Result.Stdout
}

// Result is the outcome when the process exits with code 0.
type Result struct {
	Stdout []byte
	Stderr []byte
}

// ExitError is a process exiting with a non-zero code; stderr is kept intact for the user to inspect.
type ExitError struct {
	Cmd    string
	Code   int
	Stderr []byte
}

func (e *ExitError) Error() string {
	msg := strings.TrimSpace(string(e.Stderr))
	if msg == "" {
		return fmt.Sprintf("%s: exit %d", e.Cmd, e.Code)
	}
	return fmt.Sprintf("%s: exit %d: %s", e.Cmd, e.Code, msg)
}

// Shell runs external processes; an interface so tests need no Docker.
type Shell interface {
	Run(ctx context.Context, c Cmd) (Result, error)
}

// ExecShell runs for real via os/exec. Verbose logs every command to slog (env
// variable names only, no values).
type ExecShell struct {
	Verbose bool
}

func (s ExecShell) Run(ctx context.Context, c Cmd) (Result, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	// Do not set WaitDelay: that timer runs even when the child exits normally,
	// while the tail of pg_dump's stdout is still waiting to be uploaded (minio-go
	// does not read the pipe while pushing a part) → spurious backup failure. Hanging
	// on ctx cancel is handled locally (StartSession closes the session's stdin pipe).
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Stdin = c.Stdin
	var stdout, stderr bytes.Buffer
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	} else {
		cmd.Stdout = &stdout
	}
	cmd.Stderr = &stderr
	if s.Verbose {
		slog.Debug("exec", "cmd", c.Name+" "+strings.Join(c.Args, " "), "env", envNames(c.Env))
	}
	err := cmd.Run()
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	var ee *exec.ExitError
	switch {
	case err == nil:
		return res, nil
	case errors.As(err, &ee):
		return res, &ExitError{Cmd: c.Name + " " + firstArg(c.Args), Code: ee.ExitCode(), Stderr: stderr.Bytes()}
	default:
		return res, fmt.Errorf("%s: %w", c.Name, err)
	}
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func envNames(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		out = append(out, k)
	}
	return out
}
