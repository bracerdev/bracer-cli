// Package drill restores the run a backup just wrote into a temporary
// Postgres (a Docker container) the way restore would, and compares it
// against run.json: the last step of every backup (ADR-0026).
package drill

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"filippo.io/age"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/crypto"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/restore"
	"bracer.dev/bracer/internal/storage"
)

// Deps is everything Run touches outside; tests replace it with fakes.
type Deps struct {
	Shell pg.Shell
	// Tools checks Docker; defaults to pg.NewTools(Shell).
	Tools *pg.Tools
	Store storage.Store
	Now   func() time.Time
	Log   *slog.Logger
	// ReadIdentities defaults to crypto.ReadIdentities.
	ReadIdentities func(path string) ([]age.Identity, []string, error)
	// Rand returns n random bytes as hex; defaults to crypto/rand.
	Rand func(n int) string
	// ReadyTimeout is how long to wait for pg_isready (default 90 s); Poll is the polling interval (1 s).
	ReadyTimeout time.Duration
	Poll         time.Duration
	// SkipInstances are left out: the preflight of init keeps an
	// instance that was already broken without making the new app.yaml wait on it.
	SkipInstances []string
}

// Result is the outcome of the drill step of one run: the detail of each
// instance goes to --json and /report; run.json keeps the outcome.
type Result struct {
	Kind       string            `json:"kind"`
	App        string            `json:"app"`
	TS         string            `json:"ts"`
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt time.Time         `json:"finished_at"`
	OK         bool              `json:"ok"`
	Instances  []*InstanceResult `json:"instances"`
	Warnings   []string          `json:"warnings,omitempty"`
}

// InstanceResult is the outcome of one instance (one temporary container).
type InstanceResult struct {
	Name string `json:"name"`
	// Result is passed, failed or off, as run.json records it.
	Result string `json:"result"`
	Image  string `json:"image,omitempty"`
	// ImageTag names the build of Image when Image is a digest (a
	// container source): what the source container runs, for display.
	ImageTag    string             `json:"image_tag,omitempty"`
	Mode        string             `json:"mode,omitempty"` // full | data
	TookSeconds float64            `json:"took_seconds,omitempty"`
	OK          bool               `json:"ok"`
	Error       string             `json:"error,omitempty"`
	Checks      []restore.Check    `json:"checks,omitempty"` // the comparison lines that failed
	Notes       []string           `json:"notes,omitempty"`
	Disk        *restore.DiskCheck `json:"disk,omitempty"`
	Databases   []*DatabaseResult  `json:"databases"`
}

// DatabaseResult is the outcome of restoring + comparing one database.
type DatabaseResult struct {
	DB             string        `json:"db"`
	OK             bool          `json:"ok"`
	Bytes          int64         `json:"bytes"`
	RestoreSeconds float64       `json:"restore_seconds"`
	TablesExpected int           `json:"tables_expected"`
	RestoredAs     string        `json:"restored_as"` // the new database it was loaded into
	TablesFound    int           `json:"tables_found"`
	MissingTables  []string      `json:"missing_tables,omitempty"`
	RowMismatches  []RowMismatch `json:"row_mismatches,omitempty"`
	Warnings       []string      `json:"warnings,omitempty"` // extension version differs…
	Error          string        `json:"error,omitempty"`
}

// RowMismatch is a table whose row count differs from run.json (shared with restore).
type RowMismatch = restore.RowMismatch

// Label marks temporary containers so orphans can be cleaned up.
const Label = "dev.bracer.app"

const (
	containerPrefix  = "bracer-drill-"
	defaultReadyWait = 90 * time.Second
	defaultPoll      = time.Second
	slowPull         = 30 * time.Second
)

func (r *Result) warn(format string, args ...interface{}) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...))
}

// Instance returns the result of the instance name, nil when there is none.
func (r *Result) Instance(name string) *InstanceResult {
	for _, ir := range r.Instances {
		if ir.Name == name {
			return ir
		}
	}
	return nil
}

// Failures are the instance and database errors, as printed.
func (ir *InstanceResult) Failures() []string {
	var out []string
	if ir.Error != "" {
		out = append(out, ir.Error)
	}
	for _, dr := range ir.Databases {
		switch {
		case dr.Error != "":
			out = append(out, dr.DB+": "+dr.Error)
		case !dr.OK && ir.Error == "":
			out = append(out, dr.DB+": does not match run.json")
		}
	}
	return out
}

