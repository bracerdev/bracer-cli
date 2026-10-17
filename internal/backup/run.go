package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/crypto"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/storage"
)

// Deps is everything Run touches outside; tests replace it with fakes.
type Deps struct {
	Shell   pg.Shell
	Store   storage.Store
	Secrets *config.Secrets
	GOOS    string
	Tools   *pg.Tools // nil: built from Shell and GOOS
	Now     func() time.Time
	Log     *slog.Logger
	// Connect can be replaced in tests; defaults to the package's Connect.
	Connect ConnectFunc
}

// Result is the outcome of one run, printed to the console and --json, sent to /report.
type Result struct {
	Kind        string            `json:"kind"`
	App         string            `json:"app"`
	StartedAt   time.Time         `json:"started_at"`
	FinishedAt  time.Time         `json:"finished_at"`
	OK          bool              `json:"ok"`
	Instances   []*InstanceResult `json:"instances"`
	OldKeyDumps int               `json:"old_key_dumps,omitempty"`
	Warnings    []string          `json:"warnings,omitempty"`
}

// InstanceResult is the outcome of one instance.
type InstanceResult struct {
	Name        string            `json:"name"`
	Runner      string            `json:"runner,omitempty"`
	PgVersion   string            `json:"pg_version,omitempty"`
	ToolVersion string            `json:"tool_version,omitempty"` // the pg_dump --version used
	OK          bool              `json:"ok"`
	Error       string            `json:"error,omitempty"`
	Databases   []*DatabaseResult `json:"databases"`
	Added       []string          `json:"added,omitempty"`
	Removed     []string          `json:"removed,omitempty"`
	Globals     *GlobalsResult    `json:"globals,omitempty"`
}

// DatabaseResult is the outcome of one database.
type DatabaseResult struct {
	Name      string  `json:"db"`
	OK        bool    `json:"ok"`
	Bytes     int64   `json:"bytes,omitempty"`
	Tables    int     `json:"tables"`
	Seconds   float64 `json:"seconds"`
	TS        string  `json:"ts,omitempty"`
	Encrypted bool    `json:"encrypted"`
	Error     string  `json:"error,omitempty"`
}

// GlobalsResult is the outcome of pg_dumpall --globals-only.
type GlobalsResult struct {
	OK           bool   `json:"ok"`
	HasPasswords bool   `json:"has_passwords"`
	Bytes        int64  `json:"bytes,omitempty"`
	Error        string `json:"error,omitempty"`
}

func (r *Result) warn(format string, args ...interface{}) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...))
}

const tsLayout = "2006-01-02T150405Z"

// Run backs up the whole app.yaml (spec backup § Flow steps 2–4).
// Per-database/instance errors live in Result; an error is returned only when
// the run cannot start (bucket or manifest cannot be loaded).
func Run(ctx context.Context, cfg *config.App, d Deps) (*Result, error) {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Connect == nil {
		d.Connect = Connect
	}
	if d.Tools == nil {
		d.Tools = pg.NewTools(d.Shell, d.GOOS)
	}
	res := &Result{Kind: "backup", App: cfg.App, StartedAt: d.Now().UTC()}
	if cfg.Storage == nil {
		return nil, errors.New("storage: is not set in app.yaml; run bracer init")
	}
	recipient := ""
	if cfg.Encryption == "age" {
		recipient = cfg.AgeRecipient
		if p, err := config.ExpandHome(cfg.AgeIdentityFile); err != nil || cfg.AgeIdentityFile == "" {
			res.warn("age_identity_file is not set; drill will not be able to decrypt")
		} else if _, recips, err := crypto.ReadIdentities(p); err != nil {
			res.warn("age_identity_file %s is not readable (%v); drill will not be able to decrypt", cfg.AgeIdentityFile, err)
		} else if !contains(recips, recipient) {
			res.warn("age_identity_file %s does not hold the private key for %s", cfg.AgeIdentityFile, recipient)
		}
	}

	manifestKey := storage.Join(cfg.Storage.Prefix, cfg.App, "manifest.json")
	manifest, err := LoadManifest(ctx, d.Store, manifestKey, cfg.App)
	skipRetention, skipSave := false, false
	var corrupt *CorruptError
	if errors.As(err, &corrupt) {
		// Preserve the corrupt bytes before overwriting: they are the only index of the old dumps.
		copyKey := manifestKey + ".corrupt-" + d.Now().UTC().Format(tsLayout)
		if perr := d.Store.Put(ctx, copyKey, bytes.NewReader(corrupt.Raw), int64(len(corrupt.Raw))); perr != nil {
			res.warn("%v; could not save a copy to %s (%v), so manifest.json is left untouched this run and retention is skipped", err, copyKey, perr)
			skipSave = true
		} else {
			res.warn("%v; the old file was copied to %s; treating it as empty and skipping retention this run", err, copyKey)
		}
		skipRetention = true
	} else if err != nil {
		return nil, fmt.Errorf("load manifest: %w", err)
	}
	added := map[string]bool{} // keys created this run: retention never deletes them

	for _, in := range cfg.Instances {
		ir := &InstanceResult{Name: in.Name}
		res.Instances = append(res.Instances, ir)
		runInstance(ctx, cfg, in, manifest, ir, recipient, added, d)
	}

	// Write the manifest first, retention afterwards (AC retention). Retention: drop
	// from the manifest → write the manifest → only then delete objects, so the manifest
	// never points to a missing object (a failed delete only leaves an orphan object).
	if skipSave {
		res.warn("manifest.json not written this run; the new dumps are in the bucket but not indexed until the next successful run")
	} else if err := SaveManifest(ctx, d.Store, manifestKey, manifest, d.Now()); err != nil {
		res.warn("manifest.json could not be written: %v; retention skipped", err)
		skipRetention = true
	}
	if !skipRetention {
		toDelete := applyRetention(cfg, manifest, added, res)
		if len(toDelete) > 0 {
			if err := SaveManifest(ctx, d.Store, manifestKey, manifest, d.Now()); err != nil {
				res.warn("manifest.json could not be rewritten after retention: %v; nothing deleted", err)
			} else {
				for _, key := range toDelete {
					if err := d.Store.Delete(ctx, key); err != nil {
						res.warn("retention: could not delete %s: %v (left as an orphan)", key, err)
						continue
					}
					d.Log.Info("retention: deleted", "key", key)
				}
			}
		}
	}
	if !skipSave {
		reportOrphans(ctx, cfg, manifestKey, manifest, res, d)
	}
	if recipient != "" {
		res.OldKeyDumps = manifest.countOldKey(recipient)
	}
	res.FinishedAt = d.Now().UTC()
	res.OK = true
	for _, ir := range res.Instances {
		if !ir.OK {
			res.OK = false
		}
	}
	return res, nil
}

