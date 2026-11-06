package drill

import (
	"context"
	"time"

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
		who, endpoint := "$USER", ""
		if rt := cfg.Runtime; rt != nil {
			if rt.User != "" {
				who = rt.User
			}
			endpoint = rt.DockerHost
		}
		return nil, &drillError{cause: causeDocker, docker: pg.WhyDockerDown(err, who, endpoint),
			msg: "Docker is not available for the temporary Postgres: " + firstLine(err.Error())}
	}
	return &dockerRuntime{sh: d.Shell}, nil
}

// cleanupCtx is a fresh context for tearing down: Ctrl-C/timeout must still clean up.
func cleanupCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
}

// The free disk a drill needs (spec backup § Drill): TempPostgresBytes for
// the temporary Postgres itself, plus SourceFactor × the size of the
// instance's databases at the source (pg_database_size, read in the same
// run). Measured with the temporary Postgres as the drill starts it
// (wal_level=minimal) and pg_restore --single-transaction, on PG10 and
// PG18: a fresh cluster is about 40 MB; pg_wal stays at 16 MB; the peak
// is the restored databases (about their size at the source) plus the
// sort space of the largest index build, 1.25 to 1.75 × the source in all
// cases measured. The size of the compressed dump says nothing here: one
// compressed 80 times, another not at all.
const (
	TempPostgresBytes int64 = 400 << 20
	SourceFactor            = 2
)

// NeedBytes is the free disk a drill of databases of sourceBytes at the
// source needs.
func NeedBytes(sourceBytes int64) int64 {
	return TempPostgresBytes + sourceBytes*SourceFactor
}

// reportDisk records the disk check; short or not measurable, the drill
// does not run (ADR-0022). The check is early, not a promise: a disk that
// fills during the load is told by diskFull.
func reportDisk(dc restore.DiskCheck, ir *InstanceResult) error {
	ir.Disk = &dc
	switch {
	case !dc.Checked:
		return failf(causeDiskUnknown, "cannot check free disk space for a drill: %s", dc.Error)
	case !dc.Enough():
		return failf(causeDiskShort, "not enough disk space for a drill: %s free at %s, about %s needed (%s for the temporary Postgres + %d × the databases' size at the source)",
			humanBytes(dc.FreeBytes), dc.DataDirectory, humanBytes(dc.NeedBytes), humanBytes(TempPostgresBytes), SourceFactor)
	}
	return nil
}
