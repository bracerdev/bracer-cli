package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/crypto"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/restore"
	"bracer.dev/bracer/internal/storage"
)

// multiFlag is a flag that can be repeated.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// runRestore is `bracer restore` (spec backup/fetch-restore, ADR-0015).
func runRestore(ctx context.Context, args []string, d backupDeps) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	cfgPath := fs.String("c", "app.yaml", "path of app.yaml (missing: asked for, read from the bucket)")
	envFile := fs.String("env-file", "", "dotenv file with secrets (overrides secrets_file, overridden by the environment)")
	var o restore.Options
	var dbs multiFlag
	fs.StringVar(&o.Instance, "instance", "", "source instance in the bucket (default: the only one with backups)")
	fs.Var(&dbs, "database", "source database to restore; repeat for several (default: every database of the run)")
	fs.StringVar(&o.TS, "ts", "", "backup run as shown by fetch --list (default: the latest)")
	fs.StringVar(&o.File, "file", "", "preview a file fetched earlier instead of the bucket")
	fs.StringVar(&o.IntoContainer, "into-container", "", "target: name of a PostgreSQL container on this machine")
	fs.IntVar(&o.IntoLocal, "into-local", 0, "target: port of PostgreSQL installed on this machine (over its socket)")
	fs.StringVar(&o.SocketDir, "socket-dir", "", "with --into-local: socket directory (default /var/run/postgresql)")
	fs.BoolVar(&o.IntoURL, "into-url", false, "target: ask for a postgres:// URL (not echoed, not stored)")
	fs.StringVar(&o.IntoURLEnv, "into-url-env", "", "target: name of an environment variable holding a postgres:// URL")
	fs.BoolVar(&o.Preview, "preview", false, "restore the data to look at only: no owners, privileges or users; CREATEDB is enough")
	fs.StringVar(&o.SQLDir, "sql-dir", "", "directory for the env-up/env-down/data-down SQL (default ./bracer-restore-<app>-<instance>-<ts>)")
	fs.BoolVar(&o.AppURL, "app-url", false, "after the load, ask for the app's URL and check it connects and reads the tables it has SELECT on")
	fs.StringVar(&o.AppURLEnv, "app-url-env", "", "as --app-url, from an environment variable")
	fs.BoolVar(&o.DryRun, "dry-run", false, "show source, target and the comparison; write and change nothing")
	removedInto := fs.String("into", "", "removed: use --into-container, --into-local or --into-url")
	removedSkip := fs.Bool("skip-disk-check", false, "removed: free disk is only a warning now")
	asJSON := fs.Bool("json", false, "print the result as JSON on stdout")
	verbose := fs.Bool("verbose", false, "log every docker and pg_* command")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	o.Databases = dbs
	fail := func(code int, format string, a ...interface{}) int {
		fmt.Fprintf(d.stderr, "bracer restore: "+format+"\n", a...)
		return code
	}
	if *removedInto != "" {
		return fail(exitUsage, "--into was removed; use --into-container <name>, --into-local <port> or --into-url")
	}
	if *removedSkip {
		return fail(exitUsage, "--skip-disk-check was removed: free disk is only a warning now")
	}
	logger := newLogger(d.stderr, *verbose)
	if sh, ok := d.shell.(pg.ExecShell); ok {
		sh.Verbose = *verbose
		d.shell = sh
	}

	var cfg *config.App
	var secrets *config.Secrets
	var st storage.Store
	if _, err := os.Stat(*cfgPath); errors.Is(err, os.ErrNotExist) && isTerminal(d.stdin) {
		fmt.Fprintf(d.stderr, "%s not found: reading the app's configuration from its bucket (nothing is written to disk)\n", *cfgPath)
		cfg, secrets, st, err = loadFromBucket(ctx, newPrompter(d.stdin, d.stderr), d)
		if err != nil {
			return fail(exitUsage, "%v", err)
		}
	} else {
		var code int
		cfg, secrets, st, code, err = loadAppAndStore(*cfgPath, *envFile, d, logger)
		if err != nil {
			return fail(code, "%v", err)
		}
	}
	d = d.withRuntime(cfg)
	started := false
	deps := restore.Deps{Shell: d.shell, Store: st, Secrets: secrets, Tools: d.tools(), Log: logger, Timeout: cfg.DrillTimeout(),
		AskURL:    askHidden(d.stdin, d.stderr, "Target URL (postgres://user:password@host:port/postgres, not shown, not stored): "),
		AskAppURL: askHidden(d.stdin, d.stderr, "The app's URL, exactly as the app uses it (not shown, not stored): ")}
	if !*asJSON {
		deps.Started = func(res *restore.Result) {
			started = true
			printRestoreHead(d.stdout, res)
			printChecks(d.stdout, res.Checks, false)
			fmt.Fprintln(d.stdout, "loading the data…")
		}
	}
	res, err := restore.Run(ctx, cfg, o, deps)
	rerun := "bracer restore " + shellJoin(args)
	if err != nil {
		var ue *restore.UsageError
		usage := errors.As(err, &ue) || errors.Is(err, restore.ErrPreflight)
		if res != nil && errors.Is(err, restore.ErrPreflight) {
			if *asJSON {
				printJSON(d.stdout, res)
			} else {
				printRestoreHead(d.stdout, res)
				printNotReady(d.stdout, res, rerun)
			}
			return exitUsage
		}
		if res != nil && *asJSON {
			res.Error = err.Error()
			printJSON(d.stdout, res)
		}
		if usage {
			return fail(exitUsage, "%v", err)
		}
		if res != nil && !*asJSON {
			for _, w := range res.Warnings {
				fmt.Fprintf(d.stderr, "warning: %s\n", w)
			}
		}
		return fail(exitError, "%v", err)
	}
	if *asJSON {
		printJSON(d.stdout, res)
	} else {
		if !started {
			printRestoreHead(d.stdout, res)
			if res.Mode != restore.ModePreview {
				printChecks(d.stdout, res.Checks, false)
			}
		}
		printRestoreResult(d.stdout, res, args)
	}
	if !res.OK && !res.DryRun {
		return exitError
	}
	return exitOK
}

