package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bracer.dev/bracer/internal/config"
)

func scheduleCfg() *config.App {
	return &config.App{
		App:             "shop",
		SecretsFile:     "shop.env",
		Instances:       []config.Instance{{Name: "main", Container: "shop-db", User: "postgres", PasswordEnv: "BRACER_SHOP_MAIN_PASSWORD", Databases: config.Databases{All: true}}},
		Storage:         &config.Storage{Endpoint: "https://s3", Bucket: "b", AccessKeyEnv: "BRACER_SHOP_S3_ACCESS_KEY", SecretKeyEnv: "BRACER_SHOP_S3_SECRET_KEY"},
		Hosted:          &config.Hosted{Server: "https://app.bracer.dev", TokenEnv: "BRACER_SHOP_TOKEN"},
		AgeIdentityFile: "~/.config/bracer/shop.key",
	}
}

func inputs(cfg *config.App) scheduleInputs {
	return scheduleInputs{cfg: cfg, cfgPath: "/srv/shop/app.yaml", binary: "/usr/local/bin/bracer", path: "/usr/bin:/usr/local/bin:/bin", drill: true, username: "deploy", home: "/home/deploy",
		backupAt: when{cron: "0 3 * * *", onCalendar: "*-*-* 03:00:00"}, drillAt: when{cron: "0 4 * * 1", onCalendar: "Mon *-*-* 04:00:00"}, secretsAbs: "/srv/shop/shop.env"}
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

// fakeCheck records the dry runs install makes and fails those listed.
type fakeCheck struct {
	runs []string // "uid args… | env…"
	fail map[string]string
}

func (f *fakeCheck) run(_ context.Context, a account, dir string, env, args []string, stdout, _ io.Writer) int {
	f.runs = append(f.runs, fmt.Sprintf("%d %v %s | %s | %s", a.uid, a.groups, strings.Join(args, " "), strings.Join(env, " "), dir))
	if msg, ok := f.fail[args[1]]; ok {
		io.WriteString(stdout, msg)
		return 1
	}
	return 0
}

// testDeps: a temp unit directory, a fake systemctl, root, docker on PATH, stdin from `in`.
func testDeps(t *testing.T, sc *fakeSystemctl, in string) (scheduleDeps, *bytes.Buffer, *bytes.Buffer) {
	d, out, errb, _ := testDepsCheck(t, sc, in)
	return d, out, errb
}

func testDepsCheck(t *testing.T, sc *fakeSystemctl, in string) (scheduleDeps, *bytes.Buffer, *bytes.Buffer, *fakeCheck) {
	t.Helper()
	var out, errb bytes.Buffer
	fc := &fakeCheck{fail: map[string]string{}}
	d := scheduleDeps{
		unitDir:    t.TempDir(),
		systemctl:  sc.run,
		hasSystemd: func() bool { return true },
		euid:       0,
		lookPath: func(tool string) (string, error) {
			if tool == "docker" || tool == "bracer" {
				return "/usr/bin/" + tool, nil
			}
			return "", errors.New("not found")
		},
		executable: func() string { return "/usr/local/bin/bracer" },
		invoker:    func() string { return "deploy" },
		lookupUser: func(name string) (account, error) {
			switch name {
			case "deploy":
				return account{home: "/home/deploy", uid: 1000, gid: 1000, groups: []uint32{1000, 998}}, nil
			case "khanh":
				return account{home: "/home/khanh", uid: 1001, gid: 1001}, nil
			case "root":
				return account{home: "/home/root"}, nil
			}
			return account{}, errors.New("unknown")
		},
		check:  fc.run,
		stdin:  strings.NewReader(in),
		stdout: &out,
		stderr: &errb,
	}
	return d, &out, &errb, fc
}

// writeAppYAML writes a valid app.yaml in a temp dir and returns its path.
func writeAppYAML(t *testing.T, app string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "app.yaml")
	y := fmt.Sprintf(`app: %s
secrets_file: %s.env
instances:
  - name: main
    container: shop-db
    user: postgres
    pg_major: 17
    databases: all
storage:
  endpoint: https://s3.example
  bucket: b
  access_key_env: BRACER_SHOP_S3_ACCESS_KEY
  secret_key_env: BRACER_SHOP_S3_SECRET_KEY
encryption: none
`, app, app)
	if err := os.WriteFile(p, []byte(y), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseWhen(t *testing.T) {
	for _, tc := range []struct{ in, cron, cal string }{
		{"03:00", "0 3 * * *", "*-*-* 03:00:00"},
		{"4:05", "5 4 * * *", "*-*-* 04:05:00"},
		{"23:59", "59 23 * * *", "*-*-* 23:59:00"},
		{"0 3 * * *", "0 3 * * *", "*-*-* 03:00:00"},
		{"30 2 * * 1,5", "30 2 * * 1,5", "Mon,Fri *-*-* 02:30:00"},
		{"0 4 * * mon", "0 4 * * mon", "Mon *-*-* 04:00:00"},
		{"*/5 * * * *", "*/5 * * * *", ""},
		{"0 3 1 * *", "0 3 1 * *", ""},
		{"0 3 * * 1-5", "0 3 * * 1-5", ""},
	} {
		w, err := parseWhen(tc.in)
		if err != nil || w.cron != tc.cron || w.onCalendar != tc.cal {
			t.Errorf("parseWhen(%q) = %+v, %v; want cron %q cal %q", tc.in, w, err, tc.cron, tc.cal)
		}
	}
	for _, bad := range []string{"24:00", "3", "0 3 * *", "tomorrow"} {
		if _, err := parseWhen(bad); err == nil {
			t.Errorf("parseWhen(%q) must fail", bad)
		}
	}
}

func TestRenderCron(t *testing.T) {
	var out bytes.Buffer
	renderCron(&out, inputs(scheduleCfg()))
	s := out.String()
	for _, want := range []string{
		"PATH=/usr/bin:",
		"0 3 * * * /usr/local/bin/bracer backup -c /srv/shop/app.yaml 2>&1 | logger -t bracer",
		"0 4 * * 1 /usr/local/bin/bracer drill  -c /srv/shop/app.yaml",
		"secrets_file (/srv/shop/shop.env)",
		"Results are reported to https://app.bracer.dev",
		"bracer ping <heartbeat-id> -- ",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "--env-file") || strings.Contains(s, "BRACER_SHOP_MAIN_PASSWORD") {
		t.Error("with secrets_file, no env names or --env-file may appear")
	}

	// No secrets_file: list the variable names, without values.
	cfg := scheduleCfg()
	cfg.SecretsFile, cfg.Hosted = "", nil
	in := inputs(cfg)
	in.secretsAbs, in.drill, in.path, in.binary = "", false, "/usr/local/bin:/usr/bin:/bin", "bracer"
	out.Reset()
	renderCron(&out, in)
	s = out.String()
	for _, want := range []string{"BRACER_SHOP_MAIN_PASSWORD, BRACER_SHOP_S3_ACCESS_KEY, BRACER_SHOP_S3_SECRET_KEY", "docker was not found on PATH", "PATH=/usr/local/bin:/usr/bin:/bin", "temporary path", "not reported anywhere"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}

func TestRenderUnits(t *testing.T) {
	files := renderUnits(inputs(scheduleCfg()), scheduleJobs)
	if len(files) != 4 || files[0].name != "bracer-shop-backup.service" || files[1].name != "bracer-shop-backup.timer" || files[2].name != "bracer-shop-drill.service" || files[3].name != "bracer-shop-drill.timer" {
		t.Fatalf("names: %+v", files)
	}
	svc := files[0].content
	for _, want := range []string{
		"Type=oneshot\n", "User=deploy\n", "WorkingDirectory=/srv/shop\n", "Environment=HOME=/home/deploy\n",
		"Environment=PATH=/usr/bin:/usr/local/bin:/bin\n", "SyslogIdentifier=bracer\n",
		"ExecStart=/usr/local/bin/bracer backup -c /srv/shop/app.yaml\n",
	} {
		if !strings.Contains(svc, want) {
			t.Errorf("service missing %q:\n%s", want, svc)
		}
	}
	timer := files[3].content
	for _, want := range []string{"OnCalendar=Mon *-*-* 04:00:00\n", "Persistent=true\n", "RandomizedDelaySec=5m\n", "WantedBy=timers.target\n", "Description=Run bracer-shop-drill.service\n"} {
		if !strings.Contains(timer, want) {
			t.Errorf("timer missing %q:\n%s", want, timer)
		}
	}
	all := svc + files[2].content
	if strings.Contains(all, "EnvironmentFile") || strings.Contains(all, "BRACER_SHOP") {
		t.Error("units must not reference secrets")
	}

	var out bytes.Buffer
	renderSystemd(&out, inputs(scheduleCfg()))
	if s := out.String(); !strings.Contains(s, "enable --now bracer-shop-backup.timer bracer-shop-drill.timer") || !strings.Contains(s, "# ---- bracer-shop-backup.service ----") {
		t.Errorf("renderSystemd:\n%s", s)
	}
}

func TestScheduleInstallWritesUnitsAndEnables(t *testing.T) {
	sc := &fakeSystemctl{show: map[string]map[string]string{}}
	d, out, errb := testDeps(t, sc, "y\n")
	cfgPath := writeAppYAML(t, "shop")
	code := runSchedule(context.Background(), []string{"install", "-c", cfgPath, "--drill-at", "0 4 * * 1"}, d)
	if code != exitOK {
		t.Fatalf("code=%d stderr=%s stdout=%s", code, errb, out)
	}
	for _, name := range []string{"bracer-shop-backup.service", "bracer-shop-backup.timer", "bracer-shop-drill.service", "bracer-shop-drill.timer"} {
		b, err := os.ReadFile(filepath.Join(d.unitDir, name))
		if err != nil {
			t.Fatalf("%s not written: %v", name, err)
		}
		if name == "bracer-shop-backup.service" && !strings.Contains(string(b), "WorkingDirectory="+filepath.Dir(cfgPath)+"\n") {
			t.Errorf("WorkingDirectory wrong:\n%s", b)
		}
		if name == "bracer-shop-drill.timer" && !strings.Contains(string(b), "OnCalendar=Mon *-*-* 04:00:00") {
			t.Errorf("drill timer:\n%s", b)
		}
	}
	calls := strings.Join(sc.calls, "\n")
	for _, want := range []string{"daemon-reload", "enable --now bracer-shop-backup.timer bracer-shop-drill.timer", "show -p"} {
		if !strings.Contains(calls, want) {
			t.Errorf("systemctl calls missing %q:\n%s", want, calls)
		}
	}
	if !strings.Contains(out.String(), "Install these units and start the timers? [y/N]") || !strings.Contains(out.String(), "Installed 4 units") || !strings.Contains(out.String(), "last run: never") {
		t.Errorf("stdout:\n%s", out)
	}
}

func TestScheduleInstallDeclinedAndNonInteractive(t *testing.T) {
	sc := &fakeSystemctl{}
	d, out, _ := testDeps(t, sc, "n\n")
	cfgPath := writeAppYAML(t, "shop")
	if code := runSchedule(context.Background(), []string{"install", "-c", cfgPath}, d); code != exitOK || !strings.Contains(out.String(), "Nothing installed") {
		t.Errorf("declined: code=%d out=%s", code, out)
	}
	if entries, _ := os.ReadDir(d.unitDir); len(entries) != 0 || len(sc.calls) != 0 {
		t.Errorf("declined install must write nothing and call nothing: %v %v", entries, sc.calls)
	}
	d, _, errb := testDeps(t, sc, "")
	if code := runSchedule(context.Background(), []string{"install", "-c", cfgPath}, d); code != exitUsage || !strings.Contains(errb.String(), "--yes") {
		t.Errorf("no stdin: code=%d err=%s", code, errb)
	}
}

func TestScheduleInstallWithoutDockerSkipsDrillAndRetiresStale(t *testing.T) {
	sc := &fakeSystemctl{show: map[string]map[string]string{}}
	d, out, errb := testDeps(t, sc, "")
	d.lookPath = func(tool string) (string, error) {
		if tool == "bracer" {
			return "/usr/local/bin/bracer", nil
		}
		return "", errors.New("not found")
	}
	// A drill pair from an earlier install, and a unit of another app that must survive.
	for _, n := range []string{"bracer-shop-drill.service", "bracer-shop-drill.timer", "bracer-shopper-backup.timer", "bracer-blog-backup.timer"} {
		os.WriteFile(filepath.Join(d.unitDir, n), []byte("old"), 0o644)
	}
	cfgPath := writeAppYAML(t, "shop")
	if code := runSchedule(context.Background(), []string{"install", "--yes", "-c", cfgPath}, d); code != exitOK {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if !strings.Contains(out.String(), "drill is not scheduled") || strings.Contains(out.String(), "[y/N]") {
		t.Errorf("stdout:\n%s", out)
	}
	for _, n := range []string{"bracer-shop-drill.service", "bracer-shop-drill.timer"} {
		if _, err := os.Stat(filepath.Join(d.unitDir, n)); err == nil {
			t.Errorf("%s must be removed", n)
		}
	}
	for _, n := range []string{"bracer-shopper-backup.timer", "bracer-blog-backup.timer", "bracer-shop-backup.service", "bracer-shop-backup.timer"} {
		if _, err := os.Stat(filepath.Join(d.unitDir, n)); err != nil {
			t.Errorf("%s must exist: %v", n, err)
		}
	}
	calls := strings.Join(sc.calls, "\n")
	if !strings.Contains(calls, "disable --now bracer-shop-drill.timer") || !strings.Contains(calls+"\n", "enable --now bracer-shop-backup.timer\n") {
		t.Errorf("calls:\n%s", calls)
	}
}

func TestScheduleInstallRefusals(t *testing.T) {
	cfgPath := writeAppYAML(t, "shop")
	run := func(mod func(*scheduleDeps), args ...string) (int, string) {
		d, _, errb := testDeps(t, &fakeSystemctl{}, "")
		mod(&d)
		code := runSchedule(context.Background(), append([]string{"install", "--yes", "-c", cfgPath}, args...), d)
		return code, errb.String()
	}
	if code, err := run(func(d *scheduleDeps) { d.hasSystemd = func() bool { return false } }); code != exitUsage || !strings.Contains(err, "schedule print --format cron") {
		t.Errorf("no systemd: %d %s", code, err)
	}
	if code, err := run(func(d *scheduleDeps) { d.euid = 1000 }); code != exitUsage || !strings.Contains(err, "sudo") {
		t.Errorf("not root: %d %s", code, err)
	}
	if code, err := run(func(*scheduleDeps) {}, "--backup-at", "*/5 * * * *"); code != exitUsage || !strings.Contains(err, "fixed minute and hour") {
		t.Errorf("complex cron: %d %s", code, err)
	}
	if code, err := run(func(*scheduleDeps) {}, "--user", "nobody-here"); code != exitUsage || !strings.Contains(err, "unknown user") {
		t.Errorf("unknown user: %d %s", code, err)
	}
	if code, err := run(func(d *scheduleDeps) {
		d.executable = func() string { return "bracer" }
		d.lookPath = func(string) (string, error) { return "", errors.New("no") }
	}); code != exitUsage || !strings.Contains(err, "--binary") {
		t.Errorf("temp binary: %d %s", code, err)
	}
	if code, err := run(func(*scheduleDeps) {}, "-c", "/nonexistent/app.yaml"); code != exitUsage {
		t.Errorf("missing app.yaml: %d %s", code, err)
	}
	// systemctl enable failing is a runtime error that names the unit directory.
	d, _, errb := testDeps(t, &fakeSystemctl{fail: "enable"}, "")
	if code := runSchedule(context.Background(), []string{"install", "--yes", "-c", cfgPath}, d); code != exitError || !strings.Contains(errb.String(), "Failed to enable unit") || !strings.Contains(errb.String(), "schedule remove") {
		t.Errorf("enable fails: %d %s", code, errb)
	}
}

func TestScheduleInstallBinaryAndUserFlags(t *testing.T) {
	sc := &fakeSystemctl{show: map[string]map[string]string{}}
	d, _, errb := testDeps(t, sc, "")
	d.executable = func() string { return "bracer" } // running from /tmp
	cfgPath := writeAppYAML(t, "shop")
	if code := runSchedule(context.Background(), []string{"install", "--yes", "-c", cfgPath, "--binary", "/opt/bracer/bracer", "--user", "root"}, d); code != exitOK {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	b, _ := os.ReadFile(filepath.Join(d.unitDir, "bracer-shop-backup.service"))
	if !strings.Contains(string(b), "ExecStart=/opt/bracer/bracer backup -c ") || !strings.Contains(string(b), "User=root\n") || !strings.Contains(string(b), "Environment=HOME=/home/root\n") {
		t.Errorf("service:\n%s", b)
	}
	// Without --binary, a temporary binary falls back to bracer on PATH.
	d, _, errb = testDeps(t, sc, "")
	d.executable = func() string { return "bracer" }
	if code := runSchedule(context.Background(), []string{"install", "--yes", "-c", cfgPath}, d); code != exitOK {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	b, _ = os.ReadFile(filepath.Join(d.unitDir, "bracer-shop-backup.service"))
	if !strings.Contains(string(b), "ExecStart=/usr/bin/bracer backup") {
		t.Errorf("fallback to PATH:\n%s", b)
	}
}

func TestScheduleRemove(t *testing.T) {
	sc := &fakeSystemctl{}
	d, out, errb := testDeps(t, sc, "")
	cfgPath := writeAppYAML(t, "shop")
	if code := runSchedule(context.Background(), []string{"remove", "-c", cfgPath}, d); code != exitOK || !strings.Contains(out.String(), "No schedule installed") {
		t.Errorf("nothing to remove: %d %s %s", code, out, errb)
	}
	for _, n := range []string{"bracer-shop-backup.service", "bracer-shop-backup.timer", "bracer-shop-drill.service", "bracer-shop-drill.timer", "bracer-shop-backup.timer.bak", "bracer-blog-backup.timer", "bracer-shop-x.conf"} {
		os.WriteFile(filepath.Join(d.unitDir, n), []byte("x"), 0o644)
	}
	out.Reset()
	if code := runSchedule(context.Background(), []string{"remove", "-c", cfgPath}, d); code != exitOK {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	entries, _ := os.ReadDir(d.unitDir)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if strings.Join(left, " ") != "bracer-blog-backup.timer bracer-shop-backup.timer.bak bracer-shop-x.conf" {
		t.Errorf("left: %v", left)
	}
	calls := strings.Join(sc.calls, "\n")
	for _, want := range []string{"disable --now bracer-shop-backup.timer", "disable --now bracer-shop-drill.timer", "stop bracer-shop-backup.service", "stop bracer-shop-drill.service", "daemon-reload"} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing %q in calls:\n%s", want, calls)
		}
	}
	if strings.Contains(calls, "blog") {
		t.Errorf("another app's unit was touched:\n%s", calls)
	}
}

func TestScheduleStatus(t *testing.T) {
	sc := &fakeSystemctl{show: map[string]map[string]string{
		"bracer-shop-backup.timer":   {"UnitFileState": "enabled", "ActiveState": "active", "NextElapseUSecRealtime": "Sun 2026-10-19 03:02:41 UTC", "LastTriggerUSec": "Sat 2026-10-18 03:01:10 UTC"},
		"bracer-shop-backup.service": {"ActiveState": "inactive", "Result": "success", "ExecMainStatus": "0", "ExecMainExitTimestamp": "Sat 2026-10-18 03:01:52 UTC"},
		"bracer-shop-drill.timer":    {"UnitFileState": "enabled", "ActiveState": "active", "NextElapseUSecRealtime": "Sun 2026-10-19 04:00:12 UTC"},
		"bracer-shop-drill.service":  {"ActiveState": "failed", "Result": "exit-code", "ExecMainStatus": "1", "ExecMainExitTimestamp": "Sat 2026-10-18 04:00:40 UTC"},
	}}
	d, out, errb := testDeps(t, sc, "")
	d.euid = 1000 // status needs no root
	cfgPath := writeAppYAML(t, "shop")
	if code := runSchedule(context.Background(), []string{"status", "-c", cfgPath}, d); code != exitError || !strings.Contains(out.String(), "No schedule installed") {
		t.Errorf("not installed: %d %s %s", code, out, errb)
	}
	for _, n := range []string{"bracer-shop-backup.service", "bracer-shop-backup.timer", "bracer-shop-drill.service", "bracer-shop-drill.timer"} {
		os.WriteFile(filepath.Join(d.unitDir, n), []byte("x"), 0o644)
	}
	out.Reset()
	code := runSchedule(context.Background(), []string{"status", "-c", cfgPath}, d)
	s := out.String()
	for _, want := range []string{
		"backup: bracer-shop-backup.timer enabled, next run Sun 2026-10-19 03:02:41 UTC",
		"last run Sat 2026-10-18 03:01:52 UTC: success",
		"drill: bracer-shop-drill.timer enabled, next run Sun 2026-10-19 04:00:12 UTC",
		"last run Sat 2026-10-18 04:00:40 UTC: failed (exit-code, exit status 1)",
		"journalctl -t bracer",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if code != exitError {
		t.Errorf("a failed last run must exit 1, got %d", code)
	}
	// Only backup installed, running right now, timer disabled by hand.
	sc.show["bracer-shop-backup.timer"] = map[string]string{"UnitFileState": "disabled", "ActiveState": "inactive", "NextElapseUSecRealtime": "n/a"}
	sc.show["bracer-shop-backup.service"] = map[string]string{"ActiveState": "activating", "ExecMainStartTimestamp": "Sat 2026-10-18 03:01:10 UTC"}
	os.Remove(filepath.Join(d.unitDir, "bracer-shop-drill.timer"))
	out.Reset()
	if code := runSchedule(context.Background(), []string{"status", "-c", cfgPath}, d); code != exitOK || !strings.Contains(out.String(), "disabled, not active, next run never") || !strings.Contains(out.String(), "running since Sat 2026-10-18 03:01:10 UTC") || strings.Contains(out.String(), "drill:") {
		t.Errorf("code=%d out:\n%s", code, out)
	}
}

func TestSchedulePrintAndDeprecatedAlias(t *testing.T) {
	cfgPath := writeAppYAML(t, "shop")
	d, out, errb := testDeps(t, &fakeSystemctl{}, "")
	d.euid, d.hasSystemd = 1000, func() bool { return false } // print needs neither
	if code := runSchedule(context.Background(), []string{"print", "-c", cfgPath}, d); code != exitOK || !strings.Contains(out.String(), "0 3 * * * /usr/local/bin/bracer backup -c "+cfgPath) {
		t.Errorf("cron: %d %s %s", code, out, errb)
	}
	out.Reset()
	if code := runSchedule(context.Background(), []string{"print", "-c", cfgPath, "--format", "systemd", "--backup-at", "02:30"}, d); code != exitOK || !strings.Contains(out.String(), "OnCalendar=*-*-* 02:30:00") || !strings.Contains(out.String(), "# ---- bracer-shop-drill.timer ----") {
		t.Errorf("systemd: %d %s %s", code, out, errb)
	}
	if code := runSchedule(context.Background(), []string{"print", "-c", cfgPath, "--format", "systemd", "--backup-at", "*/5 * * * *"}, d); code != exitUsage {
		t.Errorf("complex cron for systemd must fail: %d", code)
	}
	if code := runSchedule(context.Background(), []string{"print", "-c", cfgPath, "--format", "github"}, d); code != exitUsage || !strings.Contains(errb.String(), "cron or systemd") {
		t.Errorf("github must be rejected: %d %s", code, errb)
	}
	if len((&fakeSystemctl{}).calls) != 0 {
		t.Error("print must not call systemctl")
	}
	out.Reset()
	errb.Reset()
	if code := runCronExample(context.Background(), []string{"-c", cfgPath}, d); code != exitOK || !strings.Contains(errb.String(), "deprecated") || !strings.Contains(out.String(), "PATH=") {
		t.Errorf("alias: %d err=%s out=%s", code, errb, out)
	}
	if code := runSchedule(context.Background(), []string{"bogus"}, d); code != exitUsage {
		t.Errorf("unknown subcommand: %d", code)
	}
}

func TestPingFlagOrder(t *testing.T) {
	// Flags after and before the id both work; bad server → exit 2 without a network call.
	var out, errb bytes.Buffer
	for _, args := range [][]string{{"hb", "--server", "nope"}, {"--server", "nope", "hb"}} {
		errb.Reset()
		if code := runPing(nil, args, nil, &out, &errb); code != exitUsage || !strings.Contains(errb.String(), "http(s)") {
			t.Errorf("%v: code=%d err=%s", args, code, errb.String())
		}
	}
	errb.Reset()
	if code := runPing(nil, []string{"hb", "extra", "--server", "http://x"}, nil, &out, &errb); code != exitUsage || !strings.Contains(errb.String(), "unexpected argument") {
		t.Errorf("code=%d err=%s", code, errb.String())
	}
}

// spec schedule AC: User= and PATH come from runtime: (not $SUDO_USER, not sudo's
// PATH); the dry runs run first as that user with exactly HOME and PATH.
func TestScheduleInstallUsesRuntimeAndChecksFirst(t *testing.T) {
	cfgPath := writeAppYAML(t, "shop")
	raw, _ := os.ReadFile(cfgPath)
	os.WriteFile(cfgPath, append(raw, []byte("runtime:\n  user: deploy\n  path: /opt/docker/bin:/usr/bin:/bin\n  docker: /opt/docker/bin/docker\n")...), 0o644)
	sc := &fakeSystemctl{show: map[string]map[string]string{}}
	d, out, errb, fc := testDepsCheck(t, sc, "")
	d.invoker = func() string { return "khanh" } // whoever ran sudo does not matter
	d.lookPath = func(string) (string, error) { return "", errors.New("sudo's secure_path has no docker") }
	if code := runSchedule(context.Background(), []string{"install", "--yes", "-c", cfgPath}, d); code != exitOK {
		t.Fatalf("code=%d err=%s out=%s", code, errb, out)
	}
	b, _ := os.ReadFile(filepath.Join(d.unitDir, "bracer-shop-drill.service"))
	if !strings.Contains(string(b), "User=deploy\n") || !strings.Contains(string(b), "Environment=PATH=/opt/docker/bin:/usr/bin:/bin\n") || !strings.Contains(string(b), "Environment=HOME=/home/deploy\n") {
		t.Errorf("service:\n%s", b)
	}
	dir := filepath.Dir(cfgPath)
	want := []string{
		"1000 [1000 998] /usr/local/bin/bracer backup --dry-run -c " + cfgPath + " | HOME=/home/deploy PATH=/opt/docker/bin:/usr/bin:/bin | " + dir,
		"1000 [1000 998] /usr/local/bin/bracer drill --dry-run -c " + cfgPath + " | HOME=/home/deploy PATH=/opt/docker/bin:/usr/bin:/bin | " + dir,
	}
	if strings.Join(fc.runs, "\n") != strings.Join(want, "\n") {
		t.Errorf("checks:\n%s", strings.Join(fc.runs, "\n"))
	}

	// A failing dry run: nothing written, nothing called, exit 1.
	d, out, _, fc = testDepsCheck(t, sc, "")
	sc.calls = nil
	fc.fail["drill"] = "bracer drill: Docker is not available for the temporary Postgres: permission denied\nFix: bracer check\n"
	if code := runSchedule(context.Background(), []string{"install", "--yes", "-c", cfgPath}, d); code != exitError || !strings.Contains(out.String(), "Fix: bracer check") || !strings.Contains(out.String(), "Nothing installed. Fix the above, then run this command again.") || !strings.Contains(out.String(), "--no-check") {
		t.Errorf("code=%d out=%s", code, out)
	}
	if entries, _ := os.ReadDir(d.unitDir); len(entries) != 0 || len(sc.calls) != 0 {
		t.Errorf("failed check must write nothing: %v %v", entries, sc.calls)
	}

	// --no-check: warn and install.
	d, _, errb, fc = testDepsCheck(t, sc, "")
	fc.fail["drill"] = "x"
	if code := runSchedule(context.Background(), []string{"install", "--yes", "--no-check", "-c", cfgPath}, d); code != exitOK || len(fc.runs) != 0 || !strings.Contains(errb.String(), "warning: not checked") {
		t.Errorf("--no-check: code=%d runs=%v err=%s", code, fc.runs, errb)
	}
}

// drill.runtime: none → only the backup pair, checked and installed; print skips drill too.
func TestScheduleDrillOff(t *testing.T) {
	cfgPath := writeAppYAML(t, "shop")
	raw, _ := os.ReadFile(cfgPath)
	os.WriteFile(cfgPath, append(raw, []byte("drill:\n  runtime: none\n")...), 0o644)
	d, out, errb, fc := testDepsCheck(t, &fakeSystemctl{show: map[string]map[string]string{}}, "")
	if code := runSchedule(context.Background(), []string{"install", "--yes", "-c", cfgPath}, d); code != exitOK {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if len(fc.runs) != 1 || !strings.Contains(out.String(), "drill is off for app shop (drill.runtime: none)") {
		t.Errorf("runs=%v out=%s", fc.runs, out)
	}
	if _, err := os.Stat(filepath.Join(d.unitDir, "bracer-shop-drill.timer")); err == nil {
		t.Error("no drill timer with drill.runtime: none")
	}
	out.Reset()
	d.hasSystemd = func() bool { return false }
	if code := runSchedule(context.Background(), []string{"print", "-c", cfgPath}, d); code != exitOK || strings.Contains(out.String(), " drill  -c") {
		t.Errorf("print:\n%s", out)
	}
}

// status of a failed run shows its last error line and Fix line from the journal.
func TestScheduleStatusShowsLastFix(t *testing.T) {
	sc := &fakeSystemctl{show: map[string]map[string]string{
		"bracer-shop-backup.timer":   {"UnitFileState": "enabled", "ActiveState": "active", "NextElapseUSecRealtime": "Sun 2026-10-19 03:02:41 UTC"},
		"bracer-shop-backup.service": {"ActiveState": "failed", "Result": "exit-code", "ExecMainStatus": "1", "ExecMainExitTimestamp": "Sat 2026-10-18 03:01:52 UTC", "InvocationID": "abc123"},
	}}
	d, out, _ := testDeps(t, sc, "")
	var asked []string
	d.journal = func(_ context.Context, args ...string) (string, error) {
		asked = args
		return "time=… level=INFO msg=start\nbracer backup: app.yaml: instances[0]: pod is no longer supported\nFix: bracer init -c /srv/shop/app.yaml\n", nil
	}
	cfgPath := writeAppYAML(t, "shop")
	os.WriteFile(filepath.Join(d.unitDir, "bracer-shop-backup.timer"), []byte("x"), 0o644)
	if code := runSchedule(context.Background(), []string{"status", "-c", cfgPath}, d); code != exitError {
		t.Errorf("code=%d", code)
	}
	if !strings.Contains(out.String(), "        bracer backup: app.yaml: instances[0]: pod is no longer supported\n        Fix: bracer init -c /srv/shop/app.yaml\n") || len(asked) == 0 || asked[0] != "_SYSTEMD_INVOCATION_ID=abc123" {
		t.Errorf("asked=%v out:\n%s", asked, out)
	}
}
