package drill

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/crypto"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/restore"
	"bracer.dev/bracer/internal/storage"
)

// fakeTable is a table in the fake Postgres: the real row count.
type fakeTable struct {
	schema, name string
	rows         int64
	analyzed     bool
}

// fakeDocker fakes enough of `docker` for drill: run/rm/ps/logs/exec with psql,
// pg_isready, pg_restore. Dump contents (payload) map to tables.
type fakeDocker struct {
	mu          sync.Mutex
	dumps       map[string][]fakeTable // contents pg_restore receives → tables
	containers  map[string]*fakeContainer
	orphans     []string
	readyAfter  int // number of pg_isready failures before ready
	isreadyCall int
	checks      map[string]string // check sql → output ("" = no rows, "ERR:…" = error)
	failRun     bool
	calls       []string
	removed     []string
	volumes     map[string]bool
	removedVols []string
	orphanVols  []string
	dfAvailKB   int64             // -1: df fails (cannot measure)
	createOpts  []string          // what follows TEMPLATE template0 in every CREATE DATABASE, all containers
	available   map[string]string // pg_available_extensions in the temporary image
	noDocker    bool              // `docker version` fails
}

type fakeContainer struct {
	image      string
	dbs        map[string][]fakeTable
	createOpts []string // what follows TEMPLATE template0 in each CREATE DATABASE
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		dumps: map[string][]fakeTable{
			"PGDMP-shop": {{"public", "orders", 500, false}, {"public", "events", 83_000, false}, {"public", "empty", 0, false}},
		},
		containers: map[string]*fakeContainer{},
		checks:     map[string]string{},
		volumes:    map[string]bool{},
		dfAvailKB:  1 << 30,
	}
}

func (f *fakeDocker) Run(_ context.Context, c pg.Cmd) (pg.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(c.Args, " "))
	if c.Name != "docker" {
		return pg.Result{}, fmt.Errorf("fake: not docker: %s", c.Name)
	}
	a := c.Args
	switch a[0] {
	case "version":
		if f.noDocker {
			return pg.Result{}, &pg.ExitError{Cmd: "docker version", Code: 1, Stderr: []byte("Cannot connect to the Docker daemon")}
		}
		return pg.Result{Stdout: []byte("29.0\n")}, nil
	case "ps":
		return pg.Result{Stdout: []byte(strings.Join(f.orphans, "\n"))}, nil
	case "rm":
		for _, id := range a[1:] {
			if strings.HasPrefix(id, "-") {
				continue
			}
			f.removed = append(f.removed, id)
			delete(f.containers, id)
		}
		return pg.Result{}, nil
	case "info":
		return pg.Result{Stdout: []byte("/var/lib/docker\n")}, nil
	case "image":
		return pg.Result{Stdout: []byte(`{"/var/lib/postgresql/data":{}}` + "\n")}, nil
	case "volume":
		switch a[1] {
		case "create":
			f.volumes[a[len(a)-1]] = true
		case "ls":
			return pg.Result{Stdout: []byte(strings.Join(f.orphanVols, "\n"))}, nil
		case "rm":
			for _, v := range a[2:] {
				f.removedVols = append(f.removedVols, v)
				delete(f.volumes, v)
			}
		}
		return pg.Result{}, nil
	case "logs":
		return pg.Result{Stdout: []byte("fake log line\n")}, nil
	case "run":
		if len(a) > 3 && a[len(a)-3] == "df" {
			// df must measure through the created drill volume, mounted read-only; no
			// host path is bind-mounted.
			joined := strings.Join(a, " ")
			var vol string
			for i, x := range a {
				if x == "-v" {
					vol, _, _ = strings.Cut(a[i+1], ":")
				}
			}
			if !f.volumes[vol] || !strings.Contains(joined, "-v "+vol+":/mnt/bracer-disk:ro") || strings.Contains(joined, "-v /") {
				return pg.Result{}, fmt.Errorf("fake: df must mount a created volume read-only: %v", a)
			}
			if f.dfAvailKB < 0 {
				return pg.Result{}, &pg.ExitError{Cmd: "docker run", Code: 1, Stderr: []byte("cannot run")}
			}
			return pg.Result{Stdout: []byte(fmt.Sprintf("Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/x 100 1 %d 1%% /mnt/bracer-disk\n", f.dfAvailKB))}, nil
		}
		if f.failRun {
			return pg.Result{}, &pg.ExitError{Cmd: "docker run", Code: 125, Stderr: []byte("Unable to find image")}
		}
		var name string
		for i, x := range a {
			if x == "--name" {
				name = a[i+1]
			}
		}
		if !strings.Contains(strings.Join(a, " "), "--network none") || !strings.Contains(strings.Join(a, " "), "--label "+Label+"=shop") {
			return pg.Result{}, fmt.Errorf("fake: run must be --network none with label: %v", a)
		}
		if !strings.Contains(strings.Join(a, " "), "-v "+name+":/var/lib/postgresql/data") || !f.volumes[name] {
			return pg.Result{}, fmt.Errorf("fake: run must mount the named volume %s: %v", name, a)
		}
		for i, x := range a {
			if x == "-e" && strings.Contains(a[i+1], "=") {
				return pg.Result{}, fmt.Errorf("fake: secret value in argv: %s", a[i+1])
			}
		}
		if !hasEnv(c.Env, "POSTGRES_PASSWORD") {
			return pg.Result{}, fmt.Errorf("fake: POSTGRES_PASSWORD missing from env")
		}
		f.containers[name] = &fakeContainer{image: a[len(a)-1], dbs: map[string][]fakeTable{"postgres": nil}}
		return pg.Result{Stdout: []byte("abc123\n")}, nil
	case "exec":
		return f.exec(a[1:], c)
	}
	return pg.Result{}, fmt.Errorf("fake: unexpected docker %v", a)
}

