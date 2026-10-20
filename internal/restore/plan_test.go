package restore

import (
	"strings"
	"testing"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/pg"
)

const superGlobals = "CREATE ROLE postgres;\nALTER ROLE postgres WITH SUPERUSER INHERIT CREATEROLE CREATEDB LOGIN REPLICATION BYPASSRLS;\n"

func mustGlobals(t *testing.T, sql string) *RoleGlobals {
	t.Helper()
	g, err := ParseGlobals([]byte(sql))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// planSource is a full backup run: users postgres and "Report Reader", database
// shop owned by app with pg_stat_statements, DateStyle ISO, DMY.
func planSource(t *testing.T) *SourceRun {
	pre := "pg_stat_statements"
	env := &pg.Env{SourceKind: "container", ServerVersionNum: 170006, ServerVersion: "17.6", BootstrapSuperuser: "postgres",
		PreloadLibraries: &pre, HasFileSettings: true,
		Settings: []pg.Setting{{Name: "DateStyle", Value: "ISO, DMY", Vartype: "string"}}}
	g := mustGlobals(t, superGlobals+
		"CREATE ROLE app;\nALTER ROLE app WITH NOSUPERUSER INHERIT NOCREATEROLE NOCREATEDB LOGIN NOREPLICATION NOBYPASSRLS PASSWORD 'SCRAM-SHA-256$4096:x';\n"+
		"CREATE ROLE \"Report Reader\";\nALTER ROLE \"Report Reader\" WITH NOSUPERUSER INHERIT NOCREATEROLE NOCREATEDB LOGIN NOREPLICATION NOBYPASSRLS;\n")
	shop := &SourceDB{Name: "shop",
		Dump: &backup.Dump{Locale: &pg.Locale{Encoding: "UTF8", Collate: "en_US.utf8", Ctype: "en_US.utf8"},
			Extensions: []pg.Extension{{Name: "pg_stat_statements", Version: "1.11", Schema: "public"}}},
		AttrSQL: []byte("CREATE DATABASE shop WITH TEMPLATE = template0 ENCODING = 'UTF8';\nALTER DATABASE shop OWNER TO app;\nGRANT CONNECT ON DATABASE shop TO \"Report Reader\";\n")}
	return &SourceRun{Instance: "main", TS: "2026-10-16T030000Z", Kind: "container", Env: env, Globals: g, DBs: []*SourceDB{shop}}
}

// freshTarget is a new postgres:17 container: no database shop, only postgres, nothing preloaded.
func freshTarget(t *testing.T) *TargetState {
	empty := ""
	env := &pg.Env{SourceKind: "container", ServerVersionNum: 170011, ServerVersion: "17.11", CurrentUser: "postgres", Superuser: true,
		PreloadLibraries: &empty, HasFileSettings: true,
		Settings: []pg.Setting{{Name: "DateStyle", Value: "ISO, MDY", Vartype: "string", Source: "configuration file", Context: "user"}}}
	g := mustGlobals(t, superGlobals)
	return &TargetState{Kind: "container", Container: "db", Env: env, Major: 17,
		Roles:    &TargetRoles{Roles: g.Roles, ParamGrants: map[string]ParamGrant{}, HasGrantOptions: true},
		Avail:    map[string]ExtAvail{"pg_stat_statements": {Default: "1.11", Trusted: false}},
		PkgLib:   map[string]bool{"pg_stat_statements.so": true},
		AutoConf: map[string]string{},
		DBs:      map[string]*TargetDB{"shop": {Name: "shop"}},
		Names:    map[string]bool{"postgres": true, "template0": true, "template1": true},
		LocaleOK: map[string]string{"UTF8\x00en_US.utf8\x00en_US.utf8\x00\x00": ""}}
}

func failed(p *Plan) map[string]Check {
	out := map[string]Check{}
	for _, c := range p.Failed() {
		out[c.Area+" "+c.Name] = c
	}
	return out
}

func has(stmts []string, s string) bool {
	for _, x := range stmts {
		if x == s {
			return true
		}
	}
	return false
}

func index(stmts []string, prefix string) int {
	for i, x := range stmts {
		if strings.HasPrefix(x, prefix) {
			return i
		}
	}
	return -1
}

// ADR-0015 § 3: a fresh target gets every gap as SQL, with its undo, and nothing is loaded.
func TestPlanFreshTarget(t *testing.T) {
	src, tgt := planSource(t), freshTarget(t)
	tgt.LocaleOK = map[string]string{localeKey(src.DBs[0].Dump.Locale): ""}
	p := MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull})
	f := failed(p)
	for _, k := range []string{"database shop", "preload pg_stat_statements", "setting DateStyle", "role app", "role Report Reader"} {
		if c, ok := f[k]; !ok || c.Fix != FixSQL {
			t.Errorf("%s: %+v (all: %v)", k, c, f)
		}
	}
	if p.OK() || !p.Restart || p.RestartCmd != "docker restart db" {
		t.Errorf("ok %v restart %v %q", p.OK(), p.Restart, p.RestartCmd)
	}
	for _, s := range []string{
		`CREATE ROLE "app" WITH NOSUPERUSER INHERIT NOCREATEROLE NOCREATEDB LOGIN NOREPLICATION NOBYPASSRLS PASSWORD 'SCRAM-SHA-256$4096:x';`,
		`CREATE DATABASE "shop" TEMPLATE template0 encoding 'UTF8' lc_collate 'en_US.utf8' lc_ctype 'en_US.utf8' OWNER "app";`,
		`GRANT CONNECT ON DATABASE "shop" TO "Report Reader";`,
		`ALTER SYSTEM SET shared_preload_libraries = 'pg_stat_statements';`,
		`ALTER SYSTEM SET DateStyle = 'ISO, DMY';`,
	} {
		if !has(p.Up, s) {
			t.Errorf("env-up lacks %s:\n%s", s, strings.Join(p.Up, "\n"))
		}
	}
	// Roles before the databases they own; a restart replaces the reload.
	if index(p.Up, `CREATE ROLE "app"`) > index(p.Up, `CREATE DATABASE "shop"`) || has(p.Up, "SELECT pg_reload_conf();") {
		t.Errorf("env-up order:\n%s", strings.Join(p.Up, "\n"))
	}
	// env-down undoes in reverse: settings first, then the database, then the users.
	for _, s := range []string{`ALTER SYSTEM RESET shared_preload_libraries;`, `DROP DATABASE IF EXISTS "shop" WITH (FORCE);`, `DROP ROLE IF EXISTS "app";`} {
		if !has(p.Down, s) {
			t.Errorf("env-down lacks %s:\n%s", s, strings.Join(p.Down, "\n"))
		}
	}
	if index(p.Down, "ALTER SYSTEM RESET") > index(p.Down, "DROP DATABASE") || index(p.Down, "DROP DATABASE") > index(p.Down, "DROP ROLE") {
		t.Errorf("env-down order:\n%s", strings.Join(p.Down, "\n"))
	}
}

