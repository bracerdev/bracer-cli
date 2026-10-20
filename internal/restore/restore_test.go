package restore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
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
	failRestore  string
	roles        map[string]bool
	globalsSQL   string            // SQL last received by psql -f -
	dfAvailKB    int64             // 0 = no df
	readOnly     bool              // target user has no CREATEDB/CREATEROLE
	toolVersion  string            // pg_dump --version on the target (default 17.6)
	createDBOnly bool              // target user has CREATEDB, no CREATEROLE, not superuser
	createOpts   string            // what followed TEMPLATE template0 in the latest CREATE DATABASE
	available    map[string]string // pg_available_extensions on the target
	attrSQL      map[string]string // dump content → what pg_restore --create -n … prints
	owner        map[string]string // database → owner (default: postgres)
	settings     map[string][]string
	acl          map[string]map[string]bool // database → "grantee PRIV" (nil: acldefault)
	attrFail     string                     // psql -f - reports an ERROR for statements containing it
	attrCreate   string                     // the --create invocation's args, last seen
	attrRun      string                     // attribute statements last sent to psql -f -
	schemaSQL    map[string]string          // dump content → what pg_restore --create --schema-only prints
	preload      string                     // shared_preload_libraries on the target
	noSettings   bool                       // SHOW shared_preload_libraries is denied
	roleRows     map[string]string          // user → pg_roles columns after rolname (default: LOGIN INHERIT)
	members      [][3]string                // member, group, admin_option
	roleSettings map[string][]string        // user → setconfig entries
	trusted      map[string]bool            // extension → trusted
	passwordSQL  string                     // ALTER ROLE … PASSWORD last sent to psql -f -
	userTables   int                        // tables of a user in database postgres
}

