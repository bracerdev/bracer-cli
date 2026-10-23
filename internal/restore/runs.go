package restore

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/storage"
)

// RunInfo is one run of an instance as its run.json records it: what the
// numbered list restore asks from shows (ADR-0026).
type RunInfo struct {
	TS      string   `json:"ts"`
	Missing []string `json:"missing,omitempty"`
	// Drill is passed, failed or off: the test-restore of this run.
	Drill       string  `json:"drill"`
	TookSeconds float64 `json:"took_seconds,omitempty"` // of the drill that passed
	DBs         []RunDB `json:"dbs"`
}

// RunDB is one database dump of a run: enough to tell a clean run from a
// broken one without downloading anything.
type RunDB struct {
	Name    string `json:"name"`
	Rows    int64  `json:"rows"`
	RowsEst bool   `json:"rows_est,omitempty"` // some tables only have an estimate
	Bytes   int64  `json:"bytes"`
}

// runsZone is the time zone the run list shows next to UTC; tests set it.
var runsZone = time.Local

// LoadRuns reads run.json of every run folder under root, newest first. A
// folder without run.json is a run that did not end; one whose run.json
// does not read is left out too: both have nothing restore can trust.
func LoadRuns(ctx context.Context, st storage.Store, root string) ([]*backup.Run, error) {
	refs, err := backup.ListRuns(ctx, st, root)
	if err != nil {
		return nil, err
	}
	var out []*backup.Run
	for _, ref := range refs {
		if !ref.Done {
			continue
		}
		r, err := backup.LoadRun(ctx, st, root, ref.TS)
		if err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// Runs lists the runs restore can load for an instance, newest first: those
// whose run.json has the instance with an environment file and the run's
// app.yaml (ADR-0022).
func Runs(runs []*backup.Run, instance string) []RunInfo {
	var out []RunInfo
	for _, r := range runs {
		ri := r.Instances[instance]
		if ri == nil || ri.Env == nil || r.AppYAML == nil {
			continue
		}
		info := RunInfo{TS: r.TS, Missing: ri.Missing, Drill: backup.DrillOff}
		if ri.Drill != nil {
			info.Drill = ri.Drill.Result
			if ri.Drill.Result == backup.DrillPassed {
				info.TookSeconds = ri.Drill.TookSeconds
			}
		}
		names := make([]string, 0, len(ri.Databases))
		for name := range ri.Databases {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			d := ri.Databases[name]
			rows, est := backup.TotalRows(d)
			info.DBs = append(info.DBs, RunDB{Name: name, Rows: rows, RowsEst: est, Bytes: d.Bytes})
		}
		out = append(out, info)
	}
	return out
}

// RunLines describes one run for the numbered list restore asks from
// (ADR-0016, ADR-0026): ts with local time, completeness, its drill, then each
// database's rows and size.
func RunLines(r RunInfo) []string {
	state := "complete"
	if len(r.Missing) > 0 {
		state = "missing: " + strings.Join(r.Missing, ", ")
	}
	switch r.Drill {
	case backup.DrillPassed:
		state += ", drill passed"
	case backup.DrillFailed:
		state += ", drill failed"
	default:
		state += ", not test-restored"
	}
	out := []string{fmt.Sprintf("%s%s  %s", r.TS, localTime(r.TS), state)}
	var dbs []string
	for _, db := range r.DBs {
		rows := fmt.Sprintf("%d rows", db.Rows)
		if db.RowsEst {
			rows = "~" + rows
		}
		dbs = append(dbs, fmt.Sprintf("%s %s, %s", db.Name, rows, humanBytes(db.Bytes)))
	}
	if len(dbs) > 0 {
		out = append(out, "    "+strings.Join(dbs, "; "))
	}
	return out
}

// localTime is " (15:28 +07)": the run's ts in runsZone, with its date when
// that differs from the UTC date.
func localTime(ts string) string {
	t, err := time.Parse(tsLayout, ts)
	if err != nil {
		return ""
	}
	l := t.In(runsZone)
	if l.Format("2006-01-02") != t.Format("2006-01-02") {
		return " (" + l.Format("2006-01-02 15:04 -07") + ")"
	}
	return " (" + l.Format("15:04 -07") + ")"
}

// ReadRunConfig reads the app.yaml of the run (ADR-0016); nil without error
// when the run has none.
func ReadRunConfig(ctx context.Context, st storage.Store, r *backup.Run, ids *Identities) (*config.App, error) {
	if r.AppYAML == nil {
		return nil, nil
	}
	data, err := readObject(ctx, st, r.AppYAML, ids, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("app.yaml of the run %s: %w", r.TS, err)
	}
	cfg, err := config.ParseRun(data)
	if err != nil {
		return nil, fmt.Errorf("app.yaml of the run %s: %w", r.TS, err)
	}
	return cfg, nil
}

// Expected is what the run of instance meant to back up, from the run's
// app.yaml; known is false without one, or for an instance it does not name.
func Expected(runCfg *config.App, instance string) (dbs []string, known bool) {
	if runCfg == nil {
		return nil, false
	}
	for _, in := range runCfg.Instances {
		if in.Name == instance {
			return in.Databases.Names(), true
		}
	}
	return nil, false
}
