package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/storage"
)

// scheduleDeps is what the unit install, remove and status need from the
// machine, injected so tests run with a temporary unit directory, a fake
// systemctl and a fake sudo.
type scheduleDeps struct {
	unitDir    string                                                    // /etc/systemd/system
	systemctl  func(ctx context.Context, args ...string) (string, error) // runs systemctl read-only, returns stdout
	hasSystemd func() bool
	euid       int
	executable func() string        // absolute path of this binary
	invoker    func() (name string) // $SUDO_USER, else the current user
	lookupUser func(name string) (account, error)
	// journal runs journalctl and returns stdout (status: the last error and Fix line).
	journal func(ctx context.Context, args ...string) (string, error)
	// newStore opens the bucket (status reads the newest run.json).
	newStore func(storage.S3Config) (storage.Store, error)
	// root runs a command that writes the system (install, rm, systemctl
	// daemon-reload|enable|disable|stop|reset-failed) on the terminal: through
	// sudo unless already root (ADR-0027). Bracer never runs itself as root.
	root   func(ctx context.Context, name string, args ...string) error
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	// tty: stdin and stdout are a terminal (remove asks).
	tty bool
}

func defaultScheduleDeps(stdin io.Reader, stdout, stderr io.Writer) scheduleDeps {
	euid := os.Geteuid()
	return scheduleDeps{
		unitDir:    "/etc/systemd/system",
		systemctl:  execSystemctl,
		hasSystemd: hasSystemd,
		euid:       euid,
		executable: binaryPath,
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
		journal: func(ctx context.Context, args ...string) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, "journalctl", args...).Output()
			return string(out), err
		},
		newStore: func(c storage.S3Config) (storage.Store, error) { return storage.NewS3(c) },
		root:     rootRunner(execTTY, euid),
		stdin:    stdin,
		stdout:   stdout,
		stderr:   stderr,
		tty:      isTerminal(stdin) && isTerminal(stdout),
	}
}

// rootRunner runs commands as root on the terminal: directly when already
// root, else through sudo (which asks its own password there).
func rootRunner(runTTY func(context.Context, string, ...string) error, euid int) func(context.Context, string, ...string) error {
	return func(ctx context.Context, name string, args ...string) error {
		if euid == 0 {
			return runTTY(ctx, name, args...)
		}
		return runTTY(ctx, "sudo", append([]string{name}, args...)...)
	}
}

// home is where the apps are (ADR-0021): under sudo, the home of the user
// who ran sudo, not /root.
func (d scheduleDeps) home() string {
	if d.euid == 0 {
		if u := d.invoker(); u != "" && u != "root" {
			if a, err := d.lookupUser(u); err == nil && a.home != "" {
				return a.home
			}
		}
	}
	return userHome()
}

// account is the user the jobs run as: what User= means to systemd.
type account struct {
	home string
}

func lookupAccount(name string) (account, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return account{}, err
	}
	return account{home: u.HomeDir}, nil
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