func runInstance(ctx context.Context, cfg *config.App, in config.Instance, m *Manifest, ir *InstanceResult, recipient string, added map[string]bool, d Deps) {
	conn, runner, err := d.Connect(ctx, d.Tools, in, d.Secrets)
	if err != nil {
		ir.Error = err.Error()
		return
	}
	ir.Runner = runner.Describe()
	client := &pg.Client{Runner: runner, Conn: conn}
	if err := client.Ping(ctx); err != nil {
		ir.Error = fmt.Sprintf("connect: %v", err)
		return
	}
	ver, err := client.ServerVersion(ctx)
	if err != nil {
		ir.Error = err.Error()
		return
	}
	ir.PgVersion = ver
	im := m.instance(in.Name)
	im.PgVersion = ver
	toolVer, _, err := client.ToolVersion(ctx)
	if err != nil {
		ir.Error = err.Error()
		return
	}
	ir.ToolVersion = toolVer

	var dbs []string
	if in.Databases.All {
		dbs, err = client.ListDatabases(ctx)
		if err != nil {
			ir.Error = fmt.Sprintf("list databases: %v", err)
			return
		}
		known := im.knownDatabases()
		if len(known) > 0 {
			ir.Added = diff(dbs, known)
			ir.Removed = diff(known, dbs)
		}
	} else {
		for _, db := range in.Databases.List {
			dbs = append(dbs, db.Name)
		}
	}

	priv, err := client.Privileges(ctx)
	if err != nil {
		ir.Error = fmt.Sprintf("check user: %v", err)
		return
	}

	ir.OK = true
	for _, db := range dbs {
		dr := &DatabaseResult{Name: db, Encrypted: recipient != ""}
		ir.Databases = append(ir.Databases, dr)
		start := d.Now()
		ts := start.UTC().Format(tsLayout)
		dr.TS = ts
		// Encoding/locale so restore can recreate the database identically (spec backup § Flow 3c);
		// if it cannot be read, the dump cannot be recreated identically → this database fails.
		loc, err := client.DatabaseLocale(ctx, db, pg.MajorOf(ver))
		if err != nil {
			dr.Error = fmt.Sprintf("read encoding/locale of %s: %v", db, err)
			ir.OK = false
			d.Log.Error("dump failed", "instance", in.Name, "db", db, "err", dr.Error)
			continue
		}
		c := client.WithDB(db)
		exts, err := c.Extensions(ctx)
		if err != nil {
			dr.Error = fmt.Sprintf("read extensions of %s: %v", db, err)
			ir.OK = false
			d.Log.Error("dump failed", "instance", in.Name, "db", db, "err", dr.Error)
			continue
		}
		// One deadline per database: count + dump + upload (spec backup § Edge cases).
		dbctx, cancel := context.WithTimeout(ctx, cfg.BackupTimeout())
		n, sha, tables, key, err := dumpDatabase(dbctx, cfg, in, runner, c.Conn, db, ts, recipient, d)
		cancel()
		dr.Seconds = d.Now().Sub(start).Seconds()
		if err != nil {
			if errors.Is(dbctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				err = fmt.Errorf("timed out after %s (backup.timeout); the connection was closed, PostgreSQL 14+ stops the server-side work within 10 s: %w", cfg.BackupTimeout(), err)
			}
			dr.Error = err.Error()
			ir.OK = false
			d.Log.Error("dump failed", "instance", in.Name, "db", db, "err", err)
			continue
		}
		dr.Tables = len(tables)
		dr.OK, dr.Bytes = true, n
		added[key] = true
		im.database(db).addDump(&Dump{TS: ts, Key: key, ToolVersion: toolVer, Locale: loc, Extensions: exts, Bytes: n, SHA256: sha, Encrypted: recipient != "", Recipient: recipientPtr(recipient), UploadedAt: d.Now().UTC(), Tables: tables})
		d.Log.Info("dumped", "instance", in.Name, "db", db, "bytes", n, "seconds", fmt.Sprintf("%.1f", dr.Seconds))
	}

	// Globals: once per instance. → ADR-0006
	gr := &GlobalsResult{HasPasswords: priv.Superuser || priv.ReadAllData}
	ir.Globals = gr
	args := []string{"--globals-only"}
	if !gr.HasPasswords {
		args = append(args, "--no-role-passwords")
	}
	args = append(args, lockWaitArg(cfg))
	ts := d.Now().UTC().Format(tsLayout)
	key := storage.Join(cfg.Storage.Prefix, cfg.App, in.Name, "globals", ts+ext(".sql", recipient))
	gctx, cancel := context.WithTimeout(ctx, cfg.BackupTimeout())
	n, sha, err := streamToStore(gctx, runner, conn, "pg_dumpall", args, d.Store, key, recipient)
	cancel()
	if err != nil {
		gr.Error = err.Error()
		ir.OK = false
		return
	}
	gr.OK, gr.Bytes = true, n
	added[key] = true
	im.Globals.Dumps = append(im.Globals.Dumps, &Dump{TS: ts, Key: key, ToolVersion: toolVer, Bytes: n, SHA256: sha, Encrypted: recipient != "", Recipient: recipientPtr(recipient), UploadedAt: d.Now().UTC()})
	sortDumps(im.Globals.Dumps)
	im.Globals.Latest = ts
	im.Globals.HasPasswords = gr.HasPasswords
}

