package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/drill"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/storage"
)

// backupDeps is everything `backup` touches outside the process; tests replace it with fakes.
type backupDeps struct {
	shell    pg.Shell
	newStore func(storage.S3Config) (storage.Store, error)
	http     *http.Client
	stdin    io.Reader // restore asks for confirmation
	stdout   io.Writer
	stderr   io.Writer
}

func defaultBackupDeps(stdin io.Reader, stdout, stderr io.Writer) backupDeps {
	return backupDeps{
		shell:    pg.ExecShell{},
		newStore: func(c storage.S3Config) (storage.Store, error) { return storage.NewS3(c) },
		http:     &http.Client{Timeout: 15 * time.Second},
		stdin:    stdin,
		stdout:   stdout,
		stderr:   stderr,
	}
}

// tools builds pg.Tools from the current shell (after --verbose and runtime: are applied).
func (d backupDeps) tools() *pg.Tools {
	return pg.NewTools(d.shell)
}

// withRuntime applies app.yaml's runtime: to the real shell: the absolute
// docker path, PATH and DOCKER_HOST, over the process environment
// (ADR-0022). Docker missing at that path is an error, never a lookup in
// PATH. Fake shells in tests are left alone.
func (d backupDeps) withRuntime(cfg *config.App) (backupDeps, error) {
	sh, ok := d.shell.(pg.ExecShell)
	if !ok {
		return d, nil
	}
	rt := cfg.Runtime
	if _, err := os.Stat(rt.Docker); err != nil {
		return d, fmt.Errorf("docker not found at %s (runtime.docker)\nFix: %s", rt.Docker, backup.FixReinit(cfg.App))
	}
	sh.Docker = rt.Docker
	sh.Env = []string{"PATH=" + rt.Path}
	if rt.DockerHost != "" {
		sh.Env = append(sh.Env, "DOCKER_HOST="+rt.DockerHost)
	}
	d.shell = sh
	return d, nil
}

// loadApp reads app.yaml (or the draft) of a job, applies runtime: and
// loads its secrets: what every job does before it touches anything.
// A config that cannot be read is exitUsage; Docker missing is exitError.
func loadApp(path string, d backupDeps, logger *slog.Logger) (*config.App, *config.Secrets, backupDeps, int, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, nil, d, exitUsage, err
	}
	secrets, warns, err := config.LoadSecrets(cfg)
	if err != nil {
		return nil, nil, d, exitUsage, fmt.Errorf("%v\nFix: %s", err, backup.FixReinit(cfg.App))
	}
	for _, w := range warns {
		logger.Warn(w)
	}
	var missing []string
	for _, in := range cfg.Instances {
		for _, name := range []string{in.PasswordEnv, in.URLEnv} {
			if name != "" {
				if _, err := secrets.Require(name); err != nil {
					missing = append(missing, name)
				}
			}
		}
	}
	for _, name := range []string{cfg.Storage.AccessKeyEnv, cfg.Storage.SecretKeyEnv} {
		if _, err := secrets.Require(name); err != nil {
			missing = append(missing, name)
		}
	}
	if cfg.Hosted != nil {
		if _, err := secrets.Require(cfg.Hosted.TokenEnv); err != nil {
			missing = append(missing, cfg.Hosted.TokenEnv)
		}
	}
	if name := cfg.HeartbeatURLEnv; name != "" {
		if _, err := secrets.Require(name); err != nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, nil, d, exitUsage, fmt.Errorf("missing in %s: %s\nFix: %s", cfg.SecretsFile, strings.Join(missing, ", "), backup.FixReinit(cfg.App))
	}
	d, err = d.withRuntime(cfg)
	if err != nil {
		return nil, nil, d, exitError, err
	}
	return cfg, secrets, d, exitOK, nil
}

