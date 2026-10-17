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
	"slices"
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
	dbs      []string
	payload  map[string]string // db → pg_dump contents
	failDump string            // db whose pg_dump fails
	super    bool
	readAll  bool // pg_read_all_data
	calls    []string
}

func (f *fakeRunner) Describe() string { return "fake runner" }

func (f *fakeRunner) Run(_ context.Context, conn pg.Conn, tool string, args []string, stdin io.Reader, stdout io.Writer) error {
	f.calls = append(f.calls, tool+" "+strings.Join(args, " ")+" db="+conn.Database)
	switch tool {
	case "psql":
		if args[len(args)-1] == "-" { // -f - session: reads statements one by one from stdin
			return f.session(conn, stdin, stdout)
		}
		sql := args[len(args)-1]
		switch {
		case strings.HasPrefix(sql, "select 1"):
			io.WriteString(stdout, "1\n")
		case strings.HasPrefix(sql, "show server_version"):
			io.WriteString(stdout, "17.11\n")
		case strings.HasPrefix(sql, "select datname"):
			io.WriteString(stdout, strings.Join(f.dbs, "\n")+"\n")
		case strings.HasPrefix(sql, "select current_user"):
			su, ra := "f", "f"
			if f.super {
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
		case strings.HasPrefix(sql, "select extname, extversion from pg_extension"):
			io.WriteString(stdout, "uuid-ossp\x1f1.1\n")
		default:
			return fmt.Errorf("fake: unexpected sql %q", sql)
		}
	case "pg_dump":
		if len(args) == 1 && args[0] == "--version" {
			io.WriteString(stdout, "pg_dump (PostgreSQL) 17.11\n")
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

// memStore is an in-memory bucket.
type memStore struct {
	mu                 sync.Mutex
	objects            map[string][]byte
	failPut            string   // Put fails for keys containing this string
	deletedWhileListed []string // keys deleted while manifest.json still points to them
	deleted            []string
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
	if bytes.Contains(m.objects["bracer/shop/manifest.json"], []byte(key)) {
		m.deletedWhileListed = append(m.deletedWhileListed, key)
	}
	delete(m.objects, key)
	m.deleted = append(m.deleted, key)
	return nil
}

func (m *memStore) manifest(t *testing.T) *Manifest {
	t.Helper()
	data, ok := m.objects["bracer/shop/manifest.json"]
	if !ok {
		t.Fatal("manifest.json missing")
	}
	var mf Manifest
	if err := json.Unmarshal(data, &mf); err != nil {
		t.Fatal(err)
	}
	return &mf
}

func baseCfg() *config.App {
	return &config.App{
		App:        "shop",
		Instances:  []config.Instance{{Name: "main", Container: "shop-db", User: "postgres", PgMajor: 17, Databases: config.Databases{All: true}}},
		Storage:    &config.Storage{Endpoint: "https://s3.example.com", Bucket: "b", Prefix: "bracer", AccessKeyEnv: "A", SecretKeyEnv: "S"},
		Encryption: "none",
		Retention:  &config.Retention{Keep: 2},
	}
}

func deps(st storage.Store, r pg.Runner) Deps {
	clock := time.Date(2026, 10, 16, 3, 0, 0, 0, time.UTC)
	return Deps{
		Store: st,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time {
			clock = clock.Add(time.Second)
			return clock
		},
		Connect: func(context.Context, *pg.Tools, config.Instance, *config.Secrets) (pg.Conn, pg.Runner, error) {
			return pg.Conn{User: "postgres", Database: "postgres"}, r, nil
		},
	}
}

func TestRunPlainDumpsAndManifest(t *testing.T) {
	st := newMemStore()
	r := &fakeRunner{dbs: []string{"postgres", "shop"}, payload: map[string]string{"postgres": "PGDMP-postgres", "shop": "PGDMP-shop"}, super: true}
	res, err := Run(context.Background(), baseCfg(), deps(st, r))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || len(res.Instances) != 1 || !res.Instances[0].OK || len(res.Instances[0].Databases) != 2 {
		b, _ := json.MarshalIndent(res, "", " ")
		t.Fatalf("result:\n%s", b)
	}
	var dumpKeys, globalKeys int
	for k, v := range st.objects {
		switch {
		case strings.HasSuffix(k, ".dump"):
			dumpKeys++
			if !strings.HasPrefix(k, "bracer/shop/main/") || !strings.HasPrefix(string(v), "PGDMP-") {
				t.Errorf("object %s = %q", k, v)
			}
		case strings.Contains(k, "/globals/") && strings.HasSuffix(k, ".sql"):
			globalKeys++
		}
	}
	if dumpKeys != 2 || globalKeys != 1 {
		t.Errorf("dumps=%d globals=%d (globals must run once per instance)", dumpKeys, globalKeys)
	}
	mf := st.manifest(t)
	im := mf.Instances["main"]
	if im == nil || im.PgVersion != "17.11" || !im.Globals.HasPasswords || im.Globals.Latest == "" {
		t.Fatalf("manifest instance: %+v", im)
	}
	shop := im.Databases["shop"]
	if shop == nil || len(shop.Dumps) != 1 || shop.Latest != shop.Dumps[0].TS {
		t.Fatalf("manifest shop: %+v", shop)
	}
	d := shop.Dumps[0]
	if d.Locale == nil || *d.Locale != (pg.Locale{Encoding: "UTF8", Collate: "C.UTF-8", Ctype: "C.UTF-8"}) {
		t.Errorf("manifest locale: %+v", d.Locale)
	}
	if len(d.Extensions) != 1 || d.Extensions[0] != (pg.Extension{Name: "uuid-ossp", Version: "1.1"}) {
		t.Errorf("manifest extensions: %+v", d.Extensions)
	}
	sum := sha256.Sum256([]byte("PGDMP-shop"))
	if d.Bytes != int64(len("PGDMP-shop")) || d.SHA256 != hex.EncodeToString(sum[:]) || d.Encrypted || d.Recipient != nil {
		t.Errorf("dump entry: %+v", d)
	}
	// row_counts auto: events (5e9) is only estimated; new_table (-1) and orders (<1e6) are counted exactly.
	// measurements is a partitioned parent (relkind p): not counted, no rows_est.
	want := map[string][2]*int64{"events": {nil, i64(5000000000)}, "measurements": {nil, nil}, "new_table": {i64(7), nil}, "orders": {i64(14300), nil}}
	if len(d.Tables) != 4 {
		t.Fatalf("tables: %+v", d.Tables)
	}
	for _, tb := range d.Tables {
		w := want[tb.Name]
		if !eqI64(tb.Rows, w[0]) || !eqI64(tb.RowsEst, w[1]) {
			t.Errorf("table %s: rows=%v rows_est=%v", tb.Name, deref(tb.Rows), deref(tb.RowsEst))
		}
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

func TestRowCountsModes(t *testing.T) {
	for _, tc := range []struct {
		mode               string
		max                int64
		events, orders, nt bool
	}{
		{"auto", 1_000_000, false, true, true},
		{"auto", 10_000, false, false, true},
		{"exact", 1_000_000, true, true, true},
		{"estimate", 1_000_000, false, false, false},
	} {
		r := &fakeRunner{dbs: []string{"shop"}, payload: map[string]string{"shop": "x"}, super: true}
		st := newMemStore()
		cfg := baseCfg()
		cfg.RowCounts, cfg.ExactMaxRows = tc.mode, tc.max
		res, err := Run(context.Background(), cfg, deps(st, r))
		if err != nil || !res.OK {
			t.Fatalf("%s: %v %+v", tc.mode, err, res)
		}
		d := st.manifest(t).Instances["main"].Databases["shop"].Dumps[0]
		got := map[string]bool{}
		for _, tb := range d.Tables {
			got[tb.Name] = tb.Rows != nil
			if (tb.Rows != nil) == (tb.RowsEst != nil) && tb.Name != "new_table" && tb.Name != "measurements" {
				t.Errorf("%s/%s: exactly one of rows/rows_est: %v %v", tc.mode, tb.Name, deref(tb.Rows), deref(tb.RowsEst))
			}
		}
		if got["events"] != tc.events || got["orders"] != tc.orders || got["new_table"] != tc.nt || got["measurements"] {
			t.Errorf("%s max=%d: counted %v", tc.mode, tc.max, got)
		}
	}
}

func TestRunEncryptedWithAge(t *testing.T) {
	kp, _ := crypto.Generate()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "shop.key")
	crypto.WriteIdentityFile(keyPath, kp)
	cfg := baseCfg()
	cfg.Encryption, cfg.AgeRecipient, cfg.AgeIdentityFile = "age", kp.Recipient, keyPath
	st := newMemStore()
	r := &fakeRunner{dbs: []string{"shop"}, payload: map[string]string{"shop": strings.Repeat("PGDMP-shop ", 1000)}, super: true}
	res, err := Run(context.Background(), cfg, deps(st, r))
	if err != nil || !res.OK {
		t.Fatal(err, res)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", res.Warnings)
	}
	mf := st.manifest(t)
	d := mf.Instances["main"].Databases["shop"].Dumps[0]
	if !strings.HasSuffix(d.Key, ".dump.age") || !d.Encrypted || d.Recipient == nil || *d.Recipient != kp.Recipient {
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
	ids, _, _ := crypto.ReadIdentities(keyPath)
	rd, err := crypto.Decrypt(bytes.NewReader(stored), ids)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(rd)
	if string(plain) != r.payload["shop"] {
		t.Error("decrypted payload differs")
	}
	// Private key gone → still backs up, with a warning.
	os.Remove(keyPath)
	res, _ = Run(context.Background(), cfg, deps(newMemStore(), r))
	if !res.OK || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "drill") {
		t.Errorf("missing identity must warn but still back up: ok=%v warnings=%v", res.OK, res.Warnings)
	}
}

func TestRunOneDatabaseFailsOthersContinue(t *testing.T) {
	st := newMemStore()
	r := &fakeRunner{dbs: []string{"bad", "good"}, payload: map[string]string{"good": "PGDMP-good"}, failDump: "bad"}
	res, err := Run(context.Background(), baseCfg(), deps(st, r))
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || res.Instances[0].OK {
		t.Error("result must be not ok")
	}
	bad, good := res.Instances[0].Databases[0], res.Instances[0].Databases[1]
	if bad.OK || !strings.Contains(bad.Error, "row-level security") {
		t.Errorf("bad: %+v (stderr of pg_dump must be kept)", bad)
	}
	if !good.OK {
		t.Errorf("good must still be dumped: %+v", good)
	}
	mf := st.manifest(t)
	if _, exists := mf.Instances["main"].Databases["bad"]; exists {
		t.Error("failed dump must not enter the manifest")
	}
	for k := range st.objects {
		if strings.Contains(k, "/bad/") {
			t.Errorf("partial object stored: %s", k)
		}
	}
	// User without pg_read_all_data → globals without passwords.
	if res.Instances[0].Globals == nil || res.Instances[0].Globals.HasPasswords {
		t.Errorf("globals: %+v", res.Instances[0].Globals)
	}
	if !contains(r.calls, "pg_dumpall --globals-only --no-role-passwords --lock-wait-timeout=60000 db=postgres") {
		t.Errorf("calls: %v", r.calls)
	}
}

func TestRetentionDeletesOldestAfterUploadOnly(t *testing.T) {
	st := newMemStore()
	// Old manifest with 2 dumps of shop (keep=2) and 1 dump of events (keep=5 per database).
	old := newManifest("shop")
	im := old.instance("main")
	for i, ts := range []string{"2026-10-14T030000Z", "2026-10-15T030000Z"} {
		key := "bracer/shop/main/shop/" + ts + ".dump"
		st.objects[key] = []byte("old")
		im.database("shop").addDump(&Dump{TS: ts, Key: key, UploadedAt: time.Date(2026, 10, 14+i, 3, 0, 0, 0, time.UTC)})
	}
	st.objects["bracer/shop/main/events/2026-10-15T030000Z.dump"] = []byte("old")
	im.database("events").addDump(&Dump{TS: "2026-10-15T030000Z", Key: "bracer/shop/main/events/2026-10-15T030000Z.dump", UploadedAt: time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC)})
	SaveManifest(context.Background(), st, "bracer/shop/manifest.json", old, time.Now())

	cfg := baseCfg()
	cfg.Instances[0].Databases = config.Databases{List: []config.Database{{Name: "shop"}, {Name: "events", Retention: &config.Retention{Keep: 5}}}}
	r := &fakeRunner{payload: map[string]string{"shop": "new", "events": "new"}, super: true}
	res, err := Run(context.Background(), cfg, deps(st, r))
	if err != nil || !res.OK {
		t.Fatal(err, res)
	}
	if len(st.deleted) != 1 || st.deleted[0] != "bracer/shop/main/shop/2026-10-14T030000Z.dump" {
		t.Errorf("deleted = %v (only the oldest shop dump beyond keep=2)", st.deleted)
	}
	if len(st.deletedWhileListed) != 0 {
		t.Errorf("manifest must be rewritten before objects are deleted: %v", st.deletedWhileListed)
	}
	mf := st.manifest(t)
	if n := len(mf.Instances["main"].Databases["shop"].Dumps); n != 2 {
		t.Errorf("shop dumps in manifest = %d", n)
	}
	if n := len(mf.Instances["main"].Databases["events"].Dumps); n != 2 {
		t.Errorf("events dumps = %d (keep=5, nothing pruned)", n)
	}

	// Upload fails → nothing is deleted.
	st2 := newMemStore()
	for k, v := range st.objects {
		st2.objects[k] = v
	}
	st2.failPut = "/shop/2026-10-16"
	res, err = Run(context.Background(), cfg, deps(st2, r))
	if err != nil || res.OK {
		t.Fatal("put failure must make the run not ok", err)
	}
	if len(st2.deleted) != 0 {
		t.Errorf("nothing may be deleted when upload failed: %v", st2.deleted)
	}
}

func TestAllDiffAndCorruptManifest(t *testing.T) {
	st := newMemStore()
	old := newManifest("shop")
	old.instance("main").database("gone")
	old.instance("main").database("shop")
	SaveManifest(context.Background(), st, "bracer/shop/manifest.json", old, time.Now())
	r := &fakeRunner{dbs: []string{"shop", "fresh"}, payload: map[string]string{"shop": "x", "fresh": "y"}, super: true}
	res, err := Run(context.Background(), baseCfg(), deps(st, r))
	if err != nil {
		t.Fatal(err)
	}
	ir := res.Instances[0]
	if len(ir.Added) != 1 || ir.Added[0] != "fresh" || len(ir.Removed) != 1 || ir.Removed[0] != "gone" {
		t.Errorf("added=%v removed=%v", ir.Added, ir.Removed)
	}

	st.objects["bracer/shop/manifest.json"] = []byte("{not json")
	res, err = Run(context.Background(), baseCfg(), deps(st, r))
	if err != nil || !res.OK || len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "skipping retention") || !strings.Contains(res.Warnings[0], "copied to bracer/shop/manifest.json.corrupt-") {
		t.Errorf("corrupt manifest: err=%v ok=%v warnings=%v", err, res.OK, res.Warnings)
	}
	copies := 0
	for k, v := range st.objects {
		if strings.HasPrefix(k, "bracer/shop/manifest.json.corrupt-") && string(v) == "{not json" {
			copies++
		}
	}
	if copies != 1 {
		t.Errorf("corrupt manifest must be kept as a copy, found %d", copies)
	}
	if _, err := LoadManifest(context.Background(), st, "bracer/shop/manifest.json", "shop"); err != nil {
		t.Error("manifest must be rewritten as valid JSON after the run")
	}
	// The corrupt copy cannot be written → the manifest is not overwritten.
	st.objects["bracer/shop/manifest.json"] = []byte("{still not json")
	st.failPut = ".corrupt-"
	res, err = Run(context.Background(), baseCfg(), deps(st, r))
	if err != nil || string(st.objects["bracer/shop/manifest.json"]) != "{still not json" || !strings.Contains(strings.Join(res.Warnings, "\n"), "left untouched") {
		t.Errorf("manifest must stay untouched when the copy fails: err=%v obj=%q warnings=%v", err, st.objects["bracer/shop/manifest.json"], res.Warnings)
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

func TestRetentionNeverPrunesThisRunAndWarnsOrphans(t *testing.T) {
	st := newMemStore()
	// Two old dumps with uploaded_at in the future (the machine clock ran ahead when they were made).
	old := newManifest("shop")
	im := old.instance("main")
	for i, ts := range []string{"2026-10-20T030000Z", "2026-10-21T030000Z"} {
		key := "bracer/shop/main/shop/" + ts + ".dump"
		st.objects[key] = []byte("old")
		im.database("shop").addDump(&Dump{TS: ts, Key: key, UploadedAt: time.Date(2026, 10, 20+i, 3, 0, 0, 0, time.UTC)})
	}
	// One orphan object and a key outside the prefix in the manifest.
	st.objects["bracer/shop/main/shop/stray.dump"] = []byte("?")
	im.database("other").addDump(&Dump{TS: "2026-01-01T000000Z", Key: "elsewhere/x.dump", UploadedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	im.database("other").addDump(&Dump{TS: "2026-01-02T000000Z", Key: "elsewhere/y.dump", UploadedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)})
	im.database("other").addDump(&Dump{TS: "2026-01-03T000000Z", Key: "elsewhere/z.dump", UploadedAt: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)})
	st.objects["bracer/shop/main/other/2026-01-04T000000Z.dump"] = []byte("old")
	im.database("other").addDump(&Dump{TS: "2026-01-04T000000Z", Key: "bracer/shop/main/other/2026-01-04T000000Z.dump", UploadedAt: time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)})
	SaveManifest(context.Background(), st, "bracer/shop/manifest.json", old, time.Now())

	cfg := baseCfg() // keep 2
	cfg.Instances[0].Databases = config.Databases{List: []config.Database{{Name: "shop"}, {Name: "other"}}}
	r := &fakeRunner{payload: map[string]string{"shop": "new", "other": "new"}, super: true}
	res, err := Run(context.Background(), cfg, deps(st, r))
	if err != nil || !res.OK {
		t.Fatal(err, res)
	}
	// shop: 3 dumps, keep 2 → the newest by the clock (2026-10-16, "oldest" when sorted) is the one just created: not deleted, stop.
	// other: 3 foreign keys + 1 real old dump + the new one, keep 2 → foreign keys are kept (warning) but do not block: the real old dump is deleted.
	if strings.Join(st.deleted, ",") != "bracer/shop/main/other/2026-01-04T000000Z.dump" {
		t.Errorf("only the old real dump of `other` may be deleted (never this run's, never keys outside the prefix): %v", st.deleted)
	}
	warned := strings.Join(res.Warnings, "\n")
	for _, k := range []string{"x", "y", "z"} {
		if !strings.Contains(warned, "elsewhere/"+k+".dump is outside bracer/shop/") {
			t.Errorf("key outside prefix must be refused: %v", res.Warnings)
		}
	}
	if m, _ := LoadManifest(context.Background(), st, "bracer/shop/manifest.json", "shop"); m == nil || len(m.Instances["main"].Databases["other"].Dumps) != 4 {
		t.Errorf("manifest must keep the 3 foreign keys and the new dump: %+v", m)
	}
	if !strings.Contains(warned, "1 object(s) under bracer/shop/ are not in manifest.json (e.g. bracer/shop/main/shop/stray.dump)") {
		t.Errorf("orphan warning missing: %v", res.Warnings)
	}
	if _, ok := st.objects["bracer/shop/main/shop/stray.dump"]; !ok {
		t.Error("orphans must be left untouched")
	}
}

// Container with password_env (Bitnami demands a password even over the socket): the
// password must go into Conn so ExecRunner passes -e PGPASSWORD; pod with password_env → clear error.
func TestTargetPasswordForContainerNotPod(t *testing.T) {
	sec := config.NewSecrets(map[string]string{"PW": "s3cret"}, "test")
	_, conn, err := Target(config.Instance{Name: "c", Container: "db", User: "postgres", PasswordEnv: "PW", Databases: config.Databases{All: true}}, sec)
	if err != nil || conn.Password != "s3cret" {
		t.Fatalf("container: conn=%+v err=%v", conn, err)
	}
	_, conn, err = Target(config.Instance{Name: "c", Container: "db", User: "postgres", Databases: config.Databases{All: true}}, sec)
	if err != nil || conn.Password != "" {
		t.Fatalf("container without password_env: conn=%+v err=%v", conn, err)
	}
	if _, _, err = Target(config.Instance{Name: "p", Pod: "ns/pg", User: "postgres", PasswordEnv: "PW", Databases: config.Databases{All: true}}, sec); err == nil || !strings.Contains(err.Error(), "cannot be passed to a pod") {
		t.Fatalf("pod with password_env: err=%v", err)
	}
}
