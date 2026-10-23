package drill

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
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
	if tool == "psql" && hasArg(args, "-0") && c.Stdout != nil {
		c.Stdout = psqlRows{c.Stdout}
	}
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

// The app's folder in the bucket, the place that writes it (ADR-0023) and
// the run the tests drill.
const (
	testFolder = "shop-7f3k2q"
	testRoot   = "bracer/" + testFolder + "/"
	testTS     = "2026-10-16T030000Z"
)

var here = backup.Writer{ID: "1111222233334444", Hostname: "vps-1", Path: "/home/deploy/.config/bracer/shop"}

// seed writes the run testTS of app shop to the bucket the way backup lays it
// out (ADR-0026): app.yaml, then for instance main the dump of database shop
// (tables orders rows 500, events rows_est 83000, empty rows 0), globals and
// the environment file, then run.json. It returns the run as drill gets it
// from backup: loaded back, keys filled in.
func seed(t *testing.T, st *memStore, recipient string) *backup.Run {
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
	enc := recipient != ""
	var rp *string
	if enc {
		rp = &recipient
	}
	ext := ""
	if enc {
		ext = ".age"
	}
	appYAML, err := config.Marshal(baseCfg())
	if err != nil {
		t.Fatal(err)
	}
	env, err := json.Marshal(&pg.Env{Schema: pg.EnvSchema, SourceKind: "container", ServerVersionNum: 170006, ServerVersion: "17.6", BootstrapSuperuser: "postgres", Superuser: true})
	if err != nil {
		t.Fatal(err)
	}
	dir := backup.RunDir(testRoot, testTS)
	put(dir+"app.yaml"+ext, string(appYAML))
	put(dir+"main/shop.dump"+ext, "PGDMP-shop")
	put(dir+"main/globals.sql"+ext, "CREATE ROLE postgres;\nCREATE ROLE app;\n")
	put(dir+"main/env.json"+ext, string(env))
	run := &backup.Run{App: "shop", TS: testTS, Writer: here,
		Backup:  backup.Outcome{Result: backup.ResultOK, Reasons: []string{}},
		AppYAML: &backup.Dump{Bytes: int64(len(appYAML))},
		Instances: map[string]*backup.RunInstance{"main": {
			Kind: "container", PgVersion: "17.6", ToolVersion: "17.6", SystemID: config.SystemIDNone, Encrypted: enc, Recipient: rp, Missing: []string{},
			Env:     &backup.Dump{Bytes: int64(len(env))},
			Globals: &backup.Dump{Bytes: 42, HasPasswords: true},
			Databases: map[string]*backup.Dump{"shop": {
				Bytes:  10,
				Locale: &pg.Locale{Encoding: "UTF8", Collate: "C.UTF-8", Ctype: "C.UTF-8"},
				Tables: []pg.Table{
					{Schema: "public", Name: "orders", Rows: i64(500)},
					{Schema: "public", Name: "events", RowsEst: i64(83_000)},
					{Schema: "public", Name: "empty", Rows: i64(0)},
				},
			}},
		}},
	}
	data, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	st.Put(ctx, backup.RunKey(testRoot, testTS), bytes.NewReader(data), int64(len(data)))
	run, err = backup.LoadRun(ctx, st, testRoot, testTS)
	if err != nil {
		t.Fatal(err)
	}
	// The keys the loaded run points at must be the objects written above:
	// the layout the test assumes is the one backup writes.
	ri := run.Instances["main"]
	for _, dp := range []*backup.Dump{run.AppYAML, ri.Env, ri.Globals, ri.Databases["shop"]} {
		if dp.Key == "" || dp.Encrypted != enc {
			t.Fatalf("loaded run: key %q encrypted %v", dp.Key, dp.Encrypted)
		}
		if _, ok := st.objects[dp.Key]; !ok {
			t.Fatalf("loaded run points at %s, which was not written", dp.Key)
		}
	}
	return run
}

