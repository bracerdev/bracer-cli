package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/restore"
	"bracer.dev/bracer/internal/storage"
)

// runRestore is `bracer restore` (spec backup/restore, ADR-0022, ADR-0023):
// one attended session that starts from the bucket, never from an app
// directory on this machine. Every choice is asked; the only flag is --verbose.
func runRestore(ctx context.Context, args []string, d backupDeps) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	verbose := fs.Bool("verbose", false, "log every docker and pg_* command")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return exitUsage
	}
	fail := func(code int, format string, a ...interface{}) int {
		fmt.Fprintf(d.stderr, "bracer restore: "+format+"\n", a...)
		return code
	}
	if len(pos) > 0 {
		return fail(exitUsage, "restore takes no app name: it starts from the bucket")
	}
	if !isTerminal(d.stdin) || !isTerminal(d.stdout) {
		return fail(exitUsage, "restore needs a terminal")
	}
	logger := newLogger(d.stderr, *verbose)
	d = d.verbose(*verbose)
	// Docker, as step 0 of init: usable by this user; no systemd, no sudo.
	if it, _, _ := checkDocker(ctx, defaultEnvDeps(d.shell)); !it.OK {
		fmt.Fprintf(d.stderr, "bracer restore: %s\n", it.Detail)
		for _, f := range it.Fix {
			fmt.Fprintf(d.stderr, "Fix: %s\n", f)
		}
		return exitUsage
	}
	r := &restoreRun{ctx: ctx, d: d, p: newPrompter(d.stdin, d.stdout), w: d.stdout, log: logger}
	code, err := r.run()
	if err != nil {
		if errors.Is(err, ErrNoInput) || errors.Is(err, errQuit) {
			return fail(exitUsage, "nothing was loaded")
		}
		return fail(code, "%v", err)
	}
	return code
}

type restoreRun struct {
	ctx        context.Context
	d          backupDeps
	p          *prompter
	w          io.Writer
	log        *slog.Logger
	st         storage.Store
	bucketName string
	s          *restore.Session
	choice     restore.TargetChoice
}

func (r *restoreRun) printf(format string, a ...interface{}) { fmt.Fprintf(r.w, format, a...) }

func (r *restoreRun) run() (int, error) {
	folders, err := r.bucket()
	if err != nil {
		return exitUsage, err
	}
	f, err := r.pickApp(folders)
	if err != nil {
		return exitUsage, err
	}
	r.s = restore.NewSession(restore.Deps{Shell: r.d.shell, Store: r.st, Tools: r.d.tools(), Log: r.log}, f)
	if err := r.key(); err != nil {
		return exitUsage, err
	}
	if err := r.pickInstance(); err != nil {
		return exitUsage, err
	}
	if err := r.pickRun(); err != nil {
		return exitUsage, err
	}
	if err := r.untilReady(); err != nil {
		return exitUsage, err
	}
	appURL, err := r.appURL()
	if err != nil {
		return exitUsage, err
	}
	if err := r.confirm(); err != nil {
		return exitUsage, err
	}
	r.printf("loading the data…\n")
	out := r.s.Load(r.ctx, appURL)
	r.printOutcome(out)
	if !out.OK {
		return exitError, nil
	}
	return exitOK, nil
}

