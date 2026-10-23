package restore

import (
	"context"
	"strings"
	"testing"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/pg"
)

// ADR-0016, ADR-0026: a run lacking a database of its app.yaml is refused;
// the customer picks another. The list is read first (ListRuns), then a run
// of it is picked.
func TestPickRunRefusesIncompleteRun(t *testing.T) {
	ctx := context.Background()
	st := newMemStore()
	seed(t, st, "")
	seedRun(t, st, seedTS, []string{"auth"}, "shop", "auth")
	s := NewSession(deps(st), shopFolder(t, st))
	s.Instance = "main"
	if s.Newest().TS != seedTS {
		t.Fatalf("newest: %+v", s.Newest())
	}
	// Nothing can be picked before the list was read.
	if err := s.PickRun(ctx, seedTS); err == nil || !strings.Contains(err.Error(), "no run "+seedTS+" of main in the list") {
		t.Errorf("before ListRuns: %v", err)
	}
	if runs, err := s.ListRuns(ctx); err != nil || len(runs) != 1 || runs[0].TS != seedTS {
		t.Fatalf("list: %+v %v", runs, err)
	}
	if err := s.PickRun(ctx, seedTS); err == nil || !strings.Contains(err.Error(), "is missing auth; pick another") || s.Run != nil {
		t.Fatalf("incomplete run: %v", err)
	}
	// A run without its own app.yaml cannot say what it meant to back up.
	if err := s.PickRun(ctx, oldTS); err == nil || !strings.Contains(err.Error(), "has no app.yaml naming main") {
		t.Errorf("no app.yaml: %v", err)
	}
	if err := s.PickRun(ctx, "2026-10-14T030000Z"); err == nil || !strings.Contains(err.Error(), "in the list") {
		t.Errorf("unknown run: %v", err)
	}

	st = newMemStore()
	seed(t, st, "")
	seedRun(t, st, seedTS, nil, "shop")
	editRun(t, st, seedTS, func(r *backup.Run) {
		r.Instances["main"].Drill = &backup.DrillOutcome{Result: backup.DrillPassed, TookSeconds: 12.5}
	})
	s = NewSession(deps(st), shopFolder(t, st))
	s.Instance = "main"
	if _, err := s.ListRuns(ctx); err != nil {
		t.Fatal(err)
	}
	if s.Drilled() != 0 {
		t.Errorf("drilled before a pick: %v", s.Drilled())
	}
	if err := s.PickRun(ctx, seedTS); err != nil || s.Run.TS != seedTS || strings.Join(s.Run.Names, ",") != "shop" || s.Run.Globals == nil || s.Run.Env == nil {
		t.Fatalf("complete run: %v %+v", err, s.Run)
	}
	if s.Run.DBs[0].Key != backup.RunDir(shopRoot, seedTS)+"main/shop.dump" || s.Run.Globals.Key != backup.RunDir(shopRoot, seedTS)+"main/globals.sql" {
		t.Errorf("keys of the run: %+v %+v", s.Run.DBs[0], s.Run.Globals)
	}
	if s.Drilled() != 12.5 || s.SQLDir() != "bracer-restore-shop-main-"+seedTS {
		t.Errorf("drilled %v, dir %q", s.Drilled(), s.SQLDir())
	}
}

