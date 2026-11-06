package cli

import (
	"bytes"
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

// BR-009: a failure report carries started_at and finished_at, never ts.
// Hosted keys a body without ts by its arrival and stores started_at /
// finished_at when non-zero (apps/api/internal/monitor/report.go
// ParseReport); the web grades only rows with started_at against a
// schedule slot (apps/web/src/lib/report.ts classifyRuns, rowTime,
// SLOT_WINDOW). Without them an early failure at its slot showed as Failed
// plus Missed.

// br009Start is the fake command start, in a zone other than UTC so the
// tests see the conversion.
var br009Start = time.Date(2026, 11, 3, 15, 2, 31, 123456789, time.FixedZone("ICT", 7*3600))

// stepClock is a fake clock: its first reading is start, each later one a
// second after the previous.
type stepClock struct {
	next  time.Time
	calls int
}

func (c *stepClock) now() time.Time {
	t := c.next
	c.next = c.next.Add(time.Second)
	c.calls++
	return t
}

// br009App is earlyApp with the fake clock in its deps.
func br009App(t *testing.T, srv *reportServer, storeErr error) (*config.App, string, backupDeps, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cfg, p, d, out, errb := earlyApp(t, srv, storeErr)
	d.now = (&stepClock{next: br009Start}).now
	return cfg, p, d, out, errb
}

// failureTimes checks the two times of a failure body as hosted decodes
// them (time.Time via encoding/json) and returns them; there is no ts.
func failureTimes(t *testing.T, data []byte) (started, finished time.Time) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["ts"]; ok {
		t.Fatalf("a failure report carries ts: %s", data)
	}
	for _, k := range []string{"started_at", "finished_at"} {
		s, _ := m[k].(string)
		if _, err := time.Parse(time.RFC3339Nano, s); err != nil || !strings.HasSuffix(s, "Z") {
			t.Fatalf("%s %q is not RFC3339 UTC (%v): %s", k, s, err, data)
		}
	}
	// The fields of hosted's report struct.
	var r struct {
		TS         string    `json:"ts"`
		StartedAt  time.Time `json:"started_at"`
		FinishedAt time.Time `json:"finished_at"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	if r.TS != "" || r.StartedAt.IsZero() || r.FinishedAt.Before(r.StartedAt) || r.StartedAt.Location() != time.UTC {
		t.Fatalf("times %v .. %v: %s", r.StartedAt, r.FinishedAt, data)
	}
	return r.StartedAt, r.FinishedAt
}

// oneTimedFailure is oneFailedReport plus the times: started_at is the fake
// command start, the clock's first reading, so it was taken before loadApp.
func oneTimedFailure(t *testing.T, srv *reportServer, want string) {
	t.Helper()
	oneFailedReport(t, srv, want)
	started, finished := failureTimes(t, []byte(srv.got()[0]))
	if !started.Equal(br009Start) || !finished.After(started) {
		t.Fatalf("started %v, finished %v; want start %v", started, finished, br009Start.UTC())
	}
}

func TestBR009FailureReportBody(t *testing.T) {
	secrets := config.NewSecrets(map[string]string{"K": earlySecretKey}, "test")
	cfg := &config.App{App: "shop", Schedule: &config.Schedule{Every: "daily", At: []string{"15:00"}, Keep: 7, Since: "2026-10-01T00:00:00Z"}}
	body := failureReport("backup", "shop", br009Start, br009Start.Add(2*time.Second), errors.New("list: "+earlySecretKey+" denied"), secrets)
	data, err := reportBody(cfg, body)
	if err != nil {
		t.Fatal(err)
	}
	started, finished := failureTimes(t, data)
	if !started.Equal(br009Start) || finished.Sub(started) != 2*time.Second {
		t.Fatalf("started %v, finished %v", started, finished)
	}
	var m map[string]any
	json.Unmarshal(data, &m)
	if m["status"] != "failed" || m["ok"] != false || m["kind"] != "backup" || m["app"] != "shop" || m["error"] != "list: *** denied" {
		t.Fatalf("body: %s", data)
	}
	if _, ok := m["schedule"].(map[string]any); !ok || m["timezone"] == nil || m["utc_offset_minutes"] == nil || m["hostname"] == nil {
		t.Fatalf("no schedule/zone/hostname: %s", data)
	}
	// A clock that went back never makes finished_at earlier than started_at.
	back := failureReport("backup", "shop", br009Start, br009Start.Add(-time.Minute), errors.New("x"), nil)
	if back["finished_at"] != back["started_at"] {
		t.Fatalf("clock back: %v .. %v", back["started_at"], back["finished_at"])
	}
}

// BR-009 with BR-008: the times are protocol fields; no secrets_file value,
// however short, and no pattern changes them, and the server accepts the body.
func TestBR009TimesPassRedaction(t *testing.T) {
	started := time.Date(2026, 11, 3, 8, 2, 31, 200000000, time.UTC)
	finished := started.Add(2026 * time.Millisecond)
	secrets := config.NewSecrets(map[string]string{"YEAR": "2026", "PIN": "abcd", "T": "T08", "Z": "02:31", "K": earlySecretKey}, "test")
	body := failureReport("backup", "shop", started, finished, errors.New("bucket 2026 refused abcd "+earlySecretKey), secrets)
	in, _ := json.Marshal(body)
	data, err := reportBody(&config.App{App: "shop"}, body)
	if err != nil {
		t.Fatal(err)
	}
	if looksLikeCredentials(string(data)) {
		t.Fatalf("the server would refuse the body: %s", data)
	}
	var before, after map[string]json.RawMessage
	json.Unmarshal(in, &before)
	json.Unmarshal(data, &after)
	for _, k := range []string{"started_at", "finished_at"} {
		if !bytes.Equal(before[k], after[k]) {
			t.Errorf("%s changed: %s -> %s", k, before[k], after[k])
		}
	}
	if string(after["started_at"]) != `"2026-11-03T08:02:31.2Z"` {
		t.Errorf("started_at: %s", after["started_at"])
	}
	for _, s := range []string{"2026-11-03T08:02:31.2Z", "2026-11-03T08:02:33.226Z", "2026-11-03T08:02:31Z"} {
		if got := (*config.Secrets)(nil).Redact(s); got != s {
			t.Errorf("Redact(%q) = %q", s, got)
		}
	}
	// The error, not a protocol field, is still redacted.
	if e := string(after["error"]); strings.Contains(e, earlySecretKey) || strings.Contains(e, "abcd") {
		t.Errorf("error: %s", e)
	}
}

// BR-009: every early failure of the backup command reports the command's
// start and the time of the failure.
func TestBR009EarlyFailureTimes(t *testing.T) {
	t.Run("store", func(t *testing.T) {
		srv := newReportServer(t)
		_, _, d, _, errb := br009App(t, srv, errors.New("s3: Access Denied"))
		if code := runBackup(context.Background(), []string{"shop"}, d); code != exitError {
			t.Fatalf("exit %d\n%s", code, errb)
		}
		oneTimedFailure(t, srv, "s3: Access Denied")
	})
	t.Run("lock held", func(t *testing.T) {
		srv := newReportServer(t)
		_, p, d, _, errb := br009App(t, srv, nil)
		held, err := backup.Acquire(p)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Release()
		if code := runBackup(context.Background(), []string{"shop"}, d); code != exitError {
			t.Fatalf("exit %d\n%s", code, errb)
		}
		oneTimedFailure(t, srv, "backup lock held")
	})
	// loadApp refuses the config: hosted: is read again loosely.
	t.Run("invalid app.yaml", func(t *testing.T) {
		srv := newReportServer(t)
		_, p, d, _, errb := br009App(t, srv, nil)
		data, _ := os.ReadFile(p)
		os.WriteFile(p, append(data, []byte("surprise: 1\n")...), 0o644)
		if code := runBackup(context.Background(), []string{"shop"}, d); code != exitUsage {
			t.Fatalf("exit %d\n%s", code, errb)
		}
		oneTimedFailure(t, srv, "surprise")
	})
	t.Run("secret missing", func(t *testing.T) {
		srv := newReportServer(t)
		cfg, _, d, _, errb := br009App(t, srv, nil)
		writeEarlySecrets(t, cfg, false, true)
		if code := runBackup(context.Background(), []string{"shop"}, d); code != exitUsage {
			t.Fatalf("exit %d\n%s", code, errb)
		}
		oneTimedFailure(t, srv, "missing in "+cfg.SecretsFile)
	})
	// backup.Backup's own error: the bucket has no folder of the app.
	t.Run("backup error", func(t *testing.T) {
		srv := newReportServer(t)
		_, _, d, out, errb := br009App(t, srv, nil)
		if code := runBackup(context.Background(), []string{"shop"}, d); code != exitError || !strings.Contains(errb.String(), "the bucket has no folder") {
			t.Fatalf("exit %d\n%s", code, errb)
		}
		oneTimedFailure(t, srv, "the bucket has no folder")
		if !strings.Contains(out.String(), "✓ reported to "+srv.URL) {
			t.Errorf("no reported line:\n%s", out)
		}
	})
}

// BR-009: a dry run and init's run still report nothing.
func TestBR009DryRunAndInitDraftNoReport(t *testing.T) {
	storeErr := errors.New("s3: Access Denied")
	for _, args := range [][]string{{"shop", "--dry-run"}, {"shop", "--init-draft"}} {
		srv := newReportServer(t)
		_, p, d, _, _ := br009App(t, srv, storeErr)
		if args[1] == "--init-draft" {
			if err := os.Rename(p, draftPath(p)); err != nil {
				t.Fatal(err)
			}
		}
		if code := runBackup(context.Background(), args, d); code != exitError || len(srv.got()) != 0 {
			t.Fatalf("%v: exit %d, reports %v", args, code, srv.got())
		}
	}
}

// BR-009: a run that reaches its Result reports it as before: its own ts,
// started_at and finished_at, not the command's clock. (The fake shell has
// no Docker, so the run fails per instance; the body is still the Result.)
func TestBR009ResultReportUnchanged(t *testing.T) {
	srv := newStrictReportServer(t)
	d, out, errb := urlApp(t, srv, map[string]string{"main": "postgres://u:" + br008Password + "@10.0.0.1:abc/db"})
	d.now = (&stepClock{next: br009Start}).now
	if code := runBackup(context.Background(), []string{"shop"}, d); code != exitError {
		t.Fatalf("exit %d\n%s%s", code, out, errb)
	}
	bodies := srv.got()
	if len(bodies) != 1 {
		t.Fatalf("%d reports", len(bodies))
	}
	var r struct {
		TS         string                   `json:"ts"`
		StartedAt  time.Time                `json:"started_at"`
		FinishedAt time.Time                `json:"finished_at"`
		Instances  []*backup.InstanceResult `json:"instances"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &r); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(backupTSLayout, r.TS); err != nil || r.StartedAt.IsZero() || r.StartedAt.Equal(br009Start) || r.FinishedAt.Before(r.StartedAt) || len(r.Instances) != 1 {
		t.Fatalf("Result body changed: %s", bodies[0])
	}
}