func hasEnv(env []string, key string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return true
		}
	}
	return false
}

func envValue(env []string, key string) string {
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return strings.TrimPrefix(e, key+"=")
		}
	}
	return ""
}

func (f *fakeDocker) exec(a []string, c pg.Cmd) (pg.Result, error) {
	// exec -i [-e NAME]... <container> <tool> args...
	i := 1
	for i < len(a) && a[i] == "-e" {
		if strings.Contains(a[i+1], "=") {
			return pg.Result{}, fmt.Errorf("fake: secret value in argv: %s", a[i+1])
		}
		i += 2
	}
	name, tool, args := a[i], a[i+1], a[i+2:]
	ct := f.containers[name]
	if ct == nil {
		return pg.Result{}, &pg.ExitError{Cmd: "docker exec", Code: 1, Stderr: []byte("Error: No such container: " + name)}
	}
	return f.tool(ct, tool, args, envValue(c.Env, "PGDATABASE"), c)
}

// tool emulates pg_isready, psql and pg_restore inside a container; db is PGDATABASE.
func (f *fakeDocker) tool(ct *fakeContainer, tool string, args []string, db string, c pg.Cmd) (pg.Result, error) {
	write := func(s string) {
		if c.Stdout != nil {
			io.WriteString(c.Stdout, s)
		}
	}
	switch tool {
	case "pg_isready":
		f.isreadyCall++
		if f.isreadyCall <= f.readyAfter {
			return pg.Result{}, &pg.ExitError{Cmd: "pg_isready", Code: 2}
		}
		return pg.Result{}, nil
	case "psql":
		if args[len(args)-1] == "-" { // globals
			data, _ := io.ReadAll(c.Stdin)
			var stderr bytes.Buffer
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "CREATE ROLE postgres") {
					stderr.WriteString(`psql:<stdin>:3: ERROR:  role "postgres" already exists` + "\n")
				}
				if strings.HasPrefix(line, "BROKEN") {
					stderr.WriteString("psql:<stdin>:9: ERROR:  syntax error at or near \"BROKEN\"\n")
				}
			}
			return pg.Result{Stderr: stderr.Bytes()}, nil
		}
		sql := args[len(args)-1]
		tables := ct.dbs[db]
		switch {
		case strings.HasPrefix(sql, "create database "):
			n, opts, _ := strings.Cut(strings.TrimPrefix(sql, "create database "), " template template0")
			n = strings.Trim(n, `"`)
			ct.createOpts = append(ct.createOpts, opts)
			f.createOpts = append(f.createOpts, opts)
			if _, ok := ct.dbs[n]; ok {
				return pg.Result{}, &pg.ExitError{Cmd: "psql", Code: 1, Stderr: []byte("ERROR:  database already exists")}
			}
			ct.dbs[n] = nil
		case strings.HasPrefix(sql, "select name, coalesce(default_version, '') from pg_available_extensions"):
			var out strings.Builder
			for n, v := range f.available {
				fmt.Fprintf(&out, "%s\x1f%s\n", n, v)
			}
			write(out.String())
		case sql == "analyze":
			for k := range tables {
				tables[k].analyzed = true
			}
		case strings.HasPrefix(sql, "select n.nspname, c.relname"):
			var out strings.Builder
			for _, t := range tables {
				est := int64(-1)
				if t.analyzed {
					est = t.rows + t.rows/50 // reltuples off by 2%
				}
				fmt.Fprintf(&out, "%s\x1f%s\x1f%d\n", t.schema, t.name, est)
			}
			write(out.String())
		case strings.HasPrefix(sql, "select count(*) from "):
			ident := strings.TrimPrefix(sql, "select count(*) from ")
			for _, t := range tables {
				if pg.QuoteIdent(t.schema)+"."+pg.QuoteIdent(t.name) == ident {
					write(fmt.Sprintf("%d\n", t.rows))
					return pg.Result{}, nil
				}
			}
			return pg.Result{}, &pg.ExitError{Cmd: "psql", Code: 1, Stderr: []byte("ERROR:  relation does not exist")}
		default:
			out, ok := f.checks[sql]
			if !ok {
				return pg.Result{}, fmt.Errorf("fake: unexpected sql %q", sql)
			}
			if strings.HasPrefix(out, "ERR:") {
				return pg.Result{}, &pg.ExitError{Cmd: "psql", Code: 1, Stderr: []byte(strings.TrimPrefix(out, "ERR:"))}
			}
			write(out)
		}
		return pg.Result{}, nil
	case "pg_restore":
		if _, ok := ct.dbs[db]; !ok {
			return pg.Result{}, &pg.ExitError{Cmd: "pg_restore", Code: 1, Stderr: []byte(`pg_restore: error: connection to server failed: database "` + db + `" does not exist`)}
		}
		data, _ := io.ReadAll(c.Stdin)
		tables, ok := f.dumps[string(data)]
		if !ok {
			return pg.Result{}, &pg.ExitError{Cmd: "pg_restore", Code: 1, Stderr: []byte("pg_restore: error: could not read from input file: end of file\npg_restore: error: input file does not appear to be a valid archive")}
		}
		cp := make([]fakeTable, len(tables))
		copy(cp, tables)
		ct.dbs[db] = cp
		return pg.Result{}, nil
	}
	return pg.Result{}, fmt.Errorf("fake: unexpected tool %s", tool)
}

