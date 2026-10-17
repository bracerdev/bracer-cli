package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"golang.org/x/term"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/crypto"
	"bracer.dev/bracer/internal/restore"
	"bracer.dev/bracer/internal/storage"
)

// runFetch is `bracer fetch` (spec backup/fetch-restore).
func runFetch(ctx context.Context, args []string, d backupDeps) int {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	cfgPath := fs.String("c", "app.yaml", "path of app.yaml")
	envFile := fs.String("env-file", "", "dotenv file with secrets (overrides secrets_file, overridden by the environment)")
	instance := fs.String("instance", "", "instance to fetch from (default: the only one with backups)")
	database := fs.String("database", "", "database to fetch")
	globals := fs.Bool("globals", false, "fetch the globals dump (users and their settings) instead of a database")
	ts := fs.String("ts", "", "dump timestamp as shown by --list (default: latest)")
	out := fs.String("o", "", "output file, or - for stdout (default: <app>-<instance>-<db>-<ts>.dump)")
	raw := fs.Bool("raw", false, "keep the object as stored (encrypted), do not decrypt")
	force := fs.Bool("force", false, "overwrite the output file if it exists")
	list := fs.Bool("list", false, "list every dump in the bucket and exit")
	verbose := fs.Bool("verbose", false, "log every docker command")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	logger := newLogger(d.stderr, *verbose)
	fail := func(code int, format string, a ...interface{}) int {
		fmt.Fprintf(d.stderr, "bracer fetch: "+format+"\n", a...)
		return code
	}

	cfg, secrets, st, code, err := loadAppAndStore(*cfgPath, *envFile, d, logger)
	if err != nil {
		return fail(code, "%v", err)
	}
	_ = secrets
	manifest, err := backup.LoadManifest(ctx, st, storage.Join(cfg.Storage.Prefix, cfg.App, "manifest.json"), cfg.App)
	if err != nil {
		return fail(exitError, "load manifest: %v", err)
	}
	if *list {
		printList(d.stdout, restore.List(manifest))
		return exitOK
	}

	sel, err := restore.Select(manifest, cfg, *instance, *database, *ts, *globals)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	dp := sel.Dump

	var ids *restore.Identities
	if dp.Encrypted && !*raw {
		path, perr := config.ExpandHome(cfg.AgeIdentityFile)
		if perr != nil {
			ids = &restore.Identities{Err: perr}
		} else {
			ids = restore.ReadIdentities(path, crypto.ReadIdentities)
		}
	}

	// Destination: stdout or a file (O_EXCL unless --force).
	var w io.Writer
	var f *os.File
	path := *out
	if path == "-" {
		if of, ok := d.stdout.(*os.File); ok && term.IsTerminal(int(of.Fd())) {
			return fail(exitUsage, "refusing to write a binary dump to a terminal; use -o <file>")
		}
		w = d.stdout
	} else {
		if path == "" {
			path = defaultFetchName(cfg.App, sel, *raw)
		}
		flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
		if *force {
			flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		}
		f, err = os.OpenFile(path, flags, 0o600)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				return fail(exitUsage, "%s exists; pass -o <other file> or --force", path)
			}
			return fail(exitError, "%v", err)
		}
		w = f
	}

	n, err := restore.Fetch(ctx, st, dp, ids, *raw, w)
	if f != nil {
		f.Close()
	}
	if err != nil {
		if f != nil {
			os.Remove(path)
		}
		return fail(exitError, "%v", err)
	}
	what := sel.Database
	if sel.Globals {
		what = "globals"
	}
	enc := "not encrypted"
	switch {
	case dp.Encrypted && *raw:
		enc = "still encrypted (--raw)"
	case dp.Encrypted:
		enc = "decrypted"
	}
	dest := path
	if path == "-" {
		dest = "stdout"
	}
	fmt.Fprintf(d.stderr, "fetched %s/%s %s → %s (%s, %s, sha256 verified)\n", sel.Instance, what, dp.TS, dest, humanBytes(n), enc)
	return exitOK
}

var reFileChar = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// defaultFetchName: <app>-<instance>-<db>-<ts>.dump[.age] (globals: .sql).
func defaultFetchName(app string, sel *restore.Selection, raw bool) string {
	what, ext := sel.Database, ".dump"
	if sel.Globals {
		what, ext = "globals", ".sql"
	}
	name := reFileChar.ReplaceAllString(strings.Join([]string{app, sel.Instance, what, sel.Dump.TS}, "-"), "_") + ext
	if raw && sel.Dump.Encrypted {
		name += ".age"
	}
	return name
}

func printList(w io.Writer, entries []restore.Entry) {
	if len(entries) == 0 {
		fmt.Fprintln(w, "no dumps in the bucket yet; run bracer backup")
		return
	}
	fmt.Fprintf(w, "%-12s %-20s %-20s %10s  %s\n", "INSTANCE", "DATABASE", "TS", "SIZE", "ENCRYPTION")
	for _, e := range entries {
		ts := e.TS
		if e.Latest {
			ts += " *"
		}
		enc := "none"
		if e.Encrypted {
			enc = "age " + e.Recipient
		}
		fmt.Fprintf(w, "%-12s %-20s %-20s %10s  %s\n", e.Instance, e.Database, ts, humanBytes(e.Bytes), enc)
	}
	fmt.Fprintln(w, "* = latest")
}

func newLogger(w io.Writer, verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	return logger
}

// loadAppAndStore reads app.yaml, loads the bucket secrets and opens the Store; code is the exit code on error.
func loadAppAndStore(cfgPath, envFile string, d backupDeps, logger *slog.Logger) (*config.App, *config.Secrets, storage.Store, int, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, nil, exitUsage, err
	}
	secrets, warns, err := config.LoadSecrets(cfg, envFile)
	if err != nil {
		return nil, nil, nil, exitUsage, err
	}
	for _, w := range warns {
		logger.Warn(w)
	}
	st, err := openStore(cfg, secrets, d, cfgPath, nil)
	if err != nil {
		return nil, nil, nil, exitUsage, err
	}
	return cfg, secrets, st, exitOK, nil
}
