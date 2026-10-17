package restore

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"filippo.io/age"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/crypto"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/storage"
)

// Options holds the flags of `bracer restore`.
type Options struct {
	Instance      string // --instance: source instance in the manifest
	Database      string // --database: source database
	TS            string // --ts, defaults to latest
	File          string // --file: the source is a fetched file instead of the bucket
	Into          string // --into <instance>[/<db>]
	IntoURLEnv    string // --into-url-env NAME
	WithGlobals   bool   // --with-globals: only create the missing users
	SkipDiskCheck bool   // --skip-disk-check
	NoOwner       bool   // --no-owner
	NoACL         bool   // --no-acl
	DryRun        bool   // --dry-run
}

// Deps is everything Run reaches outside; tests swap in fakes.
type Deps struct {
	Shell          pg.Shell
	Store          storage.Store
	Secrets        *config.Secrets
	GOOS           string
	Tools          *pg.Tools // nil: built from Shell and GOOS
	Now            func() time.Time
	Log            *slog.Logger
	ReadIdentities func(path string) ([]age.Identity, []string, error)
	// Connect defaults to backup.Connect.
	Connect backup.ConnectFunc
	// Timeout for pg_restore; 0 = DefaultDrillTimeout (shares the drill.timeout setting).
	Timeout time.Duration
}

// Result is the result of `restore` (`--json`, spec fetch-restore § Data model).
type Result struct {
	Kind           string         `json:"kind"`
	App            string         `json:"app"`
	Source         Source         `json:"source"`
	Target         Target         `json:"target"`
	Disk           *DiskCheck     `json:"disk,omitempty"`
	Globals        *GlobalsResult `json:"globals,omitempty"`
	OK             bool           `json:"ok"`
	DryRun         bool           `json:"dry_run,omitempty"`
	RestoreSeconds float64        `json:"restore_seconds"`
	TablesExpected int            `json:"tables_expected"`
	TablesFound    int            `json:"tables_found"`
	MissingTables  []string       `json:"missing_tables,omitempty"`
	RowMismatches  []RowMismatch  `json:"row_mismatches,omitempty"`
	Error          string         `json:"error,omitempty"`
	Warnings       []string       `json:"warnings,omitempty"`
}

// Source is the dump being restored.
type Source struct {
	Instance  string `json:"instance,omitempty"`
	DB        string `json:"db,omitempty"`
	TS        string `json:"ts,omitempty"`
	Key       string `json:"key,omitempty"`
	File      string `json:"file,omitempty"`
	Bytes     int64  `json:"bytes,omitempty"`
	Encrypted bool   `json:"encrypted"`
	PgVersion string `json:"pg_version,omitempty"`
}

// Target is the target server and database.
type Target struct {
	Instance  string `json:"instance,omitempty"`
	URLEnv    string `json:"url_env,omitempty"`
	Host      string `json:"host,omitempty"`
	DB        string `json:"db"`
	Runner    string `json:"runner"`
	PgVersion string `json:"pg_version,omitempty"`
	Exists    bool   `json:"exists"`
}

// ErrExists: the target database already exists; restore never overwrites.
var ErrExists = errors.New("database exists")

func (r *Result) warn(format string, a ...interface{}) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, a...))
}

