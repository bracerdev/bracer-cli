package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/crypto"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/storage"
)

// initDeps is everything init touches outside the process, so tests
// can run the whole flow without Docker or network.
type initDeps struct {
	shell    pg.Shell
	probe    pg.Prober
	newStore func(storage.S3Config) (storage.Store, error)
	env      envDeps // step 0; its shell is replaced by shell
	home     func() (string, error)
	// runSelf runs this binary with args and exactly env (steps 15, 16); returns the exit code.
	runSelf func(ctx context.Context, args, env []string, stdout, stderr io.Writer) int
	// runTTY runs a command attached to the terminal (sudo true).
	runTTY func(ctx context.Context, name string, args ...string) error
	// checkHeartbeat sends GET <url>/log (step 13b).
	checkHeartbeat func(ctx context.Context, url string) error
	// installTimer writes the app's units and enables its timer (step 17,
	// ADR-0027); the root work goes through sudo.
	installTimer func(ctx context.Context, cfg *config.App, cfgPath string) error
	euid         int
	// tty: stdin and stdout are a terminal.
	tty    bool
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

// initRun holds the state of one init run.
type initRun struct {
	initDeps
	ctx     context.Context
	p       *prompter
	tools   *pg.Tools
	docker  pg.Docker
	cfg     *config.App
	runtime *config.Runtime // from step 0
	path    string          // ~/.config/bracer/<app>/app.yaml (ADR-0021)
	appName string
	again   string            // the command line typed, printed last when it stops for a fix
	secrets map[string]string // variable name → collected value, not written anywhere yet
}

func defaultInitDeps(stdin io.Reader, stdout, stderr io.Writer) initDeps {
	return initDeps{
		shell:          pg.ExecShell{},
		probe:          pg.ProbeSSLRequest,
		newStore:       func(c storage.S3Config) (storage.Store, error) { return storage.NewS3(c) },
		env:            defaultEnvDeps(pg.ExecShell{}),
		home:           os.UserHomeDir,
		runSelf:        execSelf,
		runTTY:         execTTY,
		checkHeartbeat: checkHeartbeatURL,
		installTimer: func(ctx context.Context, cfg *config.App, cfgPath string) error {
			return installUnits(ctx, cfg, cfgPath, defaultScheduleDeps(stdin, stdout, stderr))
		},
		euid:   os.Geteuid(),
		tty:    isTerminal(stdin) && isTerminal(stdout),
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
	}
}

// runInit is `bracer init <app>`: a new app, with a new folder in the bucket
// (spec backup/init, ADR-0023). There is no edit: an app is changed by
// removing it and running init again (ADR-0030).
func runInit(ctx context.Context, args []string, d initDeps) int {
	const cmd = "init"
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	verbose := fs.Bool("verbose", false, "print every docker and pg_* command")
	fail := func(format string, a ...interface{}) int {
		fmt.Fprintf(d.stderr, "bracer "+cmd+": "+format+"\n", a...)
		return exitUsage
	}
	name, err := appArg(fs, args)
	if err != nil {
		if strings.HasPrefix(err.Error(), "one app at a time") {
			return fail("%v", err)
		}
		return exitUsage
	}
	home, err := d.home()
	if err != nil {
		return fail("home directory: %v", err)
	}
	switch {
	case name == "":
		return fail("%v", noName(config.KnownApps(home), cmd))
	case !config.ValidAppName(name):
		return fail("%q is not an app name: lowercase letters, digits, `-`, `_` only", name)
	}
	path := config.AppPath(home, name)
	if _, err := os.Stat(path); err == nil {
		return fail("app %s exists on this machine\nFix: %s", name, backup.FixReinit(name))
	}
	if sh, ok := d.shell.(pg.ExecShell); ok {
		sh.Verbose = *verbose
		d.shell = sh
	}
	d.env.shell = d.shell
	r := &initRun{initDeps: d, ctx: ctx, p: newPrompter(d.stdin, d.stdout), appName: name, path: path,
		again: commandLine(cmd, args), docker: pg.Docker{Shell: d.shell}, secrets: map[string]string{}}
	r.tools = pg.NewTools(d.shell)
	err = r.run()
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errEnvBlocked), errors.Is(err, errSelfCheck):
		return exitError
	case errors.Is(err, errQuit):
		fmt.Fprintf(d.stdout, "\n%s Run again when ready:\n  %s\n", r.unchanged(), r.again)
		return exitError
	case errors.Is(err, ErrNoInput), errors.As(err, new(*usageError)):
		fmt.Fprintf(d.stderr, "bracer %s: %v\n", cmd, err)
		return exitUsage
	case errors.Is(err, context.Canceled):
		fmt.Fprintf(d.stderr, "\nbracer %s: interrupted. %s\n", cmd, r.unchanged())
		return exitError
	default:
		fmt.Fprintf(d.stderr, "bracer %s: %v\n", cmd, err)
		return exitError
	}
}

