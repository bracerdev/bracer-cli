package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"bracer.dev/bracer/internal/config"
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
	// Location is the time zone the calendar retention counts days, weeks
	// and months in: the machine's (ADR-0028). nil: time.Local.
	Location *time.Location
	// Connect can be replaced in tests; defaults to the package's Connect.
	Connect ConnectFunc
	// Here is this place; the folder's writer must be it (ADR-0023).
	Here Writer
	// Create is init's first real run: a folder with no run is started, owned by Here.
	Create bool
	// SchemaOnly is the run preflight makes (ADR-0019): pg_dump --schema-only,
	// no row counts, no encryption, into a Store held in memory; no run.json,
	// no retention. A database with external not accepted in app.yaml
	// fails.
	SchemaOnly bool
	// Drill test-restores the run once every object of it is in the bucket,
	// before run.json is written (ADR-0026): it records the outcome of each
	// instance in run.Instances[name].Drill and may put its own result in
	// res.Instances[i].Drill for --json and /report. nil: no instance is
	// test-restored (drill off).
	Drill DrillFunc
}

// DrillFunc is the drill step of a run; see Deps.Drill.
type DrillFunc func(ctx context.Context, run *Run, res *Result)

// Result is the outcome of one run, printed to the console and --json, sent to /report.
type Result struct {
	Kind       string    `json:"kind"`
	App        string    `json:"app"`
	TS         string    `json:"ts"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	OK         bool      `json:"ok"`
	// Status is ok or failed, as exit 0 and 1 (ADR-0022, ADR-0029): the
	// worse of the backup and the drill.
	Status    string            `json:"status"`
	Instances []*InstanceResult `json:"instances"`
	// Error is what failed the run as a whole although every instance was
	// dumped: the age key that cannot open the run (ADR-0019), app.yaml or
	// run.json that could not be written.
	Error string `json:"error,omitempty"`
	// Warnings is what the run found worth a look without failing it
	// (ADR-0029): each is printed, written to run.json and sent with the
	// heartbeat.
	Warnings []string `json:"warnings,omitempty"`
}

// InstanceResult is the outcome of one instance.
type InstanceResult struct {
	Name      string `json:"name"`
	Runner    string `json:"runner,omitempty"`
	PgVersion string `json:"pg_version,omitempty"`
	SystemID  string `json:"system_id,omitempty"`
	// IdentityChanged: the server is another cluster than the one init recorded;
	// dumps are taken but retention deletes nothing (spec backup § Flow 3a2).
	IdentityChanged bool              `json:"identity_changed,omitempty"`
	ToolVersion     string            `json:"tool_version,omitempty"` // the pg_dump --version used
	OK              bool              `json:"ok"`
	Error           string            `json:"error,omitempty"`
	Databases       []*DatabaseResult `json:"databases"`
	// Missing names what this run lacks of the instance: a database, the
	// environment file, globals (full source), app.yaml (ADR-0016).
	Missing []string       `json:"missing"`
	Globals *GlobalsResult `json:"globals,omitempty"`
	Env     *EnvResult     `json:"env,omitempty"`
	Kind    string         `json:"source_kind,omitempty"` // container, local, url
	// Drill is the drill's own result for this instance (the drill package's),
	// for --json and /report; run.json keeps only its outcome.
	Drill any `json:"drill,omitempty"`
	// DrillFailed is why the drill of this instance failed; "" when it passed
	// or is off. It fails the run (exit 1).
	DrillFailed []string `json:"-"`
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

// Instance returns the result of the instance name, nil when there is none.
func (r *Result) Instance(name string) *InstanceResult {
	for _, ir := range r.Instances {
		if ir.Name == name {
			return ir
		}
	}
	return nil
}

// finish sets OK and Status from the errors of the run.
func (r *Result) finish() {
	r.OK = len(r.Failures()) == 0
	r.Status = ResultOK
	if !r.OK {
		r.Status = ResultFailed
	}
}

// Failures lists every error of the run: the backup's, then the drill's.
func (r *Result) Failures() []string {
	out := r.BackupFailures()
	for _, ir := range r.Instances {
		for _, reason := range ir.DrillFailed {
			out = append(out, ir.Name+": drill: "+reason)
		}
	}
	return out
}

// BackupFailures lists every error of the backup part, one per instance,
// database, environment file or globals that failed, as printed.
func (r *Result) BackupFailures() []string {
	var out []string
	for _, ir := range r.Instances {
		if ir.Error != "" {
			out = append(out, ir.Name+": "+ir.Error)
		}
		for _, dr := range ir.Databases {
			if !dr.OK {
				out = append(out, ir.Name+"/"+dr.Name+": "+dr.Error)
			}
		}
		if ir.Env != nil && !ir.Env.OK {
			out = append(out, ir.Name+": environment: "+ir.Env.Error)
		}
		if ir.Globals != nil && !ir.Globals.OK {
			out = append(out, ir.Name+"/globals: "+ir.Globals.Error)
		}
	}
	if r.Error != "" {
		out = append(out, r.Error)
	}
	return out
}

// FixReinit is the one way to change an app (ADR-0030): the Fix of a
// preflight failure, the advice of a warning.
func FixReinit(app string) string { return "bracer remove " + app + ", then bracer init " + app }

const tsLayout = "2006-01-02T150405Z"

// clockTolerance is how far behind the newest folder this machine's clock
// may be for the run to go on with <newest> + 1 s (ADR-0028).
const clockTolerance = time.Hour

// Backup backs up the whole app.yaml into one run folder, test-restores it and
// ends it with run.json (spec backup § Flow steps 2–7). Per-database/instance
// errors live in Result; an error is returned only when the run cannot
// start: the folder has no run, is unreadable or another place's
// (ADR-0023), and then nothing was written.
func Backup(ctx context.Context, cfg *config.App, d Deps) (*Result, error) {
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
	if d.Location == nil {
		d.Location = time.Local
	}
	res := &Result{Kind: "backup", App: cfg.App, StartedAt: d.Now().UTC()}
	var prev *Run
	var refs []RunRef
	if !d.SchemaOnly {
		var err error
		if prev, refs, err = OpenFolder(ctx, d.Store, cfg, d.Here, d.Create); err != nil {
			return nil, err
		}
	}
	recipient := ""
	skipRetention := d.SchemaOnly
	if cfg.Encryption == "age" && !d.SchemaOnly {
		recipient = cfg.AgeRecipient
		if msg := keyMismatch(cfg); msg != "" {
			// Still dump: a backup nobody here can open beats none. But delete
			// nothing of the app until the key matches again (ADR-0019).
			res.Error = fmt.Sprintf("%s: it cannot open what this run encrypts; nothing is deleted until it can\nFix: put the private key of %s back into %s (it is on the rescue card init printed)", msg, cfg.AgeRecipient, cfg.AgeIdentityFile)
			skipRetention = true
		}
	}

	// One ts for the whole run: every instance, database, globals, environment
	// file and the run's app.yaml (ADR-0016). The new folder must sort last:
	// a clock a little behind the newest folder (NTP, the same second) is
	// bumped past it; one far behind stops the run, since following a
	// future-dated folder would put every later run on the same day
	// (ADR-0028).
	now := d.Now().UTC()
	ts := now.Format(tsLayout)
	if len(refs) > 0 && ts <= refs[0].TS {
		if t, err := time.Parse(tsLayout, refs[0].TS); err == nil {
			if ahead := t.Sub(now); ahead >= clockTolerance {
				return nil, fmt.Errorf("the newest run %s is %s ahead of this machine's clock; fix the clock (or delete that folder in the bucket if the clock was wrong when it ran)", refs[0].TS, aheadText(ahead))
			}
			ts = t.Add(time.Second).Format(tsLayout)
			res.warn("clock is behind the last run %s; this run is %s", refs[0].TS, ts)
		}
	}
	res.TS = ts
	run := newRun(cfg.App, Root(cfg), ts, d.Here)
	for _, in := range cfg.Instances {
		ir := &InstanceResult{Name: in.Name, Missing: []string{}}
		res.Instances = append(res.Instances, ir)
		runInstance(ctx, cfg, in, run, prev, ir, recipient, res, d)
	}
	if err := putRunAppYAML(ctx, cfg, run, recipient, d); err != nil {
		res.Error = fmt.Sprintf("could not write app.yaml of this run to the bucket: %v; restore and drill need it to know what the run meant to back up", err)
	}
	markMissing(cfg.Instances, run, res)
	if d.Drill != nil {
		d.Drill(ctx, run, res)
	}
	for _, ir := range res.Instances {
		if ri := run.Instances[ir.Name]; ri != nil && ri.Drill == nil {
			ri.Drill = &DrillOutcome{Result: DrillOff}
		}
	}
	if d.SchemaOnly {
		res.FinishedAt = d.Now().UTC()
		res.finish()
		return res, nil
	}
	checkRowDrops(cfg.Instances, run, newRunCache(ctx, d.Store, run, prev, refs), res)
	// The outcome is known before run.json is written, so it goes with it;
	// run.json is the last object of the run (ADR-0026).
	res.FinishedAt = d.Now().UTC()
	run.Backup = NewOutcome(res.StartedAt, res.FinishedAt, res.BackupFailures(), res.Warnings, FixReinit(cfg.App))
	res.finish()
	if err := SaveRun(ctx, d.Store, run); err != nil {
		if res.Error != "" {
			res.Error += "\n"
		}
		res.Error += fmt.Sprintf("run.json could not be written: %v; this run's objects are in the bucket but no restore can find them, and the next backup deletes them", err)
		res.finish()
		return res, nil
	}
	// Only a run that passed deletes anything (ADR-0029): the newest passed
	// run was kept when it was made and no failed run after it deletes.
	if skipRetention || run.Verdict() != ResultOK {
		return res, nil
	}
	applyRetention(ctx, *cfg.Schedule, run, refs, res, d)
	return res, nil
}

// checkIdentity is spec backup § Flow 3a2: compare the server with what init
// recorded. A different cluster or major is still dumped and only warned
// about (ADR-0029). Never stops a backup because something changed.
func checkIdentity(ctx context.Context, client *pg.Client, app string, in config.Instance, ver string, ir *InstanceResult, res *Result) {
	if major := pg.MajorOf(ver); major != 0 && major != in.PgMajor {
		res.warn("Postgres on %s is now %d (app.yaml says %d); run %s to record it", in.Name, major, in.PgMajor, FixReinit(app))
	}
	if in.SystemID == config.SystemIDNone {
		return // managed: pg_control_system() is not allowed (init recorded none)
	}
	id, err := client.SystemIdentifier(ctx)
	if err != nil {
		res.warn("%s: could not read system_identifier (%v); not checking whether it is still the same Postgres cluster", in.Name, firstLine(err.Error()))
		return
	}
	ir.SystemID = id
	if id == in.SystemID {
		return
	}
	ir.IdentityChanged = true
	when := "at an unknown time"
	if t := pg.InitdbTime(id); !t.IsZero() {
		when = t.Format("2006-01-02 15:04 UTC")
	}
	res.warn("%s is a different Postgres cluster than before (initialized %s; system_identifier %s, app.yaml says %s); run %s to confirm it", in.Name, when, id, in.SystemID, FixReinit(app))
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// markMissing records on each instance, in the result and in run.json, what
// the run lacks of what app.yaml asked for (ADR-0016).
func markMissing(insts []config.Instance, run *Run, res *Result) {
	for i, in := range insts {
		ir := res.Instances[i]
		ri := run.instance(in.Name)
		missing := []string{}
		dumped := map[string]bool{}
		for _, dr := range ir.Databases {
			if dr.OK {
				dumped[dr.Name] = true
			}
		}
		for _, name := range in.Databases.Names() {
			if !dumped[name] {
				missing = append(missing, name)
			}
		}
		if ri.Env == nil {
			missing = append(missing, "env")
		}
		if in.Kind().Full() && ri.Globals == nil {
			missing = append(missing, "globals")
		}
		if run.AppYAML == nil {
			missing = append(missing, "app.yaml")
		}
		ir.Missing, ri.Missing = missing, missing
		if len(missing) > 0 {
			res.warn("%s: this run is missing %s; restore needs an older run until the next backup succeeds", in.Name, strings.Join(missing, ", "))
		}
	}
}

// CheckSuperuser: a container or local source is backed up only as a
// superuser (ADR-0017); nil for a URL source.
func CheckSuperuser(ctx context.Context, client *pg.Client, in config.Instance) error {
	if !in.Kind().Full() {
		return nil
	}
	priv, err := client.Privileges(ctx)
	if err != nil {
		return fmt.Errorf("check user: %w", err)
	}
	return superuserErr(in, priv)
}

func superuserErr(in config.Instance, priv pg.Privileges) error {
	if in.Kind().Full() && !priv.Superuser {
		return fmt.Errorf("%s is not a superuser: a full backup (users, settings) needs one", priv.Role)
	}
	return nil
}

// runInstance backs up one instance into the run.
func runInstance(ctx context.Context, cfg *config.App, in config.Instance, run, prev *Run, ir *InstanceResult, recipient string, res *Result, d Deps) {
	kind := in.Kind()
	ri := run.instance(in.Name)
	ri.Kind = kind.String()
	ri.Encrypted, ri.Recipient = recipient != "", recipientPtr(recipient)
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
	// A full source is backed up only as its superuser, checked before
	// anything is dumped, so a run that has an environment file is whole (ADR-0017).
	priv, err := client.Privileges(ctx)
	if err != nil {
		ir.Error = fmt.Sprintf("check user: %v", err)
		return
	}
	if err := superuserErr(in, priv); err != nil {
		ir.Error = fmt.Sprintf("%v; nothing was backed up\n  Fix: %s", err, FixReinit(cfg.App))
		return
	}
	ver, err := client.ServerVersion(ctx)
	if err != nil {
		ir.Error = err.Error()
		return
	}
	ir.PgVersion, ri.PgVersion = ver, ver
	checkIdentity(ctx, client, cfg.App, in, ver, ir, res)
	ri.SystemID, ri.IdentityChanged = ir.SystemID, ir.IdentityChanged
	if libs, err := client.PreloadLibraries(ctx); err != nil {
		ri.SharedPreloadLibraries = nil
	} else {
		ri.SharedPreloadLibraries = &libs
	}
	toolVer, _, err := client.ToolVersion(ctx)
	if err != nil {
		ir.Error = err.Error()
		return
	}
	ir.ToolVersion, ri.ToolVersion = toolVer, toolVer
	if !kind.Full() {
		res.warn("%s: databases only (URL source): users and privileges are not backed up", in.Name)
	}
	var prevRI *RunInstance
	if prev != nil {
		prevRI = prev.Instances[in.Name]
	}

	// Exactly the databases the customer chose; the server's others are
	// not bracer's business (ADR-0016, ADR-0024).
	dbs := in.Databases.Names()

	ir.OK = true
	extNames := map[string]bool{}
	for _, db := range dbs {
		dr := &DatabaseResult{Name: db, Encrypted: recipient != ""}
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
		ext, err := c.External(ctx)
		if err != nil {
			fail("read %s for what depends on something outside Postgres: %v", db, err)
			continue
		}
		if left := diff(ext, accepted(in, db)); len(left) > 0 {
			msg := fmt.Sprintf("depends on something outside Postgres that a backup cannot carry: %s", strings.Join(left, ", "))
			if d.SchemaOnly {
				fail("%s (not accepted in app.yaml)\n  Fix: %s", msg, FixReinit(cfg.App))
				continue
			}
			res.warn("%s/%s %s; run %s to accept it", in.Name, db, msg, FixReinit(cfg.App))
		}
		needs, err := c.RestoreNeeds(ctx, pg.MajorOf(ver))
		if err != nil {
			fail("read what a restore of %s needs on the target server: %v", db, err)
			continue
		}
		if needs.Empty() {
			needs = nil
		}
		if in.DrillOff() && prevRI != nil {
			if last := prevRI.Databases[db]; last != nil {
				if added := diff(needNames(needs), needNames(last.RestoreNeeds)); len(added) > 0 {
					res.warn("%s/%s: a restore now needs %s; this instance has no test-restore to check it", in.Name, db, strings.Join(added, ", "))
				}
			}
		}
		locks, err := c.LockObjects(ctx)
		if err != nil {
			fail("count the objects of %s a restore locks: %v", db, err)
			continue
		}
		// One deadline per database: count + dump + upload (spec backup § Edge cases).
		key := dumpKey(run.root, run.TS, in.Name, db, recipient != "")
		dbctx, cancel := context.WithTimeout(ctx, config.BackupTimeout)
		n, sha, tables, err := dumpDatabase(dbctx, in, runner, c.Conn, db, key, recipient, d)
		cancel()
		dr.Seconds = d.Now().Sub(start).Seconds()
		if err != nil {
			if errors.Is(dbctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				err = fmt.Errorf("timed out after %s; the connection was closed, PostgreSQL 14+ stops the server-side work within 10 s: %w", config.BackupTimeout, err)
			}
			fail("%s", err.Error())
			continue
		}
		dr.Tables = len(tables)
		dr.OK, dr.Bytes = true, n
		ri.Databases[db] = &Dump{Key: key, TS: run.TS, ToolVersion: toolVer, Locale: loc, Extensions: exts, External: ext, RestoreNeeds: needs, LockObjects: locks, Bytes: n, SHA256: sha, Encrypted: recipient != "", Recipient: recipientPtr(recipient), Tables: tables}
		d.Log.Info("dumped", "instance", in.Name, "db", db, "bytes", n, "seconds", fmt.Sprintf("%.1f", dr.Seconds))
	}

	// The environment of this run (ADR-0015 § 2).
	er := &EnvResult{}
	ir.Env = er
	env, err := collectEnv(ctx, in, client, kind, dbs, extNames, pg.MajorOf(ver), res, d)
	if err == nil {
		env.TS = run.TS
		var data []byte
		data, err = json.MarshalIndent(env, "", "  ")
		if err == nil {
			key := envKey(run.root, run.TS, in.Name, recipient != "")
			var n int64
			var sha string
			n, sha, err = putBytes(ctx, d.Store, key, data, recipient)
			if err == nil {
				er.OK, er.Bytes = true, n
				ri.Env = &Dump{Key: key, TS: run.TS, Bytes: n, SHA256: sha, Encrypted: recipient != "", Recipient: recipientPtr(recipient)}
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
	args = append(args, lockWaitArg())
	key := globalsKey(run.root, run.TS, in.Name, recipient != "")
	gctx, cancel := context.WithTimeout(ctx, config.BackupTimeout)
	n, sha, err := streamToStore(gctx, runner, conn, "pg_dumpall", args, d.Store, key, recipient)
	cancel()
	if err != nil {
		gr.Error = err.Error()
		ir.OK = false
		return
	}
	gr.OK, gr.Bytes = true, n
	ri.Globals = &Dump{Key: key, TS: run.TS, ToolVersion: toolVer, Bytes: n, SHA256: sha, Encrypted: recipient != "", Recipient: recipientPtr(recipient), HasPasswords: gr.HasPasswords}
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

// applyRetention deletes every run folder outside the retention window of
// app.yaml (ADR-0029): the runs that have run.json and at least one dump
// are grouped by day, ISO week or month of schedule.every in the machine's
// time zone (ADR-0028), and every run in the schedule.keep newest periods
// that have one stays. Every other folder under bracer/<folder>/ goes:
// older runs, runs without a dump, folders without run.json (runs that did
// not end). Only a run that passed calls it, so the newest passed run is
// never deleted. Only keys under bracer/<folder>/<ts>/ are ever deleted;
// every run removed is one log line (ADR-0027).
func applyRetention(ctx context.Context, sc config.Schedule, run *Run, refs []RunRef, res *Result, d Deps) {
	tss := []string{run.TS}
	for _, ref := range refs {
		if ref.TS != run.TS && ref.Done && ref.Dumps > 0 {
			tss = append(tss, ref.TS)
		}
	}
	keep := keepSet(tss, sc.Every, sc.Keep, d.Location)
	for _, ref := range refs {
		if ref.TS == run.TS || keep[ref.TS] {
			continue
		}
		// run.json first: a folder without it is a run that did not end, and
		// the next run deletes what a failed delete leaves.
		keys := append([]string(nil), ref.Keys...)
		sort.Slice(keys, func(i, j int) bool { return keys[i] == RunKey(run.root, ref.TS) })
		removed := true
		for _, key := range keys {
			if err := d.Store.Delete(ctx, key); err != nil {
				res.warn("retention: could not delete %s: %v; the next run tries again", key, err)
				removed = false
				break
			}
		}
		if removed {
			d.Log.Info("retention: removed run " + ref.TS)
		}
	}
}

// keepSet is the ts the retention window keeps among tss: every ts of the
// keep newest periods (day, ISO week or month of every) that have one, in
// the time zone loc (ADR-0029). A ts that does not parse is kept.
func keepSet(tss []string, every string, keep int, loc *time.Location) map[string]bool {
	sorted := append([]string(nil), tss...)
	sort.Sort(sort.Reverse(sort.StringSlice(sorted)))
	period := func(t time.Time) string { return t.Format("2006-01-02") }
	switch every {
	case config.EveryWeekly:
		period = func(t time.Time) string { y, w := t.ISOWeek(); return fmt.Sprintf("%d-W%02d", y, w) }
	case config.EveryMonthly:
		period = func(t time.Time) string { return t.Format("2006-01") }
	}
	out := map[string]bool{}
	seen := map[string]bool{}
	for _, ts := range sorted {
		t, err := time.Parse(tsLayout, ts)
		if err != nil {
			out[ts] = true
			continue
		}
		k := period(t.In(loc))
		if !seen[k] {
			if len(seen) >= keep {
				break
			}
			seen[k] = true
		}
		out[ts] = true
	}
	return out
}

// putRunAppYAML writes the app.yaml in use (it holds no secret) as the
// app.yaml of this run: what the run meant to back up (ADR-0016).
func putRunAppYAML(ctx context.Context, cfg *config.App, run *Run, recipient string, d Deps) error {
	data, err := config.Marshal(cfg)
	if err != nil {
		return err
	}
	key := appYAMLKey(run.root, run.TS, recipient != "")
	n, sha, err := putBytes(ctx, d.Store, key, data, recipient)
	if err != nil {
		return err
	}
	run.AppYAML = &Dump{Key: key, TS: run.TS, Bytes: n, SHA256: sha, Encrypted: recipient != "", Recipient: recipientPtr(recipient)}
	return nil
}

// lockWaitArg is the --lock-wait-timeout for pg_dump/pg_dumpall.
func lockWaitArg() string {
	return fmt.Sprintf("--lock-wait-timeout=%d", config.LockTimeout.Milliseconds())
}

// dumpDatabase opens a snapshot, sets lock_timeout, counts rows, then pg_dump --snapshot
// to the bucket; everything within ctx (which carries the per-database deadline).
func dumpDatabase(ctx context.Context, in config.Instance, runner pg.Runner, conn pg.Conn, db, key, recipient string, d Deps) (n int64, sha string, tables []pg.Table, err error) {
	// Count rows and dump within the same snapshot (spec backup, § Row counts and snapshot);
	// lock_timeout so we do not queue behind a waiting ALTER TABLE.
	if d.SchemaOnly {
		n, sha, err = streamToStore(ctx, runner, conn, "pg_dump", []string{"-Fc", "--schema-only", lockWaitArg()}, d.Store, key, "")
		return n, sha, nil, err
	}
	snap, err := pg.BeginSnapshot(ctx, runner, conn, config.LockTimeout)
	if err != nil {
		return 0, "", nil, fmt.Errorf("open snapshot: %w", err)
	}
	tables, err = snap.Tables()
	if err == nil {
		err = countRows(snap, tables)
	}
	if err != nil {
		snap.Close()
		return 0, "", nil, fmt.Errorf("read tables: %w", err)
	}
	args := append([]string{"-Fc", lockWaitArg()}, snap.Args()...)
	n, sha, err = streamToStore(ctx, runner, conn, "pg_dump", args, d.Store, key, recipient)
	if endErr := snap.End(); err == nil && endErr != nil {
		d.Log.Warn("close snapshot session", "instance", in.Name, "db", db, "err", endErr)
	}
	if err != nil {
		return 0, "", nil, err
	}
	return n, sha, tables, nil
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

// countRows fills Table.Rows of every table whose reltuples is below
// config.ExactMaxRows or never ANALYZEd (-1); the others keep RowsEst. A
// counted table drops RowsEst so run.json records only one of the two.
func countRows(snap *pg.Snapshot, tables []pg.Table) error {
	for i := range tables {
		t := &tables[i]
		if !shouldCount(*t) {
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

func shouldCount(t pg.Table) bool {
	if t.Kind == "p" {
		return false // partitioned parent: count(*) would rescan every child partition already counted
	}
	return t.RowsEst == nil || *t.RowsEst < config.ExactMaxRows
}

// orUser names the backup user in a hint, or a placeholder when app.yaml leaves it to the default.
func orUser(u string) string {
	if u == "" {
		return "<backup user>"
	}
	return pg.QuoteIdent(u)
}

// aheadText is a clock difference in hours, or days from two days on.
func aheadText(d time.Duration) string {
	if d >= 48*time.Hour {
		return fmt.Sprintf("%.0f days", d.Hours()/24)
	}
	return fmt.Sprintf("%.0f hours", d.Hours())
}

// runCache loads the run.json of earlier runs of the folder once, for the
// row-drop check and retention, newest first.
type runCache struct {
	ctx    context.Context
	st     storage.Store
	root   string
	refs   []RunRef // every folder, newest first
	loaded map[string]*Run
	errs   map[string]error
}

func newRunCache(ctx context.Context, st storage.Store, run, prev *Run, refs []RunRef) *runCache {
	c := &runCache{ctx: ctx, st: st, root: run.root, refs: refs, loaded: map[string]*Run{run.TS: run}, errs: map[string]error{}}
	if prev != nil {
		c.loaded[prev.TS] = prev
	}
	return c
}

// get is the run ts, loaded once; an error (missing, unreadable) is
// remembered too, so a bad run.json is reported once.
func (c *runCache) get(ts string) (*Run, error) {
	if r, ok := c.loaded[ts]; ok {
		return r, c.errs[ts]
	}
	r, err := LoadRun(c.ctx, c.st, c.root, ts)
	if err != nil {
		r = nil
		c.errs[ts] = err
	}
	c.loaded[ts] = r
	return r, err
}

// lastDump is the newest run that ended before this one with a dump of the
// database, and that dump; nil when none has it. Folders without a dump
// object are skipped without a load.
func (c *runCache) lastDump(instance, db string) (*Run, *Dump) {
	for _, ref := range c.refs {
		if !ref.Done || ref.Dumps == 0 {
			continue
		}
		r, _ := c.get(ref.TS)
		if r == nil {
			continue
		}
		if ri := r.Instances[instance]; ri != nil {
			if d := ri.Databases[db]; d != nil {
				return r, d
			}
		}
	}
	return nil, nil
}
