package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
)

// clocks is n distinct times of day from 00:00, every 15 minutes, then
// every minute past the last quarter.
func clocks(n int) string {
	var out []string
	for i := 0; i < n; i++ {
		m := i * 15
		if i >= 96 {
			m = (i-96)*1 + 1
		}
		out = append(out, fmt.Sprintf("%02d:%02d", m/60, m%60))
	}
	return strings.Join(out, ", ")
}

// BR-022: the schedule question refuses more than 96 times of day (the
// hosted server grades no denser schedule) and asks again; 96 is taken.
func TestBR022InitRefusesTooManyTimesOfDay(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	code, out, errOut := runWorld(t, dir, &world{sh: containerWorld()}, answers(s3Answers, noEncryption, pickAll, drillOff,
		[]string{"", clocks(97), clocks(96), ""}, noHeartbeat)...)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "  97 times of day; at most 96 (one every 15 minutes)\n") {
		t.Errorf("no refusal at the question:\n%s", out)
	}
	cfg, err := config.Load(appYAML(dir))
	if err != nil || cfg.Schedule == nil || len(cfg.Schedule.At) != 96 {
		t.Fatalf("schedule: %v %+v", err, cfg)
	}
}

// BR-022: a drill that failed because the instance's backup did not reach
// the bucket whole does not say "backup uploaded".
func TestBR022PrintDrillSaysWhetherUploaded(t *testing.T) {
	for _, tc := range []struct {
		name string
		ir   backup.InstanceResult
		want string
	}{
		{"uploaded", backup.InstanceResult{Name: "main", OK: true, Missing: []string{}},
			"✗ main: backup uploaded; drill failed: image not found\n"},
		{"failed before upload", backup.InstanceResult{Name: "main", Error: "cannot connect", Missing: []string{"shop", "env", "globals"}},
			"✗ main: backup incomplete; drill failed: image not found\n"},
		{"one database missing", backup.InstanceResult{Name: "main", Missing: []string{"shop"}},
			"✗ main: backup incomplete; drill failed: image not found\n"},
	} {
		ir := tc.ir
		ir.DrillFailed = []string{"image not found"}
		var w bytes.Buffer
		printDrill(&w, &backup.Result{Instances: []*backup.InstanceResult{&ir}}, nil)
		if w.String() != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, w.String(), tc.want)
		}
	}
}
