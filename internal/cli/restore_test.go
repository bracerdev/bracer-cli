package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"bracer.dev/bracer/internal/restore"
)

func TestRestoreRemovedFlags(t *testing.T) {
	for args, want := range map[string]string{
		"--into main":       "--into was removed; use --into-container <name>, --into-local <port> or --into-url",
		"--skip-disk-check": "--skip-disk-check was removed",
	} {
		var out, errOut bytes.Buffer
		code := runRestore(context.Background(), strings.Fields(args), backupDeps{stdout: &out, stderr: &errOut})
		if code != exitUsage || !strings.Contains(errOut.String(), want) {
			t.Errorf("%s: code %d, stderr %q", args, code, errOut.String())
		}
	}
}

func TestWithDatabases(t *testing.T) {
	args := []string{"-c", "app.yaml", "--database", "shop", "--database=blog", "--into-container", "pg"}
	got := shellJoin(withDatabases(args, []string{"shop", "my db"}))
	if got != "-c app.yaml --into-container pg --database shop --database 'my db'" {
		t.Errorf("got %s", got)
	}
}

func TestPrintRestoreSwap(t *testing.T) {
	restoreNow = func() time.Time { return time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC) }
	defer func() { restoreNow = time.Now }()
	res := &restore.Result{Mode: restore.ModeFull, OK: false, Source: restore.Source{Instance: "main"},
		Databases: []*restore.DBResult{
			{SourceDB: "shop", TargetDB: "shop_restored_20261016_0300", State: restore.StateHasData, OK: true},
			{SourceDB: "blog", TargetDB: "blog", State: restore.StateEmpty, Error: "pg_restore: boom; the transaction rolled back, blog is empty as before"},
		}}
	var w bytes.Buffer
	printRestoreResult(&w, res, []string{"--into-container", "pg"})
	s := w.String()
	for _, want := range []string{
		`ALTER DATABASE "shop" RENAME TO "shop_old_20261019";`,
		`ALTER DATABASE "shop_restored_20261016_0300" RENAME TO "shop";`,
		"the transaction rolled back, blog is empty as before",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}

// ADR-0015: not ready → the failed checks, the SQL files and how to apply them.
func TestPrintNotReady(t *testing.T) {
	res := &restore.Result{Mode: restore.ModeFull, Target: restore.Target{Container: "pg", User: "postgres"},
		Checks: []restore.Check{
			{Area: "role", Name: "app", Detail: "missing", Fix: restore.FixSQL},
			{Area: "extension", Name: "shop: vector 0.8.1", Detail: "the target server does not have it", Fix: restore.FixManual, Manual: "use an image with vector"},
			{Area: "version", Name: "PostgreSQL 17", OK: true},
		},
		SQLDir: "/tmp/r", EnvUp: "/tmp/r/01-env-up.sql", EnvDown: "/tmp/r/01-env-down.sql",
		Apply: "docker exec -i pg psql -U postgres -d postgres -v ON_ERROR_STOP=1 < <file>", Restart: "docker restart pg"}
	var w bytes.Buffer
	printNotReady(&w, res, "bracer restore --into-container pg")
	s := w.String()
	for _, want := range []string{"✗ role app: missing", "fix: in env-up", "fix: use an image with vector",
		"apply with: docker exec -i pg psql -U postgres -d postgres -v ON_ERROR_STOP=1 < /tmp/r/01-env-up.sql",
		"then restart: docker restart pg", "env-up may hold password hashes", "then run again: bracer restore --into-container pg"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "version") {
		t.Errorf("passed checks are not listed here:\n%s", s)
	}
}