// memStore is an in-memory bucket.
type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newMemStore() *memStore { return &memStore{objects: map[string][]byte{}} }

func (m *memStore) Check(context.Context) error { return nil }
func (m *memStore) Describe() string            { return "mem" }
func (m *memStore) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = data
	return nil
}
func (m *memStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (m *memStore) List(context.Context, string) ([]storage.Object, error) { return nil, nil }
func (m *memStore) Delete(context.Context, string) error                   { return nil }

func i64(n int64) *int64 { return &n }

// seed writes the manifest + objects for app shop/instance main with database shop
// (tables orders rows 500, events rows_est 83000, empty rows 0) and globals.
func seed(t *testing.T, st *memStore, recipient string) {
	t.Helper()
	ctx := context.Background()
	put := func(key, content string) {
		var buf bytes.Buffer
		if recipient != "" {
			w, err := crypto.Encrypt(&buf, recipient)
			if err != nil {
				t.Fatal(err)
			}
			io.WriteString(w, content)
			w.Close()
		} else {
			buf.WriteString(content)
		}
		st.Put(ctx, key, &buf, int64(buf.Len()))
	}
	var rp *string
	if recipient != "" {
		rp = &recipient
	}
	put("bracer/shop/main/shop/2026-10-16T030000Z.dump", "PGDMP-shop")
	put("bracer/shop/main/globals/2026-10-16T030000Z.sql", "CREATE ROLE postgres;\nCREATE ROLE app;\n")
	m := &backup.Manifest{App: "shop", Instances: map[string]*backup.InstanceManifest{
		"main": {PgVersion: "17.6", Databases: map[string]*backup.DatabaseManifest{
			"shop": {Latest: "2026-10-16T030000Z", Dumps: []*backup.Dump{{
				TS: "2026-10-16T030000Z", Key: "bracer/shop/main/shop/2026-10-16T030000Z.dump", Bytes: 10, Encrypted: recipient != "", Recipient: rp,
				Locale: &pg.Locale{Encoding: "UTF8", Collate: "C.UTF-8", Ctype: "C.UTF-8"},
				Tables: []pg.Table{
					{Schema: "public", Name: "orders", Rows: i64(500)},
					{Schema: "public", Name: "events", RowsEst: i64(83_000)},
					{Schema: "public", Name: "empty", Rows: i64(0)},
				},
			}}},
		}, Globals: backup.GlobalsManifest{Latest: "2026-10-16T030000Z", HasPasswords: true, Dumps: []*backup.Dump{{
			TS: "2026-10-16T030000Z", Key: "bracer/shop/main/globals/2026-10-16T030000Z.sql", Encrypted: recipient != "", Recipient: rp,
		}}}},
	}}
	if err := backup.SaveManifest(ctx, st, "bracer/shop/manifest.json", m, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func baseCfg() *config.App {
	return &config.App{
		App:        "shop",
		Instances:  []config.Instance{{Name: "main", Container: "shop-db", User: "postgres", PgMajor: 17, Databases: config.Databases{All: true}}},
		Storage:    &config.Storage{Endpoint: "https://s3.example.com", Bucket: "b", Prefix: "bracer", AccessKeyEnv: "A", SecretKeyEnv: "S"},
		Encryption: "none",
	}
}

func deps(f *fakeDocker, st storage.Store) Deps {
	return depsFor(f, st)
}

// depsFor: the fake clock advances 1 s per Now(); no kubectl on PATH unless the shell is a fakeKube.
func depsFor(sh pg.Shell, st storage.Store) Deps {
	clock := time.Date(2026, 10, 17, 3, 0, 0, 0, time.UTC)
	lookPath := func(string) (string, error) { return "", errors.New("not found") }
	if _, ok := sh.(*fakeKube); ok {
		lookPath = func(name string) (string, error) {
			if name == "kubectl" {
				return "/usr/local/bin/kubectl", nil
			}
			return "", errors.New("not found")
		}
	}
	return Deps{
		Shell: sh, Store: st,
		Tools: &pg.Tools{Shell: sh, LookPath: lookPath},
		Now:   func() time.Time { clock = clock.Add(time.Second); return clock },
		Rand:  func(n int) string { return strings.Repeat("a", n*2) },
		Poll:  time.Millisecond,
	}
}

func TestDrillPassAndCleanup(t *testing.T) {
	f := newFakeDocker()
	f.readyAfter = 2
	f.orphans = []string{"deadbeef"}
	f.orphanVols = []string{"oldvol"}
	f.checks["select 1 from orders limit 1"] = "1\n"
	st := newMemStore()
	seed(t, st, "")
	cfg := baseCfg()
	cfg.Drill = &config.Drill{Checks: []config.Check{{Name: "orders exist", SQL: "select 1 from orders limit 1"}, {Name: "other db only", SQL: "select 2", Databases: []string{"other"}}}}

	res, err := Run(context.Background(), cfg, deps(f, st))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Kind != "drill" || len(res.Instances) != 1 {
		t.Fatalf("result: %+v", res)
	}
	ir := res.Instances[0]
	if ir.Image != "postgres:17-alpine" || ir.Container != "" {
		t.Errorf("instance: %+v", ir)
	}
	// The database is created with the locale from the manifest.
	if len(f.createOpts) != 1 || f.createOpts[0] != " encoding 'UTF8' lc_collate 'C.UTF-8' lc_ctype 'C.UTF-8'" {
		t.Errorf("create database options: %q", f.createOpts)
	}
	if ir.Globals == nil || !ir.Globals.OK || ir.Globals.IgnoredErrors != 1 {
		t.Errorf("globals: %+v", ir.Globals)
	}
	if len(ir.Databases) != 1 {
		t.Fatalf("databases: %+v", ir.Databases)
	}
	dr := ir.Databases[0]
	if !dr.OK || dr.TablesExpected != 3 || dr.TablesFound != 3 || len(dr.RowMismatches) != 0 || dr.Bytes != 10 {
		t.Errorf("db: %+v", dr)
	}
	if len(dr.Checks) != 1 || !dr.Checks[0].OK || dr.Checks[0].Name != "orders exist" {
		t.Errorf("checks: %+v", dr.Checks)
	}
	// Orphans are cleaned up first, the temporary container is removed afterwards.
	if len(f.removed) != 2 || f.removed[0] != "deadbeef" || !strings.HasPrefix(f.removed[1], "bracer-drill-shop-") {
		t.Errorf("removed: %v", f.removed)
	}
	if len(f.removedVols) != 2 || f.removedVols[0] != "oldvol" || f.removedVols[1] != f.removed[1] || len(f.volumes) != 0 {
		t.Errorf("volumes removed: %v, left: %v", f.removedVols, f.volumes)
	}
	if len(f.containers) != 0 {
		t.Errorf("containers left: %v", f.containers)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "leftover") {
		t.Errorf("warnings: %v", res.Warnings)
	}
	// create database from template0, pg_restore via stdin with -d.
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, `create database "shop" template template0`) || !strings.Contains(joined, "pg_restore -d dbname='shop' --no-data-for-failed-tables") {
		t.Errorf("calls:\n%s", joined)
	}
}

func TestDrillRowMismatchAndMissingTable(t *testing.T) {
	f := newFakeDocker()
	f.dumps["PGDMP-shop"] = []fakeTable{{"public", "orders", 480, false}, {"public", "events", 60_000, false}}
	st := newMemStore()
	seed(t, st, "")
	res, err := Run(context.Background(), baseCfg(), deps(f, st))
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("must fail")
	}
	dr := res.Instances[0].Databases[0]
	if dr.OK || dr.TablesFound != 2 || len(dr.MissingTables) != 1 || dr.MissingTables[0] != `"public"."empty"` {
		t.Errorf("db: %+v", dr)
	}
	if len(dr.RowMismatches) != 2 {
		t.Fatalf("mismatches: %+v", dr.RowMismatches)
	}
	if m := dr.RowMismatches[0]; m.Table != `"public"."orders"` || m.Expected != 500 || m.Found != 480 || m.Estimated {
		t.Errorf("exact mismatch: %+v", m)
	}
	if m := dr.RowMismatches[1]; m.Table != `"public"."events"` || !m.Estimated || m.Expected != 83_000 {
		t.Errorf("estimated mismatch: %+v", m)
	}
}

func TestWithinTolerance(t *testing.T) {
	for _, tc := range []struct {
		exp, found int64
		ok         bool
	}{
		{500, 480, true}, {500, 600, true}, {500, 601, false}, {0, 100, true}, {0, 101, false},
		{83_000, 90_000, true}, {83_000, 91_301, false}, {10_000, 9_000, true}, {10_000, 8_999, false},
	} {
		if got := restore.WithinTolerance(tc.exp, tc.found); got != tc.ok {
			t.Errorf("withinTolerance(%d, %d) = %v", tc.exp, tc.found, got)
		}
	}
}

func TestDrillEncrypted(t *testing.T) {
	kp, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "shop.key")
	if err := crypto.WriteIdentityFile(keyPath, kp); err != nil {
		t.Fatal(err)
	}
	st := newMemStore()
	seed(t, st, kp.Recipient)
	cfg := baseCfg()
	cfg.Encryption, cfg.AgeRecipient, cfg.AgeIdentityFile = "age", kp.Recipient, keyPath

	res, err := Run(context.Background(), cfg, deps(newFakeDocker(), st))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		b := new(strings.Builder)
		fmt.Fprintf(b, "%+v", res.Instances[0].Databases[0])
		t.Fatalf("encrypted drill must pass: %s globals=%+v", b, res.Instances[0].Globals)
	}

	// Different key → fails naming the recipient.
	other, _ := crypto.Generate()
	otherPath := filepath.Join(dir, "other.key")
	crypto.WriteIdentityFile(otherPath, other)
	cfg.AgeIdentityFile = otherPath
	res, err = Run(context.Background(), cfg, deps(newFakeDocker(), st))
	if err != nil {
		t.Fatal(err)
	}
	dr := res.Instances[0].Databases[0]
	if res.OK || dr.OK || !strings.Contains(dr.Error, kp.Recipient) || !strings.Contains(dr.Error, "no matching key") {
		t.Errorf("wrong key: %+v", dr)
	}

	// Unreadable key file → encrypted database fails, with a warning.
	cfg.AgeIdentityFile = filepath.Join(dir, "missing.key")
	res, _ = Run(context.Background(), cfg, deps(newFakeDocker(), st))
	if res.OK || len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "age_identity_file") {
		t.Errorf("missing key file: ok=%v warnings=%v", res.OK, res.Warnings)
	}
	_ = os.Remove(keyPath)
}

