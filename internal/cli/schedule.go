package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
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
	executable func() string // absolute path of this binary
	// home is where the apps are: appHome.
	home       func() (string, error)
	lookupUser func(name string) (account, error)
	// stampDir holds systemd's stamps of Persistent=true system timers.
	stampDir string
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
		executable: binaryPath,
		home:       appHome,
		lookupUser: lookupAccount,
		stampDir:   "/var/lib/systemd/timers",
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

// appHome is where the apps are (ADR-0021), for every command: the home of
// the user the command runs as (root under sudo), because that is the user
// the installed unit runs the job as (User=runtime.user, the user who ran
// init, with Environment=HOME=<its home>). A $HOME that names another
// directory (sudo -E, a kept or hand-set HOME) is refused: the command would
// use one home and the nightly job another.
func appHome() (string, error) {
	name, home, err := currentAccount()
	if err != nil {
		return "", fmt.Errorf("the user this runs as (uid %d) has no entry in the user database (/etc/passwd): its home cannot be told\nFix: run bracer as a user that has an /etc/passwd entry", os.Getuid())
	}
	return checkHome(name, home, os.Getenv("HOME"))
}

// processEUID is os.Geteuid; a variable so tests can run "as root".
var processEUID = os.Geteuid

// rootOnly is said wherever a command run as root finds no app: it sees
// only root's apps (appHome), so an app made by `bracer init` without sudo
// is not there, and init-ing it again as root would make a second app.
const rootOnly = "as root, bracer sees only root's apps"

// fixNoApp is the Fix line of a command that finds no app named name ("":
// no app at all) in appHome. As root, the app most likely belongs to the
// user who ran init without sudo; bracer does not look in other homes.
func fixNoApp(cmd, name string) string {
	if processEUID() != 0 || cmd == "init" {
		if name == "" {
			name = "<name>"
		}
		return "Fix: bracer init " + name
	}
	if name != "" {
		cmd += " " + name
	}
	return fmt.Sprintf("Fix: run bracer %s without sudo, as the user who created the app (%s)", cmd, rootOnly)
}

// currentAccount is the user this process runs as and its home in the user
// database; a variable so tests can stand in for it.
var currentAccount = func() (name, home string, err error) {
	u, err := user.Current()
	if err != nil {
		return "", "", err
	}
	return u.Username, u.HomeDir, nil
}

// checkHome is the rule of appHome: home is the user's home in the user
// database; env, $HOME, must be empty or the same directory.
func checkHome(name, home, env string) (string, error) {
	if home == "" {
		return "", fmt.Errorf("user %s has no home directory", name)
	}
	if env == "" || filepath.Clean(env) == filepath.Clean(home) {
		return home, nil
	}
	if a, err := os.Stat(env); err == nil {
		if b, err := os.Stat(home); err == nil && os.SameFile(a, b) {
			return home, nil
		}
	}
	return "", fmt.Errorf("bracer uses the home of the user it runs as (%s, %s), but $HOME points elsewhere (%s)\nFix: run it as the user whose home is %s, without sudo -E, or unset the HOME override", name, home, env, env)
}

// account is the user the jobs run as: what User= means to systemd.
type account struct {
	home string
}

// lookupAccount finds a user in the user database (the unit's User=); a
// variable so tests can prove which names are looked up.
var lookupAccount = func(name string) (account, error) {
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
	// The job finds its app by name in this home (ADR-0021): the home every
	// command uses (appHome), so init wrote the app there.
	in.home = a.home
	return in, nil
}

// unitBase is the name of the app's unit pair: one timer runs `bracer
// backup <app>`, the drill inside it (ADR-0026).
func unitBase(app string) string { return "bracer-" + app }

// unitFile is one rendered unit: its file name and content.
type unitFile struct {
	name, content string
}