// usageError is a usage error (exit 2): broken app.yaml…
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

func usageErrorf(format string, args ...interface{}) error {
	return &usageError{err: fmt.Errorf(format, args...)}
}

var (
	// errQuit: the user chose [q]; what to run is already printed.
	errQuit = errors.New("quit")
	// errSelfCheck: a proof or the install failed; the child's own error is already printed.
	errSelfCheck = errors.New("self-check failed")
)

// unchanged says what init leaves behind when it stops early.
func (r *initRun) unchanged() string { return "No app.yaml written." }

func (r *initRun) warn(format string, args ...interface{}) {
	fmt.Fprintf(r.stderr, "warning: "+format+"\n", args...)
}

func (r *initRun) run() error {
	if err := r.checkMachine(); err != nil {
		return err
	}
	return r.runInit()
}

// checkMachine is step 0, before any question (spec init § Flow 0).
func (r *initRun) checkMachine() error {
	r.p.printf("Checking this machine:\n")
	rep := checkEnv(r.ctx, r.env)
	printEnv(r.stdout, rep, r.again)
	if rep.Blocked() {
		r.p.printf("\nNothing written.\n")
		return errEnvBlocked
	}
	r.runtime = rep.Runtime
	if r.euid == 0 {
		r.p.printf("Running as root: the app's files go to root's home and the jobs will run as root.\n")
		return nil
	}
	// The timers are installed with sudo at the end: ask for it now, while
	// nothing is written yet (ADR-0022). Not sudo -v: it asks for a password
	// even when a NOPASSWD rule lets the user run commands.
	if err := r.runTTY(r.ctx, "sudo", "true"); err != nil {
		r.p.printf("  ✗ sudo: init installs the nightly timer with sudo, and sudo failed: %v\n  Fix: let %s use sudo (or run bracer as root), then run: %s\n\nNothing written.\n", err, r.runtime.User, r.again)
		return errEnvBlocked
	}
	return nil
}

// runInit is steps 2–19 for a new app.
func (r *initRun) runInit() error {
	r.cfg = &config.App{App: r.appName, Runtime: r.runtime}
	r.p.printf("\nNew app %s: its files go to %s.\n", r.appName, filepath.Dir(r.path))
	// Bucket and encryption first: a wrong bucket stops init before any
	// discovery (ADR-0022).
	if err := r.setupStorage(); err != nil {
		return err
	}
	if err := r.setupEncryption(); err != nil {
		return err
	}
	inst, err := r.pickInstance()
	if err != nil {
		return err
	}
	if inst == nil {
		return fmt.Errorf("no instance to back up; nothing was written. When it is fixed, run: %s", r.again)
	}
	r.cfg.Instances = append(r.cfg.Instances, *inst)
	if err := r.askDrill(); err != nil {
		return err
	}
	if err := r.askSchedule(); err != nil {
		return err
	}
	if err := r.askHeartbeat(); err != nil {
		return err
	}
	return r.finish()
}

// finish is steps 11 and 14–19: the secrets, the proof (preflight, a real
// backup with its drill), the timer, the rescue card.
func (r *initRun) finish() error {
	if err := r.storeSecrets(); err != nil {
		return err
	}
	if err := r.prove(); err != nil {
		return err
	}
	if err := r.install(); err != nil {
		return err
	}
	if err := r.rescueCard(); err != nil {
		return err
	}
	r.p.printf("\nResults of the nightly jobs:\n  bracer status %s\n  journalctl -t bracer\n", r.appName)
	r.printHeartbeat()
	return nil
}

