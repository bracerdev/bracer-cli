// Package backup is the pipeline dump → [age] → bucket → run.json → retention
// (spec backup/backup). The CLI only parses flags and prints the result.
package backup

import (
	"context"
	"fmt"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
)

// ConnectFunc is the signature of Connect, so tests can replace it with a fake.
type ConnectFunc func(ctx context.Context, tools *pg.Tools, in config.Instance, s *config.Secrets) (pg.Conn, pg.Runner, error)

// Connect builds Conn + Runner for one instance in app.yaml per ADR-0013 and
// ADR-0015: container → docker exec (image on its network when it has no
// pg_dump); local → postgres:<pg_major> over the mounted socket; URL → docker
// run postgres:<pg_major>. pg_major in app.yaml picks the image; if
// absent, ask the server.
func Connect(ctx context.Context, tools *pg.Tools, in config.Instance, s *config.Secrets) (pg.Conn, pg.Runner, error) {
	where, conn, err := Target(in, s)
	if err != nil {
		return conn, nil, err
	}
	runner, _, err := tools.Resolve(ctx, where, conn, in.PgMajor)
	if err != nil {
		return conn, nil, fmt.Errorf("instance %s: %w", in.Name, err)
	}
	return conn, runner, nil
}

// Target builds where to run and the connection info (including the password from
// a secret) for one instance, without choosing a runner yet.
func Target(in config.Instance, s *config.Secrets) (pg.Where, pg.Conn, error) {
	conn := pg.Conn{User: in.User, Database: "postgres"}
	switch in.Kind() {
	case config.KindContainer:
		// The socket inside a container is usually `trust`, but not always (Bitnami:
		// `local all all md5`): with password_env, pass it via -e PGPASSWORD.
		if in.PasswordEnv != "" {
			v, err := s.Require(in.PasswordEnv)
			if err != nil {
				return pg.Where{}, conn, err
			}
			conn.Password = v
		}
		return pg.Where{Container: in.Container}, conn, nil
	case config.KindLocal:
		// Peer authentication over the socket first; password_env when pg_hba asks for one.
		l := pg.Local{Dir: in.Local.Dir(), Port: in.Local.Port}
		password := ""
		if in.PasswordEnv != "" {
			v, err := s.Require(in.PasswordEnv)
			if err != nil {
				return pg.Where{}, conn, err
			}
			password = v
		}
		return pg.Where{Local: &l}, pg.LocalConn(l, in.User, password), nil
	case config.KindURL:
		v, err := s.Require(in.URLEnv)
		if err != nil {
			return pg.Where{}, pg.Conn{}, err
		}
		conn, err := pg.ParseURL(v)
		if err != nil {
			return pg.Where{}, conn, fmt.Errorf("instance %s: %s: %w", in.Name, in.URLEnv, err)
		}
		return pg.Where{}, conn, nil
	}
	return pg.Where{}, pg.Conn{}, fmt.Errorf("instance %s: invalid connection", in.Name)
}
