package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/storage"
)

// A run is one folder bracer/<folder>/<ts>/ in the bucket, ended by
// run.json, written last (ADR-0026). The newest run.json is the state of
// the app: status, retention, the writer check and restore read it.
//
// <ts>/app.yaml[.age]              the app.yaml the run backed up
// <ts>/<instance>/<db>.dump[.age]
// <ts>/<instance>/globals.sql[.age]
// <ts>/<instance>/env.json[.age]
// <ts>/run.json                    plaintext, metadata only

// Run is run.json.
type Run struct {
	Schema int    `json:"schema"`
	App    string `json:"app"`
	TS     string `json:"ts"`
	// Writer is the one place that writes the folder (ADR-0023).
	Writer Writer `json:"writer"`
	// Backup is how the dump part went; the run's verdict is Verdict().
	Backup Outcome `json:"backup"`
	// AppYAML is the app.yaml of the run; nil when it could not be written.
	AppYAML   *Dump                   `json:"app_yaml,omitempty"`
	Instances map[string]*RunInstance `json:"instances"`

	root string // bracer/<folder>/, for the keys of the objects
}

// Outcome is how the backup part of a run went: ok or failed (ADR-0029);
// status reads it.
type Outcome struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Result     string    `json:"result"` // ok, failed
	Reasons    []string  `json:"reasons"`
	Fix        string    `json:"fix"`
	// Warnings is everything the run found worth a look that did not fail
	// it, each line as printed (ADR-0029).
	Warnings []string `json:"warnings"`
}

// Results of a run, as exit 0 and 1 (ADR-0022, ADR-0029).
const (
	ResultOK     = "ok"
	ResultFailed = "failed"
)

// RunInstance is one instance in a run.
type RunInstance struct {
	Kind        string `json:"kind"` // container, local, url
	PgVersion   string `json:"pg_version,omitempty"`
	ToolVersion string `json:"tool_version,omitempty"` // the pg_dump --version used
	// SystemID is the system_identifier of the cluster (spec backup § Flow
	// 3a2); IdentityChanged when it is not the one app.yaml records.
	SystemID        string  `json:"system_id,omitempty"`
	IdentityChanged bool    `json:"identity_changed,omitempty"`
	Encrypted       bool    `json:"encrypted"`
	Recipient       *string `json:"recipient"`
	// SharedPreloadLibraries is the server's; nil when the backup user could
	// not read it (restore then does not check the target).
	SharedPreloadLibraries *string `json:"shared_preload_libraries,omitempty"`
	// Missing names what the run lacks of this instance: a database that
	// failed, globals (full source), env. Empty: the instance is whole.
	Missing   []string         `json:"missing"`
	Env       *Dump            `json:"env,omitempty"`
	Globals   *Dump            `json:"globals,omitempty"`
	Databases map[string]*Dump `json:"databases"`
	// Drill is the test-restore of this run's objects.
	Drill *DrillOutcome `json:"drill,omitempty"`
}

// Dump is one object of a run: its size and checksum as stored (after
// encryption), and for a database what restore needs to know about it. Key,
// TS, Encrypted and Recipient are not in run.json: they follow from the run
// and the instance, and are filled in when a run is loaded.
type Dump struct {
	Key       string  `json:"-"`
	TS        string  `json:"-"`
	Encrypted bool    `json:"-"`
	Recipient *string `json:"-"`
	Bytes     int64   `json:"bytes"`
	SHA256    string  `json:"sha256"`
	// HasPasswords: the globals dump carries password hashes.
	HasPasswords bool `json:"has_passwords,omitempty"`
	// Locale is the database's encoding/locale at dump time; restore and drill
	// create the database with exactly these values.
	Locale *pg.Locale `json:"locale,omitempty"`
	// Extensions installed in the database at dump time (except plpgsql): drill
	// picks the image from them, drill/restore check the target has them all.
	Extensions []pg.Extension `json:"extensions,omitempty"`
	Tables     []pg.Table     `json:"tables,omitempty"`
	// External lists what depends on something outside Postgres that a dump
	// cannot carry: a restore brings it back but cannot promise it works
	// (ADR-0015, ADR-0029).
	External []string `json:"external,omitempty"`
	// RestoreNeeds is what the target server must have for a restore of
	// this database beyond the dump.
	RestoreNeeds *pg.RestoreNeeds `json:"restore_needs,omitempty"`
	// LockObjects sizes the locks a single-transaction load holds.
	LockObjects *pg.LockObjects `json:"lock_objects,omitempty"`
	// ToolVersion is the pg_dump used; a pg_restore reading the archive must
	// have major ≥ it. Filled from the instance when loaded.
	ToolVersion string `json:"-"`
}

// DrillOutcome is the test-restore of an instance in a run.
type DrillOutcome struct {
	Result      string    `json:"result"` // passed, failed, off
	At          time.Time `json:"at,omitempty"`
	TookSeconds float64   `json:"took_seconds,omitempty"`
	Reasons     []string  `json:"reasons,omitempty"`
}

