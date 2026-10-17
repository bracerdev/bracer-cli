package drill

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/restore"
)

// restoreGlobals: bucket → [age] → psql, ignoring "already exists".
func restoreGlobals(ctx context.Context, t *tempPG, g *backup.Dump, ids *restore.Identities, d Deps) *GlobalsResult {
	rc, err := restore.OpenDump(ctx, d.Store, g, ids)
	if err != nil {
		return &GlobalsResult{Error: err.Error()}
	}
	defer rc.Close()
	gr := restore.Globals(ctx, t.runner(), t.conn("postgres"), rc)
	d.Log.Info("globals restored", "ignored_errors", gr.IgnoredErrors, "ok", gr.OK)
	return gr
}

// restoreDatabase: create database → pg_restore via stdin → compare → checks.
// drillPostgresDB is the name of the temporary database that receives the `postgres` dump.
const drillPostgresDB = "bracer_drill_postgres"

func restoreDatabase(ctx context.Context, cfg *config.App, t *tempPG, db string, dp *backup.Dump, avail map[string]string, ids *restore.Identities, d Deps) *DatabaseResult {
	dr := &DatabaseResult{DB: db, TS: dp.TS, Bytes: dp.Bytes, TablesExpected: len(dp.Tables)}
	log := d.Log.With("db", db)

	// The database's extensions must exist in the image; check before doing anything
	// (spec drill § Flow 3b); a different version is only a warning.
	missing, differ := pg.MissingExtensions(dp.Extensions, avail)
	if len(missing) > 0 {
		dr.Error = fmt.Sprintf("needs extension %s, which %s does not have; set drill.image in app.yaml to an image that has it", joinExtensions(missing), t.image)
		log.Error("extension missing in image", "err", dr.Error)
		return dr
	}
	for _, e := range differ {
		dr.Warnings = append(dr.Warnings, fmt.Sprintf("%s has extension %s %s, the source database used %s", t.image, e.Name, e.Version, versionOf(dp.Extensions, e.Name)))
	}

	rc, err := restore.OpenDump(ctx, d.Store, dp, ids)
	if err != nil {
		dr.Error = err.Error()
		return dr
	}
	defer rc.Close()

	// Always restore into a new database from template0. `postgres` exists in the
	// temporary cluster but is not necessarily empty: the image's init scripts
	// (postgis/postgis…) wrote into it, so restoring straight there fails with "already exists".
	target := db
	if db == "postgres" {
		target = drillPostgresDB
		dr.RestoredAs = target
	}
	// Encoding/locale from the manifest; the chosen image is ≥ the source server's
	// major, so the provider is always present and no major check is needed (0).
	if err := restore.CreateDatabase(ctx, t.client("postgres"), target, dp.Locale, 0); err != nil {
		dr.Error = fmt.Sprintf("create database %s: %v", target, err)
		return dr
	}

	start := d.Now()
	err = restore.Database(ctx, t.runner(), t.conn(target), target, nil, rc, cfg.DrillTimeout())
	dr.RestoreSeconds = d.Now().Sub(start).Seconds()
	switch {
	case err == nil:
		log.Info("restored", "bytes", dp.Bytes, "seconds", fmt.Sprintf("%.1f", dr.RestoreSeconds))
	case errors.Is(err, restore.ErrTimeout):
		dr.Error = err.Error() + " (drill.timeout)"
		log.Error("restore failed", "err", dr.Error)
	case ctx.Err() != nil:
		dr.Error = "pg_restore interrupted"
		return dr
	default:
		dr.Error = err.Error()
		log.Error("restore failed", "err", dr.Error)
	}

	// Still compare when pg_restore fails: it shows what is missing.
	cmp, cerr := restore.Compare(ctx, t.client(target), dp.Tables)
	dr.TablesFound, dr.MissingTables, dr.RowMismatches = cmp.Found, cmp.Missing, cmp.Mismatches
	if cerr != nil && dr.Error == "" {
		dr.Error = fmt.Sprintf("compare tables: %v", cerr)
	}
	if cfg.Drill != nil {
		for _, c := range cfg.Drill.Checks {
			if c.AppliesTo(db) {
				dr.Checks = append(dr.Checks, runCheck(ctx, t.client(target), c))
			}
		}
	}
	dr.OK = dr.Error == "" && cmp.OK()
	for _, c := range dr.Checks {
		if !c.OK {
			dr.OK = false
		}
	}
	return dr
}

// runCheck runs a drill.checks entry: ≥ 1 row is a pass. Results are not logged (security NFR).
func runCheck(ctx context.Context, c *pg.Client, chk config.Check) CheckResult {
	cr := CheckResult{Name: chk.Name}
	rows, err := c.Query(ctx, chk.SQL)
	switch {
	case err != nil:
		cr.Error = firstLine(err.Error())
	case len(rows) == 0:
		cr.Error = "no rows returned"
	default:
		cr.OK = true
	}
	return cr
}

func firstLine(s string) string {
	for i, ch := range s {
		if ch == '\n' {
			return s[:i]
		}
	}
	return s
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