func TestDrillRestoreErrorStillCompares(t *testing.T) {
	f := newFakeDocker()
	st := newMemStore()
	seed(t, st, "")
	st.objects["bracer/shop/main/shop/2026-10-16T030000Z.dump"] = []byte("garbage")
	res, err := Run(context.Background(), baseCfg(), deps(f, st))
	if err != nil {
		t.Fatal(err)
	}
	dr := res.Instances[0].Databases[0]
	if res.OK || dr.OK || !strings.Contains(dr.Error, "pg_restore: ") || !strings.Contains(dr.Error, "valid archive") {
		t.Errorf("db: %+v", dr)
	}
	if len(dr.MissingTables) != 3 || dr.TablesFound != 0 {
		t.Errorf("must still report missing tables: %+v", dr)
	}
	if len(f.containers) != 0 {
		t.Error("container must be removed after failure")
	}
}

func TestDrillGlobalsOtherErrorDoesNotBlock(t *testing.T) {
	f := newFakeDocker()
	st := newMemStore()
	seed(t, st, "")
	st.objects["bracer/shop/main/globals/2026-10-16T030000Z.sql"] = []byte("CREATE ROLE postgres;\nBROKEN;\n")
	res, _ := Run(context.Background(), baseCfg(), deps(f, st))
	g := res.Instances[0].Globals
	if g.OK || g.IgnoredErrors != 1 || !strings.Contains(g.Error, "syntax error") {
		t.Errorf("globals: %+v", g)
	}
	if !res.Instances[0].Databases[0].OK || !res.OK {
		t.Errorf("globals error must not fail the database: %+v", res.Instances[0].Databases[0])
	}
}

