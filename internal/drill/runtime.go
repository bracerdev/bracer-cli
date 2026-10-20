package drill

import (
	"context"
	"fmt"
	"time"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/restore"
)

// UsageError is a drill setting that cannot be honoured (exit 2): drill.runtime: none.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// tempPG is the temporary Postgres: tools run through run, connecting over the
// socket as user, the source's bootstrap superuser.
type tempPG struct {
	run   pg.Runner
	image string
	name  string // container name
	user  string

	volumes []string // named volumes created for it
}

func (t *tempPG) runner() pg.Runner { return t.run }

func (t *tempPG) conn(db string) pg.Conn { return pg.Conn{User: t.user, Database: db} }

// client returns a pg.Client connected to database db of the temporary Postgres.
func (t *tempPG) client(db string) *pg.Client {
	return &pg.Client{Runner: t.runner(), Conn: t.conn(db)}
}

// OffError is the message of drill.runtime: none (spec drill AC).
func OffError(app string) error {
	return &UsageError{fmt.Sprintf("drill is off for app %s (drill.runtime: none). To test-restore on another machine, copy app.yaml there and set drill.runtime", app)}
}

// pickRuntime (spec drill § Data model "runtime check"): drill.runtime: none →
// UsageError unless --runtime docker; otherwise Docker, which must answer.
func pickRuntime(ctx context.Context, cfg *config.App, d Deps) (*dockerRuntime, error) {
	if cfg.DrillOff() && !d.ForceDocker {
		return nil, OffError(cfg.App)
	}
	if err := d.Tools.Docker().Info(ctx); err != nil {
		return nil, fmt.Errorf("Docker is not available for the temporary Postgres: %v\nFix: bracer check", firstLine(err.Error()))
	}
	return &dockerRuntime{sh: d.Shell}, nil
}

// cleanupCtx is a fresh context for tearing down: Ctrl-C/timeout must still clean up.
func cleanupCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
}

// reportDisk records the disk check and decides whether to go on (spec drill § Data model).
func reportDisk(dc restore.DiskCheck, in config.Instance, ir *InstanceResult, res *Result, d Deps) error {
	ir.Disk = &dc
	switch {
	case !dc.Checked:
		res.warn("%s: %s; the drill may fill the disk", in.Name, dc.Error)
	case !dc.Enough() && !d.SkipDiskCheck:
		return fmt.Errorf("not enough disk space for a drill: %s free at %s, about %s may be needed; free space or pass --skip-disk-check", humanBytes(dc.FreeBytes), dc.DataDirectory, humanBytes(dc.NeedBytes))
	case !dc.Enough():
		res.warn("%s: only %s free at %s, about %s may be needed (--skip-disk-check)", in.Name, humanBytes(dc.FreeBytes), dc.DataDirectory, humanBytes(dc.NeedBytes))
	}
	return nil
}
