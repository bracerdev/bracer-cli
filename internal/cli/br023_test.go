package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
)

// sinceRun runs init on init's own clock: the clock reads t0 until the
// first preflight, which moves it to afterPreflight (time spent at the
// drill question it fails); the real run reads the draft, then moves the
// clock to afterRun. It returns the draft's since during the real run, and
// app.yaml's since at install and at the end.
func sinceRun(t *testing.T, t0, afterPreflight, afterRun time.Time) (draft, install, final string, out string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	clock := t0
	w := &world{sh: containerWorld(), now: func() time.Time { return clock }}
	// The first preflight fails the drill: init asks, answered n.
	w.preflight = func(n int) *preflightResult {
		pf := &preflightResult{Kind: "backup-dry-run", App: "shop", OK: true}
		if n == 1 {
			clock = afterPreflight
			pf.OK = false
			pf.Lines = []preflightLine{{Section: "drill", Instance: "shop-db", Area: "drill", Mark: "✗", Text: "shop-db (image postgres:17-alpine): extension postgis is not in the image"}}
		}
		return pf
	}
	w.selfRun = func(args []string) int {
		if hasArg(args, "--dry-run") {
			return 0
		}
		raw, err := os.ReadFile(draftPath(appYAML(dir)))
		if err != nil {
			t.Errorf("no draft during the real run: %v", err)
			return 1
		}
		cfg, err := config.Parse(raw)
		if err != nil {
			t.Errorf("draft: %v", err)
			return 1
		}
		draft = cfg.Schedule.Since
		clock = afterRun
		return 0
	}
	w.onInstall = func() {
		cfg, err := config.Load(appYAML(dir))
		if err != nil {
			t.Errorf("app.yaml at install: %v", err)
			return
		}
		install = cfg.Schedule.Since
	}
	code, out, errOut := runWorld(t, dir, w, answers(s3Answers, noEncryption, pickAll, drillOn, schedDefault, []string{"n"})...)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	cfg, err := config.Load(appYAML(dir))
	if err != nil {
		t.Fatal(err)
	}
	return draft, install, cfg.Schedule.Since, out
}

// BR-023: init writes schedule.since without asking: the draft carries the
// moment of the real run (stamped again after the drill question, not when
// the draft was first written), app.yaml the moment of the rename, after
// the real run and just before the timer.
func TestBR023InitWritesSince(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC)
	draft, install, final, out := sinceRun(t, t0, t0.Add(20*time.Minute+500*time.Millisecond), t0.Add(31*time.Minute+900*time.Millisecond))
	if strings.Contains(out, "since") {
		t.Errorf("since is never asked nor printed:\n%s", out)
	}
	if draft != "2026-11-05T03:20:00Z" {
		t.Errorf("the real run reports since %q, want the time it ran, after the drill question", draft)
	}
	if final != "2026-11-05T03:31:00Z" || install != final {
		t.Errorf("app.yaml since %q (at install %q), want the rename time 2026-11-05T03:31:00Z", final, install)
	}
}

// BR-023 review: app.yaml's since is always at least a second after the
// draft's, so the real run's period never merges with the timer's: within
// the same second, and when the clock went back in between.
func TestBR023SecondStampStrictlyLater(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC)
	for _, afterRun := range []time.Time{t0.Add(400 * time.Millisecond), t0.Add(-time.Hour)} {
		draft, _, final, _ := sinceRun(t, t0, t0, afterRun)
		if draft != "2026-11-05T03:00:00Z" || final != "2026-11-05T03:00:01Z" {
			t.Errorf("clock %s after the run: draft %q, app.yaml %q; want 03:00:00 then 03:00:01", afterRun, draft, final)
		}
	}
	if got := laterSince("2026-11-05T03:00:00Z", t0.Add(90*time.Second+700*time.Millisecond)); got != "2026-11-05T03:01:30Z" {
		t.Errorf("a later clock is used as is, truncated: %q", got)
	}
}

// BR-023: since travels inside the report's schedule, for a Result and for
// a failure report, and for an early failure that reads app.yaml loosely.
func TestBR023ReportCarriesSince(t *testing.T) {
	const since = "2026-11-05T03:04:05Z"
	cfg := &config.App{App: "shop", Schedule: &config.Schedule{Every: "daily", At: []string{"03:00"}, Days: []string{}, Keep: 7, Since: since}}
	res := &backup.Result{Kind: "backup", App: "shop", TS: "2026-11-05T030000Z", Status: backup.ResultOK, OK: true}
	for _, body := range []any{res, failureReport("backup", "shop", time.Now(), time.Now(), errors.New("lock"), nil)} {
		data, err := reportBody(cfg, body)
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Schedule map[string]any `json:"schedule"`
		}
		if err := json.Unmarshal(data, &m); err != nil || m.Schedule["since"] != since {
			t.Fatalf("body %s", data)
		}
	}

	srv := newReportServer(t)
	_, p, d, _, _ := earlyApp(t, srv, nil)
	loaded, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	os.WriteFile(p, append(data, []byte("surprise: 1\n")...), 0o644)
	if code := runBackup(context.Background(), []string{"shop"}, d); code != exitUsage {
		t.Fatalf("exit %d", code)
	}
	got := srv.got()
	if len(got) != 1 {
		t.Fatalf("reports: %q", got)
	}
	var m struct {
		Schedule map[string]any `json:"schedule"`
	}
	if err := json.Unmarshal([]byte(got[0]), &m); err != nil || m.Schedule["since"] != loaded.Schedule.Since || loaded.Schedule.Since == "" {
		t.Fatalf("early failure report %s, app.yaml since %q", got[0], loaded.Schedule.Since)
	}
}

// noSince is the schedule as the questions set it: init adds since.
func noSince(s *config.Schedule) config.Schedule {
	c := *s
	c.Since = ""
	return c
}