// binaryPath is the running binary, symlinks resolved: what the units run.
func binaryPath() string {
	p, err := os.Executable()
	if err != nil {
		return "bracer"
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p
}

// onCalendar is the OnCalendar= lines of a schedule (ADR-0029), in the
// machine's time zone: one line per time of day for the days of the
// rhythm; `last` is the last day of the month, *-*~01, on a line of its own.
func onCalendar(sc config.Schedule) []string {
	days := []string{"*-*-*"}
	switch sc.Every {
	case config.EveryWeekly:
		var names []string
		for _, d := range sc.Days {
			names = append(names, strings.ToUpper(d[:1])+d[1:])
		}
		days = []string{strings.Join(names, ",") + " *-*-*"}
	case config.EveryMonthly:
		days = nil
		var nums []string
		last := false
		for _, d := range sc.Days {
			if d == config.LastDay {
				last = true
				continue
			}
			n, _ := strconv.Atoi(d)
			nums = append(nums, fmt.Sprintf("%02d", n))
		}
		if len(nums) > 0 {
			days = append(days, "*-*-"+strings.Join(nums, ","))
		}
		if last {
			days = append(days, "*-*~01")
		}
	}
	var out []string
	for _, d := range days {
		for _, t := range sc.At {
			out = append(out, d+" "+t+":00")
		}
	}
	return out
}

// scheduleInputs is everything the unit templates need.
type scheduleInputs struct {
	cfg      *config.App
	cfgPath  string   // absolute
	binary   string   // absolute
	username string   // runtime.user
	home     string   // its home
	at       []string // OnCalendar lines
}

// unitInputs gathers what the units of the app need from its app.yaml and
// the machine; every problem is reported before anything is written.
func unitInputs(cfg *config.App, cfgPath string, d scheduleDeps) (scheduleInputs, error) {
	in := scheduleInputs{cfg: cfg, cfgPath: cfgPath, binary: d.executable()}
	app := config.AppOf(cfgPath)
	in.at = onCalendar(*cfg.Schedule)
	in.username = cfg.Runtime.User
	a, err := d.lookupUser(in.username)
	if err != nil {
		return in, fmt.Errorf("runtime.user %q: %v\nFix: %s", in.username, err, backup.FixReinit(app))
	}
	in.home = a.home
	// The job finds its app by name in its own home (ADR-0021).
	if config.AppPath(in.home, app) != cfgPath {
		return in, fmt.Errorf("the jobs would run as %s, whose home has no app %s\nFix: bracer init %s", in.username, app, app)
	}
	return in, nil
}

// unitBase is the name of the app's unit pair: one timer runs `bracer
// backup <app>`, the drill inside it (ADR-0026).
func unitBase(app string) string { return "bracer-" + app }

// unitFile is one rendered unit: its file name and content.
type unitFile struct {
	name, content string
}

// renderUnits renders the service and its timer.
func renderUnits(in scheduleInputs) []unitFile {
	base := unitBase(in.cfg.App)
	var s strings.Builder
	fmt.Fprintf(&s, "[Unit]\nDescription=Bracer backup for %s\n", in.cfg.App)
	s.WriteString("# Written by `bracer init`; `bracer remove` deletes it.\n\n")
	s.WriteString("[Service]\nType=oneshot\n")
	fmt.Fprintf(&s, "User=%s\n", in.username)
	fmt.Fprintf(&s, "WorkingDirectory=%s\n", filepath.Dir(in.cfgPath))
	fmt.Fprintf(&s, "Environment=HOME=%s\n", in.home)
	fmt.Fprintf(&s, "Environment=PATH=%s\n", in.cfg.Runtime.Path)
	s.WriteString("SyslogIdentifier=bracer\n")
	fmt.Fprintf(&s, "ExecStart=%s backup %s\n", in.binary, config.AppOf(in.cfgPath))
	var t strings.Builder
	fmt.Fprintf(&t, "[Unit]\nDescription=Run %s.service\n\n[Timer]\n", base)
	for _, at := range in.at {
		fmt.Fprintf(&t, "OnCalendar=%s\n", at)
	}
	fmt.Fprintf(&t, "Persistent=true\nRandomizedDelaySec=%dm\n\n[Install]\nWantedBy=timers.target\n", int(timerDelay.Minutes()))
	return []unitFile{
		{base + ".service", s.String()},
		{base + ".timer", t.String()},
	}
}

// installUnits is the last step of init (spec schedule § Cài
// unit, ADR-0027): the unit pair goes into the unit directory and the timer
// is enabled, every write through d.root. No flag, no question, no dry run:
// init has just proven the app.
func installUnits(ctx context.Context, cfg *config.App, cfgPath string, d scheduleDeps) error {
	in, err := unitInputs(cfg, cfgPath, d)
	if err != nil {
		return err
	}
	files := renderUnits(in)
	// Write the new pair, then retire units of this app that are not in it
	// (the backup and drill pairs of an earlier layout, ADR-0026).
	before, err := appUnits(d.unitDir, cfg.App)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "bracer-units-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	want := map[string]bool{}
	for _, f := range files {
		want[f.name] = true
		src := filepath.Join(tmp, f.name)
		if err := os.WriteFile(src, []byte(f.content), 0o644); err != nil {
			return err
		}
		if err := d.root(ctx, "install", "-m", "644", src, filepath.Join(d.unitDir, f.name)); err != nil {
			return fmt.Errorf("install %s: %w", f.name, err)
		}
	}
	var retired []string
	for _, name := range before {
		if want[name] {
			continue
		}
		if strings.HasSuffix(name, ".timer") {
			if err := d.root(ctx, "systemctl", "disable", "--now", name); err != nil {
				fmt.Fprintf(d.stderr, "warning: %v\n", err)
			}
		} else {
			retired = append(retired, name)
		}
		if err := d.root(ctx, "rm", "-f", filepath.Join(d.unitDir, name)); err != nil {
			return err
		}
	}
	if err := d.root(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	// A first install starts from a clean slate: systemd keeps the result of
	// a service until reset-failed, even after its unit files were deleted.
	base := unitBase(cfg.App)
	if len(before) == 0 {
		retired = append(retired, base+".service")
	}
	resetFailed(ctx, d, retired)
	if err := d.root(ctx, "systemctl", "enable", "--now", base+".timer"); err != nil {
		return err
	}
	fmt.Fprintf(d.stdout, "Installed %d units in %s.\n", len(files), d.unitDir)
	if cfg.DrillOff() {
		fmt.Fprintf(d.stdout, "drill is off for every instance of %s: backups are not test-restored.\n", cfg.App)
	}
	// Only the timer: the result lives in the bucket, and bracer status reads it.
	timer, err := showUnit(ctx, d, base+".timer", "UnitFileState", "NextElapseUSecRealtime")
	if err != nil {
		fmt.Fprintf(d.stdout, "  ✗ %v\n", err)
		return nil
	}
	fmt.Fprintf(d.stdout, "  %s.timer %s, next run %s\n", base, timer["UnitFileState"], orNever(timer["NextElapseUSecRealtime"]))
	return nil
}

// resetFailed clears what systemd remembers of the services that are in
// the failed state (only those: reset-failed on a unit that is not in
// memory prints "not loaded", and a unit that ended well has nothing to
// clear); an error only warns.
func resetFailed(ctx context.Context, d scheduleDeps, services []string) {
	var failed []string
	for _, name := range services {
		if props, err := showUnit(ctx, d, name, "ActiveState"); err == nil && props["ActiveState"] == "failed" {
			failed = append(failed, name)
		}
	}
	if len(failed) == 0 {
		return
	}
	if err := d.root(ctx, "systemctl", append([]string{"reset-failed"}, failed...)...); err != nil {
		fmt.Fprintf(d.stderr, "warning: %v\n", err)
	}
}

// appUnits lists the unit files of one app in dir: bracer-<app>.service and
// .timer, and bracer-<app>-*.service/.timer of the earlier layout, so no
// other unit is ever touched.
func appUnits(dir, app string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	base := unitBase(app)
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !e.Type().IsRegular() {
			continue
		}
		stem, ok := strings.CutSuffix(n, ".service")
		if !ok {
			stem, ok = strings.CutSuffix(n, ".timer")
		}
		if ok && (stem == base || strings.HasPrefix(stem, base+"-")) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}

// uninstallUnits is the unit step of remove (spec schedule § Gỡ unit). It
// reads no app.yaml: the units are named after the app, so a broken
// app.yaml can still be removed (ADR-0023).
func uninstallUnits(ctx context.Context, app string, d scheduleDeps) error {
	names, err := appUnits(d.unitDir, app)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Fprintf(d.stdout, "No schedule installed for app %s.\n", app)
		return nil
	}
	// Timers first so nothing starts a service while it is being removed.
	for _, name := range names {
		if strings.HasSuffix(name, ".timer") {
			if err := d.root(ctx, "systemctl", "disable", "--now", name); err != nil {
				return err
			}
		}
	}
	var services []string
	for _, name := range names {
		if strings.HasSuffix(name, ".service") {
			services = append(services, name)
			if err := d.root(ctx, "systemctl", "stop", name); err != nil {
				return err
			}
		}
	}
	for _, name := range names {
		if err := d.root(ctx, "rm", "-f", filepath.Join(d.unitDir, name)); err != nil {
			return err
		}
		fmt.Fprintf(d.stdout, "Removed %s\n", filepath.Join(d.unitDir, name))
	}
	if err := d.root(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	resetFailed(ctx, d, services)
	return nil
}

// timerDelay is RandomizedDelaySec of every timer.
const timerDelay = 5 * time.Minute
