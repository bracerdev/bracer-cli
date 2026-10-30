package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/storage"
)

// runStatus is `bracer status [app]` (spec schedule § status, ADR-0023):
// one block per app of this machine, or the app named. The result of the
// last run comes from the newest run.json in the bucket; systemd gives the
// timer and what ran without reporting. Exit 1 when something failed or
// the timer is off (ADR-0030). No root needed.
func runStatus(ctx context.Context, args []string, d scheduleDeps) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	name, err := appArg(fs, args)
	if err != nil {
		if strings.HasPrefix(err.Error(), "one app at a time") {
			fmt.Fprintf(d.stderr, "bracer status: %v\n", err)
		}
		return exitUsage
	}
	home := d.home()
	apps := []string{name}
	if name == "" {
		apps = config.KnownApps(home)
	}
	if len(apps) == 0 {
		fmt.Fprintf(d.stderr, "bracer status: no app on this machine yet (%s is empty)\nFix: bracer init <name>\n", config.AppsDir(home))
		return exitUsage
	}
	if name != "" {
		if _, err := resolveApp(home, name, "status"); err != nil {
			fmt.Fprintf(d.stderr, "bracer status: %v\n", err)
			return exitUsage
		}
	}
	code := exitOK
	for i, app := range apps {
		if i > 0 {
			fmt.Fprintln(d.stdout)
		}
		code = worse(code, appStatus(ctx, home, app, d))
	}
	return code
}

// worse is the exit code of two results: an error wins.
func worse(a, b int) int {
	if a == exitError || b == exitError {
		return exitError
	}
	return exitOK
}

// appStatus prints the block of one app.
func appStatus(ctx context.Context, home, app string, d scheduleDeps) int {
	path := config.AppPath(home, app)
	fmt.Fprintf(d.stdout, "app %s (%s)\n", app, path)
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(d.stdout, "  ✗ app.yaml cannot be read: %s\n", indentMore(err.Error()))
		return exitError
	}
	s := cfg.Storage
	fmt.Fprintf(d.stdout, "  storage: %s\n", storage.Join(s.Bucket, config.BucketPrefix, s.Folder))
	sc := cfg.Schedule
	tested := "with test-restore"
	if cfg.DrillOff() {
		tested = "not test-restored"
	}
	fmt.Fprintf(d.stdout, "  schedule: %s (%s)\n", describeSchedule(*sc), tested)
	if h := cfg.Hosted; h != nil {
		fmt.Fprintf(d.stdout, "  reports to: %s (every run; the dashboard tells you when one stops coming)\n", strings.TrimRight(h.Server, "/"))
	}
	code := exitOK
	run, err := readNewest(ctx, cfg, d)
	switch {
	case errors.Is(err, backup.ErrNoFolder):
		fmt.Fprintf(d.stdout, "  ✗ the bucket has no folder %s: its backups are gone or the bucket settings point elsewhere\n  Fix: %s\n", s.Folder, backup.FixReinit(app))
		code = exitError
	case errors.Is(err, backup.ErrRunCorrupt):
		fmt.Fprintf(d.stdout, "  ✗ %v\n  Fix: bracer remove %s, then bracer init %s\n", firstLine(err.Error()), app, app)
		code = exitError
	case err != nil:
		fmt.Fprintf(d.stdout, "  bucket unreachable: %s; results below are from systemd\n", firstLine(err.Error()))
	}
	if !d.hasSystemd() {
		fmt.Fprintf(d.stdout, "  ✗ this machine has no systemd: no timer runs\n")
		return exitError
	}
	var buf strings.Builder
	sub := d
	sub.stdout = &buf
	code = worse(code, timerStatus(ctx, app, run, sub))
	writeIndented(d.stdout, buf.String(), "  ")
	return code
}

// readNewest reads the newest run.json of the app's folder: read only,
// 30 s at most. nil with an error when the bucket cannot tell.
func readNewest(ctx context.Context, cfg *config.App, d scheduleDeps) (*backup.Run, error) {
	secrets, _, err := config.LoadSecrets(cfg)
	if err != nil {
		return nil, err
	}
	ak, err := secrets.Require(cfg.Storage.AccessKeyEnv)
	if err != nil {
		return nil, err
	}
	sk, err := secrets.Require(cfg.Storage.SecretKeyEnv)
	if err != nil {
		return nil, err
	}
	s := cfg.Storage
	st, err := d.newStore(storage.S3Config{Endpoint: s.Endpoint, Bucket: s.Bucket, Region: s.Region, AccessKey: ak, SecretKey: sk, PathStyle: s.PathStyle})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	run, _, err := backup.Newest(ctx, st, backup.Root(cfg))
	return run, err
}

