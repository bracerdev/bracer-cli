package pg

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// Local is Postgres installed on this machine, reached over its Unix socket
// (ADR-0015).
type Local struct {
	Dir  string // socket directory, /var/run/postgresql
	Port int
}

// SocketPath is the socket file of the cluster: <dir>/.s.PGSQL.<port>.
func (l Local) SocketPath() string {
	return filepath.Join(l.Dir, ".s.PGSQL."+strconv.Itoa(l.Port))
}

// SocketOwner returns the uid and gid owning the socket file. The tools run
// with that uid so `peer` authentication sees the postgres user of the host.
// A missing socket is an error: bracer never falls back to TCP.
func SocketOwner(l Local) (uid, gid int, err error) {
	fi, err := os.Stat(l.SocketPath())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, fmt.Errorf("socket %s does not exist: Postgres on port %d is not running on this machine, or uses another socket_dir", l.SocketPath(), l.Port)
		}
		return 0, 0, err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return 0, 0, fmt.Errorf("%s is not a socket", l.SocketPath())
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("cannot read the owner of %s", l.SocketPath())
	}
	return int(st.Uid), int(st.Gid), nil
}

// SocketRunner: `docker run --rm --network none` of postgres:<major> with the
// socket directory mounted at the same path, as the socket's owner, so the
// tools connect over the socket with peer authentication (ADR-0015).
type SocketRunner struct {
	Shell    Shell
	Image    string
	Local    Local
	UID, GID int
}

func (r SocketRunner) Describe() string {
	return fmt.Sprintf("docker run %s --network none -v %s (socket, uid %d)", r.Image, r.Local.Dir, r.UID)
}

func (r SocketRunner) Run(ctx context.Context, conn Conn, tool string, args []string, stdin io.Reader, stdout io.Writer) error {
	_, err := r.RunStderr(ctx, conn, tool, args, stdin, stdout)
	return err
}

// RunStderr is like Run but also returns stderr even when the command succeeds.
func (r SocketRunner) RunStderr(ctx context.Context, conn Conn, tool string, args []string, stdin io.Reader, stdout io.Writer) ([]byte, error) {
	conn.Host, conn.Port = r.Local.Dir, r.Local.Port
	env := conn.Env()
	dargs := []string{"run", "--rm", "-i", "--network", "none",
		"--user", fmt.Sprintf("%d:%d", r.UID, r.GID),
		"-v", r.Local.Dir + ":" + r.Local.Dir}
	for _, n := range envNames(env) {
		dargs = append(dargs, "-e", n)
	}
	dargs = append(dargs, r.Image, tool)
	dargs = append(dargs, args...)
	res, err := r.Shell.Run(ctx, Cmd{Name: "docker", Args: dargs, Env: env, Stdin: stdin, Stdout: stdout})
	return res.Stderr, err
}

// LocalConn is the connection for a local cluster: user postgres unless
// app.yaml names another one, over the socket.
func LocalConn(l Local, user, password string) Conn {
	if user == "" {
		user = "postgres"
	}
	return Conn{Host: l.Dir, Port: l.Port, User: user, Password: password, Database: "postgres"}
}
