package restore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/crypto"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/storage"
)

// Options holds the flags of `bracer restore` (spec fetch-restore, ADR-0015).
type Options struct {
	Instance      string   // --instance: source instance in the manifest
	Databases     []string // --database (repeatable); empty: every database of the run
	TS            string   // --ts: the run; default the latest environment file
	File          string   // --file: a fetched file, preview only
	IntoContainer string   // --into-container <name>
	IntoLocal     int      // --into-local <port>
	SocketDir     string   // --socket-dir, with --into-local
	IntoURL       bool     // --into-url: ask for the URL
	IntoURLEnv    string   // --into-url-env NAME
	PasswordEnv   string   // --password-env NAME: the target superuser's password (container, local)
	Preview       bool     // --preview: data to look at only
	DryRun        bool     // --dry-run
	SQLDir        string   // --sql-dir; default ./bracer-restore-<app>-<instance>-<ts>
	AppURL        bool     // --app-url: ask for the app's URL
	AppURLEnv     string   // --app-url-env NAME
}

// Deps is everything Run reaches outside; tests swap in fakes.
type Deps struct {
	Shell          pg.Shell
	Store          storage.Store
	Secrets        *config.Secrets
	Tools          *pg.Tools // nil: built from Shell
	Now            func() time.Time
	Log            *slog.Logger
	ReadIdentities func(path string) ([]age.Identity, []string, error)
	// Connect defaults to backup.Connect; restore uses it for --into-container and --into-local.
	Connect backup.ConnectFunc
	// ContainerUser names the superuser inside a container (default: its POSTGRES_USER).
	ContainerUser func(ctx context.Context, container string) (string, error)
	// AskURL asks for the target URL without echo (--into-url); nil: no terminal.
	AskURL func() (string, error)
	// AskPassword asks for the password of a container or local target whose
	// socket asks for one; nil: no terminal.
	AskPassword func() (string, error)
	// AskAppURL asks for the app's URL without echo (--app-url); nil: no terminal.
	AskAppURL func() (string, error)
	// Timeout for pg_restore; 0 = DefaultDrillTimeout (shares the drill.timeout setting).
	Timeout time.Duration
	// Started is called once the checks passed, before the data is loaded.
	Started func(*Result)
}

// Result is the result of `restore` (`--json`, spec fetch-restore § Data model).
type Result struct {
	Kind      string      `json:"kind"`
	App       string      `json:"app"`
	Mode      string      `json:"mode"` // full | data | preview
	Source    Source      `json:"source"`
	Target    Target      `json:"target"`
	Checks    []Check     `json:"checks,omitempty"`
	Notes     []string    `json:"notes,omitempty"`
	SQLDir    string      `json:"sql_dir,omitempty"`
	EnvUp     string      `json:"env_up,omitempty"`
	EnvDown   string      `json:"env_down,omitempty"`
	DataDown  string      `json:"data_down,omitempty"`
	Apply     string      `json:"apply,omitempty"`   // how to apply a file: the command with <file>
	Restart   string      `json:"restart,omitempty"` // after env-up, when a setting needs it
	Verify    []Check     `json:"verify,omitempty"`  // the comparison after the load that failed
	Databases []*DBResult `json:"databases"`
	AppCheck  *AppCheck   `json:"app_check,omitempty"`
	Disk      *DiskCheck  `json:"disk,omitempty"`
	Preflight *Preflight  `json:"preflight,omitempty"` // --preview only
	OK        bool        `json:"ok"`
	DryRun    bool        `json:"dry_run,omitempty"`
	Error     string      `json:"error,omitempty"`
	Warnings  []string    `json:"warnings,omitempty"`
}

// Source is where the dumps come from.
type Source struct {
	Instance  string `json:"instance,omitempty"`
	TS        string `json:"ts,omitempty"`
	Kind      string `json:"kind,omitempty"` // container | local | url
	File      string `json:"file,omitempty"`
	Encrypted bool   `json:"encrypted"`
	PgVersion string `json:"pg_version,omitempty"`
}

// Target is the target server, given on the command line and never stored.
type Target struct {
	Container string `json:"container,omitempty"`
	Local     string `json:"local,omitempty"` // socket path
	URLEnv    string `json:"url_env,omitempty"`
	Host      string `json:"host,omitempty"` // host:port of a URL; no user or password
	Runner    string `json:"runner"`
	PgVersion string `json:"pg_version,omitempty"`
	User      string `json:"user,omitempty"`
	Superuser bool   `json:"superuser"`
}

