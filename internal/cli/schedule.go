package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"bracer.dev/bracer/internal/config"
)

// scheduleDeps is what `bracer schedule` needs from the machine, injected so
// tests run with a temporary unit directory and a fake systemctl.
type scheduleDeps struct {
	unitDir    string                                                    // /etc/systemd/system
	systemctl  func(ctx context.Context, args ...string) (string, error) // runs systemctl, returns stdout
	hasSystemd func() bool
	euid       int
	lookPath   func(string) (string, error)
	executable func() string        // stableBinaryPath
	invoker    func() (name string) // $SUDO_USER, else the current user
	lookupUser func(name string) (account, error)
	// check runs a dry run exactly as the unit will (install step 4b); returns the exit code.
	check func(ctx context.Context, a account, dir string, env, args []string, stdout, stderr io.Writer) int
	// journal runs journalctl and returns stdout (status: the last error and Fix line).
	journal func(ctx context.Context, args ...string) (string, error)
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
}

func defaultScheduleDeps(stdin io.Reader, stdout, stderr io.Writer) scheduleDeps {
	return scheduleDeps{
		unitDir:    "/etc/systemd/system",
		systemctl:  execSystemctl,
		hasSystemd: hasSystemd,
		euid:       os.Geteuid(),
		lookPath:   exec.LookPath,
		executable: stableBinaryPath,
		invoker: func() string {
			if s := os.Getenv("SUDO_USER"); s != "" {
				return s
			}
			if u, err := user.Current(); err == nil {
				return u.Username
			}
			return ""
		},
		lookupUser: lookupAccount,
		check:      execAs,
		journal: func(ctx context.Context, args ...string) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, "journalctl", args...).Output()
			return string(out), err
		},
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
	}
}

// account is the user the jobs run as: what User= means to systemd.
type account struct {
	home     string
	uid, gid uint32
	groups   []uint32 // supplementary groups (docker), as systemd sets them for User=
}

func lookupAccount(name string) (account, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return account{}, err
	}
	a := account{home: u.HomeDir}
	uid, err1 := strconv.ParseUint(u.Uid, 10, 32)
	gid, err2 := strconv.ParseUint(u.Gid, 10, 32)
	if err1 != nil || err2 != nil {
		return account{}, fmt.Errorf("user %s: non-numeric uid/gid", name)
	}
	a.uid, a.gid = uint32(uid), uint32(gid)
	ids, _ := u.GroupIds()
	for _, g := range ids {
		if n, err := strconv.ParseUint(g, 10, 32); err == nil {
			a.groups = append(a.groups, uint32(n))
		}
	}
	return a, nil
}

// execAs runs args[0] with exactly env, in dir, as account a (when running as
// root, which install requires): the same uid, gid and groups the unit gets.
func execAs(ctx context.Context, a account, dir string, env, args []string, stdout, stderr io.Writer) int {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env, cmd.Dir, cmd.Stdout, cmd.Stderr = env, dir, stdout, stderr
	if os.Geteuid() == 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: a.uid, Gid: a.gid, Groups: a.groups}}
	}
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintln(stderr, "bracer schedule install:", err)
		return exitError
	}
	return exitOK
}

// execSystemctl runs systemctl with a timeout; stderr is folded into the error.
func execSystemctl(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// hasSystemd: systemd is PID 1 (the directory exists only then) and systemctl is on PATH.
func hasSystemd() bool {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return false
	}
	_, err := exec.LookPath("systemctl")
	return err == nil
}

const scheduleUsage = `Usage:
  bracer schedule install -c app.yaml [--backup-at 03:00] [--drill-at 04:00] [--user NAME] [--binary PATH] [--yes] [--no-check]
  bracer schedule remove  -c app.yaml
  bracer schedule status  -c app.yaml
  bracer schedule print   -c app.yaml [--format cron|systemd] [--backup-at ...] [--drill-at ...]

install writes bracer-<app>-backup and bracer-<app>-drill service/timer units
to /etc/systemd/system and starts the timers (needs sudo). remove stops and
deletes them. Before writing anything, install runs backup --dry-run and
drill --dry-run as the jobs will (same user, HOME and PATH). status shows the
last and next run. print only prints a crontab
or the units, for machines without systemd.

Times are HH:MM (daily, machine time zone) or a cron expression such as
"0 4 * * 1" (fixed minute and hour, optional weekdays).
`