func TestDrillKeepAndFilters(t *testing.T) {
	f := newFakeDocker()
	st := newMemStore()
	seed(t, st, "")
	d := deps(f, st)
	d.Keep = true
	res, err := Run(context.Background(), baseCfg(), d)
	if err != nil {
		t.Fatal(err)
	}
	if res.Instances[0].Container == "" || len(f.containers) != 1 {
		t.Errorf("--keep must leave the container: %+v", res.Instances[0])
	}

	d = deps(newFakeDocker(), st)
	d.Database = "nope"
	res, err = Run(context.Background(), baseCfg(), d)
	if err != nil || res.OK || res.Instances[0].Error != "no database dump to restore" {
		t.Errorf("--database filter: %v %+v", err, res.Instances[0])
	}

	d = deps(newFakeDocker(), st)
	d.Instance = "ghost"
	if _, err = Run(context.Background(), baseCfg(), d); err == nil || !strings.Contains(err.Error(), "not in app.yaml") {
		t.Errorf("--instance unknown: %v", err)
	}
}

func TestDrillNoBackupYet(t *testing.T) {
	_, err := Run(context.Background(), baseCfg(), deps(newFakeDocker(), newMemStore()))
	if err == nil || !strings.Contains(err.Error(), "run bracer backup first") {
		t.Errorf("err = %v", err)
	}
}

