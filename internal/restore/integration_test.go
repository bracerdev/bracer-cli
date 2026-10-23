//go:build integration

package restore

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/pg"
)

// Needs Docker and the bracer-test-pg container (see pg/integration_test.go).
// A database with an owner, settings, a comment and privileges is dumped with
// pg_dump -Fc, then loaded into a new database the plan names: the attributes
// must come back (spec restore § Load); data mode leaves the owner and
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
	var created []string
	t.Cleanup(func() {
		ctx := context.Background()
		for _, db := range append([]string{src}, created...) {
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

	var dump bytes.Buffer
	if _, err := sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"exec", "bracer-test-pg", "pg_dump", "-Fc", "-U", "postgres", src}, Stdout: &dump}); err != nil {
		t.Fatal(err)
	}
	globals, err := sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"exec", "bracer-test-pg", "pg_dumpall", "--globals-only", "-U", "postgres"}})
	if err != nil {
		t.Fatal(err)
	}

	st := newMemStore()
	st.Put(ctx, "dump", bytes.NewReader(dump.Bytes()), int64(dump.Len()))
	dp := &backup.Dump{Key: "dump", SHA256: sha(dump.Bytes()), Bytes: int64(dump.Len()),
		Tables: []pg.Table{{Schema: "public", Name: "t", Rows: i64(10)}}}
	runner := pg.ExecRunner{Shell: sh, Container: "bracer-test-pg"}
	conn := pg.Conn{User: "postgres", Database: "postgres"}
	admin := &pg.Client{Runner: runner, Conn: conn}
	spec := TargetSpec{Kind: "container", Container: "bracer-test-pg", Shell: sh}

	info, err := ReadArchive(ctx, runner, conn, func() (io.ReadCloser, error) { return OpenDump(ctx, st, dp, nil) })
	if err != nil {
		t.Fatal(err)
	}
	env, _, err := pg.CollectEnv(ctx, admin, pg.EnvSource{Kind: "container", Container: "bracer-test-pg", Shell: sh}, []string{src})
	if err != nil {
		t.Fatal(err)
	}
	g, err := ParseGlobals(globals.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	run := &SourceRun{Instance: "main", TS: time.Now().UTC().Format(tsLayout), Kind: "container", Env: env, Globals: g,
		DBs: []*SourceDB{{Name: src, Dump: dp, AttrSQL: info.AttrSQL, Entries: info.Entries}}}

	// load plans the run on the server as it is now, loads it, and checks
	// the loaded database as restore does after the load.
	load := func(mode string) (string, *Plan) {
		t.Helper()
		tgt, err := ReadTarget(ctx, admin, spec, mode == ModeFull, nil, run.Wants())
		if err != nil {
			t.Fatal(err)
		}
		plan := MakePlan(PlanInput{Src: run, Tgt: tgt, Mode: mode, Now: time.Now()})
		into := plan.DBs[0]
		if !strings.HasPrefix(into.Target, src+"_restored_") {
			t.Fatalf("%s: target %q", mode, into.Target)
		}
		created = append(created, into.Target)
		if err := LoadNew(ctx, st, dp, into, nil, runner, conn, mode, tgt.Major, time.Minute); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		after, err := ReadTarget(ctx, admin, spec, mode == ModeFull, []string{into.Target}, run.Wants())
		if err != nil {
			t.Fatal(err)
		}
		return into.Target, MakePlan(PlanInput{Src: run, Tgt: after, Mode: mode, Loaded: map[string]string{src: into.Target}, Now: time.Now()})
	}
	attrsOK := func(p *Plan, db string) bool {
		for _, c := range p.Checks {
			if c.Area == "database" && c.Name == db+" attributes" {
				return c.OK
			}
		}
		return false
	}

	full, check := load(ModeFull)
	if !attrsOK(check, full) {
		t.Errorf("full: attributes differ: %+v", check.Failed())
	}
	if got := psql("postgres", "select pg_get_userbyid(datdba) from pg_database where datname = '"+full+"'"); got != role {
		t.Errorf("full: owner = %q", got)
	}
	if got := psql("postgres", "select has_database_privilege('public', '"+full+"', 'connect')"); got != "f" {
		t.Errorf("full: PUBLIC CONNECT was revoked: %s", got)
	}
	if got := psql("postgres", "select shobj_description(oid, 'pg_database') from pg_database where datname = '"+full+"'"); got != "attributes test" {
		t.Errorf("full: comment = %q", got)
	}
	if got := psql(full, "select count(*) from t"); got != "10" {
		t.Errorf("full: rows = %s", got)
	}

	// The same run again: the name is taken now, the next one is used.
	data, _ := load(ModeData)
	if data != full+"_2" {
		t.Errorf("data: target %q, want %s_2", data, full)
	}
	if got := psql("postgres", "select pg_get_userbyid(datdba) from pg_database where datname = '"+data+"'"); got != "postgres" {
		t.Errorf("data: owner = %q", got)
	}
	if got := psql("postgres", "select has_database_privilege('public', '"+data+"', 'connect')"); got != "t" {
		t.Errorf("data: PUBLIC keeps the default CONNECT: %s", got)
	}
	if got := psql("postgres", "select array_to_string(s.setconfig, ',') ilike '%timezone=Asia/Ho_Chi_Minh%' from pg_db_role_setting s join pg_database d on d.oid = s.setdatabase where d.datname = '"+data+"' and s.setrole = 0"); got != "t" {
		t.Errorf("data: the database setting is applied: %s", got)
	}
}