// runSchedule is `bracer schedule <install|remove|status|print>` (spec backup/schedule).
func runSchedule(ctx context.Context, args []string, d scheduleDeps) int {
	if len(args) == 0 {
		fmt.Fprint(d.stderr, scheduleUsage)
		return exitUsage
	}
	switch args[0] {
	case "install":
		return runScheduleInstall(ctx, args[1:], d)
	case "remove":
		return runScheduleRemove(ctx, args[1:], d)
	case "status":
		return runScheduleStatus(ctx, args[1:], d)
	case "print":
		return runSchedulePrint(ctx, args[1:], d)
	case "help", "--help", "-h":
		fmt.Fprint(d.stdout, scheduleUsage)
		return exitOK
	}
	fmt.Fprintf(d.stderr, "bracer schedule: unknown subcommand %q\n\n%s", args[0], scheduleUsage)
	return exitUsage
}

// runCronExample keeps the old command working: a warning, then `schedule print`.
func runCronExample(ctx context.Context, args []string, d scheduleDeps) int {
	fmt.Fprintln(d.stderr, "warning: cron-example is deprecated, use `bracer schedule print` (or `bracer schedule install` to let bracer set up systemd timers)")
	return runSchedulePrint(ctx, args, d)
}

// when is one schedule time in both forms. onCalendar is "" when the cron
// expression has no systemd equivalent bracer can produce.
type when struct {
	cron       string
	onCalendar string
}

var (
	reCron  = regexp.MustCompile(`^\S+(\s+\S+){4}$`)
	reClock = regexp.MustCompile(`^([01]?[0-9]|2[0-3]):([0-5][0-9])$`)
)

// parseWhen accepts "HH:MM" or a 5-field cron expression.
func parseWhen(s string) (when, error) {
	s = strings.TrimSpace(s)
	if m := reClock.FindStringSubmatch(s); m != nil {
		h, min := strings.TrimLeft(m[1], "0"), strings.TrimLeft(m[2], "0")
		if h == "" {
			h = "0"
		}
		if min == "" {
			min = "0"
		}
		return when{cron: fmt.Sprintf("%s %s * * *", min, h), onCalendar: fmt.Sprintf("*-*-* %02s:%02s:00", h, min)}, nil
	}
	if !reCron.MatchString(s) {
		return when{}, fmt.Errorf("%q is neither HH:MM nor a 5-field cron expression", s)
	}
	return when{cron: s, onCalendar: cronToOnCalendar(s)}, nil
}

// scheduleInputs is everything the unit and crontab templates need.
type scheduleInputs struct {
	cfg        *config.App
	cfgPath    string // absolute
	binary     string // absolute, or "bracer" if the running one is not at a stable path
	path       string // PATH of the jobs: runtime.path, else docker's directory + the usual ones
	drill      bool   // drill is scheduled: drill.runtime is not none and (runtime: is set or docker is on PATH)
	username   string
	home       string
	backupAt   when
	drillAt    when
	secretsAbs string // absolute secrets_file, "" if none
}

