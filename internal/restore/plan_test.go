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

// freshTarget is a new postgres:17 container: only postgres, nothing preloaded.
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
		DBs:      map[string]*TargetDB{},
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

// restoredShop is where the run of planSource loads shop: the run's ts, UTC.
const restoredShop = "shop_restored_20261016_0300"

// ADR-0015 § 3: a fresh target gets every server-wide gap as SQL, with its
// undo; the new database is the load's, never env-up's (ADR-0022).
func TestPlanFreshTarget(t *testing.T) {
	src, tgt := planSource(t), freshTarget(t)
	tgt.LocaleOK = map[string]string{localeKey(src.DBs[0].Dump.Locale): ""}
	p := MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull})
	f := failed(p)
	for _, k := range []string{"preload pg_stat_statements", "setting DateStyle", "role app", "role Report Reader"} {
		if c, ok := f[k]; !ok || c.Fix != FixSQL {
			t.Errorf("%s: %+v (all: %v)", k, c, f)
		}
	}
	if _, ok := f["database shop"]; ok {
		t.Errorf("a new database is not a gap: %v", f)
	}
	if p.OK() || !p.Restart || p.RestartCmd != "docker restart db" {
		t.Errorf("ok %v restart %v %q", p.OK(), p.Restart, p.RestartCmd)
	}
	for _, s := range []string{
		`CREATE ROLE "app" WITH NOSUPERUSER INHERIT NOCREATEROLE NOCREATEDB LOGIN NOREPLICATION NOBYPASSRLS PASSWORD 'SCRAM-SHA-256$4096:x';`,
		`ALTER SYSTEM SET shared_preload_libraries = 'pg_stat_statements';`,
		`ALTER SYSTEM SET DateStyle = 'ISO, DMY';`,
	} {
		if !has(p.Up, s) {
			t.Errorf("env-up lacks %s:\n%s", s, strings.Join(p.Up, "\n"))
		}
	}
	// env-up is server-wide only; a restart replaces the reload.
	if strings.Contains(strings.Join(p.Up, "\n"), " DATABASE ") || has(p.Up, "SELECT pg_reload_conf();") {
		t.Errorf("env-up:\n%s", strings.Join(p.Up, "\n"))
	}
	// The load creates the database with the source's locale, owner and privileges.
	dp := p.DBs[0]
	want := []string{
		`CREATE DATABASE "` + restoredShop + `" TEMPLATE template0 encoding 'UTF8' lc_collate 'en_US.utf8' lc_ctype 'en_US.utf8' OWNER "app";`,
		`ALTER DATABASE "` + restoredShop + `" OWNER TO app;`,
		`GRANT CONNECT ON DATABASE "` + restoredShop + `" TO "Report Reader";`,
	}
	if dp.Source != "shop" || dp.Target != restoredShop || strings.Join(dp.Create, "\n") != strings.Join(want, "\n") {
		t.Errorf("db plan: %+v\n%s", dp, strings.Join(dp.Create, "\n"))
	}
	// env-down undoes in reverse: settings first, then the users; the new
	// database is dropped by the customer before it.
	for _, s := range []string{`ALTER SYSTEM RESET shared_preload_libraries;`, `DROP ROLE IF EXISTS "app";`} {
		if !has(p.Down, s) {
			t.Errorf("env-down lacks %s:\n%s", s, strings.Join(p.Down, "\n"))
		}
	}
	if index(p.Down, "ALTER SYSTEM RESET") > index(p.Down, "DROP ROLE") || index(p.Down, "DROP DATABASE") >= 0 {
		t.Errorf("env-down:\n%s", strings.Join(p.Down, "\n"))
	}
}