// Preflight is the --preview check before anything is created.
type Preflight struct {
	OK             bool     `json:"ok"`
	NeedsSuperuser []string `json:"needs_superuser,omitempty"`
}

// DBResult is one restored database.
type DBResult struct {
	SourceDB       string        `json:"source_db"`
	TargetDB       string        `json:"target_db"`
	State          string        `json:"state,omitempty"` // absent | empty | has_data, of the source name on the target
	TS             string        `json:"ts,omitempty"`
	Key            string        `json:"key,omitempty"`
	Bytes          int64         `json:"bytes,omitempty"`
	Encrypted      bool          `json:"encrypted"`
	OK             bool          `json:"ok"`
	RestoreSeconds float64       `json:"restore_seconds"`
	TablesExpected int           `json:"tables_expected"`
	TablesFound    int           `json:"tables_found"`
	MissingTables  []string      `json:"missing_tables,omitempty"`
	RowMismatches  []RowMismatch `json:"row_mismatches,omitempty"`
	Checks         []CheckResult `json:"checks,omitempty"`
	Attributes     *AttrResult   `json:"attributes,omitempty"`
	Error          string        `json:"error,omitempty"`

	dump *backup.Dump
	info *ArchiveInfo
}

// ErrPreflight: the target needs changes the customer makes first (exit 2);
// Result.Checks says which, Result.EnvUp has the SQL.
var ErrPreflight = errors.New("the target is not ready for this restore")

func (r *Result) warn(format string, a ...interface{}) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, a...))
}

