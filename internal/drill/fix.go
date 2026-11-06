package drill

import (
	"errors"
	"fmt"
	"strings"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/restore"
)

// cause is why a drill failed: its errors carry it, never a Fix sentence;
// fixText renders the one Fix line from it (backup spec, Drill section,
// the Fix table). The order is the priority when an instance fails more
// than once: a higher cause says more about what to do. The disk check
// stops everything else; a disk that filled explains whatever failed after
// it; a copy that differs, or an archive that cannot be read, is about the
// backup itself and must not be hidden by a cleanup that failed after it;
// then what about the temporary Postgres cannot be done (no image helps),
// the image, pulling it, Docker; last what is known only to have failed.
type cause int

const (
	causeOther cause = iota
	causeDocker
	causePull
	causeImage
	causeUnsupported
	causeBackup
	causeMismatch
	causeBroken
	causeDiskFull
	causeDiskUnknown
	causeDiskShort
)

// drillError is a drill error with its cause and what its Fix needs.
type drillError struct {
	cause  cause
	msg    string
	docker pg.DockerDown // causeDocker
	image  string        // causePull: the image this machine must pull
}

func (e *drillError) Error() string { return e.msg }

func failf(c cause, format string, args ...any) error {
	return &drillError{cause: c, msg: fmt.Sprintf(format, args...)}
}

// withCause marks err as of cause c.
func withCause(c cause, err error) error {
	return &drillError{cause: c, msg: err.Error()}
}

// orCause is err with its own cause when it has one, else with c.
func orCause(c cause, err error) error {
	var e *drillError
	if errors.As(err, &e) {
		return err
	}
	return withCause(c, err)
}

// pullError: image cannot be pulled (or built from what must be pulled).
func pullError(image string, err error) error {
	return &drillError{cause: causePull, msg: err.Error(), image: image}
}

func asDrillError(err error) *drillError {
	var e *drillError
	if errors.As(err, &e) {
		return e
	}
	return &drillError{cause: causeOther, msg: err.Error()}
}

// fail records err as one more error of the instance; the cause of the
// instance is the highest of its errors (see cause), every error is kept.
func (ir *InstanceResult) fail(err error) {
	e := asDrillError(err)
	ir.errs = append(ir.errs, e.msg)
	ir.Error = strings.Join(ir.errs, "\n")
	if ir.why == nil || e.cause > ir.why.cause {
		ir.why = e
	}
}

// failImageChecks fails the instance for the checks of the first round
// that env-up cannot make true: the cause of the most telling check.
func (ir *InstanceResult) failImageChecks(image string, manual []restore.Check) {
	c := causeOther
	for _, ch := range manual {
		c = max(c, checkCause(ch))
	}
	ir.fail(failf(c, "the temporary Postgres (%s) cannot take this backup: %s", image, describe(manual)))
}

// checkCause is what a comparison line that env-up cannot fix means in a
// temporary Postgres that the drill itself starts (as its bootstrap
// superuser, from a fresh cluster):
//   - another image can have it: the server version, an extension, a
//     preload library, a C library or text search file, a locale, a
//     collation;
//   - no image can (causeUnsupported): a tablespace (it needs a directory
//     on the target), the users of a run without globals, a setting on the
//     server's command line, what the bootstrap superuser cannot lose;
//   - the archive cannot be read (causeBroken): the database's own entries;
//   - anything else is the drill's own (causeOther).
func checkCause(ch restore.Check) cause {
	switch ch.Area {
	case "version", "extension", "preload", "locale", "collation", "library", "text search file":
		if strings.HasPrefix(ch.Detail, "cannot read") || strings.HasPrefix(ch.Detail, "cannot check") {
			return causeOther
		}
		return causeImage
	case "tablespace", "role", "setting":
		return causeUnsupported
	case "database":
		if strings.HasSuffix(ch.Name, " attributes") {
			return causeBroken
		}
	}
	return causeOther
}

// diskFull reports a drill failure whose text says the disk filled up:
// ENOSPC as Postgres prints it ("No space left on device") or as Go and
// the Docker daemon do (lowercase: docker pull, volume create, run), or
// Postgres failing to grow a relation file ("could not extend file …").
// A shared memory segment that cannot grow is /dev/shm of the container,
// not the disk, so a line about one does not count.
func diskFull(text string) bool {
	for _, line := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return r == '\n' || r == ';' }) {
		if strings.Contains(line, "shared memory segment") {
			continue
		}
		if strings.Contains(line, "no space left on device") || strings.Contains(line, "could not extend file") {
			return true
		}
	}
	return false
}

// Audience is who reads a drill's Fix line, which decides what the line
// can tell them to do.
type Audience int

const (
	// Elsewhere: the app exists and runs nightly (a nightly run, status, a
	// --dry-run typed by hand): the next run tries again, the drill is
	// turned off by setting the app up again.
	Elsewhere Audience = iota
	// InitPrompt: init's preflight, read just above its question [r] retry,
	// [i] image, [n] no drill, [q] quit: the line points at those keys.
	InitPrompt
	// InitRun: init's real run (step 16). No app.yaml and no timer exist,
	// so there is no next run and nothing to remove: the way on is running
	// init again, whose check asks about the drill only for what its
	// schema-only drill meets as well.
	InitRun
)