// runBackup is `bracer backup` (spec backup/backup): one run folder, its
// drill, run.json, retention.
func runBackup(ctx context.Context, args []string, d backupDeps) (exit int) {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	dryRun := fs.Bool("dry-run", false, "check everything a backup needs; dump the schema only, into memory")
	asJSON := fs.Bool("json", false, "print the result as JSON on stdout")
	verbose := fs.Bool("verbose", false, "log every docker and pg_* command")
	initDraft := fs.Bool("init-draft", false, "internal (bracer init): read the app's draft instead of its app.yaml")
	cfgPath, code, err := selectAppOrDraft(fs, args, userHome(), "backup", initDraft)
	if code != exitOK {
		if err != nil {
			fmt.Fprintf(d.stderr, "bracer backup: %v\n", err)
		}
		return code
	}
	// name is the app's app.yaml (Fix lines, locks, the place); cfgPath what is read.
	name := cfgPath
	if *initDraft {
		cfgPath = draftPath(name)
	}
	logger := newLogger(d.stderr, *verbose)
	d = d.verbose(*verbose)
	fail := func(code int, format string, a ...interface{}) int {
		fmt.Fprintf(d.stderr, "bracer backup: "+format+"\n", a...)
		return code
	}

	cfg, secrets, d, code, err := loadApp(cfgPath, d, logger)
	if err != nil {
		return fail(code, "%v", err)
	}
	// The one signal of the night: /start now, /<exit> at the end, the drill
	// included (ADR-0025, ADR-0026).
	var warnings []string
	if !*dryRun {
		hb := newHeartbeat(cfg, secrets, d, logger)
		hb.send(ctx, "start", "")
		defer func() { hb.finish(ctx, exit, warnings) }()
	}
	here, err := backup.Here(cfg.Storage.Folder, name)
	if err != nil {
		return fail(exitError, "%v", err)
	}
	// init's first real run starts the folder; it has no app.yaml yet (ADR-0023).
	_, statErr := os.Stat(name)
	create := *initDraft && os.IsNotExist(statErr)
	// One lock for the whole run, the drill included; the preflight takes it
	// too, since its drill removes the app's leftover temporary containers.
	lock, err := backup.Acquire(name)
	if err != nil {
		return fail(exitError, "%v", err)
	}
	defer lock.Release()
	st, err := openStore(cfg, secrets, d)
	if err != nil {
		return fail(exitError, "%v", err)
	}

	if *dryRun {
		return dryRunBackup(ctx, cfg, secrets, st, d, here, create, *asJSON, logger)
	}

	var dres *drill.Result
	step := drill.Step(cfg, drillDeps(d, st, logger), func(r *drill.Result) { dres = r })
	res, err := backup.Backup(ctx, cfg, backup.Deps{Shell: d.shell, Store: st, Secrets: secrets, Tools: d.tools(), Log: logger, Here: here, Create: create, Drill: step})
	if err != nil {
		report(ctx, cfg, secrets, d, logger, failureReport("backup", cfg.App, err))
		return fail(exitError, "%v", err)
	}
	warnings = res.Warnings
	if *asJSON {
		printJSON(d.stdout, res)
	} else {
		printResult(d.stdout, res, dres)
	}
	report(ctx, cfg, secrets, d, logger, res)
	return exitOf(res.Status)
}

// drillDeps is the drill step's view of the world.
func drillDeps(d backupDeps, st storage.Store, logger *slog.Logger) drill.Deps {
	return drill.Deps{Shell: d.shell, Tools: d.tools(), Store: st, Log: logger}
}

// exitOf is the exit code of a run result (ADR-0022, ADR-0029).
func exitOf(status string) int {
	if status == backup.ResultOK {
		return exitOK
	}
	return exitError
}

// newLogger logs to w, every command with verbose.
func newLogger(w io.Writer, verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	return logger
}

// verbose turns on command logging of the real shell.
func (d backupDeps) verbose(on bool) backupDeps {
	if sh, ok := d.shell.(pg.ExecShell); ok {
		sh.Verbose = on
		d.shell = sh
	}
	return d
}