// timerStatus prints the app's timer and its last run. run is the newest
// run.json; nil means the bucket was not read, and the result comes from
// systemd and the journal.
func timerStatus(ctx context.Context, app string, run *backup.Run, d scheduleDeps) int {
	installed, err := appUnits(d.unitDir, app)
	if err != nil {
		fmt.Fprintf(d.stdout, "✗ %v\n", err)
		return exitError
	}
	base := unitBase(app)
	if !containsStr(installed, base+".timer") {
		fmt.Fprintf(d.stdout, "no timer\nFix: %s\n", backup.FixReinit(app))
		return exitError
	}
	timer, err := showUnit(ctx, d, base+".timer", "UnitFileState", "ActiveState", "NextElapseUSecRealtime")
	if err != nil {
		fmt.Fprintf(d.stdout, "✗ %v\n", err)
		return exitError
	}
	svc, err := showUnit(ctx, d, base+".service", "ActiveState", "Result", "ExecMainStatus", "ExecMainStartTimestamp", "ExecMainExitTimestamp", "InvocationID")
	if err != nil {
		fmt.Fprintf(d.stdout, "✗ %v\n", err)
		return exitError
	}
	// A timer that is not enabled and active backs nothing up: an error,
	// whatever the last run says (ADR-0030).
	code := exitOK
	if state, active := timer["UnitFileState"], timer["ActiveState"]; state != "enabled" || active != "active" {
		fmt.Fprintf(d.stdout, "%s.timer disabled (%s, %s)\nFix: %s\n", base, state, active, backup.FixReinit(app))
		code = exitError
	} else {
		fmt.Fprintf(d.stdout, "%s.timer enabled, next run %s\n", base, orNever(timer["NextElapseUSecRealtime"]))
	}
	if a := svc["ActiveState"]; a == "activating" || a == "active" {
		fmt.Fprintf(d.stdout, "running since %s\n", orNever(svc["ExecMainStartTimestamp"]))
		return code
	}
	if run == nil {
		return worse(code, systemdResult(ctx, d, d.stdout, "", svc))
	}
	code = worse(code, printRun(d.stdout, run))
	// A run systemd saw after the newest run.json did not report.
	if start, ok := systemdTime(svc["ExecMainStartTimestamp"]); ok {
		if at, err := time.Parse(backupTSLayout, run.TS); err == nil && start.After(at) {
			exit := svc["ExecMainStatus"]
			fmt.Fprintf(d.stdout, "the run at %s did not report to the bucket (exit %s); see journalctl -t bracer\n", start.Format("2006-01-02 15:04"), exit)
			code = worse(code, exitError)
		}
	}
	return code
}

// backupTSLayout is the ts of a run folder (spec backup).
const backupTSLayout = "2006-01-02T150405Z"

// printRun prints the newest run: its verdict (ADR-0027), the reasons when
// it failed, the drill of each instance that failed, and the Fix; the
// warnings of the run under either (spec schedule § status, ADR-0029).
func printRun(w io.Writer, run *backup.Run) int {
	label, code := "ok", exitOK
	if run.Verdict() == backup.ResultFailed {
		label, code = "failed", exitError
	}
	at := run.TS
	if t, err := time.Parse(backupTSLayout, run.TS); err == nil {
		at = t.Local().Format("2006-01-02 15:04")
	}
	fmt.Fprintf(w, "last run %s: %s\n", at, label)
	if code != exitOK {
		for _, r := range run.Backup.Reasons {
			fmt.Fprintf(w, "  %s\n", r)
		}
		for _, name := range run.InstanceNames() {
			if dr := run.Instances[name].Drill; dr != nil && dr.Result == backup.DrillFailed {
				fmt.Fprintf(w, "  %s: test-restore failed: %s\n", name, strings.Join(dr.Reasons, "; "))
			}
		}
		if run.Backup.Fix != "" {
			fmt.Fprintf(w, "Fix: %s\n", run.Backup.Fix)
		}
	}
	for _, msg := range run.Backup.Warnings {
		fmt.Fprintf(w, "  warning: %s\n", msg)
	}
	return code
}

// systemdResult prints the last run of the service as systemd has it: exit 0
// ok, anything else failed, with the last error and Fix line of its journal.
func systemdResult(ctx context.Context, d scheduleDeps, w io.Writer, pad string, svc map[string]string) int {
	if isUnset(svc["ExecMainExitTimestamp"]) {
		fmt.Fprintf(w, "%slast run: never\n", pad)
		return exitOK
	}
	label, code := "ok", exitOK
	if svc["Result"] != "success" {
		label, code = fmt.Sprintf("failed (exit %s)", svc["ExecMainStatus"]), exitError
	}
	fmt.Fprintf(w, "%slast run %s: %s\n", pad, svc["ExecMainExitTimestamp"], label)
	if code != exitOK {
		for _, l := range lastFailure(ctx, d, svc["InvocationID"]) {
			fmt.Fprintf(w, "%s  %s\n", pad, l)
		}
	}
	return code
}

// systemdTime reads a timestamp of `systemctl show` ("Tue 2026-10-21
// 03:00:05 +07") in this machine's time zone, which systemd prints in.
func systemdTime(v string) (time.Time, bool) {
	f := strings.Fields(v)
	if isUnset(v) || len(f) < 3 {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", f[1]+" "+f[2], time.Local)
	return t, err == nil
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

// writeIndented writes s with every line prefixed.
func writeIndented(w io.Writer, s, prefix string) {
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		fmt.Fprintln(w, prefix+l)
	}
}
