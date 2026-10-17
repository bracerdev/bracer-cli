package restore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	"bracer.dev/bracer/internal/storage"
)

// fakeServer fakes a real Postgres behind a Runner: database → tables; pg_restore
// loads tables according to the dump content it receives.
type fakeServer struct {
	mu           sync.Mutex
	dbs          map[string][]pg.Table
	dumps        map[string][]pg.Table // content → tables
	version      string
	pooler       bool
	calls        []string
	failRestore  bool
	roles        map[string]bool
	globalsSQL   string            // SQL last received by psql -f -
	dfAvailKB    int64             // 0 = no df
	readOnly     bool              // target user has no CREATEDB/CREATEROLE
	toolVersion  string            // pg_dump --version on the target (default 17.6)
	createDBOnly bool              // target user has CREATEDB, no CREATEROLE, not superuser
	createOpts   string            // what followed TEMPLATE template0 in the latest CREATE DATABASE
	available    map[string]string // pg_available_extensions on the target
}

func newFakeServer() *fakeServer {
	return &fakeServer{
		dbs:       map[string][]pg.Table{"postgres": nil, "shop": {{Schema: "public", Name: "orders", Rows: i64(500)}}},
		dumps:     map[string][]pg.Table{"PGDMP-shop": {{Schema: "public", Name: "orders", Rows: i64(500)}}},
		version:   "17.6",
		roles:     map[string]bool{"postgres": true, "pg_read_all_data": true},
		dfAvailKB: 50 << 20, // 50 GB
	}
}

// DiskFree: the fake can measure, like docker exec.
func (f *fakeServer) DiskFree(_ context.Context, dir string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "df -Pk "+dir)
	if f.dfAvailKB == 0 {
		return 0, fmt.Errorf("df: not found")
	}
	return f.dfAvailKB * 1024, nil
}

func (f *fakeServer) Describe() string { return "fake server" }

func (f *fakeServer) Run(ctx context.Context, conn pg.Conn, tool string, args []string, stdin io.Reader, stdout io.Writer) error {
	_, err := f.RunStderr(ctx, conn, tool, args, stdin, stdout)
	return err
}

func (f *fakeServer) RunStderr(_ context.Context, conn pg.Conn, tool string, args []string, stdin io.Reader, stdout io.Writer) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, tool+" "+strings.Join(args, " ")+" db="+conn.Database)
	write := func(s string) { io.WriteString(stdout, s) }
	switch tool {
	case "psql":
		if args[len(args)-1] == "-" {
			data, _ := io.ReadAll(stdin)
			f.globalsSQL = string(data)
			var stderr []byte
			for _, line := range strings.Split(string(data), "\n") {
				if name, ok := strings.CutPrefix(strings.TrimSpace(line), "CREATE ROLE "); ok {
					name = strings.Trim(strings.TrimSuffix(name, ";"), `"`)
					if f.roles[name] {
						stderr = append(stderr, []byte(`ERROR:  role "`+name+`" already exists`+"\n")...)
						continue
					}
					f.roles[name] = true
				}
			}
			return stderr, nil
		}
		sql := args[len(args)-1]
		tables := f.dbs[conn.Database]
		switch {
		case sql == "show server_version":
			write(f.version + "\n")
		case sql == "select current_user, rolsuper, rolcreatedb, rolcreaterole from pg_roles where rolname = current_user":
			if f.readOnly {
				write("df_backup\x1ff\x1ff\x1ff\n")
			} else if f.createDBOnly {
				write("deployer\x1ff\x1ft\x1ff\n")
			} else {
				write("postgres\x1ft\x1ft\x1ft\n")
			}
		case strings.HasPrefix(sql, "select name, coalesce(default_version, '') from pg_available_extensions"):
			for n, v := range f.available {
				write(n + "\x1f" + v + "\n")
			}
		case sql == "show data_directory":
			write("/var/lib/postgresql/data\n")
		case sql == "select rolname from pg_roles order by rolname":
			for name := range f.roles {
				write(name + "\n")
			}
		case sql == "show server_version_num":
			if _, ok := f.dbs[conn.Database]; !ok {
				return []byte(`FATAL:  database "` + conn.Database + `" does not exist`), &pg.ExitError{Cmd: "psql", Code: 2}
			}
			write("170006\n")
		case sql == "select version()":
			if f.pooler {
				write("PgBouncer 1.22\n")
			} else {
				write("PostgreSQL " + f.version + "\n")
			}
		case sql == "show pool_mode":
			return []byte("ERROR"), &pg.ExitError{Cmd: "psql", Code: 1}
		case strings.HasPrefix(sql, "select 1 from pg_database where datname = "):
			name := strings.Trim(strings.TrimPrefix(sql, "select 1 from pg_database where datname = "), "'")
			if _, ok := f.dbs[name]; ok {
				write("1\n")
			}
		case strings.HasPrefix(sql, "create database "):
			name, opts, _ := strings.Cut(strings.TrimPrefix(sql, "create database "), " template template0")
			name = strings.Trim(name, `"`)
			f.createOpts = opts
			if _, ok := f.dbs[name]; ok {
				return []byte("ERROR:  database exists"), &pg.ExitError{Cmd: "psql", Code: 1}
			}
			f.dbs[name] = nil
		case sql == "analyze":
		case strings.HasPrefix(sql, "select n.nspname, c.relname"):
			for _, t := range tables {
				write(fmt.Sprintf("%s\x1f%s\x1f-1\n", t.Schema, t.Name))
			}
		case strings.HasPrefix(sql, "select count(*) from "):
			ident := strings.TrimPrefix(sql, "select count(*) from ")
			for _, t := range tables {
				if t.Ident() == ident {
					write(fmt.Sprintf("%d\n", *t.Rows))
				}
			}
		default:
			return nil, fmt.Errorf("fake: unexpected sql %q", sql)
		}
		return nil, nil
	case "pg_dump":
		if len(args) == 1 && args[0] == "--version" {
			v := f.toolVersion
			if v == "" {
				v = "17.6"
			}
			write("pg_dump (PostgreSQL) " + v + "\n")
			return nil, nil
		}
		return nil, fmt.Errorf("fake: unexpected pg_dump %v", args)
	case "pg_restore":
		if f.failRestore {
			return []byte("pg_restore: error: could not execute query: ERROR:  role \"app\" does not exist"), &pg.ExitError{Cmd: "pg_restore", Code: 1}
		}
		data, _ := io.ReadAll(stdin)
		tables, ok := f.dumps[string(data)]
		if !ok {
			return []byte("pg_restore: error: input file does not appear to be a valid archive"), &pg.ExitError{Cmd: "pg_restore", Code: 1}
		}
		if _, exists := f.dbs[conn.Database]; !exists {
			return []byte("database does not exist"), &pg.ExitError{Cmd: "pg_restore", Code: 1}
		}
		f.dbs[conn.Database] = tables
		return nil, nil
	}
	return nil, fmt.Errorf("fake: unexpected tool %s", tool)
}

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