// Run performs the restore (spec fetch-restore § Flow). *UsageError → exit 2;
// ErrPreflight → exit 2 with the result; other errors → exit 1.
func Run(ctx context.Context, cfg *config.App, o Options, d Deps) (*Result, error) {
	d = withDefaults(d)
	n := 0
	for _, set := range []bool{o.IntoContainer != "", o.IntoLocal != 0, o.IntoURL, o.IntoURLEnv != ""} {
		if set {
			n++
		}
	}
	if n != 1 {
		return nil, usagef("pass exactly one target: --into-container <name>, --into-local <port>, --into-url, or --into-url-env <NAME>")
	}
	if o.Preview || o.File != "" {
		return runPreview(ctx, cfg, o, d)
	}
	res := &Result{Kind: "restore", App: cfg.App, DryRun: o.DryRun, Databases: []*DBResult{}}

	// 1. One run from the bucket.
	manifest, err := backup.LoadManifest(ctx, d.Store, storage.Join(cfg.Storage.Prefix, cfg.App, "manifest.json"), cfg.App)
	if err != nil {
		return nil, fmt.Errorf("load manifest: %w", err)
	}
	inst, err := ResolveInstance(manifest, cfg, o.Instance)
	if err != nil {
		return nil, err
	}
	im := manifest.Instances[inst]
	rp, err := PickRun(im, inst, o.TS, o.Databases)
	if err != nil {
		return nil, err
	}
	res.Source = Source{Instance: inst, TS: rp.TS, PgVersion: im.PgVersion, Encrypted: rp.Env.Encrypted}
	for _, dp := range rp.DBs {
		res.Source.Encrypted = res.Source.Encrypted || dp.Encrypted
	}
	var ids *Identities
	if res.Source.Encrypted {
		ids = identities(cfg, d)
	}

	// 2. Target, from the command line only (never stored).
	conn, runner, spec, err := connectTarget(ctx, o, d, res)
	if err != nil {
		return nil, err
	}
	admin := &pg.Client{Runner: runner, Conn: conn}
	if res.Target.PgVersion, err = admin.ServerVersion(ctx); err != nil {
		return nil, fmt.Errorf("connect to target: %w", err)
	}
	if runner, err = pickRestoreRunner(ctx, d, o, rp, runner, conn, res); err != nil {
		return nil, err
	}
	admin = &pg.Client{Runner: runner, Conn: conn}

	src, err := LoadSource(ctx, d.Store, rp, ids, runner, conn)
	if err != nil {
		return res, fmt.Errorf("%w; nothing was changed", err)
	}
	res.Source.Kind = src.Kind
	res.Mode = src.Mode()
	ap, err := admin.AdminPrivileges(ctx)
	if err != nil {
		return nil, fmt.Errorf("check target user: %w", err)
	}
	res.Target.User, res.Target.Superuser = ap.Role, ap.Superuser
	if res.Mode == ModeFull && !ap.Superuser {
		return res, usagef("a full backup restores only into a target where you connect as a superuser; %s on %s is not one.\n  To look at the data only, use --preview.", ap.Role, targetName(res.Target))
	}
	if res.Mode == ModeData {
		res.warn("this backup has databases only (source was a URL): restoring data owned by %s, without users or privileges", ap.Role)
	}
	if pooler, info := admin.DetectPooler(ctx); pooler {
		res.warn("target looks like a connection pooler (%s); pg_restore needs a direct connection to the server", info)
	}

	// 3. Compare.
	full := res.Mode == ModeFull
	tgt, err := ReadTarget(ctx, admin, spec, full, src.Names(), src.Locales())
	if err != nil {
		return res, fmt.Errorf("read target: %w", err)
	}
	plan := MakePlan(PlanInput{Src: src, Tgt: tgt, Mode: res.Mode, Now: d.Now()})
	res.Checks, res.Notes = plan.Checks, plan.Notes
	if spec.Password {
		res.Notes = append(res.Notes, "the target asks for a password: set PGPASSWORD to it in your shell before applying the files")
	}
	res.Apply = applyCommand(spec, conn, tgt.Major)
	for i, dp := range plan.DBs {
		res.Databases = append(res.Databases, &DBResult{SourceDB: dp.Source, TargetDB: dp.Target, State: dp.State, TS: rp.TS,
			Key: rp.DBs[i].Key, Bytes: rp.DBs[i].Bytes, Encrypted: rp.DBs[i].Encrypted, dump: rp.DBs[i]})
	}
	var dumpBytes int64
	for _, dp := range rp.DBs {
		dumpBytes += dp.Bytes
	}
	dc := CheckDisk(ctx, runner, admin, dumpBytes)
	res.Disk = &dc
	if dc.Checked && !dc.Enough() {
		res.warn("only %s free at %s, about %s may be needed", humanBytes(dc.FreeBytes), dc.DataDirectory, humanBytes(dc.NeedBytes))
	}
	dir := o.SQLDir
	if dir == "" {
		dir = "bracer-restore-" + cfg.App + "-" + inst + "-" + rp.TS
	}
	if !plan.OK() {
		if len(plan.Up) > 0 && !o.DryRun {
			if err := writeEnvFiles(dir, plan, res, spec); err != nil {
				return res, fmt.Errorf("write SQL files: %w", err)
			}
		}
		if plan.Restart {
			res.Restart = plan.RestartCmd
		}
		return res, ErrPreflight
	}
	if o.DryRun {
		return res, nil
	}

	// 4. Load: each database in one transaction, then ANALYZE.
	if d.Started != nil {
		d.Started(res)
	}
	loaded := map[string]string{}
	var targets []string
	res.OK = true
	for _, db := range res.Databases {
		start := d.Now()
		err := LoadDB(ctx, d.Store, db.dump, ids, runner, conn, db.TargetDB, res.Mode, d.Timeout)
		db.RestoreSeconds = d.Now().Sub(start).Seconds()
		if err != nil {
			db.Error = err.Error()
			if ctx.Err() != nil {
				db.Error = "pg_restore interrupted"
			}
			db.Error += "; the transaction rolled back, " + db.TargetDB + " is empty as before"
			res.OK = false
			continue
		}
		loaded[db.SourceDB] = db.TargetDB
		targets = append(targets, db.TargetDB)
		d.Log.Info("restored", "db", db.TargetDB, "seconds", fmt.Sprintf("%.1f", db.RestoreSeconds))
	}
	if len(targets) > 0 {
		paths, err := WriteSQLFiles(dir, SQLFile{Suffix: "data-down", Header: dataDownHeader(targets, res.Apply), Statements: DataDown(tgt, targets)})
		if err != nil {
			res.warn("could not write data-down SQL: %v", err)
		} else {
			res.SQLDir, res.DataDown = absPath(dir), paths[0]
		}
	}
	if !res.OK {
		return res, nil
	}

	// 5. Prove it: the environment again, the tables, drill.checks, the app.
	after, err := ReadTarget(ctx, admin, spec, full, targets, src.Locales())
	if err != nil {
		return res, fmt.Errorf("read target after the load: %w", err)
	}
	check := MakePlan(PlanInput{Src: src, Tgt: after, Mode: res.Mode, Loaded: loaded, Now: d.Now()})
	if !check.OK() {
		res.Verify = check.Failed()
		res.OK = false
	}
	dumps := map[string]*backup.Dump{}
	for _, db := range res.Databases {
		c := &pg.Client{Runner: runner, Conn: conn.WithDatabase(db.TargetDB)}
		db.TablesExpected = len(db.dump.Tables)
		cmp, err := Compare(ctx, c, db.dump.Tables)
		if err != nil {
			db.Error = fmt.Sprintf("compare tables: %v", err)
			res.OK = false
			continue
		}
		db.TablesFound, db.MissingTables, db.RowMismatches = cmp.Found, cmp.Missing, cmp.Mismatches
		db.Checks = RunChecks(ctx, c, cfg, db.SourceDB)
		db.OK = cmp.OK()
		for _, ch := range db.Checks {
			db.OK = db.OK && ch.OK
		}
		res.OK = res.OK && db.OK
		dumps[db.TargetDB] = db.dump
	}
	if o.AppURL || o.AppURLEnv != "" {
		raw, err := appURL(o, d)
		if err != nil {
			res.AppCheck = &AppCheck{Error: err.Error()}
		} else {
			res.AppCheck = CheckApp(ctx, d.Shell, pg.ImageForMajor(tgt.Major), appNetwork(tgt), raw, dumps)
		}
		res.OK = res.OK && res.AppCheck.OK
	}
	return res, nil
}

