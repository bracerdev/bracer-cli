package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/drill"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/storage"
)

// init proves what it writes before it becomes app.yaml
// (ADR-0019, ADR-0022): a draft next to app.yaml, the preflight on the
// draft as the timer runs it, a real backup (test-restored), then the
// rename and the timer.

// draftPath is .<file>.init next to the file.
func draftPath(abs string) string {
	return filepath.Join(filepath.Dir(abs), "."+filepath.Base(abs)+".init")
}

// resultPath is where `backup --init-draft` writes its Result, next to the
// draft: init --web reports it once the app is in the account (BR-021).
func resultPath(draft string) string { return draft + ".result.json" }

// prove is steps 14–17 up to the rename; under --web, the app is created
// or continued in the account between the real run and the rename.
func (r *initRun) prove() (err error) {
	draft := draftPath(r.path)
	defer func() {
		if err != nil {
			_ = os.Remove(draft)
		}
		_ = os.Remove(resultPath(draft))
	}()
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	// A draft is a whole app.yaml, since included; realRun stamps it again
	// right before the run it proves (spec backup/schedule, BR-023).
	r.cfg.Schedule.Since = config.SinceOf(r.now())
	if err := config.SaveDraft(draft, r.cfg); err != nil {
		return err
	}
	if err := r.preflight(draft); err != nil {
		return err
	}
	if err := r.realRun(draft); err != nil {
		return err
	}
	if r.web != nil {
		if err := r.web.connect(draft); err != nil {
			return err
		}
	}
	// 17. The draft becomes app.yaml: the unit calls the app by name, so a
	// timer firing now sees the new file. app.yaml's schedule is in force
	// from now, just before the timer, and strictly after the real run's
	// since: the real run above never stands in for a slot the timer owes.
	r.cfg.Schedule.Since = laterSince(r.cfg.Schedule.Since, r.now())
	if err := config.SaveDraft(draft, r.cfg); err != nil {
		return err
	}
	if err := os.Chmod(draft, 0o644); err != nil {
		return err
	}
	if err := os.Rename(draft, r.path); err != nil {
		return err
	}
	r.written = true
	r.p.printf("\nWrote %s.\n", r.path)
	if r.web != nil {
		r.web.reportProof(resultPath(draft))
	}
	return nil
}

// laterSince is now as a since, at least a second after since: two
// stamps within one second would make the real run's period and the
// timer's one (BR-023).
func laterSince(since string, now time.Time) string {
	t := now.UTC().Truncate(time.Second)
	if prev, err := time.Parse(config.SinceLayout, since); err == nil && !t.After(prev) {
		t = prev.Add(time.Second)
	}
	return config.SinceOf(t)
}

// drillAny reports an instance whose drill is on.
func (r *initRun) drillAny() bool {
	return !r.cfg.DrillOff()
}

// cleanEnv is the environment the nightly job runs with: HOME and runtime.path only.
func (r *initRun) cleanEnv() ([]string, error) {
	home, err := r.home()
	if err != nil {
		return nil, fmt.Errorf("home directory: %w", err)
	}
	return []string{"HOME=" + home, "PATH=" + r.cfg.Runtime.Path}, nil
}

// childArgs is a bracer command on the draft, as if it were app.yaml.
func (r *initRun) childArgs(cmd ...string) []string {
	return append(append(cmd[:1:1], r.appName, "--init-draft"), cmd[1:]...)
}

// preflight is step 15: backup --dry-run on the draft as the nightly job
// will run it, with a drill of the schema-only run. A drill that fails for
// one instance can be retried as is (after freeing disk, say), take
// another image or be turned off for it; anything else stops.
func (r *initRun) preflight(draft string) error {
	env, err := r.cleanEnv()
	if err != nil {
		return err
	}
	for {
		args := r.childArgs("backup", "--dry-run", "--json")
		r.p.printf("\nChecking as the nightly job will run: bracer %s\n", strings.Join(args, " "))
		var out bytes.Buffer
		code := r.runSelf(r.ctx, args, env, &out, r.stderr)
		if err := r.ctx.Err(); err != nil {
			return err
		}
		var pf preflightResult
		if jerr := json.Unmarshal(out.Bytes(), &pf); jerr != nil {
			r.stdout.Write(out.Bytes())
			return r.checkFailed("the check", code)
		}
		printPreflight(r.stdout, &pf)
		if code == exitOK && pf.OK {
			return nil
		}
		failed := pf.drillFailed()
		if pf.otherFailed() || len(failed) == 0 {
			return r.checkFailed("the check", code)
		}
		for _, name := range failed {
			retry, err := r.fixDrill(name)
			if err != nil {
				return err
			}
			if retry {
				// Run the same check again now; what was answered for
				// the instances before this one is saved first.
				break
			}
		}
		if err := config.SaveDraft(draft, r.cfg); err != nil {
			return err
		}
	}
}