// applyRetention drops from the manifest the dumps beyond `keep` per database
// (in uploaded_at order) and returns the keys to delete. It never deletes dumps
// created this run (added) even with clock skew, and only deletes keys under <prefix>/<app>/.
func applyRetention(cfg *config.App, m *Manifest, added map[string]bool, res *Result) []string {
	defKeep := 7
	if cfg.Retention != nil {
		defKeep = cfg.Retention.Keep
	}
	root := storage.Join(cfg.Storage.Prefix, cfg.App) + "/"
	var toDelete []string
	for _, in := range cfg.Instances {
		im := m.Instances[in.Name]
		if im == nil {
			continue
		}
		names := make([]string, 0, len(im.Databases))
		for name := range im.Databases {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			dm := im.Databases[name]
			keep := defKeep
			for _, db := range in.Databases.List {
				if db.Name == name && db.Retention != nil {
					keep = db.Retention.Keep
				}
			}
			var del []string
			dm.Dumps, del = prune(dm.Dumps, keep, added, root, res)
			toDelete = append(toDelete, del...)
			if len(dm.Dumps) > 0 {
				dm.Latest = dm.Dumps[len(dm.Dumps)-1].TS
			}
		}
		var del []string
		im.Globals.Dumps, del = prune(im.Globals.Dumps, defKeep, added, root, res)
		toDelete = append(toDelete, del...)
	}
	return toDelete
}

// prune drops the oldest dumps until `keep` remain; a dump created this run is kept
// and stops pruning (no skipping past it to delete others); a key outside root is kept and the next one is examined.
func prune(dumps []*Dump, keep int, added map[string]bool, root string, res *Result) ([]*Dump, []string) {
	sortDumps(dumps)
	var del []string
	var kept []*Dump
	for len(dumps)+len(kept) > keep {
		old := dumps[0]
		if added[old.Key] {
			break
		}
		if !strings.HasPrefix(old.Key, root) {
			// Foreign key (manifest edited/copied): keep it but still examine the others,
			// so one foreign key does not make this database accumulate dumps forever.
			res.warn("retention: %s is outside %s; not deleting it", old.Key, root)
			kept = append(kept, old)
			dumps = dumps[1:]
			continue
		}
		del = append(del, old.Key)
		dumps = dumps[1:]
	}
	return append(kept, dumps...), del
}