// setupStorage is step 2: the bucket, checked, and a new folder in it
// (ADR-0023). Init reads no other folder of bracer/ (ADR-0027).
func (r *initRun) setupStorage() error {
	r.p.printf("\nBackups go to an S3-compatible bucket you own (R2, B2, Wasabi, MinIO...).\n")
	for {
		endpoint, err := r.p.askRequired("S3 endpoint URL (e.g. https://<account>.r2.cloudflarestorage.com)")
		if err != nil {
			return err
		}
		bucket, err := r.p.askRequired("Bucket")
		if err != nil {
			return err
		}
		region, err := r.p.ask("Region", "auto")
		if err != nil {
			return err
		}
		pathStyle, err := r.p.yesNo("Path-style URLs (needed for MinIO/Garage)?", false)
		if err != nil {
			return err
		}
		akEnv := config.EnvVarName(r.cfg.App, "s3", "access_key")
		skEnv := config.EnvVarName(r.cfg.App, "s3", "secret_key")
		ak, err := r.p.askSecret("Access key")
		if err != nil {
			return err
		}
		sk, err := r.p.askSecret("Secret key")
		if err != nil {
			return err
		}
		st, err := r.newStore(storage.S3Config{Endpoint: endpoint, Bucket: bucket, Region: region, AccessKey: ak, SecretKey: sk, PathStyle: pathStyle})
		if err == nil {
			err = st.Check(r.ctx)
		}
		var folder string
		if err == nil {
			folder, err = newFolder(r.ctx, st, r.cfg.App)
		}
		if err == nil {
			r.p.printf("  ✓ bucket reachable; this app's backups go to %s/\n", storage.Join(bucket, config.BucketPrefix, folder))
			r.cfg.Storage = &config.Storage{Endpoint: endpoint, Bucket: bucket, Folder: folder, Region: region, AccessKeyEnv: akEnv, SecretKeyEnv: skEnv, PathStyle: pathStyle}
			r.secrets[akEnv] = ak
			r.secrets[skEnv] = sk
			return nil
		}
		r.p.printf("  ✗ %v\n", err)
		// No skip: without a bucket backup has nowhere to go.
		_, key, err := r.p.choose("[r] fix and retry  [q] quit", 0, "r", "q")
		if err != nil {
			return err
		}
		if key == "q" {
			return errQuit
		}
	}
}

// newFolder is <app>-<6 random [a-z0-9]>, not yet used under bracer/.
func newFolder(ctx context.Context, st storage.Store, app string) (string, error) {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	for attempt := 0; attempt < 10; attempt++ {
		b := make([]byte, 6)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		for i := range b {
			b[i] = chars[int(b[i])%len(chars)]
		}
		name := app + "-" + string(b)
		objs, err := st.List(ctx, backup.FolderRoot(name))
		if err != nil {
			return "", err
		}
		if len(objs) == 0 {
			return name, nil
		}
	}
	return "", errors.New("could not find an unused folder name in the bucket")
}