func (r *initRun) checkFailed(what string, code int) error {
	r.p.printf("\n%s failed (exit %d). %s Fix the above, then run: %s\n", what, code, r.unchanged(), r.again)
	return errSelfCheck
}

// fixDrill is step 15 for an instance whose drill failed in the preflight;
// retry is true when the same check is to run again with nothing changed.
func (r *initRun) fixDrill(name string) (retry bool, err error) {
	in := r.cfg.FindInstance(name)
	if in == nil {
		return false, fmt.Errorf("instance %s is not in the draft", name)
	}
	r.p.printf("\nThe drill of %s fails (above). The nightly drill would fail the same way.\n", name)
	for {
		_, key, err := r.p.choose("[r] retry  [i] enter a Docker image for this instance's drill  [n] no drill for this instance  [q] quit", 0, "r", "i", "n", "q")
		if err != nil {
			return false, err
		}
		switch key {
		case "q":
			return false, errQuit
		case "r":
			return true, nil
		case "n":
			in.SetDrillOff(true)
			r.p.printf("%s: drill.runtime: none; its backups will not be test-restored and every report will say so.\n", name)
			return false, nil
		}
		image, err := r.p.askRequired("Docker image (built on the official postgres image, with the extensions and libraries the databases use)")
		if err != nil {
			return false, err
		}
		image = strings.TrimSpace(image)
		if config.IsBitnamiImage(image) {
			r.p.printf("Bitnami images cannot be the drill image (their entrypoint runs a temporary server bracer cannot tell from the real one).\n")
			continue
		}
		in.SetDrillImage(image)
		return false, nil
	}
}

// realRun is step 16: a real backup of the draft, its drill included,
// always. Drill on with too little disk, or disk that cannot be measured,
// stops here.
func (r *initRun) realRun(draft string) error {
	secrets, _, err := config.LoadSecrets(r.cfg)
	if err != nil {
		return err
	}
	// Each instance is drilled in its own temporary Postgres, one after
	// the other: the disk must hold the largest, by the drill's own rule.
	var size, need int64
	var sizeErr error
	for _, in := range r.cfg.Instances {
		n, err := r.databaseBytes(in, secrets)
		if err != nil {
			sizeErr = fmt.Errorf("%s: cannot read the size of its databases: %v", in.Name, firstLine(err.Error()))
			break
		}
		size += n
		if !in.DrillOff() {
			need = max(need, drill.NeedBytes(n))
		}
	}
	drillOn := r.drillAny()
	r.p.printf("\nA real backup now proves the data too:\n")
	if sizeErr == nil {
		r.p.printf("  - the databases hold %s (an upper bound of the dumps)\n", humanBytes(size))
	}
	r.p.printf("  - while pg_dump runs, ALTER TABLE and migrations on these databases wait; drill uses CPU and memory of this machine\n")
	if drillOn {
		free, freeErr := r.dockerFree()
		switch {
		case r.ctx.Err() != nil:
			// The measures were cut short by the interrupt: not a disk problem.
			return r.ctx.Err()
		case sizeErr != nil:
			r.p.printf("  ✗ %v, so the disk the drill needs is unknown; answer n to test-restore on this machine to go on without a drill\n", sizeErr)
			return r.checkFailed("the disk check", exitError)
		case freeErr != nil:
			r.p.printf("  ✗ cannot check free disk space for the drill: %s; answer n to test-restore on this machine to go on without a drill\n", firstLine(freeErr.Error()))
			return r.checkFailed("the disk check", exitError)
		case free < need:
			r.p.printf("  ✗ free up %s for the drill (%s free, about %s needed: %s for the temporary Postgres + %d × the databases), or answer n to test-restore on this machine\n", humanBytes(need-free), humanBytes(free), humanBytes(need), humanBytes(drill.TempPostgresBytes), drill.SourceFactor)
			return r.checkFailed("the disk check", exitError)
		}
		r.p.printf("  - drill restores them into a temporary Postgres in Docker: %s free, about %s needed\n", humanBytes(free), humanBytes(need))
	}
	env, err := r.cleanEnv()
	if err != nil {
		return err
	}
	// The real run's period starts now, not when the draft was first
	// written: time spent at the drill questions must not fall inside it,
	// or this run could serve a slot that passed meanwhile (BR-023).
	r.cfg.Schedule.Since = config.SinceOf(r.now())
	if err := config.SaveDraft(draft, r.cfg); err != nil {
		return err
	}
	args := r.childArgs("backup")
	r.p.printf("\nRunning: bracer %s\n", strings.Join(args, " "))
	code := r.runSelf(r.ctx, args, env, r.stdout, r.stderr)
	if err := r.ctx.Err(); err != nil {
		// The run cleaned up after itself; say interrupted, not failed.
		return err
	}
	if code != exitOK {
		return r.checkFailed("the real run", code)
	}
	return nil
}

