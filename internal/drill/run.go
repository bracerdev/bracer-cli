// Package drill restores the latest backup into a temporary Postgres (a Docker
// container) and compares it against the manifest (spec backup/drill).
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
	// ConfigPath is the absolute path of app.yaml, named in `Fix: bracer init -c …`.
	ConfigPath string

	Keep          bool   // --keep: keep the temporary container
	ForceDocker   bool   // --runtime docker: run even with drill.runtime: none
	Instance      string // --instance: only this instance
	Database      string // --database: only this database
	SkipDiskCheck bool   // --skip-disk-check: run even if Docker root seems short on space
}

// Result is the outcome of one drill, printed to the console and --json, sent to /report.
type Result struct {
	Kind       string            `json:"kind"`
	App        string            `json:"app"`
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt time.Time         `json:"finished_at"`
	OK         bool              `json:"ok"`
	Runtime    string            `json:"runtime,omitempty"` // docker
	Instances  []*InstanceResult `json:"instances"`
	Warnings   []string          `json:"warnings,omitempty"`
}

// InstanceResult is the outcome of one instance (one temporary container).
type InstanceResult struct {
	Name      string   `json:"name"`
	Image     string   `json:"image,omitempty"`
	Container string   `json:"container,omitempty"` // only with --keep (docker)
	Volumes   []string `json:"volumes,omitempty"`   // only with --keep (docker)
	User      string   `json:"user,omitempty"`      // only with --keep: the superuser to connect as
	// SystemID is the system_identifier of the cluster the restored dumps came from.
	SystemID  string             `json:"system_id,omitempty"`
	TS        string             `json:"ts,omitempty"`     // the run drilled
	Source    string             `json:"source,omitempty"` // container | local | url
	Mode      string             `json:"mode,omitempty"`   // full | data
	OK        bool               `json:"ok"`
	Error     string             `json:"error,omitempty"`
	Checks    []restore.Check    `json:"checks,omitempty"` // the comparison lines that failed
	Notes     []string           `json:"notes,omitempty"`
	Disk      *restore.DiskCheck `json:"disk,omitempty"`
	Databases []*DatabaseResult  `json:"databases"`
}

// DatabaseResult is the outcome of restoring + comparing one database.
type DatabaseResult struct {
	DB             string        `json:"db"`
	TS             string        `json:"ts"`
	SystemID       string        `json:"system_id,omitempty"` // cluster the dump came from (spec backup § Flow 3a2)
	OK             bool          `json:"ok"`
	Bytes          int64         `json:"bytes"`
	RestoreSeconds float64       `json:"restore_seconds"`
	TablesExpected int           `json:"tables_expected"`
	RestoredAs     string        `json:"restored_as,omitempty"` // the `postgres` dump is restored into this database
	TablesFound    int           `json:"tables_found"`
	MissingTables  []string      `json:"missing_tables,omitempty"`
	RowMismatches  []RowMismatch `json:"row_mismatches,omitempty"`
	Checks         []CheckResult `json:"checks,omitempty"`
	Warnings       []string      `json:"warnings,omitempty"` // extension version differs…
	Error          string        `json:"error,omitempty"`
}

// RowMismatch is a table whose row count differs from the manifest (shared with restore).
type RowMismatch = restore.RowMismatch

// CheckResult is the outcome of one drill.checks entry (shared with restore).
type CheckResult = restore.CheckResult

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

// Run drills the whole app.yaml (spec drill § Flow). Per-database/instance errors
// live in Result; an error is returned only when the run cannot start.
func Run(ctx context.Context, cfg *config.App, d Deps) (*Result, error) {
	d = withDefaults(d)
	if cfg.DrillOff() && !d.ForceDocker {
		return nil, OffError(cfg.App)
	}
	res := &Result{Kind: "drill", App: cfg.App, StartedAt: d.Now().UTC()}

	manifestKey := storage.Join(cfg.Storage.Prefix, cfg.App, "manifest.json")
	manifest, err := backup.LoadManifest(ctx, d.Store, manifestKey, cfg.App)
	if err != nil {
		return nil, fmt.Errorf("load manifest: %w", err)
	}

	var instances []config.Instance
	var missing []string
	for _, in := range cfg.Instances {
		if d.Instance != "" && in.Name != d.Instance {
			continue
		}
		if im := manifest.Instances[in.Name]; im != nil && len(im.Databases) > 0 {
			instances = append(instances, in)
		} else {
			missing = append(missing, in.Name)
		}
	}
	if d.Instance != "" && len(instances) == 0 && len(missing) == 0 {
		return nil, fmt.Errorf("instance %q is not in app.yaml", d.Instance)
	}
	if len(instances) == 0 {
		return nil, fmt.Errorf("no backup for instance(s) %s in the bucket; run bracer backup first", strings.Join(missing, ", "))
	}
	for _, name := range missing {
		res.warn("instance %s has no backup yet, skipped", name)
	}

	rt, err := pickRuntime(ctx, cfg, d)
	if err != nil {
		return nil, err
	}
	res.Runtime = config.DrillRuntimeDocker

	if what, err := rt.cleanupOrphans(ctx, cfg.App); err != nil {
		res.warn("could not remove leftover drill resources: %v", err)
	} else if what != "" {
		res.warn("removed %s from a previous run", what)
	}

	res.OK = true
	for _, in := range instances {
		ir := &InstanceResult{Name: in.Name}
		res.Instances = append(res.Instances, ir)
		runInstance(ctx, rt, cfg, in, manifest.Instances[in.Name], ir, res, d)
		if !ir.OK {
			res.OK = false
		}
	}
	res.FinishedAt = d.Now().UTC()
	return res, nil
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

// latest finds the dump with TS = ts.
func latest(dumps []*backup.Dump, ts string) *backup.Dump {
	for _, dp := range dumps {
		if dp.TS == ts {
			return dp
		}
	}
	return nil
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

func inList(list []config.Database, name string) bool {
	for _, db := range list {
		if db.Name == name {
			return true
		}
	}
	return false
}