// After env-up the same comparison has nothing left; after the load (Loaded) too.
func TestPlanMatchingTarget(t *testing.T) {
	src, tgt := planSource(t), freshTarget(t)
	tgt.LocaleOK = map[string]string{localeKey(src.DBs[0].Dump.Locale): ""}
	pre := "pg_stat_statements"
	tgt.Env.PreloadLibraries = &pre
	tgt.Env.Settings[0].Value = "ISO, DMY"
	tgt.Roles = &TargetRoles{Roles: src.Globals.Roles, ParamGrants: map[string]ParamGrant{}, HasGrantOptions: true}
	tgt.DBs["shop"] = &TargetDB{Name: "shop", Exists: true, Owner: "app", Locale: src.DBs[0].Dump.Locale,
		Settings: map[string]string{}, SettingName: map[string]string{},
		ACL: map[string]bool{"PUBLIC CONNECT": true, "PUBLIC TEMPORARY": true, "app CONNECT": true, "app CREATE": true, "app TEMPORARY": true, "Report Reader CONNECT": true}}
	p := MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull})
	if !p.OK() || len(p.Up) != 0 {
		t.Fatalf("failed %v, up:\n%s", failed(p), strings.Join(p.Up, "\n"))
	}
	if p.DBs[0].Target != "shop" || p.DBs[0].State != StateEmpty {
		t.Errorf("db plan: %+v", p.DBs[0])
	}
	// The load added tables: Loaded says so, and the plan does not move it to a side database.
	tgt.DBs["shop"].Objects = 5
	p = MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull, Loaded: map[string]string{"shop": "shop"}})
	if !p.OK() || p.DBs[0].Target != "shop" {
		t.Errorf("after the load: %v %+v", failed(p), p.DBs[0])
	}
}

