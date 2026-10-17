package pg

import (
	"context"
	"fmt"
	"strings"
)

// Where is where Postgres runs. Exactly one field is set; all three empty means
// Postgres is reached over TCP (host:port on this machine, or a connection string).
type Where struct {
	Container string
	Pod       PodRef
	SocketDir string
}

// Tools is the tooling available in the environment (docker, kubectl, the
// host's Postgres binaries) and the runner selection table per ADR-0009. Detection
// results are cached for one CLI run. LookPath/Glob can be swapped in tests.
type Tools struct {
	Shell    Shell
	GOOS     string
	LookPath func(string) (string, error)
	Glob     func(string) ([]string, error)

	local     *LocalTools
	localDone bool
	kube      *Kube
	kubeDone  bool
	dockerOK  *bool
	hasTool   map[string]bool
}

// NewTools uses the real lookups.
func NewTools(sh Shell, goos string) *Tools {
	return &Tools{Shell: sh, GOOS: goos}
}

// Local is the host's Postgres tool set; nil if there is none.
func (t *Tools) Local(ctx context.Context) *LocalTools {
	if !t.localDone {
		t.local, _ = LocalFinder{Shell: t.Shell, LookPath: t.LookPath, Glob: t.Glob}.Find(ctx)
		t.localDone = true
	}
	return t.local
}

// Kubectl is kubectl or k3s kubectl; nil if there is none.
func (t *Tools) Kubectl() *Kube {
	if !t.kubeDone {
		t.kube, _ = FindKubectl(t.Shell, t.LookPath)
		t.kubeDone = true
	}
	return t.kube
}

// Docker is the Docker CLI through the same Shell.
func (t *Tools) Docker() Docker { return Docker{Shell: t.Shell} }

// DockerOK reports whether `docker version` works (both client and daemon).
func (t *Tools) DockerOK(ctx context.Context) bool {
	if t.dockerOK == nil {
		ok := t.Docker().Version(ctx) == nil
		t.dockerOK = &ok
	}
	return *t.dockerOK
}

func (t *Tools) cachedHasTool(key string, probe func() bool) bool {
	if t.hasTool == nil {
		t.hasTool = map[string]bool{}
	}
	if v, ok := t.hasTool[key]; ok {
		return v
	}
	v := probe()
	t.hasTool[key] = v
	return v
}

// localSearched is where pg_dump was looked for, for error messages.
const localSearched = "PATH, /usr/lib/postgresql/*/bin, /usr/pgsql-*/bin, /opt/homebrew/opt/postgresql@*/bin"

// Pick chooses a runner per the ADR-0009 table. major is the server's major (0
// when unknown): host binaries older than the server are rejected.
func (t *Tools) Pick(ctx context.Context, w Where, major int) (Runner, error) {
	image := ClientImage
	if major > 0 {
		image = ImageForMajor(major)
	}
	switch {
	case w.Container != "":
		if t.cachedHasTool("container:"+w.Container, func() bool { return t.Docker().HasTool(ctx, w.Container, "pg_dump") }) {
			return ExecRunner{Shell: t.Shell, Container: w.Container}, nil
		}
		return ImageRunner{Shell: t.Shell, Image: image, Network: "container:" + w.Container}, nil
	case w.Pod.Name != "":
		kube := t.Kubectl()
		if kube == nil {
			return nil, fmt.Errorf("kubectl not found (looked for kubectl and k3s on PATH); it is needed to reach pod %s", w.Pod)
		}
		if !t.cachedHasTool("pod:"+w.Pod.String(), func() bool { return kube.HasTool(ctx, w.Pod, "pg_dump") }) {
			return nil, fmt.Errorf("pod %s has no pg_dump; use an image that ships the PostgreSQL client tools, or point bracer at the service with a postgres:// URL", w.Pod)
		}
		return KubeExecRunner{Kube: *kube, Pod: w.Pod}, nil
	case w.SocketDir != "":
		local := t.Local(ctx)
		if local == nil {
			return nil, fmt.Errorf("no pg_dump on this host (looked in %s); Postgres at %s is only reachable over its Unix socket. Install the PostgreSQL client package%s, or enable TCP and use host/port", localSearched, w.SocketDir, forMajor(major))
		}
		if major > 0 && local.Major < major {
			return nil, fmt.Errorf("pg_dump %s in %s is older than PostgreSQL %d and cannot dump it; install the PostgreSQL %d client package", local.Version, local.Dir, major, major)
		}
		return LocalRunner{Shell: t.Shell, Tools: *local}, nil
	default:
		// Prefer the same major (ADR-0009 § Amendments): a pg_dump newer than the server
		// produces an archive that a pg_restore of the server's major cannot read, and
		// emits SET for parameters the old server does not know. Major unknown: ask via the host binary.
		local := t.Local(ctx)
		if local != nil && (major == 0 || local.Major == major) {
			return LocalRunner{Shell: t.Shell, Tools: *local}, nil
		}
		if t.DockerOK(ctx) {
			return NewHostRunnerFor(t.Shell, image, t.GOOS), nil
		}
		if local != nil && local.Major > major {
			// No Docker: a newer binary still dumps correctly; the manifest records
			// tool_version so drill/restore can pick a new enough pg_restore.
			return LocalRunner{Shell: t.Shell, Tools: *local}, nil
		}
		var why []string
		if local == nil {
			why = append(why, "no pg_dump on this host (looked in "+localSearched+")")
		} else {
			why = append(why, fmt.Sprintf("pg_dump %s in %s is older than PostgreSQL %d", local.Version, local.Dir, major))
		}
		why = append(why, "Docker is not available (`docker version` failed)")
		return nil, fmt.Errorf("%s; install the PostgreSQL client package%s or Docker", strings.Join(why, " and "), forMajor(major))
	}
}

func forMajor(major int) string {
	if major > 0 {
		return fmt.Sprintf(" for PostgreSQL %d", major)
	}
	return ""
}

// Resolve picks a runner, asks the server for its version if unknown, then picks
// again for the right major (host binary too old → Docker image). Returns the runner and major.
func (t *Tools) Resolve(ctx context.Context, w Where, conn Conn, major int) (Runner, int, error) {
	r, err := t.Pick(ctx, w, major)
	if err != nil {
		return nil, 0, err
	}
	if major > 0 {
		return r, major, nil
	}
	_, major, err = (&Client{Runner: r, Conn: conn}).ServerVersionNum(ctx)
	if err != nil {
		return r, 0, err
	}
	r, err = t.Pick(ctx, w, major)
	if err != nil {
		return nil, major, err
	}
	return r, major, nil
}