func sha(b []byte) string {
	return fmt.Sprintf("%x", sha256sum(b))
}

func seed(t *testing.T, st *memStore, recipient string) {
	t.Helper()
	ctx := context.Background()
	put := func(key, content string) string {
		var buf bytes.Buffer
		if recipient != "" {
			w, _ := crypto.Encrypt(&buf, recipient)
			io.WriteString(w, content)
			w.Close()
		} else {
			buf.WriteString(content)
		}
		st.Put(ctx, key, bytes.NewReader(buf.Bytes()), int64(buf.Len()))
		return sha(buf.Bytes())
	}
	var rp *string
	if recipient != "" {
		rp = &recipient
	}
	s1 := put("bracer/shop/main/shop/2026-10-16T030000Z.dump", "PGDMP-shop")
	s0 := put("bracer/shop/main/shop/2026-10-15T030000Z.dump", "PGDMP-old")
	sg := put("bracer/shop/main/globals/2026-10-16T030000Z.sql", globalsSQL)
	m := &backup.Manifest{App: "shop", Instances: map[string]*backup.InstanceManifest{
		"main": {PgVersion: "17.6", Databases: map[string]*backup.DatabaseManifest{
			"shop": {Latest: "2026-10-16T030000Z", Dumps: []*backup.Dump{
				{TS: "2026-10-16T030000Z", Key: "bracer/shop/main/shop/2026-10-16T030000Z.dump", Bytes: 10, SHA256: s1, Encrypted: recipient != "", Recipient: rp,
					Locale: &pg.Locale{Encoding: "UTF8", Collate: "en_US.utf8", Ctype: "en_US.utf8"},
					Tables: []pg.Table{{Schema: "public", Name: "orders", Rows: i64(500)}}},
				{TS: "2026-10-15T030000Z", Key: "bracer/shop/main/shop/2026-10-15T030000Z.dump", Bytes: 9, SHA256: s0, Encrypted: recipient != "", Recipient: rp},
			}},
		}, Globals: backup.GlobalsManifest{Latest: "2026-10-16T030000Z", Dumps: []*backup.Dump{
			{TS: "2026-10-16T030000Z", Key: "bracer/shop/main/globals/2026-10-16T030000Z.sql", SHA256: sg, Encrypted: recipient != "", Recipient: rp},
		}}},
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

func deps(f *fakeServer, st storage.Store) Deps {
	clock := time.Date(2026, 10, 17, 3, 0, 0, 0, time.UTC)
	return Deps{
		Shell: pg.ExecShell{}, Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Secrets: config.NewSecrets(map[string]string{"BRACER_RESTORE_URL": "postgres://u:p@newhost:5433/postgres"}, "test"), GOOS: "darwin",
		Now: func() time.Time { clock = clock.Add(time.Second); return clock },
		Connect: func(context.Context, *pg.Tools, config.Instance, *config.Secrets) (pg.Conn, pg.Runner, error) {
			return pg.Conn{User: "postgres", Database: "postgres"}, f, nil
		},
	}
}

func TestSelectAndList(t *testing.T) {
	st := newMemStore()
	seed(t, st, "")
	m, _ := backup.LoadManifest(context.Background(), st, "bracer/shop/manifest.json", "shop")
	cfg := baseCfg()
	sel, err := Select(m, cfg, "", "shop", "", false)
	if err != nil || sel.Instance != "main" || sel.Dump.TS != "2026-10-16T030000Z" {
		t.Fatalf("latest: %+v %v", sel, err)
	}
	if sel, err = Select(m, cfg, "main", "shop", "2026-10-15T030000Z", false); err != nil || sel.Dump.TS != "2026-10-15T030000Z" {
		t.Errorf("by ts: %+v %v", sel, err)
	}
	if sel, err = Select(m, cfg, "", "", "", true); err != nil || !sel.Globals || !strings.HasSuffix(sel.Dump.Key, ".sql") {
		t.Errorf("globals: %+v %v", sel, err)
	}
	for _, tc := range []struct {
		in, db, ts string
		want       string
	}{
		{"", "nope", "", "databases in the bucket: shop"},
		{"", "shop", "2020-01-01T000000Z", "available: 2026-10-16T030000Z, 2026-10-15T030000Z"},
		{"ghost", "shop", "", "not in app.yaml"},
		{"", "", "", "pass --database"},
	} {
		_, err := Select(m, cfg, tc.in, tc.db, tc.ts, false)
		var ue *UsageError
		if err == nil || !strings.Contains(err.Error(), tc.want) || !asUsage(err, &ue) {
			t.Errorf("Select(%q,%q,%q) = %v, want %q usage error", tc.in, tc.db, tc.ts, err, tc.want)
		}
	}
	cfg.Instances = append(cfg.Instances, config.Instance{Name: "second", Container: "x", Databases: config.Databases{All: true}})
	m.Instances["second"] = &backup.InstanceManifest{Databases: map[string]*backup.DatabaseManifest{"d": {Latest: "t", Dumps: []*backup.Dump{{TS: "t"}}}}}
	if _, err := Select(m, cfg, "", "shop", "", false); err == nil || !strings.Contains(err.Error(), "pass --instance") {
		t.Errorf("several instances: %v", err)
	}
	entries := List(m)
	if len(entries) != 4 || entries[0].Database != "shop" || !entries[0].Latest || entries[1].Latest || entries[2].Database != "globals" {
		t.Errorf("list: %+v", entries)
	}
}

func TestFetchPlainEncryptedRawAndChecksum(t *testing.T) {
	ctx := context.Background()
	kp, _ := crypto.Generate()
	keyPath := filepath.Join(t.TempDir(), "k")
	crypto.WriteIdentityFile(keyPath, kp)
	ids := ReadIdentities(keyPath, crypto.ReadIdentities)

	st := newMemStore()
	seed(t, st, kp.Recipient)
	m, _ := backup.LoadManifest(ctx, st, "bracer/shop/manifest.json", "shop")
	dp := m.Instances["main"].Databases["shop"].Dumps[0]

	var out bytes.Buffer
	n, err := Fetch(ctx, st, dp, ids, false, &out)
	if err != nil || out.String() != "PGDMP-shop" || n != 10 {
		t.Fatalf("decrypted fetch: %v %q", err, out.String())
	}
	out.Reset()
	if _, err := Fetch(ctx, st, dp, &Identities{Err: os.ErrNotExist}, true, &out); err != nil || !bytes.HasPrefix(out.Bytes(), []byte("age-encryption.org/v1")) {
		t.Errorf("raw fetch must not need a key: %v %q", err, out.Bytes()[:min(20, out.Len())])
	}
	if _, err := Fetch(ctx, st, dp, &Identities{Err: os.ErrNotExist}, false, io.Discard); err == nil || !strings.Contains(err.Error(), kp.Recipient) {
		t.Errorf("missing key: %v", err)
	}
	dp.SHA256 = "deadbeef"
	var ce *ErrChecksum
	if _, err := Fetch(ctx, st, dp, ids, false, io.Discard); err == nil || !asChecksum(err, &ce) || ce.Want != "deadbeef" {
		t.Errorf("checksum: %v", err)
	}
}

func TestRestoreIntoNewDatabase(t *testing.T) {
	f := newFakeServer()
	st := newMemStore()
	seed(t, st, "")
	res, err := Run(context.Background(), baseCfg(), Options{Database: "shop", Into: "main/shop_restored", WithGlobals: true}, deps(f, st))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Target.DB != "shop_restored" || res.Target.Exists || res.TablesFound != 1 || res.TablesExpected != 1 || res.Target.PgVersion != "17.6" {
		t.Errorf("res: %+v", res)
	}
	// Globals: only the missing users are created (app, "Odd Role"); postgres is untouched.
	if res.Globals == nil || !res.Globals.OK || strings.Join(res.Globals.Created, ",") != "app,Odd Role" || res.Globals.SkippedExisting != 1 {
		t.Errorf("globals: %+v", res.Globals)
	}
	if strings.Join(res.Globals.SkippedGrants, ",") != `postgres -> Odd Role` ||
		strings.Join(res.Globals.Downgraded, ",") != "Odd Role: SUPERUSER,Odd Role: CREATEROLE,app: GRANT SET ON PARAMETER log_min_duration_statement" ||
		strings.Join(res.Globals.PgGrants, ",") != "pg_read_all_data -> app" ||
		strings.Join(res.Globals.SkippedMissing, ",") != "pg_maintain -> app" {
		t.Errorf("globals detail: %+v", res.Globals)
	}
	// Preamble emitted by Bracer (the file's \restrict, \set, SELECT set_config are not copied);
	// every statement is rebuilt from tokens, user names quoted; pg_* grants have no WITH.
	want := `SET standard_conforming_strings = on;
SET default_transaction_read_only = off;
SET client_encoding = 'UTF8';
CREATE ROLE "app";
ALTER ROLE "app" WITH NOSUPERUSER INHERIT LOGIN PASSWORD 'SCRAM-SHA-256$y';
COMMENT ON ROLE "app" IS 'line one
line two; ALTER ROLE postgres NOLOGIN';
CREATE ROLE "Odd Role";
ALTER ROLE "Odd Role" WITH NOLOGIN NOSUPERUSER NOCREATEROLE;
GRANT "pg_read_all_data" TO "app";
GRANT "app" TO "Odd Role" WITH INHERIT TRUE;
ALTER ROLE "app" IN DATABASE shop SET search_path TO 'app';
`
	if f.globalsSQL != want {
		t.Errorf("filtered globals sent to psql:\n%s\nwant:\n%s", f.globalsSQL, want)
	}
	warned := strings.Join(res.Warnings, "\n")
	for _, w := range []string{"skipped GRANT postgres -> Odd Role", "Odd Role: SUPERUSER", "Odd Role: CREATEROLE", "GRANT pg_read_all_data -> app gives", "skipped GRANT pg_maintain -> app: that built-in role does not exist"} {
		if !strings.Contains(warned, w) {
			t.Errorf("missing warning %q in %v", w, res.Warnings)
		}
	}
	if !f.roles["app"] || !f.roles["Odd Role"] {
		t.Errorf("roles after: %v", f.roles)
	}
	if res.Disk == nil || !res.Disk.Checked || res.Disk.DataDirectory != "/var/lib/postgresql/data" || res.Disk.NeedBytes != 50 {
		t.Errorf("disk: %+v", res.Disk)
	}
	if _, ok := f.dbs["shop_restored"]; !ok || len(f.dbs["shop_restored"]) != 1 {
		t.Errorf("fake dbs: %v", f.dbs)
	}
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, `create database "shop_restored" template template0`) || !strings.Contains(joined, "pg_restore -d dbname='shop_restored' --no-data-for-failed-tables db=shop_restored") {
		t.Errorf("calls:\n%s", joined)
	}
}

func TestRestoreRefusesExisting(t *testing.T) {
	f := newFakeServer()
	st := newMemStore()
	seed(t, st, "")
	res, err := Run(context.Background(), baseCfg(), Options{Database: "shop", Into: "main"}, deps(f, st))
	if !errors.Is(err, ErrExists) || !strings.Contains(err.Error(), "already exists on main") || !strings.Contains(err.Error(), "never drops") || strings.Contains(err.Error(), "--drop") {
		t.Fatalf("err = %v", err)
	}
	if res == nil || !res.Target.Exists {
		t.Errorf("res: %+v", res)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "pg_restore") || strings.Contains(c, "create database") {
			t.Errorf("must not change anything: %s", c)
		}
	}
}

func TestRestoreDiskCheck(t *testing.T) {
	st := newMemStore()
	seed(t, st, "")
	// Not enough space: dump 10 B × 5 = 50 B > 40 B free → ErrDiskSpace, nothing created.
	f := newFakeServer()
	f.dfAvailKB = 0 // df broken → cannot measure → warn only
	res, err := Run(context.Background(), baseCfg(), Options{Database: "shop", Into: "main/shop_new"}, deps(f, st))
	if err != nil || !res.OK || res.Disk.Checked || len(res.Warnings) == 0 {
		t.Errorf("unmeasurable disk must only warn: %v %+v", err, res)
	}
	f = newFakeServer()
	f.dfAvailKB = 1 // 1 KB free but NeedBytes = 50 B → enough
	if res, err := Run(context.Background(), baseCfg(), Options{Database: "shop", Into: "main/shop_new"}, deps(f, st)); err != nil || !res.OK || !res.Disk.Checked {
		t.Errorf("enough: %v %+v", err, res)
	}
	// Dump larger than the free space.
	f = newFakeServer()
	f.dfAvailKB = 1
	m, _ := backup.LoadManifest(context.Background(), st, "bracer/shop/manifest.json", "shop")
	m.Instances["main"].Databases["shop"].Dumps[0].Bytes = 1 << 20
	backup.SaveManifest(context.Background(), st, "bracer/shop/manifest.json", m, time.Now())
	res, err = Run(context.Background(), baseCfg(), Options{Database: "shop", Into: "main/shop_new"}, deps(f, st))
	if !errors.Is(err, ErrDiskSpace) || !strings.Contains(err.Error(), "--skip-disk-check") {
		t.Fatalf("err = %v", err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "pg_restore") || strings.Contains(c, "create database") {
			t.Errorf("must not change anything: %s", c)
		}
	}
	f = newFakeServer()
	f.dfAvailKB = 1
	res, err = Run(context.Background(), baseCfg(), Options{Database: "shop", Into: "main/shop_new", SkipDiskCheck: true}, deps(f, st))
	if err != nil || !res.OK || len(res.Warnings) == 0 {
		t.Errorf("--skip-disk-check: %v %+v", err, res)
	}
}

func TestFilterGlobals(t *testing.T) {
	have := map[string]bool{"postgres": true, "pg_read_all_data": true}
	plan, err := FilterGlobals([]byte(globalsSQL), have)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(plan.Created, ",") != "app,Odd Role" || plan.SkippedExisting != 1 {
		t.Errorf("created=%v skipped=%d", plan.Created, plan.SkippedExisting)
	}
	o := string(plan.SQL)
	for _, bad := range []string{`ROLE "postgres"`, "TABLESPACE", "GRANTED BY", "ADMIN", "PARAMETER", "pg_maintain", `\restrict`, `\set`, "set_config", `"Odd Role" WITH NOLOGIN SUPERUSER`} {
		if strings.Contains(o, bad) {
			t.Errorf("must drop %q:\n%s", bad, o)
		}
	}

	// Preamble Bracer always emits before every statement.
	const P = "SET standard_conforming_strings = on;\nSET default_transaction_read_only = off;\n"
	// Each tricky case, one small file per case.
	cases := []struct {
		name, sql, want string
		err             bool
	}{
		{"role name with TO inside", `CREATE ROLE "x TO y"; GRANT "x TO y" TO postgres;`, "CREATE ROLE \"x TO y\";\n", false},
		{"existing role quoted differently", `ALTER ROLE Postgres WITH SUPERUSER;`, "", false},
		{"unquoted ident lowercased", `CREATE ROLE Admin; ALTER ROLE Admin WITH LOGIN;`, "CREATE ROLE \"admin\";\nALTER ROLE \"admin\" WITH LOGIN;\n", false},
		{"two statements one line", `CREATE ROLE a; ALTER ROLE postgres NOLOGIN;`, "CREATE ROLE \"a\";\n", false},
		{"grant to new role from existing role", `CREATE ROLE a; GRANT postgres TO a;`, "CREATE ROLE \"a\";\n", false},
		{"grant from new role", `CREATE ROLE a; CREATE ROLE b; GRANT a TO b;`, "CREATE ROLE \"a\";\nCREATE ROLE \"b\";\nGRANT \"a\" TO \"b\";\n", false},
		{"replication and bypassrls downgraded", `CREATE ROLE a; ALTER ROLE a WITH REPLICATION BYPASSRLS CONNECTION LIMIT 5 VALID UNTIL '2030-01-01';`, "CREATE ROLE \"a\";\nALTER ROLE \"a\" WITH NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 5 VALID UNTIL '2030-01-01';\n", false},
		{"createrole downgraded", `CREATE ROLE a CREATEROLE CREATEDB;`, "CREATE ROLE \"a\" WITH NOCREATEROLE CREATEDB;\n", false},
		{"escape string literal", `CREATE ROLE a; COMMENT ON ROLE a IS E'it\'s; DROP ROLE postgres';`, "CREATE ROLE \"a\";\nCOMMENT ON ROLE \"a\" IS E'it\\'s; DROP ROLE postgres';\n", false},
		{"backslash meta other than restrict", "\\! id\nCREATE ROLE a;", "", true},
		{"restrict line with a second meta-command", "\\restrict abc \\echo hi\nCREATE ROLE a;\n\\unrestrict abc\n", "CREATE ROLE \"a\";\n", false},
		{"dollar quoting", `CREATE ROLE a; COMMENT ON ROLE a IS $$x$$;`, "", true},
		{"unknown statement", `CREATE ROLE a; DROP DATABASE shop;`, "", true},
		{"unterminated", `CREATE ROLE a`, "", true},
		{"unknown role option", `CREATE ROLE a IN ROLE postgres;`, "", true},
		{"grant on tablespace dropped", `GRANT ALL ON TABLESPACE fast TO a;`, "", false},
		{"grant on table", `GRANT SELECT ON TABLE t TO a;`, "", true},
		{"grant on parameter dropped", `CREATE ROLE a; GRANT SET, ALTER SYSTEM ON PARAMETER work_mem TO a;`, "CREATE ROLE \"a\";\n", false},
		{"in database setting", `CREATE ROLE a; ALTER ROLE a IN DATABASE shop SET search_path TO 'x';`, "CREATE ROLE \"a\";\nALTER ROLE \"a\" IN DATABASE shop SET search_path TO 'x';\n", false},
		{"alter role all", `ALTER ROLE ALL SET log_statement TO 'all';`, "", true},
		{"alter role current_user", `ALTER ROLE current_user SET log_statement TO 'all';`, "", true},
		{"grant to session_user", `CREATE ROLE a; GRANT a TO session_user;`, "", true},
		{"quoted ALL is a real name", `CREATE ROLE "ALL";`, "CREATE ROLE \"ALL\";\n", false},
		{"set standard_conforming_strings off", `SET standard_conforming_strings = off;`, "", true},
		{"set other parameter", `SET search_path = public;`, "", true},
		{"client_encoding client-only", `SET client_encoding = 'SJIS';`, "", true},
		{"client_encoding bare and lowercase", `SET client_encoding = latin1;`, "SET client_encoding = 'LATIN1';\n", false},
		{"select with extra call", `SELECT pg_catalog.set_config('search_path', '', false), pg_sleep(1);`, "", true},
		{"select set_config dropped", `SELECT pg_catalog.set_config('search_path', '', false);`, "", false},
		{"pg role missing on target", `CREATE ROLE a; GRANT pg_maintain TO a WITH ADMIN OPTION;`, "CREATE ROLE \"a\";\n", false},
		{"pg grant loses options", `CREATE ROLE a; GRANT pg_read_all_data TO a WITH ADMIN TRUE, INHERIT TRUE GRANTED BY postgres;`, "CREATE ROLE \"a\";\nGRANT \"pg_read_all_data\" TO \"a\";\n", false},
	}
	for _, c := range cases {
		plan, err := FilterGlobals([]byte(c.sql), have)
		if c.err {
			if err == nil {
				t.Errorf("%s: want error, got %q", c.name, plan.SQL)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if string(plan.SQL) != P+c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, plan.SQL, P+c.want)
		}
	}
	if got := DBNameConninfo(`a'b\c=d`); got != `dbname='a\'b\\c=d'` {
		t.Errorf("DBNameConninfo: %s", got)
	}
}

func TestRestoreDryRun(t *testing.T) {
	f := newFakeServer()
	st := newMemStore()
	seed(t, st, "")
	res, err := Run(context.Background(), baseCfg(), Options{Database: "shop", Into: "main/shop_new", DryRun: true}, deps(f, st))
	if err != nil || !res.DryRun || res.Target.Exists || res.Disk == nil || !res.Disk.Checked {
		t.Errorf("dry-run: %v %+v", err, res)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "pg_restore") || strings.Contains(c, "create database") {
			t.Errorf("dry-run must not change anything: %s", c)
		}
	}
}