// openStore opens the bucket of app.yaml; loadApp has checked its keys.
func openStore(cfg *config.App, secrets *config.Secrets, d backupDeps) (storage.Store, error) {
	ak, _ := secrets.Require(cfg.Storage.AccessKeyEnv)
	sk, _ := secrets.Require(cfg.Storage.SecretKeyEnv)
	return d.newStore(storage.S3Config{Endpoint: cfg.Storage.Endpoint, Bucket: cfg.Storage.Bucket, Region: cfg.Storage.Region, AccessKey: ak, SecretKey: sk, PathStyle: cfg.Storage.PathStyle})
}

// preflightResult is `backup --dry-run` (spec backup AC --dry-run, ADR-0019):
// one line per check, grouped by section. init reads it as JSON to tell which
// instance's drill failed.
type preflightResult struct {
	Kind  string          `json:"kind"`
	App   string          `json:"app"`
	OK    bool            `json:"ok"`
	Lines []preflightLine `json:"lines"`
}

// preflightLine is one line; Mark is ✓ ✗ ! - or empty (information).
type preflightLine struct {
	Section  string `json:"section"`            // storage, encryption, instance <name>, drill
	Instance string `json:"instance,omitempty"` // the instance the line is about
	Area     string `json:"area,omitempty"`     // bucket, age, connect, database, env, drill
	Mark     string `json:"mark,omitempty"`
	Text     string `json:"text"`
	Fix      string `json:"fix,omitempty"`
}

func (p *preflightResult) add(l preflightLine) {
	if l.Mark == "✗" {
		p.OK = false
	}
	p.Lines = append(p.Lines, l)
}

// drillFailed lists the instances whose drill failed in the preflight.
func (p *preflightResult) drillFailed() []string {
	var out []string
	for _, l := range p.Lines {
		if l.Area == "drill" && l.Mark == "✗" && l.Instance != "" && !containsStr(out, l.Instance) {
			out = append(out, l.Instance)
		}
	}
	return out
}

// otherFailed reports a failed line that is not the drill of one instance.
func (p *preflightResult) otherFailed() bool {
	for _, l := range p.Lines {
		if l.Mark == "✗" && (l.Area != "drill" || l.Instance == "") {
			return true
		}
	}
	return false
}

func printPreflight(w io.Writer, p *preflightResult) {
	fmt.Fprintf(w, "app %s\n", p.App)
	section := ""
	for _, l := range p.Lines {
		if l.Section != section {
			section = l.Section
			fmt.Fprintf(w, "\n%s\n", section)
		}
		if l.Mark == "" {
			fmt.Fprintf(w, "  %s\n", indentMore(l.Text))
		} else {
			fmt.Fprintf(w, "  %s %s\n", l.Mark, indentMore(l.Text))
		}
		if l.Fix != "" {
			fmt.Fprintf(w, "    Fix: %s\n", l.Fix)
		}
	}
	fmt.Fprintf(w, "\nDry run: only the schema was dumped, into memory; nothing was uploaded or pinged.\n")
}

// dryRunBackup is the preflight: everything a backup needs except the data.
// It writes, reads and deletes the probe object, round-trips the age key,
// makes a schema-only run into memory and drills it. Nothing else is written
// to the bucket; nothing is pinged.
func dryRunBackup(ctx context.Context, cfg *config.App, secrets *config.Secrets, st storage.Store, d backupDeps, here backup.Writer, create, asJSON bool, logger *slog.Logger) int {
	p := runPreflight(ctx, cfg, secrets, st, d, here, create, logger)
	if asJSON {
		printJSON(d.stdout, p)
	} else {
		printPreflight(d.stdout, p)
	}
	if !p.OK {
		return exitError
	}
	return exitOK
}