func baseCfg() *config.App {
	return &config.App{
		App:         "shop",
		SecretsFile: "/home/deploy/.config/bracer/shop/secrets.env",
		Instances:   []config.Instance{{Name: "main", Connection: config.Connection{Container: "shop-db", User: "postgres", PgMajor: 17, SystemID: config.SystemIDNone}, Databases: config.Databases([]config.Database{{Name: "shop"}})}},
		Storage:     &config.Storage{Endpoint: "https://s3.example.com", Bucket: "b", Folder: testFolder, AccessKeyEnv: "A", SecretKeyEnv: "S"},
		Encryption:  "none",
		Schedule:    &config.Schedule{Every: config.EveryDaily, At: []string{"03:00"}, Days: []string{}, Keep: 7},
		Runtime:     &config.Runtime{User: "deploy", Path: "/usr/bin:/bin", Docker: "/usr/bin/docker"},
	}
}

// withSide adds a second instance, side, to cfg, with drill off when off.
func withSide(cfg *config.App, off bool) *config.App {
	side := cfg.Instances[0]
	side.Name, side.Container = "side", "side-db"
	side.Drill = nil
	side.SetDrillOff(off)
	cfg.Instances = append(cfg.Instances, side)
	return cfg
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

// containersStarted counts the `docker run` calls that start a container.
func containersStarted(f *fakeDocker) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, "run ") && !strings.Contains(c, " df ") {
			n++
		}
	}
	return n
}