func TestRestoreIntoURLEnv(t *testing.T) {
	// The runner for a URL is an ImageRunner over the real Shell; replace Shell with a fake docker.
	f := newFakeServer()
	st := newMemStore()
	seed(t, st, "")
	d := deps(f, st)
	d.Shell = dockerShell{f}
	// No host binary → the runner is a Docker image (ADR-0009).
	d.Tools = &pg.Tools{Shell: d.Shell, GOOS: "darwin", LookPath: noBinary, Glob: noGlob}
	// The new server already has "shop" → refuse, suggest another name in the URL.
	_, err := Run(context.Background(), baseCfg(), Options{Database: "shop", IntoURLEnv: "BRACER_RESTORE_URL"}, d)
	if err == nil || !strings.Contains(err.Error(), "URL path") {
		t.Fatalf("exists on url target: %v", err)
	}
	// The target name comes from the URL path.
	d.Secrets = config.NewSecrets(map[string]string{"BRACER_RESTORE_URL": "postgres://u:p@newhost:5433/shop_new"}, "test")
	res, err := Run(context.Background(), baseCfg(), Options{Database: "shop", IntoURLEnv: "BRACER_RESTORE_URL"}, d)
	if err != nil || !res.OK || res.Target.URLEnv != "BRACER_RESTORE_URL" || res.Target.Host != "newhost:5433" || res.Target.Instance != "" || res.Target.DB != "shop_new" {
		t.Fatalf("url env: %v %+v", err, res)
	}
	if !strings.Contains(res.Target.Runner, "postgres:17-alpine") {
		t.Errorf("runner must use the target's major: %s", res.Target.Runner)
	}
	if _, err := Run(context.Background(), baseCfg(), Options{Database: "shop", IntoURLEnv: "MISSING"}, d); err == nil || !strings.Contains(err.Error(), "MISSING") {
		t.Errorf("missing env: %v", err)
	}
	if _, err := Run(context.Background(), baseCfg(), Options{Database: "shop"}, d); err == nil || !strings.Contains(err.Error(), "exactly one of") {
		t.Errorf("no target: %v", err)
	}
}