// Drill results.
const (
	DrillPassed = "passed"
	DrillFailed = "failed"
	DrillOff    = "off"
)

const runSchema = 1

// ErrRunCorrupt: a run.json is unreadable.
var ErrRunCorrupt = errors.New("run.json is not valid JSON")

// RunFile is the name of the file that ends a run.
const RunFile = "run.json"

// RunDir is <root><ts>/.
func RunDir(root, ts string) string { return root + ts + "/" }

// RunKey is <root><ts>/run.json.
func RunKey(root, ts string) string { return RunDir(root, ts) + RunFile }

// Keys of the objects of a run.
func appYAMLKey(root, ts string, encrypted bool) string {
	return RunDir(root, ts) + "app.yaml" + ageExt(encrypted)
}

func dumpKey(root, ts, instance, db string, encrypted bool) string {
	return RunDir(root, ts) + instance + "/" + db + ".dump" + ageExt(encrypted)
}

func globalsKey(root, ts, instance string, encrypted bool) string {
	return RunDir(root, ts) + instance + "/globals.sql" + ageExt(encrypted)
}

func envKey(root, ts, instance string, encrypted bool) string {
	return RunDir(root, ts) + instance + "/env.json" + ageExt(encrypted)
}

func ageExt(encrypted bool) string {
	if encrypted {
		return ".age"
	}
	return ""
}

// newRun starts the run ts of the app under root.
func newRun(app, root, ts string, here Writer) *Run {
	return &Run{Schema: runSchema, App: app, TS: ts, Writer: here, Instances: map[string]*RunInstance{}, root: root}
}

// Root is bracer/<folder>/ of the run.
func (r *Run) Root() string { return r.root }

// Verdict is the run's result as ok or failed, computed from what run.json
// records and nowhere stored (ADR-0027, ADR-0029): failed when the backup
// failed or any drill failed; ok otherwise. backup, status, retention and
// restore all read it.
func (r *Run) Verdict() string {
	if r.Backup.Result == ResultFailed {
		return ResultFailed
	}
	for _, in := range r.Instances {
		if in != nil && in.Drill != nil && in.Drill.Result == DrillFailed {
			return ResultFailed
		}
	}
	return ResultOK
}

// hydrate fills the fields that are not in run.json: keys and encryption
// of every object, from the run and its instance.
func (r *Run) hydrate(root string) {
	r.root = root
	if r.Instances == nil {
		r.Instances = map[string]*RunInstance{}
	}
	if a := r.AppYAML; a != nil {
		enc := false
		var rcpt *string
		for _, ri := range r.Instances {
			enc, rcpt = ri.Encrypted, ri.Recipient
			break
		}
		a.Key, a.TS, a.Encrypted, a.Recipient = appYAMLKey(root, r.TS, enc), r.TS, enc, rcpt
	}
	for name, ri := range r.Instances {
		if ri.Databases == nil {
			ri.Databases = map[string]*Dump{}
		}
		fill := func(d *Dump, key string) {
			d.Key, d.TS, d.Encrypted, d.Recipient, d.ToolVersion = key, r.TS, ri.Encrypted, ri.Recipient, ri.ToolVersion
		}
		if ri.Env != nil {
			fill(ri.Env, envKey(root, r.TS, name, ri.Encrypted))
		}
		if ri.Globals != nil {
			fill(ri.Globals, globalsKey(root, r.TS, name, ri.Encrypted))
		}
		for db, d := range ri.Databases {
			fill(d, dumpKey(root, r.TS, name, db, ri.Encrypted))
		}
	}
}

// instance returns the instance entry, created when missing.
func (r *Run) instance(name string) *RunInstance {
	ri := r.Instances[name]
	if ri == nil {
		ri = &RunInstance{Databases: map[string]*Dump{}, Missing: []string{}}
		r.Instances[name] = ri
	}
	return ri
}

// Whole: the instance has everything the run meant to back up of it.
func (ri *RunInstance) Whole() bool { return ri != nil && len(ri.Missing) == 0 }

// DrillPassed: the instance was test-restored in this run and passed.
func (ri *RunInstance) DrillPassed() bool {
	return ri != nil && ri.Drill != nil && ri.Drill.Result == DrillPassed
}

// AllDrillsPassed: every instance of the run that was test-restored passed,
// and at least one was. The run retention keeps as "latest that passed a
// drill" (ADR-0019, ADR-0026).
func (r *Run) AllDrillsPassed() bool {
	drilled := false
	for _, ri := range r.Instances {
		if ri.Drill == nil || ri.Drill.Result == DrillOff {
			continue
		}
		if ri.Drill.Result != DrillPassed {
			return false
		}
		drilled = true
	}
	return drilled
}

// Encrypted: some object of the run is encrypted, so restore needs the age key.
func (r *Run) Encrypted() bool {
	for _, ri := range r.Instances {
		if ri.Encrypted {
			return true
		}
	}
	return false
}