// scheduleFlags parses the flags shared by install and print and loads app.yaml.
func scheduleFlags(name string, args []string, d scheduleDeps, extra func(*flag.FlagSet)) (*flag.FlagSet, scheduleInputs, int) {
	fs := flag.NewFlagSet("schedule "+name, flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	cfgPath := fs.String("c", "app.yaml", "path of app.yaml")
	backupAt := fs.String("backup-at", "03:00", "when to run backup: HH:MM or a cron expression")
	drillAt := fs.String("drill-at", "04:00", "when to run drill: HH:MM or a cron expression")
	if extra != nil {
		extra(fs)
	}
	if err := fs.Parse(args); err != nil {
		return nil, scheduleInputs{}, exitUsage
	}
	fail := func(format string, a ...interface{}) (*flag.FlagSet, scheduleInputs, int) {
		fmt.Fprintf(d.stderr, "bracer schedule %s: "+format+"\n", append([]interface{}{name}, a...)...)
		return nil, scheduleInputs{}, exitUsage
	}
	if fs.NArg() > 0 {
		return fail("unexpected argument %q", fs.Arg(0))
	}
	var in scheduleInputs
	var err error
	if in.backupAt, err = parseWhen(*backupAt); err != nil {
		return fail("--backup-at: %v", err)
	}
	if in.drillAt, err = parseWhen(*drillAt); err != nil {
		return fail("--drill-at: %v", err)
	}
	if in.cfg, err = config.Load(*cfgPath); err != nil {
		return fail("%v", err)
	}
	in.cfgPath, _ = filepath.Abs(*cfgPath)
	in.binary = d.executable()
	// app.yaml written by init records the context that worked (spec init § Data
	// model runtime:); nothing is looked up again under sudo, whose PATH is secure_path.
	dockerFound := in.cfg.Runtime != nil
	if rt := in.cfg.Runtime; rt != nil && rt.Path != "" {
		in.path = rt.Path
	} else {
		var dirs []string
		if p, err := d.lookPath("docker"); err == nil {
			dirs = append(dirs, filepath.Dir(p))
			dockerFound = true
		}
		in.path = joinPath(strings.Join(dirs, ":"), "/usr/local/bin:/usr/bin:/bin")
	}
	in.drill = !in.cfg.DrillOff() && dockerFound
	if rt := in.cfg.Runtime; rt != nil && rt.User != "" {
		in.username = rt.User
	} else {
		in.username = d.invoker()
	}
	if in.username != "" {
		if a, err := d.lookupUser(in.username); err == nil {
			in.home = a.home
		}
	}
	if in.cfg.SecretsFile != "" {
		p, _ := config.ExpandHome(in.cfg.SecretsFile)
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(in.cfgPath), p)
		}
		in.secretsAbs = p
	}
	return fs, in, exitOK
}

