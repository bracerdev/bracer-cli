package drill

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/restore"
)

const (
	off       = ", or turn the drill of main off: bracer remove shop, then bracer init shop"
	otherHere = "answer r to try again, or n to turn its drill off"
	otherAway = "the next run tries again" + off
	imageHere = "answer i to enter another Docker image for this drill, or n to turn its drill off"
	imageAway = "bracer remove shop, then bracer init shop: when its check asks about the drill of main, answer i to enter another Docker image, or n to turn it off"
	differs   = "the next run backs up and test-restores again; until one passes, do not rely on this backup: its restored copy differs from what it recorded"
	pullAway  = "check that this machine can pull postgres:17: docker pull postgres:17; the next run tries again" + off
	pullHere  = "check that this machine can pull postgres:17: docker pull postgres:17, then answer r; answer i if the image name is wrong, or n to turn its drill off"
)

// BR-030: the one Fix line of each cause, at init's prompt and elsewhere
// (backup spec, Drill section, the Fix table). Only an image or a pull
// failure points at another image; only what can change on another try
// says to try again.
func TestBR030Fix(t *testing.T) {
	ir := &InstanceResult{Name: "main", Image: "postgres:17", Disk: &restore.DiskCheck{Checked: true, FreeBytes: 300 << 20, NeedBytes: 700 << 20}}
	running := &drillError{docker: pg.WhyDockerDown(errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock"), "deploy", "")}
	denied := &drillError{docker: pg.WhyDockerDown(errors.New("permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock"), "deploy", "")}
	rootless := &drillError{docker: pg.WhyDockerDown(errors.New("Cannot connect to the Docker daemon at unix:///run/user/1000/docker.sock"), "deploy", "")}
	pull := &drillError{image: "postgres:17"}
	none := &drillError{}
	for _, tc := range []struct {
		name           string
		c              cause
		why            *drillError
		prompt, beyond string
	}{
		{"disk short", causeDiskShort, none,
			"free up at least 400.0 MB for Docker, then answer r; or answer n to turn its drill off",
			"free up at least 400.0 MB for Docker" + off},
		{"disk full", causeDiskFull, none,
			"free up disk space for Docker (300.0 MB was free when the drill started), then answer r; or answer n to turn its drill off",
			"free up disk space for Docker (300.0 MB was free when the drill started)" + off},
		{"disk not measured", causeDiskUnknown, none,
			"check that Docker can run postgres:17 and that docker info names its root directory, then answer r; or answer n to turn its drill off",
			"check that Docker can run postgres:17 and that docker info names its root directory" + off},
		{"image", causeImage, none, imageHere, imageAway},
		{"pull", causePull, pull, pullHere, pullAway},
		{"Docker not running", causeDocker, running,
			"start Docker: sudo systemctl enable --now docker, then answer r; or answer n to turn its drill off",
			"start Docker: sudo systemctl enable --now docker" + off},
		{"Docker refuses the user", causeDocker, denied,
			"answer q, let deploy use Docker: sudo usermod -aG docker deploy (the docker group is equivalent to root), then log out and back in, then run bracer init shop again; or answer n to turn its drill off",
			"let deploy use Docker: sudo usermod -aG docker deploy (the docker group is equivalent to root), then log out and back in" + off},
		{"rootless Docker not running", causeDocker, rootless,
			"start the rootless Docker of deploy: systemctl --user enable --now docker, as deploy, then answer r; or answer n to turn its drill off",
			"start the rootless Docker of deploy: systemctl --user enable --now docker, as deploy" + off},
		{"unsupported", causeUnsupported, none,
			"this backup cannot be test-restored in a temporary Postgres for the reason above, whatever the image; answer n to turn its drill off",
			"this backup cannot be test-restored in a temporary Postgres for the reason above, whatever the image; turn the drill of main off: bracer remove shop, then bracer init shop"},
		{"mismatch", causeMismatch, none,
			"answer r to test-restore again; if it differs again, this backup does not restore what it recorded and cannot be relied on, so answer n only to accept backups that are never test-restored",
			differs},
		{"archive unreadable", causeBroken, none,
			"answer r to back up and test-restore again; until one passes, do not rely on this backup: its archive cannot be read, so answer n only to accept backups that are never test-restored",
			"the next run backs up and test-restores again; until one passes, do not rely on this backup: its archive cannot be read"},
		{"backup incomplete", causeBackup, none, "", ""},
		{"other", causeOther, none, otherHere, otherAway},
	} {
		if got := fixText(tc.c, tc.why, "shop", "main", ir, InitPrompt); got != tc.prompt {
			t.Errorf("%s at init's prompt: %q", tc.name, got)
		}
		if got := fixText(tc.c, tc.why, "shop", "main", ir, Elsewhere); got != tc.beyond {
			t.Errorf("%s elsewhere: %q", tc.name, got)
		}
		for _, aud := range []Audience{InitPrompt, InitRun, Elsewhere} {
			got := fixText(tc.c, tc.why, "shop", "main", ir, aud)
			if tc.c != causeImage && tc.c != causePull && strings.Contains(got, "image") && tc.c != causeUnsupported {
				t.Errorf("%s points at an image: %q", tc.name, got)
			}
			if aud != Elsewhere && (strings.Contains(got, "bracer remove") || strings.Contains(got, "next run")) {
				t.Errorf("%s in init says to remove the app or wait for a next run: %q", tc.name, got)
			}
		}
	}
	if got := fixText(causeDiskFull, none, "shop", "main", nil, Elsewhere); got != "free up disk space for Docker"+off {
		t.Errorf("disk full, not measured: %q", got)
	}
}

// BR-030: Docker that does not answer gets the Fix of why, told apart by
// the same classification as init's step 0 (pg.WhyDockerDown), for the
// user the jobs run as; the step records it in run.json.
func TestBR030DockerFix(t *testing.T) {
	for msg, fix := range map[string]string{
		"Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?":                        "start Docker: sudo systemctl enable --now docker" + off,
		"permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock: dial unix: connect": "let deploy use Docker: sudo usermod -aG docker deploy (the docker group is equivalent to root), then log out and back in" + off,
		"Cannot connect to the Docker daemon at unix:///run/user/1000/docker.sock. Is the docker daemon running?":                  "start the rootless Docker of deploy: systemctl --user enable --now docker, as deploy" + off,
	} {
		st := newMemStore()
		run := seed(t, st, "")
		f := newFakeDocker()
		f.noDocker, f.dockerErr = true, msg
		cfg := baseCfg()
		bres := backupResult(cfg)
		Step(cfg, deps(f, st), nil)(context.Background(), run, bres)
		want := "Docker is not available for the temporary Postgres: docker info: exit 1: " + msg + "\nFix: " + fix
		if got := run.Instances["main"].Drill.Reasons; !equalStrings(got, []string{want}) {
			t.Errorf("run.json for %q:\n%q\nwant %q", msg, got, want)
		}
	}
	// Rootless as the jobs reach it (runtime.docker_host), whatever docker says.
	f := newFakeDocker()
	f.noDocker, f.dockerErr = true, "Cannot connect to the Docker daemon"
	cfg := baseCfg()
	cfg.Runtime.DockerHost = "unix:///run/user/1000/docker.sock"
	_, err := Run(context.Background(), cfg, seed(t, newMemStore(), ""), deps(f, newMemStore()))
	if why := asDrillError(err); why.cause != causeDocker || !why.docker.Rootless || why.docker.User != "deploy" {
		t.Errorf("rootless by endpoint: %+v", why)
	}
}

// BR-030: an image that cannot be pulled (a registry rate limit, no DNS,
// no network) is not fixed by another image: the pull Fix, whether the
// official-image check or the volume inspection finds it.
func TestBR030PullFix(t *testing.T) {
	const limit = "toomanyrequests: You have reached your pull rate limit"
	for _, tc := range []struct{ inspect, pull, reason string }{
		{"{{.Id}}", limit, "image postgres:17 is not on this machine and cannot be pulled: docker pull: exit 1: " + limit},
		{"{{json .Config.Volumes}}", limit, "inspect postgres:17: pull postgres:17: docker pull: exit 1: " + limit},
		// Pulled, and still not there to inspect.
		{"{{json .Config.Volumes}}", "", "inspect postgres:17: docker image inspect: exit 1: Error: No such image"},
	} {
		for _, atPrompt := range []bool{true, false} {
			st := newMemStore()
			run := br030Run(t, st, 1<<20)
			f := newFakeDocker()
			f.inspectFail, f.pullErr = tc.inspect, tc.pull
			cfg := baseCfg()
			cfg.Instances[0].SetDrillImage("postgres:17")
			d := depsFor(officialImage{f}, st)
			if atPrompt {
				d.Audience = InitPrompt
			}
			bres := backupResult(cfg)
			Step(cfg, d, nil)(context.Background(), run, bres)
			fix := pullAway
			if atPrompt {
				fix = pullHere
			}
			if got := bres.Instance("main").DrillFailed; !equalStrings(got, []string{tc.reason + "\nFix: " + fix}) {
				t.Errorf("%s, at prompt %v: %q", tc.inspect, atPrompt, got)
			}
		}
	}
}

// BR-030: the image of a local source that cannot be built (its base not
// pulled, its packages not fetched) gets the pull Fix for its base image.
func TestBR030LocalBuildFix(t *testing.T) {
	f := newFakeDocker()
	f.inspectFail = localImageRepo // not built yet; docker build is not faked, so it fails
	rt := &dockerRuntime{sh: f}
	_, err := rt.buildLocalImage(context.Background(), "shop", &pg.Env{ServerVersionNum: 170006}, nil)
	why := asDrillError(err)
	if why.cause != causePull || why.image != "postgres:17" || !strings.HasPrefix(why.msg, "build bracer-drill/shop:") {
		t.Fatalf("%+v", why)
	}
	if got := fixText(why.cause, why, "shop", "main", nil, Elsewhere); got != pullAway {
		t.Errorf("%q", got)
	}
}

// BR-030: pg_restore of the temporary Postgres older than the pg_dump
// that made the dumps is the image's: another image has a newer one.
func TestBR030ToolTooOld(t *testing.T) {
	st := newMemStore()
	run := br030Run(t, st, 1<<20)
	f := newFakeDocker()
	f.toolVersion = "16.4"
	cfg := baseCfg()
	cfg.Instances[0].SetDrillImage("postgres:17")
	bres := backupResult(cfg)
	Step(cfg, depsFor(officialImage{f}, st), nil)(context.Background(), run, bres)
	want := "dumps were made with pg_dump 17; pg_restore 16.4 in postgres:17 cannot read them\nFix: " + imageAway
	if got := bres.Instance("main").DrillFailed; !equalStrings(got, []string{want}) {
		t.Errorf("%q", got)
	}
}

// BR-030: what a comparison line env-up cannot fix means in the temporary
// Postgres: another image has it, no image can take it, or the archive
// cannot be read (spec backup § Drill).
func TestBR030CheckCause(t *testing.T) {
	for _, tc := range []struct {
		ch   restore.Check
		want cause
	}{
		{restore.Check{Area: "version", Name: "server", Detail: "target is PostgreSQL 16.4, the backup is from 17.6"}, causeImage},
		{restore.Check{Area: "extension", Name: "shop: postgis", Detail: "the target server does not have it"}, causeImage},
		{restore.Check{Area: "preload", Name: "timescaledb", Detail: "the target server does not load it"}, causeImage},
		{restore.Check{Area: "preload", Name: "timescaledb", Detail: "cannot read shared_preload_libraries on the target"}, causeOther},
		{restore.Check{Area: "library", Name: "x.so", Detail: "the target does not have it"}, causeImage},
		{restore.Check{Area: "text search file", Name: "x.dict", Detail: "cannot check that the target has it: its directory is not readable"}, causeOther},
		{restore.Check{Area: "locale", Name: "de_DE.UTF-8", Detail: "locale not available"}, causeImage},
		{restore.Check{Area: "collation", Name: "public.x", Detail: "the target cannot create it"}, causeImage},
		{restore.Check{Area: "tablespace", Name: "fast", Detail: "objects of the backup live in it; the target does not have it"}, causeUnsupported},
		{restore.Check{Area: "role", Name: "users", Detail: "the run has no globals"}, causeUnsupported},
		{restore.Check{Area: "setting", Name: "work_mem", Detail: "64MB in the backup (set on the server's command line)"}, causeUnsupported},
		{restore.Check{Area: "database", Name: "shop attributes", Detail: "could not read the database's entries from the archive"}, causeBroken},
		{restore.Check{Area: "database", Name: "CREATEDB", Detail: "postgres needs CREATEDB"}, causeOther},
		{restore.Check{Area: "locks", Name: "max_locks_per_transaction", Detail: "64, the restore needs 128"}, causeOther},
	} {
		if got := checkCause(tc.ch); got != tc.want {
			t.Errorf("%s %s: %d, want %d", tc.ch.Area, tc.ch.Name, got, tc.want)
		}
	}
}

// BR-030: the first round's lines that env-up cannot fix fail the drill
// with the cause of the most telling one: a tablespace beside a missing
// extension cannot be taken by any image; later rounds are the drill's.
func TestBR030FailRound(t *testing.T) {
	ext := restore.Check{Area: "extension", Name: "shop: postgis", Detail: "the target server does not have it", Fix: restore.FixManual}
	ts := restore.Check{Area: "tablespace", Name: "fast", Detail: "objects of the backup live in it; the target does not have it", Fix: restore.FixManual}
	attrs := restore.Check{Area: "database", Name: "shop attributes", Detail: "could not read the database's entries from the archive", Fix: restore.FixManual}
	sql := restore.Check{Area: "role", Name: "shop_app", Detail: "missing", Fix: restore.FixSQL}
	for _, tc := range []struct {
		name   string
		round  int
		checks []restore.Check
		hasUp  bool
		stop   bool
		fix    string
	}{
		{"extension", 1, []restore.Check{sql, ext}, true, true, imageAway},
		{"extension and tablespace", 1, []restore.Check{ext, ts}, true, true,
			"this backup cannot be test-restored in a temporary Postgres for the reason above, whatever the image; turn the drill of main off: bracer remove shop, then bracer init shop"},
		{"archive", 1, []restore.Check{ext, attrs}, true, true,
			"the next run backs up and test-restores again; until one passes, do not rely on this backup: its archive cannot be read"},
		{"env-up has it", 1, []restore.Check{sql}, true, false, ""},
		{"env-up did not do it", 2, []restore.Check{ext}, true, true, otherAway},
	} {
		ir := &InstanceResult{Name: "main", Image: "postgres:17"}
		if stop := ir.failRound("postgres:17", tc.round, tc.checks, tc.hasUp); stop != tc.stop {
			t.Errorf("%s: stop %v", tc.name, stop)
			continue
		}
		if !tc.stop {
			if ir.Error != "" {
				t.Errorf("%s: %q", tc.name, ir.Error)
			}
			continue
		}
		ir.settle("shop", Elsewhere)
		if ir.Fix != tc.fix {
			t.Errorf("%s: %q", tc.name, ir.Fix)
		}
	}
}

// BR-030: with every database loaded, a target that no longer matches the
// backup is a copy that differs from what the run recorded.
func TestBR030FailAfterLoad(t *testing.T) {
	ir := &InstanceResult{Name: "main", Image: "postgres:17", Databases: []*DatabaseResult{{DB: "shop", OK: true, checked: true}}}
	if ir.failAfterLoad(nil) || ir.Error != "" {
		t.Fatalf("nothing left: %+v", ir)
	}
	left := []restore.Check{{Area: "role", Name: "shop_app", Detail: "differs"}}
	if !ir.failAfterLoad(left) || len(ir.Checks) != 1 {
		t.Fatalf("left: %+v", ir)
	}
	ir.settle("shop", Elsewhere)
	want := []string{"after the load the target no longer matches the backup: role shop_app (differs)\nFix: " + differs}
	if got := ir.Failures(); !equalStrings(got, want) {
		t.Errorf("%q", got)
	}
}

// BR-030: the cause of an instance is the highest of its failures, not the
// first or the last; every failure stays a reason, the copy that differs
// is still said after a cleanup that failed, and the one Fix goes on the
// last line. A copy differs only when every database loaded.
func TestBR030Settle(t *testing.T) {
	afterLoad := failf(causeMismatch, "after the load the target no longer matches the backup: role shop_app (differs)")
	drop := failf(causeOther, "drop the restored databases: psql: exit 1")
	verified := func(db string, ok bool) *DatabaseResult { return &DatabaseResult{DB: db, OK: ok, checked: true} }
	loadErr := func(db, msg string) *DatabaseResult { return &DatabaseResult{DB: db, Error: msg} }
	const differsLine = "shop: the restored copy differs from what the backup recorded"
	for _, tc := range []struct {
		name  string
		errs  []error
		dbs   []*DatabaseResult
		lines []string
	}{
		{"rows differ, then the cleanup fails", []error{drop}, []*DatabaseResult{verified("auth", true), verified("shop", false)},
			[]string{"drop the restored databases: psql: exit 1", differsLine + "\nFix: " + differs}},
		{"after-load mismatch, then the cleanup fails", []error{afterLoad, drop}, []*DatabaseResult{verified("shop", true)},
			[]string{"after the load the target no longer matches the backup: role shop_app (differs)", "drop the restored databases: psql: exit 1\nFix: " + differs}},
		{"the cleanup fails, then the after-load mismatch", []error{drop, afterLoad}, []*DatabaseResult{verified("shop", true)},
			[]string{"drop the restored databases: psql: exit 1", "after the load the target no longer matches the backup: role shop_app (differs)\nFix: " + differs}},
		{"after-load mismatch beside a database that did not load", []error{afterLoad}, []*DatabaseResult{verified("auth", true), loadErr("shop", "pg_restore: error: could not execute query")},
			[]string{"after the load the target no longer matches the backup: role shop_app (differs)", "shop: pg_restore: error: could not execute query\nFix: " + otherAway}},
		{"rows differ beside a database that did not load", nil, []*DatabaseResult{verified("auth", false), loadErr("shop", "pg_restore: error: could not execute query")},
			[]string{"auth: the restored copy differs from what the backup recorded", "shop: pg_restore: error: could not execute query\nFix: " + otherAway}},
		{"loaded but never compared", []error{failf(causeOther, "read the temporary Postgres after the load: psql: exit 2")}, []*DatabaseResult{{DB: "shop"}},
			[]string{"read the temporary Postgres after the load: psql: exit 2\nFix: " + otherAway}},
		{"a database that did not load alone", nil, []*DatabaseResult{loadErr("shop", "pg_restore: error: could not execute query")},
			[]string{"shop: pg_restore: error: could not execute query\nFix: " + otherAway}},
		{"disk full while loading, beside a mismatch", []error{afterLoad}, []*DatabaseResult{loadErr("shop", `pg_restore: error: could not extend file "base/1/2": No space left on device`)},
			[]string{"after the load the target no longer matches the backup: role shop_app (differs)", `shop: pg_restore: error: could not extend file "base/1/2": No space left on device` + "\nFix: free up disk space for Docker (2.0 GB was free when the drill started)" + off}},
		{"shared memory while loading", nil, []*DatabaseResult{loadErr("shop", `pg_restore: error: could not resize shared memory segment "/PostgreSQL.1" to 8388608 bytes: No space left on device`)},
			[]string{`shop: pg_restore: error: could not resize shared memory segment "/PostgreSQL.1" to 8388608 bytes: No space left on device` + "\nFix: " + otherAway}},
		{"an image that cannot be pulled for a full disk", []error{pullError("postgres:17", errors.New("pull postgres:17: write /var/lib/docker/tmp/x: no space left on device"))}, nil,
			[]string{"pull postgres:17: write /var/lib/docker/tmp/x: no space left on device\nFix: free up disk space for Docker (2.0 GB was free when the drill started)" + off}},
		{"the image, then the drill's own", []error{failf(causeImage, "image postgres:17 has no extension postgis"), drop}, nil,
			[]string{"image postgres:17 has no extension postgis", "drop the restored databases: psql: exit 1\nFix: " + imageAway}},
	} {
		ir := &InstanceResult{Name: "main", Image: "postgres:17", Disk: &restore.DiskCheck{Checked: true, FreeBytes: 2 << 30, NeedBytes: 1 << 30}, Databases: tc.dbs}
		for _, err := range tc.errs {
			ir.fail(err)
		}
		ir.settle("shop", Elsewhere)
		if got := ir.Failures(); !equalStrings(got, tc.lines) {
			t.Errorf("%s:\n%q\nwant %q", tc.name, got, tc.lines)
		}
	}
}

// BR-030: the image's own failure (an extension it does not have, as
// checkImage says it) gets the image Fix: inside init's preflight the keys
// of its question, elsewhere setting the app up again. The step carries it
// to run.json and the backup's result, one Fix line.
func TestBR030ExtensionFix(t *testing.T) {
	for _, atPrompt := range []bool{true, false} {
		st := newMemStore()
		run := br030Run(t, st, 1<<20)
		run.Instances["main"].Databases["shop"].Extensions = []pg.Extension{{Name: "postgis"}}
		f := newFakeDocker()
		cfg := baseCfg()
		cfg.Instances[0].SetDrillImage("postgres:17")
		bres := backupResult(cfg)
		d := depsFor(officialImage{f}, st)
		if atPrompt {
			d.Audience = InitPrompt
		}
		Step(cfg, d, nil)(context.Background(), run, bres)
		fix := imageAway
		if atPrompt {
			fix = imageHere
		}
		want := []string{"image postgres:17 has no extension postgis\nFix: " + fix}
		if got := bres.Instance("main").DrillFailed; !equalStrings(got, want) {
			t.Errorf("at prompt %v: %q", atPrompt, got)
		}
		if got := run.Instances["main"].Drill.Reasons; !equalStrings(got, want) {
			t.Errorf("run.json, at prompt %v: %q", atPrompt, got)
		}
	}
}

// BR-030: in a nightly run the disk may fill after the disk check passed;
// the Docker daemon saying so when the temporary Postgres starts gets the
// disk Fix in run.json and in the backup's result.
func TestBR030DiskFullNightly(t *testing.T) {
	st := newMemStore()
	run := br030Run(t, st, 1<<20)
	f := newFakeDocker()
	f.runStderr = "docker: Error response from daemon: mkdir /var/lib/docker/overlay2/x: no space left on device."
	cfg := baseCfg()
	cfg.Instances[0].SetDrillImage("postgres:17")
	bres := backupResult(cfg)
	Step(cfg, depsFor(officialImage{f}, st), nil)(context.Background(), run, bres)
	const fix = "\nFix: free up disk space for Docker (1.0 TB was free when the drill started)" + off
	reasons := run.Instances["main"].Drill.Reasons
	if len(reasons) != 1 || !strings.HasPrefix(reasons[0], "start postgres:17: ") || !strings.HasSuffix(reasons[0], "no space left on device."+fix) {
		t.Errorf("run.json: %q", reasons)
	}
	if got := bres.Instance("main").DrillFailed; !equalStrings(got, reasons) {
		t.Errorf("backup result: %q", got)
	}
}

// BR-030: in init's real run no app.yaml and no timer exist: the way on is
// running init again, after the cause's own action; turning the drill off
// or another image is offered only where init's check meets the same
// failure and asks (backup spec, Drill section, the Fix table).
func TestBR030FixInitRun(t *testing.T) {
	ir := &InstanceResult{Name: "main", Image: "postgres:17", Disk: &restore.DiskCheck{Checked: true, FreeBytes: 300 << 20, NeedBytes: 700 << 20}}
	down := func(msg string) *drillError {
		return &drillError{docker: pg.WhyDockerDown(errors.New(msg), "deploy", "")}
	}
	const again = "run bracer init shop again"
	const asked = "answer n when its check asks about the drill of main"
	for _, tc := range []struct {
		name string
		c    cause
		why  *drillError
		want string
	}{
		{"disk short", causeDiskShort, &drillError{}, "free up at least 400.0 MB for Docker, then " + again + "; if you cannot, " + asked},
		{"disk full", causeDiskFull, &drillError{}, "free up disk space for Docker (300.0 MB was free when the drill started), then " + again},
		{"disk not measured", causeDiskUnknown, &drillError{}, "check that Docker can run postgres:17 and that docker info names its root directory, then " + again + "; if you cannot, " + asked},
		{"archive unreadable", causeBroken, &drillError{}, "this backup's archive cannot be read: " + again + ", and if it happens again, do not rely on backups of main until a drill passes"},
		{"mismatch", causeMismatch, &drillError{}, "this backup did not restore what it recorded: " + again + ", and if it happens again, do not rely on backups of main until a drill passes"},
		{"backup incomplete", causeBackup, &drillError{}, ""},
		{"unsupported", causeUnsupported, &drillError{}, "this backup cannot be test-restored in a temporary Postgres for the reason above, whatever the image; " + again + " and " + asked},
		{"image", causeImage, &drillError{}, again + ": its check asks about the drill of main; answer i to enter another Docker image, or n to turn it off"},
		{"pull", causePull, &drillError{image: "postgres:17"}, "check that this machine can pull postgres:17: docker pull postgres:17, then " + again + "; if the image name is wrong, answer i when its check asks about the drill of main"},
		{"Docker not running", causeDocker, down("Cannot connect to the Docker daemon at unix:///var/run/docker.sock"), "start Docker: sudo systemctl enable --now docker, then " + again},
		{"Docker refuses the user", causeDocker, down("permission denied while trying to connect to the Docker daemon socket"),
			"let deploy use Docker: sudo usermod -aG docker deploy (the docker group is equivalent to root), then log out and back in, then " + again},
		{"rootless Docker not running", causeDocker, down("Cannot connect to the Docker daemon at unix:///run/user/1000/docker.sock"),
			"start the rootless Docker of deploy: systemctl --user enable --now docker, as deploy, then " + again},
		{"other", causeOther, &drillError{}, again},
	} {
		if got := fixText(tc.c, tc.why, "shop", "main", ir, InitRun); got != tc.want {
			t.Errorf("%s:\n%q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// BR-030: the audience reaches the Fix line of a whole drill through Deps:
// init's real run gets init's sentences, a run of an app that exists (the
// zero value) the nightly ones.
func TestBR030AudienceThroughStep(t *testing.T) {
	for aud, fix := range map[Audience]string{
		InitRun:   "run bracer init shop again: its check asks about the drill of main; answer i to enter another Docker image, or n to turn it off",
		Elsewhere: imageAway,
	} {
		st := newMemStore()
		run := br030Run(t, st, 1<<20)
		run.Instances["main"].Databases["shop"].Extensions = []pg.Extension{{Name: "postgis"}}
		cfg := baseCfg()
		cfg.Instances[0].SetDrillImage("postgres:17")
		d := depsFor(officialImage{newFakeDocker()}, st)
		d.Audience = aud
		Step(cfg, d, nil)(context.Background(), run, backupResult(cfg))
		if got := run.Instances["main"].Drill.Reasons; !equalStrings(got, []string{"image postgres:17 has no extension postgis\nFix: " + fix}) {
			t.Errorf("audience %d: %q", aud, got)
		}
	}
}

// BR-030: when Docker does not answer, no temporary Postgres starts, yet
// the instance's drill result is the failure with its Fix, so the report
// (drill.result, drill.error, drill.fix) says what run.json says.
func TestBR030NoDockerReported(t *testing.T) {
	st := newMemStore()
	run := seed(t, st, "")
	f := newFakeDocker()
	f.noDocker = true
	cfg := baseCfg()
	bres := backupResult(cfg)
	Step(cfg, deps(f, st), nil)(context.Background(), run, bres)
	x, ok := bres.Instance("main").Drill.(*InstanceResult)
	if !ok || x.Result != backup.DrillFailed || x.OK || !strings.HasPrefix(x.Error, "Docker is not available") ||
		x.Fix != "start Docker: sudo systemctl enable --now docker"+off {
		t.Fatalf("drill result: %+v", bres.Instance("main").Drill)
	}
	reasons := run.Instances["main"].Drill.Reasons
	if !equalStrings(reasons, []string{x.Error + "\nFix: " + x.Fix}) || !equalStrings(bres.Instance("main").DrillFailed, reasons) {
		t.Errorf("run.json %q, result %q", reasons, bres.Instance("main").DrillFailed)
	}
	data, _ := json.Marshal(bres.Instance("main"))
	if !strings.Contains(string(data), `"result":"failed"`) || !strings.Contains(string(data), `"fix":"start Docker`) {
		t.Errorf("report: %s", data)
	}
}

// diskFull: what says the disk filled up is told apart from other drill
// failures, in Postgres's words and in the Docker daemon's; shared memory
// (/dev/shm) running out is not the disk.
func TestBR030DiskFull(t *testing.T) {
	for text, full := range map[string]bool{
		`postgres: create shop_restored_1: psql: PANIC:  could not write to file "pg_wal/xlogtemp.535": No space left on device`:                                          true,
		`pg_restore: error: could not extend file "base/16384/16390": No space left on device`:                                                                            true,
		`ERROR:  could not extend file "base/16384/16390": wrote only 4096 of 8192 bytes at block 3`:                                                                      true,
		`pull postgres:17: docker pull -q postgres:17: exit 1: failed to register layer: write /usr/lib/x.so: no space left on device`:                                    true,
		`create volume bracer-drill-shop-aaaaaa: docker volume create: exit 1: Error response from daemon: mkdir /var/lib/docker/volumes/x: no space left on device`:      true,
		`pg_restore: error: could not execute query: ERROR:  could not resize shared memory segment "/PostgreSQL.2058389298" to 8388608 bytes: No space left on device`:   false,
		`could not resize shared memory segment "/PostgreSQL.1" to 8388608 bytes: No space left on device; db: could not extend file "base/1/2": No space left on device`: true,
		"extension postgis is not in the image":                        false,
		"FATAL:  the database system is not yet accepting connections": false,
	} {
		if diskFull(text) != full {
			t.Errorf("diskFull(%q) = %v", text, !full)
		}
	}
}