func noBinary(string) (string, error) { return "", errors.New("not found") }
func noGlob(string) ([]string, error) { return nil, nil }

// dockerShell turns `docker run … <image> <tool> args` into fakeServer.RunStderr.
type dockerShell struct{ f *fakeServer }

func (s dockerShell) Run(ctx context.Context, c pg.Cmd) (pg.Result, error) {
	if c.Name == "docker" && c.Args[0] == "version" {
		return pg.Result{}, nil
	}
	i := 0
	for i < len(c.Args) && !strings.HasPrefix(c.Args[i], "postgres:") {
		i++
	}
	if i >= len(c.Args) {
		return pg.Result{}, fmt.Errorf("fake shell: no image in %v", c.Args)
	}
	for _, a := range c.Args {
		if strings.Contains(a, "PGPASSWORD=") {
			return pg.Result{}, fmt.Errorf("secret in argv: %v", c.Args)
		}
	}
	tool, args := c.Args[i+1], c.Args[i+2:]
	db := ""
	for _, e := range c.Env {
		if strings.HasPrefix(e, "PGDATABASE=") {
			db = strings.TrimPrefix(e, "PGDATABASE=")
		}
	}
	var out bytes.Buffer
	stderr, err := s.f.RunStderr(ctx, pg.Conn{Database: db}, tool, args, c.Stdin, &out)
	if c.Stdout != nil {
		c.Stdout.Write(out.Bytes())
	}
	return pg.Result{Stdout: out.Bytes(), Stderr: stderr}, err
}