func newFakeServer() *fakeServer {
	return &fakeServer{
		dbs:       map[string][]pg.Table{"postgres": nil, "shop": {{Schema: "public", Name: "orders", Rows: i64(500)}}},
		dumps:     map[string][]pg.Table{"PGDMP-shop": {{Schema: "public", Name: "orders", Rows: i64(500)}}},
		attrSQL:   map[string]string{"PGDMP-shop": "CREATE DATABASE shop WITH TEMPLATE = template0 ENCODING = 'UTF8';\nALTER DATABASE shop OWNER TO postgres;\n\\connect shop\n"},
		owner:     map[string]string{},
		schemaSQL: map[string]string{"PGDMP-shop": shopSchemaSQL},
		settings:  map[string][]string{},
		acl:       map[string]map[string]bool{},
		version:   "17.6",
		roles:     map[string]bool{"postgres": true, "pg_read_all_data": true},
		// As in globalsSQL, so the target's postgres matches the backup.
		roleSettings: map[string][]string{"postgres": {"log_statement=all"}},
		dfAvailKB:    50 << 20, // 50 GB
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
			if strings.HasPrefix(string(data), "ALTER ROLE") && strings.Contains(string(data), " PASSWORD ") {
				f.passwordSQL = string(data)
				return nil, nil
			}
			if isAttrRun(string(data)) {
				f.attrRun = string(data)
			} else {
				f.globalsSQL = string(data)
			}
			var stderr []byte
			for _, line := range strings.Split(string(data), "\n") {
				if f.attrFail != "" && strings.Contains(line, f.attrFail) {
					stderr = append(stderr, []byte("psql:<stdin>:2: ERROR:  unrecognized configuration parameter\n")...)
					continue
				}
				f.applyAttr(strings.TrimSuffix(strings.TrimSpace(line), ";"))
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
		inNames := func() []string {
			var out []string
			for _, m := range regexp.MustCompile(`'([^']*)'`).FindAllStringSubmatch(sql[strings.LastIndex(sql, " in ("):], -1) {
				out = append(out, m[1])
			}
			return out
		}
		switch {
		case strings.HasPrefix(sql, "select datname from pg_database where datname not in"):
			for name := range f.dbs {
				if name != "postgres" && name != "template0" && name != "template1" {
					write(name + "\n")
				}
			}
		case strings.HasPrefix(sql, "select rolname from pg_roles where rolname !~"):
			for name := range f.roles {
				if !strings.HasPrefix(name, "pg_") && name != "postgres" {
					write(name + "\n")
				}
			}
		case strings.HasPrefix(sql, "select count(*) from pg_class c"):
			write(fmt.Sprintf("%d\n", f.userTables))
		case sql == "show shared_preload_libraries":
			if f.noSettings {
				return []byte("ERROR:  permission denied to examine \"shared_preload_libraries\""), &pg.ExitError{Cmd: "psql", Code: 1}
			}
			write(f.preload + "\n")
		case strings.HasPrefix(sql, "select rolname, rolsuper, rolinherit"):
			for _, n := range inNames() {
				if !f.roles[n] {
					continue
				}
				row := f.roleRows[n]
				if row == "" {
					row = "f\x1ft\x1ff\x1ff\x1ft\x1ff\x1ff\x1f-1\x1f"
					if n == "postgres" {
						row = "t\x1ft\x1ft\x1ft\x1ft\x1ft\x1ft\x1f-1\x1f"
					}
				}
				write(n + "\x1f" + row + "\n")
			}
		case strings.HasPrefix(sql, "select r.rolname, c from pg_db_role_setting"):
			for _, n := range inNames() {
				for _, c := range f.roleSettings[n] {
					write(n + "\x1f" + c + "\n")
				}
			}
		case strings.HasPrefix(sql, "select m.rolname, g.rolname, a.admin_option"):
			for _, m := range f.members {
				write(m[0] + "\x1f" + m[1] + "\x1f" + m[2] + "\n")
			}
		case strings.HasPrefix(sql, "select e.name, coalesce(v.trusted"):
			for n, tr := range f.trusted {
				v := "f"
				if tr {
					v = "t"
				}
				write(n + "\x1f" + v + "\n")
			}
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
		case strings.HasPrefix(sql, "select pg_get_userbyid(datdba) from pg_database where datname = "):
			name := strings.Trim(strings.TrimPrefix(sql, "select pg_get_userbyid(datdba) from pg_database where datname = "), "'")
			if _, ok := f.dbs[name]; ok {
				write(f.ownerOf(name) + "\n")
			}
		case strings.HasPrefix(sql, "select coalesce(r.rolname, ''), c from pg_db_role_setting"):
			name := sql[strings.LastIndex(sql, "= '")+3 : len(sql)-1]
			for _, kv := range f.settings[name] {
				write(kv + "\n")
			}
		case strings.HasPrefix(sql, "select case when a.grantee = 0"):
			name := sql[strings.LastIndex(sql, "= '")+3 : len(sql)-1]
			for k := range f.aclOf(name) {
				g, p, _ := strings.Cut(k, " ")
				write(g + "\x1f" + p + "\n")
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
		if len(args) > 1 && args[0] == "--create" && args[1] == "--schema-only" {
			data, _ := io.ReadAll(stdin)
			if _, ok := f.attrSQL[string(data)]; !ok {
				return []byte("pg_restore: error: input file does not appear to be a valid archive"), &pg.ExitError{Cmd: "pg_restore", Code: 1}
			}
			write(f.schemaSQL[string(data)])
			return nil, nil
		}
		if len(args) > 0 && args[0] == "--create" {
			f.attrCreate = strings.Join(args, " ")
			data, _ := io.ReadAll(stdin)
			out, ok := f.attrSQL[string(data)]
			if !ok {
				return []byte("pg_restore: error: input file does not appear to be a valid archive"), &pg.ExitError{Cmd: "pg_restore", Code: 1}
			}
			write(out)
			return nil, nil
		}
		if f.failRestore != "" && f.failRestore == conn.Database {
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

var (
	reAttrOwner   = regexp.MustCompile(`^ALTER DATABASE "([^"]+)" OWNER TO (\S+)$`)
	reAttrSet     = regexp.MustCompile(`^ALTER DATABASE "([^"]+)" SET (\S+) TO (.+)$`)
	reAttrRoleSet = regexp.MustCompile(`^ALTER ROLE (\S+) IN DATABASE "([^"]+)" SET (\S+) TO (.+)$`)
	reAttrACL     = regexp.MustCompile(`^(GRANT|REVOKE) (.+) ON DATABASE "([^"]+)" (TO|FROM) (\S+)$`)
)

// applyAttr plays one rebuilt attribute statement the way the server would store it.
func (f *fakeServer) applyAttr(stmt string) {
	stored := func(v string) string { // 'a', 'b' → a, b
		parts := strings.Split(v, ",")
		for i, p := range parts {
			parts[i] = strings.Trim(strings.TrimSpace(p), "'")
		}
		return strings.Join(parts, ", ")
	}
	switch {
	case reAttrOwner.MatchString(stmt):
		m := reAttrOwner.FindStringSubmatch(stmt)
		f.owner[m[1]] = m[2]
	case reAttrSet.MatchString(stmt):
		m := reAttrSet.FindStringSubmatch(stmt)
		f.settings[m[1]] = append(f.settings[m[1]], "\x1f"+strings.Trim(m[2], `"`)+"="+stored(m[3]))
	case reAttrRoleSet.MatchString(stmt):
		m := reAttrRoleSet.FindStringSubmatch(stmt)
		f.settings[m[2]] = append(f.settings[m[2]], m[1]+"\x1f"+m[3]+"="+stored(m[4]))
	case reAttrACL.MatchString(stmt):
		m := reAttrACL.FindStringSubmatch(stmt)
		acl := f.aclOf(m[3])
		for _, p := range strings.Split(m[2], ",") {
			p = strings.TrimSpace(p)
			privs := []string{p}
			if p == "ALL" {
				privs = []string{"CREATE", "CONNECT", "TEMPORARY"}
			}
			for _, pr := range privs {
				if m[1] == "GRANT" {
					acl[m[5]+" "+pr] = true
				} else {
					delete(acl, m[5]+" "+pr)
				}
			}
		}
		f.acl[m[3]] = acl
	}
}

// isAttrRun: every statement is about a database (Attributes.Apply), not globals.
func isAttrRun(sql string) bool {
	for _, l := range strings.Split(strings.TrimSpace(sql), "\n") {
		if !strings.Contains(l, " DATABASE ") {
			return false
		}
	}
	return sql != ""
}

func (f *fakeServer) ownerOf(db string) string {
	if o := f.owner[db]; o != "" {
		return o
	}
	return "postgres"
}

// aclOf: acldefault for a database until a GRANT/REVOKE changes it.
func (f *fakeServer) aclOf(db string) map[string]bool {
	if f.acl[db] != nil {
		return f.acl[db]
	}
	o := f.ownerOf(db)
	return map[string]bool{o + " CREATE": true, o + " CONNECT": true, o + " TEMPORARY": true, "PUBLIC CONNECT": true, "PUBLIC TEMPORARY": true}
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
		Shell: pg.ExecShell{}, Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Secrets: config.NewSecrets(map[string]string{"BRACER_RESTORE_URL": "postgres://u:p@newhost:5433/postgres"}, "test"),
		Now: func() time.Time { clock = clock.Add(time.Second); return clock },
		Connect: func(context.Context, *pg.Tools, config.Instance, *config.Secrets) (pg.Conn, pg.Runner, error) {
			return pg.Conn{User: "postgres", Database: "postgres"}, f, nil
		},
		ContainerUser: func(context.Context, string) (string, error) { return "postgres", nil },
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

// shopSchemaSQL is what pg_restore --create --schema-only prints for the shop dump.
const shopSchemaSQL = `--
-- Name: shop; Type: DATABASE; Schema: -; Owner: postgres
--

CREATE DATABASE shop WITH TEMPLATE = template0 ENCODING = 'UTF8';

--
-- Name: orders; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.orders (
    id integer
);
`

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
