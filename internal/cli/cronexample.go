package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"bracer.dev/bracer/internal/config"
)

// cronInputs is everything the template needs; separated from os for tests.
type cronInputs struct {
	cfg        *config.App
	cfgPath    string // absolute
	binary     string // absolute, or "bracer" if not stable
	dockerDir  string // directory containing docker, "" if not found
	username   string
	backupAt   string
	drillAt    string
	system     bool
	secretsAbs string // absolute secrets_file, "" if none
}

var reCron = regexp.MustCompile(`^\S+(\s+\S+){4}$`)

// runCronExample is `bracer cron-example` (spec backup/cron-example).
func runCronExample(_ context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cron-example", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("c", "app.yaml", "path of app.yaml")
	format := fs.String("format", "cron", "cron | systemd")
	backupAt := fs.String("backup-at", "0 3 * * *", "cron expression for backup")
	drillAt := fs.String("drill-at", "0 4 * * *", "cron expression for drill")
	system := fs.Bool("system", false, "systemd: system units instead of --user units")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	fail := func(format string, a ...interface{}) int {
		fmt.Fprintf(stderr, "bracer cron-example: "+format+"\n", a...)
		return exitUsage
	}
	for _, e := range []string{*backupAt, *drillAt} {
		if !reCron.MatchString(strings.TrimSpace(e)) {
			return fail("%q is not a 5-field cron expression", e)
		}
	}
	if *format != "cron" && *format != "systemd" {
		return fail("--format must be cron or systemd")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fail("%v", err)
	}
	in := cronInputs{cfg: cfg, backupAt: *backupAt, drillAt: *drillAt, system: *system}
	in.cfgPath, _ = filepath.Abs(*cfgPath)
	in.binary = stableBinaryPath()
	if p, err := exec.LookPath("docker"); err == nil {
		in.dockerDir = filepath.Dir(p)
	}
	if u, err := user.Current(); err == nil {
		in.username = u.Username
	}
	if cfg.SecretsFile != "" {
		p, _ := config.ExpandHome(cfg.SecretsFile)
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(in.cfgPath), p)
		}
		in.secretsAbs = p
	}
	if *format == "systemd" {
		renderSystemd(stdout, in)
	} else {
		renderCron(stdout, in)
	}
	return exitOK
}

// stableBinaryPath: path of the running binary, symlinks resolved; a temporary
// path from `go run`/go-build returns a bare "bracer".
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

func (in cronInputs) secretsNote() []string {
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

func (in cronInputs) hostedNote() string {
	if in.cfg.Hosted == nil {
		return "# No hosted: in app.yaml, so runs are not reported anywhere. Add hosted: to get alerts when a run fails or goes missing."
	}
	return "# Results are reported to " + in.cfg.Hosted.Server + " after each run."
}

func (in cronInputs) binaryNote() string {
	if in.binary == "bracer" {
		return "# bracer is running from a temporary path; install it to a fixed path (e.g. /usr/local/bin/bracer) and adjust below."
	}
	return ""
}

func renderCron(w io.Writer, in cronInputs) {
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
	if in.dockerDir != "" {
		fmt.Fprintf(w, "PATH=%s:/usr/local/bin:/usr/bin:/bin\n", in.dockerDir)
	} else {
		fmt.Fprintln(w, "# docker was not found in PATH; set PATH so cron can find it:")
		fmt.Fprintln(w, "PATH=/usr/local/bin:/usr/bin:/bin")
	}
	fmt.Fprintf(w, "%s %s backup -c %s 2>&1 | logger -t bracer\n", in.backupAt, in.binary, in.cfgPath)
	fmt.Fprintf(w, "%s %s drill  -c %s 2>&1 | logger -t bracer\n", in.drillAt, in.binary, in.cfgPath)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "# Watch any other job the same way (create a heartbeat on the dashboard, paste its id):")
	fmt.Fprintf(w, "# 30 2 * * * %s ping <heartbeat-id> -- /path/to/your-job.sh\n", in.binary)
}

func renderSystemd(w io.Writer, in cronInputs) {
	scope, install := "--user ", "systemctl --user daemon-reload && systemctl --user enable --now"
	dir := "~/.config/systemd/user/"
	if in.system {
		scope, install = "", "systemctl daemon-reload && systemctl enable --now"
		dir = "/etc/systemd/system/"
	}
	fmt.Fprintf(w, "# Bracer schedule for app %q as systemd %sunits. Save each block to %s<name>, then:\n", in.cfg.App, scope, dir)
	fmt.Fprintf(w, "#   %s bracer-backup-%s.timer bracer-drill-%s.timer\n", install, in.cfg.App, in.cfg.App)
	fmt.Fprintln(w, in.hostedNote())
	if n := in.binaryNote(); n != "" {
		fmt.Fprintln(w, n)
	}
	for _, l := range in.secretsNote() {
		fmt.Fprintln(w, l)
	}
	for _, job := range []struct{ name, at string }{{"backup", in.backupAt}, {"drill", in.drillAt}} {
		unit := fmt.Sprintf("bracer-%s-%s", job.name, in.cfg.App)
		fmt.Fprintf(w, "\n# ---- %s.service ----\n[Unit]\nDescription=Bracer %s for %s\n\n[Service]\nType=oneshot\n", unit, job.name, in.cfg.App)
		if in.system && in.username != "" {
			fmt.Fprintf(w, "User=%s\n", in.username)
		}
		if in.dockerDir != "" {
			fmt.Fprintf(w, "Environment=PATH=%s:/usr/local/bin:/usr/bin:/bin\n", in.dockerDir)
		}
		fmt.Fprintf(w, "ExecStart=%s %s -c %s\n", in.binary, job.name, in.cfgPath)
		fmt.Fprintf(w, "\n# ---- %s.timer ----\n[Unit]\nDescription=Run %s\n\n[Timer]\nOnCalendar=%s\nPersistent=true\n\n[Install]\nWantedBy=timers.target\n", unit, unit, cronToOnCalendar(job.at))
	}
}

// cronToOnCalendar turns "M H * * *" into OnCalendar; a complex expression is
// kept as is with a note for the user to translate by hand.
func cronToOnCalendar(expr string) string {
	f := strings.Fields(expr)
	if len(f) == 5 && f[2] == "*" && f[3] == "*" && isNum(f[0]) && isNum(f[1]) {
		dow := "*-*-*"
		if f[4] != "*" {
			dow = cronDow(f[4]) + " *-*-*"
		}
		return fmt.Sprintf("%s %02s:%02s:00", dow, f[1], f[0])
	}
	return "*-*-* 03:00:00   # TODO: translate cron expression \"" + expr + "\" by hand"
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

func cronDow(s string) string {
	names := []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if isNum(p) && p[0]-'0' < 7 && len(p) == 1 {
			out = append(out, names[p[0]-'0'])
		} else {
			out = append(out, p)
		}
	}
	return strings.Join(out, ",")
}
