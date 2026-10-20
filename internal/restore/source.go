package restore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/storage"
)

// ErrNoEnv: the run was made before ADR-0015 and has no environment file;
// it can only be previewed.
var ErrNoEnv = fmt.Errorf("this backup was made before Bracer recorded the server's environment (ADR-0015), so restore cannot check a target against it")

// RunPick is one run chosen from the manifest: its ts, environment and
// globals objects, and the dump of each database.
type RunPick struct {
	Instance string
	TS       string
	Env      *backup.Dump
	Globals  *backup.Dump
	DBs      []*backup.Dump
	Names    []string
}

// PickRun chooses the run at ts (default: the latest environment file) and
// the dump of every database at that ts (dbs empty: all of them).
func PickRun(im *backup.InstanceManifest, instance, ts string, dbs []string) (*RunPick, error) {
	if len(im.Env.Dumps) == 0 {
		return nil, usagef("%v; use --preview to look at the data", ErrNoEnv)
	}
	if ts == "" {
		ts = im.Env.Latest
	}
	rp := &RunPick{Instance: instance, TS: ts}
	for _, d := range im.Env.Dumps {
		if d.TS == ts {
			rp.Env = d
		}
	}
	if rp.Env == nil {
		var have []string
		for _, d := range im.Env.Dumps {
			have = append(have, d.TS)
		}
		return nil, usagef("no backup run of %s at %s (runs with an environment file: %s); an older run can only be previewed (--preview)", instance, ts, strings.Join(have, ", "))
	}
	for _, d := range im.Globals.Dumps {
		if d.TS == ts {
			rp.Globals = d
		}
	}
	if len(dbs) == 0 {
		for name, dm := range im.Databases {
			for _, d := range dm.Dumps {
				if d.TS == ts {
					dbs = append(dbs, name)
				}
			}
		}
		sort.Strings(dbs)
	}
	if len(dbs) == 0 {
		return nil, usagef("the run %s of %s has no database dump", ts, instance)
	}
	for _, name := range dbs {
		var dp *backup.Dump
		if dm := im.Databases[name]; dm != nil {
			for _, d := range dm.Dumps {
				if d.TS == ts {
					dp = d
				}
			}
		}
		if dp == nil {
			return nil, usagef("the run %s of %s has no dump of %s; restore takes one run whole", ts, instance, name)
		}
		rp.DBs = append(rp.DBs, dp)
		rp.Names = append(rp.Names, name)
	}
	return rp, nil
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