// identities reads the age key named in app.yaml.
func identities(cfg *config.App, d Deps) *Identities {
	path, err := config.ExpandHome(cfg.AgeIdentityFile)
	if err != nil {
		return &Identities{Err: err}
	}
	return ReadIdentities(path, d.ReadIdentities)
}

// pickRestoreRunner: an archive from pg_dump N can only be read by pg_restore ≥ N.
func pickRestoreRunner(ctx context.Context, d Deps, o Options, rp *RunPick, runner pg.Runner, conn pg.Conn, res *Result) (pg.Runner, error) {
	need := 0
	for _, dp := range rp.DBs {
		need = max(need, pg.MajorOf(dp.ToolVersion))
	}
	if need == 0 {
		return runner, nil
	}
	admin := &pg.Client{Runner: runner, Conn: conn}
	tv, tmaj, err := admin.ToolVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("check pg_restore on target: %w", err)
	}
	if tmaj >= need {
		return runner, nil
	}
	if o.IntoContainer != "" {
		return nil, usagef("dumps were made with pg_dump %d; pg_restore %s in container %s cannot read them (archive format is newer)", need, tv, o.IntoContainer)
	}
	w := pg.Where{}
	if o.IntoLocal != 0 {
		w.Local = &pg.Local{Dir: socketDir(o), Port: o.IntoLocal}
	}
	alt, err := d.Tools.Pick(ctx, w, need)
	if err != nil {
		return nil, fmt.Errorf("dumps were made with pg_dump %d but pg_restore %s cannot read them: %v", need, tv, err)
	}
	res.Target.Runner = alt.Describe()
	return alt, nil
}

func socketDir(o Options) string {
	if o.SocketDir != "" {
		return o.SocketDir
	}
	return config.DefaultSocketDir
}