// After env-up the same comparison has nothing left; after the load (Loaded)
// too, checked in the database the run was loaded into.
func TestPlanMatchingTarget(t *testing.T) {
	src, tgt := planSource(t), freshTarget(t)
	tgt.LocaleOK = map[string]string{localeKey(src.DBs[0].Dump.Locale): ""}
	pre := "pg_stat_statements"
	tgt.Env.PreloadLibraries = &pre
	tgt.Env.Settings[0].Value = "ISO, DMY"
	tgt.Roles = &TargetRoles{Roles: src.Globals.Roles, ParamGrants: map[string]ParamGrant{}, HasGrantOptions: true}
	p := MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull})
	if !p.OK() || len(p.Up) != 0 {
		t.Fatalf("failed %v, up:\n%s", failed(p), strings.Join(p.Up, "\n"))
	}
	if p.DBs[0].Target != restoredShop {
		t.Errorf("db plan: %+v", p.DBs[0])
	}
	// The load created it: Loaded keeps its name although it is now taken.
	tgt.Names[restoredShop] = true
	tgt.DBs[restoredShop] = &TargetDB{Name: restoredShop, Exists: true, Objects: 5, Owner: "app", Locale: src.DBs[0].Dump.Locale,
		Settings: map[string]string{}, SettingName: map[string]string{},
		ACL: map[string]bool{"PUBLIC CONNECT": true, "PUBLIC TEMPORARY": true, "app CONNECT": true, "app CREATE": true, "app TEMPORARY": true, "Report Reader CONNECT": true}}
	p = MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull, Loaded: map[string]string{"shop": restoredShop}})
	if !p.OK() || p.DBs[0].Target != restoredShop || len(p.DBs[0].Create) != 0 {
		t.Errorf("after the load: %v %+v", failed(p), p.DBs[0])
	}
	// What the load did not bring back fails the check.
	tgt.DBs[restoredShop].Owner = "postgres"
	delete(tgt.DBs[restoredShop].ACL, "Report Reader CONNECT")
	p = MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull, Loaded: map[string]string{"shop": restoredShop}})
	if c := failed(p)["database "+restoredShop+" attributes"]; !strings.Contains(c.Detail, "owner postgres, the backup's app") || !strings.Contains(c.Detail, "privileges") {
		t.Errorf("attributes: %+v", c)
	}
	delete(tgt.DBs, restoredShop)
	if _, ok := failed(MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull, Loaded: map[string]string{"shop": restoredShop}}))["database "+restoredShop]; !ok {
		t.Error("a database missing after the load must fail")
	}
}

// ADR-0022: the run always loads into a new database; the target's own
// databases, empty or not, are never touched. A taken name gets _2.
func TestPlanNewDatabaseName(t *testing.T) {
	src, tgt := planSource(t), freshTarget(t)
	tgt.Names["shop"], tgt.Names[restoredShop] = true, true
	tgt.DBs["shop"] = &TargetDB{Name: "shop", Exists: true, Objects: 3, Owner: "app", Locale: src.DBs[0].Dump.Locale}
	p := MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull, Now: time.Now()})
	dp := p.DBs[0]
	if dp.Target != restoredShop+"_2" || index(dp.Create, `CREATE DATABASE "`+restoredShop+`_2" TEMPLATE template0`) != 0 {
		t.Errorf("db plan: %+v", dp)
	}
	all := strings.Join(append(append(append([]string{}, dp.Create...), p.Up...), p.Down...), "\n")
	if strings.Contains(all, `"shop"`) || strings.Contains(all, "DATABASE shop") {
		t.Errorf("touches the target's shop:\n%s", all)
	}
	// Two runs in one minute: the second takes the next free number.
	tgt.Names[restoredShop+"_2"] = true
	if dp := MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull}).DBs[0]; dp.Target != restoredShop+"_3" {
		t.Errorf("third: %+v", dp)
	}
	// No readable run ts: the name takes the time of the restore.
	src.TS = ""
	if dp := MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull, Now: time.Date(2026, 10, 17, 5, 6, 0, 0, time.UTC)}).DBs[0]; dp.Target != "shop_restored_20261017_0506" {
		t.Errorf("no ts: %+v", dp)
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

// A URL run is data only: the new database is owned by the connecting user,
// a setting that differs is set on it when it is created, and after the load
// the settings in effect in it are compared (spec restore § data mode).
func TestPlanDataModeSetsSettingsOnCreate(t *testing.T) {
	src, tgt := planSource(t), freshTarget(t)
	src.Kind, src.Env.SourceKind, src.Globals = "url", "url", nil
	src.Env.DBSettings = map[string][]pg.Setting{"shop": {{Name: "TimeZone", Value: "GMT", Vartype: "string"}}}
	tgt.Roles = nil
	tgt.Env.CurrentUser, tgt.Env.Superuser = "deployer", false
	tgt.Env.PreloadLibraries = src.Env.PreloadLibraries
	tgt.Avail["pg_stat_statements"] = ExtAvail{Default: "1.11", Trusted: true}
	tgt.Effective = []pg.Setting{{Name: "TimeZone", Value: "UTC", Vartype: "string"}}
	p := MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeData})
	f := failed(p)
	if c := f["database CREATEDB"]; c.Fix != FixManual || !strings.Contains(c.Detail, "deployer needs CREATEDB") {
		t.Errorf("CREATEDB: %+v (%v)", c, f)
	}
	if _, ok := f["role app"]; ok || len(p.Up) != 0 {
		t.Errorf("data mode creates no user: %v\n%s", f, strings.Join(p.Up, "\n"))
	}
	want := []string{
		`CREATE DATABASE "` + restoredShop + `" TEMPLATE template0 encoding 'UTF8' lc_collate 'en_US.utf8' lc_ctype 'en_US.utf8' OWNER "deployer";`,
		`ALTER DATABASE "` + restoredShop + `" SET TimeZone = 'GMT';`,
	}
	if got := strings.Join(p.DBs[0].Create, "\n"); got != strings.Join(want, "\n") {
		t.Errorf("create:\n%s", got)
	}
	tgt.CreateDB = true
	if p := MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeData}); !p.OK() {
		t.Errorf("with CREATEDB: %v", failed(p))
	}

	// After the load: read in the loaded database, equal or not.
	loaded := map[string]string{"shop": restoredShop}
	tgt.DBs[restoredShop] = &TargetDB{Name: restoredShop, Exists: true, Owner: "deployer", Settings: map[string]string{}, SettingName: map[string]string{},
		Effective: []pg.Setting{{Name: "TimeZone", Value: "GMT", Vartype: "string"}}}
	if p := MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeData, Loaded: loaded}); !p.OK() {
		t.Errorf("after the load: %v", failed(p))
	}
	tgt.DBs[restoredShop].Effective[0].Value = "UTC"
	if c, ok := failed(MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeData, Loaded: loaded}))["setting shop: TimeZone"]; !ok || !strings.Contains(c.Detail, "in "+restoredShop+" UTC, backup GMT") {
		t.Errorf("differs after the load: %+v", c)
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
	if want := "sudo apt install postgresql-18-postgis-3-scripts postgresql-18-postgis-3"; hint != want {
		t.Errorf("hint %q, want %q", hint, want)
	}
}

