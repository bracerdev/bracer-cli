package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/storage"
)

// backupDeps is everything `backup` touches outside the process; tests replace it with fakes.
type backupDeps struct {
	shell    pg.Shell
	newStore func(storage.S3Config) (storage.Store, error)
	http     *http.Client
	goos     string
	lookPath func(string) (string, error) // nil: exec.LookPath; tests block host binaries
	glob     func(string) ([]string, error)
	stdin    io.Reader // restore --drop asks for confirmation
	stdout   io.Writer
	stderr   io.Writer
}

func defaultBackupDeps(stdin io.Reader, stdout, stderr io.Writer) backupDeps {
	return backupDeps{
		shell:    pg.ExecShell{},
		newStore: func(c storage.S3Config) (storage.Store, error) { return storage.NewS3(c) },
		http:     &http.Client{Timeout: 15 * time.Second},
		goos:     runtime.GOOS,
		stdin:    stdin,
		stdout:   stdout,
		stderr:   stderr,
	}
}

// tools builds pg.Tools from the current shell (after --verbose is applied).
func (d backupDeps) tools() *pg.Tools {
	return &pg.Tools{Shell: d.shell, GOOS: d.goos, LookPath: d.lookPath, Glob: d.glob}
}

// runBackup is `bracer backup` (spec backup/backup).
func runBackup(ctx context.Context, args []string, d backupDeps) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	cfgPath := fs.String("c", "app.yaml", "path of app.yaml")
	envFile := fs.String("env-file", "", "dotenv file with secrets (overrides secrets_file, overridden by the environment)")
	dryRun := fs.Bool("dry-run", false, "check config, connections and bucket; dump nothing")
	asJSON := fs.Bool("json", false, "print the result as JSON on stdout")
	verbose := fs.Bool("verbose", false, "log every docker, kubectl and pg_* command")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(d.stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	if sh, ok := d.shell.(pg.ExecShell); ok {
		sh.Verbose = *verbose
		d.shell = sh
	}
	fail := func(code int, format string, a ...interface{}) int {
		fmt.Fprintf(d.stderr, "bracer backup: "+format+"\n", a...)
		return code
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	secrets, warns, err := config.LoadSecrets(cfg, *envFile)
	if err != nil {
		return fail(exitUsage, "%v", err)
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
	st, err := openStore(cfg, secrets, d, *cfgPath, missing)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}

	if *dryRun {
		return dryRunBackup(ctx, cfg, secrets, st, d, *cfgPath, logger)
	}

	lock, err := backup.Acquire(*cfgPath)
	if err != nil {
		return fail(exitError, "%v", err)
	}
	defer lock.Release()

	res, err := backup.Run(ctx, cfg, backup.Deps{Shell: d.shell, Store: st, Secrets: secrets, GOOS: d.goos, Tools: d.tools(), Log: logger})
	if err != nil {
		report(ctx, cfg, secrets, d, logger, failureReport("backup", cfg.App, err))
		return fail(exitError, "%v", err)
	}
	if *asJSON {
		enc := json.NewEncoder(d.stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	} else {
		printResult(d.stdout, res)
	}
	report(ctx, cfg, secrets, d, logger, res)
	if !res.OK {
		return exitError
	}
	return exitOK
}

// openStore checks storage: and the bucket keys, then opens the Store. missing is
// the other secrets already missing, merged into the same message.
func openStore(cfg *config.App, secrets *config.Secrets, d backupDeps, cfgPath string, missing []string) (storage.Store, error) {
	if cfg.Storage == nil {
		return nil, fmt.Errorf("storage: is not set in %s; run bracer init", cfgPath)
	}
	ak, err := secrets.Require(cfg.Storage.AccessKeyEnv)
	if err != nil {
		missing = append(missing, cfg.Storage.AccessKeyEnv)
	}
	sk, err := secrets.Require(cfg.Storage.SecretKeyEnv)
	if err != nil {
		missing = append(missing, cfg.Storage.SecretKeyEnv)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing secret(s): %s\nProvide them as environment variables, with --env-file <path>, or via secrets_file in %s", strings.Join(missing, ", "), cfgPath)
	}
	return d.newStore(storage.S3Config{Endpoint: cfg.Storage.Endpoint, Bucket: cfg.Storage.Bucket, Region: cfg.Storage.Region, AccessKey: ak, SecretKey: sk, PathStyle: cfg.Storage.PathStyle})
}

// dryRunBackup checks the config, connects to each instance, HEADs the bucket, prints the plan.
func dryRunBackup(ctx context.Context, cfg *config.App, secrets *config.Secrets, st storage.Store, d backupDeps, cfgPath string, logger *slog.Logger) int {
	w := d.stdout
	ok := true
	fmt.Fprintf(w, "app %s (%s)\n", cfg.App, cfgPath)
	if cfg.SecretsFile != "" {
		fmt.Fprintf(w, "secrets_file %s\n", cfg.SecretsFile)
	}
	fmt.Fprintf(w, "\nstorage: %s\n", st.Describe())
	if err := st.Check(ctx); err != nil {
		fmt.Fprintf(w, "  ✗ %v\n", err)
		ok = false
	} else {
		fmt.Fprintf(w, "  ✓ bucket reachable\n")
	}
	for _, name := range []string{cfg.Storage.AccessKeyEnv, cfg.Storage.SecretKeyEnv} {
		fmt.Fprintf(w, "  %s from %s\n", name, secrets.Source[name])
	}
	switch cfg.Encryption {
	case "age":
		fmt.Fprintf(w, "\nencryption: age, recipient %s, identity %s\n", cfg.AgeRecipient, cfg.AgeIdentityFile)
	default:
		fmt.Fprintf(w, "\nencryption: none (backups stored in clear)\n")
	}
	manifestKey := storage.Join(cfg.Storage.Prefix, cfg.App, "manifest.json")
	manifest, err := backup.LoadManifest(ctx, st, manifestKey, cfg.App)
	if err != nil {
		fmt.Fprintf(w, "manifest: %v\n", err)
	} else {
		var orphans []string
		for name := range manifest.Instances {
			if cfg.FindInstance(name) == nil {
				orphans = append(orphans, name)
			}
		}
		sort.Strings(orphans)
		if len(orphans) > 0 {
			logger.Warn("manifest has instances that are not in app.yaml (renamed or removed); their dumps stay untouched", "instances", strings.Join(orphans, ", "))
		}
	}
	tools := d.tools()
	for _, in := range cfg.Instances {
		fmt.Fprintf(w, "\ninstance %s\n", in.Name)
		for _, name := range []string{in.PasswordEnv, in.URLEnv} {
			if name != "" {
				fmt.Fprintf(w, "  %s from %s\n", name, secrets.Source[name])
			}
		}
		conn, runner, err := backup.Connect(ctx, tools, in, secrets)
		if err != nil {
			fmt.Fprintf(w, "  ✗ %v\n", err)
			ok = false
			continue
		}
		fmt.Fprintf(w, "  runner: %s\n", runner.Describe())
		client := &pg.Client{Runner: runner, Conn: conn}
		if err := client.Ping(ctx); err != nil {
			fmt.Fprintf(w, "  ✗ connect: %v\n", err)
			ok = false
			continue
		}
		ver, _ := client.ServerVersion(ctx)
		if in.Databases.All {
			dbs, err := client.ListDatabases(ctx)
			if err != nil {
				fmt.Fprintf(w, "  ✗ list databases: %v\n", err)
				ok = false
				continue
			}
			fmt.Fprintf(w, "  ✓ PostgreSQL %s, all databases (%d now: %s)\n", ver, len(dbs), strings.Join(dbs, ", "))
		} else {
			names := make([]string, 0, len(in.Databases.List))
			for _, db := range in.Databases.List {
				names = append(names, db.Name)
			}
			fmt.Fprintf(w, "  ✓ PostgreSQL %s, %d database(s): %s\n", ver, len(names), strings.Join(names, ", "))
		}
	}
	fmt.Fprintf(w, "\nDry run: nothing was dumped, uploaded or pinged.\n")
	if !ok {
		return exitError
	}
	return exitOK
}

// printResult prints one line per database (NFR observability).
func printResult(w io.Writer, res *backup.Result) {
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
		if len(ir.Added) > 0 {
			fmt.Fprintf(w, "  new database(s) since last run: %s\n", strings.Join(ir.Added, ", "))
		}
		if len(ir.Removed) > 0 {
			fmt.Fprintf(w, "  database(s) gone since last run: %s\n", strings.Join(ir.Removed, ", "))
		}
	}
	if res.OldKeyDumps > 0 {
		fmt.Fprintf(w, "%d dump(s) in the bucket still use a previous age key\n", res.OldKeyDumps)
	}
	for _, wmsg := range res.Warnings {
		fmt.Fprintf(w, "warning: %s\n", wmsg)
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

// report sends POST /report when app.yaml has hosted: (contract in spec ping).
// body is the Result of backup/drill; if the run could not start, failureReport is sent.
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
	data, err := json.Marshal(body)
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

// failureReport is the minimal body when the command could not start.
func failureReport(kind, app string, err error) map[string]any {
	return map[string]any{"kind": kind, "app": app, "ok": false, "error": err.Error()}
}