// connectTarget builds the runner for the target given on the command line.
func connectTarget(ctx context.Context, o Options, d Deps, res *Result) (pg.Conn, pg.Runner, TargetSpec, error) {
	switch {
	case o.IntoContainer != "":
		user, err := d.ContainerUser(ctx, o.IntoContainer)
		if err != nil {
			return pg.Conn{}, nil, TargetSpec{}, usagef("--into-container: %v", err)
		}
		in := config.Instance{Name: o.IntoContainer, Container: o.IntoContainer, User: user}
		conn, runner, err := connectSocket(ctx, o, d, in, "container "+o.IntoContainer)
		if err != nil {
			return conn, nil, TargetSpec{}, err
		}
		conn.Database = "postgres"
		res.Target.Container = o.IntoContainer
		res.Target.Runner = runner.Describe()
		return conn, runner, TargetSpec{Kind: "container", Container: o.IntoContainer, Shell: d.Shell, Password: conn.Password != ""}, nil
	case o.IntoLocal != 0:
		in := config.Instance{Name: "local", Local: &config.Local{Port: o.IntoLocal, SocketDir: o.SocketDir}}
		conn, runner, err := connectSocket(ctx, o, d, in, fmt.Sprintf("the local Postgres on port %d", o.IntoLocal))
		if err != nil {
			return conn, nil, TargetSpec{}, err
		}
		l := &pg.Local{Dir: socketDir(o), Port: o.IntoLocal}
		res.Target.Local = l.SocketPath()
		res.Target.Runner = runner.Describe()
		return conn, runner, TargetSpec{Kind: "local", Local: l, Shell: d.Shell, Password: conn.Password != ""}, nil
	}
	var raw string
	if o.IntoURL {
		if d.AskURL == nil {
			return pg.Conn{}, nil, TargetSpec{}, usagef("no terminal to ask for the target URL; pass it in an environment variable with --into-url-env <NAME>")
		}
		v, err := d.AskURL()
		if err != nil {
			return pg.Conn{}, nil, TargetSpec{}, usagef("--into-url: %v", err)
		}
		raw = strings.TrimSpace(v)
	} else {
		v, err := d.Secrets.Require(o.IntoURLEnv)
		if err != nil {
			return pg.Conn{}, nil, TargetSpec{}, usagef("--into-url-env: %v", err)
		}
		raw = v
		res.Target.URLEnv = o.IntoURLEnv
	}
	conn, err := pg.ParseURL(raw)
	if err != nil {
		return conn, nil, TargetSpec{}, usagef("target URL: %v", err)
	}
	if conn.Database == "" {
		conn.Database = "postgres"
	}
	runner, _, err := d.Tools.Resolve(ctx, pg.Where{}, conn, 0)
	if err != nil {
		return conn, nil, TargetSpec{}, fmt.Errorf("connect to %s: %w", hostPort(conn), err)
	}
	res.Target.Host = hostPort(conn)
	res.Target.Runner = runner.Describe()
	return conn, runner, TargetSpec{Kind: "url"}, nil
}

// passwordPrompt is how psql asks when the socket wants a password.
var passwordPrompt = []string{"no password supplied", "password authentication failed", "Password for user"}

// connectSocket connects to a container or local target: peer or trust
// first, then the password from --password-env or the terminal.
func connectSocket(ctx context.Context, o Options, d Deps, in config.Instance, what string) (pg.Conn, pg.Runner, error) {
	const typed = "BRACER_TARGET_PASSWORD"
	secrets := d.Secrets
	in.PasswordEnv = o.PasswordEnv
	conn, runner, err := d.Connect(ctx, d.Tools, in, secrets)
	if err == nil || o.PasswordEnv != "" || !containsAny(err.Error(), passwordPrompt) {
		if err != nil {
			return conn, nil, fmt.Errorf("connect to %s: %w", what, err)
		}
		return conn, runner, nil
	}
	if d.AskPassword == nil {
		return conn, nil, usagef("%s asks for a password; pass it in an environment variable with --password-env <NAME>, or run restore in a terminal", what)
	}
	v, aerr := d.AskPassword()
	if aerr != nil {
		return conn, nil, usagef("password: %v", aerr)
	}
	in.PasswordEnv = typed
	conn, runner, err = d.Connect(ctx, d.Tools, in, secrets.With(typed, v, "terminal"))
	if err != nil {
		return conn, nil, fmt.Errorf("connect to %s: %w", what, err)
	}
	return conn, runner, nil
}

func containsAny(s string, subs []string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}

