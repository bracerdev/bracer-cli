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

// runInstance drills the run of one instance the way restore would restore
// it (ADR-0015 § 4, ADR-0022): compare, env-up, compare again, load each
// database into a new one, verify, drop them, env-down, and the temporary
// Postgres must be as it was. The run has everything of the instance
// (ri.Missing is empty).
func runInstance(ctx context.Context, rt *dockerRuntime, cfg *config.App, in config.Instance, run *backup.Run, ri *backup.RunInstance, ir *InstanceResult, res *Result, d Deps) {
	ids := identitiesFor(cfg, run.Encrypted(), d, res)
	runCfg, err := restore.ReadRunConfig(ctx, d.Store, run, ids)
	if err != nil {
		ir.Error = err.Error()
		return
	}
	dbs, known := restore.Expected(runCfg, in.Name)
	if !known {
		ir.Error = "the run's app.yaml does not name this instance"
		return
	}
	if missing := restore.RunMissing(ri, dbs); len(missing) > 0 {
		ir.Error = fmt.Sprintf("the run is missing %s; nothing to test-restore", strings.Join(missing, ", "))
		return
	}
	sort.Strings(dbs)
	rp, err := restore.PickRun(ri, in.Name, run.TS, dbs)
	if err != nil {
		ir.Error = err.Error()
		return
	}
	env, err := restore.ReadEnv(ctx, d.Store, rp.Env, ids)
	if err != nil {
		ir.Error = err.Error()
		return
	}

	// Image: like the source, checked before anything runs.
	image, err := chooseImage(ctx, rt, cfg, in, env, rp.DBs)
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
		ir.Error = err.Error() + "\nFix: " + backup.FixReinit(cfg.App)
		return
	}
	ir.Notes = append(ir.Notes, notes...)

	user := env.BootstrapSuperuser
	if user == "" || env.SourceKind == "url" {
		user = "postgres"
	}
	var need int64
	locks := 0
	for _, dp := range rp.DBs {
		need += dp.Bytes
		locks = max(locks, dp.LockObjects.Locks())
	}
	t, err := rt.start(ctx, cfg, in, image, user, preload, drillLocksPer(locks), need*restore.DiskFactor, ir, res, d)
	if err != nil {
		ir.Error = err.Error()
		return
	}
	defer rt.remove(ctx, t, res, in.Name)
	ir.OK = drillRun(ctx, rt, t, rp, ids, ir, d)
}

// drillRun is the part inside the temporary Postgres; false on any failure.
func drillRun(ctx context.Context, rt *dockerRuntime, t *tempPG, rp *restore.RunPick, ids *restore.Identities, ir *InstanceResult, d Deps) bool {
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
	read := func(loaded []string) (*restore.TargetState, error) {
		return restore.ReadTarget(ctx, admin, spec, full, loaded, src.Wants())
	}

	// 1. Compare with the fresh temporary Postgres.
	before, err := read(nil)
	if err != nil {
		return fail("read the temporary Postgres: %v", err)
	}
	snap := fingerprint(before)

	// 2. env-up until nothing is left, as a customer applies env-up and
	// checks again.
	var downs [][]string
	restart := false
	cur := before
	var again *restore.Plan
	for round := 1; ; round++ {
		plan := restore.MakePlan(restore.PlanInput{Src: src, Tgt: cur, Mode: mode, Now: d.Now(), OwnUser: t.user})
		var noteTo *InstanceResult
		if round == 1 {
			for _, n := range plan.Notes {
				// checkImage already said which preload libraries the image left out
				if !strings.HasPrefix(n, "the source also preloads") {
					ir.Notes = append(ir.Notes, n)
				}
			}
			noteTo = ir
		}
		failed := drillFailed(plan, noteTo)
		if len(failed) == 0 {
			again = plan
			break
		}
		var manual []restore.Check
		for _, c := range failed {
			if c.Fix != restore.FixSQL {
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
		if cur, err = read(nil); err != nil {
			return fail("read the temporary Postgres after env-up: %v", err)
		}
	}

	// 3. Load each database into a new one, in one transaction.
	ok := true
	loaded := map[string]string{}
	var targets []string
	for i, dp := range again.DBs {
		sdb := src.DBs[i]
		dr := &DatabaseResult{DB: sdb.Name, Bytes: sdb.Dump.Bytes, TablesExpected: len(sdb.Dump.Tables), RestoredAs: dp.Target}
		ir.Databases = append(ir.Databases, dr)
		start := d.Now()
		err := restore.LoadNew(ctx, d.Store, sdb.Dump, dp, ids, t.runner(), t.conn("postgres"), mode, cur.Major, config.DrillTimeout)
		dr.RestoreSeconds = d.Now().Sub(start).Seconds()
		var after *restore.ErrAfterLoad
		var committed *restore.ErrCommitted
		if err == nil || errors.As(err, &after) || errors.As(err, &committed) {
			targets = append(targets, dp.Target)
		}
		switch {
		case err == nil:
			loaded[sdb.Name] = dp.Target
			d.Log.Info("restored", "db", sdb.Name, "seconds", fmt.Sprintf("%.1f", dr.RestoreSeconds))
		case errors.Is(err, restore.ErrTimeout):
			dr.Error, ok = err.Error(), false
		case ctx.Err() != nil:
			dr.Error, ok = "pg_restore interrupted", false
		default:
			dr.Error, ok = err.Error(), false
		}
	}

	// 4. Verify: environment, tables.
	if len(loaded) > 0 {
		var names []string
		for _, n := range loaded {
			names = append(names, n)
		}
		after, err := read(names)
		if err != nil {
			return fail("read the temporary Postgres after the load: %v", err)
		}
		check := restore.MakePlan(restore.PlanInput{Src: src, Tgt: after, Mode: mode, Loaded: loaded, Now: d.Now(), OwnUser: t.user})
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
		cmp, err := restore.Compare(ctx, t.client(again.DBs[i].Target), sdb.Dump.Tables)
		dr.TablesFound, dr.MissingTables, dr.RowMismatches = cmp.Found, cmp.Missing, cmp.Mismatches
		if err != nil {
			dr.Error = fmt.Sprintf("compare tables: %v", err)
		}
		dr.OK = dr.Error == "" && cmp.OK()
		ok = ok && dr.OK
	}

	// 5. Drop the new databases, env-down, and the temporary Postgres is as it was.
	var drops []string
	for _, n := range targets {
		drops = append(drops, restore.DropDB(n, cur.Major))
	}
	if err := restore.ApplySQL(ctx, t.runner(), t.conn("postgres"), drops); err != nil {
		return fail("drop the restored databases: %v", err)
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
	final, err := read(nil)
	if err != nil {
		return fail("read the temporary Postgres after env-down: %v", err)
	}
	if diff := diffPrints(snap, fingerprint(final)); len(diff) > 0 {
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
func fingerprint(st *restore.TargetState) map[string]string {
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
func identitiesFor(cfg *config.App, encrypted bool, d Deps, res *Result) *restore.Identities {
	if !encrypted {
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

// drillSlots is a lower bound of the temporary Postgres's lock slots
// (max_connections alone, default 100).
const drillSlots = 100

// drillLocksPer is the max_locks_per_transaction the temporary Postgres starts
// with so the largest database loads in one transaction; 0 keeps the default
// (spec drill).
func drillLocksPer(locks int) int {
	if locks <= 64*drillSlots {
		return 0
	}
	return pg.LocksPer(locks, drillSlots)
}