// reportOrphans lists objects under <prefix>/<app>/ that the manifest does not
// point to: a sign of two machines writing the same app or of an overwritten manifest.
func reportOrphans(ctx context.Context, cfg *config.App, manifestKey string, m *Manifest, res *Result, d Deps) {
	root := storage.Join(cfg.Storage.Prefix, cfg.App) + "/"
	objs, err := d.Store.List(ctx, root)
	if err != nil {
		return
	}
	keys := m.Keys()
	var orphans []string
	for _, o := range objs {
		if o.Key == manifestKey || strings.HasPrefix(o.Key, manifestKey+".corrupt-") || keys[o.Key] {
			continue
		}
		orphans = append(orphans, o.Key)
	}
	if len(orphans) == 0 {
		return
	}
	sort.Strings(orphans)
	shown := orphans
	if len(shown) > 3 {
		shown = shown[:3]
	}
	res.warn("%d object(s) under %s are not in manifest.json (e.g. %s); another machine may be writing the same app, or the manifest was overwritten. They are left untouched", len(orphans), root, strings.Join(shown, ", "))
}

// lockWaitArg is the --lock-wait-timeout for pg_dump/pg_dumpall from backup.lock_timeout.
func lockWaitArg(cfg *config.App) string {
	return fmt.Sprintf("--lock-wait-timeout=%d", cfg.BackupLockTimeout().Milliseconds())
}

// dumpDatabase opens a snapshot, sets lock_timeout, counts rows, then pg_dump --snapshot
// to the bucket; everything within ctx (which carries the backup.timeout deadline).
func dumpDatabase(ctx context.Context, cfg *config.App, in config.Instance, runner pg.Runner, conn pg.Conn, db, ts, recipient string, d Deps) (n int64, sha string, tables []pg.Table, key string, err error) {
	// Count rows and dump within the same snapshot (spec backup, § Row counts and snapshot);
	// lock_timeout so we do not queue behind a waiting ALTER TABLE.
	snap, err := pg.BeginSnapshot(ctx, runner, conn, cfg.BackupLockTimeout())
	if err != nil {
		return 0, "", nil, "", fmt.Errorf("open snapshot: %w", err)
	}
	tables, err = snap.Tables()
	if err == nil {
		err = countRows(snap, tables, cfg.RowCountsMode(), cfg.ExactMaxRowsOrDefault())
	}
	if err != nil {
		snap.Close()
		return 0, "", nil, "", fmt.Errorf("read tables: %w", err)
	}
	key = storage.Join(cfg.Storage.Prefix, cfg.App, in.Name, db, ts+ext(".dump", recipient))
	args := append([]string{"-Fc", lockWaitArg(cfg)}, snap.Args()...)
	n, sha, err = streamToStore(ctx, runner, conn, "pg_dump", args, d.Store, key, recipient)
	if endErr := snap.End(); err == nil && endErr != nil {
		d.Log.Warn("close snapshot session", "instance", in.Name, "db", db, "err", endErr)
	}
	if err != nil {
		return 0, "", nil, "", err
	}
	return n, sha, tables, key, nil
}

func ext(base, recipient string) string {
	if recipient != "" {
		return base + ".age"
	}
	return base
}

func recipientPtr(r string) *string {
	if r == "" {
		return nil
	}
	return &r
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// countRows fills Table.Rows per row_counts: exact counts everything, estimate counts
// nothing, auto counts tables whose reltuples is below the threshold or never ANALYZEd (-1).
// A counted table drops RowsEst so the manifest records only one of the two.
func countRows(snap *pg.Snapshot, tables []pg.Table, mode string, maxRows int64) error {
	for i := range tables {
		t := &tables[i]
		if !shouldCount(*t, mode, maxRows) {
			continue
		}
		n, err := snap.Count(*t)
		if err != nil {
			return fmt.Errorf("count %s: %w", t.Ident(), err)
		}
		t.Rows, t.RowsEst = &n, nil
	}
	return nil
}

func shouldCount(t pg.Table, mode string, maxRows int64) bool {
	if t.Kind == "p" {
		return false // partitioned parent: count(*) would rescan every child partition already counted
	}
	switch mode {
	case config.RowCountsExact:
		return true
	case config.RowCountsEstimate:
		return false
	default:
		return t.RowsEst == nil || *t.RowsEst < maxRows
	}
}
