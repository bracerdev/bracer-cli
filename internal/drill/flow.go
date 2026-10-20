package drill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/restore"
)

// runInstance drills the latest run of one instance the way restore would
// restore it (ADR-0015 § 4): compare, env-up, compare again, load, verify,
// data-down, env-down, and the temporary Postgres must be as it was.
func runInstance(ctx context.Context, rt *dockerRuntime, cfg *config.App, in config.Instance, im *backup.InstanceManifest, ir *InstanceResult, res *Result, d Deps) {
	if len(im.Env.Dumps) == 0 || im.Env.Latest == "" {
		ir.Error = "the latest backup has no environment record; run bracer backup with this version first"
		return
	}
	ts := im.Env.Latest
	ir.TS = ts
	var dbs []string
	for name, dm := range im.Databases {
		if latest(dm.Dumps, ts) == nil {
			continue
		}
		if !in.Databases.All && !inList(in.Databases.List, name) {
			res.warn("%s/%s: in the bucket but not in app.yaml, skipped", in.Name, name)
			continue
		}
		if d.Database != "" && name != d.Database {
			continue
		}
		dbs = append(dbs, name)
	}
	sort.Strings(dbs)
	if len(dbs) == 0 {
		ir.Error = "the latest run " + ts + " has no database dump to restore"
		return
	}
	rp, err := restore.PickRun(im, in.Name, ts, dbs)
	if err != nil {
		ir.Error = err.Error()
		return
	}
	ids := identitiesFor(cfg, rp, d, res)
	env, err := restore.ReadEnv(ctx, d.Store, rp.Env, ids)
	if err != nil {
		ir.Error = err.Error()
		return
	}
	ir.Source = env.SourceKind
	for _, dp := range rp.DBs {
		if dp.SystemID != "" {
			ir.SystemID = dp.SystemID
		}
	}

	// Image: like the source, checked before anything runs.
	image, err := chooseImage(ctx, rt, cfg, env, rp.DBs, res, in.Name)
	if err != nil {
		ir.Error = err.Error()
		return
	}
	ir.Image = image
	if err := rt.checkOfficial(ctx, image); err != nil {
		ir.Error = err.Error()
		return
	}
	exts, libs, err := rt.imageFiles(ctx, image)
	if err != nil {
		ir.Error = err.Error()
		return
	}
	preload, notes, err := checkImage(env, rp.DBs, image, exts, libs)
	if err != nil {
		ir.Error = err.Error() + "; set drill.image in app.yaml to an image that has it"
		return
	}
	ir.Notes = append(ir.Notes, notes...)

	user := env.BootstrapSuperuser
	if user == "" || env.SourceKind == "url" {
		user = "postgres"
	}
	var need int64
	for _, dp := range rp.DBs {
		need += dp.Bytes
	}
	t, err := rt.start(ctx, cfg, in, image, user, preload, need*restore.DiskFactor, ir, res, d)
	if err != nil {
		ir.Error = err.Error()
		return
	}
	defer func() {
		if d.Keep {
			ir.Container, ir.Volumes, ir.User = t.name, t.volumes, t.user
			return
		}
		rt.remove(ctx, t, res, in.Name)
	}()
	ir.OK = drillRun(ctx, rt, t, cfg, rp, ids, ir, d)
}

