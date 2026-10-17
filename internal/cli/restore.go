package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/restore"
)

// runRestore is `bracer restore` (spec backup/fetch-restore).
func runRestore(ctx context.Context, args []string, d backupDeps) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	cfgPath := fs.String("c", "app.yaml", "path of app.yaml")
	envFile := fs.String("env-file", "", "dotenv file with secrets (overrides secrets_file, overridden by the environment)")
	var o restore.Options
	fs.StringVar(&o.Instance, "instance", "", "source instance in the bucket (default: the only one with backups)")
	fs.StringVar(&o.Database, "database", "", "source database to restore")
	fs.StringVar(&o.TS, "ts", "", "dump timestamp as shown by fetch --list (default: latest)")
	fs.StringVar(&o.File, "file", "", "restore from a file fetched earlier instead of the bucket")
	fs.StringVar(&o.Into, "into", "", "target: <instance>[/<database>] from app.yaml (default database: same name as source)")
	fs.StringVar(&o.IntoURLEnv, "into-url-env", "", "target: name of an environment variable holding a postgres:// URL")
	fs.BoolVar(&o.WithGlobals, "with-globals", false, "create users missing on the target from the globals dump first (existing users are never changed)")
	fs.BoolVar(&o.SkipDiskCheck, "skip-disk-check", false, "restore even if the target seems to lack free disk space")
	fs.BoolVar(&o.NoOwner, "no-owner", false, "pass --no-owner to pg_restore")
	fs.BoolVar(&o.NoACL, "no-acl", false, "pass --no-acl to pg_restore")
	fs.BoolVar(&o.DryRun, "dry-run", false, "show source, target and what would happen; change nothing")
	asJSON := fs.Bool("json", false, "print the result as JSON on stdout")
	verbose := fs.Bool("verbose", false, "log every docker, kubectl and pg_* command")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	logger := newLogger(d.stderr, *verbose)
	if sh, ok := d.shell.(pg.ExecShell); ok {
		sh.Verbose = *verbose
		d.shell = sh
	}
	fail := func(code int, format string, a ...interface{}) int {
		fmt.Fprintf(d.stderr, "bracer restore: "+format+"\n", a...)
		return code
	}

	cfg, secrets, st, code, err := loadAppAndStore(*cfgPath, *envFile, d, logger)
	if err != nil {
		return fail(code, "%v", err)
	}
	deps := restore.Deps{Shell: d.shell, Store: st, Secrets: secrets, GOOS: d.goos, Tools: d.tools(), Log: logger, Timeout: cfg.DrillTimeout()}
	res, err := restore.Run(ctx, cfg, o, deps)
	if err != nil {
		var ue *restore.UsageError
		switch {
		case errors.As(err, &ue), errors.Is(err, restore.ErrExists), errors.Is(err, restore.ErrDiskSpace):
			return fail(exitUsage, "%v", err)
		default:
			return fail(exitError, "%v", err)
		}
	}
	if *asJSON {
		enc := json.NewEncoder(d.stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	} else {
		printRestoreResult(d.stdout, res)
	}
	if !res.OK && !res.DryRun {
		return exitError
	}
	return exitOK
}

func printRestoreResult(w io.Writer, res *restore.Result) {
	src := res.Source.File
	if src == "" {
		src = fmt.Sprintf("%s/%s %s (%s)", res.Source.Instance, res.Source.DB, res.Source.TS, humanBytes(res.Source.Bytes))
	}
	tgt := res.Target.Instance
	if tgt == "" {
		tgt = "$" + res.Target.URLEnv
	}
	if res.Target.Host != "" {
		tgt += " (" + res.Target.Host + ")"
	}
	if res.DryRun {
		enc := "not encrypted"
		if res.Source.Encrypted {
			enc = "encrypted"
		}
		fmt.Fprintf(w, "source: %s, %s\n", src, enc)
		fmt.Fprintf(w, "target: %s, database %q, PostgreSQL %s via %s\n", tgt, res.Target.DB, res.Target.PgVersion, res.Target.Runner)
		fmt.Fprintf(w, "        database does not exist and would be created from template0\n")
		if dc := res.Disk; dc != nil && dc.Checked {
			fmt.Fprintf(w, "        disk: %s free at %s, about %s needed\n", humanBytes(dc.FreeBytes), dc.DataDirectory, humanBytes(dc.NeedBytes))
		}
		fmt.Fprintf(w, "Dry run: nothing was changed.\n")
	} else {
		mark := "✓"
		if !res.OK {
			mark = "✗"
		}
		cmp := fmt.Sprintf("%d tables", res.TablesFound)
		if res.Source.File == "" {
			cmp = fmt.Sprintf("%d/%d tables", res.TablesFound, res.TablesExpected)
		}
		fmt.Fprintf(w, "%s %s → %s/%s: restored in %.1f s, %s\n", mark, src, tgt, res.Target.DB, res.RestoreSeconds, cmp)
		if res.Error != "" {
			fmt.Fprintf(w, "    %s\n", strings.ReplaceAll(res.Error, "\n", "\n    "))
		}
		if n := len(res.MissingTables); n > 0 {
			shown, more := res.MissingTables, ""
			if n > 10 {
				shown, more = shown[:10], fmt.Sprintf(" (+%d more)", n-10)
			}
			fmt.Fprintf(w, "    missing tables: %s%s\n", strings.Join(shown, ", "), more)
		}
		for _, m := range res.RowMismatches {
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
		if g := res.Globals; g != nil {
			if g.OK {
				fmt.Fprintf(w, "  users created: %s (%d already existed, left untouched)\n", orNone(g.Created), g.SkippedExisting)
			} else {
				fmt.Fprintf(w, "  globals: %s\n", firstLine(g.Error))
			}
		}
		// Same instance, different name: print the swap recipe ready to use.
		if res.OK && res.Source.Instance != "" && res.Source.Instance == res.Target.Instance && res.Source.DB != res.Target.DB {
			fmt.Fprintf(w, "  To swap it in once you have checked it (both databases must have no open connections):\n    alter database %s rename to %s;\n    alter database %s rename to %s;\n",
				pg.QuoteIdent(res.Source.DB), pg.QuoteIdent(res.Source.DB+"_old"), pg.QuoteIdent(res.Target.DB), pg.QuoteIdent(res.Source.DB))
		}
	}
	for _, wmsg := range res.Warnings {
		fmt.Fprintf(w, "warning: %s\n", wmsg)
	}
}

func orNone(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, ", ")
}