// settle gives a failed instance its cause and its one Fix line, once its
// result is complete. A copy differs only when every database loaded: a
// database that did not load is that failure's.
func (ir *InstanceResult) settle(app string, aud Audience) {
	if ir.OK {
		return
	}
	why := ir.why
	if why == nil {
		why = &drillError{cause: causeOther}
	}
	loadFailed, differs := false, false
	for _, dr := range ir.Databases {
		loadFailed = loadFailed || dr.Error != ""
		differs = differs || (dr.checked && !dr.OK)
	}
	c := why.cause
	switch {
	case c == causeMismatch && loadFailed:
		c = causeOther
	case differs && !loadFailed && causeMismatch > c:
		c = causeMismatch
	}
	if causeDiskFull > c && diskFull(strings.Join(ir.Failures(), "\n")) {
		c = causeDiskFull
	}
	ir.cause = c
	ir.Fix = fixText(c, why, app, ir.Name, ir, aud)
}

// fixText is the one Fix line of a drill failure of instance with cause c,
// for aud; why holds what the error knew, ir what was measured (nil when
// no instance started). "" for causeBackup: the backup's own Fix stands.
func fixText(c cause, why *drillError, app, instance string, ir *InstanceResult, aud Audience) string {
	off := ", or turn the drill of " + instance + " off: " + backup.FixReinit(app)
	again := "run bracer init " + app + " again"
	// asked: init's check meets the same failure and asks about the drill.
	asked := "answer n when its check asks about the drill of " + instance
	// retry is an action after which trying again can pass; orOff: init's
	// check meets the same failure, so turning the drill off is a way too.
	retry := func(s string, orOff bool) string {
		switch aud {
		case InitPrompt:
			return s + ", then answer r; or answer n to turn its drill off"
		case InitRun:
			if orOff {
				return s + ", then " + again + "; if you cannot, " + asked
			}
			return s + ", then " + again
		}
		return s + off
	}
	image := ""
	if ir != nil {
		image = ir.Image
	}
	switch c {
	case causeDiskShort:
		return retry("free up at least "+humanBytes(ir.Disk.NeedBytes-ir.Disk.FreeBytes)+" for Docker", true)
	case causeDiskFull:
		free := ""
		if ir != nil && ir.Disk != nil && ir.Disk.Checked {
			free = " (" + humanBytes(ir.Disk.FreeBytes) + " was free when the drill started)"
		}
		// Filled during the load: init's schema-only drill does not meet it.
		return retry("free up disk space for Docker"+free, false)
	case causeDiskUnknown:
		return retry("check that Docker can run "+image+" and that docker info names its root directory", true)
	case causeDocker:
		do := why.docker.Sentence()
		if aud == InitPrompt && why.docker.NeedsLogin() {
			return "answer q, " + do + ", then " + again + "; or answer n to turn its drill off"
		}
		return retry(do, false)
	case causePull:
		do := "check that this machine can pull " + why.image + ": docker pull " + why.image
		switch aud {
		case InitPrompt:
			return do + ", then answer r; answer i if the image name is wrong, or n to turn its drill off"
		case InitRun:
			return do + ", then " + again + "; if the image name is wrong, answer i when its check asks about the drill of " + instance
		}
		return do + "; the next run tries again" + off
	case causeImage:
		switch aud {
		case InitPrompt:
			return "answer i to enter another Docker image for this drill, or n to turn its drill off"
		case InitRun:
			return again + ": its check asks about the drill of " + instance + "; answer i to enter another Docker image, or n to turn it off"
		}
		return backup.FixReinit(app) + ": when its check asks about the drill of " + instance + ", answer i to enter another Docker image, or n to turn it off"
	case causeUnsupported:
		const why = "this backup cannot be test-restored in a temporary Postgres for the reason above, whatever the image"
		switch aud {
		case InitPrompt:
			return why + "; answer n to turn its drill off"
		case InitRun:
			return why + "; " + again + " and " + asked
		}
		return why + "; turn the drill of " + instance + " off: " + backup.FixReinit(app)
	case causeMismatch:
		switch aud {
		case InitPrompt:
			return "answer r to test-restore again; if it differs again, this backup does not restore what it recorded and cannot be relied on, so answer n only to accept backups that are never test-restored"
		case InitRun:
			return "this backup did not restore what it recorded: " + again + ", and if it happens again, do not rely on backups of " + instance + " until a drill passes"
		}
		return "the next run backs up and test-restores again; until one passes, do not rely on this backup: its restored copy differs from what it recorded"
	case causeBroken:
		switch aud {
		case InitPrompt:
			return "answer r to back up and test-restore again; until one passes, do not rely on this backup: its archive cannot be read, so answer n only to accept backups that are never test-restored"
		case InitRun:
			return "this backup's archive cannot be read: " + again + ", and if it happens again, do not rely on backups of " + instance + " until a drill passes"
		}
		return "the next run backs up and test-restores again; until one passes, do not rely on this backup: its archive cannot be read"
	case causeBackup:
		return ""
	}
	switch aud {
	case InitPrompt:
		return "answer r to try again, or n to turn its drill off"
	case InitRun:
		return again
	}
	return "the next run tries again" + off
}