// Recipient is the age recipient of the run; "" when nothing is encrypted.
func (r *Run) Recipient() string {
	for _, ri := range r.Instances {
		if ri.Encrypted && ri.Recipient != nil {
			return *ri.Recipient
		}
	}
	return ""
}

// InstanceNames, sorted.
func (r *Run) InstanceNames() []string {
	out := make([]string, 0, len(r.Instances))
	for n := range r.Instances {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// RunRef is one run folder as the bucket lists it: its ts, whether it has
// run.json, and every key under it.
type RunRef struct {
	TS   string
	Done bool // run.json is there: the run ended
	// Dumps counts the database dumps of the folder: a run without one
	// (nothing connected) takes no place in the calendar (ADR-0028).
	Dumps int
	Keys  []string
}

// ListRuns lists the run folders under root, newest first, from one listing
// of root. Keys outside a <ts>/ folder (the probe object, anything from
// another layout) are not runs and never touched.
func ListRuns(ctx context.Context, st storage.Store, root string) ([]RunRef, error) {
	objs, err := st.List(ctx, root)
	if err != nil {
		return nil, err
	}
	byTS := map[string]*RunRef{}
	for _, o := range objs {
		rest := strings.TrimPrefix(o.Key, root)
		ts, file, ok := strings.Cut(rest, "/")
		if !ok {
			continue
		}
		if _, err := time.Parse(tsLayout, ts); err != nil {
			continue
		}
		ref := byTS[ts]
		if ref == nil {
			ref = &RunRef{TS: ts}
			byTS[ts] = ref
		}
		ref.Keys = append(ref.Keys, o.Key)
		if file == RunFile {
			ref.Done = true
		}
		if strings.Contains(file, "/") && (strings.HasSuffix(file, ".dump") || strings.HasSuffix(file, ".dump.age")) {
			ref.Dumps++
		}
	}
	out := make([]RunRef, 0, len(byTS))
	for _, ref := range byTS {
		sort.Strings(ref.Keys)
		out = append(out, *ref)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS > out[j].TS })
	return out, nil
}

// NewestDone is the newest run that ended, or "" when none did.
func NewestDone(refs []RunRef) string {
	for _, r := range refs {
		if r.Done {
			return r.TS
		}
	}
	return ""
}

// LoadRun reads run.json of the run ts. Missing → ErrNoFolder; corrupt →
// ErrRunCorrupt.
func LoadRun(ctx context.Context, st storage.Store, root, ts string) (*Run, error) {
	key := RunKey(root, ts)
	rc, err := st.Get(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s not found", ErrNoFolder, key)
	}
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	r := &Run{}
	if err := json.Unmarshal(data, r); err != nil {
		return nil, fmt.Errorf("%s: %w: %v", key, ErrRunCorrupt, err)
	}
	if r.TS == "" {
		r.TS = ts
	}
	r.hydrate(root)
	return r, nil
}

// SaveRun writes run.json of the run: the last object of a run.
func SaveRun(ctx context.Context, st storage.Store, r *Run) error {
	r.Schema = runSchema
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return st.Put(ctx, RunKey(r.root, r.TS), bytes.NewReader(data), int64(len(data)))
}

// Newest loads the newest run.json under root: the state of the app.
// ErrNoFolder when no run ended there.
func Newest(ctx context.Context, st storage.Store, root string) (*Run, []RunRef, error) {
	refs, err := ListRuns(ctx, st, root)
	if err != nil {
		return nil, nil, err
	}
	ts := NewestDone(refs)
	if ts == "" {
		return nil, refs, fmt.Errorf("%w: no run.json under %s", ErrNoFolder, root)
	}
	r, err := LoadRun(ctx, st, root, ts)
	return r, refs, err
}

// NewOutcome builds the outcome of the backup part from what failed, each
// as printed, and the warnings of the run. A line starting with "Fix:" is
// not a reason; the first one found is the fix, else fix.
func NewOutcome(started, finished time.Time, failures, warnings []string, fix string) Outcome {
	o := Outcome{StartedAt: started.UTC(), FinishedAt: finished.UTC(), Result: ResultOK, Reasons: []string{}, Warnings: append([]string{}, warnings...)}
	first := ""
	for _, r := range failures {
		var text []string
		for _, line := range strings.Split(r, "\n") {
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "Fix: "):
				if first == "" {
					first = strings.TrimPrefix(line, "Fix: ")
				}
			case line != "":
				text = append(text, line)
			}
		}
		if len(text) > 0 {
			o.Reasons = append(o.Reasons, strings.Join(text, "; "))
		}
	}
	if len(failures) > 0 {
		o.Result = ResultFailed
		o.Fix = first
		if o.Fix == "" {
			o.Fix = fix
		}
	}
	return o
}

// Worse of two results: failed over ok.
func Worse(a, b string) string {
	if a == ResultFailed || b == ResultFailed {
		return ResultFailed
	}
	return ResultOK
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
