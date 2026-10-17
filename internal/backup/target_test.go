package backup

import (
	"testing"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
)

func TestTargetBuildsWhereAndConn(t *testing.T) {
	s := config.NewSecrets(map[string]string{"PW": "s3cret", "URL": "postgres://u:p@db.example.com:5433/app?sslmode=require"}, "test")

	w, c, err := Target(config.Instance{Name: "a", Container: "shop-db", User: "postgres"}, s)
	if err != nil || w.Container != "shop-db" || c.Host != "" || c.User != "postgres" || c.Database != "postgres" {
		t.Errorf("container: %+v %+v %v", w, c, err)
	}
	w, c, err = Target(config.Instance{Name: "b", Pod: "ns/pg-0/db", User: "attendy"}, s)
	if err != nil || w.Pod.String() != "ns/pg-0/db" || c.Host != "" || c.Password != "" {
		t.Errorf("pod: %+v %+v %v", w, c, err)
	}
	w, c, err = Target(config.Instance{Name: "c", Socket: "/var/run/postgresql/.s.PGSQL.5433", User: "postgres", PasswordEnv: "PW"}, s)
	if err != nil || w.SocketDir != "/var/run/postgresql" || c.Host != "/var/run/postgresql" || c.Port != 5433 || c.Password != "s3cret" {
		t.Errorf("socket: %+v %+v %v", w, c, err)
	}
	w, c, err = Target(config.Instance{Name: "d", Socket: "/tmp", User: "postgres"}, s)
	if err != nil || w.SocketDir != "/tmp" || c.Port != 5432 {
		t.Errorf("default socket 5432: %+v %+v %v", w, c, err)
	}
	w, c, err = Target(config.Instance{Name: "e", Host: "localhost", Port: 5433, User: "u", PasswordEnv: "PW"}, s)
	if err != nil || w != (pg.Where{}) || c.Host != "localhost" || c.Password != "s3cret" {
		t.Errorf("host: %+v %+v %v", w, c, err)
	}
	w, c, err = Target(config.Instance{Name: "f", URLEnv: "URL"}, s)
	if err != nil || w != (pg.Where{}) || c.Host != "db.example.com" || c.Port != 5433 || c.Params["sslmode"] != "require" {
		t.Errorf("url: %+v %+v %v", w, c, err)
	}
	if _, _, err = Target(config.Instance{Name: "g", URLEnv: "MISSING"}, s); err == nil {
		t.Error("missing secret must be an error")
	}
}