// drillRun is the part inside the temporary Postgres; false on any failure.
func drillRun(ctx context.Context, rt *dockerRuntime, t *tempPG, cfg *config.App, rp *restore.RunPick, ids *restore.Identities, ir *InstanceResult, d Deps) bool {
	fail := func(format string, args ...interface{}) bool {
		ir.Error = fmt.Sprintf(format, args...)
		return false
	}
	admin := t.client("postgres")
	if need := toolMajor(rp.DBs); need > 0 {
		v, have, err := admin.ToolVersion(ctx)
		if err != nil {
			return fail("check pg_restore in %s: %v", t.image, err)
		}
		if have < need {
			return fail("dumps were made with pg_dump %d; pg_restore %s in %s cannot read them; set drill.image to a newer image", need, v, t.image)
		}
	}
	src, err := restore.LoadSource(ctx, d.Store, rp, ids, t.runner(), t.conn("postgres"))
	if err != nil {
		return fail("%v", err)
	}
	mode := src.Mode()
	ir.Mode = mode
	full := mode == restore.ModeFull
	spec := restore.TargetSpec{Kind: "container", Container: t.name, Shell: rt.sh}
	read := func(names []string) (*restore.TargetState, error) {
		return restore.ReadTarget(ctx, admin, spec, full, names, src.Locales())
	}
	names := src.Names()

	// 1. Compare with the fresh temporary Postgres.
	before, err := read(names)
	if err != nil {
		return fail("read the temporary Postgres: %v", err)
	}
	snap := fingerprint(before, names)

	// 2. env-up until nothing is left, as a customer runs restore again after
	// each env-up file (a database created by one round has its settings
	// compared in the next).
	var downs [][]string
	restart := false
	cur := before
	var again *restore.Plan
	for round := 1; ; round++ {
		plan := restore.MakePlan(restore.PlanInput{Src: src, Tgt: cur, Mode: mode, Now: d.Now()})
		var noteTo *InstanceResult
		if round == 1 {
			ir.Notes = append(ir.Notes, plan.Notes...)
			noteTo = ir
		}
		failed := drillFailed(plan, noteTo)
		if len(failed) == 0 {
			again = plan
			break
		}
		var manual []restore.Check
		for _, c := range failed {
			if c.Fix != restore.FixSQL && c.Fix != restore.FixRerun {
				manual = append(manual, c)
			}
		}
		switch {
		case len(manual) > 0 && round == 1:
			ir.Checks = manual
			return fail("the temporary Postgres (%s) cannot take this backup: %s", t.image, describe(manual))
		case len(manual) > 0 || round > maxRounds || len(plan.Up) == 0:
			ir.Checks = failed
			return fail("env-up did not make the target match: %s", describe(failed))
		}
		if err := restore.ApplySQL(ctx, t.runner(), t.conn("postgres"), plan.Up); err != nil {
			return fail("env-up failed: %v", err)
		}
		downs = append(downs, plan.Down)
		if plan.Restart {
			restart = true
			if err := rt.restart(ctx, t, d); err != nil {
				return fail("%v", err)
			}
		}
		if cur, err = read(names); err != nil {
			return fail("read the temporary Postgres after env-up: %v", err)
		}
	}
	ready := cur

	// 3. Load each database in one transaction.
	ok := true
	loaded := map[string]string{}
	var targets []string
	for i, dp := range again.DBs {
		sdb := src.DBs[i]
		dr := &DatabaseResult{DB: sdb.Name, TS: rp.TS, SystemID: sdb.Dump.SystemID, Bytes: sdb.Dump.Bytes, TablesExpected: len(sdb.Dump.Tables)}
		if dp.Target != sdb.Name {
			dr.RestoredAs = dp.Target
		}
		ir.Databases = append(ir.Databases, dr)
		start := d.Now()
		err := restore.LoadDB(ctx, d.Store, sdb.Dump, ids, t.runner(), t.conn("postgres"), dp.Target, mode, cfg.DrillTimeout())
		dr.RestoreSeconds = d.Now().Sub(start).Seconds()
		switch {
		case err == nil:
			loaded[sdb.Name] = dp.Target
			targets = append(targets, dp.Target)
			d.Log.Info("restored", "db", sdb.Name, "seconds", fmt.Sprintf("%.1f", dr.RestoreSeconds))
		case errors.Is(err, restore.ErrTimeout):
			dr.Error, ok = err.Error()+" (drill.timeout)", false
		case ctx.Err() != nil:
			dr.Error, ok = "pg_restore interrupted", false
		default:
			dr.Error, ok = err.Error(), false
		}
	}

	// 4. Verify: environment, tables, drill.checks.
	if len(targets) > 0 {
		after, err := read(targets)
		if err != nil {
			return fail("read the temporary Postgres after the load: %v", err)
		}
		check := restore.MakePlan(restore.PlanInput{Src: src, Tgt: after, Mode: mode, Loaded: loaded, Now: d.Now()})
		if left := drillFailed(check, nil); len(left) > 0 {
			ir.Checks = left
			ir.Error = "after the load the target no longer matches the backup: " + describe(left)
			ok = false
		}
	}
	for i, dr := range ir.Databases {
		if dr.Error != "" {
			continue
		}
		sdb := src.DBs[i]
		c := t.client(again.DBs[i].Target)
		cmp, err := restore.Compare(ctx, c, sdb.Dump.Tables)
		dr.TablesFound, dr.MissingTables, dr.RowMismatches = cmp.Found, cmp.Missing, cmp.Mismatches
		if err != nil {
			dr.Error = fmt.Sprintf("compare tables: %v", err)
		}
		dr.Checks = restore.RunChecks(ctx, c, cfg, sdb.Name)
		dr.OK = dr.Error == "" && cmp.OK()
		for _, ch := range dr.Checks {
			dr.OK = dr.OK && ch.OK
		}
		ok = ok && dr.OK
	}
	if d.Keep {
		return ok
	}

	// 5. data-down, env-down, and the temporary Postgres is as it was.
	if err := restore.ApplySQL(ctx, t.runner(), t.conn("postgres"), restore.DataDown(ready, targets)); err != nil {
		return fail("data-down failed: %v", err)
	}
	for i := len(downs) - 1; i >= 0; i-- {
		if err := restore.ApplySQL(ctx, t.runner(), t.conn("postgres"), downs[i]); err != nil {
			return fail("env-down failed: %v", err)
		}
	}
	if restart {
		if err := rt.restart(ctx, t, d); err != nil {
			return fail("%v", err)
		}
	}
	final, err := read(names)
	if err != nil {
		return fail("read the temporary Postgres after env-down: %v", err)
	}
	if diff := diffPrints(snap, fingerprint(final, names)); len(diff) > 0 {
		return fail("env-down did not restore the previous state: %s", listFirstN(diff, 5))
	}
	return ok
}

