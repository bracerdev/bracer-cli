package backup

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/crypto"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/storage"
)

// fakeRunner fakes psql/pg_dump/pg_dumpall by tool and SQL statement.
type fakeRunner struct {
	dbs        []string
	extraDBs   []string          // on the server but not in app.yaml
	payload    map[string]string // db → pg_dump contents
	failDump   string            // db whose pg_dump fails
	notSuper   bool              // the connected user is not a superuser (default: it is)
	cfuncs     string            // rows of the C functions query (probin, function)
	colls      string            // rows of the collations query
	readAll    bool              // pg_read_all_data
	sysid      string            // system_identifier; empty: pg_control_system() fails (managed)
	noSettings bool              // shared_preload_libraries unreadable (no pg_read_all_settings)
	calls      []string
}

func (f *fakeRunner) Describe() string { return "fake runner" }

func (f *fakeRunner) Run(_ context.Context, conn pg.Conn, tool string, args []string, stdin io.Reader, stdout io.Writer) error {
	f.calls = append(f.calls, tool+" "+strings.Join(args, " ")+" db="+conn.Database)
	if tool == "psql" && hasArg(args, "-0") {
		stdout = psqlRows{stdout}
	}
	switch tool {
	case "psql":
		if args[len(args)-1] == "-" { // -f - session: reads statements one by one from stdin
			return f.session(conn, stdin, stdout)
		}
		sql := args[len(args)-1]
		switch {
		case strings.HasPrefix(sql, "select 1"):
			io.WriteString(stdout, "1\n")
		case sql == "show server_version_num":
			io.WriteString(stdout, "170011\n")
		case strings.HasPrefix(sql, "show server_version"):
			io.WriteString(stdout, "17.11\n")
		// Environment record (ADR-0015).
		case strings.HasPrefix(sql, "select (select rolname from pg_roles where oid = 10)"):
			su := "f"
			if !f.notSuper {
				su = "t"
			}
			io.WriteString(stdout, "postgres\x1fbackup\x1f"+su+"\x1foff\x1f100\x1f\n")
		case strings.HasPrefix(sql, "select name, setting, coalesce(unit, '')"):
			io.WriteString(stdout, "DateStyle\x1fISO, MDY\x1f\x1fstring\x1fconfiguration file\x1fISO, MDY\x1fuser\n")
		case strings.HasPrefix(sql, "select lower(name), coalesce(setting, '') from pg_file_settings"):
			if !strings.Contains(sql, "auto.conf") {
				io.WriteString(stdout, "datestyle\x1fiso, mdy\n")
			}
		case strings.HasPrefix(sql, "select lower(name) from pg_settings"),
			strings.HasPrefix(sql, "select spcname"),
			strings.HasPrefix(sql, "select 'function '"),
			strings.HasPrefix(sql, "select 'subscription '"),
			strings.HasPrefix(sql, "select td.oid::regdictionary"):
		case strings.HasPrefix(sql, "select p.probin"):
			io.WriteString(stdout, f.cfuncs)
		case strings.HasPrefix(sql, "with uses as"):
			io.WriteString(stdout, f.colls)
		case strings.HasPrefix(sql, "select encode(sha256(pg_read_binary_file"):
			io.WriteString(stdout, "5ea1\n")
		case strings.HasPrefix(sql, "select r.n, f.n from"):
			io.WriteString(stdout, "412\x1f37\n")
		case strings.HasPrefix(sql, "select (select count(*) from pg_class"):
			io.WriteString(stdout, "0\n")
		case sql == "show shared_preload_libraries":
			if f.noSettings {
				return &pg.ExitError{Cmd: "psql", Code: 1, Stderr: []byte(`ERROR:  permission denied to examine "shared_preload_libraries"`)}
			}
			io.WriteString(stdout, "pg_stat_statements,timescaledb\n")
		case strings.HasPrefix(sql, "select datname"):
			for _, n := range append(append([]string{"template1"}, f.dbs...), f.extraDBs...) {
				io.WriteString(stdout, n+"\n")
			}
		case strings.HasPrefix(sql, "select current_user"):
			su, ra := "f", "f"
			if !f.notSuper {
				su = "t"
			}
			if f.readAll {
				ra = "t"
			}
			io.WriteString(stdout, "backup\x1f"+su+"\x1f"+ra+"\x1ft\n")
		case strings.HasPrefix(sql, "select n.nspname, c.relname"):
			io.WriteString(stdout, "public\x1forders\x1f14302\npublic\x1fnew_table\x1f-1\n")
		case strings.HasPrefix(sql, "select pg_encoding_to_char(encoding), datcollate, datctype"):
			io.WriteString(stdout, "UTF8\x1fC.UTF-8\x1fC.UTF-8\x1fc\x1f\x1f\n")
		case strings.HasPrefix(sql, "select extname, extversion"):
			if conn.Database != "template1" {
				io.WriteString(stdout, "postgis\x1f3.4.2\x1fpublic\x1ft\nuuid-ossp\x1f1.1\x1fpublic\x1ff\n")
			}
		case strings.HasPrefix(sql, "select system_identifier"):
			if f.sysid == "" {
				return &pg.ExitError{Cmd: "psql", Code: 1, Stderr: []byte("ERROR:  permission denied for function pg_control_system")}
			}
			io.WriteString(stdout, f.sysid+"\n")
		default:
			return fmt.Errorf("fake: unexpected sql %q", sql)
		}
	case "pg_dump":
		if len(args) == 1 && args[0] == "--version" {
			io.WriteString(stdout, "pg_dump (PostgreSQL) 17.11\n")
			return nil
		}
		if slices.Contains(args, "--schema-only") {
			io.WriteString(stdout, "PGDMP-schema")
			return nil
		}
		if !slices.Contains(args, "--snapshot="+fakeSnapshotID) {
			return fmt.Errorf("fake: pg_dump without --snapshot: %v", args)
		}
		if conn.Database == f.failDump {
			return &pg.ExitError{Cmd: "pg_dump", Code: 1, Stderr: []byte("pg_dump: error: query would be affected by row-level security policy")}
		}
		io.WriteString(stdout, f.payload[conn.Database])
	case "pg_dumpall":
		io.WriteString(stdout, "CREATE ROLE app;\n")
	default:
		return fmt.Errorf("fake: unexpected tool %s", tool)
	}
	return nil
}

const fakeSnapshotID = "00000003-0000001B-1"

// session fakes psql -f -: answers each statement, prints the __BRACER_END__ marker after each \echo.
// Tables: orders reltuples 14302 / count 14300; new_table -1 / count 7; events reltuples 5e9.
func (f *fakeRunner) session(conn pg.Conn, stdin io.Reader, stdout io.Writer) error {
	sc := bufio.NewScanner(stdin)
	lastErr := ""
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, `\echo `) {
			flag := "false"
			if lastErr != "" {
				flag = "true"
			}
			fmt.Fprintf(stdout, "__BRACER_END__ %s %s\n", flag, lastErr)
			continue
		}
		sql := strings.TrimSuffix(line, ";")
		f.calls = append(f.calls, "session: "+sql+" db="+conn.Database)
		lastErr = ""
		switch {
		case sql == "select 1":
			io.WriteString(stdout, "1\n")
		case strings.HasPrefix(sql, "begin"), sql == "commit", strings.HasPrefix(sql, "set "):
		case sql == "select pg_export_snapshot()":
			io.WriteString(stdout, fakeSnapshotID+"\n")
		case strings.HasPrefix(sql, "select n.nspname, c.relname"):
			io.WriteString(stdout, "public\x1fevents\x1f5000000000\x1fr\npublic\x1fmeasurements\x1f-1\x1fp\npublic\x1fnew_table\x1f-1\x1fr\npublic\x1forders\x1f14302\x1fr\n")
		case sql == `select count(*) from "public"."orders"`:
			io.WriteString(stdout, "14300\n")
		case sql == `select count(*) from "public"."new_table"`:
			io.WriteString(stdout, "7\n")
		case sql == `select count(*) from "public"."events"`:
			io.WriteString(stdout, "4999999999\n")
		default:
			lastErr = "ERROR:  fake: unexpected statement " + sql
		}
	}
	return nil
}

// fakeRows is what the fake's shop database totals: events (estimated 5e9),
// new_table (7) and orders (14300); measurements is a partitioned parent.
const fakeRows = 5000000000 + 7 + 14300

// root is <prefix>/<folder>/ of baseCfg.
const root = "bracer/shop-abc123/"

// memStore is an in-memory bucket that records the order of writes and deletes.
type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	failPut string   // Put fails for keys containing this string
	puts    []string // keys written, in order
	deleted []string // keys deleted, in order
}

