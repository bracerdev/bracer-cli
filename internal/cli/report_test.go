package cli

import (
	"encoding/json"
	"errors"
	"testing"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
)

// Spec backup § Flow 8: the /report body is the run's JSON plus the schedule
// (hosted computes the next run from it) and the hostname.
func TestReportBody(t *testing.T) {
	cfg := &config.App{App: "shop", Schedule: &config.Schedule{Every: "weekly", At: []string{"03:00"}, Days: []string{"sun"}, Keep: 4}}
	res := &backup.Result{Kind: "backup", App: "shop", TS: "2026-10-23T030000Z", Status: backup.ResultOK, OK: true, Warnings: []string{"x"}}
	for _, body := range []any{res, failureReport("backup", "shop", errors.New("lock"))} {
		data, err := reportBody(cfg, body)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		sc, _ := m["schedule"].(map[string]any)
		if sc["every"] != "weekly" || sc["keep"] != float64(4) || m["hostname"] == "" || m["kind"] != "backup" || m["status"] == nil {
			t.Fatalf("body = %s", data)
		}
	}
}
