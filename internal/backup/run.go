package backup

import (
	"bytes"
	"context"
	"encoding/json"
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
	Tools   *pg.Tools // nil: built from Shell
	Now     func() time.Time
	Log     *slog.Logger
	// Connect can be replaced in tests; defaults to the package's Connect.
	Connect ConnectFunc
	// ConfigPath is the absolute path of app.yaml, named in `Fix: bracer init -c …`.
	ConfigPath string
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
	Name      string `json:"name"`
	Runner    string `json:"runner,omitempty"`
	PgVersion string `json:"pg_version,omitempty"`
	SystemID  string `json:"system_id,omitempty"`
	// IdentityChanged: the server is another cluster than the one init recorded;
	// dumps are taken but retention keeps every old one (spec backup § Flow 3a2).
	IdentityChanged bool              `json:"identity_changed,omitempty"`
	ToolVersion     string            `json:"tool_version,omitempty"` // the pg_dump --version used
	OK              bool              `json:"ok"`
	Error           string            `json:"error,omitempty"`
	Databases       []*DatabaseResult `json:"databases"`
	Added           []string          `json:"added,omitempty"`
	Removed         []string          `json:"removed,omitempty"`
	Globals         *GlobalsResult    `json:"globals,omitempty"`
	Env             *EnvResult        `json:"env,omitempty"`
	Kind            string            `json:"source_kind,omitempty"` // container, local, url
}