// applyCommand is how the customer applies a file to this target. When the
// target asked for a password, psql takes it from PGPASSWORD in their shell.
func applyCommand(spec TargetSpec, conn pg.Conn, major int) string {
	switch spec.Kind {
	case "container":
		env := ""
		if spec.Password {
			env = " -e PGPASSWORD"
		}
		return fmt.Sprintf("docker exec -i%s %s psql -U %s -d postgres -v ON_ERROR_STOP=1 < <file>", env, spec.Container, conn.User)
	case "local":
		h, env := "", ""
		if spec.Local.Dir != config.DefaultSocketDir {
			h = " -h " + spec.Local.Dir
		}
		if spec.Password {
			env = " --preserve-env=PGPASSWORD"
		}
		return fmt.Sprintf("sudo -u postgres%s psql%s -p %d -d postgres -v ON_ERROR_STOP=1 < <file>", env, h, spec.Local.Port)
	}
	return fmt.Sprintf(`PGURL='<target URL>' docker run --rm -i --network host -e PGURL %s sh -c 'psql "$PGURL" -v ON_ERROR_STOP=1' < <file>`, pg.ImageForMajor(major))
}

// appNetwork: the app of a container target reaches it on the container's
// network (db:5432 resolves as the app sees it); others from the host.
func appNetwork(t *TargetState) string {
	if t.Kind == "container" && t.Env.Container != nil && len(t.Env.Container.Networks) > 0 {
		return t.Env.Container.Networks[0]
	}
	return "host"
}

func appURL(o Options, d Deps) (string, error) {
	if o.AppURLEnv != "" {
		return d.Secrets.Require(o.AppURLEnv)
	}
	if d.AskAppURL == nil {
		return "", fmt.Errorf("no terminal to ask for the app URL; use --app-url-env <NAME>")
	}
	v, err := d.AskAppURL()
	return strings.TrimSpace(v), err
}

func writeEnvFiles(dir string, plan *Plan, res *Result, spec TargetSpec) error {
	head := func(what string) []string {
		h := []string{
			fmt.Sprintf("bracer restore: %s for the backup run %s of %s", what, res.Source.TS, res.Source.Instance),
			"Apply with: " + res.Apply,
		}
		if what == "env-up" {
			h = append(h, "Read it first. It may hold password hashes: delete this directory when done.",
				"Then run the same bracer restore again.")
			if plan.Restart {
				h = append(h, "After it: "+plan.RestartCmd)
			}
		} else {
			h = append(h, "Puts the target back as it was before env-up (safe to run twice).")
			if plan.Restart {
				h = append(h, "After it: "+plan.RestartCmd)
			}
		}
		return h
	}
	paths, err := WriteSQLFiles(dir, SQLFile{Suffix: "env-up", Header: head("env-up"), Statements: plan.Up},
		SQLFile{Suffix: "env-down", Header: head("env-down"), Statements: plan.Down})
	if err != nil {
		return err
	}
	res.SQLDir, res.EnvUp, res.EnvDown = absPath(dir), paths[0], paths[1]
	return nil
}

func dataDownHeader(targets []string, apply string) []string {
	return []string{
		"bracer restore: data-down for " + strings.Join(targets, ", "),
		"Empties the loaded databases again: drops each and recreates it as it was right before the load.",
		"Apply with: " + apply,
	}
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func withDefaults(d Deps) Deps {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.ReadIdentities == nil {
		d.ReadIdentities = crypto.ReadIdentities
	}
	if d.Connect == nil {
		d.Connect = backup.Connect
	}
	if d.Timeout == 0 {
		d.Timeout = config.DefaultDrillTimeout
	}
	if d.Tools == nil {
		d.Tools = pg.NewTools(d.Shell)
	}
	if d.ContainerUser == nil {
		docker := d.Tools.Docker()
		d.ContainerUser = docker.ContainerUser
	}
	return d
}

func hostPort(c pg.Conn) string {
	if c.Host == "" {
		return ""
	}
	if c.Port != 0 {
		return c.Host + ":" + strconv.Itoa(c.Port)
	}
	return c.Host
}

func targetName(t Target) string {
	switch {
	case t.Container != "":
		return "container " + t.Container
	case t.Local != "":
		return "local " + t.Local
	case t.Host != "":
		return t.Host
	}
	return "the target"
}

func major(v string) int {
	n := 0
	fmt.Sscanf(v, "%d", &n)
	return n
}

// humanBytes is as in the CLI, used for the disk message.
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

// pgdmpMagic is the first 5 bytes of a custom archive (pg_dump -Fc).
const pgdmpMagic = "PGDMP"

// checksumErr returns the source stream's SHA-256 error if one was detected.
func checksumErr(r interface{}) error {
	if c, ok := r.(interface{ ChecksumErr() error }); ok {
		return c.ChecksumErr()
	}
	return nil
}
