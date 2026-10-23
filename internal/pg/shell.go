// Package pg discovers Postgres on the user's machine (Docker containers, TCP
// ports) and runs Postgres tools (psql, pg_dump…) through Docker: docker exec into
// the user's container, or docker run of postgres:<major>. → ADR-0002, ADR-0013
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
//
// Docker and Env carry app.yaml's runtime: (spec init § Data model): `docker` is
// run as Docker when set, and then PATH and DOCKER_HOST of every child are
// Env's alone: the process environment does not change which Docker runs, so
// a run by hand is the run of the timer (ADR-0022).
type ExecShell struct {
	Verbose bool
	Docker  string
	Env     []string
}

// baseEnv is the process environment, without PATH and DOCKER_HOST when
// runtime: is applied.
func (s ExecShell) baseEnv() []string {
	if s.Docker == "" {
		return os.Environ()
	}
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PATH=") && !strings.HasPrefix(kv, "DOCKER_HOST=") {
			out = append(out, kv)
		}
	}
	return out
}

func (s ExecShell) Run(ctx context.Context, c Cmd) (Result, error) {
	name := c.Name
	if name == "docker" && s.Docker != "" {
		name = s.Docker
	}
	cmd := exec.CommandContext(ctx, name, c.Args...)
	// Do not set WaitDelay: that timer runs even when the child exits normally,
	// while the tail of pg_dump's stdout is still waiting to be uploaded (minio-go
	// does not read the pipe while pushing a part) → spurious backup failure. Hanging
	// on ctx cancel is handled locally (StartSession closes the session's stdin pipe).
	// exec keeps the last value of a duplicated key: process env < runtime < c.Env.
	cmd.Env = append(append(s.baseEnv(), s.Env...), c.Env...)
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