func runPreflight(ctx context.Context, cfg *config.App, secrets *config.Secrets, st storage.Store, d backupDeps, here backup.Writer, create bool, logger *slog.Logger) *preflightResult {
	p := &preflightResult{Kind: "backup-dry-run", App: cfg.App, OK: true}
	sec := "storage: " + st.Describe()
	p.add(preflightLine{Section: sec, Text: "secrets_file " + cfg.SecretsFile})
	// The folder first: nothing is written, not even the probe, to a folder
	// that has no run or another place's (ADR-0023).
	folder := preflightLine{Section: sec, Area: "folder", Mark: "✓"}
	var newest *backup.Run
	if err := st.Check(ctx); err != nil {
		folder.Mark, folder.Text = "✗", err.Error()
		p.add(folder)
		return p
	}
	if create {
		folder.Text = "folder " + cfg.Storage.Folder + ": started by the first backup"
	} else if prev, _, err := backup.OpenFolder(ctx, st, cfg, here, false); err != nil {
		folder.Mark, folder.Text = "✗", err.Error()
		p.add(folder)
		return p
	} else {
		newest = prev
		folder.Text = "folder " + backup.Root(cfg) + ", written from here; newest run " + prev.TS
	}
	p.add(folder)
	probe := backup.ProbeKey(cfg.Storage.Folder)
	bucket := preflightLine{Section: sec, Area: "bucket", Mark: "✗"}
	if err := backup.Probe(ctx, st, probe, time.Now()); errors.Is(err, backup.ErrNoDelete) {
		bucket.Text, bucket.Fix = err.Error(), "allow the access key to delete objects in the bucket (s3:DeleteObject)"
	} else if err != nil {
		bucket.Text, bucket.Fix = err.Error(), "allow the access key to write and read objects under "+backup.Root(cfg)
	} else {
		bucket.Mark, bucket.Text = "✓", "wrote, read back and deleted "+probe
	}
	p.add(bucket)
	for _, name := range []string{cfg.Storage.AccessKeyEnv, cfg.Storage.SecretKeyEnv} {
		p.add(preflightLine{Section: sec, Text: name + " from " + secrets.Source[name]})
	}
	switch cfg.Encryption {
	case "age":
		sec = fmt.Sprintf("encryption: age, recipient %s, identity %s", cfg.AgeRecipient, cfg.AgeIdentityFile)
		l := preflightLine{Section: sec, Area: "age", Mark: "✓", Text: "a sample encrypted to the recipient decrypts with age_identity_file"}
		if err := backup.AgeRoundTrip(cfg); err != nil {
			l.Mark, l.Text, l.Fix = "✗", err.Error(), "put the private key of "+cfg.AgeRecipient+" back into "+cfg.AgeIdentityFile
		}
		p.add(l)
	default:
		p.add(preflightLine{Section: "encryption: none", Text: "backups stored in clear"})
	}
	if newest != nil {
		var orphans []string
		for name := range newest.Instances {
			if cfg.FindInstance(name) == nil {
				orphans = append(orphans, name)
			}
		}
		sort.Strings(orphans)
		if len(orphans) > 0 {
			logger.Warn("the newest run has instances that are not in app.yaml (renamed or removed); their old runs stay until retention", "instances", strings.Join(orphans, ", "))
		}
	}

	// The run itself, schema only, into memory, drilled like a real one.
	mem := storage.NewMemory(backup.PreflightMaxBytes)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	var dres *drill.Result
	step := func(ctx context.Context, run *backup.Run, res *backup.Result) {
		// An instance whose backup failed above is not drilled.
		var skip []string
		for _, ir := range res.Instances {
			if !ir.OK {
				skip = append(skip, ir.Name)
			}
		}
		dd := drill.Deps{Shell: d.shell, Tools: d.tools(), Store: mem, Log: quiet, SkipInstances: skip}
		drill.Step(cfg, dd, func(r *drill.Result) { dres = r })(ctx, run, res)
	}
	res, err := backup.Backup(ctx, cfg, backup.Deps{Shell: d.shell, Store: mem, Secrets: secrets, Tools: d.tools(), Log: quiet, SchemaOnly: true, Drill: step})
	if err != nil {
		p.add(preflightLine{Section: "backup", Area: "backup", Mark: "✗", Text: err.Error()})
		return p
	}
	var drillOn []string
	for _, ir := range res.Instances {
		in := cfg.FindInstance(ir.Name)
		sec := "instance " + ir.Name
		add := func(area, mark, format string, a ...interface{}) {
			p.add(preflightLine{Section: sec, Instance: ir.Name, Area: area, Mark: mark, Text: fmt.Sprintf(format, a...)})
		}
		for _, name := range []string{in.PasswordEnv, in.URLEnv} {
			if name != "" {
				add("", "", "%s from %s", name, secrets.Source[name])
			}
		}
		if ir.Runner != "" {
			add("", "", "runner: %s", ir.Runner)
		}
		if ir.Error != "" {
			add("connect", "✗", "%s", ir.Error)
			continue
		}
		add("connect", "✓", "PostgreSQL %s, %d database(s)", ir.PgVersion, len(ir.Databases))
		for _, dr := range ir.Databases {
			if dr.OK {
				add("database", "✓", "%s: schema dumped (%s)", dr.Name, humanBytes(dr.Bytes))
			} else {
				add("database", "✗", "%s: %s", dr.Name, dr.Error)
			}
		}
		if e := ir.Env; e != nil && !e.OK {
			add("env", "✗", "environment: %s", e.Error)
		}
		if g := ir.Globals; g != nil && !g.OK {
			add("env", "✗", "globals: %s", g.Error)
		}
		if ir.Env != nil && ir.Env.OK && (ir.Globals == nil || ir.Globals.OK) {
			what := "environment"
			if ir.Globals != nil {
				what += " and globals"
			}
			add("env", "✓", "%s read", what)
		}

		switch {
		case in.DrillOff():
			add("drill", "-", "drill: off for this instance (not test-restored)")
		case ir.OK:
			drillOn = append(drillOn, ir.Name)
		default:
			add("drill", "-", "drill: not tried (the backup above fails)")
		}
	}
	preflightDrill(cfg, res, dres, drillOn, p)
	for _, msg := range res.Warnings {
		p.add(preflightLine{Section: "warnings", Mark: "!", Text: msg})
	}
	return p
}

