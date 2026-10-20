package drill

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"

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
	attrSQL     map[string]string // dump content → what pg_restore --create -n … prints
	attrFail    string            // psql -f - reports an ERROR for statements containing it
}

type fakeContainer struct {
	image      string
	dbs        map[string][]fakeTable
	createOpts []string // what follows TEMPLATE template0 in each CREATE DATABASE
	owner      map[string]string
	settings   map[string][]string // database → "role\x1fname=value"
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
		attrSQL:    map[string]string{"PGDMP-shop": "CREATE DATABASE shop WITH TEMPLATE = template0;\nALTER DATABASE shop OWNER TO postgres;\n"},
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
		if f.noDocker {
			return pg.Result{}, &pg.ExitError{Cmd: "docker info", Code: 1, Stderr: []byte("Cannot connect to the Docker daemon")}
		}
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
				if f.attrFail != "" && strings.Contains(line, f.attrFail) {
					stderr.WriteString("psql:<stdin>:2: ERROR:  unrecognized configuration parameter\n")
					continue
				}
				ct.applyAttr(strings.TrimSuffix(line, ";"))
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
		case sql == "select rolname from pg_roles order by rolname":
			write("postgres\nshop_app\n")
		case strings.HasPrefix(sql, "select pg_get_userbyid(datdba) from pg_database where datname = "):
			name := strings.Trim(strings.TrimPrefix(sql, "select pg_get_userbyid(datdba) from pg_database where datname = "), "'")
			if o := ct.owner[name]; o != "" {
				write(o + "\n")
			} else {
				write("postgres\n")
			}
		case strings.HasPrefix(sql, "select coalesce(r.rolname, ''), c from pg_db_role_setting"):
			name := sql[strings.LastIndex(sql, "= '")+3 : len(sql)-1]
			for _, kv := range ct.settings[name] {
				write(kv + "\n")
			}
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
		if len(args) > 0 && args[0] == "--create" {
			data, _ := io.ReadAll(c.Stdin)
			out, ok := f.attrSQL[string(data)]
			if !ok {
				return pg.Result{}, &pg.ExitError{Cmd: "pg_restore", Code: 1, Stderr: []byte("pg_restore: error: input file does not appear to be a valid archive")}
			}
			write(out)
			return pg.Result{}, nil
		}
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

var (
	reAttrOwner = regexp.MustCompile(`^ALTER DATABASE "([^"]+)" OWNER TO (\S+)$`)
	reAttrSet   = regexp.MustCompile(`^ALTER DATABASE "([^"]+)" SET (\S+) TO '([^']*)'$`)
)

// applyAttr stores an owner or a database setting the way the server would.
func (ct *fakeContainer) applyAttr(stmt string) {
	if ct.owner == nil {
		ct.owner, ct.settings = map[string]string{}, map[string][]string{}
	}
	if m := reAttrOwner.FindStringSubmatch(stmt); m != nil {
		ct.owner[m[1]] = m[2]
	}
	if m := reAttrSet.FindStringSubmatch(stmt); m != nil {
		ct.settings[m[1]] = append(ct.settings[m[1]], "\x1f"+strings.Trim(m[2], `"`)+"="+m[3])
	}
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

// depsFor: the fake clock advances 1 s per Now().
func depsFor(sh pg.Shell, st storage.Store) Deps {
	clock := time.Date(2026, 10, 17, 3, 0, 0, 0, time.UTC)
	return Deps{
		Shell: sh, Store: st,
		Tools: pg.NewTools(sh),
		Now:   func() time.Time { clock = clock.Add(time.Second); return clock },
		Rand:  func(n int) string { return strings.Repeat("a", n*2) },
		Poll:  time.Millisecond,
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

func TestDrillNoBackupYet(t *testing.T) {
	_, err := Run(context.Background(), baseCfg(), deps(newFakeDocker(), newMemStore()))
	if err == nil || !strings.Contains(err.Error(), "run bracer backup first") {
		t.Errorf("err = %v", err)
	}
}

// The `postgres` dump is not restored straight into the existing `postgres` database
// (the image may have written into it) but into bracer_drill_postgres created from template0.
// Extensions in the manifest: the image is chosen by the mapping table; missing in
// the image → the database fails before create database (spec drill § Flow 3a–3b).
// spec drill AC: drill.runtime: none → UsageError before anything, --runtime docker overrides.
func TestDrillOff(t *testing.T) {
	st := newMemStore()
	seed(t, st, "")
	cfg := baseCfg()
	cfg.Drill = &config.Drill{Runtime: config.DrillRuntimeNone}
	f := newFakeDocker()
	_, err := Run(context.Background(), cfg, deps(f, st))
	var ue *UsageError
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), "drill is off for app shop (drill.runtime: none)") || len(f.calls) != 0 {
		t.Fatalf("off: err=%v calls=%v", err, f.calls)
	}
	if _, err := DryRun(context.Background(), cfg, deps(f, st)); !errors.As(err, &ue) {
		t.Errorf("dry-run off: %v", err)
	}
	d := deps(f, st)
	d.ForceDocker = true
	// The seeded backup predates ADR-0015: drill runs, and says why it cannot drill it.
	if res, err := Run(context.Background(), cfg, d); err != nil || res.OK || res.Instances[0].Error != "the latest backup has no environment record; run bracer backup with this version first" {
		t.Errorf("--runtime docker: %v %+v", err, res)
	}
}

// spec drill AC: a dump from a newer major than pg_major fails before any container;
// the result names the system_id of the dumps tried.
// spec drill AC "--dry-run": no container; no backup yet is not a failure; Docker or key problems fail with a Fix.
func TestDrillDryRun(t *testing.T) {
	f := newFakeDocker()
	res, err := DryRun(context.Background(), baseCfg(), deps(f, newMemStore()))
	if err != nil || !res.OK || len(res.Checks) != 2 || res.Checks[1].Detail != "no backup yet" {
		t.Fatalf("empty bucket: %v %+v", err, res)
	}
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "info") {
			t.Errorf("dry-run ran %s", c)
		}
	}

	st := newMemStore()
	seed(t, st, "")
	f.noDocker = true
	res, _ = DryRun(context.Background(), baseCfg(), deps(f, st))
	if res.OK || res.Checks[0].OK || res.Checks[0].Fix != "bracer check" || !strings.Contains(res.Checks[1].Detail, "shop 2026-10-16T030000Z") {
		t.Errorf("no docker: %+v", res.Checks)
	}

	cfg := baseCfg()
	cfg.Encryption, cfg.AgeRecipient, cfg.AgeIdentityFile = "age", "age1other", "/keys/shop.key"
	d := deps(newFakeDocker(), st)
	d.ReadIdentities = func(string) ([]age.Identity, []string, error) { return nil, []string{"age1mine"}, nil }
	res, _ = DryRun(context.Background(), cfg, d)
	if res.OK || res.Checks[1].Name != "age key" || !strings.Contains(res.Checks[1].Detail, "no key for age_recipient age1other") {
		t.Errorf("key: %+v", res.Checks)
	}
}

// spec fetch-restore AC (drill): the temporary database gets the archive's
// owner and settings, read back with restore's code; a setting that does not
// apply fails the database and names it.
// ADR-0014: the temporary Postgres preloads the libraries of the extensions the
// databases use (from the manifest), not the source's other libraries.