// backupResult is the backup's result for cfg, every instance dumped, the
// way backup.Run hands it to the drill step.
func backupResult(cfg *config.App) *backup.Result {
	res := &backup.Result{Kind: "backup", App: cfg.App, TS: testTS, OK: true}
	for _, in := range cfg.Instances {
		res.Instances = append(res.Instances, &backup.InstanceResult{Name: in.Name, OK: true, Missing: []string{}})
	}
	return res
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

// spec backup § Drill: every instance with drill.runtime: none → nothing is
// drilled: no Docker, a result with no instance, and run.json records off.
func TestDrillOff(t *testing.T) {
	cfg := baseCfg()
	cfg.Instances[0].SetDrillOff(true)
	st := newMemStore()
	run := seed(t, st, "")
	f := newFakeDocker()
	res, err := Run(context.Background(), cfg, run, deps(f, st))
	if err != nil || res == nil || !res.OK || len(res.Instances) != 0 || len(f.calls) != 0 {
		t.Fatalf("off: res=%+v err=%v calls=%v", res, err, f.calls)
	}

	bres := backupResult(cfg)
	Step(cfg, deps(f, st), nil)(context.Background(), run, bres)
	d := run.Instances["main"].Drill
	if d == nil || d.Result != backup.DrillOff || len(d.Reasons) != 0 || !d.At.IsZero() {
		t.Errorf("run.json drill: %+v", d)
	}
	if ir := bres.Instance("main"); ir.DrillFailed != nil || ir.Drill != nil || len(f.calls) != 0 {
		t.Errorf("backup result: %+v calls=%v", ir, f.calls)
	}
}

// Docker not answering is the one error Run returns: no instance could start,
// so the result has none, and the step fails every instance that is on with it.
func TestDrillNoDocker(t *testing.T) {
	st := newMemStore()
	run := seed(t, st, "")
	f := newFakeDocker()
	f.noDocker = true
	cfg := withSide(baseCfg(), true)
	res, err := Run(context.Background(), cfg, run, deps(f, st))
	if err == nil || !strings.HasPrefix(err.Error(), "Docker is not available for the temporary Postgres: ") || !strings.Contains(err.Error(), "Cannot connect to the Docker daemon") || !strings.HasSuffix(err.Error(), "\nFix: bracer remove shop, then bracer init shop") {
		t.Fatalf("err = %v", err)
	}
	if res == nil || res.OK || len(res.Instances) != 0 || res.TS != testTS || res.App != "shop" {
		t.Fatalf("res = %+v", res)
	}

	run.Instances["side"] = &backup.RunInstance{Kind: "container", Missing: []string{}, Databases: map[string]*backup.Dump{}}
	bres := backupResult(cfg)
	Step(cfg, deps(f, st), nil)(context.Background(), run, bres)
	d := run.Instances["main"].Drill
	if d == nil || d.Result != backup.DrillFailed || len(d.Reasons) != 1 || d.Reasons[0] != err.Error() || d.At.IsZero() {
		t.Errorf("run.json drill: %+v", d)
	}
	if ir := bres.Instance("main"); len(ir.DrillFailed) != 1 || ir.DrillFailed[0] != err.Error() || ir.Drill != nil {
		t.Errorf("backup result: %+v", ir)
	}
	if d := run.Instances["side"].Drill; d == nil || d.Result != backup.DrillOff {
		t.Errorf("side: %+v", d)
	}
	if bres.Instance("side").DrillFailed != nil {
		t.Errorf("side must not fail: %+v", bres.Instance("side"))
	}
}

// ADR-0016, ADR-0019: the run must hold every database its own app.yaml
// names; one that does not fails the instance before any container, naming
// what is missing. Whether backup recorded it in run.json or the run's
// app.yaml names a database the run has no dump of, drilling the data that
// is there would pass on a backup that is not whole.
func TestDrillRunIncomplete(t *testing.T) {
	const want = "the run is missing auth; nothing to test-restore"
	for name, prepare := range map[string]func(t *testing.T, st *memStore, run *backup.Run){
		"recorded in run.json": func(t *testing.T, st *memStore, run *backup.Run) {
			run.Instances["main"].Missing = []string{"auth"}
		},
		"named by the run's app.yaml": func(t *testing.T, st *memStore, run *backup.Run) {
			runCfg := baseCfg()
			runCfg.Instances[0].Databases = []config.Database{{Name: "shop"}, {Name: "auth"}}
			data, err := config.Marshal(runCfg)
			if err != nil {
				t.Fatal(err)
			}
			st.Put(context.Background(), run.AppYAML.Key, bytes.NewReader(data), int64(len(data)))
		},
	} {
		t.Run(name, func(t *testing.T) {
			st := newMemStore()
			run := seed(t, st, "")
			prepare(t, st, run)
			f := newFakeDocker()
			res, err := Run(context.Background(), baseCfg(), run, deps(f, st))
			if err != nil || res.OK || len(res.Instances) != 1 {
				t.Fatalf("err = %v, res = %+v", err, res)
			}
			ir := res.Instances[0]
			if ir.Name != "main" || ir.Result != backup.DrillFailed || ir.OK || ir.Error != want || len(ir.Databases) != 0 {
				t.Errorf("instance: %+v", ir)
			}
			if containersStarted(f) != 0 {
				t.Errorf("no container may start: %v", f.calls)
			}
		})
	}
}

// ADR-0019: the run has nothing of the instance (its backup failed): drill
// fails it instead of passing an older run.
func TestDrillRunLacksInstance(t *testing.T) {
	st := newMemStore()
	run := seed(t, st, "")
	delete(run.Instances, "main")
	f := newFakeDocker()
	res, err := Run(context.Background(), baseCfg(), run, deps(f, st))
	if err != nil || res.OK || len(res.Instances) != 1 {
		t.Fatalf("err = %v, res = %+v", err, res)
	}
	ir := res.Instances[0]
	if ir.Name != "main" || ir.Result != backup.DrillFailed || ir.Error != "the run has nothing of this instance" || containersStarted(f) != 0 {
		t.Errorf("instance: %+v calls=%v", ir, f.calls)
	}
}

// ADR-0019: an instance with drill off, or one the caller leaves out (the
// preflight of init), is not in the result; the others are drilled.
func TestDrillSkipsInstanceOff(t *testing.T) {
	st := newMemStore()
	run := seed(t, st, "")
	run.Instances["main"].Missing = []string{"auth"} // main fails fast, without a container
	cfg := withSide(baseCfg(), true)
	f := newFakeDocker()
	res, err := Run(context.Background(), cfg, run, deps(f, st))
	if err != nil || res.OK || len(res.Instances) != 1 || res.Instances[0].Name != "main" || res.Instance("side") != nil {
		t.Fatalf("drill off: err = %v, res = %+v", err, res)
	}

	d := deps(f, st)
	d.SkipInstances = []string{"main"}
	f.calls = nil
	res, err = Run(context.Background(), cfg, run, d)
	if err != nil || !res.OK || len(res.Instances) != 0 || len(f.calls) != 0 {
		t.Fatalf("skipped: err = %v, res = %+v calls=%v", err, res, f.calls)
	}
}

// ADR-0026: the drill step records the outcome of each instance where the
// backup keeps it: run.json (result, when, how long, why) and the backup's
// result (what fails the run, the drill's own detail for --json), and hands
// its own result to the caller.
func TestStep(t *testing.T) {
	st := newMemStore()
	run := seed(t, st, "")
	run.Instances["main"].Missing = []string{"auth"}
	run.Instances["side"] = &backup.RunInstance{Kind: "container", Missing: []string{}, Databases: map[string]*backup.Dump{}}
	cfg := withSide(baseCfg(), true)
	f := newFakeDocker()
	f.orphans = []string{"deadbeef"}
	var kept *Result
	bres := backupResult(cfg)
	Step(cfg, deps(f, st), func(r *Result) { kept = r })(context.Background(), run, bres)

	if kept == nil || kept.Kind != "drill" || kept.App != "shop" || kept.TS != testTS || kept.OK || len(kept.Instances) != 1 {
		t.Fatalf("kept: %+v", kept)
	}
	if !kept.FinishedAt.After(kept.StartedAt) {
		t.Errorf("times: %v %v", kept.StartedAt, kept.FinishedAt)
	}
	x := kept.Instance("main")
	d := run.Instances["main"].Drill
	if d == nil || d.Result != backup.DrillFailed || !d.At.Equal(kept.StartedAt) || d.TookSeconds != x.TookSeconds || d.TookSeconds <= 0 {
		t.Errorf("run.json main: %+v", d)
	}
	if want := []string{"the run is missing auth; nothing to test-restore"}; !equalStrings(d.Reasons, want) {
		t.Errorf("reasons: %v", d.Reasons)
	}
	ir := bres.Instance("main")
	if !equalStrings(ir.DrillFailed, d.Reasons) || ir.Drill != x {
		t.Errorf("backup result main: failed=%v drill=%+v", ir.DrillFailed, ir.Drill)
	}
	if !equalStrings(bres.Failures(), []string{"main: drill: the run is missing auth; nothing to test-restore"}) {
		t.Errorf("failures: %v", bres.Failures())
	}
	if s := run.Instances["side"].Drill; s == nil || s.Result != backup.DrillOff {
		t.Errorf("run.json side: %+v", s)
	}
	if side := bres.Instance("side"); side.DrillFailed != nil || side.Drill != nil {
		t.Errorf("backup result side: %+v", side)
	}
	// What the drill warns about reaches the backup's result.
	if want := []string{"removed 1 leftover drill container(s) and 0 volume(s) from a previous run"}; !equalStrings(bres.Warnings, want) || !equalStrings(kept.Warnings, want) {
		t.Errorf("warnings: backup %v drill %v", bres.Warnings, kept.Warnings)
	}
	if !equalStrings(f.removed, []string{"deadbeef"}) {
		t.Errorf("orphans removed: %v", f.removed)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// psqlRows turns the newline-ended rows the fakes print into what psql -0
// prints: each row ended by a NUL. The session marker line keeps its newline.
type psqlRows struct{ w io.Writer }

func (p psqlRows) Write(b []byte) (int, error) {
	if bytes.HasPrefix(b, []byte("__BRACER_END__")) || bytes.IndexByte(b, 0) >= 0 {
		return p.w.Write(b)
	}
	if _, err := p.w.Write(bytes.ReplaceAll(b, []byte("\n"), []byte{0})); err != nil {
		return 0, err
	}
	return len(b), nil
}

func hasArg(args []string, a string) bool {
	for _, x := range args {
		if x == a {
			return true
		}
	}
	return false
}
