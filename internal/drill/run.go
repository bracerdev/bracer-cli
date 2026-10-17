// Package drill restores the latest backup into a temporary Postgres in Docker
// and compares it against the manifest (spec backup/drill).
package drill

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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
	Instances  []*InstanceResult `json:"instances"`
	Warnings   []string          `json:"warnings,omitempty"`
}

// InstanceResult is the outcome of one instance (one temporary container).
type InstanceResult struct {
	Name      string             `json:"name"`
	Image     string             `json:"image,omitempty"`
	Container string             `json:"container,omitempty"` // only with --keep
	Volumes   []string           `json:"volumes,omitempty"`   // only with --keep
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

	if nc, nv, err := cleanupOrphans(ctx, d, cfg.App); err != nil {
		res.warn("could not remove leftover drill containers/volumes: %v", err)
	} else if nc+nv > 0 {
		res.warn("removed %d leftover drill container(s) and %d volume(s) from a previous run", nc, nv)
	}

	ids := loadIdentities(cfg, manifest, d, res)

	res.OK = true
	for _, in := range instances {
		ir := &InstanceResult{Name: in.Name}
		res.Instances = append(res.Instances, ir)
		runInstance(ctx, cfg, in, manifest.Instances[in.Name], ids, ir, res, d)
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

// cleanupOrphans removes the app's drill containers and volumes left over from a
// previous run (same label; named volumes can be cleaned up, unlike anonymous ones).
func cleanupOrphans(ctx context.Context, d Deps, app string) (containers, volumes int, err error) {
	out, err := d.Shell.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"ps", "-aq", "--filter", "label=" + Label + "=" + app, "--filter", "name=" + containerPrefix}})
	if err != nil {
		return 0, 0, err
	}
	if ids := strings.Fields(string(out.Stdout)); len(ids) > 0 {
		if _, err := d.Shell.Run(ctx, pg.Cmd{Name: "docker", Args: append([]string{"rm", "-f", "-v"}, ids...)}); err != nil {
			return 0, 0, err
		}
		containers = len(ids)
	}
	out, err = d.Shell.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"volume", "ls", "-q", "--filter", "label=" + Label + "=" + app, "--filter", "name=" + containerPrefix}})
	if err != nil {
		return containers, 0, err
	}
	if names := strings.Fields(string(out.Stdout)); len(names) > 0 {
		if _, err := d.Shell.Run(ctx, pg.Cmd{Name: "docker", Args: append([]string{"volume", "rm"}, names...)}); err != nil {
			return containers, 0, err
		}
		volumes = len(names)
	}
	return containers, volumes, nil
}

// checkDockerDisk measures the free disk where the drill will write (spec drill § Data model):
// df in a temporary container of the (already pulled) image with the drill volume
// mounted read-only — the very filesystem the restore will use, even when Docker
// runs in a VM, and without bind-mounting anything from the host into the image
// (Docker root holds the data of every other container). If that cannot run → df
// on the Docker root directly on the host; if both fail → Checked=false.
func checkDockerDisk(ctx context.Context, d Deps, image, volume string, needBytes int64) restore.DiskCheck {
	dc := restore.DiskCheck{NeedBytes: needBytes}
	const mnt = "/mnt/bracer-disk"
	out, err := d.Shell.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"run", "--rm", "--network", "none", "-v", volume + ":" + mnt + ":ro", image, "df", "-Pk", mnt}})
	where := "volume " + volume
	if err != nil {
		info, ierr := d.Shell.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"info", "--format", "{{.DockerRootDir}}"}})
		dir := strings.TrimSpace(string(info.Stdout))
		if ierr != nil || dir == "" {
			dc.Error = fmt.Sprintf("could not check free disk space (df in %s: %v; docker info gave no root directory)", image, err)
			return dc
		}
		where = dir
		out, err = d.Shell.Run(ctx, pg.Cmd{Name: "df", Args: []string{"-Pk", dir}})
	}
	dc.DataDirectory = where
	if err != nil {
		dc.Error = fmt.Sprintf("could not check free disk space at %s (%v)", where, err)
		return dc
	}
	free, err := pg.ParseDF(out.Stdout)
	if err != nil {
		dc.Error = fmt.Sprintf("could not read df output for %s: %v", where, err)
		return dc
	}
	dc.Checked, dc.FreeBytes = true, free
	return dc
}