// A database with data is never touched: the run goes next to it.
func TestPlanHasDataGoesNextToIt(t *testing.T) {
	src, tgt := planSource(t), freshTarget(t)
	tgt.DBs["shop"] = &TargetDB{Name: "shop", Exists: true, Objects: 3, Owner: "app", Locale: src.DBs[0].Dump.Locale}
	tgt.Names["shop"] = true
	p := MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull, Now: time.Now()})
	if dp := p.DBs[0]; dp.State != StateHasData || dp.Target != "shop_restored_20261016_0300" {
		t.Errorf("db plan: %+v", dp)
	}
	if index(p.Up, `CREATE DATABASE "shop_restored_20261016_0300"`) < 0 || index(p.Up, `DROP DATABASE`) >= 0 {
		t.Errorf("env-up:\n%s", strings.Join(p.Up, "\n"))
	}
}

// What SQL cannot fix is a manual check with the change to make.
func TestPlanManualChecks(t *testing.T) {
	src, tgt := planSource(t), freshTarget(t)
	tgt.Avail = map[string]ExtAvail{}
	tgt.Env.ServerVersionNum, tgt.Major = 160004, 16
	tgt.LocaleOK = map[string]string{localeKey(src.DBs[0].Dump.Locale): `invalid locale name "en_US.utf8"`}
	f := failed(MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull}))
	for _, k := range []string{"version PostgreSQL 17", "extension shop: pg_stat_statements 1.11", "locale UTF8, en_US.utf8"} {
		if c, ok := f[k]; !ok || c.Fix != FixManual || !strings.Contains(c.Manual, "container db") {
			t.Errorf("%s: %+v", k, c)
		}
	}
	// A setting on the container's command line cannot be changed by ALTER SYSTEM.
	tgt = freshTarget(t)
	tgt.Env.Container = &pg.ContainerEnv{Cmd: []string{"postgres", "-c", "datestyle=ISO, MDY"}}
	if c := failed(MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull}))["setting DateStyle"]; c.Fix != FixManual || !strings.Contains(c.Manual, "-c DateStyle") {
		t.Errorf("command line setting: %+v", c)
	}
}

// An empty database with another locale is dropped and created again; env-down brings it back.
func TestPlanRecreatesEmptyDatabaseWithOtherLocale(t *testing.T) {
	src, tgt := planSource(t), freshTarget(t)
	tgt.DBs["shop"] = &TargetDB{Name: "shop", Exists: true, Owner: "postgres", ACLNull: true,
		Locale: &pg.Locale{Encoding: "UTF8", Collate: "C", Ctype: "C"}, Settings: map[string]string{}, SettingName: map[string]string{}, ACL: map[string]bool{}}
	p := MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull})
	if !p.DBs[0].Recreate || index(p.Up, `DROP DATABASE "shop"`) < 0 {
		t.Fatalf("recreate: %+v\n%s", p.DBs[0], strings.Join(p.Up, "\n"))
	}
	if index(p.Down, `CREATE DATABASE "shop" TEMPLATE template0 encoding 'UTF8' lc_collate 'C' lc_ctype 'C' OWNER "postgres";`) < 0 {
		t.Errorf("env-down:\n%s", strings.Join(p.Down, "\n"))
	}
	// env-down drops the restored database once, then builds the old one.
	if n := strings.Count(strings.Join(p.Down, "\n"), `DROP DATABASE IF EXISTS "shop"`); n != 1 {
		t.Errorf("env-down drops shop %d times:\n%s", n, strings.Join(p.Down, "\n"))
	}
}

