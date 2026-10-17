package pg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
)

// Runner runs a Postgres tool (psql, pg_dump, pg_dumpall, pg_restore) with the
// given connection info: docker exec, kubectl exec, a host binary or
// docker run, depending on where Postgres runs. → ADR-0009
type Runner interface {
	Run(ctx context.Context, conn Conn, tool string, args []string, stdin io.Reader, stdout io.Writer) error
	// Describe is a one-liner for --dry-run and --verbose.
	Describe() string
}

// ExecRunner: `docker exec` into the user's own Postgres container; connects
// over the Unix socket inside the container, so Conn.Host is left empty.
type ExecRunner struct {
	Shell     Shell
	Container string
}

func (r ExecRunner) Describe() string {
	return "docker exec " + r.Container
}

func (r ExecRunner) Run(ctx context.Context, conn Conn, tool string, args []string, stdin io.Reader, stdout io.Writer) error {
	_, err := r.RunStderr(ctx, conn, tool, args, stdin, stdout)
	return err
}

// RunStderr is like Run but also returns stderr even when the command succeeds.
func (r ExecRunner) RunStderr(ctx context.Context, conn Conn, tool string, args []string, stdin io.Reader, stdout io.Writer) ([]byte, error) {
	env := conn.Env()
	dargs := []string{"exec", "-i"}
	for _, n := range envNames(env) {
		dargs = append(dargs, "-e", n)
	}
	dargs = append(dargs, r.Container, tool)
	dargs = append(dargs, args...)
	res, err := r.Shell.Run(ctx, Cmd{Name: "docker", Args: dargs, Env: env, Stdin: stdin, Stdout: stdout})
	return res.Stderr, err
}

// StderrRunner is a Runner whose stderr can be read when the command succeeds
// (counting psql errors with ON_ERROR_STOP=0). Every runner in this package has it.
type StderrRunner interface {
	Runner
	RunStderr(ctx context.Context, conn Conn, tool string, args []string, stdin io.Reader, stdout io.Writer) ([]byte, error)
}

// RunCapture runs through RunStderr if the runner supports it; otherwise stderr is only available on error.
func RunCapture(ctx context.Context, r Runner, conn Conn, tool string, args []string, stdin io.Reader, stdout io.Writer) ([]byte, error) {
	if sr, ok := r.(StderrRunner); ok {
		return sr.RunStderr(ctx, conn, tool, args, stdin, stdout)
	}
	err := r.Run(ctx, conn, tool, args, stdin, stdout)
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Stderr, err
	}
	return nil, err
}

// ImageRunner: `docker run --rm` of a client image (postgres:N). Network is chosen
// by target: "container:<name>" for a container without pg_dump, "host" for
// Postgres on a Linux machine, empty + HostGateway for macOS/Windows.
type ImageRunner struct {
	Shell       Shell
	Image       string
	Network     string
	HostGateway bool // --add-host=host.docker.internal:host-gateway, rewrites localhost to host.docker.internal
}

// NewHostRunner picks how to reach Postgres on this machine based on the OS.
func NewHostRunner(sh Shell, image string) ImageRunner {
	return NewHostRunnerFor(sh, image, runtime.GOOS)
}

// NewHostRunnerFor is NewHostRunner with a given GOOS (for tests).
func NewHostRunnerFor(sh Shell, image, goos string) ImageRunner {
	if goos == "linux" {
		return ImageRunner{Shell: sh, Image: image, Network: "host"}
	}
	return ImageRunner{Shell: sh, Image: image, HostGateway: true}
}

func (r ImageRunner) Describe() string {
	var parts []string
	parts = append(parts, "docker run "+r.Image)
	if r.Network != "" {
		parts = append(parts, "--network "+r.Network)
	}
	if r.HostGateway {
		parts = append(parts, "via host.docker.internal")
	}
	return strings.Join(parts, " ")
}

func (r ImageRunner) Run(ctx context.Context, conn Conn, tool string, args []string, stdin io.Reader, stdout io.Writer) error {
	_, err := r.RunStderr(ctx, conn, tool, args, stdin, stdout)
	return err
}

// RunStderr is like Run but also returns stderr even when the command succeeds.
func (r ImageRunner) RunStderr(ctx context.Context, conn Conn, tool string, args []string, stdin io.Reader, stdout io.Writer) ([]byte, error) {
	if r.HostGateway && isLoopback(conn.Host) {
		conn.Host = "host.docker.internal"
	}
	if r.Network != "" && strings.HasPrefix(r.Network, "container:") && conn.Host == "" {
		conn.Host = "localhost"
	}
	env := conn.Env()
	dargs := []string{"run", "--rm", "-i"}
	if r.Network != "" {
		dargs = append(dargs, "--network", r.Network)
	}
	if r.HostGateway {
		dargs = append(dargs, "--add-host=host.docker.internal:host-gateway")
	}
	for _, n := range envNames(env) {
		dargs = append(dargs, "-e", n)
	}
	dargs = append(dargs, r.Image, tool)
	dargs = append(dargs, args...)
	res, err := r.Shell.Run(ctx, Cmd{Name: "docker", Args: dargs, Env: env, Stdin: stdin, Stdout: stdout})
	return res.Stderr, err
}

func isLoopback(h string) bool {
	switch h {
	case "", "localhost", "127.0.0.1", "::1", "0.0.0.0", "::":
		return true
	}
	return false
}

// ClientImage is the fixed, latest client image used to ask the server for its
// version before the major is known (spec init step 4).
const ClientImage = "postgres:18-alpine"

// ImageForMajor is the image with the same major as the server, used for dump and drill.
func ImageForMajor(major int) string {
	return fmt.Sprintf("postgres:%d-alpine", major)
}