func TestRestoreFromFile(t *testing.T) {
	f := newFakeServer()
	st := newMemStore()
	seed(t, st, "")
	dir := t.TempDir()
	plain := filepath.Join(dir, "shop.dump")
	os.WriteFile(plain, []byte("PGDMP-shop"), 0o600)
	res, err := Run(context.Background(), baseCfg(), Options{File: plain, Into: "main/from_file"}, deps(f, st))
	if err != nil || !res.OK || res.Source.File != plain || res.Source.Encrypted || res.TablesFound != 1 || res.TablesExpected != 0 {
		t.Fatalf("file: %v %+v", err, res)
	}

	kp, _ := crypto.Generate()
	keyPath := filepath.Join(dir, "k")
	crypto.WriteIdentityFile(keyPath, kp)
	enc := filepath.Join(dir, "shop.dump.age")
	ef, _ := os.Create(enc)
	w, _ := crypto.Encrypt(ef, kp.Recipient)
	io.WriteString(w, "PGDMP-shop")
	w.Close()
	ef.Close()
	cfg := baseCfg()
	cfg.AgeIdentityFile = keyPath
	res, err = Run(context.Background(), cfg, Options{File: enc, Into: "main/from_age"}, deps(newFakeServer(), st))
	if err != nil || !res.OK || !res.Source.Encrypted {
		t.Errorf("age file: %v %+v", err, res)
	}
	if _, err := Run(context.Background(), baseCfg(), Options{File: plain, Into: "main"}, deps(newFakeServer(), st)); err == nil || !strings.Contains(err.Error(), "--into <instance>/<db>") {
		t.Errorf("file without db name: %v", err)
	}
}

