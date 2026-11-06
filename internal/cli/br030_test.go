package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/drill"
)

// BR-030: the preflight shows the Fix line the drill gave a failure, as
// the drill step records it in the backup's result, and adds none: one Fix
// line per failure, none when the drill gives none (the backup's own
// lines say why). Inside init's preflight it is the keys of init's
// question; the drill decides the sentence (its tests in package drill).
func TestBR030PreflightDrillFix(t *testing.T) {
	const ext = "image postgres:17 has no extension postgis"
	for _, tc := range []struct {
		name, why, fix string
	}{
		{"image, at init's prompt", ext, "answer i to enter another Docker image for this drill, or n to turn its drill off"},
		{"anything else, at init's prompt", "start postgres:17: exit 1", "answer r to try again, or n to turn its drill off"},
		{"backup incomplete", "the run is missing auth; nothing to test-restore", ""},
	} {
		reason := tc.why
		if tc.fix != "" {
			reason += "\nFix: " + tc.fix
		}
		res := &backup.Result{Instances: []*backup.InstanceResult{{Name: "main", DrillFailed: []string{reason}}}}
		dres := &drill.Result{Instances: []*drill.InstanceResult{{Name: "main", Image: "postgres:17"}}}
		p := &preflightResult{OK: true}
		preflightDrill(res, dres, []string{"main"}, p)
		if len(p.Lines) != 1 || p.OK {
			t.Fatalf("%s: %+v", tc.name, p)
		}
		l := p.Lines[0]
		if l.Mark != "✗" || l.Text != "main (image postgres:17): "+tc.why || l.Fix != tc.fix {
			t.Errorf("%s:\ntext %q\nfix  %q", tc.name, l.Text, l.Fix)
		}
		var out bytes.Buffer
		printPreflight(&out, p)
		want := 1
		if tc.fix == "" {
			want = 0
		}
		if n := strings.Count(out.String(), "Fix: "); n != want {
			t.Errorf("%s: %d Fix lines:\n%s", tc.name, n, out.String())
		}
	}
}

// BR-030: who reads the drill's Fix lines follows the flags init passes to
// its child: --init-draft --dry-run is the preflight above init's drill
// question, --init-draft alone init's real run (no app.yaml, no timer yet);
// without --init-draft (the nightly run, a --dry-run typed by hand) the app
// exists.
func TestBR030DrillAudience(t *testing.T) {
	for _, tc := range []struct {
		initDraft, dryRun bool
		want              drill.Audience
	}{
		{true, true, drill.InitPrompt},
		{true, false, drill.InitRun},
		{false, false, drill.Elsewhere},
		{false, true, drill.Elsewhere},
	} {
		if got := drillAudience(tc.initDraft, tc.dryRun); got != tc.want {
			t.Errorf("--init-draft %v --dry-run %v: %d", tc.initDraft, tc.dryRun, got)
		}
	}
	if d := drillDeps(backupDeps{}, nil, nil, drill.InitRun); d.Audience != drill.InitRun {
		t.Errorf("drillDeps: %d", d.Audience)
	}
}

// BR-030: [r] runs the same preflight again; what was answered for an
// instance before it in the same round ([n] for main here) is kept, and
// the instance retried is left as it was.
func TestBR030RetryKeepsEarlierAnswers(t *testing.T) {
	dir := t.TempDir()
	draft := dir + "/.app.yaml.init"
	inst := func(name string) config.Instance {
		return config.Instance{Name: name, Connection: config.Connection{Container: name + "-db", User: "postgres", PgMajor: 17, SystemID: config.SystemIDNone}, Databases: config.Databases([]config.Database{{Name: "shop"}})}
	}
	cfg := &config.App{App: "shop", SecretsFile: dir + "/secrets.env", Encryption: "none",
		Instances: []config.Instance{inst("main"), inst("side")},
		Storage:   &config.Storage{Endpoint: "https://s3.example.com", Bucket: "b", Folder: "shop-abc123", AccessKeyEnv: "A", SecretKeyEnv: "S"},
		Schedule:  &config.Schedule{Every: config.EveryDaily, At: []string{"03:00"}, Days: []string{}, Keep: 7, Since: "2026-11-06T00:00:00Z"},
		Runtime:   &config.Runtime{User: "deploy", Path: "/usr/bin:/bin", Docker: "/usr/bin/docker"}}
	var saved []*config.App
	runs := 0
	fail := func(name string) preflightLine {
		return preflightLine{Section: "drill", Instance: name, Area: "drill", Mark: "✗", Text: name + ": not enough disk space for a drill"}
	}
	var out bytes.Buffer
	r := &initRun{
		initDeps: initDeps{
			home:   func() (string, error) { return dir, nil },
			stdout: &out, stderr: io.Discard,
			runSelf: func(_ context.Context, args, _ []string, stdout, _ io.Writer) int {
				runs++
				// The draft as this run reads it.
				c, err := config.Load(draft)
				if err == nil {
					saved = append(saved, c)
				}
				pf := preflightResult{Kind: "backup-dry-run", App: "shop", OK: true}
				if runs == 1 {
					pf.OK, pf.Lines = false, []preflightLine{fail("main"), fail("side")}
				}
				json.NewEncoder(stdout).Encode(pf)
				if pf.OK {
					return exitOK
				}
				return exitError
			},
		},
		ctx: context.Background(), cfg: cfg, appName: "shop",
		p: newPrompter(context.Background(), strings.NewReader("n\nr\n"), &out),
	}
	if err := config.SaveDraft(draft, cfg); err != nil {
		t.Fatal(err)
	}
	if err := r.preflight(draft); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if runs != 2 || strings.Count(out.String(), "[r] retry  [i] enter a Docker image") != 2 {
		t.Fatalf("runs %d:\n%s", runs, out.String())
	}
	if len(saved) != 2 || !saved[1].Instances[0].DrillOff() || saved[1].Instances[1].DrillOff() || saved[1].Instances[1].DrillImage() != "" {
		t.Errorf("the second check must see main off and side unchanged: %+v", saved)
	}
}