func TestDrillExplicitDatabasesSkipsOthers(t *testing.T) {
	st := newMemStore()
	seed(t, st, "")
	cfg := baseCfg()
	cfg.Instances[0].Databases = config.Databases{List: []config.Database{{Name: "other"}}}
	res, err := Run(context.Background(), cfg, deps(newFakeDocker(), st))
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || res.Instances[0].Error == "" || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "not in app.yaml, skipped") {
		t.Errorf("res: %+v warnings=%v", res.Instances[0], res.Warnings)
	}
}

func TestDrillNotReady(t *testing.T) {
	f := newFakeDocker()
	f.readyAfter = 1000
	st := newMemStore()
	seed(t, st, "")
	d := deps(f, st)
	d.ReadyTimeout = 5 * time.Second // the fake clock advances 1 s per Now()
	res, _ := Run(context.Background(), baseCfg(), d)
	ir := res.Instances[0]
	if ir.OK || !strings.Contains(ir.Error, "not ready after") || !strings.Contains(ir.Error, "fake log line") {
		t.Errorf("instance: %+v", ir)
	}
	if len(f.containers) != 0 {
		t.Error("container must be removed")
	}
}

func TestDrillStartFails(t *testing.T) {
	f := newFakeDocker()
	f.failRun = true
	st := newMemStore()
	seed(t, st, "")
	res, _ := Run(context.Background(), baseCfg(), deps(f, st))
	if res.OK || !strings.Contains(res.Instances[0].Error, "start postgres:17-alpine") {
		t.Errorf("instance: %+v", res.Instances[0])
	}
}