func TestRestoreFailureLeavesDatabase(t *testing.T) {
	f := newFakeServer()
	f.failRestore = true
	st := newMemStore()
	seed(t, st, "")
	res, err := Run(context.Background(), baseCfg(), Options{Database: "shop", Into: "main/broken"}, deps(f, st))
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || !strings.Contains(res.Error, "role \"app\" does not exist") || !strings.Contains(res.Error, "--with-globals") || !strings.Contains(res.Error, "left in place") {
		t.Errorf("res.Error = %q", res.Error)
	}
	if _, ok := f.dbs["broken"]; !ok {
		t.Error("target database must be left for inspection")
	}
}

func TestRestorePoolerAndVersionWarnings(t *testing.T) {
	f := newFakeServer()
	f.pooler = true
	f.version = "16.4"
	st := newMemStore()
	seed(t, st, "")
	res, err := Run(context.Background(), baseCfg(), Options{Database: "shop", Into: "main/x", DryRun: true}, deps(f, st))
	if err != nil || len(res.Warnings) != 2 || !strings.Contains(res.Warnings[0], "pooler") || !strings.Contains(res.Warnings[1], "newer server") {
		t.Errorf("warnings: %v %v", err, res.Warnings)
	}
}

func asUsage(err error, target **UsageError) bool {
	u, ok := err.(*UsageError)
	if ok {
		*target = u
	}
	return ok
}

func asChecksum(err error, target **ErrChecksum) bool {
	c, ok := err.(*ErrChecksum)
	if ok {
		*target = c
	}
	return ok
}

