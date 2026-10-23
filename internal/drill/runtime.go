package drill

import (
	"context"
	"fmt"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/restore"
)

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

// pickRuntime (spec drill § Data model "runtime check"): Docker must answer.
func pickRuntime(ctx context.Context, cfg *config.App, d Deps) (*dockerRuntime, error) {
	if err := d.Tools.Docker().Info(ctx); err != nil {
		return nil, fmt.Errorf("Docker is not available for the temporary Postgres: %v\nFix: %s", firstLine(err.Error()), backup.FixReinit(cfg.App))
	}
	return &dockerRuntime{sh: d.Shell}, nil
}

// cleanupCtx is a fresh context for tearing down: Ctrl-C/timeout must still clean up.
func cleanupCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
}

// reportDisk records the disk check; short or not measurable, the drill
// does not run (ADR-0022).
func reportDisk(dc restore.DiskCheck, ir *InstanceResult) error {
	ir.Disk = &dc
	switch {
	case !dc.Checked:
		return fmt.Errorf("cannot check free disk space for a drill: %s", dc.Error)
	case !dc.Enough():
		return fmt.Errorf("not enough disk space for a drill: %s free at %s, about %s needed (%d × the dumps)", humanBytes(dc.FreeBytes), dc.DataDirectory, humanBytes(dc.NeedBytes), restore.DiskFactor)
	}
	return nil
}