// BR-030: bracer status keeps a drill reason's own Fix line under it,
// indented, not as a bare line in the middle of the list.
func TestBR030StatusIndentsDrillFix(t *testing.T) {
	run := &backup.Run{App: "shop", TS: "2026-11-06T030000Z",
		Backup: backup.Outcome{Result: backup.ResultOK, Reasons: []string{}},
		Instances: map[string]*backup.RunInstance{"main": {Missing: []string{}, Drill: &backup.DrillOutcome{Result: backup.DrillFailed,
			Reasons: []string{"not enough disk space for a drill: 300.0 MB free at volume v, about 402.0 MB needed\nFix: free up at least 102.0 MB for Docker, or turn the drill of main off: bracer remove shop, then bracer init shop"}}}}}
	var out bytes.Buffer
	if code := printRun(&out, run); code != exitError {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	want := "  main: test-restore failed: not enough disk space for a drill: 300.0 MB free at volume v, about 402.0 MB needed\n    Fix: free up at least 102.0 MB for Docker, or turn the drill of main off: bracer remove shop, then bracer init shop\n"
	if !strings.Contains(out.String(), want) {
		t.Errorf("status:\n%s", out.String())
	}
}

// BR-030: the real run of init needs the drill's own rule: 300 MB free
// is far more than any multiple of 1 MB of dumps, yet too little for the
// temporary Postgres plus 2 × the databases, so init stops before the run
// and writes nothing.
func TestBR030RealRunNeed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	sh := containerWorld()
	sh.out["df -Pk /var/lib/docker"] = "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 61255492 60948292 307200 99% /\n"
	w := &world{sh: sh}
	code, out, _ := runWorld(t, dir, w, answers(s3Answers, noEncryption, pickAll, drillOn, schedDefault)...)
	if code != exitError || !strings.Contains(out, "  ✗ free up 102.0 MB for the drill (300.0 MB free, about 402.0 MB needed: 400.0 MB for the temporary Postgres + 2 × the databases), or answer n to test-restore on this machine\n") || len(w.selfLog) != 2 {
		t.Errorf("exit %d %q:\n%s", code, w.selfLog, out)
	}
	if _, err := os.Stat(appYAML(dir)); !os.IsNotExist(err) {
		t.Error("nothing may be written")
	}
}

// BR-030: [r] runs the same preflight again with nothing changed (the disk
// was freed meanwhile) and init goes on when it passes.
func TestBR030DrillRetry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	w := &world{sh: containerWorld(), preflight: func(n int) *preflightResult {
		pf := &preflightResult{Kind: "backup-dry-run", App: "shop", OK: true}
		if n == 1 {
			pf.OK = false
			pf.Lines = []preflightLine{{Section: "drill", Instance: "shop-db", Area: "drill", Mark: "✗",
				Text: "shop-db (image postgres:17): not enough disk space for a drill: 58.0 MB free at volume v, about 402.0 MB needed (400.0 MB for the temporary Postgres + 2 × the databases' size at the source)",
				Fix:  "free up at least 344.0 MB for Docker, then answer r; or answer n to turn its drill off"}}
		}
		return pf
	}}
	code, out, errOut := runWorld(t, dir, w, answers(s3Answers, noEncryption, pickAll, drillOn, schedDefault, []string{"r"})...)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "The drill of shop-db fails (above). The nightly drill would fail the same way.\n[r] retry  [i] enter a Docker image for this instance's drill  [n] no drill for this instance  [q] quit: ") {
		t.Errorf("prompt:\n%s", out)
	}
	if n := strings.Count(out, "Checking as the nightly job will run: bracer backup shop --init-draft --dry-run --json"); n != 2 {
		t.Errorf("preflight ran %d times:\n%s", n, out)
	}
	cfg, err := config.Load(appYAML(dir))
	if err != nil {
		t.Fatal(err)
	}
	if in := cfg.Instances[0]; in.DrillOff() || in.DrillImage() != "" {
		t.Errorf("retry must change nothing: %+v", in.Drill)
	}
}