// globalsSQL mimics pg_dumpall --globals-only (PG 17.6+: has \restrict).
const globalsSQL = `--
-- PostgreSQL database cluster dump
--

\restrict abc

SET default_transaction_read_only = off;

SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);

--
-- Roles
--

CREATE ROLE postgres;
ALTER ROLE postgres WITH SUPERUSER INHERIT LOGIN PASSWORD 'SCRAM-SHA-256$x';
COMMENT ON ROLE postgres IS 'admin';
CREATE ROLE app;
ALTER ROLE app WITH NOSUPERUSER INHERIT LOGIN PASSWORD 'SCRAM-SHA-256$y';
COMMENT ON ROLE app IS 'line one
line two; ALTER ROLE postgres NOLOGIN';
CREATE ROLE "Odd Role";
ALTER ROLE "Odd Role" WITH NOLOGIN SUPERUSER CREATEROLE;

--
-- Role memberships
--

GRANT pg_read_all_data TO app WITH ADMIN OPTION GRANTED BY postgres;
GRANT postgres TO "Odd Role" GRANTED BY postgres;
GRANT app TO "Odd Role" WITH INHERIT TRUE GRANTED BY postgres;
GRANT pg_maintain TO app GRANTED BY postgres;

--
-- Tablespaces
--

CREATE TABLESPACE fast OWNER postgres LOCATION '/mnt/fast';

--
-- Parameter ACLs
--

GRANT SET ON PARAMETER log_min_duration_statement TO app;

--
-- User Configurations
--

ALTER ROLE app IN DATABASE shop SET search_path TO 'app';
ALTER ROLE postgres SET log_statement TO 'all';

\unrestrict abc
`

func TestCheckDiskLocalRunnerRemoteHost(t *testing.T) {
	admin := &pg.Client{Runner: pg.LocalRunner{}, Conn: pg.Conn{Host: "db.internal", Port: 5432}}
	dc := CheckDisk(context.Background(), pg.LocalRunner{}, admin, 100)
	if dc.Checked || !strings.Contains(dc.Error, "db.internal is not this machine") || dc.NeedBytes != 500 {
		t.Errorf("remote host must not be measured locally: %+v", dc)
	}
	for _, h := range []string{"", "/var/run/postgresql", "localhost", "127.0.0.1", "::1"} {
		if !pg.IsLocalHost(h) {
			t.Errorf("%q must count as local", h)
		}
	}
	if pg.IsLocalHost("10.0.0.5") || pg.IsLocalHost("db.example.com") {
		t.Error("remote hosts must not count as local")
	}
}

// Read-only target user (the instance's backup user): usage error before creating anything.
func TestRestoreTargetUserCannotCreateDB(t *testing.T) {
	f := newFakeServer()
	f.readOnly = true
	st := newMemStore()
	seed(t, st, "")
	_, err := Run(context.Background(), baseCfg(), Options{Database: "shop", Into: "main/shop_r"}, deps(f, st))
	if !errors.As(err, new(*UsageError)) || !strings.Contains(err.Error(), "df_backup") || !strings.Contains(err.Error(), "CREATEDB") || !strings.Contains(err.Error(), "--into-url-env") {
		t.Fatalf("err = %v", err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "pg_restore") || strings.Contains(c, "create database") || strings.HasPrefix(c, "psql") && strings.Contains(c, "-f -") {
			t.Errorf("must not change anything: %s", c)
		}
	}
	// CREATEDB present but --with-globals needs CREATEROLE.
	f.readOnly = false
	f.createDBOnly = true
	_, err = Run(context.Background(), baseCfg(), Options{Database: "shop", Into: "main/shop_r", WithGlobals: true}, deps(f, st))
	if !errors.As(err, new(*UsageError)) || !strings.Contains(err.Error(), "CREATEROLE") {
		t.Fatalf("with-globals err = %v", err)
	}
	if _, err = Run(context.Background(), baseCfg(), Options{Database: "shop", Into: "main/shop_r"}, deps(f, st)); err != nil {
		t.Fatalf("CREATEDB alone must be enough without --with-globals: %v", err)
	}
}

// File is not a pg_dump archive: stop before creating the database.
func TestRestoreRejectsNonArchiveBeforeCreate(t *testing.T) {
	f := newFakeServer()
	st := newMemStore()
	seed(t, st, "")
	bad := filepath.Join(t.TempDir(), "corrupt.dump")
	os.WriteFile(bad, []byte("this is not a dump at all"), 0o600)
	_, err := Run(context.Background(), baseCfg(), Options{File: bad, Into: "main/shop_r"}, deps(f, st))
	if err == nil || !strings.Contains(err.Error(), "not a pg_dump custom-format archive") || !strings.Contains(err.Error(), "nothing was created") {
		t.Fatalf("err = %v", err)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "create database") || strings.HasPrefix(c, "pg_restore") {
			t.Errorf("must not change anything: %s", c)
		}
	}
}