// Run performs the restore (spec fetch-restore § Flow). *UsageError → exit 2;
// other errors → exit 1; Result.Error records pg_restore/comparison errors.
func Run(ctx context.Context, cfg *config.App, o Options, d Deps) (*Result, error) {
	d = withDefaults(d)
	res := &Result{Kind: "restore", App: cfg.App, DryRun: o.DryRun}

	// 1. Source.
	var manifest *backup.Manifest
	var sel *Selection
	var globals *backup.Dump
	if o.File == "" {
		var err error
		manifest, err = backup.LoadManifest(ctx, d.Store, storage.Join(cfg.Storage.Prefix, cfg.App, "manifest.json"), cfg.App)
		if err != nil {
			return nil, fmt.Errorf("load manifest: %w", err)
		}
		if o.Database == "" {
			return nil, usagef("pass --database <name> (or --file <dump>)")
		}
		sel, err = Select(manifest, cfg, o.Instance, o.Database, o.TS, false)
		if err != nil {
			return nil, err
		}
		res.Source = Source{Instance: sel.Instance, DB: sel.Database, TS: sel.Dump.TS, Key: sel.Dump.Key, Bytes: sel.Dump.Bytes, Encrypted: sel.Dump.Encrypted, PgVersion: sel.PgVersion}
		if sel.Dump.SHA256 == "" {
			res.warn("manifest has no sha256 for this dump; its integrity cannot be checked")
		}
		if !sel.Dump.Encrypted && earlierEncrypted(manifest, sel) {
			res.warn("this dump is not encrypted although an earlier dump of %s was; check that encryption was turned off on purpose", sel.Database)
		}
		if o.WithGlobals {
			im := manifest.Instances[sel.Instance]
			for _, g := range im.Globals.Dumps {
				if g.TS == im.Globals.Latest {
					globals = g
				}
			}
			if globals == nil {
				return nil, usagef("--with-globals: instance %s has no globals dump", sel.Instance)
			}
		}
	} else {
		if o.WithGlobals {
			return nil, usagef("--with-globals needs the bucket; it cannot be combined with --file")
		}
		res.Source = Source{File: o.File}
	}
	targetDB := o.Database

	// 2. Target.
	if (o.Into == "") == (o.IntoURLEnv == "") {
		return nil, usagef("pass exactly one of --into <instance>[/<db>] or --into-url-env <NAME>")
	}
	var conn pg.Conn
	var runner pg.Runner
	var in *config.Instance
	if o.Into != "" {
		name, db, _ := strings.Cut(o.Into, "/")
		in = cfg.FindInstance(name)
		if in == nil {
			return nil, usagef("--into: instance %q is not in app.yaml", name)
		}
		if db != "" {
			targetDB = db
		}
		var err error
		conn, runner, err = d.Connect(ctx, d.Tools, *in, d.Secrets)
		if err != nil {
			return nil, fmt.Errorf("connect to %s: %w", name, err)
		}
		res.Target.Instance = name
	} else {
		raw, err := d.Secrets.Require(o.IntoURLEnv)
		if err != nil {
			return nil, usagef("--into-url-env: %v", err)
		}
		conn, err = pg.ParseURL(raw)
		if err != nil {
			return nil, usagef("--into-url-env %s: %v", o.IntoURLEnv, err)
		}
		res.Target.URLEnv = o.IntoURLEnv
		// The database in the URL (postgres://…/newdb) is the target name; "postgres" or empty → the source name.
		if conn.Database != "" && conn.Database != "postgres" {
			targetDB = conn.Database
		}
		// Runner per ADR-0009: a host binary that is new enough, otherwise an image of the same major as
		// the target server (version queried first). Connect to the maintenance database: the database
		// in the URL may not exist yet (it is the target).
		conn.Database = "postgres"
		runner, _, err = d.Tools.Resolve(ctx, pg.Where{}, conn, 0)
		if err != nil {
			return nil, fmt.Errorf("connect to %s: %w", conn.Host, err)
		}
	}
	if targetDB == "" {
		if o.File != "" {
			return nil, usagef("--file needs the target database name: --into <instance>/<db>, or a database in the --into-url-env URL")
		}
		return nil, usagef("target database name is required")
	}
	res.Target.DB = targetDB
	res.Target.Host = hostPort(conn)
	res.Target.Runner = runner.Describe()
	admin := &pg.Client{Runner: runner, Conn: conn}
	admin.Conn.Database = "postgres"
	if v, err := admin.ServerVersion(ctx); err != nil {
		return nil, fmt.Errorf("connect to target: %w", err)
	} else {
		res.Target.PgVersion = v
	}
	// The target user must be able to create databases (and users with --with-globals); the instance's
	// backup user is read-only, so report right away, before downloading anything.
	if ap, err := admin.AdminPrivileges(ctx); err != nil {
		return nil, fmt.Errorf("check target user: %w", err)
	} else if !ap.Superuser && (!ap.CreateDB || (o.WithGlobals && !ap.CreateRole)) {
		need := "CREATEDB"
		if ap.CreateDB {
			need = "CREATEROLE (for --with-globals)"
		}
		return nil, usagef("user %s on %s cannot restore: it needs %s or superuser. A backup user (pg_read_all_data) is read-only by design; restore with --into-url-env pointing at an admin URL, or GRANT the privilege yourself", ap.Role, targetName(res.Target), need)
	}
	if pooler, info := admin.DetectPooler(ctx); pooler {
		res.warn("target looks like a connection pooler (%s); pg_restore needs a direct connection (Supabase: port 5432, not 6543)", info)
	}
	if sel != nil && sel.PgVersion != "" && major(sel.PgVersion) > major(res.Target.PgVersion) {
		res.warn("dump is from PostgreSQL %s, target is %s; pg_restore may reject a dump from a newer server", sel.PgVersion, res.Target.PgVersion)
	}
	// An archive from pg_dump N can only be read by pg_restore ≥ N (ADR-0009 § Amendments).
	if sel != nil && pg.MajorOf(sel.Dump.ToolVersion) > 0 {
		need := pg.MajorOf(sel.Dump.ToolVersion)
		tv, tmaj, err := admin.ToolVersion(ctx)
		if err != nil {
			return nil, fmt.Errorf("check pg_restore on target: %w", err)
		}
		if tmaj < need {
			switch {
			case o.IntoURLEnv != "" || (in != nil && in.Host != "") || (in != nil && in.URLEnv != ""):
				// Host/URL: re-pick a runner that is new enough (a host binary of the same major, or the postgres:N image).
				alt, perr := d.Tools.Pick(ctx, pg.Where{}, need)
				if perr != nil {
					return nil, fmt.Errorf("dump was made with pg_dump %s but pg_restore %s here cannot read it: %v", sel.Dump.ToolVersion, tv, perr)
				}
				runner = alt
				admin = &pg.Client{Runner: runner, Conn: admin.Conn}
				res.Target.Runner = runner.Describe()
				if _, amaj, aerr := admin.ToolVersion(ctx); aerr != nil || amaj < need {
					return nil, fmt.Errorf("dump was made with pg_dump %s; no pg_restore >= %d is available here (install the PostgreSQL %d client package or Docker)", sel.Dump.ToolVersion, need, need)
				}
			default:
				// Container/pod/socket: pg_restore is the target server's own and cannot be swapped.
				return nil, usagef("dump was made with pg_dump %s; pg_restore %s in %s cannot read it (archive format is newer). Restore from a host with pg_restore >= %d via --into-url-env", sel.Dump.ToolVersion, tv, targetName(res.Target), need)
			}
		}
		if major(res.Target.PgVersion) < need {
			res.warn("dump was made with pg_dump %s, target is PostgreSQL %s: pg_restore may report errors for settings this server does not know (pg_dump output is meant for the same or a newer server)", sel.Dump.ToolVersion, res.Target.PgVersion)
		}
	}
	// Source locale (manifest): the ICU/builtin provider needs a target server that is new enough; if known
	// up front, stop before downloading anything.
	var loc *pg.Locale
	if sel != nil {
		loc = sel.Dump.Locale
	}
	if loc != nil {
		if _, err := loc.CreateOptions(major(res.Target.PgVersion)); err != nil {
			return nil, usagef("cannot create %s on %s: %v", targetDB, targetName(res.Target), err)
		}
	} else {
		res.warn("source locale unknown (--file, or a manifest written by an older Bracer): %s is created with the server's default locale", targetDB)
	}
	// The source database's extensions must exist on the target (evidence:
	// pg_available_extensions), checked before creating anything (spec fetch-restore § Flow 2).
	if sel != nil && len(sel.Dump.Extensions) > 0 {
		avail, err := admin.AvailableExtensions(ctx)
		if err != nil {
			return nil, fmt.Errorf("check extensions on target: %w", err)
		}
		missing, differ := pg.MissingExtensions(sel.Dump.Extensions, avail)
		if len(missing) > 0 {
			return nil, usagef("target %s does not have extension %s that database %s uses; install it on the target first", targetName(res.Target), joinExtensions(missing), sel.Database)
		}
		for i, e := range differ {
			res.warn("target has extension %s %s, the source database used %s %s; pg_restore installs the target's version", e.Name, e.Version, e.Name, versionOf(sel.Dump.Extensions, e.Name))
			_ = i
		}
	}

	// 3. Does the target database exist? There is no overwrite flag: the customer drops it themselves if they really want to.
	exists, err := DatabaseExists(ctx, admin, targetDB)
	if err != nil {
		return nil, fmt.Errorf("check database: %w", err)
	}
	res.Target.Exists = exists
	if exists {
		how := fmt.Sprintf("restore into another name with --into %s/<name>", res.Target.Instance)
		if res.Target.Instance == "" {
			how = "put another database name in the URL path"
		}
		return res, fmt.Errorf("%w: database %s already exists on %s; %s, or drop it yourself first (bracer never drops databases)", ErrExists, targetDB, targetName(res.Target), how)
	}

	// Free disk on the target: actual data ≈ DiskFactor × the compressed dump.
	dumpBytes := int64(0)
	if sel != nil {
		dumpBytes = sel.Dump.Bytes
	} else if fi, err := os.Stat(o.File); err == nil {
		dumpBytes = fi.Size()
	}
	dc := CheckDisk(ctx, runner, admin, dumpBytes)
	res.Disk = &dc
	if !dc.Checked {
		res.warn("%s", dc.Error)
	} else if !dc.Enough() {
		if !o.SkipDiskCheck {
			return res, fmt.Errorf("%w: %s has %s free at %s; restoring a %s dump may need about %s; free space or pass --skip-disk-check",
				ErrDiskSpace, targetName(res.Target), humanBytes(dc.FreeBytes), dc.DataDirectory, humanBytes(dumpBytes), humanBytes(dc.NeedBytes))
		}
		res.warn("only %s free at %s, about %s may be needed (--skip-disk-check)", humanBytes(dc.FreeBytes), dc.DataDirectory, humanBytes(dc.NeedBytes))
	}
	if o.DryRun {
		return res, nil
	}

	// The age key only when the source is encrypted (file: recognized by the header).
	var ids *Identities
	if (sel != nil && (sel.Dump.Encrypted || (globals != nil && globals.Encrypted))) || o.File != "" {
		path, perr := config.ExpandHome(cfg.AgeIdentityFile)
		if perr != nil {
			ids = &Identities{Err: perr}
		} else {
			ids = ReadIdentities(path, d.ReadIdentities)
		}
	}

	// 4. Open the source, create database, globals, pg_restore over stdin.
	var src interface {
		Read([]byte) (int, error)
		Close() error
	}
	if o.File != "" {
		rc, enc, err := OpenFile(o.File, ids)
		if err != nil {
			return res, fmt.Errorf("open %s: %w", o.File, err)
		}
		src, res.Source.Encrypted = rc, enc
	} else {
		rc, err := OpenDump(ctx, d.Store, sel.Dump, ids)
		if err != nil {
			return res, err
		}
		src = rc
	}
	defer src.Close()
	// A pg_dump custom archive starts with "PGDMP"; a foreign/corrupt file stops us
	// before creating the database (checksumErr is still called on the original src).
	body := bufio.NewReader(src)
	if head, _ := body.Peek(len(pgdmpMagic)); !bytes.Equal(head, []byte(pgdmpMagic)) {
		return res, fmt.Errorf("%s is not a pg_dump custom-format archive (expected it to start with %q); nothing was created", sourceName(o, sel), pgdmpMagic)
	}
	if err := CreateDatabase(ctx, admin, targetDB, loc, major(res.Target.PgVersion)); err != nil {
		return res, fmt.Errorf("create database %s: %w", targetDB, err)
	}
	leftInPlace := fmt.Sprintf("database %s was left in place for inspection; drop it with: drop database %s", targetDB, pg.QuoteIdent(targetDB))

	// 5. Globals (after create database: a failure here only leaves an empty database behind).
	if globals != nil {
		rc, err := OpenDump(ctx, d.Store, globals, ids)
		if err != nil {
			return res, fmt.Errorf("globals: %w; %s", err, leftInPlace)
		}
		existing, err := ListRoles(ctx, admin)
		if err != nil {
			rc.Close()
			return res, fmt.Errorf("globals: list users on target: %w; %s", err, leftInPlace)
		}
		res.Globals = GlobalsCreateMissing(ctx, runner, conn, existing, rc)
		rc.Close()
		if !res.Globals.OK {
			res.warn("globals: %s", res.Globals.Error)
		}
		for _, g := range res.Globals.SkippedGrants {
			res.warn("globals: skipped GRANT %s: the granted user already exists on the target and bracer does not add members to existing users", g)
		}
		for _, dg := range res.Globals.Downgraded {
			res.warn("globals: privilege removed from user, %s (new users get no cluster-wide power; ALTER ROLE yourself if needed)", dg)
		}
		for _, g := range res.Globals.SkippedMissing {
			res.warn("globals: skipped GRANT %s: that built-in role does not exist on the target's PostgreSQL version", g)
		}
		for _, g := range res.Globals.PgGrants {
			role, _, _ := strings.Cut(g, " ->")
			if RiskyPgRoles[role] {
				res.warn("globals: GRANT %s gives the new user access beyond the restored database; REVOKE it if that is not intended", g)
			}
		}
		d.Log.Info("globals: users created", "created", len(res.Globals.Created), "existing_skipped", res.Globals.SkippedExisting, "ok", res.Globals.OK)
	}
	var extra []string
	if o.NoOwner {
		extra = append(extra, "--no-owner")
	}
	if o.NoACL {
		extra = append(extra, "--no-acl")
	}
	start := d.Now()
	rerr := Database(ctx, runner, conn, targetDB, extra, body, d.Timeout)
	res.RestoreSeconds = d.Now().Sub(start).Seconds()
	// A SHA-256 mismatch takes priority: for an encrypted dump, age reports the error before handing over
	// the last chunk, so pg_restore only sees a truncated stream ("could not read input file").
	if ce := checksumErr(src); ce != nil {
		rerr = ce
	}
	if rerr != nil {
		if ctx.Err() != nil {
			res.Error = "pg_restore interrupted"
		} else {
			res.Error = rerr.Error()
		}
		res.Error += "\n" + leftInPlace
		return res, nil
	}
	d.Log.Info("restored", "db", targetDB, "seconds", fmt.Sprintf("%.1f", res.RestoreSeconds))

	// 6. Compare against the manifest (file: only count tables).
	target := &pg.Client{Runner: runner, Conn: conn}
	target.Conn.Database = targetDB
	if sel != nil {
		res.TablesExpected = len(sel.Dump.Tables)
		cmp, err := Compare(ctx, target, sel.Dump.Tables)
		if err != nil {
			res.Error = fmt.Sprintf("compare tables: %v", err)
			return res, nil
		}
		res.TablesFound, res.MissingTables, res.RowMismatches = cmp.Found, cmp.Missing, cmp.Mismatches
		res.OK = cmp.OK()
	} else {
		tables, err := target.Tables(ctx)
		if err != nil {
			res.Error = fmt.Sprintf("list tables: %v", err)
			return res, nil
		}
		res.TablesFound = len(tables)
		res.OK = true
	}
	return res, nil
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
	if d.GOOS == "" {
		d.GOOS = "linux"
	}
	if d.Tools == nil {
		d.Tools = pg.NewTools(d.Shell, d.GOOS)
	}
	return d
}

