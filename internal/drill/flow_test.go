package drill

import (
	"context"
	"strings"
	"testing"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/restore"
)

// ADR-0015 § 4: a local source is rebuilt from the official image of its
// major, its extension packages and its locales.
func TestLocalDockerfile(t *testing.T) {
	env := &pg.Env{ServerVersionNum: 180006, Local: &pg.LocalEnv{Packages: []pg.Package{
		{Extension: "vector", Package: "postgresql-18-pgvector"}, {Extension: "postgis", Package: "postgresql-18-postgis-3"}}}}
	dumps := []*backup.Dump{
		{Locale: &pg.Locale{Encoding: "UTF8", Collate: "en_GB.UTF-8", Ctype: "en_GB.UTF-8"}},
		{Locale: &pg.Locale{Encoding: "UTF8", Collate: "C.UTF-8", Ctype: "en_US.utf8"}},
		{Locale: &pg.Locale{Encoding: "UTF8", Collate: "vi_VN.utf8@x", Ctype: "C", Provider: "icu", Locale: "vi"}},
	}
	got, err := LocalDockerfile(env, dumps)
	want := "FROM postgres:18\n" +
		"RUN apt-get update && apt-get install -y --no-install-recommends postgresql-18-pgvector postgresql-18-postgis-3 && rm -rf /var/lib/apt/lists/*\n" +
		"RUN localedef -i en_GB -c -f UTF-8 -A /usr/share/locale/locale.alias en_GB.UTF-8\n"
	if err != nil || got != want {
		t.Errorf("err %v\n%s", err, got)
	}
	// A dump made by a newer pg_dump needs that major; package names follow it.
	dumps[0].ToolVersion = "19.1"
	if got, _ := LocalDockerfile(env, dumps[:1]); !strings.HasPrefix(got, "FROM postgres:19\n") || !strings.Contains(got, "postgresql-19-pgvector") {
		t.Errorf("newer tool:\n%s", got)
	}
	if _, err := LocalDockerfile(env, []*backup.Dump{{Locale: &pg.Locale{Collate: "de_DE; rm -rf /", Ctype: "C"}}}); err == nil {
		t.Error("a locale name with shell characters must be refused")
	}
}

func TestCheckImage(t *testing.T) {
	pre := "pg_stat_statements,timescaledb,neon"
	env := &pg.Env{PreloadLibraries: &pre}
	dumps := []*backup.Dump{{Extensions: []pg.Extension{{Name: "timescaledb", Version: "2.17.0"}, {Name: "pg_trgm", Version: "1.6"}}}}
	exts := map[string]bool{"timescaledb": true, "pg_trgm": true}
	libs := map[string]bool{"timescaledb.so": true, "pg_stat_statements.so": true}
	preload, notes, err := checkImage(env, dumps, "img", exts, libs)
	if err != nil || strings.Join(preload, ",") != "pg_stat_statements,timescaledb" || len(notes) != 1 || !strings.Contains(notes[0], "neon") {
		t.Errorf("preload %v notes %v err %v", preload, notes, err)
	}
	// An extension without its control file, or a needed library without its .so, stops before any container.
	if _, _, err := checkImage(env, dumps, "img", map[string]bool{"pg_trgm": true}, libs); err == nil || !strings.Contains(err.Error(), "has no extension timescaledb") {
		t.Errorf("missing extension: %v", err)
	}
	if _, _, err := checkImage(env, dumps, "img", exts, map[string]bool{}); err == nil || !strings.Contains(err.Error(), "no library timescaledb") {
		t.Errorf("missing library: %v", err)
	}
}

// env-down must bring the temporary Postgres back exactly; anything left is named.
func TestFingerprintDiff(t *testing.T) {
	st := func() *restore.TargetState {
		return &restore.TargetState{Env: &pg.Env{Settings: []pg.Setting{{Name: "DateStyle", Value: "ISO, MDY"}}},
			Names: map[string]bool{"postgres": true}, DBs: map[string]*restore.TargetDB{"shop": {Name: "shop"}}, AutoConf: map[string]string{}}
	}
	a, b := st(), st()
	b.Names["shop"] = true
	b.Env.Settings[0].Value = "ISO, DMY"
	b.AutoConf["datestyle"] = "ISO, DMY"
	b.DBs["shop"].Connections = []string{"app@local"} // not part of the state
	got := strings.Join(diffPrints(fingerprint(a, []string{"shop"}), fingerprint(b, []string{"shop"})), "; ")
	if got != "databases changed; postgresql.auto.conf datestyle was added; setting DateStyle changed" {
		t.Errorf("diff: %s", got)
	}
	if d := diffPrints(fingerprint(a, []string{"shop"}), fingerprint(st(), []string{"shop"})); len(d) != 0 {
		t.Errorf("same state: %v", d)
	}
}

// pg_hba and ssl are the real target's business: notes in a drill, not failures.
func TestDrillFailedSkipsTargetOnly(t *testing.T) {
	p := &restore.Plan{Checks: []restore.Check{
		{Area: "ssl", Name: "ssl", Detail: "on in the backup"},
		{Area: "pg_hba", Name: "app", Detail: "md5"},
		{Area: "extension", Name: "shop: vector", Fix: restore.FixManual},
		{Area: "database", Name: "shop", OK: true},
	}}
	ir := &InstanceResult{}
	got := drillFailed(p, ir)
	if len(got) != 1 || got[0].Area != "extension" || len(ir.Notes) != 2 || !strings.Contains(ir.Notes[0], "restore checks this on the real target") {
		t.Errorf("failed %+v notes %v", got, ir.Notes)
	}
}

// A container that exits while starting fails at once, with its logs.
func TestWaitReadyStoppedContainer(t *testing.T) {
	sh := &scriptShell{answers: map[string]pg.Result{
		"docker inspect": {Stdout: []byte("false\n")},
		"docker logs":    {Stderr: []byte("FATAL:  could not access file \"neon\": No such file or directory\n")},
	}, fail: map[string]bool{"docker exec": true}}
	rt := &dockerRuntime{sh: sh}
	tp := &tempPG{run: pg.ExecRunner{Shell: sh, Container: "bracer-drill-shop-1"}, name: "bracer-drill-shop-1", image: "postgres:18", user: "app"}
	d := withDefaults(Deps{Shell: sh, ReadyTimeout: time.Hour, Poll: time.Millisecond})
	err := rt.waitReady(context.Background(), tp, d)
	if err == nil || !strings.Contains(err.Error(), "stopped while starting") || !strings.Contains(err.Error(), `could not access file "neon"`) {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(strings.Join(sh.calls, "\n"), "bracer-drill-shop-1 pg_isready -h 127.0.0.1 -U app") {
		t.Errorf("pg_isready must use the bootstrap user: %v", sh.calls)
	}
}

// scriptShell answers by "name first-arg"; fail makes that command exit 1.
type scriptShell struct {
	answers map[string]pg.Result
	fail    map[string]bool
	calls   []string
}

func (s *scriptShell) Run(_ context.Context, c pg.Cmd) (pg.Result, error) {
	s.calls = append(s.calls, c.Name+" "+strings.Join(c.Args, " "))
	key := c.Name
	if len(c.Args) > 0 {
		key += " " + c.Args[0]
	}
	if s.fail[key] {
		return pg.Result{}, &pg.ExitError{Cmd: key, Code: 1}
	}
	return s.answers[key], nil
}