func TestChooseImage(t *testing.T) {
	cfg := baseCfg()
	in := cfg.Instances[0]
	if img, _ := chooseImage(cfg, in, &backup.InstanceManifest{PgVersion: "16.4"}, nil); img != "postgres:16-alpine" {
		t.Error(img)
	}
	if img, _ := chooseImage(cfg, in, &backup.InstanceManifest{PgVersion: "16.4", Databases: map[string]*backup.DatabaseManifest{"shop": {Latest: "t1", Dumps: []*backup.Dump{{TS: "t1", ToolVersion: "18.1"}}}}}, []string{"shop"}); img != "postgres:18-alpine" {
		t.Errorf("tool_version 18 > server 16: image = %q", img)
	}
	if img, _ := chooseImage(cfg, in, &backup.InstanceManifest{}, nil); img != "postgres:17-alpine" {
		t.Error(img)
	}
	// Common extension → the vendor's own image at the right version (spec drill § Flow 3a.2).
	postgis := &backup.InstanceManifest{PgVersion: "16.4", Databases: map[string]*backup.DatabaseManifest{"gis": {Latest: "t1", Dumps: []*backup.Dump{{TS: "t1", Extensions: []pg.Extension{{Name: "postgis", Version: "3.4.2"}, {Name: "uuid-ossp", Version: "1.1"}}}}}}}
	if img, err := chooseImage(cfg, in, postgis, []string{"gis"}); err != nil || img != "postgis/postgis:16-3.4" {
		t.Errorf("postgis: %q, %v", img, err)
	}
	cfg.Drill = &config.Drill{Image: "supabase/postgres:15"}
	if img, _ := chooseImage(cfg, in, &backup.InstanceManifest{PgVersion: "16.4"}, nil); img != "supabase/postgres:15" {
		t.Error(img)
	}
	in.PgMajor = 0
	if _, err := chooseImage(baseCfg(), in, &backup.InstanceManifest{}, nil); err == nil {
		t.Error("must fail without any version")
	}
}

func TestDrillDiskCheckAndLimits(t *testing.T) {
	// Short on space: fails before docker run.
	f := newFakeDocker()
	f.dfAvailKB = 0 // 0 KB free, dump 10 B × 5 = 50 B needed
	st := newMemStore()
	seed(t, st, "")
	res, err := Run(context.Background(), baseCfg(), deps(f, st))
	if err != nil {
		t.Fatal(err)
	}
	ir := res.Instances[0]
	if res.OK || !strings.Contains(ir.Error, "not enough disk space for a drill") || ir.Disk == nil || !ir.Disk.Checked || ir.Disk.FreeBytes != 0 || ir.Disk.NeedBytes != 50 {
		t.Errorf("disk: ok=%v err=%q disk=%+v", res.OK, ir.Error, ir.Disk)
	}
	if len(f.containers) != 0 || len(f.volumes) != 0 || len(f.removedVols) != 1 {
		t.Errorf("no container and no volume may remain when disk is short: %v %v removed=%v", f.containers, f.volumes, f.removedVols)
	}
	if ir.Disk.DataDirectory != "volume "+f.removedVols[0] {
		t.Errorf("disk must be measured through the drill volume: %+v", ir.Disk)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "run -d") {
			t.Errorf("docker run must not happen: %s", c)
		}
	}

	// --skip-disk-check: runs, warns; memory/cpus go into docker run.
	f = newFakeDocker()
	f.dfAvailKB = 0
	cfg := baseCfg()
	cfg.Drill = &config.Drill{Memory: "2g", CPUs: "1"}
	d := deps(f, st)
	d.SkipDiskCheck = true
	res, err = Run(context.Background(), cfg, d)
	if err != nil || !res.OK || !strings.Contains(strings.Join(res.Warnings, "\n"), "--skip-disk-check") {
		t.Errorf("skip: err=%v ok=%v warnings=%v", err, res.OK, res.Warnings)
	}
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, "--memory 2g --cpus 1 postgres:17-alpine") {
		t.Errorf("limits missing in docker run:\n%s", joined)
	}

	// Cannot measure: warns, still runs.
	f = newFakeDocker()
	f.dfAvailKB = -1
	res, err = Run(context.Background(), baseCfg(), deps(f, st))
	if err != nil || !res.OK || res.Instances[0].Disk == nil || res.Instances[0].Disk.Checked || !strings.Contains(strings.Join(res.Warnings, "\n"), "may fill the disk") {
		t.Errorf("unmeasurable: err=%v ok=%v disk=%+v warnings=%v", err, res.OK, res.Instances[0].Disk, res.Warnings)
	}
}