// imageVolumes reads the image's VOLUME paths (pulling it if absent) so that
// named volumes can be mounted there, avoiding anonymous volumes.
func imageVolumes(ctx context.Context, d Deps, image string) ([]string, error) {
	inspect := func() ([]string, error) {
		out, err := d.Shell.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"image", "inspect", "-f", "{{json .Config.Volumes}}", image}})
		if err != nil {
			return nil, err
		}
		var vols map[string]struct{}
		if err := json.Unmarshal(bytes.TrimSpace(out.Stdout), &vols); err != nil {
			return nil, fmt.Errorf("parse image volumes: %w", err)
		}
		paths := make([]string, 0, len(vols))
		for p := range vols {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		return paths, nil
	}
	paths, err := inspect()
	if err == nil {
		return paths, nil
	}
	if _, perr := d.Shell.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"pull", "-q", image}}); perr != nil {
		return nil, fmt.Errorf("pull %s: %w", image, perr)
	}
	return inspect()
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

// tempPG is the temporary Postgres container; tools run via docker exec, connecting
// over the socket as user postgres.
type tempPG struct {
	sh    pg.Shell
	name  string
	image string
}

func (t *tempPG) runner() pg.Runner { return pg.ExecRunner{Shell: t.sh, Container: t.name} }

func (t *tempPG) conn(db string) pg.Conn { return pg.Conn{User: "postgres", Database: db} }

// client returns a pg.Client connected to database db of the temporary container.
func (t *tempPG) client(db string) *pg.Client {
	return &pg.Client{Runner: t.runner(), Conn: t.conn(db)}
}