// setupEncryption is step 3. A key left in the app directory by remove is
// reused, so the old folder stays readable from here (ADR-0030).
func (r *initRun) setupEncryption() error {
	yes, err := r.p.yesNo("\nEncrypt backups with age? Protects the data from whoever can read the bucket (recommended)", true)
	if err != nil {
		return err
	}
	if !yes {
		r.cfg.Encryption = "none"
		r.p.printf("Backups will be stored unencrypted in the bucket.\n")
		return nil
	}
	// Whatever key this app ends up with, nobody has confirmed it is off
	// this machine yet, a key left by an init that stopped included (step 18).
	if !r.tty {
		return usageErrorf("init must confirm the age key was copied; run it in a terminal")
	}
	pub, err := r.p.ask("Paste an age public key (age1...), or press Enter to generate a new keypair", "")
	if err != nil {
		return err
	}
	// The private key is always age.key in the app directory (ADR-0021).
	path, err := r.configPath(config.KeyFile)
	if err != nil {
		return err
	}
	if pub == "" {
		if _, recips, rerr := crypto.ReadIdentities(path); rerr == nil && len(recips) > 0 {
			r.cfg.Encryption, r.cfg.AgeRecipient, r.cfg.AgeIdentityFile = "age", recips[len(recips)-1], path
			r.p.printf("Using the key already in %s (public key %s).\n", path, recips[len(recips)-1])
			return nil
		} else if _, serr := os.Stat(path); serr == nil {
			return fmt.Errorf("%s exists but is not an age identity file; move it away and run %s again", path, r.again)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		kp, err := crypto.Generate()
		if err != nil {
			return err
		}
		if err := crypto.WriteIdentityFile(path, kp); err != nil {
			return err
		}
		r.cfg.Encryption, r.cfg.AgeRecipient, r.cfg.AgeIdentityFile = "age", kp.Recipient, path
		r.p.printf("Private key written to %s (mode 600); it is never printed.\n", path)
		return nil
	}
	recip, err := crypto.ParseRecipient(pub)
	if err != nil {
		return err
	}
	// The key file must open what the recipient encrypts, or drill could
	// never open a backup here (ADR-0019, ADR-0022).
	for {
		_, recips, rerr := crypto.ReadIdentities(path)
		if rerr == nil && containsStr(recips, recip) {
			r.cfg.Encryption, r.cfg.AgeRecipient, r.cfg.AgeIdentityFile = "age", recip, path
			return nil
		}
		if rerr != nil {
			r.p.printf("  ✗ %s is not readable: %v\n", path, rerr)
		} else {
			r.p.printf("  ✗ %s does not hold the private key for %s\n", path, recip)
		}
		_, key, err := r.p.choose(fmt.Sprintf("Put the matching private key in %s (mode 600).\n[r] it is there now, check again  [q] quit", path), 0, "r", "q")
		if err != nil {
			return err
		}
		if key == "q" {
			return errQuit
		}
	}
}

// askDrill is step 12: drill on this machine, and its memory limit.
func (r *initRun) askDrill() error {
	yes, err := r.p.yesNo("Test-restore backups on this machine every night?", true)
	if err != nil {
		return err
	}
	if !yes {
		for i := range r.cfg.Instances {
			r.cfg.Instances[i].SetDrillOff(true)
		}
		r.cfg.Drill = nil
		r.p.printf("Backups on this machine will not be test-restored; every report will say so.\n")
		return nil
	}
	for {
		v, err := r.p.ask("Limit the drill's memory (e.g. 2g; Enter for no limit)", "")
		if err != nil {
			return err
		}
		if v != "" && !config.ValidDockerMemory(v) {
			r.p.printf("  %q is not a docker --memory value (a number with b, k, m or g)\n", v)
			continue
		}
		r.cfg.Drill = nil
		if v != "" {
			r.cfg.Drill = &config.Drill{Memory: v}
		}
		return nil
	}
}

// askSchedule is step 13 (ADR-0029): the rhythm, the times of day, the
// days (weekly, monthly) and how many periods to keep every backup of.
func (r *initRun) askSchedule() error {
	s := config.DefaultSchedule
	r.p.printf("\nBackup runs by this machine's clock (%s); the test-restore runs inside each backup.\n", machineZone())
	rhythms := []string{config.EveryDaily, config.EveryWeekly, config.EveryMonthly}
	_, key, err := r.p.choose(fmt.Sprintf("[d] daily  [w] weekly  [m] monthly  [Enter] %s", s.Every), 0, "d", "w", "m", "")
	if err != nil {
		return err
	}
	if i := strings.Index("dwm", key); key != "" && rhythms[i] != s.Every {
		// A new rhythm starts from its defaults.
		s.Every, s.Keep = rhythms[i], config.DefaultKeep(rhythms[i])
		s.Days = []string{}
		switch s.Every {
		case config.EveryWeekly:
			s.Days = []string{"mon"}
		case config.EveryMonthly:
			s.Days = []string{"1"}
		}
	}
	askList := func(prompt string, dst *[]string, check func(string) (string, string)) error {
		for {
			v, err := r.p.ask(prompt, strings.Join(*dst, ", "))
			if err != nil {
				return err
			}
			var out []string
			seen := map[string]bool{}
			problem := ""
			for _, part := range strings.Split(v, ",") {
				part = strings.ToLower(strings.TrimSpace(part))
				if part == "" {
					continue
				}
				norm, bad := check(part)
				switch {
				case bad != "":
					problem = bad
				case seen[norm]:
					problem = norm + " twice"
				}
				seen[norm] = true
				out = append(out, norm)
			}
			if problem == "" && len(out) == 0 {
				problem = "at least one"
			}
			if problem != "" {
				r.p.printf("  %s\n", problem)
				continue
			}
			*dst = out
			return nil
		}
	}
	if err := askList("Time of day (HH:MM, several separated by commas)", &s.At, func(v string) (string, string) {
		if !config.ValidClock(v) {
			return v, v + " is not HH:MM"
		}
		return v, ""
	}); err != nil {
		return err
	}
	switch s.Every {
	case config.EveryWeekly:
		if err := askList("Days of the week (mon…sun, several separated by commas)", &s.Days, func(v string) (string, string) {
			if !slices.Contains(config.Weekdays, v) {
				return v, v + " is not a day of the week (mon…sun)"
			}
			return v, ""
		}); err != nil {
			return err
		}
	case config.EveryMonthly:
		if err := askList("Days of the month (1–28 or last, several separated by commas)", &s.Days, func(v string) (string, string) {
			if v == config.LastDay {
				return v, ""
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 28 {
				return v, v + " is not a day of the month (1–28 or last)"
			}
			return strconv.Itoa(n), ""
		}); err != nil {
			return err
		}
	}
	for {
		v, err := r.p.ask(fmt.Sprintf("Keep every backup of the last N %ss", s.Period()), strconv.Itoa(s.Keep))
		if err != nil {
			return err
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			r.p.printf("  a whole number ≥ 1\n")
			continue
		}
		s.Keep = n
		break
	}
	r.cfg.Schedule = &s
	return nil
}

// describeSchedule is a schedule in one line: "daily at 03:00, 15:00, keep
// every backup of the last 7 days".
func describeSchedule(sc config.Schedule) string {
	var b strings.Builder
	b.WriteString(sc.Every)
	if len(sc.Days) > 0 {
		b.WriteString(" on " + strings.Join(sc.Days, ", "))
	}
	fmt.Fprintf(&b, " at %s, keep every backup of the last %d %ss", strings.Join(sc.At, ", "), sc.Keep, sc.Period())
	return b.String()
}

// storeSecrets is step 11: no question, secrets.env in the app directory is
// the one source every command reads (absolute, chmod 600). It holds exactly
// what this run checked; a leftover of an interrupted init is replaced
// (ADR-0030).
func (r *initRun) storeSecrets() error {
	path, err := r.configPath(config.SecretsFile)
	if err != nil {
		return err
	}
	r.cfg.SecretsFile = path
	if err := config.WriteEnvFile(path, r.secrets); err != nil {
		return err
	}
	names := slices.Sorted(maps.Keys(r.secrets))
	r.p.printf("Secrets saved to %s (mode 600): %s. They are never written to %s.\n", path, strings.Join(names, ", "), r.path)
	return nil
}

// configPath is <home>/.config/bracer/<app>/<name>, absolute: the timers run
// without a shell, so app.yaml must not depend on `~` (ADR-0021).
func (r *initRun) configPath(name string) (string, error) {
	return filepath.Join(filepath.Dir(r.path), name), nil
}

// execSelf runs this binary for the proofs with exactly env.
func execSelf(ctx context.Context, args, env []string, stdout, stderr io.Writer) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, "bracer init: cannot find this binary:", err)
		return exitError
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env, cmd.Stdout, cmd.Stderr = env, stdout, stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintln(stderr, "bracer init:", err)
		return exitError
	}
	return exitOK
}

// execTTY runs a command on the terminal (sudo asks its password there).
func execTTY(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
