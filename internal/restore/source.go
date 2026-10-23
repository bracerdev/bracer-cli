package restore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/storage"
)

// RunPick is one run chosen for an instance: its ts, environment and
// globals objects, and the dump of each database.
type RunPick struct {
	Instance string
	TS       string
	Env      *backup.Dump
	Globals  *backup.Dump
	DBs      []*backup.Dump
	Names    []string
}

// PickRun takes the instance of the run ts and the dump of each database of
// dbs (the databases of the run's app.yaml). Completeness is checked apart
// (RunMissing).
func PickRun(ri *backup.RunInstance, instance, ts string, dbs []string) (*RunPick, error) {
	rp := &RunPick{Instance: instance, TS: ts, Env: ri.Env, Globals: ri.Globals}
	if rp.Env == nil {
		return nil, fmt.Errorf("the run %s of %s has no environment file", ts, instance)
	}
	if len(dbs) == 0 {
		return nil, fmt.Errorf("the run %s of %s has no database", ts, instance)
	}
	for _, name := range dbs {
		dp := ri.Databases[name]
		if dp == nil {
			return nil, fmt.Errorf("the run %s of %s has no dump of %s", ts, instance, name)
		}
		rp.DBs = append(rp.DBs, dp)
		rp.Names = append(rp.Names, name)
	}
	return rp, nil
}

// RunMissing is what the run lacks of an instance (ADR-0016): the databases
// of its app.yaml (expected) without a dump, and what backup recorded as
// missing in run.json.
func RunMissing(ri *backup.RunInstance, expected []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, name := range expected {
		if ri.Databases[name] == nil {
			add(name)
		}
	}
	for _, m := range ri.Missing {
		add(m)
	}
	return out
}

// MaxEnv is the largest environment file read into memory.
const MaxEnv = 4 << 20

// LoadSource reads the run's environment, globals and each archive's table
// of contents (through runner's pg_restore; no connection is made).
func LoadSource(ctx context.Context, st storage.Store, rp *RunPick, ids *Identities, runner pg.Runner, conn pg.Conn) (*SourceRun, error) {
	env, err := ReadEnv(ctx, st, rp.Env, ids)
	if err != nil {
		return nil, err
	}
	src := &SourceRun{Instance: rp.Instance, TS: rp.TS, Kind: env.SourceKind, Env: env}
	if env.SourceKind != "url" {
		if rp.Globals == nil {
			return nil, fmt.Errorf("the run %s has no globals dump although its source is %s; restore needs both", rp.TS, env.SourceKind)
		}
		g, err := readObject(ctx, st, rp.Globals, ids, MaxGlobals)
		if err != nil {
			return nil, fmt.Errorf("globals: %w", err)
		}
		if src.Globals, err = ParseGlobals(g); err != nil {
			return nil, err
		}
	}
	for i, dp := range rp.DBs {
		dp := dp
		info, err := ReadArchive(ctx, runner, conn, func() (io.ReadCloser, error) { return OpenDump(ctx, st, dp, ids) })
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", dp.Key, err)
		}
		src.DBs = append(src.DBs, &SourceDB{Name: rp.Names[i], Dump: dp, AttrSQL: info.AttrSQL, Entries: info.Entries})
	}
	return src, nil
}

// ReadEnv reads one environment file.
func ReadEnv(ctx context.Context, st storage.Store, dp *backup.Dump, ids *Identities) (*pg.Env, error) {
	data, err := readObject(ctx, st, dp, ids, MaxEnv)
	if err != nil {
		return nil, fmt.Errorf("environment file: %w", err)
	}
	env := &pg.Env{}
	if err := json.Unmarshal(data, env); err != nil {
		return nil, fmt.Errorf("environment file %s: %w", dp.Key, err)
	}
	if env.Schema > pg.EnvSchema {
		return nil, fmt.Errorf("environment file %s has format %d; this bracer reads up to %d: update bracer", dp.Key, env.Schema, pg.EnvSchema)
	}
	return env, nil
}

func readObject(ctx context.Context, st storage.Store, dp *backup.Dump, ids *Identities, max int64) ([]byte, error) {
	rc, err := OpenDump(ctx, st, dp, ids)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s is larger than %d MB", dp.Key, max>>20)
	}
	if ce := checksumErr(rc); ce != nil {
		return nil, ce
	}
	return data, nil
}

// Locales are the source locales of the run, for ReadTarget.
func (s *SourceRun) Locales() []*pg.Locale {
	var out []*pg.Locale
	for _, d := range s.DBs {
		out = append(out, d.Dump.Locale)
	}
	return out
}

// Wants is what ReadTarget reads for this run: the database locales, the
// locales of the collations the dumps create.
func (s *SourceRun) Wants() Wants {
	w := Wants{Locales: s.Locales()}
	for _, d := range s.DBs {
		n := d.Dump.RestoreNeeds
		if n == nil {
			continue
		}
		for i := range n.Collations {
			if c := &n.Collations[i]; c.Created {
				w.Locales = append(w.Locales, &c.Locale)
			}
		}
	}
	return w
}

// Names are the source database names.
func (s *SourceRun) Names() []string {
	out := make([]string, len(s.DBs))
	for i, d := range s.DBs {
		out[i] = d.Name
	}
	return out
}

// Mode is full for a container or local source, data for a URL.
func (s *SourceRun) Mode() string {
	if s.Kind == "url" {
		return ModeData
	}
	return ModeFull
}