func newMemStore() *memStore { return &memStore{objects: map[string][]byte{}} }

func (m *memStore) Check(context.Context) error { return nil }
func (m *memStore) Describe() string            { return "mem" }
func (m *memStore) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if m.failPut != "" && strings.Contains(key, m.failPut) {
		return errors.New("403 Forbidden")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = data
	m.puts = append(m.puts, key)
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
func (m *memStore) List(_ context.Context, prefix string) ([]storage.Object, error) {
	var out []storage.Object
	for k, v := range m.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, storage.Object{Key: k, Size: int64(len(v))})
		}
	}
	return out, nil
}
func (m *memStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	m.deleted = append(m.deleted, key)
	return nil
}

// newest loads the newest run.json of the folder: the state of the app.
func (m *memStore) newest(t *testing.T) *Run {
	t.Helper()
	r, _, err := Newest(context.Background(), m, root)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// keysOf lists the keys under the run folder ts, sorted.
func (m *memStore) keysOf(ts string) []string {
	var out []string
	for k := range m.objects {
		if strings.HasPrefix(k, RunDir(root, ts)) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// putRun writes r as the run.json of its ts under root and returns it as
// LoadRun reads it back (keys hydrated).
func putRun(t *testing.T, st *memStore, r *Run) *Run {
	t.Helper()
	r.Schema = runSchema
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	st.objects[RunKey(root, r.TS)] = data
	loaded, err := LoadRun(context.Background(), st, root, r.TS)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

// oldRun puts an ended run of shop at ts into the bucket: the dump and a
// run.json written by here, ok, drill off, orders with rows rows. edit
// changes the run before it is written.
func oldRun(t *testing.T, st *memStore, ts string, rows int64, edit ...func(*Run)) *Run {
	t.Helper()
	key := dumpKey(root, ts, "main", "shop", false)
	st.objects[key] = []byte("old " + ts)
	sum := sha256.Sum256(st.objects[key])
	r := &Run{
		App: "shop", TS: ts, Writer: here,
		Backup: Outcome{Result: ResultOK, Reasons: []string{}},
		Instances: map[string]*RunInstance{"main": {
			Kind: "container", Missing: []string{}, Drill: &DrillOutcome{Result: DrillOff},
			Databases: map[string]*Dump{"shop": {Bytes: int64(len(st.objects[key])), SHA256: hex.EncodeToString(sum[:]), Tables: []pg.Table{{Schema: "public", Name: "orders", Rows: &rows}}}},
		}},
	}
	for _, e := range edit {
		e(r)
	}
	return putRun(t, st, r)
}

// baseCfg backs up dbs (default: shop) of one container instance, keeping
// every run of the last 2 days; a test that needs another keep sets it.
func baseCfg(dbs ...string) *config.App {
	if len(dbs) == 0 {
		dbs = []string{"shop"}
	}
	var list []config.Database
	for _, n := range dbs {
		list = append(list, config.Database{Name: n})
	}
	return &config.App{
		App:        "shop",
		Instances:  []config.Instance{{Name: "main", Connection: config.Connection{Container: "shop-db", User: "postgres", PgMajor: 17, SystemID: config.SystemIDNone}, Databases: config.Databases(list)}},
		Storage:    &config.Storage{Endpoint: "https://s3.example.com", Bucket: "b", Folder: "shop-abc123", AccessKeyEnv: "A", SecretKeyEnv: "S"},
		Encryption: "none",
		Schedule:   &config.Schedule{Every: config.EveryDaily, At: []string{"03:00"}, Days: []string{}, Keep: 2},
	}
}

// here is the place the tests run from; deps creates its folder when missing.
var here = Writer{ID: "1111222233334444", Hostname: "vps-1", Path: "/home/deploy/.config/bracer/shop"}

func deps(st storage.Store, r pg.Runner) Deps {
	clock := time.Date(2026, 10, 16, 3, 0, 0, 0, time.UTC)
	return Deps{
		Store:  st,
		Here:   here,
		Create: true,
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		// The calendar in UTC here; TestKeepSet covers another zone.
		Location: time.UTC,
		Now: func() time.Time {
			clock = clock.Add(time.Second)
			return clock
		},
		Connect: func(context.Context, *pg.Tools, config.Instance, *config.Secrets) (pg.Conn, pg.Runner, error) {
			return pg.Conn{User: "postgres", Database: "postgres"}, r, nil
		},
	}
}

// fakeDrill is a drill step that gives every instance the result, failing
// the instance in the result when the drill failed.
func fakeDrill(result string, reasons ...string) DrillFunc {
	return func(_ context.Context, run *Run, res *Result) {
		for name, ri := range run.Instances {
			ri.Drill = &DrillOutcome{Result: result, At: time.Date(2026, 10, 16, 3, 5, 0, 0, time.UTC), TookSeconds: 12.5, Reasons: reasons}
			if result == DrillFailed {
				res.Instance(name).DrillFailed = reasons
			}
		}
	}
}

func TestBackupPlainDumpsAndRunJSON(t *testing.T) {
	st := newMemStore()
	r := &fakeRunner{dbs: []string{"postgres", "shop"}, payload: map[string]string{"postgres": "PGDMP-postgres", "shop": "PGDMP-shop"}}
	res, err := Backup(context.Background(), baseCfg(r.dbs...), deps(st, r))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Status != ResultOK || len(res.Instances) != 1 || !res.Instances[0].OK || len(res.Instances[0].Databases) != 2 || len(res.Instances[0].Missing) != 0 || res.Instances[0].Missing == nil {
		b, _ := json.MarshalIndent(res, "", " ")
		t.Fatalf("result:\n%s", b)
	}
	// Every object of the run is under one folder <root><ts>/ (ADR-0026).
	var dumpKeys, globalKeys int
	for k, v := range st.objects {
		if !strings.HasPrefix(k, RunDir(root, res.TS)) {
			t.Errorf("object outside the run folder: %s", k)
		}
		switch {
		case strings.HasSuffix(k, ".dump"):
			dumpKeys++
			if !strings.HasPrefix(k, RunDir(root, res.TS)+"main/") || !strings.HasPrefix(string(v), "PGDMP-") {
				t.Errorf("object %s = %q", k, v)
			}
		case strings.HasSuffix(k, "/main/globals.sql"):
			globalKeys++
		}
	}
	if dumpKeys != 2 || globalKeys != 1 {
		t.Errorf("dumps=%d globals=%d (globals must run once per instance)", dumpKeys, globalKeys)
	}
	// run.json ends the run: the last object written.
	if last := st.puts[len(st.puts)-1]; last != RunKey(root, res.TS) {
		t.Errorf("last object written = %s, want run.json", last)
	}
	run := st.newest(t)
	if run.TS != res.TS || run.App != "shop" || run.Writer != here || run.Verdict() != ResultOK || run.Backup.Result != ResultOK || len(run.Backup.Reasons) != 0 || run.Backup.Fix != "" || run.Backup.StartedAt != res.StartedAt || run.Backup.FinishedAt != res.FinishedAt {
		t.Fatalf("run.json: %+v", run)
	}
	if run.AppYAML == nil || run.AppYAML.Key != appYAMLKey(root, run.TS, false) || !bytes.Contains(st.objects[run.AppYAML.Key], []byte("app: shop")) {
		t.Errorf("app.yaml of the run: %+v", run.AppYAML)
	}
	ri := run.Instances["main"]
	if ri == nil || ri.Kind != "container" || ri.PgVersion != "17.11" || ri.ToolVersion == "" || !ri.Whole() || ri.Globals == nil || !ri.Globals.HasPasswords || ri.Globals.Key != globalsKey(root, run.TS, "main", false) {
		t.Fatalf("run instance: %+v", ri)
	}
	if ri.Drill == nil || ri.Drill.Result != DrillOff {
		t.Errorf("no drill step given: drill must be off, got %+v", ri.Drill)
	}
	if ri.SharedPreloadLibraries == nil || *ri.SharedPreloadLibraries != "pg_stat_statements,timescaledb" {
		t.Errorf("shared_preload_libraries: %v", ri.SharedPreloadLibraries)
	}
	shop := ri.Databases["shop"]
	if shop == nil || shop.Key != dumpKey(root, run.TS, "main", "shop", false) || shop.TS != run.TS || shop.ToolVersion != ri.ToolVersion {
		t.Fatalf("shop (keys must be hydrated from the run): %+v", shop)
	}
	if shop.Locale == nil || *shop.Locale != (pg.Locale{Encoding: "UTF8", Collate: "C.UTF-8", Ctype: "C.UTF-8"}) {
		t.Errorf("locale: %+v", shop.Locale)
	}
	if len(shop.Extensions) != 2 || shop.Extensions[0] != (pg.Extension{Name: "postgis", Version: "3.4.2", Schema: "public", Config: true}) || shop.Extensions[1] != (pg.Extension{Name: "uuid-ossp", Version: "1.1", Schema: "public"}) {
		t.Errorf("extensions: %+v", shop.Extensions)
	}
	// ADR-0015: one ts per run, shared by every dump, the globals and the environment file.
	if ri.Env == nil || ri.Env.Key != envKey(root, run.TS, "main", false) || ri.Env.TS != run.TS || ri.Databases["postgres"].TS != run.TS {
		t.Errorf("run ts: env %+v, postgres %+v", ri.Env, ri.Databases["postgres"])
	}
	var env pg.Env
	if err := json.Unmarshal(st.objects[ri.Env.Key], &env); err != nil || env.SourceKind != "container" || env.ServerVersionNum != 170011 || !env.HasFileSettings || len(env.Settings) != 1 {
		t.Errorf("environment record: %v %+v", err, env)
	}
	sum := sha256.Sum256([]byte("PGDMP-shop"))
	if shop.Bytes != int64(len("PGDMP-shop")) || shop.SHA256 != hex.EncodeToString(sum[:]) || shop.Encrypted || shop.Recipient != nil {
		t.Errorf("dump entry: %+v", shop)
	}
	// row_counts auto: events (5e9) is only estimated; new_table (-1) and orders (<1e6) are counted exactly.
	// measurements is a partitioned parent (relkind p): not counted, no rows_est.
	want := map[string][2]*int64{"events": {nil, i64(5000000000)}, "measurements": {nil, nil}, "new_table": {i64(7), nil}, "orders": {i64(14300), nil}}
	if len(shop.Tables) != 4 {
		t.Fatalf("tables: %+v", shop.Tables)
	}
	for _, tb := range shop.Tables {
		w := want[tb.Name]
		if !eqI64(tb.Rows, w[0]) || !eqI64(tb.RowsEst, w[1]) {
			t.Errorf("table %s: rows=%v rows_est=%v", tb.Name, deref(tb.Rows), deref(tb.RowsEst))
		}
	}
	if n, est := TotalRows(shop); n != fakeRows || !est {
		t.Errorf("total rows = %d est=%v", n, est)
	}
	if res.Instances[0].Databases[1].Tables != 4 || res.Instances[0].Databases[1].Bytes == 0 {
		t.Errorf("database result: %+v", res.Instances[0].Databases[1])
	}
	if !slices.Contains(r.calls, `session: select count(*) from "public"."orders" db=shop`) || slices.Contains(r.calls, `session: select count(*) from "public"."events" db=shop`) || slices.Contains(r.calls, `session: select count(*) from "public"."measurements" db=shop`) {
		t.Errorf("count calls: %v", r.calls)
	}
	if !slices.Contains(r.calls, "session: commit db=shop") {
		t.Errorf("snapshot session must COMMIT after dump: %v", r.calls)
	}
	// Both SETs must come before begin: inside a transaction, a failing SET (PG < 14) aborts the transaction.
	if i, j := slices.Index(r.calls, "session: set client_connection_check_interval = '10s' db=shop"), slices.Index(r.calls, "session: begin isolation level repeatable read read only db=shop"); i < 0 || j < 0 || i > j {
		t.Errorf("SETs must run before begin: %v", r.calls)
	}
	if !slices.Contains(r.calls, "session: set lock_timeout = '60000ms' db=shop") || !slices.Contains(r.calls, "pg_dump -Fc --lock-wait-timeout=60000 --snapshot="+fakeSnapshotID+" db=shop") {
		t.Errorf("lock timeouts missing: %v", r.calls)
	}
}

func i64(n int64) *int64 { return &n }
func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}
func eqI64(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func TestBackupEncryptedWithAge(t *testing.T) {
	kp, _ := crypto.Generate()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "shop.key")
	crypto.WriteIdentityFile(keyPath, kp)
	cfg := baseCfg()
	cfg.Encryption, cfg.AgeRecipient, cfg.AgeIdentityFile = "age", kp.Recipient, keyPath
	st := newMemStore()
	r := &fakeRunner{dbs: []string{"shop"}, payload: map[string]string{"shop": strings.Repeat("PGDMP-shop ", 1000)}}
	res, err := Backup(context.Background(), cfg, deps(st, r))
	if err != nil || !res.OK {
		t.Fatal(err, res)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", res.Warnings)
	}
	run := st.newest(t)
	if !run.Encrypted() || run.Recipient() != kp.Recipient || run.AppYAML == nil || !strings.HasSuffix(run.AppYAML.Key, "/app.yaml.age") {
		t.Fatalf("run: encrypted=%v recipient=%q app.yaml=%+v", run.Encrypted(), run.Recipient(), run.AppYAML)
	}
	d := run.Instances["main"].Databases["shop"]
	if d.Key != dumpKey(root, run.TS, "main", "shop", true) || !d.Encrypted || d.Recipient == nil || *d.Recipient != kp.Recipient {
		t.Fatalf("dump entry: %+v", d)
	}
	stored := st.objects[d.Key]
	sum := sha256.Sum256(stored)
	if d.SHA256 != hex.EncodeToString(sum[:]) || d.Bytes != int64(len(stored)) {
		t.Error("sha256/bytes must describe the stored (encrypted) object")
	}
	if bytes.Contains(stored, []byte("PGDMP")) {
		t.Error("stored object is not encrypted")
	}
	// run.json itself is plaintext metadata.
	if !bytes.Contains(st.objects[RunKey(root, run.TS)], []byte(`"result": "ok"`)) {
		t.Error("run.json must be plaintext")
	}
	ids, _, _ := crypto.ReadIdentities(keyPath)
	rd, err := crypto.Decrypt(bytes.NewReader(stored), ids)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(rd)
	if string(plain) != r.payload["shop"] {
		t.Error("decrypted payload differs")
	}
	// Private key gone → still backs up, deletes nothing (not even a run
	// outside keep), not ok (ADR-0019).
	os.Remove(keyPath)
	st2 := newMemStore()
	old := oldRun(t, st2, "2026-10-01T030000Z", 10)
	res, _ = Backup(context.Background(), cfg, deps(st2, r))
	if res.OK || res.Status != ResultFailed || !strings.Contains(res.Error, "nothing is deleted until it can") || !strings.Contains(res.Error, "Fix:") {
		t.Errorf("key mismatch must back up and fail: ok=%v error=%q", res.OK, res.Error)
	}
	if run := st2.newest(t); run.TS == old.TS || run.Instances["main"].Databases["shop"] == nil || run.Verdict() != ResultFailed {
		t.Errorf("the run must still be written: %+v", run)
	}
	if len(st2.deleted) != 0 || len(st2.keysOf(old.TS)) != 2 {
		t.Errorf("nothing may be deleted: %v", st2.deleted)
	}
}

func TestBackupOneDatabaseFailsOthersContinue(t *testing.T) {
	st := newMemStore()
	r := &fakeRunner{dbs: []string{"bad", "good"}, payload: map[string]string{"good": "PGDMP-good"}, failDump: "bad"}
	res, err := Backup(context.Background(), baseCfg(r.dbs...), deps(st, r))
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || res.Status != ResultFailed || res.Instances[0].OK {
		t.Error("result must be failed")
	}
	bad, good := res.Instances[0].Databases[0], res.Instances[0].Databases[1]
	if bad.OK || !strings.Contains(bad.Error, "row-level security") {
		t.Errorf("bad: %+v (stderr of pg_dump must be kept)", bad)
	}
	if !good.OK {
		t.Errorf("good must still be dumped: %+v", good)
	}
	if m := res.Instances[0].Missing; len(m) != 1 || m[0] != "bad" {
		t.Errorf("missing = %v", m)
	}
	run := st.newest(t)
	ri := run.Instances["main"]
	if _, exists := ri.Databases["bad"]; exists || ri.Whole() || len(ri.Missing) != 1 || ri.Missing[0] != "bad" {
		t.Errorf("failed dump must not enter run.json; missing must name it: %+v", ri)
	}
	if run.Verdict() != ResultFailed || run.Backup.Result != ResultFailed || len(run.Backup.Reasons) != 1 || !strings.HasPrefix(run.Backup.Reasons[0], "main/bad: ") || run.Backup.Fix != "bracer remove shop, then bracer init shop" {
		t.Errorf("backup outcome: %+v", run.Backup)
	}
	for k := range st.objects {
		if strings.HasSuffix(k, "/bad.dump") {
			t.Errorf("partial object stored: %s", k)
		}
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "main: this run is missing bad; restore needs an older run") {
		t.Errorf("warnings: %v", res.Warnings)
	}
}

// ADR-0017: a full source whose user is no longer a superuser is not backed
// up at all: nothing is dumped and nothing of the instance is written.
func TestBackupFullSourceNeedsSuperuser(t *testing.T) {
	st := newMemStore()
	r := &fakeRunner{dbs: []string{"shop"}, payload: map[string]string{"shop": "PGDMP-shop"}, notSuper: true}
	res, err := Backup(context.Background(), baseCfg(r.dbs...), deps(st, r))
	if err != nil {
		t.Fatal(err)
	}
	ir := res.Instances[0]
	if res.OK || ir.OK || !strings.Contains(ir.Error, "is not a superuser: a full backup (users, settings) needs one; nothing was backed up") || !strings.Contains(ir.Error, "Fix: bracer remove shop, then bracer init shop") {
		t.Fatalf("instance: %+v", ir)
	}
	for _, c := range r.calls {
		if strings.HasPrefix(c, "pg_dump") {
			t.Errorf("must not dump: %s", c)
		}
	}
	for k := range st.objects {
		if strings.Contains(k, "/main/") {
			t.Errorf("nothing of the instance may be stored: %s", k)
		}
	}
	// The run still ends: run.json says what it lacks.
	ri := st.newest(t).Instances["main"]
	if ri == nil || ri.Env != nil || ri.Globals != nil || len(ri.Databases) != 0 || strings.Join(ri.Missing, ",") != "shop,env,globals" {
		t.Errorf("run instance: %+v", ri)
	}
}

// ADR-0020, ADR-0026: retention deletes whole run folders outside keep,
// run.json first; a failed run still ends with run.json and retention runs.
func TestRetentionDeletesWholeFolders(t *testing.T) {
	st := newMemStore()
	// Two old runs, one a day; the newer one also has events; daily: 2.
	oldRun(t, st, "2026-10-14T030000Z", 10)
	old15 := oldRun(t, st, "2026-10-15T030000Z", 10, func(r *Run) {
		r.Instances["main"].Databases["events"] = &Dump{Bytes: 3}
	})
	st.objects[dumpKey(root, old15.TS, "main", "events", false)] = []byte("old")

	cfg := baseCfg("shop", "events")
	r := &fakeRunner{payload: map[string]string{"shop": "new", "events": "new"}}
	d := deps(st, r)
	var logged bytes.Buffer
	d.Log = slog.New(slog.NewTextHandler(&logged, nil))
	res, err := Backup(context.Background(), cfg, d)
	if err != nil || !res.OK {
		t.Fatal(err, res)
	}
	if strings.Join(st.deleted, ",") != RunKey(root, "2026-10-14T030000Z")+","+dumpKey(root, "2026-10-14T030000Z", "main", "shop", false) {
		t.Errorf("deleted = %v (the whole folder older than the last 2 days, run.json first)", st.deleted)
	}
	// ADR-0027: one line per removed run, not one per key.
	if strings.Count(logged.String(), "retention: removed run 2026-10-14T030000Z") != 1 || strings.Contains(logged.String(), "retention: deleted") {
		t.Errorf("retention log:\n%s", logged.String())
	}
	if len(st.keysOf(old15.TS)) != 3 || len(st.keysOf(res.TS)) != 6 {
		t.Errorf("kept folders: %v %v", st.keysOf(old15.TS), st.keysOf(res.TS))
	}
	if n := len(st.deleted) + len(st.objects); st.newest(t).TS != res.TS || n != 11 {
		t.Errorf("objects: %d", n)
	}

	// Upload fails → the run is failed, still ends with run.json, and
	// deletes nothing (ADR-0029).
	st2 := newMemStore()
	oldRun(t, st2, "2026-10-14T030000Z", 10)
	oldRun(t, st2, "2026-10-15T030000Z", 10)
	st2.failPut = "main/shop.dump"
	res, err = Backup(context.Background(), cfg, deps(st2, r))
	if err != nil || res.OK || res.Status != ResultFailed {
		t.Fatal("put failure must make the run failed", err)
	}
	if run := st2.newest(t); run.TS != res.TS || run.Verdict() != ResultFailed || strings.Join(run.Instances["main"].Missing, ",") != "shop" {
		t.Errorf("failed run.json: %+v", run)
	}
	if len(st2.deleted) != 0 || len(st2.keysOf("2026-10-14T030000Z")) != 2 {
		t.Errorf("deleted = %v; a failed run deletes nothing", st2.deleted)
	}
}

// Backup dumps exactly the databases the customer chose: one more on the
// server is not bracer's business (ADR-0024). A corrupt run.json stops
// everything before anything is written (ADR-0023).
func TestOnlyChosenAndCorruptRunJSON(t *testing.T) {
	st := newMemStore()
	r := &fakeRunner{dbs: []string{"shop"}, extraDBs: []string{"fresh"}, payload: map[string]string{"shop": "x"}}
	res, err := Backup(context.Background(), baseCfg(), deps(st, r))
	if err != nil {
		t.Fatal(err)
	}
	ir := res.Instances[0]
	if !res.OK || res.Status != ResultOK {
		t.Errorf("ok=%v status=%s", res.OK, res.Status)
	}
	if len(ir.Databases) != 1 || ir.Databases[0].Name != "shop" {
		t.Errorf("only app.yaml's databases are dumped: %+v", ir.Databases)
	}
	if run := st.newest(t); run.Backup.Result != ResultOK || len(run.Instances["main"].Databases) != 1 {
		t.Errorf("run.json: %+v", run)
	}

	// A corrupt newest run.json: nothing is written, not even over it.
	key := RunKey(root, res.TS)
	st.objects[key] = []byte("{not json")
	before := len(st.objects)
	r.calls = nil
	if _, err = Backup(context.Background(), baseCfg(), deps(st, r)); err == nil || !errors.Is(err, ErrRunCorrupt) || !strings.Contains(err.Error(), "not valid JSON") || !strings.Contains(err.Error(), "Fix: bracer remove shop, then bracer init shop") {
		t.Errorf("corrupt run.json: err=%v", err)
	}
	if len(st.objects) != before || string(st.objects[key]) != "{not json" || len(r.calls) != 0 {
		t.Errorf("nothing may be written or dumped: %d objects, calls %v", len(st.objects), r.calls)
	}
}

// ADR-0023: a folder that is missing (outside init's first run) or written
// by another place is never written to.
func TestFolderMissingOrAnotherWriter(t *testing.T) {
	r := &fakeRunner{dbs: []string{"shop"}, payload: map[string]string{"shop": "x"}}
	st := newMemStore()
	d := deps(st, r)
	d.Create = false
	_, err := Backup(context.Background(), baseCfg(), d)
	if err == nil || err.Error() != "the bucket has no folder shop-abc123: its backups are gone or the bucket settings point elsewhere\nFix: bracer remove shop, then bracer init" || len(st.objects) != 0 || len(r.calls) != 0 {
		t.Errorf("missing folder: err=%v objects=%d calls=%v", err, len(st.objects), r.calls)
	}
	// A folder that did not end (no run.json) is not a folder either.
	st.objects[RunDir(root, "2026-10-10T030000Z")+"main/shop.dump"] = []byte("half")
	if _, err := Backup(context.Background(), baseCfg(), d); err == nil || !strings.HasPrefix(err.Error(), "the bucket has no folder shop-abc123") || len(r.calls) != 0 {
		t.Errorf("folder without run.json: err=%v", err)
	}
	// init's first run creates it, owned by here.
	if _, err := Backup(context.Background(), baseCfg(), deps(st, r)); err != nil {
		t.Fatal(err)
	}
	if run := st.newest(t); run.Writer != here || run.Verdict() != ResultOK || run.Backup.Fix != "" {
		t.Errorf("created run: %+v", run)
	}
	// Another place (a copy of the app directory): nothing is written.
	n := len(st.objects)
	d = deps(st, r)
	d.Here = Writer{ID: "9999888877776666", Hostname: "vps-2", Path: "/home/deploy/.config/bracer/shop2"}
	r.calls = nil
	_, err = Backup(context.Background(), baseCfg(), d)
	if err == nil || err.Error() != "folder shop-abc123 is written by vps-1:/home/deploy/.config/bracer/shop; bracer does not write to it from here\nFix: bracer remove shop, then bracer init shop" || len(st.objects) != n || len(r.calls) != 0 {
		t.Errorf("another writer: err=%v objects %d→%d calls=%v", err, n, len(st.objects), r.calls)
	}
}

func TestHere(t *testing.T) {
	dir := t.TempDir()
	MachineIDPath = filepath.Join(dir, "machine-id")
	defer func() { MachineIDPath = "/etc/machine-id" }()
	os.WriteFile(MachineIDPath, []byte("0123456789abcdef0123456789abcdef\n"), 0o644)
	app := filepath.Join(dir, "shop")
	os.MkdirAll(app, 0o700)
	os.Symlink(app, filepath.Join(dir, "link"))
	a, err := Here("shop-abc123", filepath.Join(app, "app.yaml"))
	if err != nil || len(a.ID) != 16 || a.Path != app && !strings.HasSuffix(a.Path, "/shop") {
		t.Fatalf("here: %+v %v", a, err)
	}
	if b, _ := Here("shop-abc123", filepath.Join(dir, "link", "app.yaml")); b.ID != a.ID {
		t.Errorf("a symlink to the same directory is the same place: %s %s", a.ID, b.ID)
	}
	if c, _ := Here("shop-abc123", filepath.Join(dir, "shop2", "app.yaml")); c.ID == a.ID {
		t.Error("another directory is another place")
	}
	if c, _ := Here("shop-zzz999", filepath.Join(app, "app.yaml")); c.ID == a.ID {
		t.Error("another folder gives another id")
	}
	os.WriteFile(MachineIDPath, []byte("ffffffffffffffffffffffffffffffff\n"), 0o644)
	if c, _ := Here("shop-abc123", filepath.Join(app, "app.yaml")); c.ID == a.ID {
		t.Error("another machine is another place")
	}
	if strings.Contains(a.ID, "0123") {
		t.Error("the machine id must not show")
	}
	os.Remove(MachineIDPath)
	if _, err := Here("shop-abc123", filepath.Join(app, "app.yaml")); err == nil {
		t.Error("no machine-id must be an error")
	}
}

// ADR-0027, ADR-0029: run.json stores no verdict; Verdict() computes it
// from the backup and the drills, ok or failed.
func TestVerdict(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  Run
		want string
	}{
		{"ok", Run{Backup: Outcome{Result: ResultOK}}, ResultOK},
		{"warnings only", Run{Backup: Outcome{Result: ResultOK, Warnings: []string{"main/shop dropped"}}}, ResultOK},
		{"backup failed", Run{Backup: Outcome{Result: ResultFailed}}, ResultFailed},
		{"drill failed", Run{Backup: Outcome{Result: ResultOK}, Instances: map[string]*RunInstance{"main": {Drill: &DrillOutcome{Result: DrillFailed}}}}, ResultFailed},
		{"drill off", Run{Backup: Outcome{Result: ResultOK}, Instances: map[string]*RunInstance{"main": {Drill: &DrillOutcome{Result: DrillOff}}}}, ResultOK},
	} {
		if got := tc.run.Verdict(); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestNewOutcome(t *testing.T) {
	started := time.Date(2026, 10, 16, 3, 0, 1, 0, time.UTC)
	finished := started.Add(2*time.Minute + 10*time.Second)
	o := NewOutcome(started, finished, []string{"main/auth: database auth does not exist", "main: postgres is not a superuser\n  Fix: bracer remove shop, then bracer init shop"}, []string{"Postgres on main is now 18 (app.yaml says 17); run bracer remove shop, then bracer init shop to record it"}, "bracer remove shop, then bracer init shop")
	if o.Result != ResultFailed || len(o.Reasons) != 2 || o.Reasons[1] != "main: postgres is not a superuser" || o.Fix != "bracer remove shop, then bracer init shop" || o.StartedAt != started || o.FinishedAt != finished || len(o.Warnings) != 1 {
		t.Errorf("failed outcome: %+v", o)
	}
	// ADR-0029: a warning does not change the result and carries no Fix.
	if o := NewOutcome(started, finished, nil, []string{"main/shop dropped from 2000 to 100 rows"}, "bracer remove shop, then bracer init shop"); o.Result != ResultOK || o.Fix != "" || len(o.Reasons) != 0 || len(o.Warnings) != 1 {
		t.Errorf("warning outcome: %+v", o)
	}
	if o := NewOutcome(started, finished, nil, nil, "bracer remove shop, then bracer init shop"); o.Result != ResultOK || o.Fix != "" || o.Reasons == nil || len(o.Reasons) != 0 || o.Warnings == nil || len(o.Warnings) != 0 {
		t.Errorf("ok outcome: %+v", o)
	}
	if Worse(ResultOK, ResultFailed) != ResultFailed || Worse(ResultFailed, ResultOK) != ResultFailed || Worse(ResultOK, ResultOK) != ResultOK {
		t.Error("Worse")
	}
}

func TestLock(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "app.yaml")
	l, err := Acquire(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(cfgPath); !errors.Is(err, ErrLocked) {
		t.Errorf("second acquire: %v", err)
	}
	l.Release()
	os.WriteFile(filepath.Join(dir, ".app.yaml.lock"), []byte("999999999\n"), 0o644)
	l2, err := Acquire(cfgPath)
	if err != nil {
		t.Errorf("stale lock must be taken over: %v", err)
	}
	l2.Release()
}

// The ts of a run always sorts after the newest folder (clock behind → +1 s,
// warning); a folder without run.json is a run that did not end and goes;
// keys outside a <ts>/ folder are never touched (ADR-0026).
func TestClockBehindAndFoldersWithoutRunJSON(t *testing.T) {
	st := newMemStore()
	// A run of yesterday and one five minutes ahead of the clock (NTP set it back).
	oldRun(t, st, "2026-10-15T030000Z", 10)
	oldRun(t, st, "2026-10-16T030500Z", 10)
	// A run that did not end, a stray key, the probe and an old layout's key.
	half := RunDir(root, "2026-10-14T030000Z") + "main/shop.dump"
	st.objects[half] = []byte("half")
	strays := []string{root + "stray.dump", ProbeKey("shop-abc123"), root + "drill/main.json"}
	for _, k := range strays {
		st.objects[k] = []byte("?")
	}

	r := &fakeRunner{dbs: []string{"shop"}, payload: map[string]string{"shop": "new"}}
	res, err := Backup(context.Background(), baseCfg(), deps(st, r))
	if err != nil || !res.OK {
		t.Fatal(err, res)
	}
	if res.TS != "2026-10-16T030501Z" || !slices.Contains(res.Warnings, "clock is behind the last run 2026-10-16T030500Z; this run is 2026-10-16T030501Z") {
		t.Errorf("ts = %s, warnings %v", res.TS, res.Warnings)
	}
	if st.newest(t).TS != res.TS {
		t.Error("this run must be the newest")
	}
	// keep 2 days: every run of 10-16 (both) and of 10-15 stays (ADR-0029);
	// the folder without run.json goes.
	if strings.Join(st.deleted, ",") != half {
		t.Errorf("deleted = %v", st.deleted)
	}
	if len(st.keysOf("2026-10-15T030000Z")) != 2 || len(st.keysOf("2026-10-16T030500Z")) != 2 {
		t.Error("the runs of 10-15 and the earlier one of 10-16 must stay")
	}
	for _, k := range strays {
		if _, ok := st.objects[k]; !ok {
			t.Errorf("%s is outside every run folder and must be left alone", k)
		}
	}
}

// Container with password_env (Bitnami demands a password even over the socket): the
// password must go into Conn so ExecRunner passes -e PGPASSWORD.
func TestTargetPasswordForContainer(t *testing.T) {
	sec := config.NewSecrets(map[string]string{"PW": "s3cret"}, "test")
	_, conn, err := Target(config.Instance{Name: "c", Connection: config.Connection{Container: "db", User: "postgres", PasswordEnv: "PW"}}, sec)
	if err != nil || conn.Password != "s3cret" {
		t.Fatalf("container: conn=%+v err=%v", conn, err)
	}
	_, conn, err = Target(config.Instance{Name: "c", Connection: config.Connection{Container: "db", User: "postgres"}}, sec)
	if err != nil || conn.Password != "" {
		t.Fatalf("container without password_env: conn=%+v err=%v", conn, err)
	}
}

// spec backup § Flow 3a2, ADR-0029: another cluster at the same place is
// still dumped and only warned about, retention as usual; run.json carries
// system_id; a different major warns too.
func TestIdentityChangeWarns(t *testing.T) {
	st := newMemStore()
	for _, ts := range []string{"2026-10-14T030000Z", "2026-10-15T030000Z"} {
		oldRun(t, st, ts, 10, func(r *Run) { r.Instances["main"].SystemID = "7698217040907026472" })
	}
	cfg := baseCfg()
	cfg.Instances[0].SystemID = "7698217040907026472"
	cfg.Instances[0].PgMajor = 16
	r := &fakeRunner{payload: map[string]string{"shop": "new"}, sysid: "7562871935211888659"}
	d := deps(st, r)
	res, err := Backup(context.Background(), cfg, d)
	if err != nil || !res.OK || res.Status != ResultOK {
		t.Fatal(err, res)
	}
	ir := res.Instances[0]
	if !ir.IdentityChanged || ir.SystemID != "7562871935211888659" || !slices.Contains(st.deleted, RunKey(root, "2026-10-14T030000Z")) {
		t.Errorf("identity_changed=%v system_id=%s deleted=%v (keep 2 days: 10-14 goes as usual)", ir.IdentityChanged, ir.SystemID, st.deleted)
	}
	all := strings.Join(res.Warnings, "\n")
	if !strings.Contains(all, "main is a different Postgres cluster than before (initialized 2025-10-19") || !strings.Contains(all, "app.yaml says 7698217040907026472); run bracer remove shop, then bracer init shop to confirm it") || !strings.Contains(all, "Postgres on main is now 17 (app.yaml says 16); run bracer remove shop, then bracer init shop to record it") {
		t.Errorf("warnings: %v", res.Warnings)
	}
	run := st.newest(t)
	if ri := run.Instances["main"]; run.TS != res.TS || run.Verdict() != ResultOK || ri.SystemID != "7562871935211888659" || !ri.IdentityChanged || !reflect.DeepEqual(run.Backup.Warnings, res.Warnings) {
		t.Errorf("run.json: %+v %+v", run, run.Instances["main"])
	}

	// Same cluster → no warning; managed (system_id: none) → no check, no failure.
	r.sysid = "7562871935211888659"
	cfg.Instances[0].SystemID, cfg.Instances[0].PgMajor = r.sysid, 17
	if res, err = Backup(context.Background(), cfg, d); err != nil || res.Status != ResultOK || res.Instances[0].IdentityChanged || strings.Contains(strings.Join(res.Warnings, "\n"), "cluster") {
		t.Errorf("same cluster: %v %+v", err, res)
	}
	r.sysid = ""
	cfg.Instances[0].SystemID = config.SystemIDNone
	if res, err = Backup(context.Background(), cfg, d); err != nil || res.Status != ResultOK || res.Instances[0].IdentityChanged {
		t.Errorf("managed: %v %+v", err, res)
	}
}

// ADR-0014: a backup user without pg_read_all_settings cannot read
// shared_preload_libraries; the backup still succeeds, the field stays empty
// and a warning names the GRANT.
func TestBackupPreloadLibrariesUnreadable(t *testing.T) {
	st := newMemStore()
	r := &fakeRunner{dbs: []string{"shop"}, payload: map[string]string{"shop": "PGDMP-shop"}, noSettings: true}
	res, err := Backup(context.Background(), baseCfg(), deps(st, r))
	if err != nil || !res.OK {
		t.Fatalf("res %+v, err %v", res, err)
	}
	if ri := st.newest(t).Instances["main"]; ri.SharedPreloadLibraries != nil {
		t.Errorf("shared_preload_libraries = %q", *ri.SharedPreloadLibraries)
	}
	if w := strings.Join(res.Warnings, "\n"); !strings.Contains(w, "GRANT pg_read_all_settings TO") {
		t.Errorf("warnings: %s", w)
	}
}

// Every run ends with run.json, its instance marked whole or not, and a
// failed run deletes nothing (ADR-0029): the newest run that passed is
// never deleted, and the failed runs of the same day stay as long as the
// window keeps that day.
func TestFailedRunDeletesNothing(t *testing.T) {
	st := newMemStore()
	oldRun(t, st, "2026-10-14T030000Z", 10)
	oldRun(t, st, "2026-10-15T030000Z", 10)
	r := &fakeRunner{dbs: []string{"shop", "auth"}, payload: map[string]string{"shop": "PGDMP-shop", "auth": "PGDMP-auth"}}
	cfg := baseCfg("shop", "auth") // keep 2 days
	d := deps(st, r)
	res, err := Backup(context.Background(), cfg, d)
	if err != nil || !res.OK || len(res.Instances[0].Missing) != 0 {
		t.Fatalf("first run: %v %+v", err, res.Instances[0])
	}
	first := res.TS
	run := st.newest(t)
	if run.AppYAML == nil || run.AppYAML.TS != first || run.AppYAML.Key != appYAMLKey(root, first, false) {
		t.Fatalf("app.yaml of the run: %+v", run.AppYAML)
	}
	if _, ok := st.objects[appYAMLKey(root, first, false)]; !ok {
		t.Error("app.yaml of the run not in the bucket")
	}
	wantKeys := []string{appYAMLKey(root, first, false), dumpKey(root, first, "main", "auth", false), envKey(root, first, "main", false), globalsKey(root, first, "main", false), dumpKey(root, first, "main", "shop", false), RunKey(root, first)}
	if got := st.keysOf(first); strings.Join(got, ",") != strings.Join(wantKeys, ",") {
		t.Errorf("objects of the run: %v", got)
	}
	// The pass run kept 10-16 and 10-15 and removed 10-14.
	if strings.Join(st.deleted, ",") != RunKey(root, "2026-10-14T030000Z")+","+dumpKey(root, "2026-10-14T030000Z", "main", "shop", false) {
		t.Errorf("deleted = %v", st.deleted)
	}

	st.deleted = nil
	r.failDump = "auth"
	for i := 0; i < 3; i++ {
		res, err = Backup(context.Background(), cfg, d)
		if err != nil || res.OK || res.Status != ResultFailed || len(res.Instances[0].Missing) != 1 || res.Instances[0].Missing[0] != "auth" {
			t.Fatalf("run with auth failing: %v ok=%v %+v", err, res.OK, res.Instances[0])
		}
		if run := st.newest(t); run.TS != res.TS || run.Verdict() != ResultFailed || run.Instances["main"].Whole() || strings.Join(run.Instances["main"].Missing, ",") != "auth" {
			t.Fatalf("failed run.json: %+v %+v", run, run.Instances["main"])
		}
	}
	if len(st.deleted) != 0 || len(st.keysOf("2026-10-15T030000Z")) != 2 {
		t.Errorf("a failed run deletes nothing: %v", st.deleted)
	}
	if got := st.keysOf(first); strings.Join(got, ",") != strings.Join(wantKeys, ",") {
		t.Errorf("every object of the run that passed must be kept: %v", got)
	}

	// The next pass run keeps every run of the window, the failed ones of
	// today included (they have a dump), and removes 10-15 with keep 1.
	r.failDump = ""
	cfg.Schedule.Keep = 1
	if res, err = Backup(context.Background(), cfg, d); err != nil || !res.OK {
		t.Fatalf("pass run: %v %+v", err, res)
	}
	if strings.Join(st.deleted, ",") != RunKey(root, "2026-10-15T030000Z")+","+dumpKey(root, "2026-10-15T030000Z", "main", "shop", false) {
		t.Errorf("deleted = %v", st.deleted)
	}
	var dumps, runs int
	for k := range st.objects {
		if strings.HasSuffix(k, "/shop.dump") {
			dumps++
		}
		if strings.HasSuffix(k, "/"+RunFile) {
			runs++
		}
	}
	if dumps != 5 || runs != 5 {
		t.Errorf("shop dumps %d, runs %d: the five runs of today all stay", dumps, runs)
	}
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

// spec backup § needs: run.json records the library a C function loads and
// the collations columns use.
func TestBackupRecordsNeeds(t *testing.T) {
	f := &fakeRunner{dbs: []string{"shop"}, payload: map[string]string{"shop": "PGDMP-shop"}}
	f.cfuncs = "$libdir/mylib\x1fpublic.score(text)\n$libdir/mylib\x1fpublic.rank(text)\n"
	f.colls = "pg_catalog\x1fsv_SE\x1ff\x1fc\x1fsv_SE.utf8\x1fsv_SE.utf8\x1f\x1f\x1f2.41\x1fpublic.customers.name\n"
	st := newMemStore()
	if res, err := Backup(context.Background(), baseCfg(), deps(st, f)); err != nil || !res.OK {
		t.Fatalf("res %+v, err %v", res, err)
	}
	dp := st.newest(t).Instances["main"].Databases["shop"]
	if dp.RestoreNeeds == nil || len(dp.RestoreNeeds.Libraries) != 1 {
		t.Fatalf("needs: %+v", dp.RestoreNeeds)
	}
	if e := dp.Extensions; len(e) != 2 || !e[0].Config || e[1].Config {
		t.Errorf("extensions: %+v", e)
	}
	if lo := dp.LockObjects; lo == nil || lo.Relations != 412 || lo.ForeignKeys != 37 {
		t.Errorf("lock objects: %+v", lo)
	}
	lib := dp.RestoreNeeds.Libraries[0]
	if lib.Name != "mylib.so" || strings.Join(lib.UsedBy, ",") != "public.score(text),public.rank(text)" {
		t.Errorf("library: %+v", lib)
	}
	c := dp.RestoreNeeds.Collations
	if len(c) != 1 || c[0].Name != "sv_SE" || c[0].Created || c[0].Collate != "sv_SE.utf8" || c[0].Provider != "" || c[0].Version != "2.41" || c[0].UsedBy[0] != "public.customers.name" {
		t.Errorf("collations: %+v", c)
	}
}

// ADR-0026, ADR-0029: a failed drill fails the run and the run deletes
// nothing; a run that passed keeps only the window, whatever drills the
// older runs passed.
func TestDrillFailsRunAndDeletesNothing(t *testing.T) {
	st := newMemStore()
	passed := oldRun(t, st, "2026-10-14T030000Z", 10, func(r *Run) {
		r.Instances["main"].Drill = &DrillOutcome{Result: DrillPassed, At: time.Date(2026, 10, 14, 3, 5, 0, 0, time.UTC)}
	})
	oldRun(t, st, "2026-10-15T030000Z", 10, func(r *Run) {
		r.Instances["main"].Drill = &DrillOutcome{Result: DrillFailed, Reasons: []string{"orders: 10 rows, dump says 11"}}
	})
	if !passed.AllDrillsPassed() {
		t.Fatal("seeded run must count as drill-passed")
	}
	cfg := baseCfg()
	cfg.Schedule.Keep = 1
	r := &fakeRunner{dbs: []string{"shop"}, payload: map[string]string{"shop": "new"}}
	d := deps(st, r) // no drill step: this run's drill is off
	res, err := Backup(context.Background(), cfg, d)
	if err != nil || !res.OK {
		t.Fatal(err, res)
	}
	sorted := append([]string(nil), st.deleted...)
	sort.Strings(sorted)
	if strings.Join(sorted, ",") != dumpKey(root, "2026-10-14T030000Z", "main", "shop", false)+","+RunKey(root, "2026-10-14T030000Z")+","+dumpKey(root, "2026-10-15T030000Z", "main", "shop", false)+","+RunKey(root, "2026-10-15T030000Z") {
		t.Errorf("deleted = %v: keep 1 keeps today only, the old drill-pass is not protected", st.deleted)
	}
	if strings.Contains(strings.Join(res.Warnings, "\n"), "keeping run") {
		t.Errorf("warnings: %v", res.Warnings)
	}
	if run := st.newest(t); run.AllDrillsPassed() || run.Instances["main"].Drill.Result != DrillOff {
		t.Errorf("drill off: %+v", run.Instances["main"].Drill)
	}

	// This run passes its drill.
	st.deleted = nil
	d.Drill = fakeDrill(DrillPassed)
	res, err = Backup(context.Background(), cfg, d)
	if err != nil || !res.OK || res.Status != ResultOK {
		t.Fatal(err, res)
	}
	run := st.newest(t)
	if dr := run.Instances["main"].Drill; !run.AllDrillsPassed() || run.Verdict() != ResultOK || dr.TookSeconds != 12.5 || dr.At.IsZero() {
		t.Errorf("drill outcome: %+v", dr)
	}
	if len(st.deleted) != 0 {
		t.Errorf("deleted = %v: both runs are today", st.deleted)
	}

	// The drill fails: the backup part is ok, the run is failed (exit 1),
	// nothing is deleted.
	d.Drill = fakeDrill(DrillFailed, "orders: 7 rows, dump says 14300")
	res, err = Backup(context.Background(), cfg, d)
	if err != nil || res.OK || res.Status != ResultFailed || !slices.Contains(res.Failures(), "main: drill: orders: 7 rows, dump says 14300") || len(res.BackupFailures()) != 0 {
		t.Fatalf("drill failed: %v ok=%v status=%s failures=%v", err, res.OK, res.Status, res.Failures())
	}
	run = st.newest(t)
	if dr := run.Instances["main"].Drill; run.TS != res.TS || run.Verdict() != ResultFailed || run.Backup.Result != ResultOK || dr.Result != DrillFailed || len(dr.Reasons) != 1 || len(st.deleted) != 0 {
		t.Errorf("run.json after a failed drill: result=%s backup=%+v drill=%+v deleted=%v", run.Verdict(), run.Backup, dr, st.deleted)
	}
}

// ADR-0028, ADR-0029: a database that lost more than half of its rows (from
// 1000 or more) is a warning of this run, naming the run to restore from,
// and the run still passes. The comparison is the newest earlier run that
// dumped the database, so a run without dumps between does not blind it;
// such a run is deleted by the next pass. The run compared against lives as
// long as the window keeps it (here: the same day); the next run, rows
// stable, has no such warning.
func TestRowDropWarns(t *testing.T) {
	st := newMemStore()
	oldRun(t, st, "2026-10-10T030000Z", 10)
	before := oldRun(t, st, "2026-10-16T020000Z", 20_000_000_000) // an hour ago, by hand
	// Postgres was down in between: run.json, no dump.
	putRun(t, st, &Run{
		App: "shop", TS: "2026-10-16T023000Z", Writer: here,
		Backup:    Outcome{Result: ResultFailed, Reasons: []string{"main: connection refused"}},
		Instances: map[string]*RunInstance{"main": {Kind: "container", Missing: []string{"shop"}, Databases: map[string]*Dump{}}},
	})
	cfg := baseCfg()
	cfg.Schedule.Keep = 1
	r := &fakeRunner{dbs: []string{"shop"}, payload: map[string]string{"shop": "new"}}
	d := deps(st, r)
	res, err := Backup(context.Background(), cfg, d)
	if err != nil || !res.OK || res.Status != ResultOK {
		t.Fatal(err, res)
	}
	want := fmt.Sprintf("main/shop dropped from 20000000000 to %d rows since the run %s; if that was not on purpose, restore from it while retention keeps it", fakeRows, before.TS)
	if !slices.Contains(res.Warnings, want) {
		t.Errorf("warnings: %v\nwant %s", res.Warnings, want)
	}
	run := st.newest(t)
	if run.Verdict() != ResultOK || run.Backup.Result != ResultOK || !slices.Contains(run.Backup.Warnings, want) || len(run.Backup.Reasons) != 0 || run.Backup.Fix != "" {
		t.Errorf("run.json outcome: %+v", run.Backup)
	}
	// keep 1 keeps today: this run and the one it compared against; the
	// run without a dump goes, like the 10th.
	for _, ts := range []string{"2026-10-16T023000Z", "2026-10-10T030000Z"} {
		if !slices.Contains(st.deleted, RunKey(root, ts)) {
			t.Errorf("%s must be deleted: %v", ts, st.deleted)
		}
	}
	if slices.Contains(st.deleted, before.Instances["main"].Databases["shop"].Key) || slices.Contains(st.deleted, RunKey(root, before.TS)) {
		t.Errorf("the run compared against is of today and must stay: %v", st.deleted)
	}
	// The next run compares with this one: no drop, no warning.
	if res, err = Backup(context.Background(), cfg, d); err != nil || !res.OK || res.Status != ResultOK || strings.Contains(strings.Join(res.Warnings, "\n"), "dropped") {
		t.Errorf("next run: %v %+v", err, res)
	}
}

// ADR-0028: the newest folder a little ahead of the clock is bumped past
// (+1 s, a warning); one an hour or more ahead stops the run before
// anything is written.
func TestClockBehindNewestRun(t *testing.T) {
	st := newMemStore()
	oldRun(t, st, "2026-10-16T030500Z", 10) // 5 min ahead of the clock
	r := &fakeRunner{dbs: []string{"shop"}, payload: map[string]string{"shop": "new"}}
	res, err := Backup(context.Background(), baseCfg(), deps(st, r))
	if err != nil || !res.OK || res.TS != "2026-10-16T030501Z" || !slices.Contains(res.Warnings, "clock is behind the last run 2026-10-16T030500Z; this run is 2026-10-16T030501Z") {
		t.Fatalf("5 min ahead: %v %+v", err, res)
	}

	st = newMemStore()
	oldRun(t, st, "2026-10-16T050000Z", 10) // 2 h ahead
	puts := len(st.puts)
	res, err = Backup(context.Background(), baseCfg(), deps(st, r))
	if err == nil || err.Error() != "the newest run 2026-10-16T050000Z is 2 hours ahead of this machine's clock; fix the clock (or delete that folder in the bucket if the clock was wrong when it ran)" {
		t.Fatalf("2 h ahead: err=%v res=%+v", err, res)
	}
	if len(st.puts) != puts || len(st.deleted) != 0 {
		t.Errorf("nothing may be written or deleted: puts=%v deleted=%v", st.puts[puts:], st.deleted)
	}
	if aheadText(50*time.Hour) != "2 days" || aheadText(90*time.Minute) != "2 hours" {
		t.Error(aheadText(50*time.Hour), aheadText(90*time.Minute))
	}
}

// The run's app.yaml or run.json not writable: the run is failed; without
// app.yaml every instance misses it, without run.json nothing is deleted.
func TestAppYAMLOrRunJSONUnwritable(t *testing.T) {
	r := &fakeRunner{dbs: []string{"shop"}, payload: map[string]string{"shop": "new"}}
	st := newMemStore()
	st.failPut = "/app.yaml"
	res, err := Backup(context.Background(), baseCfg(), deps(st, r))
	if err != nil || res.OK || res.Status != ResultFailed || !strings.Contains(res.Error, "could not write app.yaml of this run") || strings.Join(res.Instances[0].Missing, ",") != "app.yaml" {
		t.Fatalf("app.yaml unwritable: %v ok=%v error=%q missing=%v", err, res.OK, res.Error, res.Instances[0].Missing)
	}
	run := st.newest(t)
	if run.AppYAML != nil || run.Verdict() != ResultFailed || strings.Join(run.Instances["main"].Missing, ",") != "app.yaml" || st.puts[len(st.puts)-1] != RunKey(root, run.TS) {
		t.Errorf("run.json: %+v", run)
	}

	st2 := newMemStore()
	old := oldRun(t, st2, "2026-10-01T030000Z", 10)
	st2.failPut = RunFile
	res, err = Backup(context.Background(), baseCfg(), deps(st2, r))
	if err != nil || res.OK || res.Status != ResultFailed || !strings.Contains(res.Error, "run.json could not be written") {
		t.Fatalf("run.json unwritable: %v ok=%v error=%q", err, res.OK, res.Error)
	}
	if len(st2.deleted) != 0 || st2.newest(t).TS != old.TS || len(st2.keysOf(res.TS)) != 4 {
		t.Errorf("a run without run.json deletes nothing and is not the newest: deleted=%v", st2.deleted)
	}
}

func TestSchemaOnlyRun(t *testing.T) {
	kp, _ := crypto.Generate()
	cfg := baseCfg()
	cfg.Encryption, cfg.AgeRecipient = "age", kp.Recipient
	st := storage.NewMemory(PreflightMaxBytes)
	r := &fakeRunner{dbs: []string{"shop"}}
	d := deps(st, r)
	d.SchemaOnly = true
	res, err := Backup(context.Background(), cfg, d)
	if err != nil || !res.OK {
		t.Fatal(err, res)
	}
	objs, _ := st.List(context.Background(), root)
	if len(objs) == 0 {
		t.Fatal("nothing dumped")
	}
	for _, o := range objs {
		if strings.HasSuffix(o.Key, ".age") {
			t.Errorf("schema-only run must not encrypt: %s", o.Key)
		}
		if strings.HasSuffix(o.Key, "/"+RunFile) {
			t.Errorf("schema-only run must not end with run.json: %s", o.Key)
		}
	}
	for _, c := range r.calls {
		if strings.HasPrefix(c, "pg_dump -Fc") && !strings.Contains(c, "--schema-only") {
			t.Errorf("data dumped: %s", c)
		}
		if strings.HasPrefix(c, "session: select count(*)") {
			t.Errorf("rows counted: %s", c)
		}
	}
}

// ADR-0029: every run of the last N days, ISO weeks or months that have one;
// several runs a day all stay.
func TestKeepSet(t *testing.T) {
	tss := []string{
		"2026-10-21T150000Z", "2026-10-21T030000Z", // two runs today: both stay
		"2026-10-20T030000Z", "2026-10-19T030000Z", // 19 is a Monday: week 43 starts
		"2026-10-12T030000Z", "2026-10-05T030000Z", // weeks 42, 41
		"2026-09-30T030000Z", "2026-08-15T030000Z", "2026-07-01T030000Z",
		"bogus",
	}
	check := func(every string, keep int, want ...string) {
		t.Helper()
		got := keepSet(tss, every, keep, time.UTC)
		var keys []string
		for k := range got {
			keys = append(keys, k)
		}
		want = append(want, "bogus") // never deleted
		sort.Strings(keys)
		sort.Strings(want)
		if strings.Join(keys, ",") != strings.Join(want, ",") {
			t.Errorf("keepSet(%s, %d) = %v\nwant %v", every, keep, keys, want)
		}
	}
	check(config.EveryDaily, 2, "2026-10-21T150000Z", "2026-10-21T030000Z", "2026-10-20T030000Z")
	check(config.EveryWeekly, 2, "2026-10-21T150000Z", "2026-10-21T030000Z", "2026-10-20T030000Z", "2026-10-19T030000Z", "2026-10-12T030000Z")
	check(config.EveryMonthly, 2, "2026-10-21T150000Z", "2026-10-21T030000Z", "2026-10-20T030000Z", "2026-10-19T030000Z", "2026-10-12T030000Z", "2026-10-05T030000Z", "2026-09-30T030000Z")
	// A week with no run is not counted: the machine was off.
	got := keepSet([]string{"2026-10-21T030000Z", "2026-09-01T030000Z"}, config.EveryWeekly, 2, time.UTC)
	if len(got) != 2 {
		t.Errorf("gap: %v", got)
	}
	// ADR-0028: days are the machine's. At UTC+7 a run at 10:00 on the 21st
	// (03:00Z) and the night run at 03:00 on the 22nd (20:00Z the 21st) are
	// two days, so keep 1 keeps only the night run and keep 2 keeps both;
	// in UTC they are one day and keep 1 keeps both.
	tss = []string{"2026-10-21T200000Z", "2026-10-21T030000Z"}
	hanoi := time.FixedZone("", 7*3600)
	if got := keepSet(tss, config.EveryDaily, 2, hanoi); len(got) != 2 {
		t.Errorf("two local days: %v", got)
	}
	if got := keepSet(tss, config.EveryDaily, 1, hanoi); len(got) != 1 || !got["2026-10-21T200000Z"] {
		t.Errorf("newest local day: %v", got)
	}
	if got := keepSet(tss, config.EveryDaily, 1, time.UTC); len(got) != 2 {
		t.Errorf("one UTC day: %v", got)
	}
}

// ListRuns groups the keys of the folder by run and tells which runs ended;
// a corrupt run.json is ErrRunCorrupt, a missing one ErrNoFolder.
func TestListRunsAndLoadRun(t *testing.T) {
	st := newMemStore()
	oldRun(t, st, "2026-10-14T030000Z", 10)
	st.objects[RunDir(root, "2026-10-15T030000Z")+"main/shop.dump"] = []byte("half")
	st.objects[root+"stray"] = []byte("?")
	st.objects[root+"notats/run.json"] = []byte("{}")
	refs, err := ListRuns(context.Background(), st, root)
	if err != nil || len(refs) != 2 || refs[0].TS != "2026-10-15T030000Z" || refs[0].Done || refs[1].TS != "2026-10-14T030000Z" || !refs[1].Done || len(refs[1].Keys) != 2 {
		t.Fatalf("refs: %+v %v", refs, err)
	}
	if NewestDone(refs) != "2026-10-14T030000Z" {
		t.Error("newest done")
	}
	if _, err := LoadRun(context.Background(), st, root, "2026-10-15T030000Z"); !errors.Is(err, ErrNoFolder) {
		t.Errorf("no run.json: %v", err)
	}
	st.objects[RunKey(root, "2026-10-15T030000Z")] = []byte("{")
	if _, err := LoadRun(context.Background(), st, root, "2026-10-15T030000Z"); !errors.Is(err, ErrRunCorrupt) {
		t.Errorf("corrupt: %v", err)
	}
	if _, _, err := Newest(context.Background(), st, "bracer/empty/"); !errors.Is(err, ErrNoFolder) {
		t.Errorf("empty folder: %v", err)
	}
}