// bucket asks where the backups are and lists the app folders under
// bracer/ (ADR-0027); a wrong answer is shown as the bucket said it, then
// asked again.
func (r *restoreRun) bucket() ([]restore.Folder, error) {
	c := storage.S3Config{Region: "auto"}
	for {
		var err error
		if c.Endpoint, err = r.askDefault("S3 endpoint URL", c.Endpoint); err != nil {
			return nil, err
		}
		if c.Bucket, err = r.askDefault("Bucket", c.Bucket); err != nil {
			return nil, err
		}
		if c.Region, err = r.p.ask("Region", c.Region); err != nil {
			return nil, err
		}
		if c.PathStyle, err = r.p.yesNo("Path-style URLs (needed for MinIO/Garage)?", c.PathStyle); err != nil {
			return nil, err
		}
		if c.AccessKey, err = r.secretDefault("Access key", c.AccessKey); err != nil {
			return nil, err
		}
		if c.SecretKey, err = r.secretDefault("Secret key", c.SecretKey); err != nil {
			return nil, err
		}
		st, err := r.d.newStore(c)
		var folders []restore.Folder
		if err == nil {
			folders, err = restore.ListFolders(r.ctx, st)
		}
		if err == nil && len(folders) == 0 {
			err = fmt.Errorf("no bracer backups in %s (nothing under %s/)", c.Bucket, config.BucketPrefix)
		}
		if err == nil {
			r.st, r.bucketName = st, c.Bucket
			return folders, nil
		}
		r.printf("  ✗ %v\n", err)
		_, key, err := r.p.choose("[r] enter again  [q] quit", 0, "r", "q")
		if err != nil {
			return nil, err
		}
		if key == "q" {
			return nil, errQuit
		}
	}
}

// askDefault asks for a required value; Enter keeps def when there is one.
func (r *restoreRun) askDefault(prompt, def string) (string, error) {
	if def == "" {
		return r.p.askRequired(prompt)
	}
	return r.p.ask(prompt, def)
}

// secretDefault asks without echo; Enter keeps the value typed before.
func (r *restoreRun) secretDefault(prompt, def string) (string, error) {
	if def != "" {
		prompt += " (Enter keeps the one typed before)"
	}
	for {
		v, err := r.p.askSecret(prompt)
		if err != nil {
			return "", err
		}
		v = strings.TrimSpace(v)
		switch {
		case v != "":
			return v, nil
		case def != "":
			return def, nil
		}
	}
}

// pickApp lists the app folders with the place that writes each and how its
// newest run went (ADR-0023, ADR-0026).
func (r *restoreRun) pickApp(folders []restore.Folder) (restore.Folder, error) {
	r.printf("\nApps in %s:\n", r.bucketName)
	for i, f := range folders {
		if f.Err != nil {
			r.printf("  %d) %s  run.json unreadable: %v\n", i+1, f.Name, firstLine(f.Err.Error()))
			continue
		}
		n := f.Newest
		where := "-"
		if n.Writer.ID != "" {
			where = n.Writer.Hostname + ":" + n.Writer.Path
		}
		r.printf("  %d) %s  %s  latest run %s: %s\n", i+1, n.App, where, n.TS, n.Verdict())
	}
	for {
		i, _, err := r.p.choose(fmt.Sprintf("App to restore [1-%d]", len(folders)), len(folders))
		if err != nil {
			return restore.Folder{}, err
		}
		if folders[i].Err != nil {
			r.printf("%s cannot be restored: its newest run.json is unreadable\n", folders[i].Name)
			continue
		}
		return folders[i], nil
	}
}

// key asks for the age key when the backups are encrypted: the age.key the
// rescue card of init told the customer to copy.
func (r *restoreRun) key() error {
	newest := r.s.Newest()
	if !newest.Encrypted() {
		return nil
	}
	recipient := newest.Recipient()
	for {
		path, err := r.p.askRequired("Path of the age key file (age.key, as on the rescue card)")
		if err != nil {
			return err
		}
		if p, err := config.ExpandHome(path); err == nil {
			path = p
		}
		if err := r.s.UseKey(path, recipient); err != nil {
			r.printf("  ✗ %v\n", err)
			continue
		}
		return nil
	}
}

// pickInstance asks which instance of the newest run (spec restore AC).
func (r *restoreRun) pickInstance() error {
	newest := r.s.Newest()
	names := newest.InstanceNames()
	switch len(names) {
	case 0:
		return fmt.Errorf("the newest run of %s has no instance", newest.App)
	case 1:
		r.s.Instance = names[0]
		r.printf("instance: %s\n", names[0])
		return nil
	}
	r.printf("\nInstances:\n")
	for i, n := range names {
		r.printf("  %d) %s (%s)\n", i+1, n, orDash(newest.Instances[n].Kind))
	}
	i, _, err := r.p.choose(fmt.Sprintf("Instance to restore [1-%d]", len(names)), len(names))
	if err != nil {
		return err
	}
	r.s.Instance = names[i]
	return nil
}