func isTerminal(in io.Reader) bool {
	f, ok := in.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// loadFromBucket asks where the bucket is, reads the copy of app.yaml that
// backup keeps there and decrypts it with the age key (ADR-0015 § 3).
func loadFromBucket(ctx context.Context, p *prompter, d backupDeps) (*config.App, *config.Secrets, storage.Store, error) {
	endpoint, err := p.askRequired("S3 endpoint (https://…)")
	if err != nil {
		return nil, nil, nil, err
	}
	bucket, err := p.askRequired("Bucket")
	if err != nil {
		return nil, nil, nil, err
	}
	prefix, err := p.ask("Prefix (Enter for none)", "")
	if err != nil {
		return nil, nil, nil, err
	}
	app, err := p.askRequired("App name")
	if err != nil {
		return nil, nil, nil, err
	}
	region, err := p.ask("Region", "auto")
	if err != nil {
		return nil, nil, nil, err
	}
	pathStyle, err := p.yesNo("Path-style URLs (MinIO, Garage)?", false)
	if err != nil {
		return nil, nil, nil, err
	}
	ak, err := p.askSecret("Access key (not shown)")
	if err != nil {
		return nil, nil, nil, err
	}
	sk, err := p.askSecret("Secret key (not shown)")
	if err != nil {
		return nil, nil, nil, err
	}
	keyPath, err := p.ask("Path of the age key file (Enter if backups are not encrypted)", "")
	if err != nil {
		return nil, nil, nil, err
	}
	st, err := d.newStore(storage.S3Config{Endpoint: endpoint, Bucket: bucket, Region: region, AccessKey: ak, SecretKey: sk, PathStyle: pathStyle})
	if err != nil {
		return nil, nil, nil, err
	}
	var data []byte
	for _, enc := range []bool{true, false} {
		key := backup.AppYAMLKey(prefix, app, enc)
		rc, gerr := st.Get(ctx, key)
		if errors.Is(gerr, storage.ErrNotFound) {
			continue
		}
		if gerr != nil {
			return nil, nil, nil, fmt.Errorf("read %s: %w", key, gerr)
		}
		var r io.Reader = rc
		if enc {
			if keyPath == "" {
				rc.Close()
				return nil, nil, nil, fmt.Errorf("%s is encrypted: give the path of the age key file", key)
			}
			path, _ := config.ExpandHome(keyPath)
			ids, _, ierr := crypto.ReadIdentities(path)
			if ierr != nil {
				rc.Close()
				return nil, nil, nil, ierr
			}
			if r, err = crypto.Decrypt(rc, ids); err != nil {
				rc.Close()
				return nil, nil, nil, fmt.Errorf("decrypt %s: %w", key, err)
			}
		}
		data, err = io.ReadAll(io.LimitReader(r, 1<<20))
		rc.Close()
		if err != nil {
			return nil, nil, nil, err
		}
		break
	}
	if data == nil {
		return nil, nil, nil, fmt.Errorf("no copy of app.yaml under %s in bucket %s (backup writes it on every run since ADR-0015)", storage.Join(prefix, app), bucket)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return nil, nil, nil, err
	}
	if keyPath != "" {
		cfg.AgeIdentityFile = keyPath
	}
	vals := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		vals[k] = v
	}
	vals[cfg.Storage.AccessKeyEnv], vals[cfg.Storage.SecretKeyEnv] = ak, sk
	cfg.Runtime = nil
	return cfg, config.NewSecrets(vals, "terminal"), st, nil
}