// A dump made with pg_dump 18 cannot be read by pg_restore 17 in the target container → exit 2, nothing created yet.
func TestRestoreRefusesNewerArchiveOnExecTarget(t *testing.T) {
	f := newFakeServer()
	st := newMemStore()
	seed(t, st, "")
	ctx := context.Background()
	m, _ := backup.LoadManifest(ctx, st, "bracer/shop/manifest.json", "shop")
	for _, dp := range m.Instances["main"].Databases["shop"].Dumps {
		dp.ToolVersion = "18.1"
	}
	if err := backup.SaveManifest(ctx, st, "bracer/shop/manifest.json", m, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, err := Run(ctx, baseCfg(), Options{Database: "shop", Into: "main/shop_r"}, deps(f, st))
	if !errors.As(err, new(*UsageError)) || !strings.Contains(err.Error(), "pg_dump 18.1") || !strings.Contains(err.Error(), "pg_restore 17.6") || !strings.Contains(err.Error(), "--into-url-env") {
		t.Fatalf("err = %v", err)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "create database") || strings.HasPrefix(c, "pg_restore") {
			t.Errorf("must not change anything: %s", c)
		}
	}
	// Target has pg_restore 18: runs, with a warning that server 17 is older than pg_dump 18.
	f.toolVersion = "18.1"
	res, err := Run(ctx, baseCfg(), Options{Database: "shop", Into: "main/shop_r"}, deps(f, st))
	if err != nil || !res.OK {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "pg_dump 18.1") && strings.Contains(w, "PostgreSQL 17.6") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

// Extension table with a row-count mismatch: warn, do not fail; a regular table mismatch still fails.
func TestCompareExtensionTablesWarnOnly(t *testing.T) {
	ext := Comparison{Mismatches: []RowMismatch{{Table: `"_timescaledb_catalog"."metadata"`, Expected: 3, Found: 4, Extension: "timescaledb"}}}
	if !ext.OK() {
		t.Errorf("extension mismatch must not fail: %+v", ext)
	}
	plain := Comparison{Mismatches: []RowMismatch{{Table: `"public"."orders"`, Expected: 3, Found: 4}}}
	if plain.OK() {
		t.Errorf("plain mismatch must fail")
	}
	if (Comparison{Missing: []string{"x"}}).OK() {
		t.Errorf("missing table must fail")
	}
}

// Locale in the manifest → CREATE DATABASE carries the exact encoding/locale; old manifest
// → server default with a warning; provider newer than the target server → exit 2 before creating.
func TestRestoreCreatesDatabaseWithSourceLocale(t *testing.T) {
	st := newMemStore()
	seed(t, st, "")
	ctx := context.Background()
	setLocale := func(loc *pg.Locale) {
		m, err := backup.LoadManifest(ctx, st, "bracer/shop/manifest.json", "shop")
		if err != nil {
			t.Fatal(err)
		}
		for _, dp := range m.Instances["main"].Databases["shop"].Dumps {
			dp.Locale = loc
		}
		if err := backup.SaveManifest(ctx, st, "bracer/shop/manifest.json", m, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	hasWarn := func(res *Result, s string) bool {
		for _, w := range res.Warnings {
			if strings.Contains(w, s) {
				return true
			}
		}
		return false
	}

	setLocale(&pg.Locale{Encoding: "UTF8", Collate: "C.UTF-8", Ctype: "C.UTF-8"})
	f := newFakeServer()
	res, err := Run(ctx, baseCfg(), Options{Database: "shop", Into: "main/shop_r"}, deps(f, st))
	if err != nil || !res.OK {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if want := " encoding 'UTF8' lc_collate 'C.UTF-8' lc_ctype 'C.UTF-8'"; f.createOpts != want {
		t.Errorf("create options = %q, want %q", f.createOpts, want)
	}
	if hasWarn(res, "locale unknown") {
		t.Errorf("must not warn about locale when the manifest has one: %v", res.Warnings)
	}

	setLocale(nil)
	f = newFakeServer()
	res, err = Run(ctx, baseCfg(), Options{Database: "shop", Into: "main/shop_r"}, deps(f, st))
	if err != nil || !res.OK {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if f.createOpts != "" || !hasWarn(res, "source locale unknown") {
		t.Errorf("old manifest: opts %q, warnings %v", f.createOpts, res.Warnings)
	}

	setLocale(&pg.Locale{Encoding: "UTF8", Collate: "C", Ctype: "C", Provider: "builtin", Locale: "C.UTF-8"})
	f = newFakeServer()
	f.version = "16.4"
	_, err = Run(ctx, baseCfg(), Options{Database: "shop", Into: "main/shop_r"}, deps(f, st))
	if !errors.As(err, new(*UsageError)) || !strings.Contains(err.Error(), "builtin locale provider (PostgreSQL 17+); target is PostgreSQL 16") {
		t.Fatalf("err = %v", err)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "create database") || strings.HasPrefix(c, "pg_restore") {
			t.Errorf("must not change anything: %s", c)
		}
	}
}

// The source database's extensions must exist on the target: missing → exit 2 before creating
// anything; a different version → warning (spec fetch-restore § Flow 2).
func TestRestoreChecksExtensionsOnTarget(t *testing.T) {
	st := newMemStore()
	seed(t, st, "")
	ctx := context.Background()
	m, err := backup.LoadManifest(ctx, st, "bracer/shop/manifest.json", "shop")
	if err != nil {
		t.Fatal(err)
	}
	for _, dp := range m.Instances["main"].Databases["shop"].Dumps {
		dp.Extensions = []pg.Extension{{Name: "pg_partman", Version: "5.0.1"}}
	}
	if err := backup.SaveManifest(ctx, st, "bracer/shop/manifest.json", m, time.Now()); err != nil {
		t.Fatal(err)
	}
	f := newFakeServer()
	f.available = map[string]string{"plpgsql": "1.0"}
	_, err = Run(ctx, baseCfg(), Options{Database: "shop", Into: "main/shop_r"}, deps(f, st))
	if !errors.As(err, new(*UsageError)) || !strings.Contains(err.Error(), "does not have extension pg_partman 5.0.1") {
		t.Fatalf("err = %v", err)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "create database") || strings.HasPrefix(c, "pg_restore") {
			t.Errorf("must not change anything: %s", c)
		}
	}
	f = newFakeServer()
	f.available = map[string]string{"pg_partman": "5.1.0"}
	res, err := Run(ctx, baseCfg(), Options{Database: "shop", Into: "main/shop_r"}, deps(f, st))
	if err != nil || !res.OK {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "pg_partman 5.1.0") && strings.Contains(w, "5.0.1") {
			found = true
		}
	}
	if !found {
		t.Errorf("want version warning, got %v", res.Warnings)
	}
}