// spec restore § outside the dump: files and collations the databases
// need are checked before anything is loaded; a file's content is not
// compared (ADR-0022).
func TestPlanNeeds(t *testing.T) {
	src, tgt := planSource(t), freshTarget(t)
	tgt.LocaleOK[localeKey(src.DBs[0].Dump.Locale)] = ""
	created := pg.Locale{Provider: "icu", Locale: "und-u-ks-level2"}
	src.DBs[0].Dump.RestoreNeeds = &pg.RestoreNeeds{
		Libraries: []pg.NeededFile{{Name: "mylib.so", UsedBy: []string{"public.score(text)"}},
			{Name: "other.so", UsedBy: []string{"public.rank(text)"}}},
		TSFiles: []pg.NeededFile{{Name: "swedish.dict", UsedBy: []string{"public.sv"}}},
		Collations: []pg.Collation{
			{Schema: "pg_catalog", Name: "sv_SE", Locale: pg.Locale{Collate: "sv_SE.utf8", Ctype: "sv_SE.utf8"}, UsedBy: []string{"public.customers.name"}},
			{Schema: "pg_catalog", Name: "de_DE", Locale: pg.Locale{Collate: "de_DE.utf8", Ctype: "de_DE.utf8"}, Version: "2.36"},
			{Schema: "public", Name: "ci", Created: true, Locale: created},
		}}
	tgt.PkgLib["mylib.so"] = true
	tgt.TSData = map[string]bool{}
	tgt.Collations = map[string]string{"de_DE": "2.41"}
	tgt.LocaleOK[localeKey(&created)] = `ICU is not supported in this build`

	p := MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull})
	f := failed(p)
	for k, want := range map[string]string{
		"library other.so":              "the target does not have it (used by shop: public.rank(text))",
		"text search file swedish.dict": "the target does not have it (used by shop: public.sv)",
		"collation sv_SE":               "the target does not have this collation (used by shop: public.customers.name)",
		`collation "public"."ci"`:       "the target cannot create it (ICU is not supported in this build)",
	} {
		if c, ok := f[k]; !ok || c.Fix != FixManual || !strings.HasPrefix(c.Detail, want) {
			t.Errorf("%s: %+v", k, c)
		}
	}
	if _, ok := f["library mylib.so"]; ok {
		t.Error("mylib.so is installed: it must not fail")
	}
	var warned bool
	for _, c := range p.Checks {
		if c.Area == "collation" && c.Name == "de_DE" && c.Warn && strings.Contains(c.Detail, "version 2.41 on the target, 2.36 in the backup") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("de_DE version must warn: %+v", p.Checks)
	}

	// Directories not readable (not a superuser): cannot check, not assumed.
	tgt.PkgLib, tgt.TSData = nil, nil
	f = failed(MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull}))
	if c := f["library mylib.so"]; !strings.HasPrefix(c.Detail, "cannot check") {
		t.Errorf("unreadable: %+v", c)
	}
}

