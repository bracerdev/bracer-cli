// Package drill restores the latest backup into a temporary Postgres (a Docker
// container, or a Kubernetes pod through kubectl) and compares it against the
// manifest (spec backup/drill).
package drill

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	goruntime "runtime"
	"sort"
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
	// Tools finds docker and kubectl to pick the runtime; defaults to pg.NewTools(Shell).
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
	// StartTimeout is how long a pod may take to reach Running (default 10 min; image pull on the node).
	StartTimeout time.Duration

	Keep          bool   // --keep: keep the temporary container
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
	Runtime    string            `json:"runtime,omitempty"` // docker | kube
	Instances  []*InstanceResult `json:"instances"`
	Warnings   []string          `json:"warnings,omitempty"`
}

// InstanceResult is the outcome of one instance (one temporary container).
type InstanceResult struct {
	Name      string             `json:"name"`
	Image     string             `json:"image,omitempty"`
	Container string             `json:"container,omitempty"` // only with --keep (docker)
	Volumes   []string           `json:"volumes,omitempty"`   // only with --keep (docker)
	Pod       string             `json:"pod,omitempty"`       // only with --keep (kube): namespace/name
	Kubectl   string             `json:"kubectl,omitempty"`   // only with --keep (kube): kubectl or k3s kubectl
	OK        bool               `json:"ok"`
	Error     string             `json:"error,omitempty"`
	Disk      *restore.DiskCheck `json:"disk,omitempty"`
	Globals   *GlobalsResult     `json:"globals,omitempty"`
	Databases []*DatabaseResult  `json:"databases"`
}

// GlobalsResult is the outcome of restoring globals (shared with restore).
type GlobalsResult = restore.GlobalsResult

