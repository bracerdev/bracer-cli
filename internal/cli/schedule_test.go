package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"bracer.dev/bracer/internal/config"
)

// testApp is a complete, valid app named app whose files are in home.
func testApp(home, app string) *config.App {
	return &config.App{
		App:         app,
		SecretsFile: filepath.Join(config.AppDir(home, app), config.SecretsFile),
		Instances: []config.Instance{{Name: "main",
			Connection: config.Connection{Container: "shop-db", User: "postgres", PgMajor: 17, SystemID: "7698217040907026472"},
			Databases:  config.Databases{{Name: "shop"}}}},
		Storage: &config.Storage{Endpoint: "https://s3.example", Bucket: "b", Folder: app + "-abc123",
			AccessKeyEnv: config.EnvVarName(app, "s3", "access_key"), SecretKeyEnv: config.EnvVarName(app, "s3", "secret_key")},
		Encryption: "none",
		Schedule:   &config.Schedule{Every: config.EveryDaily, At: []string{"03:00"}, Days: []string{}, Keep: 7},
		Runtime:    &config.Runtime{User: "deploy", Path: "/usr/bin:/bin", Docker: "/usr/bin/docker"},
	}
}

func inputs(cfg *config.App) scheduleInputs {
	return scheduleInputs{cfg: cfg, cfgPath: "/home/deploy/.config/bracer/shop/app.yaml", binary: "/usr/local/bin/bracer", username: "deploy", home: "/home/deploy",
		at: onCalendar(*cfg.Schedule)}
}

// fakeSystemctl records calls and answers `show` from canned properties.
type fakeSystemctl struct {
	calls []string
	show  map[string]map[string]string // unit → properties
	fail  string                       // a call starting with this fails
}

func (f *fakeSystemctl) run(_ context.Context, args ...string) (string, error) {
	call := strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if f.fail != "" && strings.HasPrefix(call, f.fail) {
		return "", fmt.Errorf("systemctl %s: exit status 1: Failed to enable unit", call)
	}
	if args[0] == "show" {
		unit := args[len(args)-1]
		var b strings.Builder
		for _, p := range strings.Split(args[2], ",") {
			fmt.Fprintf(&b, "%s=%s\n", p, f.show[unit][p])
		}
		return b.String(), nil
	}
	return "", nil
}

// testDeps: a temp unit directory, a fake systemctl, root, stdin from `in`.
// The jobs' user deploy has the test's home.
func testDeps(t *testing.T, sc *fakeSystemctl, in string) (scheduleDeps, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errb bytes.Buffer
	home := testHome(t)
	d := scheduleDeps{
		unitDir:    t.TempDir(),
		systemctl:  sc.run,
		hasSystemd: func() bool { return true },
		euid:       0,
		executable: func() string { return "/usr/local/bin/bracer" },
		invoker:    func() string { return "deploy" },
		lookupUser: func(name string) (account, error) {
			switch name {
			case "deploy":
				return account{home: home}, nil
			case "root":
				return account{home: "/root"}, nil
			}
			return account{}, errors.New("unknown user " + name)
		},
		root:   fakeRoot(t, sc),
		stdin:  strings.NewReader(in),
		stdout: &out,
		stderr: &errb,
	}
	return d, &out, &errb
}

// fakeRoot stands in for sudo: it performs install and rm on the temporary
// unit directory itself and hands systemctl to sc, which records the call
// and may fail it. Anything else is a bug in the code under test.
func fakeRoot(t *testing.T, sc *fakeSystemctl) func(context.Context, string, ...string) error {
	return func(ctx context.Context, name string, args ...string) error {
		switch {
		case name == "install" && len(args) == 4 && args[0] == "-m" && args[1] == "644":
			b, err := os.ReadFile(args[2])
			if err != nil {
				return err
			}
			return os.WriteFile(args[3], b, 0o644)
		case name == "rm" && len(args) == 2 && args[0] == "-f":
			if err := os.Remove(args[1]); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		case name == "systemctl":
			_, err := sc.run(ctx, args...)
			return err
		}
		t.Fatalf("unexpected root command: %s %v", name, args)
		return nil
	}
}

var testHomes sync.Map // *testing.T → its home directory