func TestPlanLocks(t *testing.T) {
	src, tgt := planSource(t), freshTarget(t)
	tgt.LocaleOK[localeKey(src.DBs[0].Dump.Locale)] = ""
	// 2000 tables with a primary key and a text column; PG17 defaults.
	src.DBs[0].Dump.LockObjects = &pg.LockObjects{Relations: 10000}
	tgt.Locks = &pg.LockCapacity{PerTransaction: 64, Slots: 122, Source: "default"}
	p := MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull})
	c, ok := failed(p)["locks max_locks_per_transaction"]
	if !ok || c.Fix != FixSQL || !strings.Contains(c.Detail, "shop needs about 35100 locks") || !strings.Contains(c.Manual, "docker restart db") {
		t.Fatalf("check: %+v", c)
	}
	if !has(p.Up, "ALTER SYSTEM SET max_locks_per_transaction = 320;") || !has(p.Down, "ALTER SYSTEM RESET max_locks_per_transaction;") || !p.Restart {
		t.Errorf("up %v down %v restart %v", p.Up, p.Down, p.Restart)
	}

	// Data mode cannot change it: manual, with the number.
	c = failed(MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeData}))["locks max_locks_per_transaction"]
	if c.Fix != FixManual || !strings.Contains(c.Manual, "at least 320") {
		t.Errorf("data mode: %+v", c)
	}

	// Set on the command line: ALTER SYSTEM would not win.
	tgt.Locks.Source = "command line"
	c = failed(MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull}))["locks max_locks_per_transaction"]
	if c.Fix != FixManual || !strings.Contains(c.Manual, "-c max_locks_per_transaction in the container's command to 320") {
		t.Errorf("command line: %+v", c)
	}

	// Enough room: ok, nothing to change.
	tgt.Locks = &pg.LockCapacity{PerTransaction: 320, Slots: 122}
	p = MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull})
	if _, ok := failed(p)["locks max_locks_per_transaction"]; ok || index(p.Up, "ALTER SYSTEM SET max_locks") >= 0 {
		t.Errorf("enough: %v %v", p.Failed(), p.Up)
	}
}

func TestPlanConfigExtension(t *testing.T) {
	src, tgt := planSource(t), freshTarget(t)
	tgt.LocaleOK[localeKey(src.DBs[0].Dump.Locale)] = ""
	ts := pg.Extension{Name: "timescaledb", Version: "2.29.2", Schema: "public", Config: true}
	src.DBs[0].Dump.Extensions = []pg.Extension{ts}
	check := func(p *Plan) Check {
		for _, c := range p.Checks {
			if c.Area == "extension" && c.Name == "shop: timescaledb 2.29.2" {
				return c
			}
		}
		t.Fatalf("no check: %+v", p.Checks)
		return Check{}
	}

	// Same default: the dump's CREATE EXTENSION installs it.
	tgt.Avail["timescaledb"] = ExtAvail{Default: "2.29.2"}
	if c := check(MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull})); !c.OK {
		t.Errorf("same default: %+v", c)
	}

	// A newer default: manual, naming the source's image.
	tgt.Avail["timescaledb"] = ExtAvail{Default: "2.31.0"}
	src.Env.Container = &pg.ContainerEnv{Image: "timescale/timescaledb:2.29.2-pg17"}
	c := check(MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: ModeFull}))
	if c.OK || c.Fix != FixManual || c.Detail != "the target installs timescaledb 2.31.0 by default; the dump holds its configuration tables as of 2.29.2" ||
		!strings.HasPrefix(c.Manual, "use image timescale/timescaledb:2.29.2-pg17 (the source's) for container db") {
		t.Errorf("newer default: %+v", c)
	}
}
