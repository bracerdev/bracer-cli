package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/storage"
)

// writeSecrets writes the secrets.env of cfg with its bucket keys.
func writeSecrets(t *testing.T, cfg *config.App) {
	t.Helper()
	data := cfg.Storage.AccessKeyEnv + "=AK\n" + cfg.Storage.SecretKeyEnv + "=SK\n"
	if err := os.WriteFile(cfg.SecretsFile, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// localTS is the run ts (UTC, spec backup) of a local time: what run.json
// holds for a run that started then on this machine.
func localTS(year int, month time.Month, day, hour, min int) string {
	return time.Date(year, month, day, hour, min, 0, 0, time.Local).UTC().Format(backupTSLayout)
}

// putRun writes run as the run.json of its ts into the app's folder of st
// (ADR-0026). App, Schema and Instances are filled when left out.
func putRun(t *testing.T, st storage.Store, cfg *config.App, run backup.Run) {
	t.Helper()
	run.Schema = 1
	if run.App == "" {
		run.App = cfg.App
	}
	if run.Instances == nil {
		run.Instances = map[string]*backup.RunInstance{}
	}
	data, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(context.Background(), backup.RunKey(backup.Root(cfg), run.TS), bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
}

// installTimer puts the unit pair of app in the unit directory.
func installTimer(t *testing.T, d scheduleDeps, app string) {
	t.Helper()
	for _, ext := range []string{".service", ".timer"} {
		if err := os.WriteFile(filepath.Join(d.unitDir, unitBase(app)+ext), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// statusDeps: testDeps as a user (status needs no root) reading the bucket st.
func statusDeps(t *testing.T, sc *fakeSystemctl, st storage.Store) (scheduleDeps, *strings.Builder) {
	t.Helper()
	d, _, _ := testDeps(t, sc, "")
	d.euid = 1000
	d.newStore = func(storage.S3Config) (storage.Store, error) { return st, nil }
	var out strings.Builder
	d.stdout = &out
	return d, &out
}

// ADR-0021: bracer status shows every app of this machine, or the one named.
func TestStatusEveryApp(t *testing.T) {
	mem := storage.NewMemory(0)
	sc := &fakeSystemctl{show: map[string]map[string]string{
		"bracer-shop.timer": {"UnitFileState": "enabled", "ActiveState": "active", "NextElapseUSecRealtime": "Wed 2026-10-21 03:02:41 +07"},
	}}
	d, out := statusDeps(t, sc, mem)
	var errb strings.Builder
	d.stderr = &errb
	home := testHome(t)
	if code := runStatus(context.Background(), nil, d); code != exitUsage || !strings.Contains(errb.String(), "no app on this machine yet") {
		t.Errorf("none: %d %s", code, errb.String())
	}
	// blog: no secrets.env, so the bucket cannot be read; no timer.
	blog := writeAppYAML(t, "blog")
	shop := testApp(home, "shop")
	shop.Schedule.At = []string{"02:00"}
	shopPath := writeApp(t, shop)
	writeSecrets(t, shop)
	putRun(t, mem, shop, backup.Run{TS: localTS(2026, 10, 20, 2, 0), Backup: backup.Outcome{Result: backup.ResultOK}})
	installTimer(t, d, "shop")

	code := runStatus(context.Background(), nil, d)
	s := out.String()
	for _, want := range []string{
		"app blog (" + blog + ")\n  storage: b/bracer/blog-abc123\n  schedule: daily at 03:00, keep every backup of the last 7 days (with test-restore)\n",
		"  bucket unreachable: secrets_file: ",
		"  no timer\n  Fix: bracer remove blog, then bracer init blog\n",
		"app shop (" + shopPath + ")\n  storage: b/bracer/shop-abc123\n  schedule: daily at 02:00, keep every backup of the last 7 days (with test-restore)\n",
		"  bracer-shop.timer enabled, next run Wed 2026-10-21 03:02:41 +07\n",
		"  last run 2026-10-20 02:00: ok\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	// blog has no timer: that is a failure, and it wins.
	if code != exitError {
		t.Errorf("code %d\n%s", code, errb.String())
	}
	out.Reset()
	if code := runStatus(context.Background(), []string{"shop"}, d); code != exitOK || strings.Contains(out.String(), "app blog") {
		t.Errorf("one app: %d\n%s", code, out)
	}
	// Every instance with drill off: said in the schedule line.
	shop.Instances[0].SetDrillOff(true)
	writeApp(t, shop)
	out.Reset()
	if code := runStatus(context.Background(), []string{"shop"}, d); code != exitOK || !strings.Contains(out.String(), "  schedule: daily at 02:00, keep every backup of the last 7 days (not test-restored)\n") {
		t.Errorf("drill off: %d\n%s", code, out)
	}
	errb.Reset()
	if code := runStatus(context.Background(), []string{"nope"}, d); code != exitUsage || !strings.Contains(errb.String(), "no app nope on this machine") {
		t.Errorf("unknown app: %d %s", code, errb.String())
	}
	errb.Reset()
	if code := runStatus(context.Background(), []string{"shop", "blog"}, d); code != exitUsage || !strings.Contains(errb.String(), "one app at a time") {
		t.Errorf("two apps: %d %s", code, errb.String())
	}
}

// ADR-0023, ADR-0026: the result of the last run is the newest run.json in
// the bucket: its reasons, the drill of each instance that failed, the Fix
// and the holds; a run systemd saw after it did not report.
func TestStatusRunFromBucket(t *testing.T) {
	mem := storage.NewMemory(0)
	sc := &fakeSystemctl{show: map[string]map[string]string{
		"bracer-shop.timer":   {"UnitFileState": "enabled", "ActiveState": "active", "NextElapseUSecRealtime": "Wed 2026-10-21 03:02:41 +07"},
		"bracer-shop.service": {"ActiveState": "inactive", "Result": "success", "ExecMainStatus": "0", "ExecMainStartTimestamp": "Tue 2026-10-20 02:59:58 +07"},
	}}
	d, out := statusDeps(t, sc, mem)
	cfg := testApp(testHome(t), "shop")
	writeApp(t, cfg)
	writeSecrets(t, cfg)
	installTimer(t, d, "shop")
	putRun(t, mem, cfg, backup.Run{
		TS: localTS(2026, 10, 19, 3, 0), Backup: backup.Outcome{Result: backup.ResultOK},
	})
	putRun(t, mem, cfg, backup.Run{
		TS:     localTS(2026, 10, 20, 3, 0),
		Backup: backup.Outcome{Result: backup.ResultOK, Warnings: []string{"main/shop dropped from 2000 to 100 rows since the run 2026-10-19T030000Z; if that was not on purpose, restore from it while retention keeps it"}},
		Instances: map[string]*backup.RunInstance{
			"main":  {Drill: &backup.DrillOutcome{Result: backup.DrillFailed, Reasons: []string{"extension postgis is not in the image"}}},
			"other": {Drill: &backup.DrillOutcome{Result: backup.DrillPassed}},
		},
	})
	code := runStatus(context.Background(), []string{"shop"}, d)
	s := out.String()
	// A drill failed: the verdict is failed (ADR-0027), exit 1, with every
	// reason; the warnings of the run follow (ADR-0029).
	want := "  bracer-shop.timer enabled, next run Wed 2026-10-21 03:02:41 +07\n" +
		"  last run 2026-10-20 03:00: failed\n" +
		"    main: test-restore failed: extension postgis is not in the image\n" +
		"    warning: main/shop dropped from 2000 to 100 rows since the run 2026-10-19T030000Z; if that was not on purpose, restore from it while retention keeps it\n"
	if !strings.Contains(s, want) {
		t.Errorf("missing %q in:\n%s", want, s)
	}
	if code != exitError || strings.Contains(s, "did not report") || strings.Contains(s, "other:") {
		t.Errorf("a failed drill must exit 1, got %d:\n%s", code, s)
	}
	// Without the failed drill the same run is ok with its warning: exit 0 (ADR-0029).
	putRun(t, mem, cfg, backup.Run{
		TS:     localTS(2026, 10, 20, 3, 30),
		Backup: backup.Outcome{Result: backup.ResultOK, Warnings: []string{"main/shop dropped from 2000 to 100 rows"}},
	})
	out.Reset()
	if code = runStatus(context.Background(), []string{"shop"}, d); code != exitOK || !strings.Contains(out.String(), "  last run 2026-10-20 03:30: ok\n    warning: main/shop dropped from 2000 to 100 rows\n") || strings.Contains(out.String(), "Fix:") {
		t.Errorf("a warning must exit 0, got %d:\n%s", code, out.String())
	}

	// A later run that exited without writing its result: any exit is failed.
	for exit, want := range map[string]int{"1": exitError, "4": exitError} {
		sc.show["bracer-shop.service"] = map[string]string{"ActiveState": "failed", "Result": "exit-code", "ExecMainStatus": exit, "ExecMainStartTimestamp": "Wed 2026-10-21 03:00:05 +07"}
		out.Reset()
		code = runStatus(context.Background(), []string{"shop"}, d)
		if code != want || !strings.Contains(out.String(), "the run at 2026-10-21 03:00 did not report to the bucket (exit "+exit+"); see journalctl -t bracer") {
			t.Errorf("exit %s: code %d\n%s", exit, code, out)
		}
	}

	// A failed run prints its reasons the same way, and exits 1.
	putRun(t, mem, cfg, backup.Run{
		TS:     localTS(2026, 10, 21, 3, 0),
		Backup: backup.Outcome{Result: backup.ResultFailed, Reasons: []string{"main: docker: container shop-db is not running"}, Fix: "docker start shop-db"},
	})
	sc.show["bracer-shop.service"] = map[string]string{"ActiveState": "inactive", "Result": "exit-code", "ExecMainStatus": "1", "ExecMainStartTimestamp": "Wed 2026-10-21 02:59:58 +07"}
	out.Reset()
	code = runStatus(context.Background(), []string{"shop"}, d)
	if code != exitError || !strings.Contains(out.String(), "  last run 2026-10-21 03:00: failed\n    main: docker: container shop-db is not running\n  Fix: docker start shop-db\n") || strings.Contains(out.String(), "did not report") {
		t.Errorf("failed: %d\n%s", code, out)
	}

	// Running now; the timer disabled by hand: an error, whatever runs (ADR-0030).
	sc.show["bracer-shop.timer"] = map[string]string{"UnitFileState": "disabled", "ActiveState": "inactive", "NextElapseUSecRealtime": "n/a"}
	sc.show["bracer-shop.service"] = map[string]string{"ActiveState": "activating", "ExecMainStartTimestamp": "Wed 2026-10-22 03:00:05 +07"}
	out.Reset()
	code = runStatus(context.Background(), []string{"shop"}, d)
	s = out.String()
	if code != exitError || !strings.Contains(s, "  bracer-shop.timer disabled (disabled, inactive)\n  Fix: bracer remove shop, then bracer init shop\n  running since Wed 2026-10-22 03:00:05 +07\n") || strings.Contains(s, "last run") {
		t.Errorf("code=%d out:\n%s", code, s)
	}
}

// The bucket cannot be read: the results come from systemd, a failed run
// with its last error and Fix line from the journal.
func TestStatusFromSystemd(t *testing.T) {
	sc := &fakeSystemctl{show: map[string]map[string]string{
		"bracer-shop.timer":   {"UnitFileState": "enabled", "ActiveState": "active", "NextElapseUSecRealtime": "Sun 2026-10-19 03:02:41 UTC"},
		"bracer-shop.service": {"ActiveState": "failed", "Result": "exit-code", "ExecMainStatus": "1", "ExecMainExitTimestamp": "Sat 2026-10-18 03:01:52 UTC", "InvocationID": "abc123"},
	}}
	d, out := statusDeps(t, sc, nil)
	d.newStore = func(storage.S3Config) (storage.Store, error) { return nil, errors.New("dial tcp: no route to host") }
	var asked []string
	d.journal = func(_ context.Context, args ...string) (string, error) {
		asked = args
		return "time=… level=INFO msg=start\nbracer backup: secrets_file: open /x: permission denied\nFix: bracer remove shop, then bracer init shop\n", nil
	}
	cfg := testApp(testHome(t), "shop")
	writeApp(t, cfg)
	writeSecrets(t, cfg)
	installTimer(t, d, "shop")
	code := runStatus(context.Background(), []string{"shop"}, d)
	s := out.String()
	for _, want := range []string{
		"  bucket unreachable: dial tcp: no route to host; results below are from systemd\n",
		"  bracer-shop.timer enabled, next run Sun 2026-10-19 03:02:41 UTC\n",
		"  last run Sat 2026-10-18 03:01:52 UTC: failed (exit 1)\n",
		"    bracer backup: secrets_file: open /x: permission denied\n",
		"    Fix: bracer remove shop, then bracer init shop\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if code != exitError || len(asked) == 0 || asked[0] != "_SYSTEMD_INVOCATION_ID=abc123" {
		t.Errorf("code=%d asked=%v", code, asked)
	}
	// Any exit other than 0 is failed (ADR-0029); never ran is "never".
	sc.show["bracer-shop.service"] = map[string]string{"ActiveState": "inactive", "Result": "exit-code", "ExecMainStatus": "4", "ExecMainExitTimestamp": "Sat 2026-10-18 04:00:40 UTC", "InvocationID": "def456"}
	out.Reset()
	if code := runStatus(context.Background(), []string{"shop"}, d); code != exitError || !strings.Contains(out.String(), "  last run Sat 2026-10-18 04:00:40 UTC: failed (exit 4)\n") {
		t.Errorf("exit 4: %d\n%s", code, out)
	}
	sc.show["bracer-shop.service"] = map[string]string{"ActiveState": "inactive"}
	out.Reset()
	if code := runStatus(context.Background(), []string{"shop"}, d); code != exitOK || !strings.Contains(out.String(), "  last run: never\n") {
		t.Errorf("never: %d\n%s", code, out)
	}
	d.hasSystemd = func() bool { return false }
	out.Reset()
	if code := runStatus(context.Background(), []string{"shop"}, d); code != exitError || !strings.Contains(out.String(), "✗ this machine has no systemd: no timer runs") {
		t.Errorf("no systemd: %d\n%s", code, out)
	}
}

// A folder that is gone or unreadable is a failure with the way out (ADR-0023, ADR-0026).
func TestStatusFolderGone(t *testing.T) {
	mem := storage.NewMemory(0)
	d, out := statusDeps(t, &fakeSystemctl{show: map[string]map[string]string{}}, mem)
	cfg := testApp(testHome(t), "shop")
	writeApp(t, cfg)
	writeSecrets(t, cfg)
	installTimer(t, d, "shop")
	if code := runStatus(context.Background(), []string{"shop"}, d); code != exitError || !strings.Contains(out.String(), "  ✗ the bucket has no folder shop-abc123: its backups are gone or the bucket settings point elsewhere\n  Fix: bracer remove shop, then bracer init shop\n") {
		t.Errorf("no folder: %d\n%s", code, out)
	}
	// Objects of a run that never ended are not a run.
	root := backup.Root(cfg)
	mem.Put(context.Background(), backup.RunDir(root, "2026-10-21T030000Z")+"main/shop.dump", strings.NewReader("x"), 1)
	out.Reset()
	if code := runStatus(context.Background(), []string{"shop"}, d); code != exitError || !strings.Contains(out.String(), "the bucket has no folder shop-abc123") {
		t.Errorf("no run.json: %d\n%s", code, out)
	}
	mem.Put(context.Background(), backup.RunKey(root, "2026-10-21T030000Z"), strings.NewReader("{"), 1)
	out.Reset()
	if code := runStatus(context.Background(), []string{"shop"}, d); code != exitError || !strings.Contains(out.String(), "run.json is not valid JSON") || !strings.Contains(out.String(), "Fix: bracer remove shop, then bracer init shop") {
		t.Errorf("corrupt: %d\n%s", code, out)
	}
	os.WriteFile(config.AppPath(testHome(t), "shop"), []byte("app: shop\n"), 0o644)
	out.Reset()
	if code := runStatus(context.Background(), []string{"shop"}, d); code != exitError || !strings.Contains(out.String(), "✗ app.yaml cannot be read") {
		t.Errorf("broken app.yaml: %d\n%s", code, out)
	}
}

// An error wins over ok; a warning is not a status (ADR-0022, ADR-0029).
func TestWorseAndExitOf(t *testing.T) {
	for _, tc := range []struct{ a, b, want int }{
		{exitOK, exitOK, exitOK},
		{exitOK, exitError, exitError},
		{exitError, exitOK, exitError},
	} {
		if got := worse(tc.a, tc.b); got != tc.want {
			t.Errorf("worse(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
	for status, want := range map[string]int{backup.ResultOK: exitOK, backup.ResultFailed: exitError, "attention": exitError, "": exitError} {
		if got := exitOf(status); got != want {
			t.Errorf("exitOf(%q) = %d, want %d", status, got, want)
		}
	}
}