func TestPickRunRefusesRunMissingGlobals(t *testing.T) {
	ctx := context.Background()
	st := newMemStore()
	seed(t, st, "")
	seedRun(t, st, seedTS, []string{"globals"}, "shop")
	editRun(t, st, seedTS, func(r *backup.Run) { r.Instances["main"].Globals = nil })
	s := NewSession(deps(st), shopFolder(t, st))
	s.Instance = "main"
	if _, err := s.ListRuns(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.PickRun(ctx, seedTS); err == nil || !strings.Contains(err.Error(), "is missing globals") {
		t.Fatalf("missing globals: %v", err)
	}
}

func TestRunConfigIsTheCriterion(t *testing.T) {
	// run.json says the instance is whole, but the run's own app.yaml names
	// auth, which has no dump: app.yaml wins.
	st := newMemStore()
	seed(t, st, "")
	seedRun(t, st, seedTS, nil, "shop", "auth")
	ctx := context.Background()
	run := loadRun(t, st, seedTS)
	runCfg, err := ReadRunConfig(ctx, st, run, nil)
	if err != nil {
		t.Fatal(err)
	}
	exp, known := Expected(runCfg, "main")
	if !known || strings.Join(exp, ",") != "shop,auth" {
		t.Fatalf("expected: %v %v", exp, known)
	}
	ri := run.Instances["main"]
	if miss := RunMissing(ri, exp); strings.Join(miss, ",") != "auth" {
		t.Errorf("missing: %v", miss)
	}
	if _, err := PickRun(ri, "main", seedTS, exp); err == nil || !strings.Contains(err.Error(), "has no dump of auth") {
		t.Errorf("pick without the dump: %v", err)
	}
	// What backup recorded as missing counts too, once.
	ri.Missing = []string{"auth", "globals"}
	if miss := RunMissing(ri, exp); strings.Join(miss, ",") != "auth,globals" {
		t.Errorf("missing with the record: %v", miss)
	}
	// An older run has no app.yaml of its own: unknown, not an error.
	if c, err := ReadRunConfig(ctx, st, loadRun(t, st, oldTS), nil); c != nil || err != nil {
		t.Errorf("old run: %v %v", c, err)
	}
	if _, known := Expected(nil, "main"); known {
		t.Error("no run config must be unknown")
	}
	if _, known := Expected(runCfg, "ghost"); known {
		t.Error("an instance the run's app.yaml does not name must be unknown")
	}
}

// ADR-0016, ADR-0026: the numbered list restore asks from, with what each
// run holds; only runs whose run.json has the instance with an environment
// file and the run's app.yaml are listed.
func TestRunLines(t *testing.T) {
	ctx := context.Background()
	st := newMemStore()
	seed(t, st, "")
	seedRun(t, st, seedTS, []string{"auth"}, "shop", "auth")
	runs, err := LoadRuns(ctx, st, shopRoot)
	if err != nil || len(runs) != 2 || runs[0].TS != seedTS || runs[1].TS != oldTS {
		t.Fatalf("runs: %+v %v", runs, err)
	}
	// oldTS has no environment file and no app.yaml: not restorable.
	if r := Runs(runs, "main"); len(r) != 1 || r[0].TS != seedTS || strings.Join(r[0].Missing, ",") != "auth" || r[0].Drill != backup.DrillOff {
		t.Errorf("restorable: %+v", r)
	}
	if r := Runs(runs, "ghost"); r != nil {
		t.Errorf("unknown instance: %+v", r)
	}
	// A folder without run.json is a run that did not end; a corrupt run.json is left out too.
	st.Put(ctx, backup.RunDir(shopRoot, "2026-10-17T030000Z")+"main/shop.dump", strings.NewReader("PGDMP"), 5)
	st.Put(ctx, backup.RunKey(shopRoot, "2026-10-18T030000Z"), strings.NewReader("{"), 1)
	if r, err := LoadRuns(ctx, st, shopRoot); err != nil || len(r) != 2 || r[0].TS != seedTS {
		t.Errorf("unfinished and corrupt runs: %+v %v", r, err)
	}

	runsZone = time.FixedZone("", 7*3600)
	defer func() { runsZone = time.Local }()
	// The older run gets its files.
	seedRun(t, st, oldTS, nil, "shop")
	runs, _ = LoadRuns(ctx, st, shopRoot)
	infos := Runs(runs, "main")
	if len(infos) != 2 {
		t.Fatalf("runs: %+v", infos)
	}
	for i, want := range [][]string{
		{seedTS + " (10:00 +07)  missing: auth, not test-restored", "    shop 500 rows, 10 B"},
		{oldTS + " (10:00 +07)  complete, not test-restored", "    shop 0 rows, 9 B"},
	} {
		if got := RunLines(infos[i]); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("run %d:\n%s\nwant:\n%s", i, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	// The drill of the run itself: passed carries how long the load took.
	editRun(t, st, seedTS, func(r *backup.Run) {
		r.Instances["main"].Missing = nil
		r.Instances["main"].Drill = &backup.DrillOutcome{Result: backup.DrillPassed, TookSeconds: 12.5}
	})
	editRun(t, st, oldTS, func(r *backup.Run) {
		r.Instances["main"].Drill = &backup.DrillOutcome{Result: backup.DrillFailed, TookSeconds: 3, Reasons: []string{"rows"}}
	})
	runs, _ = LoadRuns(ctx, st, shopRoot)
	infos = Runs(runs, "main")
	if infos[0].Drill != backup.DrillPassed || infos[0].TookSeconds != 12.5 || infos[1].Drill != backup.DrillFailed || infos[1].TookSeconds != 0 {
		t.Errorf("drills: %+v", infos)
	}
	if got := RunLines(infos[0])[0]; got != seedTS+" (10:00 +07)  complete, drill passed" {
		t.Errorf("drill passed: %q", got)
	}
	if got := RunLines(infos[1])[0]; got != oldTS+" (10:00 +07)  complete, drill failed" {
		t.Errorf("drill failed: %q", got)
	}
	// Late in the UTC day the local date differs: it is shown.
	if got := localTime("2026-10-16T200000Z"); got != " (2026-10-17 03:00 +07)" {
		t.Errorf("localTime = %q", got)
	}
}

// CheckApp tries the database that was loaded, under the name it got.
func TestCheckAppFollowsRenamedDatabase(t *testing.T) {
	loaded := map[string]*backup.Dump{"shop_restored_20261016_0300": {Tables: []pg.Table{{Schema: "public", Name: "orders"}}}}
	renamed := map[string]string{"shop": "shop_restored_20261016_0300"}
	sh := &recShell{}
	ac := CheckApp(context.Background(), sh, "postgres:17", "host", "postgres://app:pw@db:5432/shop", loaded, renamed)
	if ac.Database != "shop_restored_20261016_0300" || ac.User != "app" {
		t.Errorf("check: %+v", ac)
	}
	if len(sh.cmds) == 0 || !strings.Contains(strings.Join(append(sh.cmds[0].Args, sh.cmds[0].Env...), " "), "shop_restored_20261016_0300") {
		t.Errorf("must connect to the loaded database: %+v", sh.cmds)
	}
	ac = CheckApp(context.Background(), &recShell{}, "postgres:17", "host", "postgres://app:pw@db:5432/other", loaded, renamed)
	if ac.OK || !strings.Contains(ac.Error, "did not load") {
		t.Errorf("other database: %+v", ac)
	}
}

// recShell records commands and answers every query with no rows.
type recShell struct{ cmds []pg.Cmd }

func (s *recShell) Run(_ context.Context, c pg.Cmd) (pg.Result, error) {
	s.cmds = append(s.cmds, c)
	return pg.Result{}, nil
}