// testHome is the home directory of the test: a temp dir, also $HOME and the
// home of the sudo invoker in testDeps.
func testHome(t *testing.T) string {
	t.Helper()
	if h, ok := testHomes.Load(t); ok {
		return h.(string)
	}
	h := t.TempDir()
	t.Setenv("HOME", h)
	testHomes.Store(t, h)
	t.Cleanup(func() { testHomes.Delete(t) })
	return h
}

// writeApp saves cfg as the app.yaml of its app in the test's home and
// returns the app.yaml.
func writeApp(t *testing.T, cfg *config.App) string {
	t.Helper()
	p := config.AppPath(testHome(t), cfg.App)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(p, cfg); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeAppYAML writes a valid app in the test's home and returns its app.yaml.
func writeAppYAML(t *testing.T, app string) string {
	t.Helper()
	return writeApp(t, testApp(testHome(t), app))
}

// setSchedule rewrites schedule: of app.yaml.
func setSchedule(t *testing.T, appPath string, sc config.Schedule) {
	t.Helper()
	cfg, err := config.Load(appPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Schedule = &sc
	if err := config.Save(appPath, cfg); err != nil {
		t.Fatal(err)
	}
}

// ADR-0029: one OnCalendar line per (days, time); `last` is *-*~01.
func TestOnCalendar(t *testing.T) {
	for _, tc := range []struct {
		sc   config.Schedule
		want string
	}{
		{config.Schedule{Every: "daily", At: []string{"03:00"}}, "*-*-* 03:00:00"},
		{config.Schedule{Every: "daily", At: []string{"03:00", "15:30"}}, "*-*-* 03:00:00|*-*-* 15:30:00"},
		{config.Schedule{Every: "weekly", At: []string{"04:00"}, Days: []string{"mon", "thu"}}, "Mon,Thu *-*-* 04:00:00"},
		{config.Schedule{Every: "weekly", At: []string{"04:00", "22:00"}, Days: []string{"sun"}}, "Sun *-*-* 04:00:00|Sun *-*-* 22:00:00"},
		{config.Schedule{Every: "monthly", At: []string{"03:00"}, Days: []string{"1", "15"}}, "*-*-01,15 03:00:00"},
		{config.Schedule{Every: "monthly", At: []string{"03:00"}, Days: []string{"last"}}, "*-*~01 03:00:00"},
		{config.Schedule{Every: "monthly", At: []string{"03:00", "12:00"}, Days: []string{"28", "last"}}, "*-*-28 03:00:00|*-*-28 12:00:00|*-*~01 03:00:00|*-*~01 12:00:00"},
	} {
		if got := strings.Join(onCalendar(tc.sc), "|"); got != tc.want {
			t.Errorf("onCalendar(%+v) = %q, want %q", tc.sc, got, tc.want)
		}
	}
}

// One unit pair per app: the timer runs `bracer backup <app>`, the drill
// inside it (ADR-0026).
func TestRenderUnits(t *testing.T) {
	cfg := testApp("/home/deploy", "shop")
	files := renderUnits(inputs(cfg))
	if len(files) != 2 || files[0].name != "bracer-shop.service" || files[1].name != "bracer-shop.timer" {
		t.Fatalf("names: %+v", files)
	}
	svc := files[0].content
	for _, want := range []string{
		"Description=Bracer backup for shop\n",
		"Type=oneshot\n", "User=deploy\n", "WorkingDirectory=/home/deploy/.config/bracer/shop\n", "Environment=HOME=/home/deploy\n",
		"Environment=PATH=/usr/bin:/bin\n", "SyslogIdentifier=bracer\n",
		"ExecStart=/usr/local/bin/bracer backup shop\n", "`bracer remove` deletes it",
	} {
		if !strings.Contains(svc, want) {
			t.Errorf("service missing %q:\n%s", want, svc)
		}
	}
	if strings.Contains(svc, "bracer drill") {
		t.Errorf("no drill command: drill is a step of backup:\n%s", svc)
	}
	timer := files[1].content
	for _, want := range []string{"[Timer]\nOnCalendar=*-*-* 03:00:00\nPersistent=true\n", "RandomizedDelaySec=5m\n", "WantedBy=timers.target\n", "Description=Run bracer-shop.service\n"} {
		if !strings.Contains(timer, want) {
			t.Errorf("timer missing %q:\n%s", want, timer)
		}
	}
	// Several times a day: one OnCalendar line each (ADR-0029).
	cfg.Schedule.At = []string{"03:00", "15:00"}
	if timer := renderUnits(inputs(cfg))[1].content; !strings.Contains(timer, "[Timer]\nOnCalendar=*-*-* 03:00:00\nOnCalendar=*-*-* 15:00:00\nPersistent=true\n") {
		t.Errorf("two times:\n%s", timer)
	}
	all := svc + timer
	if strings.Contains(all, "EnvironmentFile") || strings.Contains(all, "BRACER_SHOP") {
		t.Error("units must not reference secrets")
	}
}

// loadAppYAML reads the app.yaml written by writeApp.
func loadAppYAML(t *testing.T, cfgPath string) *config.App {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// spec schedule § Cài unit (ADR-0027): the pair is written through root,
// mode 644, and the timer enabled; User= and PATH come from runtime: and
// the job calls the app by name. No question, no flag.
func TestInstallUnitsWritesUnitsAndEnables(t *testing.T) {
	sc := &fakeSystemctl{show: map[string]map[string]string{
		"bracer-shop.timer":   {"UnitFileState": "enabled", "NextElapseUSecRealtime": "Wed 2026-10-21 03:02:41 +07"},
		"bracer-shop.service": {"ActiveState": "failed"},
	}}
	d, out, errb := testDeps(t, sc, "")
	cfgPath := writeAppYAML(t, "shop")
	setSchedule(t, cfgPath, config.Schedule{Every: "weekly", At: []string{"04:00"}, Days: []string{"mon"}, Keep: 4})
	if err := installUnits(context.Background(), loadAppYAML(t, cfgPath), cfgPath, d); err != nil {
		t.Fatalf("installUnits: %v\nstderr=%s stdout=%s", err, errb, out)
	}
	for _, name := range []string{"bracer-shop.service", "bracer-shop.timer"} {
		b, err := os.ReadFile(filepath.Join(d.unitDir, name))
		if err != nil {
			t.Fatalf("%s not written: %v", name, err)
		}
		if st, _ := os.Stat(filepath.Join(d.unitDir, name)); st.Mode().Perm() != 0o644 {
			t.Errorf("%s mode %v", name, st.Mode().Perm())
		}
		if name == "bracer-shop.service" {
			for _, want := range []string{"WorkingDirectory=" + filepath.Dir(cfgPath) + "\n", "User=deploy\n", "Environment=HOME=" + testHome(t) + "\n", "Environment=PATH=/usr/bin:/bin\n", "ExecStart=/usr/local/bin/bracer backup shop\n"} {
				if !strings.Contains(string(b), want) {
					t.Errorf("service missing %q:\n%s", want, b)
				}
			}
		}
		if name == "bracer-shop.timer" && !strings.Contains(string(b), "OnCalendar=Mon *-*-* 04:00:00") {
			t.Errorf("timer:\n%s", b)
		}
	}
	entries, _ := os.ReadDir(d.unitDir)
	if len(entries) != 2 {
		t.Errorf("only the pair may be written: %v", entries)
	}
	// Writes done, systemd told, the slate clean (the service is failed, so
	// its last result is cleared), the timer on, then the timer shown.
	if calls := strings.Join(sc.calls, "\n"); calls != "daemon-reload\nshow -p ActiveState bracer-shop.service\nreset-failed bracer-shop.service\nenable --now bracer-shop.timer\nshow -p UnitFileState,NextElapseUSecRealtime bracer-shop.timer" {
		t.Errorf("systemctl calls:\n%s", calls)
	}
	if s := out.String(); strings.Contains(s, "[y/N]") || !strings.Contains(s, "Installed 2 units in "+d.unitDir+".\n") || !strings.Contains(s, "  bracer-shop.timer enabled, next run Wed 2026-10-21 03:02:41 +07\n") || strings.Contains(s, "last run") || strings.Contains(s, "drill is off") {
		t.Errorf("stdout:\n%s", s)
	}
	if errb.Len() != 0 {
		t.Errorf("stderr:\n%s", errb)
	}
}

// The backup and drill pairs of the earlier layout are retired (disabled,
// removed, reset-failed); other apps' units stay. drill.runtime: none on
// every instance is said on install (ADR-0026).
func TestInstallUnitsRetiresOldLayout(t *testing.T) {
	sc := &fakeSystemctl{show: map[string]map[string]string{
		"bracer-shop-backup.service": {"ActiveState": "failed"},
		"bracer-shop-drill.service":  {"ActiveState": "failed"},
	}}
	d, out, errb := testDeps(t, sc, "")
	cfg := testApp(testHome(t), "shop")
	cfg.Instances[0].SetDrillOff(true)
	cfgPath := writeApp(t, cfg)
	old := []string{"bracer-shop-backup.service", "bracer-shop-backup.timer", "bracer-shop-drill.service", "bracer-shop-drill.timer"}
	for _, n := range append(old, "bracer-shopper-backup.timer", "bracer-blog.timer") {
		os.WriteFile(filepath.Join(d.unitDir, n), []byte("old"), 0o644)
	}
	if err := installUnits(context.Background(), cfg, cfgPath, d); err != nil {
		t.Fatalf("installUnits: %v\n%s", err, errb)
	}
	if !strings.Contains(out.String(), "Installed 2 units") || !strings.Contains(out.String(), "drill is off for every instance of shop: backups are not test-restored.") {
		t.Errorf("stdout:\n%s", out)
	}
	for _, n := range old {
		if _, err := os.Stat(filepath.Join(d.unitDir, n)); err == nil {
			t.Errorf("%s must be removed", n)
		}
	}
	for _, n := range []string{"bracer-shopper-backup.timer", "bracer-blog.timer", "bracer-shop.service", "bracer-shop.timer"} {
		if _, err := os.Stat(filepath.Join(d.unitDir, n)); err != nil {
			t.Errorf("%s must exist: %v", n, err)
		}
	}
	calls := strings.Join(sc.calls, "\n")
	for _, want := range []string{"disable --now bracer-shop-backup.timer", "disable --now bracer-shop-drill.timer", "daemon-reload\nshow -p ActiveState bracer-shop-backup.service\nshow -p ActiveState bracer-shop-drill.service\nreset-failed bracer-shop-backup.service bracer-shop-drill.service\nenable --now bracer-shop.timer\n"} {
		if !strings.Contains(calls+"\n", want) {
			t.Errorf("calls missing %q:\n%s", want, calls)
		}
	}
	if strings.Contains(calls, "shopper") || strings.Contains(calls, "blog") {
		t.Errorf("another app's unit was touched:\n%s", calls)
	}
}

// Every problem is reported before anything is written, with the command
// that fixes it (ADR-0021: the job finds its app by name in its own home).
func TestInstallUnitsRefusals(t *testing.T) {
	cfgPath := writeAppYAML(t, "shop")
	run := func(mod func(*scheduleDeps)) string {
		d, _, _ := testDeps(t, &fakeSystemctl{}, "")
		mod(&d)
		err := installUnits(context.Background(), loadAppYAML(t, cfgPath), cfgPath, d)
		if err == nil {
			t.Fatal("installUnits must fail")
		}
		if entries, _ := os.ReadDir(d.unitDir); len(entries) != 0 {
			t.Errorf("a refused install wrote %v", entries)
		}
		return err.Error()
	}
	none := func(*scheduleDeps) {}
	if err := run(func(d *scheduleDeps) {
		d.lookupUser = func(string) (account, error) { return account{}, errors.New("unknown user deploy") }
	}); !strings.Contains(err, `runtime.user "deploy": unknown user deploy`) || !strings.Contains(err, "Fix: bracer remove shop, then bracer init shop") {
		t.Errorf("unknown user: %s", err)
	}
	cfg := loadAppYAML(t, cfgPath)
	cfg.Runtime.User = "root"
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	if err := run(none); !strings.Contains(err, "the jobs would run as root, whose home has no app shop\nFix: bracer init shop") {
		t.Errorf("app in another home: %s", err)
	}
	cfg.Runtime.User = "deploy"
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	// systemctl enable failing is an error of the install.
	d, _, _ := testDeps(t, &fakeSystemctl{fail: "enable"}, "")
	if err := installUnits(context.Background(), cfg, cfgPath, d); err == nil || !strings.Contains(err.Error(), "Failed to enable unit") {
		t.Errorf("enable fails: %v", err)
	}
	// So is sudo refusing the first write: nothing else is attempted.
	sc := &fakeSystemctl{}
	d, _, _ = testDeps(t, sc, "")
	d.root = func(context.Context, string, ...string) error { return errors.New("sudo: a password is required") }
	if err := installUnits(context.Background(), cfg, cfgPath, d); err == nil || !strings.Contains(err.Error(), "install bracer-shop.service: sudo: a password is required") || len(sc.calls) != 0 {
		t.Errorf("root fails: %v %v", err, sc.calls)
	}
}

// spec schedule § Gỡ unit: only the app's own unit files go, timers before
// services, and app.yaml is not read, so a broken one can still be removed
// (ADR-0023).
func TestUninstallUnits(t *testing.T) {
	sc := &fakeSystemctl{show: map[string]map[string]string{
		"bracer-shop-backup.service": {"ActiveState": "failed"},
		"bracer-shop-drill.service":  {"ActiveState": "failed"},
		"bracer-shop.service":        {"ActiveState": "failed"},
	}}
	d, out, errb := testDeps(t, sc, "")
	cfgPath := writeAppYAML(t, "shop")
	if err := uninstallUnits(context.Background(), "shop", d); err != nil || !strings.Contains(out.String(), "No schedule installed for app shop.") || len(sc.calls) != 0 {
		t.Errorf("nothing to remove: %v %s %v", err, out, sc.calls)
	}
	// The pair, the pairs of the earlier layout, and files that are no unit of shop.
	for _, n := range []string{"bracer-shop.service", "bracer-shop.timer", "bracer-shop-backup.service", "bracer-shop-backup.timer", "bracer-shop-drill.service", "bracer-shop-drill.timer", "bracer-shop.timer.bak", "bracer-blog.timer", "bracer-shopper.timer", "bracer-shop-x.conf"} {
		os.WriteFile(filepath.Join(d.unitDir, n), []byte("x"), 0o644)
	}
	os.WriteFile(cfgPath, []byte("not: [yaml"), 0o644)
	out.Reset()
	if err := uninstallUnits(context.Background(), "shop", d); err != nil {
		t.Fatalf("uninstallUnits: %v\n%s", err, errb)
	}
	entries, _ := os.ReadDir(d.unitDir)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if strings.Join(left, " ") != "bracer-blog.timer bracer-shop-x.conf bracer-shop.timer.bak bracer-shopper.timer" {
		t.Errorf("left: %v", left)
	}
	for _, n := range []string{"bracer-shop.timer", "bracer-shop-drill.timer", "bracer-shop-backup.service"} {
		if !strings.Contains(out.String(), "Removed "+filepath.Join(d.unitDir, n)+"\n") {
			t.Errorf("stdout missing Removed %s:\n%s", n, out)
		}
	}
	// Timers off first, then the services stopped, then (files gone) reload
	// and a clean slate for the services systemd remembers as failed.
	want := "disable --now bracer-shop-backup.timer\ndisable --now bracer-shop-drill.timer\ndisable --now bracer-shop.timer\n" +
		"stop bracer-shop-backup.service\nstop bracer-shop-drill.service\nstop bracer-shop.service\n" +
		"daemon-reload\nshow -p ActiveState bracer-shop-backup.service\nshow -p ActiveState bracer-shop-drill.service\nshow -p ActiveState bracer-shop.service\n" +
		"reset-failed bracer-shop-backup.service bracer-shop-drill.service bracer-shop.service"
	if calls := strings.Join(sc.calls, "\n"); calls != want {
		t.Errorf("calls:\n%s\nwant:\n%s", calls, want)
	}
	if errb.Len() != 0 {
		t.Errorf("stderr:\n%s", errb)
	}
	// A timer that cannot be disabled stops the uninstall before any file goes.
	os.WriteFile(filepath.Join(d.unitDir, "bracer-shop.timer"), []byte("x"), 0o644)
	sc.fail = "disable"
	if err := uninstallUnits(context.Background(), "shop", d); err == nil || !strings.Contains(err.Error(), "disable --now bracer-shop.timer") {
		t.Errorf("disable fails: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.unitDir, "bracer-shop.timer")); err != nil {
		t.Errorf("the timer file must stay: %v", err)
	}
}

// systemd keeps a service's last result until reset-failed, even after its
// unit files are gone: uninstall and a first install clear it, a reinstall
// keeps the record of a real failure. Only a failed service is reset: on
// any other one reset-failed prints systemd's "not loaded" through sudo, so
// it is not called at all. An error from reset-failed only warns.
func TestInstallUnitsResetFailed(t *testing.T) {
	sc := &fakeSystemctl{show: map[string]map[string]string{}}
	d, out, errb := testDeps(t, sc, "")
	cfgPath := writeAppYAML(t, "shop")
	cfg := loadAppYAML(t, cfgPath)
	// A first install on a machine that never ran the service: asked, nothing to reset.
	if err := installUnits(context.Background(), cfg, cfgPath, d); err != nil || !strings.Contains(out.String(), "Installed 2 units") {
		t.Fatalf("install: %v %s\n%s", err, errb, out)
	}
	if calls := strings.Join(sc.calls, "\n"); !strings.Contains(calls, "daemon-reload\nshow -p ActiveState bracer-shop.service\nenable --now") || strings.Contains(calls, "reset-failed") {
		t.Errorf("a service that never ran is not reset:\n%s", calls)
	}
	if errb.Len() != 0 {
		t.Errorf("stderr:\n%s", errb)
	}
	// Same when the last run succeeded: there is no failure to clear.
	sc.show["bracer-shop.service"] = map[string]string{"ActiveState": "inactive"}
	sc.calls = nil
	if err := uninstallUnits(context.Background(), "shop", d); err != nil || !strings.HasSuffix(strings.Join(sc.calls, "\n"), "daemon-reload\nshow -p ActiveState bracer-shop.service") || errb.Len() != 0 {
		t.Errorf("uninstall of an inactive service: %v stderr=%s\n%s", err, errb, strings.Join(sc.calls, "\n"))
	}
	// Failed: a first install clears the last result before enabling.
	sc.show["bracer-shop.service"] = map[string]string{"ActiveState": "failed"}
	sc.calls = nil
	if err := installUnits(context.Background(), cfg, cfgPath, d); err != nil {
		t.Fatal(err)
	}
	if calls := strings.Join(sc.calls, "\n"); !strings.Contains(calls, "daemon-reload\nshow -p ActiveState bracer-shop.service\nreset-failed bracer-shop.service\nenable --now") {
		t.Errorf("first install must reset-failed before enable:\n%s", calls)
	}
	sc.calls = nil
	if err := installUnits(context.Background(), cfg, cfgPath, d); err != nil {
		t.Fatal(err)
	}
	if calls := strings.Join(sc.calls, "\n"); strings.Contains(calls, "reset-failed") || strings.Contains(calls, "ActiveState") {
		t.Errorf("a reinstall keeps the record of a real failure:\n%s", calls)
	}
	sc.calls = nil
	if err := uninstallUnits(context.Background(), "shop", d); err != nil || !strings.HasSuffix(strings.Join(sc.calls, "\n"), "daemon-reload\nshow -p ActiveState bracer-shop.service\nreset-failed bracer-shop.service") {
		t.Errorf("uninstall: %v\n%s", err, strings.Join(sc.calls, "\n"))
	}
	if errb.Len() != 0 {
		t.Errorf("stderr:\n%s", errb)
	}
	// reset-failed failing on a failed service only warns; the install goes on.
	root := d.root
	d.root = func(ctx context.Context, name string, args ...string) error {
		if name == "systemctl" && args[0] == "reset-failed" {
			return errors.New("systemctl reset-failed bracer-shop.service: exit status 1: Access denied")
		}
		return root(ctx, name, args...)
	}
	if err := installUnits(context.Background(), cfg, cfgPath, d); err != nil || !strings.Contains(errb.String(), "warning: systemctl reset-failed bracer-shop.service: exit status 1: Access denied\n") {
		t.Errorf("reset-failed denied: %v stderr=%s", err, errb)
	}
}

// ADR-0027: root runs the command itself; anyone else goes through sudo on
// the terminal, which asks its own password there.
func TestRootRunner(t *testing.T) {
	var got []string
	run := func(_ context.Context, name string, args ...string) error {
		got = append([]string{name}, args...)
		return nil
	}
	rootRunner(run, 0)(context.Background(), "systemctl", "enable", "--now", "bracer-shop.timer")
	if strings.Join(got, " ") != "systemctl enable --now bracer-shop.timer" {
		t.Errorf("root: %q", got)
	}
	rootRunner(run, 1000)(context.Background(), "install", "-m", "644", "/tmp/x", "/etc/systemd/system/x")
	if strings.Join(got, " ") != "sudo install -m 644 /tmp/x /etc/systemd/system/x" {
		t.Errorf("user: %q", got)
	}
	fail := func(context.Context, string, ...string) error { return errors.New("sudo: a password is required") }
	if err := rootRunner(fail, 1000)(context.Background(), "rm", "-f", "x"); err == nil || !strings.Contains(err.Error(), "a password is required") {
		t.Errorf("sudo error must pass through: %v", err)
	}
}

// ADR-0021: commands name the app, even when the machine has only one.
func TestResolveApp(t *testing.T) {
	home := t.TempDir()
	if _, err := resolveApp(home, "", "backup"); err == nil || errors.Is(err, errNoApp) || !strings.Contains(err.Error(), "name the app: bracer backup <app> (no app on this machine yet)\nFix: bracer init <name>") {
		t.Errorf("no app: %v", err)
	}
	for _, a := range []string{"shop", "blog"} {
		os.MkdirAll(config.AppDir(home, a), 0o700)
		os.WriteFile(config.AppPath(home, a), []byte("app: "+a+"\n"), 0o600)
	}
	os.MkdirAll(filepath.Join(config.AppsDir(home), "empty"), 0o700)
	os.WriteFile(filepath.Join(config.AppsDir(home), "old.env"), nil, 0o600)
	if got := config.KnownApps(home); strings.Join(got, ",") != "blog,shop" {
		t.Errorf("KnownApps = %v", got)
	}
	if p, err := resolveApp(home, "shop", "backup"); err != nil || p != config.AppPath(home, "shop") {
		t.Errorf("named: %s %v", p, err)
	}
	if _, err := resolveApp(home, "", "backup"); err == nil || !strings.Contains(err.Error(), "name the app: bracer backup <app> (apps here: blog, shop)") {
		t.Errorf("several: %v", err)
	}
	if _, err := resolveApp(home, "nope", "backup"); !errors.Is(err, errNoApp) || !strings.Contains(err.Error(), "no app nope on this machine (apps here: blog, shop)\nFix: bracer init nope") {
		t.Errorf("missing: %v", err)
	}
	if _, err := resolveApp(home, "Bad Name", "backup"); err == nil || errors.Is(err, errNoApp) {
		t.Errorf("bad name: %v", err)
	}
	os.RemoveAll(config.AppDir(home, "blog"))
	if _, err := resolveApp(home, "", "backup"); err == nil || !strings.Contains(err.Error(), "(apps here: shop)") {
		t.Errorf("only one must still be named: %v", err)
	}
	// Flags before and after the name.
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "")
	if name, err := appArg(fs, []string{"shop", "--dry-run"}); err != nil || name != "shop" || !*dry {
		t.Errorf("appArg: %q %v %v", name, err, *dry)
	}
	if _, err := appArg(flag.NewFlagSet("y", flag.ContinueOnError), []string{"a", "b"}); err == nil {
		t.Error("two names must fail")
	}
}

// init's self-check on a new app reads the draft: there is no app.yaml yet.
func TestSelectDraftOfNewApp(t *testing.T) {
	home := testHome(t)
	p := config.AppPath(home, "shop")
	draft := true
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	if _, code, err := selectAppOrDraft(fs, []string{"shop"}, home, "backup", &draft); code != exitUsage || err == nil || !strings.Contains(err.Error(), "no draft of app shop") {
		t.Errorf("no draft: %d %v", code, err)
	}
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(draftPath(p), []byte("app: shop\n"), 0o600)
	if got, code, err := selectAppOrDraft(flag.NewFlagSet("backup", flag.ContinueOnError), []string{"shop"}, home, "backup", &draft); code != exitOK || err != nil || got != p {
		t.Errorf("draft: %s %d %v", got, code, err)
	}
	draft = false
	if _, code, err := selectAppOrDraft(flag.NewFlagSet("backup", flag.ContinueOnError), []string{"shop"}, home, "backup", &draft); code != exitUsage || !errors.Is(err, errNoApp) {
		t.Errorf("without --init-draft the app.yaml is needed: %d %v", code, err)
	}
}