// A URL run is data only: the database is created owned by the connecting
// user, and its settings are compared once it exists.
func TestPlanDataModeDefersSettings(t *testing.T) {
	src, tgt := planSource(t), freshTarget(t)
	src.Kind, src.Env.SourceKind, src.Globals = "url", "url", nil
	src.Env.DBSettings = map[string][]pg.Setting{"shop": {{Name: "TimeZone", Value: "GMT", Vartype: "string"}}}
	tgt.Roles = nil
	tgt.Env.PreloadLibraries = src.Env.PreloadLibraries
	p := MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeData})
	f := failed(p)
	if c := f["setting shop"]; c.Fix != FixRerun {
		t.Errorf("settings: %+v (%v)", c, f)
	}
	if _, ok := f["role app"]; ok || index(p.Up, `CREATE DATABASE "shop" TEMPLATE template0 encoding 'UTF8' lc_collate 'en_US.utf8' lc_ctype 'en_US.utf8' OWNER "postgres";`) < 0 {
		t.Errorf("data mode creates no user and owns the database: %v\n%s", f, strings.Join(p.Up, "\n"))
	}
}

// For UTF8, collate C.UTF-8 is byte order like C (musl stores it as C).
func TestSameLocaleCUTF8(t *testing.T) {
	a := &pg.Locale{Encoding: "UTF8", Collate: "C.UTF-8", Ctype: "C.UTF-8", Provider: "builtin", Locale: "C.UTF-8"}
	b := &pg.Locale{Encoding: "UTF8", Collate: "C", Ctype: "C.UTF-8", Provider: "builtin", Locale: "C.UTF-8"}
	if !sameLocale(a, b) {
		t.Error("C.UTF-8 collate must equal C for UTF8")
	}
	if c := (&pg.Locale{Encoding: "UTF8", Collate: "C.UTF-8", Ctype: "C"}); sameLocale(a, c) {
		t.Error("ctype C.UTF-8 differs from C")
	}
}

// Grantee names may hold spaces.
func TestACLDiffQuotesGrantees(t *testing.T) {
	got := aclDiff("shop", map[string]bool{"PUBLIC TEMPORARY": true}, map[string]bool{"Report Reader CONNECT": true})
	want := []string{`REVOKE TEMPORARY ON DATABASE "shop" FROM PUBLIC;`, `GRANT CONNECT ON DATABASE "shop" TO "Report Reader";`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got %q", got)
	}
}

// env-down resets the setting before reloading.
func TestPlanReloadAfterReset(t *testing.T) {
	src, tgt := planSource(t), freshTarget(t)
	pre := "pg_stat_statements"
	tgt.Env.PreloadLibraries = &pre
	p := MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull})
	if p.Restart || p.Up[len(p.Up)-1] != "SELECT pg_reload_conf();" || p.Down[len(p.Down)-1] != "SELECT pg_reload_conf();" || index(p.Down, "ALTER SYSTEM RESET DateStyle") < 0 {
		t.Errorf("up:\n%s\ndown:\n%s", strings.Join(p.Up, "\n"), strings.Join(p.Down, "\n"))
	}
}

// Role comments come back: a new user gets its comment, an existing one with
// another comment is changed and env-down puts the old one back.
func TestPlanRoleComments(t *testing.T) {
	src, tgt := planSource(t), freshTarget(t)
	src.Globals = mustGlobals(t, superGlobals+"CREATE ROLE app;\nALTER ROLE app WITH LOGIN;\nCOMMENT ON ROLE app IS 'owns ''shop''';\nCOMMENT ON ROLE postgres IS 'admin';\n")
	old := "old"
	tgt.Roles.Roles["postgres"].Comment = &old
	p := MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull})
	for _, s := range []string{`COMMENT ON ROLE "app" IS 'owns ''shop''';`, `COMMENT ON ROLE "postgres" IS 'admin';`} {
		if !has(p.Up, s) {
			t.Errorf("env-up lacks %s:\n%s", s, strings.Join(p.Up, "\n"))
		}
	}
	if !has(p.Down, `COMMENT ON ROLE "postgres" IS 'old';`) {
		t.Errorf("env-down:\n%s", strings.Join(p.Down, "\n"))
	}
	if index(p.Up, `CREATE ROLE "app"`) > index(p.Up, `COMMENT ON ROLE "app"`) {
		t.Errorf("comment before the role exists:\n%s", strings.Join(p.Up, "\n"))
	}
}

