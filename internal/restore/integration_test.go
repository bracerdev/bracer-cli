//go:build integration

package restore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
)

// Needs Docker and the bracer-test-pg container (see pg/integration_test.go).
// A database with an owner, settings, a comment and privileges is dumped with
// pg_dump -Fc, then restored from the file under another name: the attributes
// must come back (spec fetch-restore § Flow 8); --preview leaves the owner and
// privileges to the restoring user.
func TestIntegrationRestoreDatabaseAttributes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	sh := pg.ExecShell{}
	psql := func(db, sql string) string {
		t.Helper()
		res, err := sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"exec", "bracer-test-pg", "psql", "-X", "-A", "-t", "-q", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", db, "-c", sql}})
		if err != nil {
			t.Fatalf("psql %s: %v", sql, err)
		}
		return strings.TrimSpace(string(res.Stdout))
	}
	if _, err := sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"exec", "bracer-test-pg", "pg_isready", "-U", "postgres"}}); err != nil {
		t.Skip("bracer-test-pg not running:", err)
	}

	suffix := time.Now().UTC().Format("150405")
	src, role := "attr_"+suffix, "attr_app_"+suffix
	var restored, bare string
	t.Cleanup(func() {
		ctx := context.Background()
		for _, db := range []string{src, restored, bare} {
			if db == "" {
				continue
			}
			sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"exec", "bracer-test-pg", "psql", "-X", "-U", "postgres", "-c", "drop database if exists " + pg.QuoteIdent(db)}})
		}
		sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"exec", "bracer-test-pg", "psql", "-X", "-U", "postgres", "-c", "drop role if exists " + pg.QuoteIdent(role)}})
	})
	for _, sql := range []string{
		"create role " + role + " login",
		"create database " + src + " owner " + role,
		"alter database " + src + " set timezone to 'Asia/Ho_Chi_Minh'",
		"alter database " + src + " set app.settings.flag to 'on'",
		"alter role " + role + " in database " + src + " set search_path to app, public",
		"revoke connect, temporary on database " + src + " from public",
		"grant connect on database " + src + " to " + role,
		"comment on database " + src + " is 'attributes test'",
	} {
		psql("postgres", sql)
	}
	psql(src, "create table t (id int); insert into t select generate_series(1, 10)")

	dump := filepath.Join(t.TempDir(), src+".dump")
	f, err := os.Create(dump)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"exec", "bracer-test-pg", "pg_dump", "-Fc", "-U", "postgres", src}, Stdout: f}); err != nil {
		t.Fatal(err)
	}
	f.Close()

	cfg := &config.App{App: "shop", Encryption: "none",
		Instances: []config.Instance{{Name: "main", Container: "bracer-test-pg", User: "postgres", PgMajor: 17, Databases: config.Databases{All: true}}}}
	d := Deps{Shell: sh, Secrets: config.NewSecrets(nil, "test")}

	// ADR-0015: a file has no environment record, so it is only previewed, with or without --preview.
	res, err := Run(ctx, cfg, Options{File: dump, IntoContainer: "bracer-test-pg"}, d)
	if err != nil || res.Mode != ModePreview {
		t.Fatalf("--file: %v %+v", err, res)
	}
	restored = res.Databases[0].TargetDB
	res, err = Run(ctx, cfg, Options{File: dump, IntoContainer: "bracer-test-pg", Preview: true}, d)
	if err != nil {
		t.Fatal(err)
	}
	db := res.Databases[0]
	bare = db.TargetDB
	if !res.OK || !strings.HasPrefix(bare, src+"_preview_") || db.Attributes == nil || db.Attributes.Owner != "" || len(db.Attributes.Mismatches) != 0 {
		t.Fatalf("--preview: %+v\ndb: %+v\nwarnings: %q", res, db, res.Warnings)
	}
	if got := psql("postgres", "select pg_get_userbyid(datdba) from pg_database where datname = '"+bare+"'"); got != "postgres" {
		t.Errorf("--preview: owner = %q", got)
	}
	if got := psql("postgres", "select has_database_privilege('public', '"+bare+"', 'connect')"); got != "t" {
		t.Errorf("--preview: PUBLIC keeps the default CONNECT: %s", got)
	}
}