func printJSON(w io.Writer, v interface{}) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// askHidden reads one value without echo; nil when stdin is not a terminal.
func askHidden(in io.Reader, out io.Writer, prompt string) func() (string, error) {
	f, ok := in.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return nil
	}
	return func() (string, error) {
		fmt.Fprint(out, prompt)
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(out)
		return string(b), err
	}
}

// printRestoreHead: source run, target and mode.
func printRestoreHead(w io.Writer, res *restore.Result) {
	t := res.Target
	role := "superuser"
	if !t.Superuser {
		role = "not superuser"
	}
	if res.Source.TS != "" {
		fmt.Fprintf(w, "source: instance %s, run %s (%s)\n", res.Source.Instance, res.Source.TS, orDash(res.Source.Kind))
	}
	fmt.Fprintf(w, "target: %s, PostgreSQL %s via %s, user %s (%s)\n", targetLabel(t), t.PgVersion, t.Runner, t.User, role)
	switch res.Mode {
	case restore.ModeFull:
		fmt.Fprintln(w, "mode: full (users, settings and every database of the run)")
	case restore.ModeData:
		fmt.Fprintln(w, "mode: data (the source was a URL: databases only, owned by the connecting user)")
	case restore.ModePreview:
		if res.Preflight != nil && res.Preflight.OK {
			printPreviewBanner(w, res)
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// printChecks lists the comparison: ✗ always, ✓ only when all is set.
func printChecks(w io.Writer, checks []restore.Check, onlyFailed bool) {
	for _, c := range checks {
		mark := "✓"
		switch {
		case !c.OK:
			mark = "✗"
		case c.Warn:
			mark = "!"
		case onlyFailed:
			continue
		}
		line := fmt.Sprintf("%s %s %s", mark, c.Area, c.Name)
		if c.Detail != "" {
			line += ": " + c.Detail
		}
		fmt.Fprintln(w, line)
		switch {
		case c.Fix == restore.FixSQL && c.Manual != "":
			fmt.Fprintf(w, "    fix: in env-up; %s\n", c.Manual)
		case c.Fix == restore.FixSQL:
			fmt.Fprintln(w, "    fix: in env-up")
		case c.Fix == restore.FixRerun:
			fmt.Fprintln(w, "    checked on the next run, after env-up")
		case c.Manual != "":
			fmt.Fprintf(w, "    fix: %s\n", c.Manual)
		}
	}
}

func printPreviewBanner(w io.Writer, res *restore.Result) {
	var names, drops []string
	for _, db := range res.Databases {
		if db.TargetDB != "" {
			names = append(names, db.TargetDB)
			drops = append(drops, "DROP DATABASE "+pg.QuoteIdent(db.TargetDB)+";")
		}
	}
	if len(names) == 0 {
		return
	}
	fmt.Fprintln(w, "PREVIEW: data only, for looking at. Owners, privileges and users were not restored;")
	fmt.Fprintf(w, "an app connected to %s will not work as before.\n", strings.Join(names, ", "))
	fmt.Fprintln(w, "To bring the app back, run bracer restore without --preview.")
	fmt.Fprintf(w, "When done: %s\n", strings.Join(drops, " "))
}

func targetLabel(t restore.Target) string {
	switch {
	case t.Container != "":
		return "container " + t.Container
	case t.Local != "":
		return "local " + t.Local
	case t.Host != "":
		if t.URLEnv != "" {
			return t.Host + " ($" + t.URLEnv + ")"
		}
		return t.Host
	}
	return "$" + t.URLEnv
}

// printNotReady explains what the customer changes on the target before running again.
func printNotReady(w io.Writer, res *restore.Result, rerun string) {
	if res.Mode == restore.ModePreview {
		pf := res.Preflight
		if n := len(pf.NeedsSuperuser); n > 0 {
			shown, more := pf.NeedsSuperuser, ""
			if n > 10 {
				shown, more = shown[:10], fmt.Sprintf("\n  (+%d more)", n-10)
			}
			fmt.Fprintf(w, "user %s is not a superuser and cannot restore these; nothing was created:\n  %s%s\n", res.Target.User, strings.Join(shown, "\n  "), more)
			fmt.Fprintln(w, "Run with a superuser target.")
		}
		fmt.Fprintf(w, "then run again: %s\n", rerun)
		return
	}
	printChecks(w, res.Checks, true)
	fmt.Fprintln(w, "The target is not ready for this backup; nothing was loaded.")
	if res.EnvUp != "" {
		fmt.Fprintf(w, "SQL for the target (read it first):\n  env-up:   %s\n  env-down: %s (puts the target back as it was)\n", res.EnvUp, res.EnvDown)
		fmt.Fprintf(w, "apply with: %s\n", strings.Replace(res.Apply, "<file>", res.EnvUp, 1))
		if res.Restart != "" {
			fmt.Fprintf(w, "then restart: %s\n", res.Restart)
		}
		if res.Mode == restore.ModeFull {
			fmt.Fprintf(w, "env-up may hold password hashes: delete %s when done\n", res.SQLDir)
		}
	} else if res.DryRun {
		fmt.Fprintln(w, "Dry run: no SQL written.")
	}
	fmt.Fprintf(w, "then run again: %s\n", rerun)
	for _, n := range res.Notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
	for _, wmsg := range res.Warnings {
		fmt.Fprintf(w, "warning: %s\n", wmsg)
	}
}

func printRestoreResult(w io.Writer, res *restore.Result, args []string) {
	if res.DryRun {
		for _, db := range res.Databases {
			src := res.Source.File
			if src == "" {
				src = fmt.Sprintf("%s (%s)", db.Key, humanBytes(db.Bytes))
			}
			state := ""
			if db.State != "" {
				state = fmt.Sprintf(" (%s on the target: %s)", db.SourceDB, db.State)
			}
			fmt.Fprintf(w, "  %s: %s → %s%s\n", db.SourceDB, src, db.TargetDB, state)
		}
		if dc := res.Disk; dc != nil && dc.Checked {
			fmt.Fprintf(w, "  disk: %s free at %s, about %s needed\n", humanBytes(dc.FreeBytes), dc.DataDirectory, humanBytes(dc.NeedBytes))
		}
		if res.Mode == restore.ModePreview {
			printPreviewBanner(w, res)
		}
		fmt.Fprintln(w, "Dry run: nothing was changed.")
		for _, wmsg := range res.Warnings {
			fmt.Fprintf(w, "warning: %s\n", wmsg)
		}
		return
	}
	var failed []string
	for _, db := range res.Databases {
		mark := "✓"
		if !db.OK {
			mark = "✗"
			failed = append(failed, db.SourceDB)
		}
		cmp := fmt.Sprintf("%d tables", db.TablesFound)
		if res.Source.File == "" {
			cmp = fmt.Sprintf("%d/%d tables", db.TablesFound, db.TablesExpected)
		}
		fmt.Fprintf(w, "%s %s → %s: loaded in %.1f s, %s\n", mark, db.SourceDB, db.TargetDB, db.RestoreSeconds, cmp)
		if db.Error != "" {
			fmt.Fprintf(w, "    %s\n", strings.ReplaceAll(db.Error, "\n", "\n    "))
		}
		if n := len(db.MissingTables); n > 0 {
			shown, more := db.MissingTables, ""
			if n > 10 {
				shown, more = shown[:10], fmt.Sprintf(" (+%d more)", n-10)
			}
			fmt.Fprintf(w, "    missing tables: %s%s\n", strings.Join(shown, ", "), more)
		}
		for _, m := range db.RowMismatches {
			kind := "rows"
			if m.Estimated {
				kind = "estimated rows"
			}
			note := ""
			if m.Extension != "" {
				note = fmt.Sprintf(" (table of extension %s, managed at runtime: warning only)", m.Extension)
			}
			fmt.Fprintf(w, "    %s: %s expected %d, found %d%s\n", m.Table, kind, m.Expected, m.Found, note)
		}
		for _, c := range db.Checks {
			if c.OK {
				fmt.Fprintf(w, "    ✓ check %s\n", c.Name)
			} else {
				fmt.Fprintf(w, "    ✗ check %s: %s\n", c.Name, c.Error)
			}
		}
		printAttributes(w, db.Attributes)
	}
	if len(res.Verify) > 0 {
		fmt.Fprintln(w, "✗ after the load the target no longer matches the backup:")
		printChecks(w, res.Verify, true)
	} else if res.Mode != restore.ModePreview && len(failed) == 0 {
		fmt.Fprintln(w, "✓ environment checked again after the load: same or more than the backup")
	}
	if ac := res.AppCheck; ac != nil {
		if ac.OK {
			fmt.Fprintf(w, "✓ the app's URL connects as %s (network %s) to %s and reads the %d of %d tables it has SELECT on\n", ac.User, ac.Network, ac.Database, ac.Readable, ac.Tables)
		} else {
			fmt.Fprintf(w, "✗ the app's URL: %s\n", strings.ReplaceAll(ac.Error, "\n", "\n    "))
		}
	}
	for _, db := range res.Databases {
		if db.State != restore.StateHasData || !db.OK || db.TargetDB == db.SourceDB {
			continue
		}
		today := restoreNow().UTC().Format("20060102")
		old := db.SourceDB + "_old_" + today
		fmt.Fprintf(w, "%s was restored as %s next to the current %s. To switch:\n", db.SourceDB, db.TargetDB, db.SourceDB)
		fmt.Fprintln(w, "  1. stop the app")
		fmt.Fprintf(w, "  2. psql: ALTER DATABASE %s RENAME TO %s;\n", pg.QuoteIdent(db.SourceDB), pg.QuoteIdent(old))
		fmt.Fprintf(w, "           ALTER DATABASE %s RENAME TO %s;\n", pg.QuoteIdent(db.TargetDB), pg.QuoteIdent(db.SourceDB))
		fmt.Fprintln(w, "  3. start the app")
		fmt.Fprintf(w, "  4. when sure: DROP DATABASE %s;\n", pg.QuoteIdent(old))
	}
	if res.Mode == restore.ModePreview {
		printPreviewBanner(w, res)
	}
	if res.DataDown != "" {
		fmt.Fprintf(w, "to empty the loaded databases again: %s\n", strings.Replace(res.Apply, "<file>", res.DataDown, 1))
	}
	if res.SQLDir != "" {
		fmt.Fprintf(w, "SQL files are in %s (env-up holds password hashes: delete the directory when done)\n", res.SQLDir)
	}
	if len(failed) > 0 && res.Source.File == "" {
		fmt.Fprintf(w, "to run again for the failed databases only: bracer restore %s\n", shellJoin(withDatabases(args, failed)))
	}
	for _, n := range res.Notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
	for _, wmsg := range res.Warnings {
		fmt.Fprintf(w, "warning: %s\n", wmsg)
	}
}

// restoreNow dates the old database in the swap steps; tests replace it.
var restoreNow = time.Now

// withDatabases replaces the --database flags in args with dbs.
func withDatabases(args, dbs []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		name := strings.TrimLeft(a, "-")
		if strings.HasPrefix(a, "-") && (name == "database") {
			i++ // its value
			continue
		}
		if strings.HasPrefix(a, "-") && strings.HasPrefix(name, "database=") {
			continue
		}
		out = append(out, a)
	}
	for _, db := range dbs {
		out = append(out, "--database", db)
	}
	return out
}

// shellJoin quotes args for a POSIX shell where needed.
func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if a != "" && strings.Trim(a, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./=:@,") == "" {
			out[i] = a
		} else {
			out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(out, " ")
}

// printAttributes lists what differs between the database attributes in the
// archive and the restored database (spec fetch-restore § Flow 8).
func printAttributes(w io.Writer, a *restore.AttrResult) {
	if a == nil {
		return
	}
	if a.Error != "" {
		fmt.Fprintf(w, "    %s\n", a.Error)
	}
	for _, m := range a.Mismatches {
		fmt.Fprintf(w, "    database attribute: %s\n", m)
	}
}