// DatabaseResult is the outcome of restoring + comparing one database.
type DatabaseResult struct {
	DB             string        `json:"db"`
	TS             string        `json:"ts"`
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

// CheckResult is the outcome of one drill.checks entry.
type CheckResult struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

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
	res.Runtime = rt.kind()
	d.Log.Info("drill runtime", "runtime", rt.kind())

	if what, err := rt.cleanupOrphans(ctx, cfg.App); err != nil {
		res.warn("could not remove leftover drill resources: %v", err)
	} else if what != "" {
		res.warn("removed %s from a previous run", what)
	}

	ids := loadIdentities(cfg, manifest, d, res)

	res.OK = true
	for _, in := range instances {
		ir := &InstanceResult{Name: in.Name}
		res.Instances = append(res.Instances, ir)
		runInstance(ctx, rt, cfg, in, manifest.Instances[in.Name], ids, ir, res, d)
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
	if d.StartTimeout == 0 {
		d.StartTimeout = defaultStartWait
	}
	if d.Tools == nil {
		d.Tools = pg.NewTools(d.Shell, goruntime.GOOS)
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

// loadIdentities reads age_identity_file only when the manifest has encrypted dumps.
func loadIdentities(cfg *config.App, m *backup.Manifest, d Deps, res *Result) *restore.Identities {
	needed := false
	for _, im := range m.Instances {
		if latest(im.Globals.Dumps, im.Globals.Latest) != nil && latest(im.Globals.Dumps, im.Globals.Latest).Encrypted {
			needed = true
		}
		for _, dm := range im.Databases {
			if dp := latest(dm.Dumps, dm.Latest); dp != nil && dp.Encrypted {
				needed = true
			}
		}
	}
	if !needed {
		return &restore.Identities{}
	}
	path, err := config.ExpandHome(cfg.AgeIdentityFile)
	var ids *restore.Identities
	if err != nil {
		ids = &restore.Identities{Err: err}
	} else {
		ids = restore.ReadIdentities(path, d.ReadIdentities)
	}
	if ids.Err != nil {
		res.warn("age_identity_file: %v; encrypted dumps cannot be verified", ids.Err)
	}
	return ids
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

// chooseImage (spec drill § Flow 3a): drill.image → extension mapping of the
// databases to restore (postgis/timescaledb/vector → the vendor's own image) →
// postgres:N-alpine, with N = max(major(pg_version), highest tool_version among
// the dumps) → the instance's pg_major. pg_restore in the image must not be older
// than the pg_dump that made the dump.
func chooseImage(cfg *config.App, in config.Instance, im *backup.InstanceManifest, dbs []string) (string, error) {
	if cfg.Drill != nil && cfg.Drill.Image != "" {
		return cfg.Drill.Image, nil
	}
	tool := 0
	var exts []pg.Extension
	for _, db := range dbs {
		dm := im.Databases[db]
		if dp := latest(dm.Dumps, dm.Latest); dp != nil {
			tool = max(tool, pg.MajorOf(dp.ToolVersion))
			exts = append(exts, dp.Extensions...)
		}
	}
	if g := latest(im.Globals.Dumps, im.Globals.Latest); g != nil {
		tool = max(tool, pg.MajorOf(g.ToolVersion))
	}
	n := max(pg.MajorOf(im.PgVersion), tool)
	if n == 0 {
		n = in.PgMajor
	}
	if n == 0 {
		return "", fmt.Errorf("cannot pick a Postgres image: manifest has no pg_version and instance has no pg_major; set drill.image in app.yaml")
	}
	return pg.ImageForExtensions(n, exts)
}

func runInstance(ctx context.Context, rt runtime, cfg *config.App, in config.Instance, im *backup.InstanceManifest, ids *restore.Identities, ir *InstanceResult, res *Result, d Deps) {
	// Which databases to drill: has a latest dump, still in app.yaml, passes --database.
	var dbs []string
	for name, dm := range im.Databases {
		if dm.Latest == "" || latest(dm.Dumps, dm.Latest) == nil {
			continue
		}
		if !in.Databases.All && !inList(in.Databases.List, name) {
			res.warn("%s/%s: in the bucket but not in app.yaml, skipped", in.Name, name)
			continue
		}
		if d.Database != "" && name != d.Database {
			continue
		}
		dbs = append(dbs, name)
	}
	sort.Strings(dbs)
	if len(dbs) == 0 {
		ir.Error = "no database dump to restore"
		return
	}
	image, err := chooseImage(cfg, in, im, dbs)
	if err != nil {
		ir.Error = err.Error()
		return
	}
	ir.Image = image

	// Disk where the temporary Postgres writes: real data ≈ DiskFactor × compressed dump.
	var need int64
	for _, db := range dbs {
		need += latest(im.Databases[db].Dumps, im.Databases[db].Latest).Bytes
	}
	if g := latest(im.Globals.Dumps, im.Globals.Latest); g != nil {
		need += g.Bytes
	}
	t, err := rt.start(ctx, cfg, in, image, need*restore.DiskFactor, ir, res, d)
	if err != nil {
		ir.Error = err.Error()
		return
	}
	defer func() {
		if d.Keep {
			rt.keep(t, ir)
			return
		}
		rt.remove(ctx, t, res, in.Name)
	}()

	// Evidence of which extensions the image has (spec drill § Flow 3b); each
	// database is checked against it before restore.
	avail, err := t.client("postgres").AvailableExtensions(ctx)
	if err != nil {
		ir.Error = fmt.Sprintf("list extensions available in %s: %v", image, err)
		return
	}

	if g := latest(im.Globals.Dumps, im.Globals.Latest); g != nil {
		ir.Globals = restoreGlobals(ctx, t, g, ids, d)
	}

	ir.OK = true
	for _, db := range dbs {
		dm := im.Databases[db]
		dr := restoreDatabase(ctx, cfg, t, db, latest(dm.Dumps, dm.Latest), avail, ids, d)
		ir.Databases = append(ir.Databases, dr)
		if !dr.OK {
			ir.OK = false
		}
	}
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
