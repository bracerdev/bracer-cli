package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/drill"
	"bracer.dev/bracer/internal/pg"
)

// runDrill is `bracer drill` (spec backup/drill). Shares deps with backup:
// drill does not connect to the real instance, it only needs the bucket and
// Docker or kubectl for the temporary Postgres.
func runDrill(ctx context.Context, args []string, d backupDeps) int {
	fs := flag.NewFlagSet("drill", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	cfgPath := fs.String("c", "app.yaml", "path of app.yaml")
	envFile := fs.String("env-file", "", "dotenv file with secrets (overrides secrets_file, overridden by the environment)")
	keep := fs.Bool("keep", false, "keep the temporary Postgres container or pod for inspection")
	skipDisk := fs.Bool("skip-disk-check", false, "run even if the disk of the temporary Postgres seems short on space")
	instance := fs.String("instance", "", "drill only this instance")
	database := fs.String("database", "", "drill only this database")
	asJSON := fs.Bool("json", false, "print the result as JSON on stdout")
	verbose := fs.Bool("verbose", false, "log every docker and kubectl command")
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
		fmt.Fprintf(d.stderr, "bracer drill: "+format+"\n", a...)
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
	st, err := openStore(cfg, secrets, d, *cfgPath, nil)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	lock, err := backup.AcquireKind(*cfgPath, "drill")
	if err != nil {
		return fail(exitError, "%v", err)
	}
	defer lock.Release()

	res, err := drill.Run(ctx, cfg, drill.Deps{Shell: d.shell, Tools: d.tools(), Store: st, Log: logger, Keep: *keep, Instance: *instance, Database: *database, SkipDiskCheck: *skipDisk})
	if err != nil {
		var ue *drill.UsageError
		if errors.As(err, &ue) {
			return fail(exitUsage, "%v", err)
		}
		report(ctx, cfg, secrets, d, logger, failureReport("drill", cfg.App, err))
		return fail(exitError, "%v", err)
	}
	if *asJSON {
		enc := json.NewEncoder(d.stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	} else {
		printDrillResult(d.stdout, res)
	}
	report(ctx, cfg, secrets, d, logger, res)
	if !res.OK {
		return exitError
	}
	return exitOK
}

// printDrillResult prints one line per database (NFR observability).
func printDrillResult(w io.Writer, res *drill.Result) {
	for _, ir := range res.Instances {
		if ir.Error != "" {
			fmt.Fprintf(w, "✗ %s: %s\n", ir.Name, ir.Error)
			continue
		}
		if g := ir.Globals; g != nil && !g.OK {
			fmt.Fprintf(w, "! %s/globals: %s\n", ir.Name, firstLine(g.Error))
		}
		for _, dr := range ir.Databases {
			checks := ""
			if len(dr.Checks) > 0 {
				okN := 0
				for _, c := range dr.Checks {
					if c.OK {
						okN++
					}
				}
				checks = fmt.Sprintf(", checks %d/%d", okN, len(dr.Checks))
			}
			mark := "✓"
			if !dr.OK {
				mark = "✗"
			}
			fmt.Fprintf(w, "%s %s/%s: restored %s in %.1f s, %d/%d tables%s\n", mark, ir.Name, dr.DB, humanBytes(dr.Bytes), dr.RestoreSeconds, dr.TablesFound, dr.TablesExpected, checks)
			for _, warn := range dr.Warnings {
				fmt.Fprintf(w, "    warning: %s\n", warn)
			}
			if dr.Error != "" {
				fmt.Fprintf(w, "    %s\n", strings.ReplaceAll(dr.Error, "\n", "\n    "))
			}
			if n := len(dr.MissingTables); n > 0 {
				shown := dr.MissingTables
				more := ""
				if n > 10 {
					shown, more = shown[:10], fmt.Sprintf(" (+%d more)", n-10)
				}
				fmt.Fprintf(w, "    missing tables: %s%s\n", strings.Join(shown, ", "), more)
			}
			for _, m := range dr.RowMismatches {
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
			for _, c := range dr.Checks {
				if !c.OK {
					fmt.Fprintf(w, "    check %q failed: %s\n", c.Name, c.Error)
				}
			}
		}
		if ir.Container != "" {
			fmt.Fprintf(w, "kept container %s (holds your real data; remove it when done):\n  docker exec -it %s psql -U postgres\n  docker rm -f -v %s\n", ir.Container, ir.Container, ir.Container)
		}
		if ir.Pod != "" {
			ns, name, _ := strings.Cut(ir.Pod, "/")
			fmt.Fprintf(w, "kept pod %s (holds your real data; remove it when done):\n  %s exec -it -n %s %s -- psql -U postgres\n  %s delete -n %s pod/%s networkpolicy/%s\n", ir.Pod, ir.Kubectl, ns, name, ir.Kubectl, ns, name, name)
		}
	}
	for _, wmsg := range res.Warnings {
		fmt.Fprintf(w, "warning: %s\n", wmsg)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