// pickRun lists the runs, newest first; restore never picks one (ADR-0016).
func (r *restoreRun) pickRun() error {
	runs, err := r.s.ListRuns(r.ctx)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		return fmt.Errorf("no run of %s in the bucket can be restored for %s", r.s.Newest().App, r.s.Instance)
	}
	r.printf("\nRuns of %s (newest first):\n", r.s.Instance)
	for i, run := range runs {
		for j, line := range restore.RunLines(run) {
			if j == 0 {
				r.printf("  %2d) %s\n", i+1, line)
			} else {
				r.printf("      %s\n", line)
			}
		}
	}
	for {
		i, key, err := r.p.choose(fmt.Sprintf("Run to restore [1-%d] (q to quit)", len(runs)), len(runs), "q")
		if err != nil {
			return err
		}
		if key == "q" {
			return errQuit
		}
		if err := r.s.PickRun(r.ctx, runs[i].TS); err != nil {
			r.printf("  ✗ %v\n", err)
			continue
		}
		return nil
	}
}

// pickTarget lists the Postgres found on this machine as init does, and a URL.
func (r *restoreRun) pickTarget() error {
	r.printf("\nLooking for Postgres on this machine (Docker containers, sockets)...\n")
	containers, err := pg.Docker{Shell: r.d.shell}.ListPostgresContainers(r.ctx)
	if err != nil {
		r.printf("  ! %v\n", firstLine(err.Error()))
	}
	disc := pg.Discover(r.ctx, containers, pg.ScanHost(r.ctx, r.d.shell), pg.ProbeSSLRequest)
	var cands []pg.Candidate
	for _, c := range disc.Candidates {
		if c.Local != nil || c.Container != nil && c.Container.Running {
			cands = append(cands, c)
		}
	}
	for i, c := range cands {
		r.printf("  %d) %s\n", i+1, c.Label())
	}
	r.printf("  u) enter a URL\n")
	i, key, err := r.p.choose(fmt.Sprintf("Restore into [1-%d/u]", len(cands)), len(cands), "u")
	if err != nil {
		return err
	}
	switch {
	case key == "u":
		v, err := r.p.askSecret("Target URL (postgres://user:password@host:port/postgres, not shown, not stored)")
		if err != nil {
			return err
		}
		r.choice = restore.TargetChoice{URL: strings.TrimSpace(v)}
	case cands[i].Container != nil:
		r.choice = restore.TargetChoice{Container: cands[i].Container.Name}
	default:
		l := *cands[i].Local
		r.choice = restore.TargetChoice{Local: &l}
	}
	return nil
}

// connect connects to the chosen target, asking for its password when the
// socket wants one.
func (r *restoreRun) connect() error {
	for {
		err := r.s.Connect(r.ctx, r.choice)
		if !errors.Is(err, restore.ErrNeedPassword) {
			return err
		}
		pw, aerr := r.p.askSecret("The target asks for a password. Password of its superuser (not shown, not stored)")
		if aerr != nil {
			return aerr
		}
		r.choice.Password = pw
	}
}

// untilReady picks a target and compares it with the run until nothing
// blocks: the customer applies the fixes in another terminal and presses
// Enter (spec restore § compare).
func (r *restoreRun) untilReady() error {
	for {
		if err := r.pickTarget(); err != nil {
			return err
		}
		if err := r.connect(); err != nil {
			r.printf("  ✗ %v\n", err)
			continue
		}
		t := r.s.Target
		role := "superuser"
		if !t.Superuser {
			role = "not superuser"
		}
		r.printf("target: %s, PostgreSQL %s, user %s (%s)\n", t.Label, t.Version, t.User, role)
		ready, err := r.compareLoop()
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
	}
}