// transaction_isolation mirrors default_transaction_isolation in a session:
// a backup that recorded it must not make env-up ALTER SYSTEM it.
func TestPlanSkipsSessionOnlySettings(t *testing.T) {
	src, tgt := planSource(t), freshTarget(t)
	src.Env.Settings = append(src.Env.Settings, pg.Setting{Name: "transaction_isolation", Value: "repeatable read", Vartype: "enum"})
	p := MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull})
	if _, ok := failed(p)["setting transaction_isolation"]; ok || strings.Contains(strings.Join(p.Up, "\n"), "transaction_isolation") {
		t.Errorf("session-only setting compared:\n%s", strings.Join(p.Up, "\n"))
	}
}

// lc_messages ” (Bitnami) cannot be ALTER SYSTEM SET and changes no query
// result: a warning, no SQL.
func TestPlanLcMessagesWarnsOnly(t *testing.T) {
	src, tgt := planSource(t), freshTarget(t)
	src.Env.Settings = append(src.Env.Settings, pg.Setting{Name: "lc_messages", Value: "", Vartype: "string"})
	tgt.Env.Settings = append(tgt.Env.Settings, pg.Setting{Name: "lc_messages", Value: "en_US.utf8", Vartype: "string", Source: "configuration file", Context: "superuser"})
	p := MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull})
	if _, ok := failed(p)["setting lc_messages"]; ok || strings.Contains(strings.Join(p.Up, "\n"), "lc_messages") {
		t.Errorf("lc_messages blocks or has SQL:\n%s", strings.Join(p.Up, "\n"))
	}
}

// A loaded module's setting follows the backup (pg_cron's cron.database_name);
// a module the target has not loaded (neon) is not compared.
func TestPlanModuleSettings(t *testing.T) {
	src, tgt := planSource(t), freshTarget(t)
	src.Env.Modules = []pg.Setting{{Name: "cron.database_name", Value: "shop", Vartype: "string"}, {Name: "neon.timeline_id", Value: "x", Vartype: "string"}}
	tgt.Env.Modules = []pg.Setting{{Name: "cron.database_name", Value: "postgres", Vartype: "string", Source: "default", Context: "postmaster"}}
	p := MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull})
	if c, ok := failed(p)["setting cron.database_name"]; !ok || c.Fix != FixSQL || !p.Restart {
		t.Errorf("cron.database_name: %+v restart %v", c, p.Restart)
	}
	up := strings.Join(p.Up, "\n")
	if !strings.Contains(up, "ALTER SYSTEM SET cron.database_name = 'shop';") || strings.Contains(up, "neon") {
		t.Errorf("env-up:\n%s", up)
	}
	if !has(p.Down, "ALTER SYSTEM RESET cron.database_name;") {
		t.Errorf("env-down:\n%s", strings.Join(p.Down, "\n"))
	}
}

// A local target without an extension is told every package the source
// cluster had for it: postgis ships its scripts and its library apart.
func TestPlanExtensionHintNamesEveryPackage(t *testing.T) {
	src, tgt := planSource(t), freshTarget(t)
	src.Env.Local = &pg.LocalEnv{Packages: []pg.Package{
		{Extension: "postgis", Package: "postgresql-17-postgis-3-scripts"},
		{Extension: "postgis", Package: "postgresql-17-postgis-3"},
		{Extension: "vector", Package: "postgresql-17-pgvector"}}}
	src.DBs[0].Dump.Extensions = append(src.DBs[0].Dump.Extensions, pg.Extension{Name: "postgis", Version: "3.6.4", Schema: "public"})
	tgt.Kind = "local"
	tgt.Env.ServerVersionNum, tgt.Major = 180001, 18
	var hint string
	for k, c := range failed(MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull})) {
		if strings.HasPrefix(k, "extension ") && strings.Contains(k, "postgis") {
			hint = c.Manual
		}
	}
	if want := "sudo apt install postgresql-18-postgis-3-scripts postgresql-18-postgis-3, then run restore again"; hint != want {
		t.Errorf("hint %q, want %q", hint, want)
	}
}
