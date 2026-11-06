package pg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Runner runs a Postgres tool (psql, pg_dump, pg_dumpall, pg_restore) with the
// given connection info: docker exec into the user's container, or docker run
// of postgres:<major>. → ADR-0013
type Runner interface {
	Run(ctx context.Context, conn Conn, tool string, args []string, stdin io.Reader, stdout io.Writer) error
	// Describe is a one-liner for --dry-run and --verbose.
	Describe() string
}

// ExecRunner: `docker exec` into the user's own Postgres container; connects
// over the Unix socket inside the container, so Conn.Host is left empty. Each
// command gets its own application_name, by which a cancel terminates its
// backend: the daemon does not signal an exec'd process when the docker CLI dies.
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
	app := uniqueName("bracer-")
	env := withAppName(conn.Env(), app)
	dargs := []string{"exec", "-i"}
	for _, n := range envNames(env) {
		dargs = append(dargs, "-e", n)
	}
	dargs = append(dargs, r.Container, tool)
	dargs = append(dargs, args...)
	// The stop connects as the command did, under an application_name of its own.
	stopEnv := withAppName(conn.Env(), "bracer-stop")
	sargs := []string{"exec"}
	for _, n := range envNames(stopEnv) {
		sargs = append(sargs, "-e", n)
	}
	sargs = append(sargs, r.Container, "psql", "-X", "-A", "-t", "-q", "-c",
		"select pg_terminate_backend(pid) from pg_stat_activity where application_name = "+QuoteLiteral(app)+" and pid <> pg_backend_pid()")
	res, err := runStoppable(ctx, r.Shell,
		Cmd{Name: "docker", Args: dargs, Env: env, Stdin: stdin, Stdout: stdout},
		Cmd{Name: "docker", Args: sargs, Env: stopEnv})
	return res.Stderr, err
}

// withAppName sets PGAPPNAME in a libpq env, replacing one from the URL's parameters.
func withAppName(env []string, app string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "PGAPPNAME=") {
			out = append(out, kv)
		}
	}
	return append(out, "PGAPPNAME="+app)
}

// stopTimeout bounds the command that stops a cancelled or failed tool.
const stopTimeout = 30 * time.Second

// runStoppable runs c, and runs stop once when ctx ends while c runs (at once,
// not after c returns: c may hang on its stdin) or when c fails (an upload
// that broke closes the pipe and the docker CLI dies of EPIPE, ctx still
// live). The docker CLI dying, killed by exec.CommandContext or not, leaves a
// `docker run` client container or a `docker exec`'d tool running, and
// pg_dump with it, holding its snapshot and locks on the source. stop runs
// under a context that is not cancelled, bounded by stopTimeout; its error is
// ignored: the container or the backend may already be gone.
func runStoppable(ctx context.Context, sh Shell, c, stop Cmd) (Result, error) {
	var once sync.Once
	runStop := func() {
		once.Do(func() {
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
			defer cancel()
			_, _ = sh.Run(sctx, stop)
		})
	}
	finished, watched := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watched)
		select {
		case <-ctx.Done():
			runStop()
		case <-finished:
		}
	}()
	res, err := sh.Run(ctx, c)
	close(finished)
	<-watched
	if err != nil {
		runStop() // a cancelled c fails too: the watcher may have seen finished first
	}
	return res, err
}

// uniqueName is prefix + 16 random hex characters: no other container or
// session has it.
func uniqueName(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return prefix + hex.EncodeToString(b)
}

// clientRunArgs starts the `docker run` of a throwaway client container: named,
// so that a cancel can remove it (runStoppable), and without a log driver,
// since its stdout is the dump and dockerd copies a container's stdout to the
// log driver (json-file by default, unbounded, on the host's disk) even while
// attached.
func clientRunArgs(name string) []string {
	return []string{"run", "--rm", "-i", "--log-driver", "none", "--name", name}
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
// by target: "container:<name>" for a container without pg_dump, "host" for a URL
// (Postgres on this Linux machine or anywhere it reaches). → ADR-0013
type ImageRunner struct {
	Shell   Shell
	Image   string
	Network string
}

// NewURLRunner reaches a URL from the host's network namespace, so 127.0.0.1 in
// the URL is this machine.
func NewURLRunner(sh Shell, image string) ImageRunner {
	return ImageRunner{Shell: sh, Image: image, Network: "host"}
}

func (r ImageRunner) Describe() string {
	var parts []string
	parts = append(parts, "docker run "+r.Image)
	if r.Network != "" {
		parts = append(parts, "--network "+r.Network)
	}
	return strings.Join(parts, " ")
}

func (r ImageRunner) Run(ctx context.Context, conn Conn, tool string, args []string, stdin io.Reader, stdout io.Writer) error {
	_, err := r.RunStderr(ctx, conn, tool, args, stdin, stdout)
	return err
}

// RunStderr is like Run but also returns stderr even when the command succeeds.
func (r ImageRunner) RunStderr(ctx context.Context, conn Conn, tool string, args []string, stdin io.Reader, stdout io.Writer) ([]byte, error) {
	if r.Network != "" && strings.HasPrefix(r.Network, "container:") && conn.Host == "" {
		conn.Host = "localhost"
	}
	env := conn.Env()
	name := uniqueName("bracer-pg-")
	dargs := clientRunArgs(name)
	if r.Network != "" {
		dargs = append(dargs, "--network", r.Network)
	}
	for _, n := range envNames(env) {
		dargs = append(dargs, "-e", n)
	}
	dargs = append(dargs, r.Image, tool)
	dargs = append(dargs, args...)
	res, err := runStoppable(ctx, r.Shell,
		Cmd{Name: "docker", Args: dargs, Env: env, Stdin: stdin, Stdout: stdout},
		Cmd{Name: "docker", Args: []string{"rm", "-f", name}})
	return res.Stderr, err
}

// ClientImage is the fixed, latest client image used to ask the server for its
// version before the major is known (spec init step 4).
const ClientImage = "postgres:18-alpine"

// ImageForMajor is the image with the same major as the server, used for dump and drill.
func ImageForMajor(major int) string {
	return fmt.Sprintf("postgres:%d-alpine", major)
}
