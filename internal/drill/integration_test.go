//go:build integration

package drill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/crypto"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/storage"
)

// Needs Docker, the bracer-test-pg container (orders table with 500 rows) and a local S3
// (see backup/integration_test.go). Runs a real backup, then a real drill.
func TestIntegrationDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
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
	sh := pg.ExecShell{}
	if err := (pg.Docker{Shell: sh}).Version(ctx); err != nil {
		t.Skip("docker not running:", err)
	}

	kp, _ := crypto.Generate()
	keyPath := filepath.Join(t.TempDir(), "shop.key")
	if err := crypto.WriteIdentityFile(keyPath, kp); err != nil {
		t.Fatal(err)
	}
	prefix := "dtest-" + time.Now().UTC().Format("150405")
	cfg := &config.App{
		App:             "shop",
		Instances:       []config.Instance{{Name: "main", Container: "bracer-test-pg", User: "postgres", PgMajor: 17, Databases: config.Databases{List: []config.Database{{Name: "shop"}}}}},
		Storage:         &config.Storage{Endpoint: endpoint, Bucket: "backups", Prefix: prefix, AccessKeyEnv: "A", SecretKeyEnv: "S", PathStyle: true},
		Encryption:      "age",
		AgeRecipient:    kp.Recipient,
		AgeIdentityFile: keyPath,
		Drill:           &config.Drill{Timeout: "10m", Checks: []config.Check{{Name: "orders exist", SQL: "select 1 from orders limit 1"}, {Name: "never", SQL: "select 1 where false"}}},
	}

	// 1. Encrypted backup: the manifest must have orders rows=500 (counted in the snapshot).
	bres, err := backup.Run(ctx, cfg, backup.Deps{Shell: sh, Store: st, GOOS: "darwin"})
	if err != nil || !bres.OK {
		t.Fatalf("backup: %v %+v", err, bres)
	}
	manifestKey := storage.Join(prefix, "shop", "manifest.json")
	m, err := backup.LoadManifest(ctx, st, manifestKey, "shop")
	if err != nil {
		t.Fatal(err)
	}
	dump := m.Instances["main"].Databases["shop"].Dumps[0]
	var orders *pg.Table
	for i := range dump.Tables {
		if dump.Tables[i].Name == "orders" {
			orders = &dump.Tables[i]
		}
	}
	if orders == nil || orders.Rows == nil || *orders.Rows != 500 {
		t.Fatalf("manifest orders: %+v", orders)
	}

	// 2. Real drill: decrypts, restores, compares to 500, checks pass/fail correctly, container is cleaned up.
	res, err := Run(ctx, cfg, Deps{Shell: sh, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || len(res.Instances) != 1 || len(res.Instances[0].Databases) != 1 {
		t.Fatalf("result (check 'never' must fail, rest pass): %+v", res)
	}
	ir := res.Instances[0]
	dr := ir.Databases[0]
	if dr.Error != "" || len(dr.MissingTables) != 0 || len(dr.RowMismatches) != 0 || dr.TablesFound != 1 || dr.TablesExpected != 1 {
		t.Errorf("db: %+v", dr)
	}
	if ir.Globals == nil || !ir.Globals.OK || ir.Globals.IgnoredErrors < 1 {
		t.Errorf("globals (role postgres already exists must be ignored): %+v", ir.Globals)
	}
	if len(dr.Checks) != 2 || !dr.Checks[0].OK || dr.Checks[1].OK || dr.Checks[1].Error != "no rows returned" {
		t.Errorf("checks: %+v", dr.Checks)
	}
	if ir.Image != "postgres:17-alpine" {
		t.Errorf("image: %s", ir.Image)
	}
	assertNoDrillContainers(t, ctx, sh)

	// 3. Manifest says 499 → the drill must fail with both numbers.
	*orders.Rows = 499
	if err := backup.SaveManifest(ctx, st, manifestKey, m, time.Now()); err != nil {
		t.Fatal(err)
	}
	cfg.Drill.Checks = nil
	res, err = Run(ctx, cfg, Deps{Shell: sh, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	dr = res.Instances[0].Databases[0]
	if res.OK || len(dr.RowMismatches) != 1 || dr.RowMismatches[0].Expected != 499 || dr.RowMismatches[0].Found != 500 || dr.RowMismatches[0].Estimated {
		t.Errorf("tampered manifest: %+v", dr)
	}

	// 4. Wrong key → fails naming the recipient, no container left behind.
	other, _ := crypto.Generate()
	otherPath := filepath.Join(t.TempDir(), "other.key")
	crypto.WriteIdentityFile(otherPath, other)
	cfg.AgeIdentityFile = otherPath
	res, err = Run(ctx, cfg, Deps{Shell: sh, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	dr = res.Instances[0].Databases[0]
	if res.OK || !strings.Contains(dr.Error, kp.Recipient) {
		t.Errorf("wrong key: %+v", dr)
	}
	assertNoDrillContainers(t, ctx, sh)
}

func assertNoDrillContainers(t *testing.T, ctx context.Context, sh pg.Shell) {
	t.Helper()
	out, err := sh.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"ps", "-aq", "--filter", "label=" + Label + "=shop", "--filter", "name=" + containerPrefix}})
	if err != nil {
		t.Fatal(err)
	}
	if ids := strings.TrimSpace(string(out.Stdout)); ids != "" {
		t.Errorf("drill containers left behind: %s", ids)
	}
}
