package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/storage"
)

// Manifest is manifest.json in the bucket: plaintext, metadata only.
type Manifest struct {
	Schema    int                          `json:"schema"`
	App       string                       `json:"app"`
	UpdatedAt time.Time                    `json:"updated_at"`
	Instances map[string]*InstanceManifest `json:"instances"`
}

// InstanceManifest is the part for one instance.
type InstanceManifest struct {
	PgVersion string `json:"pg_version"`
	// Kind is the source kind of the last run: container, local or url. A url
	// run has no globals: databases only (ADR-0015).
	Kind string `json:"kind,omitempty"`
	// Env holds the environment files, one per run (ADR-0015); empty in
	// manifests written before it, whose runs can only be previewed.
	Env GlobalsManifest `json:"env"`
	// SharedPreloadLibraries is the server's shared_preload_libraries; nil when the
	// backup user could not read it (restore then does not check the target).
	SharedPreloadLibraries *string                      `json:"shared_preload_libraries,omitempty"`
	Databases              map[string]*DatabaseManifest `json:"databases"`
	Globals                GlobalsManifest              `json:"globals"`
}

// DatabaseManifest holds the list of dumps of one database.
type DatabaseManifest struct {
	Latest string  `json:"latest"`
	Dumps  []*Dump `json:"dumps"`
}

// GlobalsManifest holds the pg_dumpall --globals-only dumps.
type GlobalsManifest struct {
	Latest       string  `json:"latest"`
	HasPasswords bool    `json:"has_passwords"`
	Dumps        []*Dump `json:"dumps,omitempty"`
}

// Dump is one object in the bucket.
type Dump struct {
	TS         string    `json:"ts"`
	Key        string    `json:"key"`
	Bytes      int64     `json:"bytes"`
	SHA256     string    `json:"sha256"`
	Encrypted  bool      `json:"encrypted"`
	Recipient  *string   `json:"recipient"`
	UploadedAt time.Time `json:"uploaded_at"`
	// ToolVersion is the `pg_dump --version` used (may be newer than the server
	// when Docker is absent); a pg_restore reading this archive must have major ≥ it.
	ToolVersion string `json:"tool_version,omitempty"`
	// Locale is the database's encoding/locale at dump time; restore and drill create
	// the database with exactly these values. Nil in manifests written by older Bracer versions.
	Locale *pg.Locale `json:"locale,omitempty"`
	// Extensions installed in the database at dump time (except plpgsql): drill picks
	// the image from them, drill/restore check the target has them all before restoring.
	Extensions []pg.Extension `json:"extensions,omitempty"`
	Tables     []pg.Table     `json:"tables,omitempty"`
	// SystemID is the system_identifier of the cluster the dump came from (spec
	// backup § Flow 3a2), so drill and reports never mix two clusters' dumps.
	SystemID string `json:"system_id,omitempty"`
	// OutOfScope lists what depends on files or processes outside Postgres
	// (untrusted languages, C outside $libdir, file_fdw, subscriptions): a
	// restore cannot guarantee them (ADR-0015).
	OutOfScope []string `json:"out_of_scope,omitempty"`
}

const manifestSchema = 1

// ErrManifestCorrupt: manifest.json is unreadable; treated as empty and retention is skipped.
var ErrManifestCorrupt = errors.New("manifest.json is not valid JSON")

// CorruptError is ErrManifestCorrupt with the original bytes so backup can keep
// a copy before overwriting (spec backup § Edge cases).
type CorruptError struct {
	Raw []byte
	Err error
}

func (e *CorruptError) Error() string        { return fmt.Sprintf("%v: %v", ErrManifestCorrupt, e.Err) }
func (e *CorruptError) Is(target error) bool { return target == ErrManifestCorrupt }
func (e *CorruptError) Unwrap() error        { return e.Err }

// Keys lists every object key the manifest points to (dumps and globals alike).
func (m *Manifest) Keys() map[string]bool {
	keys := map[string]bool{}
	for _, im := range m.Instances {
		for _, dm := range im.Databases {
			for _, d := range dm.Dumps {
				keys[d.Key] = true
			}
		}
		for _, d := range im.Globals.Dumps {
			keys[d.Key] = true
		}
		for _, d := range im.Env.Dumps {
			keys[d.Key] = true
		}
	}
	return keys
}

func newManifest(app string) *Manifest {
	return &Manifest{Schema: manifestSchema, App: app, Instances: map[string]*InstanceManifest{}}
}

// LoadManifest loads the manifest from the bucket. Missing → empty. Corrupt → empty with ErrManifestCorrupt.
func LoadManifest(ctx context.Context, st storage.Store, key, app string) (*Manifest, error) {
	rc, err := st.Get(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return newManifest(app), nil
	}
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	m := newManifest(app)
	if err := json.Unmarshal(data, m); err != nil {
		return newManifest(app), &CorruptError{Raw: data, Err: err}
	}
	if m.Instances == nil {
		m.Instances = map[string]*InstanceManifest{}
	}
	return m, nil
}

// SaveManifest overwrites the whole manifest.json.
func SaveManifest(ctx context.Context, st storage.Store, key string, m *Manifest, now time.Time) error {
	m.Schema = manifestSchema
	m.UpdatedAt = now.UTC()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return st.Put(ctx, key, bytes.NewReader(data), int64(len(data)))
}

func (m *Manifest) instance(name string) *InstanceManifest {
	im := m.Instances[name]
	if im == nil {
		im = &InstanceManifest{Databases: map[string]*DatabaseManifest{}}
		m.Instances[name] = im
	}
	if im.Databases == nil {
		im.Databases = map[string]*DatabaseManifest{}
	}
	return im
}

func (im *InstanceManifest) database(name string) *DatabaseManifest {
	dm := im.Databases[name]
	if dm == nil {
		dm = &DatabaseManifest{}
		im.Databases[name] = dm
	}
	return dm
}

// addDump records a new dump and updates latest.
func (dm *DatabaseManifest) addDump(d *Dump) {
	dm.Dumps = append(dm.Dumps, d)
	sortDumps(dm.Dumps)
	dm.Latest = dm.Dumps[len(dm.Dumps)-1].TS
}

func sortDumps(ds []*Dump) {
	sort.SliceStable(ds, func(i, j int) bool { return ds[i].UploadedAt.Before(ds[j].UploadedAt) })
}

// knownDatabases is the databases present in the instance's manifest (to compare with `all`).
func (im *InstanceManifest) knownDatabases() []string {
	out := make([]string, 0, len(im.Databases))
	for k := range im.Databases {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// diff returns the elements in a that are not in b.
func diff(a, b []string) []string {
	set := map[string]bool{}
	for _, s := range b {
		set[s] = true
	}
	var out []string
	for _, s := range a {
		if !set[s] {
			out = append(out, s)
		}
	}
	return out
}

// countOldKey counts dumps still using a key other than the current recipient (after rotate-key).
func (m *Manifest) countOldKey(recipient string) int {
	n := 0
	for _, im := range m.Instances {
		for _, dm := range im.Databases {
			for _, d := range dm.Dumps {
				if d.Encrypted && d.Recipient != nil && *d.Recipient != recipient {
					n++
				}
			}
		}
	}
	return n
}