// maxRounds is how many env-up files drill applies before giving up.
const maxRounds = 3

// targetOnly are the checks about how clients reach the server (pg_hba.conf,
// certificates): settings of the real target, which the temporary server
// cannot stand for. Restore checks them on the real target.
var targetOnly = map[string]bool{"pg_hba": true, "ssl": true}

// drillFailed is plan.Failed() without the targetOnly checks, which become notes.
func drillFailed(p *restore.Plan, ir *InstanceResult) []restore.Check {
	var out []restore.Check
	for _, c := range p.Failed() {
		if targetOnly[c.Area] {
			if ir != nil {
				ir.Notes = append(ir.Notes, c.Area+" "+c.Name+": "+c.Detail+" (restore checks this on the real target)")
			}
			continue
		}
		out = append(out, c)
	}
	return out
}

func describe(checks []restore.Check) string {
	parts := make([]string, 0, len(checks))
	for _, c := range checks {
		s := c.Area + " " + c.Name
		if c.Detail != "" {
			s += " (" + c.Detail + ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "; ")
}

func toolMajor(dumps []*backup.Dump) int {
	n := 0
	for _, dp := range dumps {
		n = max(n, pg.MajorOf(dp.ToolVersion))
	}
	return n
}

// fingerprint is what env-down must bring back: roles, parameter grants,
// databases, settings.
func fingerprint(st *restore.TargetState, names []string) map[string]string {
	out := map[string]string{}
	put := func(k string, v interface{}) {
		b, _ := json.Marshal(v)
		out[k] = string(b)
	}
	if st.Roles != nil {
		for n, r := range st.Roles.Roles {
			put("role "+n, r)
		}
		put("parameter grants", st.Roles.ParamGrants)
	}
	var dbs []string
	for n := range st.Names {
		dbs = append(dbs, n)
	}
	sort.Strings(dbs)
	put("databases", dbs)
	for _, n := range names {
		if td := st.DBs[n]; td != nil {
			cp := *td
			cp.Connections, cp.Effective = nil, nil
			put("database "+n, cp)
		}
	}
	for _, s := range st.Env.Settings {
		out["setting "+s.Name] = s.Value
	}
	if st.Env.PreloadLibraries != nil {
		out["setting shared_preload_libraries"] = *st.Env.PreloadLibraries
	}
	for k, v := range st.AutoConf {
		out["postgresql.auto.conf "+k] = v
	}
	return out
}

func diffPrints(a, b map[string]string) []string {
	var out []string
	for k, v := range a {
		if w, ok := b[k]; !ok {
			out = append(out, k+" is gone")
		} else if w != v {
			out = append(out, k+" changed")
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			out = append(out, k+" was added")
		}
	}
	sort.Strings(out)
	return out
}

func listFirstN(s []string, n int) string {
	if len(s) <= n {
		return strings.Join(s, ", ")
	}
	return strings.Join(s[:n], ", ") + fmt.Sprintf(" (+%d more)", len(s)-n)
}

// identitiesFor reads age_identity_file only when the run has encrypted objects.
func identitiesFor(cfg *config.App, rp *restore.RunPick, d Deps, res *Result) *restore.Identities {
	needed := rp.Env.Encrypted || (rp.Globals != nil && rp.Globals.Encrypted)
	for _, dp := range rp.DBs {
		needed = needed || dp.Encrypted
	}
	if !needed {
		return &restore.Identities{}
	}
	path, err := config.ExpandHome(cfg.AgeIdentityFile)
	var ids *restore.Identities
	if err != nil {
		ids = &restore.Identities{Err: err}
	} else {
		ids = restore.ReadIdentities(path, d.ReadIdentities)
	}
	if ids.Err != nil {
		res.warn("age_identity_file: %v; encrypted dumps cannot be verified", ids.Err)
	}
	return ids
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
