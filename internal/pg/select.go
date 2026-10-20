package pg

import (
	"context"
	"fmt"
)

// Where is where Postgres runs: a Docker container on this machine, a cluster
// installed on this machine reached over its socket, or (empty) anything
// reached over TCP with a connection string. → ADR-0013, ADR-0015
type Where struct {
	Container string
	Local     *Local
}

// Tools is the runner selection table of ADR-0013 over one Shell. Detection
// results are cached for one CLI run.
type Tools struct {
	Shell Shell
	// SocketOwner reads the owner of a local socket; nil: the file's owner on disk.
	SocketOwner func(Local) (uid, gid int, err error)

	dockerOK *bool
	hasTool  map[string]bool
}

// NewTools uses the given shell.
func NewTools(sh Shell) *Tools {
	return &Tools{Shell: sh}
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

// Pick chooses a runner per ADR-0013. major is the server's major (0 when
// unknown: the client image is used, whose psql reads older servers).
//   - container → docker exec; no pg_dump inside → postgres:<major> on the container's network
//   - local → docker run postgres:<major> --network none, socket mounted, as the socket's owner
//   - URL → docker run postgres:<major> --network host
func (t *Tools) Pick(ctx context.Context, w Where, major int) (Runner, error) {
	image := ClientImage
	if major > 0 {
		image = ImageForMajor(major)
	}
	if !t.DockerOK(ctx) {
		return nil, fmt.Errorf("Docker is not available (`docker version` failed); bracer runs every Postgres tool through Docker")
	}
	if w.Local != nil {
		owner := t.SocketOwner
		if owner == nil {
			owner = SocketOwner
		}
		uid, gid, err := owner(*w.Local)
		if err != nil {
			return nil, err
		}
		return SocketRunner{Shell: t.Shell, Image: image, Local: *w.Local, UID: uid, GID: gid}, nil
	}
	if w.Container != "" {
		if t.cachedHasTool("container:"+w.Container, func() bool { return t.Docker().HasTool(ctx, w.Container, "pg_dump") }) {
			return ExecRunner{Shell: t.Shell, Container: w.Container}, nil
		}
		// docker exec also fails when the container is stopped: say that rather
		// than the image runner's "cannot join network namespace".
		if st, err := t.Docker().ContainerState(ctx, w.Container); err != nil {
			return nil, err
		} else if st != "running" {
			return nil, fmt.Errorf("container %s is not running (%s)\nFix: docker start %s", w.Container, st, w.Container)
		}
		return ImageRunner{Shell: t.Shell, Image: image, Network: "container:" + w.Container}, nil
	}
	return NewURLRunner(t.Shell, image), nil
}

// Resolve picks a runner, asks the server for its version if unknown, then picks
// again for the right major. Returns the runner and major.
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
