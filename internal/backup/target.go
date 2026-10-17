// Package backup is the pipeline dump → [age] → bucket → manifest → retention
// (spec backup/backup). The CLI only parses flags and prints the result.
package backup

import (
	"context"
	"fmt"
	"os"
	"strings"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
)

// ConnectFunc is the signature of Connect, so tests can replace it with a fake.
type ConnectFunc func(ctx context.Context, tools *pg.Tools, in config.Instance, s *config.Secrets) (pg.Conn, pg.Runner, error)

// Connect builds Conn + Runner for one instance in app.yaml per the ADR-0009
// table: container → docker exec; pod → kubectl exec; socket → host binary;
// host/URL → a recent enough host binary, then a Docker image. pg_major in
// app.yaml is the major used to check the host binary; if absent, ask the server.
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
		if err := password(in, s, &conn); err != nil {
			return pg.Where{}, conn, err
		}
		return pg.Where{Container: in.Container}, conn, nil
	case config.KindPod:
		ref, err := pg.ParsePodRef(in.Pod)
		if err != nil {
			return pg.Where{}, conn, fmt.Errorf("instance %s: %w", in.Name, err)
		}
		if in.PasswordEnv != "" || in.PasswordFile != "" {
			// kubectl exec cannot pass env outside argv (ADR-0009): say so plainly instead of silently ignoring it.
			return pg.Where{}, conn, fmt.Errorf("instance %s: a password cannot be passed to a pod (kubectl exec has no way to set it without exposing it); remove password_env/password_file and allow the socket connection inside the pod, or use a postgres:// URL", in.Name)
		}
		return pg.Where{Pod: ref}, conn, nil
	case config.KindSocket:
		dir, port := in.SocketDir()
		conn.Host, conn.Port = dir, port
		if err := password(in, s, &conn); err != nil {
			return pg.Where{}, conn, err
		}
		return pg.Where{SocketDir: dir}, conn, nil
	case config.KindHost:
		conn.Host, conn.Port = in.Host, in.Port
		if err := password(in, s, &conn); err != nil {
			return pg.Where{}, conn, err
		}
		return pg.Where{}, conn, nil
	case config.KindURL:
		v, err := s.Require(in.URLEnv)
		if err != nil {
			return pg.Where{}, pg.Conn{}, err
		}
		conn, err := pg.ParseURL(v)
		if err != nil {
			return pg.Where{}, conn, err
		}
		return pg.Where{}, conn, nil
	}
	return pg.Where{}, pg.Conn{}, fmt.Errorf("instance %s: invalid connection", in.Name)
}

func password(in config.Instance, s *config.Secrets, conn *pg.Conn) error {
	switch {
	case in.PasswordEnv != "":
		v, err := s.Require(in.PasswordEnv)
		if err != nil {
			return err
		}
		conn.Password = v
	case in.PasswordFile != "":
		b, err := os.ReadFile(in.PasswordFile)
		if err != nil {
			return err
		}
		conn.Password = strings.TrimRight(string(b), "\r\n")
	}
	return nil
}
