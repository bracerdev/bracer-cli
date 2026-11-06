package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bracer.dev/bracer/internal/config"
)

func TestMain(m *testing.M) {
	// Every command finds the apps in the home of the user it runs as
	// (appHome); the tests stand that home in through $HOME.
	currentAccount = func() (string, string, error) { return "tester", os.Getenv("HOME"), nil }
	// Not root, whoever runs the tests.
	processEUID = func() int { return 1000 }
	os.Exit(m.Run())
}

// stubAccount makes the process run as name, whose home in the user
// database is home.
func stubAccount(t *testing.T, name, home string) {
	t.Helper()
	old := currentAccount
	currentAccount = func() (string, string, error) { return name, home, nil }
	t.Cleanup(func() { currentAccount = old })
}

// BR-017 item 1: the home is the user database's home of the user the
// command runs as; $HOME may only agree with it.
func TestBR017CheckHome(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, home, env, want, err string
	}{
		{"deploy", "/home/deploy", "", "/home/deploy", ""},
		{"deploy", "/home/deploy", "/home/deploy/", "/home/deploy", ""},
		{"deploy", real, link, real, ""},
		{"root", "/root", "/home/deploy", "", "bracer uses the home of the user it runs as (root, /root), but $HOME points elsewhere (/home/deploy)\nFix: run it as the user whose home is /home/deploy, without sudo -E, or unset the HOME override"},
		{"nobody", "", "", "", "user nobody has no home directory"},
	} {
		got, err := checkHome(tc.name, tc.home, tc.env)
		if tc.err != "" {
			if err == nil || err.Error() != tc.err {
				t.Errorf("checkHome(%q, %q, %q) error = %v, want %q", tc.name, tc.home, tc.env, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("checkHome(%q, %q, %q) = %q, %v, want %q", tc.name, tc.home, tc.env, got, err, tc.want)
		}
	}
}

// BR-017 item 1: under sudo every command (init, status, remove, backup,
// rotate-key) uses root's home, where `sudo bracer init` wrote the app and
// where the job (User=root) looks; $SUDO_USER plays no part.
func TestBR017OneHomeForEveryCommand(t *testing.T) {
	root := t.TempDir()
	stubAccount(t, "root", root)
	t.Setenv("HOME", root)
	t.Setenv("SUDO_USER", "deploy")
	var out, errb bytes.Buffer
	if h, err := defaultScheduleDeps(nil, &out, &errb).home(); err != nil || h != root {
		t.Errorf("status/remove home = %q, %v, want %s", h, err, root)
	}
	if h, err := defaultInitDeps(nil, &out, &errb).home(); err != nil || h != root {
		t.Errorf("init home = %q, %v, want %s", h, err, root)
	}

	// A HOME kept from the sudo caller (sudo -E) is refused by every command.
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	for name, run := range map[string]func() int{
		"status": func() int { return runStatus(ctx, []string{"shop"}, defaultScheduleDeps(nil, &out, &errb)) },
		"remove": func() int {
			d := defaultScheduleDeps(nil, &out, &errb)
			d.tty = true
			return runRemove(ctx, []string{"shop"}, d)
		},
		"backup":     func() int { return runBackup(ctx, []string{"shop", "--dry-run"}, defaultBackupDeps(nil, &out, &errb)) },
		"rotate-key": func() int { return runRotateKey(ctx, []string{"shop"}, strings.NewReader(""), &out, &errb) },
	} {
		out.Reset()
		errb.Reset()
		if code := run(); code != exitUsage || !strings.Contains(errb.String(), "bracer "+name+": bracer uses the home of the user it runs as (root, "+root+"), but $HOME points elsewhere") || strings.Contains(errb.String(), "nightly job") {
			t.Errorf("%s with a foreign HOME: %d\n%s", name, code, errb.String())
		}
	}
}

// BR-017 item 1: init refuses a HOME that is not the home of the user the
// job will run as before step 0 and before any question, instead of at the
// timer step after the whole proof.
func TestBR017InitRefusesForeignHomeFirst(t *testing.T) {
	stubAccount(t, "root", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	var out, errb bytes.Buffer
	d := defaultInitDeps(strings.NewReader("\n"), &out, &errb)
	var ran []string
	d.runTTY = func(_ context.Context, name string, args ...string) error {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return nil
	}
	d.env.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	d.env.hasSystemd = func() bool { return false }
	code := runInit(context.Background(), []string{"shop"}, d)
	if code != exitUsage || !strings.Contains(errb.String(), "bracer init: home directory: bracer uses the home of the user it runs as (root, ") {
		t.Errorf("exit %d\nstderr:\n%s", code, errb.String())
	}
	if out.Len() != 0 || ran != nil {
		t.Errorf("nothing may be checked or asked first:\n%s\n%v", out.String(), ran)
	}
}

// BR-017 item 2: a run made up at boot waits for the network and for the
// system Docker daemon, without starting Docker.
func TestBR017ServiceOrderedAfterDockerAndNetwork(t *testing.T) {
	svc := renderUnits(inputs(testApp("/home/deploy", "shop")))[0].content
	unit, _, _ := strings.Cut(svc, "[Service]")
	for _, want := range []string{"Wants=network-online.target\n", "After=network-online.target docker.service\n"} {
		if !strings.Contains(unit, want) {
			t.Errorf("[Unit] missing %q:\n%s", want, svc)
		}
	}
	for _, bad := range []string{"Wants=docker", "Requires=docker", "BindsTo="} {
		if strings.Contains(svc, bad) {
			t.Errorf("the service must not start or bind Docker (%s):\n%s", bad, svc)
		}
	}
}

// BR-017 item 4: values with a space, %, $, a quote or a backslash are
// written so systemd reads them back as they are; plain ones stay plain.
func TestBR017UnitValuesEscaped(t *testing.T) {
	in := inputs(testApp("/home/deploy", "shop"))
	in.binary = "/home/deploy/my bin/bracer"
	in.cfg.Runtime.Path = "/opt/my tools/bin:/usr/bin"
	in.home = "/home/50%"
	in.cfgPath = "/home/50%/.config/bracer/shop/app.yaml"
	svc := renderUnits(in)[0].content
	for _, want := range []string{
		"\nExecStart=\"/home/deploy/my bin/bracer\" backup shop\n",
		"\nEnvironment=\"PATH=/opt/my tools/bin:/usr/bin\"\n",
		"\nEnvironment=HOME=/home/50%%\n",
		"\nWorkingDirectory=/home/50%%/.config/bracer/shop\n",
	} {
		if !strings.Contains(svc, want) {
			t.Errorf("service missing %q:\n%s", want, svc)
		}
	}
	in.binary = `/opt/a$b/q"x\y/bracer`
	// systemd expands $ in arguments only, not in the executable path: no $$.
	if svc := renderUnits(in)[0].content; !strings.Contains(svc, "\nExecStart=\"/opt/a$b/q\\\"x\\\\y/bracer\" backup shop\n") {
		t.Errorf("$, \" and \\ in the binary path:\n%s", svc)
	}
	for s, want := range map[string]string{
		"/usr/local/bin/bracer": "/usr/local/bin/bracer",
		"PATH=/usr/bin:/bin":    "PATH=/usr/bin:/bin",
		"a b":                   `"a b"`,
		"100%":                  "100%%",
		"tab\there":             `"tab\there"`,
		"it's":                  `"it's"`,
	} {
		if got := unitWord(s); got != want {
			t.Errorf("unitWord(%q) = %s, want %s", s, got, want)
		}
	}
}

// stampLog wraps fakeRoot and records every root command in order.
func stampLog(t *testing.T, d *scheduleDeps, sc *fakeSystemctl) *[]string {
	var log []string
	root := fakeRoot(t, sc)
	d.root = func(ctx context.Context, name string, args ...string) error {
		log = append(log, name+" "+strings.Join(args, " "))
		return root(ctx, name, args...)
	}
	return &log
}

func touch(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// BR-017 item 3: remove deletes the Persistent= stamp of every timer of the
// app it removes, and install deletes the stamp of the timer it enables
// before enabling it, so remove then init never makes up a run minutes
// after init's own. Other apps' stamps stay.
func TestBR017TimerStampsRemoved(t *testing.T) {
	sc := &fakeSystemctl{}
	d, _, errb := testDeps(t, sc, "")
	log := stampLog(t, &d, sc)
	stamp := func(timer string) string { return filepath.Join(d.stampDir, "stamp-"+timer) }
	touch(t, filepath.Join(d.unitDir, "bracer-shop.service"), filepath.Join(d.unitDir, "bracer-shop.timer"),
		stamp("bracer-shop.timer"), stamp("bracer-shop-2.timer"), stamp("bracer-blog.timer"))
	if err := uninstallUnits(context.Background(), "shop", d); err != nil {
		t.Fatalf("uninstallUnits: %v\n%s", err, errb)
	}
	for timer, want := range map[string]bool{"bracer-shop.timer": false, "bracer-shop-2.timer": true, "bracer-blog.timer": true} {
		if exists(stamp(timer)) != want {
			t.Errorf("after remove, stamp of %s exists = %v, want %v", timer, !want, want)
		}
	}

	// A stamp left behind (a remove by an earlier bracer) is gone before
	// the new timer starts.
	touch(t, stamp("bracer-shop.timer"))
	cfgPath := writeAppYAML(t, "shop")
	*log = nil
	if err := installUnits(context.Background(), loadAppYAML(t, cfgPath), cfgPath, d); err != nil {
		t.Fatalf("installUnits: %v\n%s", err, errb)
	}
	if exists(stamp("bracer-shop.timer")) || !exists(stamp("bracer-blog.timer")) {
		t.Errorf("install: shop's stamp must go, blog's stay")
	}
	calls := strings.Join(*log, "\n")
	rm := "rm -f " + stamp("bracer-shop.timer")
	if i, j := strings.Index(calls, rm), strings.Index(calls, "systemctl enable --now bracer-shop.timer"); i < 0 || j < 0 || i > j {
		t.Errorf("the stamp must be removed before the timer starts:\n%s", calls)
	}
}

// BR-017 item 5: an app name is at most config.MaxAppName characters, so
// its unit names and stamp file fit systemd's limits; init refuses a longer
// one before anything else.
func TestBR017AppNameLength(t *testing.T) {
	longest := strings.Repeat("a", config.MaxAppName)
	if !config.ValidAppName(longest) || config.ValidAppName(longest+"a") {
		t.Errorf("the limit must be %d characters", config.MaxAppName)
	}
	if n := len(unitBase(longest) + ".service"); n > 256 {
		t.Errorf("unit name of the longest app: %d characters", n)
	}
	if n := len("stamp-" + unitBase(longest) + ".timer"); n > 255 {
		t.Errorf("stamp file of the longest app: %d characters", n)
	}
	// The refusal names the problem: the length when the characters are fine.
	long := strings.Repeat("a", 250)
	tooLong := "at most 64 characters (it is part of the systemd unit names)"
	cfg := testApp("/home/deploy", long)
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "app: "+tooLong) || strings.Contains(err.Error(), "only") {
		t.Errorf("Validate: %v", err)
	}
	if _, err := resolveApp(t.TempDir(), long, "status"); err == nil || !strings.Contains(err.Error(), "is not an app name: "+tooLong) {
		t.Errorf("resolveApp: %v", err)
	}
	if _, err := resolveApp(t.TempDir(), "Shop", "status"); err == nil || !strings.Contains(err.Error(), "is not an app name: lowercase letters, digits, `-`, `_` only") {
		t.Errorf("resolveApp, bad characters: %v", err)
	}
	code, out, errb := runWorld(t, t.TempDir(), &world{args: []string{long}})
	if code != exitUsage || !strings.Contains(errb, "is not an app name: "+tooLong) || out != "" {
		t.Errorf("init with a 250-character name: %d\n%s%s", code, out, errb)
	}
}

// BR-017 review: a uid with no user database entry is a problem line and a
// Fix line, not Go's raw error.
func TestBR017NoPasswdEntry(t *testing.T) {
	old := currentAccount
	currentAccount = func() (string, string, error) {
		return "", "", errors.New("user: Current requires cgo or $USER, $HOME set in environment")
	}
	t.Cleanup(func() { currentAccount = old })
	_, err := appHome()
	if err == nil {
		t.Fatal("appHome must fail")
	}
	lines := strings.Split(err.Error(), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "has no entry in the user database (/etc/passwd)") || lines[1] != "Fix: run bracer as a user that has an /etc/passwd entry" || strings.Contains(err.Error(), "cgo") {
		t.Errorf("error:\n%s", err)
	}
}

// BR-017 review: restore's closing hint looks for the app in appHome too.
// HOME is empty, so only the user database's home can find the app.
func TestBR017RestoreHintUsesAppHome(t *testing.T) {
	home := t.TempDir()
	stubAccount(t, "deploy", home)
	t.Setenv("HOME", "")
	if got := initHint("shop"); got != "to back up this server: bracer init shop\n" {
		t.Errorf("no app here: %q", got)
	}
	cfg := testApp(home, "shop")
	p := config.AppPath(home, "shop")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(p, cfg); err != nil {
		t.Fatal(err)
	}
	if got := initHint("shop"); got != "" {
		t.Errorf("app in appHome: %q", got)
	}
}

// BR-017 review: the real stamp directory of system timers.
func TestBR017StampDirOfSystemTimers(t *testing.T) {
	d := defaultScheduleDeps(nil, &bytes.Buffer{}, &bytes.Buffer{})
	if d.stampDir != "/var/lib/systemd/timers" || d.stampFile("bracer-shop.timer") != "/var/lib/systemd/timers/stamp-bracer-shop.timer" {
		t.Errorf("stampDir %q", d.stampDir)
	}
}
