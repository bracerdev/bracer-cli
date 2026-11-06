package drill

import (
	"context"
	"strings"
	"testing"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/pg"
)

// br030Run is the seeded run with the database's size at the source set:
// the dump stays 10 bytes, as a dump of compressible data or a schema-only
// one is far smaller than what it restores.
func br030Run(t *testing.T, st *memStore, sourceBytes int64) *backup.Run {
	run := seed(t, st, "")
	run.Instances["main"].Databases["shop"].SourceBytes = sourceBytes
	return run
}

// BR-030: a drill needs TempPostgresBytes for the temporary Postgres itself
// plus SourceFactor × the databases' size at the source, whatever the size
// of the dump. Too little free disk stops it before the temporary Postgres
// starts, with a Fix line, and the step records it in run.json and in the
// backup's result.
func TestBR030DiskNeedBlocksBeforeTempPostgres(t *testing.T) {
	st := newMemStore()
	run := br030Run(t, st, 700<<20) // 700 MB at the source, 10 bytes of dump
	f := newFakeDocker()
	f.dfAvailKB = 1 << 20 // 1 GB free: far above any multiple of the dump
	cfg := baseCfg()
	cfg.Instances[0].SetDrillImage("postgres:17")
	bres := backupResult(cfg)
	var kept *Result
	Step(cfg, depsFor(officialImage{f}, st), func(r *Result) { kept = r })(context.Background(), run, bres)

	const why = "not enough disk space for a drill: 1.0 GB free at volume bracer-drill-shop-aaaaaa, about 1.8 GB needed (400.0 MB for the temporary Postgres + 2 × the databases' size at the source)"
	const fix = "free up at least 776.0 MB for Docker, or turn the drill of main off: bracer remove shop, then bracer init shop"
	const want = why + "\nFix: " + fix
	x := kept.Instance("main")
	if x == nil || x.OK || x.Error != why || x.Fix != fix || x.cause != causeDiskShort || x.Disk.NeedBytes != TempPostgresBytes+1400<<20 {
		t.Fatalf("instance: %+v", x)
	}
	if containersStarted(f) != 0 {
		t.Errorf("the temporary Postgres must not start: %v", f.calls)
	}
	if len(f.volumes) != 0 || !equalStrings(f.removedVols, []string{"bracer-drill-shop-aaaaaa"}) {
		t.Errorf("the volume created to measure must be removed: %v %v", f.volumes, f.removedVols)
	}
	if d := run.Instances["main"].Drill; d == nil || d.Result != backup.DrillFailed || !equalStrings(d.Reasons, []string{want}) {
		t.Errorf("run.json: %+v", d)
	}
	if ir := bres.Instance("main"); !equalStrings(ir.DrillFailed, []string{want}) {
		t.Errorf("backup result: %+v", ir)
	}

	// Inside init's preflight the Fix line points at the answers of its question.
	f = newFakeDocker()
	f.dfAvailKB = 1 << 20
	st = newMemStore()
	d := depsFor(officialImage{f}, st)
	d.Audience = InitPrompt
	res, err := Run(context.Background(), cfg, br030Run(t, st, 700<<20), d)
	if err != nil {
		t.Fatal(err)
	}
	if x := res.Instance("main"); x.Fix != "free up at least 776.0 MB for Docker, then answer r; or answer n to turn its drill off" || x.Error != why {
		t.Errorf("at the prompt: %+v", x)
	}

	// Just enough: the temporary Postgres starts, with wal_level=minimal.
	f = newFakeDocker()
	f.dfAvailKB = (TempPostgresBytes+1400<<20)>>10 + 1
	st = newMemStore()
	res, err = Run(context.Background(), cfg, br030Run(t, st, 700<<20), depsFor(officialImage{f}, st))
	if err != nil {
		t.Fatal(err)
	}
	if x := res.Instance("main"); strings.HasPrefix(x.Error, "not enough disk space") || containersStarted(f) != 1 {
		t.Errorf("enough disk: %+v calls=%v", x, f.calls)
	}
}

// BR-030: the temporary Postgres starts with wal_level=minimal and no WAL
// senders, so pg_restore --single-transaction writes its data without WAL.
func TestBR030TempPostgresWalMinimal(t *testing.T) {
	f := newFakeDocker()
	st := newMemStore()
	cfg := baseCfg()
	cfg.Instances[0].SetDrillImage("postgres:17")
	if _, err := Run(context.Background(), cfg, br030Run(t, st, 1<<20), depsFor(officialImage{f}, st)); err != nil {
		t.Fatal(err)
	}
	var started []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "run -d ") {
			started = append(started, c)
		}
	}
	if len(started) != 1 || !strings.HasSuffix(started[0], " postgres:17 -c wal_level=minimal -c max_wal_senders=0 -c archive_mode=off") {
		t.Errorf("docker run: %q", started)
	}
}

// BR-030: free disk that cannot be measured stops the drill with a Fix line
// of its own, not the image one.
func TestBR030DiskNotMeasured(t *testing.T) {
	f := newFakeDocker()
	f.dfAvailKB = -1 // df in the image fails, and df on this machine is not docker
	st := newMemStore()
	cfg := baseCfg()
	cfg.Instances[0].SetDrillImage("postgres:17")
	res, err := Run(context.Background(), cfg, br030Run(t, st, 1<<20), depsFor(officialImage{f}, st))
	if err != nil {
		t.Fatal(err)
	}
	x := res.Instance("main")
	if !strings.HasPrefix(x.Error, "cannot check free disk space for a drill: ") || x.Fix != "check that Docker can run postgres:17 and that docker info names its root directory, or turn the drill of main off: bracer remove shop, then bracer init shop" || containersStarted(f) != 0 {
		t.Errorf("result: %+v calls=%v", x, f.calls)
	}
}

// officialImage answers the image checks that come before the disk check
// as an official postgres image would; the rest goes to the fake.
type officialImage struct{ *fakeDocker }

func (o officialImage) Run(ctx context.Context, c pg.Cmd) (pg.Result, error) {
	a := c.Args
	switch {
	case len(a) > 3 && a[0] == "image" && strings.HasPrefix(a[3], "{{json .Config.Entrypoint}}"):
		return pg.Result{Stdout: []byte(`["docker-entrypoint.sh"]` + "\n" + `["postgres"]` + "\n")}, nil
	case a[0] == "run" && hasArg(a, "--entrypoint"):
		return pg.Result{Stdout: []byte("plpgsql.control\n---\nplpgsql.so\n")}, nil
	}
	return o.fakeDocker.Run(ctx, c)
}

// BR-030: the need follows the size at the source, not the dump: a dump
// of compressible data is a fraction of what it restores.
func TestBR030NeedBytes(t *testing.T) {
	const dump, source = 9 << 20, 714 << 20 // measured: 9 MB dump of a 714 MB database
	if got := NeedBytes(source); got != TempPostgresBytes+2*source || got < 1246<<20+38<<20 {
		t.Errorf("NeedBytes(%d) = %d (the restore peaked at 1246 MB over a 38 MB cluster)", source, got)
	}
	if NeedBytes(0) != TempPostgresBytes || TempPostgresBytes+5*dump > 1246<<20 {
		t.Errorf("NeedBytes(0) = %d", NeedBytes(0))
	}
}