// compareLoop compares until the target is ready (true), or the target
// cannot take the run and another must be picked (false).
func (r *restoreRun) compareLoop() (bool, error) {
	first := true
	for {
		plan, err := r.s.Compare(r.ctx)
		var pt *restore.ErrPickTarget
		if errors.As(err, &pt) {
			r.printf("  ✗ %s\n", pt.Msg)
			return false, nil
		}
		if err == nil {
			if first && r.s.Mode() == restore.ModeData {
				r.printf("this backup has databases only (source was a URL): restoring data owned by %s, without users or privileges\n", r.s.Target.User)
			}
			first = false
			printChecks(r.w, plan.Checks)
			for _, n := range plan.Notes {
				r.printf("note: %s\n", n)
			}
			if plan.OK() {
				return true, nil
			}
			if err := r.printFixes(plan); err != nil {
				return false, err
			}
		} else {
			r.printf("  ✗ %v\n", err)
		}
		r.printf("Apply the fixes above in another terminal, then press Enter to check again (q to quit): ")
		for {
			line, err := r.p.line()
			if err != nil {
				return false, err
			}
			if strings.TrimSpace(line) == "q" {
				return false, errQuit
			}
			// Connect again: the target may have been recreated or restarted.
			if err := r.connect(); err != nil {
				r.printf("  ✗ %v\nPress Enter to check again (q to quit): ", err)
				continue
			}
			break
		}
	}
}

// printFixes writes the SQL files when the plan has SQL and says how to apply them.
func (r *restoreRun) printFixes(plan *restore.Plan) error {
	if len(plan.Up) == 0 {
		return nil
	}
	up, down, fresh, err := r.s.WriteFixes()
	if err != nil {
		return fmt.Errorf("write the SQL files: %w", err)
	}
	if fresh {
		r.printf("SQL for the target (read it first):\n")
	} else {
		r.printf("Same SQL as before, not applied yet:\n")
	}
	r.printf("  env-up:   %s\n  env-down: %s (puts the target back as it was)\n", up, down)
	r.printf("apply with: %s\n", strings.Replace(r.s.Apply(), "<file>", up, 1))
	if plan.Restart {
		r.printf("then restart: %s\n", plan.RestartCmd)
	}
	if r.s.Mode() == restore.ModeFull {
		r.printf("env-up may hold password hashes: delete %s when done\n", r.s.SQLDir())
	}
	return nil
}

// appURL asks for the app's URL as in its config: the restore must prove the
// app connects (ADR-0016). Never stored, never logged.
func (r *restoreRun) appURL() (string, error) {
	for {
		v, err := r.p.askSecret("URL the app connects with, as in its config (postgres://user:password@host:port/db)")
		if err != nil {
			return "", err
		}
		v = strings.TrimSpace(v)
		switch {
		case v == "q":
			return "", errQuit
		case v == "":
			r.printf("the app check needs the app's URL (q to quit)\n")
			continue
		}
		if _, err := pg.ParseURL(v); err != nil {
			r.printf("  ✗ %v\n", err)
			continue
		}
		return v, nil
	}
}

func (r *restoreRun) confirm() error {
	r.printf("\n")
	for i, dp := range r.s.Plan.DBs {
		r.printf("  %s → %s, %s\n", dp.Source, dp.Target, humanBytes(r.s.Run.DBs[i].Bytes))
	}
	ok, err := r.p.yesNo(fmt.Sprintf("Load %d databases into %s now?", len(r.s.Plan.DBs), r.s.Target.Label), false)
	if err != nil {
		return err
	}
	if !ok {
		return errQuit
	}
	return nil
}