// preflightDrill adds the drill of the schema-only run, one line per
// instance drilled (spec backup AC preflight).
func preflightDrill(cfg *config.App, res *backup.Result, dres *drill.Result, names []string, p *preflightResult) {
	const sec = "drill (the schema-only run, in a temporary Postgres)"
	for _, name := range names {
		ir := res.Instance(name)
		var x *drill.InstanceResult
		if dres != nil {
			x = dres.Instance(name)
		}
		l := preflightLine{Section: sec, Instance: name, Area: "drill", Mark: "✓"}
		switch {
		case len(ir.DrillFailed) > 0:
			image := "-"
			if x != nil {
				image = orDash(x.Image)
			}
			l.Mark, l.Text = "✗", fmt.Sprintf("%s (image %s): %s", name, image, strings.Join(ir.DrillFailed, "; "))
			l.Fix = "choose an image that has it for instance " + name + ", or turn its drill off: " + backup.FixReinit(cfg.App)
		case x != nil:
			l.Text = fmt.Sprintf("%s: image %s, %d database(s) loaded", name, x.Image, len(x.Databases))
			for _, n := range x.Notes {
				l.Text += "\nnote: " + n
			}
		default:
			l.Text = name + ": test-restored"
		}
		p.add(l)
	}
}

func indentMore(s string) string { return strings.ReplaceAll(s, "\n", "\n    ") }

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// printResult prints one line per database of the backup and of the drill
// (NFR observability).
func printResult(w io.Writer, res *backup.Result, dres *drill.Result) {
	for _, ir := range res.Instances {
		if ir.Error != "" {
			fmt.Fprintf(w, "✗ %s: %s\n", ir.Name, ir.Error)
			continue
		}
		for _, dr := range ir.Databases {
			if dr.OK {
				fmt.Fprintf(w, "✓ %s/%s: %s, %d tables, %.1f s\n", ir.Name, dr.Name, humanBytes(dr.Bytes), dr.Tables, dr.Seconds)
			} else {
				fmt.Fprintf(w, "✗ %s/%s: %s\n", ir.Name, dr.Name, dr.Error)
			}
		}
		if g := ir.Globals; g != nil {
			if g.OK {
				note := ""
				if !g.HasPasswords {
					note = " (users without passwords)"
				}
				fmt.Fprintf(w, "✓ %s/globals: %s%s\n", ir.Name, humanBytes(g.Bytes), note)
			} else {
				fmt.Fprintf(w, "✗ %s/globals: %s\n", ir.Name, g.Error)
			}
		}
		if len(ir.Missing) > 0 {
			fmt.Fprintf(w, "✗ %s: this run is missing %s\n", ir.Name, strings.Join(ir.Missing, ", "))
		}
	}
	printDrill(w, res, dres)
	if res.Error != "" {
		fmt.Fprintf(w, "✗ %s\n", res.Error)
	}
	for _, wmsg := range res.Warnings {
		fmt.Fprintf(w, "warning: %s\n", wmsg)
	}
}

