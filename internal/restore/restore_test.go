package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sort"
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
func (m *memStore) List(_ context.Context, prefix string) ([]storage.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []storage.Object
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, storage.Object{Key: k})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}
func (m *memStore) Delete(context.Context, string) error { return nil }

func i64(n int64) *int64 { return &n }

func sha(b []byte) string {
	return fmt.Sprintf("%x", sha256sum(b))
}

// shopRoot is the seeded app folder: <prefix>/<name>/.
const shopRoot = "bracer/shop/"

// oldTS is the seeded run before seedTS: a dump only, no environment file,
// globals or app.yaml, so restore lists it nowhere.
const oldTS = "2026-10-15T030000Z"

// putObject stores content at key, through age when recipient is set, and
// returns the SHA-256 and size of the object as stored.
func putObject(t *testing.T, st *memStore, key, content, recipient string) (string, int64) {
	t.Helper()
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
	if err := st.Put(context.Background(), key, bytes.NewReader(buf.Bytes()), int64(buf.Len())); err != nil {
		t.Fatal(err)
	}
	return sha(buf.Bytes()), int64(buf.Len())
}

// writeRun stores r as <shopRoot><ts>/run.json, as backup ends a run.
func writeRun(t *testing.T, st *memStore, r *backup.Run) {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(context.Background(), backup.RunKey(shopRoot, r.TS), bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
}

// loadRun reads the seeded run ts.
func loadRun(t *testing.T, st *memStore, ts string) *backup.Run {
	t.Helper()
	r, err := backup.LoadRun(context.Background(), st, shopRoot, ts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// editRun changes the seeded run.json of ts in place.
func editRun(t *testing.T, st *memStore, ts string, edit func(*backup.Run)) {
	t.Helper()
	r := loadRun(t, st, ts)
	edit(r)
	if err := backup.SaveRun(context.Background(), st, r); err != nil {
		t.Fatal(err)
	}
}

// seed writes two runs of the app shop under shopRoot (ADR-0026): seedTS
// with the dump of shop and the globals of main, oldTS with a dump only.
// With a recipient every object but run.json is encrypted to it.
func seed(t *testing.T, st *memStore, recipient string) {
	t.Helper()
	enc := recipient != ""
	ext := ""
	if enc {
		ext = ".age"
	}
	var rp *string
	if enc {
		rp = &recipient
	}
	s1, n1 := putObject(t, st, backup.RunDir(shopRoot, seedTS)+"main/shop.dump"+ext, "PGDMP-shop", recipient)
	s0, n0 := putObject(t, st, backup.RunDir(shopRoot, oldTS)+"main/shop.dump"+ext, "PGDMP-old", recipient)
	sg, ng := putObject(t, st, backup.RunDir(shopRoot, seedTS)+"main/globals.sql"+ext, superGlobals, recipient)
	instance := func() *backup.RunInstance {
		return &backup.RunInstance{Kind: "container", PgVersion: "17.6", ToolVersion: "pg_dump (PostgreSQL) 17.6", Encrypted: enc, Recipient: rp, Missing: []string{}, Databases: map[string]*backup.Dump{}}
	}
	old := instance()
	old.Databases["shop"] = &backup.Dump{Bytes: n0, SHA256: s0}
	writeRun(t, st, &backup.Run{App: "shop", TS: oldTS, Instances: map[string]*backup.RunInstance{"main": old}})
	main := instance()
	main.Globals = &backup.Dump{Bytes: ng, SHA256: sg}
	main.Databases["shop"] = &backup.Dump{Bytes: n1, SHA256: s1,
		Locale: &pg.Locale{Encoding: "UTF8", Collate: "en_US.utf8", Ctype: "en_US.utf8"},
		Tables: []pg.Table{{Schema: "public", Name: "orders", Rows: i64(500)}}}
	writeRun(t, st, &backup.Run{App: "shop", TS: seedTS, Instances: map[string]*backup.RunInstance{"main": main}})
}

// seedRun gives the seeded run ts (plaintext) an environment file, its own
// app.yaml listing dbs for main (ADR-0016), and what backup recorded as
// missing.
func seedRun(t *testing.T, st *memStore, ts string, missing []string, dbs ...string) {
	t.Helper()
	cfg := baseCfg()
	cfg.Instances[0].Databases = nil
	for _, db := range dbs {
		cfg.Instances[0].Databases = append(cfg.Instances[0].Databases, config.Database{Name: db})
	}
	data, err := config.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sa, na := putObject(t, st, backup.RunDir(shopRoot, ts)+"app.yaml", string(data), "")
	se, ne := putObject(t, st, backup.RunDir(shopRoot, ts)+"main/env.json", "{}", "")
	editRun(t, st, ts, func(r *backup.Run) {
		r.AppYAML = &backup.Dump{Bytes: na, SHA256: sa}
		r.Instances["main"].Env = &backup.Dump{Bytes: ne, SHA256: se}
		r.Instances["main"].Missing = missing
	})
}

// baseCfg is a valid app.yaml, as init writes it.
func baseCfg() *config.App {
	return &config.App{
		App:         "shop",
		SecretsFile: "/home/u/.config/bracer/shop/secrets.env",
		Instances: []config.Instance{{Name: "main", Connection: config.Connection{Container: "shop-db", User: "postgres", PgMajor: 17, SystemID: "1"},
			Databases: config.Databases([]config.Database{{Name: "shop"}})}},
		Storage:    &config.Storage{Endpoint: "https://s3.example.com", Bucket: "b", Folder: "shop-7f3k2q", AccessKeyEnv: "A", SecretKeyEnv: "S"},
		Encryption: "none",
		Schedule:   &config.Schedule{Every: config.EveryDaily, At: []string{"03:00"}, Days: []string{}, Keep: 7},
		Runtime:    &config.Runtime{User: "u", Path: "/usr/bin:/bin", Docker: "/usr/bin/docker"},
	}
}

func deps(st storage.Store) Deps {
	clock := time.Date(2026, 10, 17, 3, 0, 0, 0, time.UTC)
	return Deps{
		Shell: pg.ExecShell{}, Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { clock = clock.Add(time.Second); return clock },
	}
}

// shopFolder is the seeded app folder, as ListFolders finds it: with the
// newest run.json.
func shopFolder(t *testing.T, st *memStore) Folder {
	t.Helper()
	r, _, err := backup.Newest(context.Background(), st, shopRoot)
	if err != nil {
		t.Fatal(err)
	}
	return Folder{Name: "shop", Root: shopRoot, Newest: r}
}

// ADR-0023, ADR-0026: each app is a folder <prefix>/<name>/ whose newest
// <ts>/run.json is its state; a run.json that cannot be read is listed with
// its error. A folder with no run.json is no app; another prefix is not seen.
func TestListFolders(t *testing.T) {
	ctx := context.Background()
	st := newMemStore()
	seed(t, st, "")
	st.Put(ctx, "bracer/broken/2026-10-16T030000Z/run.json", strings.NewReader("{"), 1)
	st.Put(ctx, "bracer/half/2026-10-16T030000Z/main/shop.dump", strings.NewReader("PGDMP"), 5)
	st.Put(ctx, "bracer/probe", strings.NewReader(""), 0)
	st.Put(ctx, "other/x/2026-10-16T030000Z/run.json", strings.NewReader("{}"), 2)
	got, err := ListFolders(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "broken" || got[0].Root != "bracer/broken/" || got[0].Err == nil || got[0].Newest != nil {
		t.Errorf("folders: %+v", got)
	}
	if len(got) == 2 && (got[1].Name != "shop" || got[1].Root != shopRoot || got[1].Err != nil || got[1].Newest == nil || got[1].Newest.App != "shop" || got[1].Newest.TS != seedTS) {
		t.Errorf("shop: %+v", got[1])
	}
}

// Dumps are read through age with the customer's key, and the SHA-256 of
// the object is checked while it streams.
func TestOpenDumpDecryptsAndChecksums(t *testing.T) {
	ctx := context.Background()
	kp, _ := crypto.Generate()
	keyPath := filepath.Join(t.TempDir(), "k")
	crypto.WriteIdentityFile(keyPath, kp)
	ids := ReadIdentities(keyPath, crypto.ReadIdentities)

	st := newMemStore()
	seed(t, st, kp.Recipient)
	dp := shopFolder(t, st).Newest.Instances["main"].Databases["shop"]
	if dp.Key != backup.RunDir(shopRoot, seedTS)+"main/shop.dump.age" || !dp.Encrypted || dp.Recipient == nil || *dp.Recipient != kp.Recipient {
		t.Fatalf("dump as loaded: %+v", dp)
	}

	if data, err := readObject(ctx, st, dp, ids, 1<<20); err != nil || string(data) != "PGDMP-shop" {
		t.Fatalf("decrypted: %v %q", err, data)
	}
	if _, err := OpenDump(ctx, st, dp, &Identities{Err: errors.New("no key file")}); err == nil || !strings.Contains(err.Error(), kp.Recipient) {
		t.Errorf("missing key: %v", err)
	}
	dp.SHA256 = "deadbeef"
	var ce *ErrChecksum
	if _, err := readObject(ctx, st, dp, ids, 1<<20); !errors.As(err, &ce) || ce.Want != "deadbeef" {
		t.Errorf("checksum: %v", err)
	}
}

// The key restore is given must open the folder's latest recipient.
func TestUseKey(t *testing.T) {
	kp, _ := crypto.Generate()
	keyPath := filepath.Join(t.TempDir(), "k")
	crypto.WriteIdentityFile(keyPath, kp)
	st := newMemStore()
	seed(t, st, kp.Recipient)
	s := NewSession(deps(st), shopFolder(t, st))
	if err := s.UseKey(keyPath, "age1other"); err == nil || !strings.Contains(err.Error(), "has no key for age1other") || !strings.Contains(err.Error(), kp.Recipient) {
		t.Errorf("other recipient: %v", err)
	}
	if err := s.UseKey(filepath.Join(t.TempDir(), "missing"), kp.Recipient); err == nil {
		t.Error("a missing key file must fail")
	}
	if err := s.UseKey(keyPath, kp.Recipient); err != nil || !s.IDs.Has(kp.Recipient) {
		t.Errorf("matching key: %v", err)
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