// printOutcome prints one line per database, the proof, then how to switch
// the app over and how to undo (spec restore § result).
func (r *restoreRun) printOutcome(out *restore.Outcome) {
	for _, db := range out.DBs {
		mark := "✓"
		if !db.OK {
			mark = "✗"
		}
		r.printf("%s %s → %s: loaded in %.1f s, %d/%d tables\n", mark, db.Source, db.Target, db.Seconds, db.TablesFound, db.TablesExpected)
		if db.Error != "" {
			r.printf("    %s\n", strings.ReplaceAll(db.Error, "\n", "\n    "))
		}
		if n := len(db.MissingTables); n > 0 {
			shown, more := db.MissingTables, ""
			if n > 10 {
				shown, more = shown[:10], fmt.Sprintf(" (+%d more)", n-10)
			}
			r.printf("    missing tables: %s%s\n", strings.Join(shown, ", "), more)
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
			r.printf("    %s: %s expected %d, found %d%s\n", m.Table, kind, m.Expected, m.Found, note)
		}
	}
	if len(out.Verify) > 0 {
		r.printf("✗ after the load the target no longer matches the backup:\n")
		printChecks(r.w, out.Verify)
	}
	if ac := out.App; ac != nil {
		if ac.OK {
			r.printf("✓ the app's URL connects as %s (network %s) to %s and reads %d of %d tables (those it has SELECT on, in schemas it has USAGE on)\n", ac.User, ac.Network, ac.Database, ac.Readable, ac.Tables)
		} else {
			r.printf("✗ the app's URL: %s\n", strings.ReplaceAll(ac.Error, "\n", "\n    "))
		}
	}
	var kept []*restore.DBResult
	for _, db := range out.DBs {
		if db.Kept {
			kept = append(kept, db)
		}
	}
	if out.OK {
		stamp := time.Now().UTC().Format("20060102_1504Z")
		r.printf("\nTo switch the app over, either:\n")
		r.printf("  1. stop the app, then in psql:\n")
		for _, db := range out.DBs {
			if r.s.TargetHas(db.Source) {
				r.printf("       ALTER DATABASE %s RENAME TO %s;\n", pg.QuoteIdent(db.Source), pg.QuoteIdent(db.Source+"_old_"+stamp))
			}
			r.printf("       ALTER DATABASE %s RENAME TO %s;\n", pg.QuoteIdent(db.Target), pg.QuoteIdent(db.Source))
		}
		r.printf("     and start the app again;\n")
		r.printf("  2. or point the app's config at the new databases.\n")
	}
	if len(kept) > 0 {
		r.printf("\nTo undo, before renaming anything:\n")
		for _, db := range kept {
			r.printf("  %s\n", db.Drop)
		}
		if downs := r.s.EnvDowns(); len(downs) > 0 {
			for _, f := range downs {
				r.printf("  %s\n", strings.Replace(r.s.Apply(), "<file>", f, 1))
			}
		}
	}
	if _, err := os.Stat(r.s.SQLDir()); err == nil {
		r.printf("SQL files are in %s (env-up holds password hashes: delete the directory when done)\n", r.s.SQLDir())
	}
	if app := r.s.Newest().App; !containsStr(config.KnownApps(userHome()), app) {
		r.printf("to back up this server: bracer init %s\n", app)
	}
}

// printChecks lists the comparison: one line per check, the fix under a ✗.
func printChecks(w io.Writer, checks []restore.Check) {
	for _, c := range checks {
		mark := "✓"
		switch {
		case !c.OK:
			mark = "✗"
		case c.Warn:
			mark = "!"
		}
		line := fmt.Sprintf("%s %s %s", mark, c.Area, c.Name)
		if c.Detail != "" {
			line += ": " + c.Detail
		}
		fmt.Fprintln(w, line)
		switch {
		case c.OK:
		case c.Fix == restore.FixSQL && c.Manual != "":
			fmt.Fprintf(w, "    fix: in env-up; %s\n", c.Manual)
		case c.Fix == restore.FixSQL:
			fmt.Fprintln(w, "    fix: in env-up")
		case c.Manual != "":
			fmt.Fprintf(w, "    fix: %s\n", c.Manual)
		}
	}
}

func isTerminal(f interface{}) bool {
	file, ok := f.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func printJSON(w io.Writer, v interface{}) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