// databaseBytes is the sum of pg_database_size of the instance's databases.
func (r *initRun) databaseBytes(in config.Instance, secrets *config.Secrets) (int64, error) {
	conn, runner, err := backup.Connect(r.ctx, r.tools, in, secrets)
	if err != nil {
		return 0, err
	}
	client := &pg.Client{Runner: runner, Conn: conn}
	return client.DatabasesSize(r.ctx, in.Databases.Names())
}

// dockerFree is the free space of the Docker root directory.
func (r *initRun) dockerFree() (int64, error) {
	out, err := r.shell.Run(r.ctx, pg.Cmd{Name: "docker", Args: []string{"info", "--format", "{{.DockerRootDir}}"}})
	if err != nil {
		return 0, err
	}
	dir := strings.TrimSpace(string(out.Stdout))
	if dir == "" {
		return 0, errors.New("docker info gave no root directory")
	}
	out, err = r.shell.Run(r.ctx, pg.Cmd{Name: "df", Args: []string{"-Pk", dir}})
	if err != nil {
		return 0, err
	}
	return pg.ParseDF(out.Stdout)
}

// install is step 17 after the rename: the timer, written by this
// command through sudo (ADR-0027).
func (r *initRun) install() error {
	r.p.printf("\nInstalling the nightly timer (sudo)\n")
	if err := r.installTimer(r.ctx, r.cfg, r.path); err != nil {
		r.p.printf("  ✗ %v\n\n%s written; no timer installed\nFix: %s\n", err, r.path, backup.FixReinit(r.appName))
		return errSelfCheck
	}
	r.timer = true
	return nil
}

// rescueCard is step 18: what a restore needs if this machine is lost. The
// age key must be confirmed copied off the machine, a reused one too:
// nothing says whether that copy was ever made.
func (r *initRun) rescueCard() error {
	s := r.cfg.Storage
	r.p.printf("\nIf this machine is lost, restore needs:\n")
	r.p.printf("  endpoint  %s\n  bucket    %s\n  folder    %s\n", s.Endpoint, s.Bucket, storage.Join(config.BucketPrefix, s.Folder))
	if r.cfg.Encryption != "age" {
		return nil
	}
	r.p.printf("  age key   %s\n  this file is the only thing that cannot be recreated; S3 keys can be made again at your provider\n", r.cfg.AgeIdentityFile)
	for {
		ans, err := r.p.ask(fmt.Sprintf("Copy %s somewhere off this machine (password manager, another disk). Without it the encrypted backups cannot be opened if this machine is lost. Type copied when done", r.cfg.AgeIdentityFile), "")
		if err != nil {
			return err
		}
		if strings.TrimSpace(ans) == "copied" {
			return nil
		}
	}
}
