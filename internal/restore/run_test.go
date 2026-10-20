package restore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
)

var into = Options{IntoContainer: "shop-db"}

func opts(mod func(*Options)) Options {
	o := into
	if mod != nil {
		mod(&o)
	}
	return o
}

// editManifest changes the seeded manifest in place.
func editManifest(t *testing.T, st *memStore, edit func(*backup.Manifest)) {
	t.Helper()
	ctx := context.Background()
	m, err := backup.LoadManifest(ctx, st, "bracer/shop/manifest.json", "shop")
	if err != nil {
		t.Fatal(err)
	}
	edit(m)
	if err := backup.SaveManifest(ctx, st, "bracer/shop/manifest.json", m, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// putObject stores content at key and points every dump with that key at its checksum.
func putObject(t *testing.T, st *memStore, key, content string) {
	t.Helper()
	st.Put(context.Background(), key, strings.NewReader(content), int64(len(content)))
	editManifest(t, st, func(m *backup.Manifest) {
		for _, im := range m.Instances {
			for _, dm := range im.Databases {
				for _, dp := range dm.Dumps {
					if dp.Key == key {
						dp.SHA256, dp.Bytes = sha([]byte(content)), int64(len(content))
					}
				}
			}
			for _, dp := range im.Globals.Dumps {
				if dp.Key == key {
					dp.SHA256 = sha([]byte(content))
				}
			}
		}
	})
}

// addDatabase adds a second database to the seeded backup and teaches the fake its dump.
func addDatabase(t *testing.T, st *memStore, f *fakeServer, db string) {
	t.Helper()
	content := "PGDMP-" + db
	key := "bracer/shop/main/" + db + "/2026-10-16T030000Z.dump"
	tables := []pg.Table{{Schema: "public", Name: db + "_items", Rows: i64(7)}}
	editManifest(t, st, func(m *backup.Manifest) {
		m.Instances["main"].Databases[db] = &backup.DatabaseManifest{Latest: "2026-10-16T030000Z", Dumps: []*backup.Dump{
			{TS: "2026-10-16T030000Z", Key: key, Tables: tables}}}
	})
	putObject(t, st, key, content)
	f.dumps[content] = tables
	f.attrSQL[content] = "CREATE DATABASE " + db + " WITH TEMPLATE = template0 ENCODING = 'UTF8';\nALTER DATABASE " + db + " OWNER TO postgres;\n"
}

// emptyTarget makes the fake a fresh server: postgres, the bootstrap user, nothing else.
func emptyTarget(f *fakeServer) {
	f.dbs = map[string][]pg.Table{"postgres": nil}
	f.roles = map[string]bool{"postgres": true, "pg_read_all_data": true}
}

// noChange fails if the target was changed.
func noChange(t *testing.T, f *fakeServer) {
	t.Helper()
	for _, c := range f.calls {
		if strings.HasPrefix(c, "pg_restore -d") || strings.Contains(c, "create database") || strings.Contains(c, "--single-transaction") {
			t.Errorf("must not change anything: %s", c)
		}
	}
}

func hasWarning(res *Result, s string) bool {
	for _, w := range res.Warnings {
		if strings.Contains(w, s) {
			return true
		}
	}
	return false
}

func TestRestoreTargetFlags(t *testing.T) {
	f := newFakeServer()
	st := newMemStore()
	seed(t, st, "")
	for _, o := range []Options{{}, {IntoContainer: "a", IntoURLEnv: "B"}, {IntoURL: true, IntoURLEnv: "B"}} {
		if _, err := Run(context.Background(), baseCfg(), o, deps(f, st)); !errors.As(err, new(*UsageError)) || !strings.Contains(err.Error(), "exactly one target") {
			t.Errorf("%+v: %v", o, err)
		}
	}
	// Without --preview the run is read first; a backup from before ADR-0015 can only be previewed.
	for _, o := range []Options{{IntoURL: true}, {IntoURLEnv: "MISSING"}, into} {
		if _, err := Run(context.Background(), baseCfg(), o, deps(f, st)); !errors.As(err, new(*UsageError)) || !strings.Contains(err.Error(), "use --preview") {
			t.Errorf("%+v, backup without environment record: %v", o, err)
		}
	}
	if _, err := Run(context.Background(), baseCfg(), Options{IntoURL: true, Preview: true}, deps(f, st)); !errors.As(err, new(*UsageError)) || !strings.Contains(err.Error(), "--into-url-env <NAME>") {
		t.Errorf("no terminal: %v", err)
	}
	if _, err := Run(context.Background(), baseCfg(), Options{IntoURLEnv: "MISSING", Preview: true}, deps(f, st)); err == nil || !strings.Contains(err.Error(), "MISSING") {
		t.Errorf("missing env: %v", err)
	}
	if _, err := Run(context.Background(), baseCfg(), opts(func(o *Options) { o.File = "x"; o.Databases = []string{"shop"} }), deps(f, st)); !errors.As(err, new(*UsageError)) {
		t.Errorf("--file with --database: %v", err)
	}
	noChange(t, f)
}

func TestRestorePreview(t *testing.T) {
	f := newFakeServer()
	f.createDBOnly = true
	f.schemaSQL["PGDMP-shop"] = shopSchemaSQL + "--\n-- Name: postgis; Type: EXTENSION; Schema: -; Owner: -\n--\n\nCREATE EXTENSION postgis;\n--\n-- Name: f(); Type: FUNCTION; Schema: public; Owner: app\n--\n\nCREATE FUNCTION public.f() RETURNS integer\n    LANGUAGE c\n    AS 'x', 'f';\n"
	f.trusted = map[string]bool{"postgis": false}
	st := newMemStore()
	seed(t, st, "")
	if _, err := Run(context.Background(), baseCfg(), into, deps(f, st)); !errors.As(err, new(*UsageError)) || !strings.Contains(err.Error(), "--preview") {
		t.Fatalf("not superuser without --preview: %v", err)
	}
	preview := opts(func(o *Options) { o.Preview = true })
	res, err := Run(context.Background(), baseCfg(), preview, deps(f, st))
	if !errors.Is(err, ErrPreflight) || strings.Join(res.Preflight.NeedsSuperuser, "; ") != "shop: extension postgis (not trusted); shop: function public.f() (language c)" {
		t.Fatalf("err = %v, res = %+v", err, res.Preflight)
	}
	noChange(t, f)

	f.schemaSQL["PGDMP-shop"] = shopSchemaSQL
	f.attrSQL["PGDMP-shop"] = shopAttrSQL
	res, err = Run(context.Background(), baseCfg(), preview, deps(f, st))
	if err != nil || !res.OK || res.Mode != ModePreview {
		t.Fatalf("err = %v, res = %+v", err, res)
	}
	db := res.Databases[0]
	if db.TargetDB != "shop_preview_20261016_0300" {
		t.Errorf("db: %+v", db)
	}
	if !strings.Contains(strings.Join(f.calls, "\n"), "pg_restore -d dbname='shop_preview_20261016_0300' --no-data-for-failed-tables --no-owner --no-acl --no-publications --no-subscriptions --no-security-labels") {
		t.Errorf("calls:\n%s", strings.Join(f.calls, "\n"))
	}
	for _, l := range strings.Split(strings.TrimSpace(f.attrRun), "\n") {
		if !strings.HasPrefix(l, `ALTER DATABASE "shop_preview_20261016_0300" SET `) {
			t.Errorf("preview applies settings only: %q", l)
		}
	}
	if f.owner["shop_preview_20261016_0300"] != "" {
		t.Errorf("owner: %q", f.owner["shop_preview_20261016_0300"])
	}
	f.readOnly, f.createDBOnly = true, false
	if _, err := Run(context.Background(), baseCfg(), preview, deps(f, st)); !errors.As(err, new(*UsageError)) || !strings.Contains(err.Error(), "CREATEDB") {
		t.Errorf("no CREATEDB: %v", err)
	}
}

func TestRestoreFromFile(t *testing.T) {
	f := newFakeServer()
	st := newMemStore()
	seed(t, st, "")
	plain := filepath.Join(t.TempDir(), "shop.dump")
	os.WriteFile(plain, []byte("PGDMP-shop"), 0o600)
	res, err := Run(context.Background(), baseCfg(), opts(func(o *Options) { o.File = plain }), deps(f, st))
	if err != nil || !res.OK || res.Source.File != plain || res.Mode != ModePreview {
		t.Fatalf("file: %v %+v", err, res)
	}
	db := res.Databases[0]
	if db.SourceDB != "shop" || db.TargetDB != "shop_preview_20261017_0300" || db.TablesFound != 1 || db.TablesExpected != 0 {
		t.Errorf("db: %+v", db)
	}
	// An empty target with --file is still a preview: no users.
	f = newFakeServer()
	emptyTarget(f)
	res, err = Run(context.Background(), baseCfg(), opts(func(o *Options) { o.File = plain }), deps(f, st))
	if err != nil || !res.OK || res.Mode != ModePreview || res.Databases[0].TargetDB != "shop_preview_20261017_0300" {
		t.Fatalf("clean + file: %v %+v", err, res)
	}
	bad := filepath.Join(t.TempDir(), "corrupt.dump")
	os.WriteFile(bad, []byte("this is not a dump at all"), 0o600)
	f = newFakeServer()
	if _, err := Run(context.Background(), baseCfg(), opts(func(o *Options) { o.File = bad }), deps(f, st)); err == nil || !strings.Contains(err.Error(), "nothing was created") {
		t.Fatalf("corrupt: %v", err)
	}
	noChange(t, f)
}

// One database fails: it is left in place and the others still run.
// A dump made with pg_dump 18 cannot be read by pg_restore 17 in the target container.
func TestSideName(t *testing.T) {
	at := time.Date(2026, 10, 16, 3, 0, 0, 0, time.UTC)
	taken := map[string]bool{"shop_restored_20261016_0300": true}
	name, err := SideName("shop", "restored", at, func(n string) (bool, error) { return taken[n], nil })
	if err != nil || name != "shop_restored_20261016_0300_2" {
		t.Errorf("name = %q, %v", name, err)
	}
	long := strings.Repeat("é", 40) // 80 bytes
	name, _ = SideName(long, "preview", at, func(string) (bool, error) { return false, nil })
	if len(name) > 63 || !strings.HasSuffix(name, "_preview_20261016_0300") || !bytes.HasPrefix([]byte(name), []byte("éé")) || strings.ContainsRune(name, '\uFFFD') {
		t.Errorf("long name = %q (%d bytes)", name, len(name))
	}
}

// A container target whose socket asks for a password (Bitnami): the password
// comes from --password-env or the terminal, and the apply command passes PGPASSWORD.
func TestConnectSocketPassword(t *testing.T) {
	fake := func(_ context.Context, _ *pg.Tools, in config.Instance, s *config.Secrets) (pg.Conn, pg.Runner, error) {
		if in.PasswordEnv == "" {
			return pg.Conn{}, nil, errors.New(`psql: error: connection to server on socket "/tmp/.s.PGSQL.5432" failed: fe_sendauth: no password supplied`)
		}
		v, err := s.Require(in.PasswordEnv)
		if err != nil {
			return pg.Conn{}, nil, err
		}
		return pg.Conn{User: in.User, Password: v}, nil, nil
	}
	in := config.Instance{Name: "db", Container: "db", User: "postgres"}
	d := Deps{Connect: fake, Secrets: config.NewSecrets(map[string]string{"TGT_PW": "from-env"}, "test")}
	if _, _, err := connectSocket(context.Background(), Options{}, d, in, "container db"); err == nil || !strings.Contains(err.Error(), "--password-env") {
		t.Errorf("no terminal: %v", err)
	}
	conn, _, err := connectSocket(context.Background(), Options{PasswordEnv: "TGT_PW"}, d, in, "container db")
	if err != nil || conn.Password != "from-env" {
		t.Errorf("--password-env: %v %+v", err, conn)
	}
	d.AskPassword = func() (string, error) { return "typed", nil }
	conn, _, err = connectSocket(context.Background(), Options{}, d, in, "container db")
	if err != nil || conn.Password != "typed" {
		t.Errorf("terminal: %v %+v", err, conn)
	}
	if got := applyCommand(TargetSpec{Kind: "container", Container: "db", Password: true}, conn, 17); !strings.HasPrefix(got, "docker exec -i -e PGPASSWORD db psql -U postgres") {
		t.Errorf("apply: %s", got)
	}
}