// EnvResult is the outcome of writing the environment file (ADR-0015).
type EnvResult struct {
	OK    bool   `json:"ok"`
	Bytes int64  `json:"bytes,omitempty"`
	Error string `json:"error,omitempty"`
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
		d.Tools = pg.NewTools(d.Shell)
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
		runInstance(ctx, cfg, in, manifest, ir, recipient, added, res, d)
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
		toDelete := applyRetention(cfg, manifest, added, identityChanged(res), res)
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
		saveAppYAML(ctx, cfg, recipient, res, d)
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

// identityChanged lists the instances whose retention is held this run.
func identityChanged(res *Result) map[string]bool {
	out := map[string]bool{}
	for _, ir := range res.Instances {
		if ir.IdentityChanged {
			out[ir.Name] = true
		}
	}
	return out
}

// fixInit is the Fix line pointing at init on this app.yaml.
func fixInit(d Deps) string {
	path := d.ConfigPath
	if path == "" {
		path = "<app.yaml>"
	}
	return "Fix: bracer init -c " + path
}

// checkIdentity is spec backup § Flow 3a2: compare the server with what init
// recorded. A different cluster is still dumped, but retention is held; a
// different major only warns. Never stops a backup because something changed.
func checkIdentity(ctx context.Context, client *pg.Client, in config.Instance, ver string, ir *InstanceResult, res *Result, d Deps) {
	if major := pg.MajorOf(ver); in.PgMajor != 0 && major != 0 && major != in.PgMajor {
		res.warn("Postgres on %s is now %d (app.yaml says %d)\n  drill picks its image from pg_major\n  %s", in.Name, major, in.PgMajor, fixInit(d))
	}
	id, err := client.SystemIdentifier(ctx)
	if err != nil {
		if in.SystemID != "" {
			res.warn("%s: could not read system_identifier (%v); not checking whether it is still the same Postgres cluster", in.Name, firstLine(err.Error()))
		}
		return
	}
	ir.SystemID = id
	if in.SystemID == "" || id == in.SystemID {
		return
	}
	ir.IdentityChanged = true
	when := "at an unknown time"
	if t := pg.InitdbTime(id); !t.IsZero() {
		when = t.Format("2006-01-02 15:04 UTC")
	}
	res.warn("%s is a different Postgres cluster than before (initialized %s); old backups are kept\n  system_identifier is %s, app.yaml says %s; retention is held for this instance until init confirms the new cluster\n  %s", in.Name, when, id, in.SystemID, fixInit(d))
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func runInstance(ctx context.Context, cfg *config.App, in config.Instance, m *Manifest, ir *InstanceResult, recipient string, added map[string]bool, res *Result, d Deps) {
	// One ts for the whole run: every database, the globals and the
	// environment file, so restore and drill take one run whole (ADR-0015).
	ts := d.Now().UTC().Format(tsLayout)
	kind := in.Kind()
	conn, runner, err := d.Connect(ctx, d.Tools, in, d.Secrets)
	if err != nil {
		ir.Error = err.Error()
		return
	}
	ir.Runner = runner.Describe()
	ir.Kind = kind.String()
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
	checkIdentity(ctx, client, in, ver, ir, res, d)
	im := m.instance(in.Name)
	im.PgVersion = ver
	im.Kind = kind.String()
	if libs, err := client.PreloadLibraries(ctx); err != nil {
		im.SharedPreloadLibraries = nil
	} else {
		im.SharedPreloadLibraries = &libs
	}
	toolVer, _, err := client.ToolVersion(ctx)
	if err != nil {
		ir.Error = err.Error()
		return
	}
	ir.ToolVersion = toolVer
	if !kind.Full() {
		res.warn("%s: databases only (URL source): users and privileges are not backed up", in.Name)
	}

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
		// template1 is copied into every new database: keep it when someone put objects there.
		if kind.Full() {
			if used, err := templateInUse(ctx, client.WithDB("template1")); err != nil {
				res.warn("%s: could not check template1 for objects: %v", in.Name, firstLine(err.Error()))
			} else if used {
				dbs = append(dbs, "template1")
			}
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
	extNames := map[string]bool{}
	for _, db := range dbs {
		dr := &DatabaseResult{Name: db, Encrypted: recipient != "", TS: ts}
		ir.Databases = append(ir.Databases, dr)
		start := d.Now()
		fail := func(format string, args ...interface{}) {
			dr.Error = fmt.Sprintf(format, args...)
			ir.OK = false
			d.Log.Error("dump failed", "instance", in.Name, "db", db, "err", dr.Error)
		}
		// Encoding/locale so restore can recreate the database identically (spec backup § Flow 3c);
		// if it cannot be read, the dump cannot be recreated identically → this database fails.
		loc, err := client.DatabaseLocale(ctx, db, pg.MajorOf(ver))
		if err != nil {
			fail("read encoding/locale of %s: %v", db, err)
			continue
		}
		c := client.WithDB(db)
		exts, err := c.Extensions(ctx)
		if err != nil {
			fail("read extensions of %s: %v", db, err)
			continue
		}
		for _, e := range exts {
			extNames[e.Name] = true
		}
		oos, err := c.OutOfScope(ctx)
		if err != nil {
			fail("read %s for what a restore cannot carry: %v", db, err)
			continue
		}
		if len(oos) > 0 {
			res.warn("%s/%s: restore cannot guarantee: %s", in.Name, db, strings.Join(oos, ", "))
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
			fail("%s", err.Error())
			continue
		}
		dr.Tables = len(tables)
		dr.OK, dr.Bytes = true, n
		added[key] = true
		im.database(db).addDump(&Dump{TS: ts, Key: key, ToolVersion: toolVer, Locale: loc, Extensions: exts, OutOfScope: oos, Bytes: n, SHA256: sha, Encrypted: recipient != "", Recipient: recipientPtr(recipient), UploadedAt: d.Now().UTC(), Tables: tables, SystemID: ir.SystemID})
		d.Log.Info("dumped", "instance", in.Name, "db", db, "bytes", n, "seconds", fmt.Sprintf("%.1f", dr.Seconds))
	}

	// The environment of this run (ADR-0015 § 2).
	er := &EnvResult{}
	ir.Env = er
	env, err := collectEnv(ctx, in, client, kind, dbs, extNames, pg.MajorOf(ver), res, d)
	if err == nil {
		env.TS = ts
		var data []byte
		data, err = json.MarshalIndent(env, "", "  ")
		if err == nil {
			key := storage.Join(cfg.Storage.Prefix, cfg.App, in.Name, "env", ts+ext(".json", recipient))
			var n int64
			var sha string
			n, sha, err = putBytes(ctx, d.Store, key, data, recipient)
			if err == nil {
				er.OK, er.Bytes = true, n
				added[key] = true
				im.Env.Dumps = append(im.Env.Dumps, &Dump{TS: ts, Key: key, SystemID: ir.SystemID, Bytes: n, SHA256: sha, Encrypted: recipient != "", Recipient: recipientPtr(recipient), UploadedAt: d.Now().UTC()})
				sortDumps(im.Env.Dumps)
				im.Env.Latest = ts
			}
		}
	}
	if err != nil {
		er.Error = err.Error()
		ir.OK = false
	}

	if !kind.Full() {
		return
	}
	// Globals: once per instance, full sources only. → ADR-0006, ADR-0015
	gr := &GlobalsResult{HasPasswords: priv.Superuser || priv.ReadAllData}
	ir.Globals = gr
	args := []string{"--globals-only"}
	if !gr.HasPasswords {
		args = append(args, "--no-role-passwords")
	}
	args = append(args, lockWaitArg(cfg))
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
	im.Globals.Dumps = append(im.Globals.Dumps, &Dump{TS: ts, Key: key, ToolVersion: toolVer, SystemID: ir.SystemID, Bytes: n, SHA256: sha, Encrypted: recipient != "", Recipient: recipientPtr(recipient), UploadedAt: d.Now().UTC()})
	sortDumps(im.Globals.Dumps)
	im.Globals.Latest = ts
	im.Globals.HasPasswords = gr.HasPasswords
}

// templateInUse: template1 holds objects of a user or an extension besides plpgsql.
func templateInUse(ctx context.Context, c *pg.Client) (bool, error) {
	n, err := c.UserObjects(ctx)
	if err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	exts, err := c.Extensions(ctx)
	if err != nil {
		return false, err
	}
	return len(exts) > 0, nil
}

// collectEnv reads the environment file of this run. A required part that
// cannot be read fails the instance; optional parts (packages) only warn.
func collectEnv(ctx context.Context, in config.Instance, client *pg.Client, kind config.Kind, dbs []string, exts map[string]bool, major int, res *Result, d Deps) (*pg.Env, error) {
	src := pg.EnvSource{Kind: kind.String(), Shell: d.Shell}
	switch kind {
	case config.KindContainer:
		src.Container = in.Container
	case config.KindLocal:
		src.Local = &pg.Local{Dir: in.Local.Dir(), Port: in.Local.Port}
	}
	env, warns, err := pg.CollectEnv(ctx, client, src, dbs)
	if err != nil {
		return nil, fmt.Errorf("read the environment: %w", err)
	}
	env.Modules = pg.NonDefaultModules(env.Modules)
	if kind.Full() && !env.HasFileSettings {
		return nil, fmt.Errorf("read the environment: pg_file_settings is not readable; a full backup needs a superuser")
	}
	for _, w := range warns {
		res.warn("%s: %s", in.Name, w)
	}
	if kind == config.KindLocal && len(exts) > 0 {
		names := make([]string, 0, len(exts))
		for n := range exts {
			names = append(names, n)
		}
		sort.Strings(names)
		pkgs, err := pg.ExtensionPackages(ctx, d.Shell, major, names)
		env.Local.Packages = pkgs
		if err != nil {
			res.warn("%s: could not find the package of every extension (%v); drill builds its image from them", in.Name, firstLine(err.Error()))
		}
	}
	return env, nil
}

// applyRetention drops from the manifest the dumps beyond `keep` per database
// (in uploaded_at order) and returns the keys to delete. It never deletes dumps
// created this run (added) even with clock skew, and only deletes keys under <prefix>/<app>/.
func applyRetention(cfg *config.App, m *Manifest, added, held map[string]bool, res *Result) []string {
	defKeep := 7
	if cfg.Retention != nil {
		defKeep = cfg.Retention.Keep
	}
	root := storage.Join(cfg.Storage.Prefix, cfg.App) + "/"
	var toDelete []string
	for _, in := range cfg.Instances {
		im := m.Instances[in.Name]
		if im == nil || held[in.Name] {
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
		// Globals and environment files belong to runs: keep each one while a
		// database dump of its run is kept (ADR-0015), otherwise by keep.
		used := map[string]bool{}
		for _, dm := range im.Databases {
			for _, d := range dm.Dumps {
				used[d.TS] = true
			}
		}
		var del []string
		im.Globals.Dumps, del = pruneRuns(im.Globals.Dumps, defKeep, used, added, root, res)
		toDelete = append(toDelete, del...)
		im.Env.Dumps, del = pruneRuns(im.Env.Dumps, defKeep, used, added, root, res)
		toDelete = append(toDelete, del...)
	}
	return toDelete
}

// pruneRuns is prune for per-run objects: one whose run still has a database
// dump is never deleted; the others go once more than keep remain.
func pruneRuns(dumps []*Dump, keep int, used, added map[string]bool, root string, res *Result) ([]*Dump, []string) {
	var held, rest []*Dump
	for _, d := range dumps {
		if used[d.TS] {
			held = append(held, d)
		} else {
			rest = append(rest, d)
		}
	}
	k := keep - len(held)
	if k < 0 {
		k = 0
	}
	rest, del := prune(rest, k, added, root, res)
	out := append(held, rest...)
	sortDumps(out)
	return out, del
}

// prune drops the oldest dumps until `keep` remain; a dump created this run is kept
// and stops pruning (no skipping past it to delete others); a key outside root is kept and the next one is examined.
func prune(dumps []*Dump, keep int, added map[string]bool, root string, res *Result) ([]*Dump, []string) {
	sortDumps(dumps)
	var del []string
	var kept []*Dump
	for len(dumps) > 0 && len(dumps)+len(kept) > keep {
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

// appYAMLKey is where the copy of app.yaml lives (without .age).
func appYAMLKey(cfg *config.App) string {
	return storage.Join(cfg.Storage.Prefix, cfg.App, "app.yaml")
}

// AppYAMLKey is the key of the app.yaml copy for an app, encrypted or not.
func AppYAMLKey(prefix, app string, encrypted bool) string {
	k := storage.Join(prefix, app, "app.yaml")
	if encrypted {
		k += ".age"
	}
	return k
}

// saveAppYAML writes the app.yaml in use (it holds no secret) next to the
// manifest, so restore works on a machine without it (ADR-0015). A failure
// only warns. The other form (plain or .age) is removed so one copy remains.
func saveAppYAML(ctx context.Context, cfg *config.App, recipient string, res *Result, d Deps) {
	data, err := config.Marshal(cfg)
	if err == nil {
		_, _, err = putBytes(ctx, d.Store, AppYAMLKey(cfg.Storage.Prefix, cfg.App, recipient != ""), data, recipient)
	}
	if err != nil {
		res.warn("could not copy app.yaml to the bucket: %v; restore on another machine will need app.yaml", err)
		return
	}
	other := AppYAMLKey(cfg.Storage.Prefix, cfg.App, recipient == "")
	if objs, err := d.Store.List(ctx, other); err == nil {
		for _, o := range objs {
			if o.Key == other {
				_ = d.Store.Delete(ctx, other)
			}
		}
	}
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
		if o.Key == manifestKey || strings.HasPrefix(o.Key, manifestKey+".corrupt-") || keys[o.Key] || o.Key == appYAMLKey(cfg) || o.Key == appYAMLKey(cfg)+".age" {
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

// orUser names the backup user in a hint, or a placeholder when app.yaml leaves it to the default.
func orUser(u string) string {
	if u == "" {
		return "<backup user>"
	}
	return pg.QuoteIdent(u)
}