// The `postgres` dump is not restored straight into the existing `postgres` database
// (the image may have written into it) but into bracer_drill_postgres created from template0.
func TestDrillRestoresPostgresDBIntoFreshDatabase(t *testing.T) {
	f := newFakeDocker()
	st := newMemStore()
	seed(t, st, "")
	ctx := context.Background()
	key := "bracer/shop/main/postgres/2026-10-16T030000Z.dump"
	st.Put(ctx, key, strings.NewReader("PGDMP-shop"), 10)
	m, err := backup.LoadManifest(ctx, st, "bracer/shop/manifest.json", "shop")
	if err != nil {
		t.Fatal(err)
	}
	m.Instances["main"].Databases["postgres"] = &backup.DatabaseManifest{Latest: "2026-10-16T030000Z", Dumps: []*backup.Dump{{
		TS: "2026-10-16T030000Z", Key: key, Bytes: 10,
		Tables: []pg.Table{{Schema: "public", Name: "orders", Rows: i64(500)}, {Schema: "public", Name: "events", RowsEst: i64(83_000)}, {Schema: "public", Name: "empty", Rows: i64(0)}},
	}}}
	if err := backup.SaveManifest(ctx, st, "bracer/shop/manifest.json", m, time.Now()); err != nil {
		t.Fatal(err)
	}
	d := deps(f, st)
	d.Database = "postgres"
	res, err := Run(ctx, baseCfg(), d)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, `create database "bracer_drill_postgres" template template0`) || !strings.Contains(joined, "pg_restore -d dbname='bracer_drill_postgres'") {
		t.Fatalf("calls:\n%s", joined)
	}
	if strings.Contains(joined, "pg_restore -d dbname='postgres'") {
		t.Fatalf("must not restore into the image's postgres database:\n%s", joined)
	}
	if len(res.Instances) != 1 || len(res.Instances[0].Databases) != 1 {
		t.Fatalf("result: %+v", res)
	}
	dr := res.Instances[0].Databases[0]
	if dr.DB != "postgres" || dr.RestoredAs != "bracer_drill_postgres" || !dr.OK || dr.TablesFound != 3 {
		t.Fatalf("result: %+v", dr)
	}
}

// Extensions in the manifest: the image is chosen by the mapping table; missing in
// the image → the database fails before create database (spec drill § Flow 3a–3b).
func TestDrillChecksExtensionsBeforeRestore(t *testing.T) {
	st := newMemStore()
	seed(t, st, "")
	ctx := context.Background()
	m, err := backup.LoadManifest(ctx, st, "bracer/shop/manifest.json", "shop")
	if err != nil {
		t.Fatal(err)
	}
	for _, dp := range m.Instances["main"].Databases["shop"].Dumps {
		dp.Extensions = []pg.Extension{{Name: "postgis", Version: "3.4.2"}}
	}
	if err := backup.SaveManifest(ctx, st, "bracer/shop/manifest.json", m, time.Now()); err != nil {
		t.Fatal(err)
	}

	f := newFakeDocker()
	f.available = map[string]string{"postgis": "3.4.2", "plpgsql": "1.0"}
	res, err := Run(ctx, baseCfg(), deps(f, st))
	if err != nil || !res.OK {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if res.Instances[0].Image != "postgis/postgis:17-3.4" {
		t.Errorf("image = %q", res.Instances[0].Image)
	}

	f = newFakeDocker()
	f.available = map[string]string{"plpgsql": "1.0"}
	res, err = Run(ctx, baseCfg(), deps(f, st))
	if err != nil || res.OK {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	dr := res.Instances[0].Databases[0]
	if !strings.Contains(dr.Error, "needs extension postgis 3.4.2") || !strings.Contains(dr.Error, "postgis/postgis:17-3.4 does not have") || !strings.Contains(dr.Error, "drill.image") {
		t.Errorf("error = %q", dr.Error)
	}
	if len(f.createOpts) != 0 {
		t.Errorf("must not create a database: %q", f.createOpts)
	}

	f = newFakeDocker()
	f.available = map[string]string{"postgis": "3.5.0"}
	res, _ = Run(ctx, baseCfg(), deps(f, st))
	if !res.OK || len(res.Instances[0].Databases[0].Warnings) != 1 || !strings.Contains(res.Instances[0].Databases[0].Warnings[0], "postgis 3.5.0") {
		t.Errorf("version differ: %+v", res.Instances[0].Databases[0])
	}
}
