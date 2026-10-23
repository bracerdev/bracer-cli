//go:build integration

package backup

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/crypto"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/storage"
)

// Needs Docker, the bracer-test-pg Postgres container (see pg/integration_test.go)
// and a local S3: BRACER_TEST_S3_ENDPOINT (default http://localhost:18333,
// bucket backups, keys bracerkey/bracersecret — SeaweedFS `server -s3`).
func TestIntegrationBackupPlainAndEncrypted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	endpoint := os.Getenv("BRACER_TEST_S3_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:18333"
	}
	st, err := storage.NewS3(storage.S3Config{Endpoint: endpoint, Bucket: "backups", Region: "us-east-1", AccessKey: "bracerkey", SecretKey: "bracersecret", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Check(ctx); err != nil {
		t.Skip("no local S3:", err)
	}
	if err := (pg.Docker{Shell: pg.ExecShell{}}).Version(ctx); err != nil {
		t.Skip("docker not running:", err)
	}
	// One folder per run of the test, so runs never see each other (ADR-0027: bracer/ is fixed).
	stamp := time.Now().UTC().Format("150405")
	cfg := &config.App{
		App:        "shop",
		Instances:  []config.Instance{{Name: "main", Container: "bracer-test-pg", User: "postgres", PgMajor: 17, SystemID: config.SystemIDNone, Databases: config.Databases([]config.Database{{Name: "shop"}})}},
		Storage:    &config.Storage{Endpoint: endpoint, Bucket: "backups", Folder: "shop-" + stamp, AccessKeyEnv: "A", SecretKeyEnv: "S", PathStyle: true},
		Encryption: "none",
		Schedule:   &config.Schedule{Every: config.EveryDaily, At: []string{"03:00"}, Days: []string{}, Keep: 7},
	}
	deps := Deps{Shell: pg.ExecShell{}, Store: st, Here: Writer{ID: "itest"}, Create: true}

	// 1. Unencrypted: the downloaded dump must be an archive pg_restore can read, run.json has tables.
	res, err := Backup(ctx, cfg, deps)
	if err != nil || !res.OK {
		t.Fatalf("plain run: %v %+v", err, res)
	}
	run, _, err := Newest(ctx, st, Root(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if run.TS != res.TS {
		t.Fatalf("newest run %s, this run %s", run.TS, res.TS)
	}
	ri := run.Instances["main"]
	d := ri.Databases["shop"]
	var hasOrders bool
	for _, tb := range d.Tables {
		if tb.Name == "orders" && tb.Rows != nil && *tb.Rows == 500 && tb.RowsEst == nil {
			hasOrders = true
		}
	}
	if !hasOrders {
		t.Errorf("run.json tables must include orders with exact rows=500: %+v", d.Tables)
	}
	plain := fetch(t, ctx, st, d.Key)
	if int64(len(plain)) != d.Bytes {
		t.Errorf("bytes: stored %d, run.json %d", len(plain), d.Bytes)
	}
	assertRestorable(t, ctx, plain)
	if ri.Globals == nil || !ri.Globals.HasPasswords {
		t.Fatalf("globals: %+v", ri.Globals)
	}
	globals := fetch(t, ctx, st, ri.Globals.Key)
	if !bytes.Contains(globals, []byte("CREATE ROLE")) || !bytes.Contains(globals, []byte("SCRAM-SHA-256")) {
		t.Error("globals must contain roles with password hashes (superuser)")
	}

	// 2. age encryption: the object is unreadable directly; decrypted with the identity, pg_restore can read it.
	kp, _ := crypto.Generate()
	keyPath := filepath.Join(t.TempDir(), "shop.key")
	crypto.WriteIdentityFile(keyPath, kp)
	cfg.Encryption, cfg.AgeRecipient, cfg.AgeIdentityFile = "age", kp.Recipient, keyPath
	cfg.Storage.Folder = "shop-a" + stamp[1:]
	res, err = Backup(ctx, cfg, deps)
	if err != nil || !res.OK || len(res.Warnings) != 0 {
		t.Fatalf("age run: %v %+v", err, res)
	}
	run, _, err = Newest(ctx, st, Root(cfg))
	if err != nil {
		t.Fatal(err)
	}
	d = run.Instances["main"].Databases["shop"]
	if !strings.HasSuffix(d.Key, ".dump.age") || !d.Encrypted || *d.Recipient != kp.Recipient {
		t.Fatalf("age dump entry: %+v", d)
	}
	enc := fetch(t, ctx, st, d.Key)
	if bytes.HasPrefix(enc, []byte("PGDMP")) {
		t.Fatal("object is not encrypted")
	}
	ids, _, _ := crypto.ReadIdentities(keyPath)
	rd, err := crypto.Decrypt(bytes.NewReader(enc), ids)
	if err != nil {
		t.Fatal(err)
	}
	dec, _ := io.ReadAll(rd)
	assertRestorable(t, ctx, dec)
}

func fetch(t *testing.T, ctx context.Context, st storage.Store, key string) []byte {
	t.Helper()
	rc, err := st.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// assertRestorable runs pg_restore -l in the postgres:17-alpine image on the given bytes.
func assertRestorable(t *testing.T, ctx context.Context, dump []byte) {
	t.Helper()
	if !bytes.HasPrefix(dump, []byte("PGDMP")) {
		t.Fatalf("not a custom-format archive: %q", dump[:min(8, len(dump))])
	}
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-i", "postgres:17-alpine", "sh", "-c", "cat > /tmp/d && pg_restore -l /tmp/d")
	cmd.Stdin = bytes.NewReader(dump)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pg_restore -l: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("TABLE public orders")) {
		t.Errorf("pg_restore -l output lacks orders table:\n%s", out)
	}
}