// Step is the drill step of a backup (backup.Deps.Drill): it drills the run
// just written and records the outcome of each instance in run.json and in
// the backup's result. keep, when set, receives the drill's own result.
func Step(cfg *config.App, d Deps, keep func(*Result)) backup.DrillFunc {
	return func(ctx context.Context, run *backup.Run, res *backup.Result) {
		dr, err := Run(ctx, cfg, run, d)
		if keep != nil {
			keep(dr)
		}
		res.Warnings = append(res.Warnings, dr.Warnings...)
		for _, in := range cfg.Instances {
			ri, ir := run.Instances[in.Name], res.Instance(in.Name)
			if ri == nil || ir == nil {
				continue
			}
			x := dr.Instance(in.Name)
			switch {
			case in.DrillOff() || skipped(d.SkipInstances, in.Name):
				ri.Drill = &backup.DrillOutcome{Result: backup.DrillOff}
			case err != nil:
				ri.Drill = &backup.DrillOutcome{Result: backup.DrillFailed, At: dr.StartedAt, Reasons: []string{err.Error()}}
			case x == nil:
				continue
			default:
				ri.Drill = &backup.DrillOutcome{Result: x.Result, At: dr.StartedAt, TookSeconds: x.TookSeconds}
				if !x.OK {
					ri.Drill.Reasons = x.Failures()
				}
				ir.Drill = x
			}
			if ri.Drill.Result == backup.DrillFailed {
				ir.DrillFailed = ri.Drill.Reasons
			}
		}
	}
}

// Run drills the run (spec backup § Drill): every instance of app.yaml not
// turned off, one temporary Postgres each. Per-database/instance errors live
// in Result; an error is returned only when no instance could start (Docker
// is not available), and the Result then has no instance.
func Run(ctx context.Context, cfg *config.App, run *backup.Run, d Deps) (*Result, error) {
	d = withDefaults(d)
	res := &Result{Kind: "drill", App: cfg.App, TS: run.TS, StartedAt: d.Now().UTC(), OK: true}
	defer func() { res.FinishedAt = d.Now().UTC() }()

	var instances []config.Instance
	for _, in := range cfg.Instances {
		if skipped(d.SkipInstances, in.Name) || in.DrillOff() {
			continue
		}
		instances = append(instances, in)
	}
	if len(instances) == 0 {
		return res, nil
	}
	rt, err := pickRuntime(ctx, cfg, d)
	if err != nil {
		res.OK = false
		return res, err
	}
	if what, err := rt.cleanupOrphans(ctx, cfg.App); err != nil {
		res.warn("could not remove leftover drill resources: %v", err)
	} else if what != "" {
		res.warn("removed %s from a previous run", what)
	}

	for _, in := range instances {
		ir := &InstanceResult{Name: in.Name, Result: backup.DrillFailed}
		res.Instances = append(res.Instances, ir)
		start := d.Now()
		if ri := run.Instances[in.Name]; ri == nil {
			ir.Error = "the run has nothing of this instance"
		} else if len(ri.Missing) > 0 {
			// Drilling an older run would report a pass for data that is no
			// longer backed up (ADR-0019).
			ir.Error = fmt.Sprintf("the run is missing %s; nothing to test-restore", strings.Join(ri.Missing, ", "))
		} else {
			runInstance(ctx, rt, cfg, in, run, ri, ir, res, d)
		}
		ir.TookSeconds = d.Now().Sub(start).Seconds()
		if ir.OK {
			ir.Result = backup.DrillPassed
		} else {
			res.OK = false
		}
	}
	return res, nil
}

func skipped(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

func withDefaults(d Deps) Deps {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if d.ReadIdentities == nil {
		d.ReadIdentities = crypto.ReadIdentities
	}
	if d.Rand == nil {
		d.Rand = randomHex
	}
	if d.ReadyTimeout == 0 {
		d.ReadyTimeout = defaultReadyWait
	}
	if d.Poll == 0 {
		d.Poll = defaultPoll
	}
	if d.Tools == nil {
		d.Tools = pg.NewTools(d.Shell)
	}
	return d
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// humanBytes as in the CLI, for disk messages.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
