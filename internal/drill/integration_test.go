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
// (see backup/integration_test.go). Runs a real backup whose last step is a real drill.
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
	stamp := time.Now().UTC().Format("150405")
	cfg := &config.App{
		App:             "shop",
		SecretsFile:     filepath.Join(t.TempDir(), "secrets.env"),
		Instances:       []config.Instance{{Name: "main", Connection: config.Connection{Container: "bracer-test-pg", User: "postgres", PgMajor: 17, SystemID: config.SystemIDNone}, Databases: config.Databases([]config.Database{{Name: "shop"}})}},
		Storage:         &config.Storage{Endpoint: endpoint, Bucket: "backups", Folder: "shop-" + stamp, AccessKeyEnv: "A", SecretKeyEnv: "S", PathStyle: true},
		Encryption:      "age",
		AgeRecipient:    kp.Recipient,
		AgeIdentityFile: keyPath,
		Schedule:        &config.Schedule{Every: config.EveryDaily, At: []string{"03:00"}, Days: []string{}, Keep: 7},
		// The run's app.yaml is read back strictly: runtime is required, unused here.
		Runtime: &config.Runtime{User: "test", Path: "/usr/bin:/bin", Docker: "/usr/bin/docker"},
	}
	here := backup.Writer{ID: "dtest"}

	// 1. Encrypted backup (init's first real run creates the folder) with the
	// drill as its last step (ADR-0026): decrypts, restores into a new
	// database, compares to the 500 rows counted in the snapshot, records
	// the outcome in run.json, and the container is cleaned up.
	var dres *Result
	bres, err := backup.Backup(ctx, cfg, backup.Deps{Shell: sh, Store: st, Here: here, Create: true,
		Drill: Step(cfg, Deps{Shell: sh, Store: st}, func(r *Result) { dres = r })})
	if err != nil || !bres.OK || bres.Status != backup.ResultOK {
		t.Fatalf("backup: %v %+v", err, bres)
	}
	if dres == nil || !dres.OK || dres.TS != bres.TS || len(dres.Instances) != 1 || len(dres.Instances[0].Databases) != 1 {
		t.Fatalf("drill result: %+v", dres)
	}
	ir := dres.Instances[0]
	dr := ir.Databases[0]
	if dr.Error != "" || len(dr.MissingTables) != 0 || len(dr.RowMismatches) != 0 || dr.TablesFound != 1 || dr.TablesExpected != 1 {
		t.Errorf("db: %+v", dr)
	}
	if ir.Result != backup.DrillPassed || !ir.OK || ir.Mode != "full" || ir.TookSeconds <= 0 {
		t.Errorf("instance: %+v", ir)
	}
	if !strings.HasPrefix(dr.RestoredAs, "shop_restored_") {
		t.Errorf("restored as: %s", dr.RestoredAs)
	}
	// ADR-0022: the source container's own image, by ID only.
	if !strings.HasPrefix(ir.Image, "sha256:") {
		t.Errorf("image: %s", ir.Image)
	}
	if bir := bres.Instance("main"); bir.DrillFailed != nil || bir.Drill != ir {
		t.Errorf("backup result: failed=%v drill=%+v", bir.DrillFailed, bir.Drill)
	}
	// run.json, written after the drill, keeps the outcome.
	run, _, err := backup.Newest(ctx, st, backup.Root(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if d := run.Instances["main"].Drill; run.TS != bres.TS || d == nil || d.Result != backup.DrillPassed || d.TookSeconds != ir.TookSeconds || !run.AllDrillsPassed() {
		t.Errorf("run.json drill: %+v", d)
	}
	dump := run.Instances["main"].Databases["shop"]
	var orders *pg.Table
	for i := range dump.Tables {
		if dump.Tables[i].Name == "orders" {
			orders = &dump.Tables[i]
		}
	}
	if orders == nil || orders.Rows == nil || *orders.Rows != 500 {
		t.Fatalf("run.json orders: %+v", orders)
	}
	assertNoDrillContainers(t, ctx, sh)

	// 2. run.json says 499 → the drill must fail with both numbers.
	*orders.Rows = 499
	res, err := Run(ctx, cfg, run, Deps{Shell: sh, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	dr = res.Instances[0].Databases[0]
	if res.OK || res.Instances[0].Result != backup.DrillFailed || len(dr.RowMismatches) != 1 || dr.RowMismatches[0].Expected != 499 || dr.RowMismatches[0].Found != 500 || dr.RowMismatches[0].Estimated {
		t.Errorf("tampered run.json: %+v", dr)
	}

	// 3. Wrong key → fails naming the recipient, no container left behind.
	other, _ := crypto.Generate()
	otherPath := filepath.Join(t.TempDir(), "other.key")
	crypto.WriteIdentityFile(otherPath, other)
	cfg.AgeIdentityFile = otherPath
	res, err = Run(ctx, cfg, run, Deps{Shell: sh, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	// The run's app.yaml is read first: the instance fails naming the recipient.
	if ir := res.Instances[0]; res.OK || !strings.Contains(ir.Error, kp.Recipient) || len(ir.Databases) != 0 {
		t.Errorf("wrong key: %+v", ir)
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