// loadApp reads app.yaml for remove and status, which only need the app name.
func loadApp(name string, args []string, d scheduleDeps) (*config.App, int) {
	fs := flag.NewFlagSet("schedule "+name, flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	cfgPath := fs.String("c", "app.yaml", "path of app.yaml")
	if err := fs.Parse(args); err != nil {
		return nil, exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(d.stderr, "bracer schedule %s: unexpected argument %q\n", name, fs.Arg(0))
		return nil, exitUsage
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(d.stderr, "bracer schedule %s: %v\n", name, err)
		return nil, exitUsage
	}
	return cfg, exitOK
}

// preflight refuses install/remove on a machine without systemd or without root.
func preflight(name string, d scheduleDeps) int {
	if !d.hasSystemd() {
		fmt.Fprintf(d.stderr, "bracer schedule %s: this machine does not run systemd. Print a crontab instead and paste it with crontab -e:\n  bracer schedule print --format cron -c <app.yaml>\n", name)
		return exitUsage
	}
	if d.euid != 0 {
		fmt.Fprintf(d.stderr, "bracer schedule %s: writing %s needs root: run it with sudo\n", name, d.unitDir)
		return exitUsage
	}
	return exitOK
}

var scheduleJobs = []string{"backup", "drill"}

func unitBase(app, job string) string { return "bracer-" + app + "-" + job }

// unitFile is one rendered unit: its file name and content.
type unitFile struct {
	name, content string
}

// renderUnits renders service and timer for each job, in a stable order.
func renderUnits(in scheduleInputs, jobs []string) []unitFile {
	var files []unitFile
	for _, job := range jobs {
		at := in.backupAt
		if job == "drill" {
			at = in.drillAt
		}
		base := unitBase(in.cfg.App, job)
		var s strings.Builder
		fmt.Fprintf(&s, "[Unit]\nDescription=Bracer %s for %s\n", job, in.cfg.App)
		s.WriteString("# Written by `bracer schedule install`; running it again overwrites this file, `bracer schedule remove` deletes it.\n\n")
		s.WriteString("[Service]\nType=oneshot\n")
		if in.username != "" {
			fmt.Fprintf(&s, "User=%s\n", in.username)
		}
		fmt.Fprintf(&s, "WorkingDirectory=%s\n", filepath.Dir(in.cfgPath))
		if in.home != "" {
			fmt.Fprintf(&s, "Environment=HOME=%s\n", in.home)
		}
		fmt.Fprintf(&s, "Environment=PATH=%s\n", in.path)
		s.WriteString("SyslogIdentifier=bracer\n")
		fmt.Fprintf(&s, "ExecStart=%s %s -c %s\n", in.binary, job, in.cfgPath)
		files = append(files, unitFile{base + ".service", s.String()})
		files = append(files, unitFile{base + ".timer", fmt.Sprintf("[Unit]\nDescription=Run %s.service\n\n[Timer]\nOnCalendar=%s\nPersistent=true\nRandomizedDelaySec=5m\n\n[Install]\nWantedBy=timers.target\n", base, at.onCalendar)})
	}
	return files
}

// jobs is backup, plus drill when it can run here.
func (in scheduleInputs) jobs() []string {
	if in.drill {
		return scheduleJobs
	}
	return []string{"backup"}
}

// drillNote explains why drill is not scheduled on this machine.
func (in scheduleInputs) drillNote() string {
	if in.cfg.DrillOff() {
		return fmt.Sprintf("drill is off for app %s (drill.runtime: none): only backup is scheduled. To test-restore on another machine, copy app.yaml and the key file there and set drill.runtime.", in.cfg.App)
	}
	return "drill is not scheduled: docker was not found on PATH. Run bracer init again on this machine, or run drill on a machine that has Docker, with the same app.yaml and key file."
}

func runScheduleInstall(ctx context.Context, args []string, d scheduleDeps) int {
	var yes, noCheck bool
	var userFlag, binaryFlag string
	_, in, code := scheduleFlags("install", args, d, func(fs *flag.FlagSet) {
		fs.BoolVar(&yes, "yes", false, "do not ask for confirmation")
		fs.BoolVar(&noCheck, "no-check", false, "install without the backup/drill dry runs")
		fs.StringVar(&userFlag, "user", "", "run the jobs as this user (default: runtime.user in app.yaml, else the user who ran sudo)")
		fs.StringVar(&binaryFlag, "binary", "", "bracer binary the units run (default: this one, or bracer on PATH)")
	})
	if code != exitOK {
		return code
	}
	fail := func(code int, format string, a ...interface{}) int {
		fmt.Fprintf(d.stderr, "bracer schedule install: "+format+"\n", a...)
		return code
	}
	for _, w := range []struct{ flag, cron, cal string }{{"--backup-at", in.backupAt.cron, in.backupAt.onCalendar}, {"--drill-at", in.drillAt.cron, in.drillAt.onCalendar}} {
		if w.cal == "" {
			return fail(exitUsage, "%s %q: only a fixed minute and hour, daily or on weekdays (\"0 4 * * 1,4\"), can be turned into a systemd timer", w.flag, w.cron)
		}
	}
	if code := preflight("install", d); code != exitOK {
		return code
	}
	if binaryFlag != "" {
		in.binary, _ = filepath.Abs(binaryFlag)
	}
	if in.binary == "bracer" {
		p, err := d.lookPath("bracer")
		if err != nil {
			return fail(exitUsage, "bracer is running from a temporary path and no bracer was found on PATH; install it (e.g. /usr/local/bin/bracer) or pass --binary <path>")
		}
		in.binary = p
	}
	if userFlag != "" {
		in.username = userFlag
	}
	if in.username == "" {
		return fail(exitError, "cannot tell which user should run the jobs; pass --user")
	}
	acct, err := d.lookupUser(in.username)
	if err != nil {
		return fail(exitUsage, "user %q: unknown user", in.username)
	}
	in.home = acct.home

	jobs := in.jobs()
	// 4b. Dry runs as the unit will run them, before anything is written.
	if noCheck {
		fmt.Fprintln(d.stderr, "warning: not checked; the jobs may fail — see bracer schedule status after the first run")
	} else {
		env := []string{"HOME=" + in.home, "PATH=" + in.path}
		for _, job := range jobs {
			args := []string{in.binary, job, "--dry-run", "-c", in.cfgPath}
			fmt.Fprintf(d.stdout, "Checking as %s: %s\n", in.username, strings.Join(args[1:], " "))
			if code := d.check(ctx, acct, filepath.Dir(in.cfgPath), env, args, d.stdout, d.stderr); code != exitOK {
				fmt.Fprintf(d.stdout, "\nNothing installed. Fix the above, then run this command again.\n(--no-check installs without this check.)\n")
				return exitError
			}
		}
		fmt.Fprintln(d.stdout)
	}
	files := renderUnits(in, jobs)
	for _, f := range files {
		fmt.Fprintf(d.stdout, "# ---- %s ----\n%s\n", filepath.Join(d.unitDir, f.name), f.content)
	}
	if !in.drill {
		fmt.Fprintln(d.stdout, "Note: "+in.drillNote())
	}
	fmt.Fprintf(d.stdout, "Times are in this machine's time zone. Edit a timer by hand if you need another schedule; `bracer schedule install` overwrites it.\n\n")
	if !yes {
		ok, err := newPrompter(d.stdin, d.stdout).yesNo("Install these units and start the timers?", false)
		if err != nil {
			if errors.Is(err, ErrNoInput) {
				return fail(exitUsage, "no terminal to confirm on; pass --yes")
			}
			return fail(exitError, "%v", err)
		}
		if !ok {
			fmt.Fprintln(d.stdout, "Nothing installed.")
			return exitOK
		}
	}

	// Write the new set, then retire units of this app that are not in it
	// (a drill pair from an earlier install, now drill.runtime: none).
	want := map[string]bool{}
	for _, f := range files {
		want[f.name] = true
		if err := writeUnit(filepath.Join(d.unitDir, f.name), f.content); err != nil {
			if errors.Is(err, os.ErrPermission) {
				return fail(exitUsage, "%v: run it with sudo", err)
			}
			return fail(exitError, "%v", err)
		}
	}
	stale, err := appUnits(d.unitDir, in.cfg.App)
	if err != nil {
		return fail(exitError, "%v", err)
	}
	for _, name := range stale {
		if want[name] {
			continue
		}
		if strings.HasSuffix(name, ".timer") {
			if _, err := d.systemctl(ctx, "disable", "--now", name); err != nil {
				fmt.Fprintf(d.stderr, "warning: %v\n", err)
			}
		}
		if err := os.Remove(filepath.Join(d.unitDir, name)); err != nil {
			return fail(exitError, "%v", err)
		}
		fmt.Fprintf(d.stdout, "Removed stale %s\n", name)
	}
	if _, err := d.systemctl(ctx, "daemon-reload"); err != nil {
		return fail(exitError, "%v", err)
	}
	enable := []string{"enable", "--now"}
	for _, job := range jobs {
		enable = append(enable, unitBase(in.cfg.App, job)+".timer")
	}
	if _, err := d.systemctl(ctx, enable...); err != nil {
		return fail(exitError, "%v\nThe unit files are in %s; check `systemctl status <unit>`, or run `bracer schedule remove` to take them out again", err, d.unitDir)
	}
	fmt.Fprintf(d.stdout, "Installed %d units in %s.\n", len(files), d.unitDir)
	return printStatus(ctx, in.cfg.App, d)
}

// writeUnit writes the file atomically with mode 0644.
func writeUnit(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// appUnits lists the unit files of one app in dir: only names that start with
// bracer-<app>- and end in .service or .timer, so no other unit is ever touched.
func appUnits(dir, app string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	prefix := "bracer-" + app + "-"
	var names []string
	for _, e := range entries {
		n := e.Name()
		if e.Type().IsRegular() && strings.HasPrefix(n, prefix) && (strings.HasSuffix(n, ".service") || strings.HasSuffix(n, ".timer")) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}

func runScheduleRemove(ctx context.Context, args []string, d scheduleDeps) int {
	cfg, code := loadApp("remove", args, d)
	if code != exitOK {
		return code
	}
	if code := preflight("remove", d); code != exitOK {
		return code
	}
	fail := func(format string, a ...interface{}) int {
		fmt.Fprintf(d.stderr, "bracer schedule remove: "+format+"\n", a...)
		return exitError
	}
	names, err := appUnits(d.unitDir, cfg.App)
	if err != nil {
		return fail("%v", err)
	}
	if len(names) == 0 {
		fmt.Fprintf(d.stdout, "No schedule installed for app %s.\n", cfg.App)
		return exitOK
	}
	// Timers first so nothing starts a service while it is being removed.
	for _, name := range names {
		if strings.HasSuffix(name, ".timer") {
			if _, err := d.systemctl(ctx, "disable", "--now", name); err != nil {
				fmt.Fprintf(d.stderr, "warning: %v\n", err)
			}
		}
	}
	for _, name := range names {
		if strings.HasSuffix(name, ".service") {
			if _, err := d.systemctl(ctx, "stop", name); err != nil {
				fmt.Fprintf(d.stderr, "warning: %v\n", err)
			}
		}
	}
	for _, name := range names {
		if err := os.Remove(filepath.Join(d.unitDir, name)); err != nil {
			return fail("%v", err)
		}
		fmt.Fprintf(d.stdout, "Removed %s\n", filepath.Join(d.unitDir, name))
	}
	if _, err := d.systemctl(ctx, "daemon-reload"); err != nil {
		return fail("%v", err)
	}
	return exitOK
}

func runScheduleStatus(ctx context.Context, args []string, d scheduleDeps) int {
	cfg, code := loadApp("status", args, d)
	if code != exitOK {
		return code
	}
	if !d.hasSystemd() {
		fmt.Fprintln(d.stderr, "bracer schedule status: this machine does not run systemd; check your crontab instead")
		return exitUsage
	}
	return printStatus(ctx, cfg.App, d)
}

// printStatus prints one block per installed job and returns exit 1 when a
// job's last run failed or nothing is installed.
func printStatus(ctx context.Context, app string, d scheduleDeps) int {
	installed, err := appUnits(d.unitDir, app)
	if err != nil {
		fmt.Fprintf(d.stderr, "bracer schedule status: %v\n", err)
		return exitError
	}
	has := map[string]bool{}
	for _, n := range installed {
		has[n] = true
	}
	code := exitOK
	any := false
	for _, job := range scheduleJobs {
		base := unitBase(app, job)
		if !has[base+".timer"] {
			continue
		}
		any = true
		timer, err := showUnit(ctx, d, base+".timer", "UnitFileState", "ActiveState", "NextElapseUSecRealtime", "LastTriggerUSec")
		if err != nil {
			fmt.Fprintf(d.stderr, "bracer schedule status: %v\n", err)
			return exitError
		}
		svc, err := showUnit(ctx, d, base+".service", "ActiveState", "Result", "ExecMainStatus", "ExecMainStartTimestamp", "ExecMainExitTimestamp", "InvocationID")
		if err != nil {
			fmt.Fprintf(d.stderr, "bracer schedule status: %v\n", err)
			return exitError
		}
		state := timer["UnitFileState"]
		if timer["ActiveState"] != "active" {
			state += ", not active"
		}
		fmt.Fprintf(d.stdout, "%s: %s.timer %s, next run %s\n", job, base, state, orNever(timer["NextElapseUSecRealtime"]))
		switch {
		case svc["ActiveState"] == "activating" || svc["ActiveState"] == "active":
			fmt.Fprintf(d.stdout, "%s  running since %s\n", strings.Repeat(" ", len(job)), orNever(svc["ExecMainStartTimestamp"]))
		case isUnset(svc["ExecMainExitTimestamp"]):
			fmt.Fprintf(d.stdout, "%s  last run: never\n", strings.Repeat(" ", len(job)))
		default:
			result := svc["Result"]
			if result != "success" {
				result = fmt.Sprintf("failed (%s, exit status %s)", result, svc["ExecMainStatus"])
				code = exitError
			}
			fmt.Fprintf(d.stdout, "%s  last run %s: %s\n", strings.Repeat(" ", len(job)), svc["ExecMainExitTimestamp"], result)
			if svc["Result"] != "success" {
				for _, l := range lastFailure(ctx, d, svc["InvocationID"]) {
					fmt.Fprintf(d.stdout, "%s  %s\n", strings.Repeat(" ", len(job)), l)
				}
			}
		}
	}
	if !any {
		fmt.Fprintf(d.stdout, "No schedule installed for app %s. Run: sudo bracer schedule install -c <app.yaml>\n", app)
		return exitError
	}
	if code != exitOK {
		fmt.Fprintf(d.stdout, "Logs: journalctl -t bracer\n")
	}
	return code
}

// lastFailure reads the log of the service's last run and returns its last
// error line and last Fix line; nothing when the journal cannot be read.
func lastFailure(ctx context.Context, d scheduleDeps, invocation string) []string {
	if d.journal == nil || isUnset(invocation) {
		return nil
	}
	out, err := d.journal(ctx, "_SYSTEMD_INVOCATION_ID="+invocation, "-n", "200", "-o", "cat", "--no-pager")
	if err != nil {
		return nil
	}
	var errLine, fix string
	for _, l := range strings.Split(out, "\n") {
		t := strings.TrimSpace(l)
		switch {
		case t == "":
		case strings.HasPrefix(t, "Fix:"):
			fix = t
		case strings.HasPrefix(t, "bracer "), strings.HasPrefix(t, "✗ "), strings.Contains(t, "level=ERROR"):
			errLine = t
		}
	}
	var lines []string
	for _, l := range []string{errLine, fix} {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// showUnit runs `systemctl show -p ...` and returns the properties as a map.
func showUnit(ctx context.Context, d scheduleDeps, unit string, props ...string) (map[string]string, error) {
	out, err := d.systemctl(ctx, "show", "-p", strings.Join(props, ","), unit)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			m[k] = strings.TrimSpace(v)
		}
	}
	return m, nil
}

func isUnset(v string) bool { return v == "" || v == "n/a" || v == "0" }

func orNever(v string) string {
	if isUnset(v) {
		return "never"
	}
	return v
}

func runSchedulePrint(ctx context.Context, args []string, d scheduleDeps) int {
	var format string
	_, in, code := scheduleFlags("print", args, d, func(fs *flag.FlagSet) {
		fs.StringVar(&format, "format", "cron", "cron | systemd")
	})
	if code != exitOK {
		return code
	}
	switch format {
	case "cron":
		renderCron(d.stdout, in)
	case "systemd":
		for _, w := range []struct{ flag, cron, cal string }{{"--backup-at", in.backupAt.cron, in.backupAt.onCalendar}, {"--drill-at", in.drillAt.cron, in.drillAt.onCalendar}} {
			if w.cal == "" {
				fmt.Fprintf(d.stderr, "bracer schedule print: %s %q: only a fixed minute and hour, daily or on weekdays, can be turned into a systemd timer\n", w.flag, w.cron)
				return exitUsage
			}
		}
		renderSystemd(d.stdout, in)
	default:
		fmt.Fprintln(d.stderr, "bracer schedule print: --format must be cron or systemd")
		return exitUsage
	}
	return exitOK
}

// stableBinaryPath: path of the running binary, symlinks resolved; a temporary
// path from `go run`/go-build or the temp directory returns a bare "bracer".
func stableBinaryPath() string {
	p, err := os.Executable()
	if err != nil {
		return "bracer"
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if strings.Contains(p, "go-build") || strings.HasPrefix(p, os.TempDir()) {
		return "bracer"
	}
	return p
}

// secretEnvNames lists the secret variable names app.yaml needs, without values.
func secretEnvNames(cfg *config.App) []string {
	set := map[string]bool{}
	for _, in := range cfg.Instances {
		for _, n := range []string{in.PasswordEnv, in.URLEnv} {
			if n != "" {
				set[n] = true
			}
		}
	}
	if cfg.Storage != nil {
		set[cfg.Storage.AccessKeyEnv] = true
		set[cfg.Storage.SecretKeyEnv] = true
	}
	if cfg.Hosted != nil {
		set[cfg.Hosted.TokenEnv] = true
	}
	var names []string
	for n := range set {
		if n != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

func (in scheduleInputs) secretsNote() []string {
	if in.secretsAbs != "" {
		return []string{fmt.Sprintf("# Secrets are read from secrets_file (%s); nothing to add here.", in.secretsAbs)}
	}
	names := secretEnvNames(in.cfg)
	if len(names) == 0 {
		return nil
	}
	return []string{
		"# app.yaml has no secrets_file, so these variables must be set for the jobs:",
		"#   " + strings.Join(names, ", "),
		"# Easiest: run `bracer init -c " + in.cfgPath + "` again and let it write a secrets_file,",
		"# or put them in a chmod 600 file and pass --env-file <path> on both commands.",
	}
}

func (in scheduleInputs) hostedNote() string {
	if in.cfg.Hosted == nil {
		return "# No hosted: in app.yaml, so runs are not reported anywhere. Add hosted: to get alerts when a run fails or goes missing."
	}
	return "# Results are reported to " + in.cfg.Hosted.Server + " after each run."
}

func (in scheduleInputs) binaryNote() string {
	if in.binary == "bracer" {
		return "# bracer is running from a temporary path; install it to a fixed path (e.g. /usr/local/bin/bracer) and adjust below."
	}
	return ""
}

func renderCron(w io.Writer, in scheduleInputs) {
	fmt.Fprintf(w, "# Bracer schedule for app %q. Install with: crontab -e (paste below)\n", in.cfg.App)
	fmt.Fprintln(w, in.hostedNote())
	if n := in.binaryNote(); n != "" {
		fmt.Fprintln(w, n)
	}
	for _, l := range in.secretsNote() {
		fmt.Fprintln(w, l)
	}
	fmt.Fprintln(w, "# Output goes to syslog; check with: journalctl -t bracer  (or /var/log/syslog)")
	fmt.Fprintln(w)
	if !in.drill {
		fmt.Fprintln(w, "# "+in.drillNote())
	}
	fmt.Fprintf(w, "PATH=%s\n", in.path)
	fmt.Fprintf(w, "%s %s backup -c %s 2>&1 | logger -t bracer\n", in.backupAt.cron, in.binary, in.cfgPath)
	if in.drill {
		fmt.Fprintf(w, "%s %s drill  -c %s 2>&1 | logger -t bracer\n", in.drillAt.cron, in.binary, in.cfgPath)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "# Watch any other job the same way (create a heartbeat on the dashboard, paste its id):")
	fmt.Fprintf(w, "# 30 2 * * * %s ping <heartbeat-id> -- /path/to/your-job.sh\n", in.binary)
}

// renderSystemd prints the same units `install` writes, for pasting by hand.
func renderSystemd(w io.Writer, in scheduleInputs) {
	app := in.cfg.App
	fmt.Fprintf(w, "# Bracer schedule for app %q as systemd units; `sudo bracer schedule install` writes these for you.\n", app)
	fmt.Fprintln(w, "# By hand: save each block to /etc/systemd/system/<name>, then:")
	var timers []string
	for _, job := range in.jobs() {
		timers = append(timers, unitBase(app, job)+".timer")
	}
	fmt.Fprintf(w, "#   sudo systemctl daemon-reload && sudo systemctl enable --now %s\n", strings.Join(timers, " "))
	fmt.Fprintln(w, in.hostedNote())
	if n := in.binaryNote(); n != "" {
		fmt.Fprintln(w, n)
	}
	for _, l := range in.secretsNote() {
		fmt.Fprintln(w, l)
	}
	if !in.drill {
		fmt.Fprintln(w, "# "+in.drillNote())
	}
	for _, f := range renderUnits(in, in.jobs()) {
		fmt.Fprintf(w, "\n# ---- %s ----\n%s", f.name, f.content)
	}
}

// cronToOnCalendar turns "M H * * [dow]" into OnCalendar; any other expression
// (ranges, steps, months) returns "".
func cronToOnCalendar(expr string) string {
	f := strings.Fields(expr)
	if len(f) != 5 || f[2] != "*" || f[3] != "*" || !isNum(f[0]) || !isNum(f[1]) {
		return ""
	}
	dow := "*-*-*"
	if f[4] != "*" {
		names, ok := cronDow(f[4])
		if !ok {
			return ""
		}
		dow = names + " *-*-*"
	}
	return fmt.Sprintf("%s %02s:%02s:00", dow, f[1], f[0])
}

func isNum(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// cronDow turns "1,5" or "mon,fri" into "Mon,Fri"; false for anything else.
func cronDow(s string) (string, bool) {
	names := []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	var out []string
	for _, p := range strings.Split(s, ",") {
		switch {
		case isNum(p) && len(p) == 1 && p[0]-'0' <= 7:
			out = append(out, names[(p[0]-'0')%7])
		default:
			found := false
			for _, n := range names {
				if strings.EqualFold(n, p) {
					out = append(out, n)
					found = true
				}
			}
			if !found {
				return "", false
			}
		}
	}
	return strings.Join(out, ","), true
}