// renderUnits renders the service and its timer. Every value taken from the
// machine (user, paths) goes through unitValue or unitWord, so systemd reads
// it as it is.
func renderUnits(in scheduleInputs) []unitFile {
	base := unitBase(in.cfg.App)
	var s strings.Builder
	fmt.Fprintf(&s, "[Unit]\nDescription=Bracer backup for %s\n", in.cfg.App)
	s.WriteString("# Written by `bracer init`; `bracer remove` deletes it.\n")
	// A run made up at boot (Persistent=true) waits for the network (the
	// bucket) and for the system Docker daemon when it is starting; Docker
	// is only ordered after, never started: a machine on rootless Docker
	// keeps its system daemon off.
	s.WriteString("Wants=network-online.target\nAfter=network-online.target docker.service\n\n")
	s.WriteString("[Service]\nType=oneshot\n")
	fmt.Fprintf(&s, "User=%s\n", unitValue(in.username))
	fmt.Fprintf(&s, "WorkingDirectory=%s\n", unitValue(filepath.Dir(in.cfgPath)))
	fmt.Fprintf(&s, "Environment=%s\n", unitWord("HOME="+in.home))
	fmt.Fprintf(&s, "Environment=%s\n", unitWord("PATH="+in.cfg.Runtime.Path))
	s.WriteString("SyslogIdentifier=bracer\n")
	// systemd expands $ in the arguments only, never in the path it
	// executes, so the binary's $ stays as it is.
	fmt.Fprintf(&s, "ExecStart=%s backup %s\n", unitWord(in.binary), config.AppOf(in.cfgPath))
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

// unitValue is s for a unit line systemd reads whole (User=,
// WorkingDirectory=): % starts a specifier on every line, %% is a literal %.
func unitValue(s string) string { return strings.ReplaceAll(s, "%", "%%") }

// unitWord is s as one word of a unit line systemd splits at whitespace and
// unquotes (Environment=, ExecStart=): as unitValue, and in double quotes
// with \ and " escaped when it holds whitespace, a quote or a backslash.
func unitWord(s string) string {
	v := unitValue(s)
	if !strings.ContainsAny(v, " \t\n\"'\\") {
		return v
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`).Replace(v) + `"`
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
	// No unit of the app yet means a first install, cleaned below.
	before, err := appUnits(d.unitDir, cfg.App)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "bracer-units-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for _, f := range files {
		src := filepath.Join(tmp, f.name)
		if err := os.WriteFile(src, []byte(f.content), 0o644); err != nil {
			return err
		}
		if err := d.root(ctx, "install", "-m", "644", src, filepath.Join(d.unitDir, f.name)); err != nil {
			return fmt.Errorf("install %s: %w", f.name, err)
		}
	}
	if err := d.root(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	// A first install starts from a clean slate: systemd keeps the result of
	// a service until reset-failed, even after its unit files were deleted.
	base := unitBase(cfg.App)
	if len(before) == 0 {
		resetFailed(ctx, d, []string{base + ".service"})
	}
	// init has just run the app: a stamp left by an earlier timer of this
	// name (a remove before this fix, a crash) would have the new timer make
	// up that "missed" run minutes from now.
	if err := d.root(ctx, "rm", "-f", d.stampFile(base+".timer")); err != nil {
		return err
	}
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

// stampFile is where systemd keeps the last run of a Persistent=true system
// timer (/var/lib/systemd/timers/stamp-<timer>). It outlives the unit file:
// a timer installed again under the same name reads it and makes up the
// runs it believes were missed.
func (d scheduleDeps) stampFile(timer string) string {
	return filepath.Join(d.stampDir, "stamp-"+timer)
}

// removeUnitFile deletes one unit file of the app and, for a timer, its
// stamp: both are bracer's own, named after the app.
func removeUnitFile(ctx context.Context, d scheduleDeps, name string) error {
	if err := d.root(ctx, "rm", "-f", filepath.Join(d.unitDir, name)); err != nil {
		return err
	}
	if strings.HasSuffix(name, ".timer") {
		return d.root(ctx, "rm", "-f", d.stampFile(name))
	}
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

// appUnits lists the unit files of one app in dir that exist as regular
// files: bracer-<app>.service and bracer-<app>.timer.
func appUnits(dir, app string) ([]string, error) {
	var names []string
	for _, n := range []string{unitBase(app) + ".service", unitBase(app) + ".timer"} {
		fi, err := os.Lstat(filepath.Join(dir, n))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if fi.Mode().IsRegular() {
			names = append(names, n)
		}
	}
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
		if err := removeUnitFile(ctx, d, name); err != nil {
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
