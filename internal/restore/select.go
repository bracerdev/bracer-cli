package restore

import (
	"fmt"
	"sort"
	"strings"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
)

// Selection is one dump chosen from the manifest.
type Selection struct {
	Instance  string
	Database  string // "" when Globals
	Globals   bool
	Dump      *backup.Dump
	PgVersion string
}

// UsageError is a usage error (exit 2): wrong dump chosen, missing --instance…
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

func usagef(format string, a ...interface{}) error {
	return &UsageError{Msg: fmt.Sprintf(format, a...)}
}

// Select picks the dump by --instance/--database/--globals/--ts from the manifest,
// only among the instances present in app.yaml (spec fetch-restore § Flow step 2).
func Select(m *backup.Manifest, cfg *config.App, instance, database, ts string, globals bool) (*Selection, error) {
	var names []string
	for _, in := range cfg.Instances {
		if im := m.Instances[in.Name]; im != nil && (len(im.Databases) > 0 || len(im.Globals.Dumps) > 0) {
			names = append(names, in.Name)
		}
	}
	if len(names) == 0 {
		return nil, usagef("no backup in the bucket for the instance(s) in app.yaml; run bracer backup first")
	}
	switch {
	case instance == "" && len(names) == 1:
		instance = names[0]
	case instance == "":
		return nil, usagef("app.yaml has several instances with backups (%s); pass --instance", strings.Join(names, ", "))
	case cfg.FindInstance(instance) == nil:
		return nil, usagef("instance %q is not in app.yaml", instance)
	case m.Instances[instance] == nil:
		return nil, usagef("no backup in the bucket for instance %s; run bracer backup first", instance)
	}
	im := m.Instances[instance]
	sel := &Selection{Instance: instance, Globals: globals, PgVersion: im.PgVersion}
	var dumps []*backup.Dump
	var latest string
	switch {
	case globals:
		dumps, latest = im.Globals.Dumps, im.Globals.Latest
		if len(dumps) == 0 {
			return nil, usagef("no globals dump for instance %s", instance)
		}
	case database == "":
		return nil, usagef("pass --database <name> or --globals; databases in the bucket for %s: %s", instance, strings.Join(dbNames(im), ", "))
	default:
		dm := im.Databases[database]
		if dm == nil || len(dm.Dumps) == 0 {
			return nil, usagef("no backup of %s for instance %s; databases in the bucket: %s", database, instance, strings.Join(dbNames(im), ", "))
		}
		dumps, latest = dm.Dumps, dm.Latest
		sel.Database = database
	}
	if ts == "" || ts == "latest" {
		ts = latest
	}
	for _, dp := range dumps {
		if dp.TS == ts {
			sel.Dump = dp
			return sel, nil
		}
	}
	var have []string
	for _, dp := range dumps {
		have = append(have, dp.TS)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(have)))
	return nil, usagef("no dump with ts %s; available: %s", ts, strings.Join(have, ", "))
}

func dbNames(im *backup.InstanceManifest) []string {
	var names []string
	for name, dm := range im.Databases {
		if len(dm.Dumps) > 0 {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return []string{"(none)"}
	}
	return names
}

// Entry is one row for `fetch --list`.
type Entry struct {
	Instance  string
	Database  string // "globals" for globals
	TS        string
	Latest    bool
	Bytes     int64
	Encrypted bool
	Recipient string
}

// List lists every dump in the manifest ordered by instance, database, ts descending.
func List(m *backup.Manifest) []Entry {
	var out []Entry
	var instances []string
	for name := range m.Instances {
		instances = append(instances, name)
	}
	sort.Strings(instances)
	for _, in := range instances {
		im := m.Instances[in]
		var dbs []string
		for name := range im.Databases {
			dbs = append(dbs, name)
		}
		sort.Strings(dbs)
		for _, db := range dbs {
			for _, dp := range im.Databases[db].Dumps {
				out = append(out, entry(in, db, dp, im.Databases[db].Latest))
			}
		}
		for _, dp := range im.Globals.Dumps {
			out = append(out, entry(in, "globals", dp, im.Globals.Latest))
		}
	}
	return out
}

func entry(in, db string, dp *backup.Dump, latest string) Entry {
	e := Entry{Instance: in, Database: db, TS: dp.TS, Latest: dp.TS == latest, Bytes: dp.Bytes, Encrypted: dp.Encrypted}
	if dp.Recipient != nil {
		e.Recipient = *dp.Recipient
	}
	return e
}