func runInstance(ctx context.Context, cfg *config.App, in config.Instance, im *backup.InstanceManifest, ids *restore.Identities, ir *InstanceResult, res *Result, d Deps) {
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

	// Docker root disk on this machine: real data ≈ DiskFactor × compressed dump.
	var need int64
	for _, db := range dbs {
		need += latest(im.Databases[db].Dumps, im.Databases[db].Latest).Bytes
	}
	if g := latest(im.Globals.Dumps, im.Globals.Latest); g != nil {
		need += g.Bytes
	}
	// A named + labeled volume for each VOLUME of the image: docker rm -f does not
	// remove a --rm container's anonymous volumes; each drill would leave a copy of the data behind.
	paths, err := imageVolumes(ctx, d, image)
	if err != nil {
		ir.Error = fmt.Sprintf("inspect %s: %v", image, err)
		return
	}
	t := &tempPG{sh: d.Shell, name: containerPrefix + cfg.App + "-" + d.Rand(3), image: image}
	// POSTGRES_PASSWORD is required by the official image; never used (socket connection).
	runArgs := []string{"run", "-d", "--rm", "--network", "none", "--label", Label + "=" + cfg.App, "--name", t.name, "-e", "POSTGRES_PASSWORD"}
	var volumes []string
	// Image declares no VOLUME: still create one volume (not mounted) to measure disk.
	if len(paths) == 0 {
		paths = []string{""}
	}
	for i, p := range paths {
		name := t.name
		if i > 0 {
			name = fmt.Sprintf("%s-%d", t.name, i)
		}
		if _, err := d.Shell.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"volume", "create", "--label", Label + "=" + cfg.App, name}}); err != nil {
			ir.Error = fmt.Sprintf("create volume %s: %v", name, err)
			removeVolumes(ctx, d, volumes, res, in.Name)
			return
		}
		volumes = append(volumes, name)
		if p != "" {
			runArgs = append(runArgs, "-v", name+":"+p)
		}
	}
	// Measure disk through the volume just created, before starting the container.
	dc := checkDockerDisk(ctx, d, image, volumes[0], need*restore.DiskFactor)
	ir.Disk = &dc
	switch {
	case !dc.Checked:
		res.warn("%s: %s; the drill may fill the disk", in.Name, dc.Error)
	case !dc.Enough() && !d.SkipDiskCheck:
		ir.Error = fmt.Sprintf("not enough disk space for a drill: %s free at %s, about %s may be needed; free space or pass --skip-disk-check", humanBytes(dc.FreeBytes), dc.DataDirectory, humanBytes(dc.NeedBytes))
		removeVolumes(ctx, d, volumes, res, in.Name)
		return
	case !dc.Enough():
		res.warn("%s: only %s free at %s, about %s may be needed (--skip-disk-check)", in.Name, humanBytes(dc.FreeBytes), dc.DataDirectory, humanBytes(dc.NeedBytes))
	}
	start := d.Now()
	if cfg.Drill != nil {
		if cfg.Drill.Memory != "" {
			runArgs = append(runArgs, "--memory", cfg.Drill.Memory)
		}
		if cfg.Drill.CPUs != "" {
			runArgs = append(runArgs, "--cpus", cfg.Drill.CPUs)
		}
	}
	runArgs = append(runArgs, image)
	if _, err := d.Shell.Run(ctx, pg.Cmd{Name: "docker", Args: runArgs, Env: []string{"POSTGRES_PASSWORD=" + d.Rand(16)}}); err != nil {
		ir.Error = fmt.Sprintf("start %s: %v", image, err)
		removeVolumes(ctx, d, volumes, res, in.Name)
		return
	}
	if took := d.Now().Sub(start); took > slowPull {
		res.warn("%s: starting %s took %.0f s (image pull?)", in.Name, image, took.Seconds())
	}
	d.Log.Info("temp postgres started", "instance", in.Name, "container", t.name, "image", image)
	defer func() {
		if d.Keep {
			ir.Container = t.name
			ir.Volumes = volumes
			return
		}
		// Ctrl-C/timeout must still clean up: a fresh context, not inheriting the cancelled ctx.
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		if _, err := d.Shell.Run(rmCtx, pg.Cmd{Name: "docker", Args: []string{"rm", "-f", "-v", t.name}}); err != nil {
			res.warn("%s: could not remove container %s: %v; run: docker rm -f -v %s", in.Name, t.name, err, t.name)
			return // the volumes are still attached to the container; remove them along with it
		}
		removeVolumes(rmCtx, d, volumes, res, in.Name)
	}()

	if err := t.waitReady(ctx, d); err != nil {
		ir.Error = err.Error()
		return
	}
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

// removeVolumes removes the drill's named volumes; on error it only warns, with the manual cleanup command.
func removeVolumes(ctx context.Context, d Deps, volumes []string, res *Result, instance string) {
	if len(volumes) == 0 {
		return
	}
	if _, err := d.Shell.Run(ctx, pg.Cmd{Name: "docker", Args: append([]string{"volume", "rm"}, volumes...)}); err != nil {
		res.warn("%s: could not remove volume(s) %s: %v; run: docker volume rm %s", instance, strings.Join(volumes, " "), err, strings.Join(volumes, " "))
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

// waitReady polls pg_isready over TCP loopback inside the container: the entrypoint's
// temporary server (during initdb) opens only the socket, so TCP being up means the real server runs.
func (t *tempPG) waitReady(ctx context.Context, d Deps) error {
	deadline := d.Now().Add(d.ReadyTimeout)
	for {
		if err := t.runner().Run(ctx, t.conn("postgres"), "pg_isready", []string{"-h", "127.0.0.1", "-U", "postgres"}, nil, io.Discard); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("interrupted while waiting for Postgres: %v", ctx.Err())
		}
		if !d.Now().Before(deadline) {
			logs, _ := d.Shell.Run(ctx, pg.Cmd{Name: "docker", Args: []string{"logs", "--tail", "20", t.name}})
			return fmt.Errorf("Postgres in %s not ready after %.0f s; last log lines:\n%s", t.name, d.ReadyTimeout.Seconds(), restore.Tail(append(logs.Stdout, logs.Stderr...), restore.StderrTail))
		}
		select {
		case <-ctx.Done():
		case <-time.After(d.Poll):
		}
	}
}
