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
	w, c, err = Target(config.Instance{Name: "b", Container: "bitnami-db", User: "app", PasswordEnv: "PW"}, s)
	if err != nil || w.Container != "bitnami-db" || c.Password != "s3cret" {
		t.Errorf("container with password: %+v %+v %v", w, c, err)
	}
	w, c, err = Target(config.Instance{Name: "f", URLEnv: "URL"}, s)
	if err != nil || w != (pg.Where{}) || c.Host != "db.example.com" || c.Port != 5433 || c.Params["sslmode"] != "require" {
		t.Errorf("url: %+v %+v %v", w, c, err)
	}
	if _, _, err = Target(config.Instance{Name: "g", URLEnv: "MISSING"}, s); err == nil {
		t.Error("missing secret must be an error")
	}
}