func hostPort(c pg.Conn) string {
	if c.Host == "" {
		return ""
	}
	if c.Port != 0 {
		return fmt.Sprintf("%s:%d", c.Host, c.Port)
	}
	return c.Host
}

func targetName(t Target) string {
	if t.Instance != "" {
		return t.Instance
	}
	return "$" + t.URLEnv
}

func major(v string) int {
	n := 0
	fmt.Sscanf(v, "%d", &n)
	return n
}

// humanBytes is as in the CLI, used for the disk error message.
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

// checksumErr returns the source stream's SHA-256 error if one was detected.
// pgdmpMagic is the first 5 bytes of a custom archive (pg_dump -Fc).
const pgdmpMagic = "PGDMP"

// sourceName names the source for error messages.
func sourceName(o Options, sel *Selection) string {
	if o.File != "" {
		return o.File
	}
	if sel != nil {
		return sel.Dump.Key
	}
	return "the source"
}

func checksumErr(r io.Reader) error {
	if c, ok := r.(interface{ ChecksumErr() error }); ok {
		return c.ChecksumErr()
	}
	return nil
}

// earlierEncrypted: an older dump of the same database was encrypted while the chosen one is not.
func earlierEncrypted(m *backup.Manifest, sel *Selection) bool {
	im := m.Instances[sel.Instance]
	if im == nil || im.Databases[sel.Database] == nil {
		return false
	}
	for _, dp := range im.Databases[sel.Database].Dumps {
		if dp.Encrypted && dp.UploadedAt.Before(sel.Dump.UploadedAt) {
			return true
		}
	}
	return false
}

// joinExtensions: "postgis 3.4.2, pg_partman 5.0.1".
func joinExtensions(exts []pg.Extension) string {
	parts := make([]string, len(exts))
	for i, e := range exts {
		parts[i] = e.String()
	}
	return strings.Join(parts, ", ")
}

// versionOf returns the version of extension name in the manifest list.
func versionOf(exts []pg.Extension, name string) string {
	for _, e := range exts {
		if e.Name == name {
			return e.Version
		}
	}
	return ""
}