// printDrill prints the drill of each instance: one line per database
// restored, or why it failed.
func printDrill(w io.Writer, res *backup.Result, dres *drill.Result) {
	for _, ir := range res.Instances {
		var x *drill.InstanceResult
		if dres != nil {
			x = dres.Instance(ir.Name)
		}
		switch {
		case len(ir.DrillFailed) > 0:
			fmt.Fprintf(w, "✗ %s: backup uploaded; drill failed: %s\n", ir.Name, indentMore(strings.Join(ir.DrillFailed, "; ")))
			if x != nil {
				for _, n := range x.Notes {
					fmt.Fprintf(w, "  note: %s\n", n)
				}
			}
		case x == nil:
			if ir.Error == "" {
				fmt.Fprintf(w, "- %s: not test-restored (drill off)\n", ir.Name)
			}
		default:
			for _, dr := range x.Databases {
				fmt.Fprintf(w, "✓ %s/%s: restored %s in %.1f s, %d/%d tables\n", ir.Name, dr.DB, humanBytes(dr.Bytes), dr.RestoreSeconds, dr.TablesFound, dr.TablesExpected)
			}
			for _, n := range x.Notes {
				fmt.Fprintf(w, "  note: %s\n", n)
			}
		}
	}
}

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

// report sends POST /report when app.yaml has hosted: (spec backup § Flow 8,
// received per spec monitor/backup-report). body is the Result of the run;
// if it could not start, failureReport is sent. The body also carries the
// schedule (so hosted knows when the next run is due) and the hostname.
// Network errors only warn, they do not change the exit code.
func report(ctx context.Context, cfg *config.App, secrets *config.Secrets, d backupDeps, logger *slog.Logger, body any) {
	h := cfg.Hosted
	if h == nil || d.http == nil {
		return
	}
	token, err := secrets.Require(h.TokenEnv)
	if err != nil {
		logger.Warn("hosted: token missing, result not reported", "env", h.TokenEnv)
		return
	}
	data, err := reportBody(cfg, body)
	if err != nil {
		logger.Warn("hosted: cannot encode result", "err", err)
		return
	}
	url := strings.TrimRight(h.Server, "/") + "/report"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		logger.Warn("hosted", "err", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "bracer/"+Version)
	resp, err := d.http.Do(req)
	if err != nil {
		logger.Warn("hosted: report failed", "err", err)
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		logger.Warn("hosted: report rejected", "status", resp.Status)
	}
}

// reportBody is the run's JSON plus schedule and hostname.
func reportBody(cfg *config.App, body any) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	m["schedule"] = cfg.Schedule
	m["hostname"], _ = os.Hostname()
	return json.Marshal(m)
}

// failureReport is the minimal body when the command could not start.
func failureReport(kind, app string, err error) map[string]any {
	return map[string]any{"kind": kind, "app": app, "status": backup.ResultFailed, "ok": false, "error": err.Error()}
}
