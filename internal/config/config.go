// Package config reads, validates and writes app.yaml.
//
// app.yaml has three levels: app → instance → database (spec backup/init § Data
// model). The file never contains secrets; every secret goes through an
// environment variable name (*_env) and is loaded by LoadSecrets.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

// App is the whole of app.yaml. Every field is written by init and
// read strictly: an unknown field is a parse error (ADR-0022).
type App struct {
	App             string     `yaml:"app"`
	SecretsFile     string     `yaml:"secrets_file"`
	Instances       []Instance `yaml:"instances"`
	Storage         *Storage   `yaml:"storage"`
	Encryption      string     `yaml:"encryption"` // none, age
	AgeRecipient    string     `yaml:"age_recipient,omitempty"`
	AgeIdentityFile string     `yaml:"age_identity_file,omitempty"`
	// HeartbeatURLEnv names the secret holding a Healthchecks-style URL: one
	// signal per backup run, no details (ADR-0025, ADR-0026).
	HeartbeatURLEnv string  `yaml:"heartbeat_url_env,omitempty"`
	Hosted          *Hosted `yaml:"hosted,omitempty"`
	Drill           *Drill  `yaml:"drill,omitempty"`
	// Schedule is when backup runs and how long runs are kept (ADR-0029);
	// the drill runs inside each backup.
	Schedule *Schedule `yaml:"schedule"`
	Runtime  *Runtime  `yaml:"runtime"`
}

// Schedule is the schedule: block of app.yaml (ADR-0029): one rhythm, one
// or more times of day, and how many periods of that rhythm to keep every
// backup of. Only init writes it; backup reads Every and Keep for
// retention, the schedule install turns it into OnCalendar lines.
type Schedule struct {
	Every string   `yaml:"every" json:"every"`         // daily, weekly, monthly
	At    []string `yaml:"at" json:"at"`               // HH:MM, machine time
	Days  []string `yaml:"days" json:"days,omitempty"` // weekly: mon…sun; monthly: 1–28 or last; daily: empty
	Keep  int      `yaml:"keep" json:"keep"`           // every backup of the last Keep periods that have one
}

// The rhythms of Schedule.Every.
const (
	EveryDaily   = "daily"
	EveryWeekly  = "weekly"
	EveryMonthly = "monthly"
)

// Weekdays are the values of Schedule.Days for a weekly schedule, Monday first.
var Weekdays = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// LastDay is the value of Schedule.Days for the last day of every month.
const LastDay = "last"

// reClock is a time of day, HH:MM.
var reClock = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// ValidClock: s is HH:MM.
func ValidClock(s string) bool { return reClock.MatchString(s) }

// DefaultKeep is how many periods init offers to keep for a rhythm.
func DefaultKeep(every string) int {
	switch every {
	case EveryWeekly:
		return 4
	case EveryMonthly:
		return 3
	}
	return 7
}

// DefaultSchedule is what init offers.
var DefaultSchedule = Schedule{Every: EveryDaily, At: []string{"03:00"}, Days: []string{}, Keep: 7}

// Validate checks the values, one problem per line.
func (s Schedule) Validate() error {
	if p := s.problems(); len(p) > 0 {
		return errors.New(strings.Join(p, "; "))
	}
	return nil
}

func (s Schedule) problems() []string {
	var errs []string
	switch s.Every {
	case EveryDaily, EveryWeekly, EveryMonthly:
	default:
		errs = append(errs, "every: must be daily, weekly or monthly")
	}
	if len(s.At) == 0 {
		errs = append(errs, "at: at least one time of day (HH:MM)")
	}
	seen := map[string]bool{}
	for _, a := range s.At {
		switch {
		case !ValidClock(a):
			errs = append(errs, fmt.Sprintf("at: %q is not HH:MM", a))
		case seen[a]:
			errs = append(errs, fmt.Sprintf("at: %s twice", a))
		}
		seen[a] = true
	}
	switch s.Every {
	case EveryDaily:
		if len(s.Days) > 0 {
			errs = append(errs, "days: must be empty for a daily schedule")
		}
	case EveryWeekly:
		if len(s.Days) == 0 {
			errs = append(errs, "days: at least one day of the week (mon…sun)")
		}
		for _, d := range s.Days {
			if !slices.Contains(Weekdays, d) {
				errs = append(errs, fmt.Sprintf("days: %q is not a day of the week (mon…sun)", d))
			}
		}
	case EveryMonthly:
		if len(s.Days) == 0 {
			errs = append(errs, "days: at least one day of the month (1–28 or last)")
		}
		for _, d := range s.Days {
			if n, err := strconv.Atoi(d); d != LastDay && (err != nil || n < 1 || n > 28) {
				errs = append(errs, fmt.Sprintf("days: %q is not a day of the month (1–28 or last)", d))
			}
		}
	}
	seen = map[string]bool{}
	for _, d := range s.Days {
		if seen[d] {
			errs = append(errs, fmt.Sprintf("days: %s twice", d))
		}
		seen[d] = true
	}
	if s.Keep < 1 {
		errs = append(errs, "keep: must be ≥ 1")
	}
	return errs
}

// Period names the unit of Every: day, week or month.
func (s Schedule) Period() string {
	switch s.Every {
	case EveryWeekly:
		return "week"
	case EveryMonthly:
		return "month"
	}
	return "day"
}

// Runtime is the context init ran in successfully on this machine, so backup
// and drill (by hand or timer) call the same docker the same way without
// reading the shell's variables (spec init § Data model).
type Runtime struct {
	User       string `yaml:"user"`                  // user that ran init; schedule runs jobs as this user
	Path       string `yaml:"path"`                  // PATH for child processes (docker credential helpers)
	Docker     string `yaml:"docker"`                // absolute path of the docker CLI
	DockerHost string `yaml:"docker_host,omitempty"` // resolved endpoint (DOCKER_HOST > DOCKER_CONTEXT > current context)
}

// What init does not ask is a constant, not a field (ADR-0022).
const (
	// BackupTimeout bounds one database: count + pg_dump + upload.
	BackupTimeout = 6 * time.Hour
	// LockTimeout is pg_dump --lock-wait-timeout and the lock_timeout of the
	// row-count session.
	LockTimeout = time.Minute
	// DrillTimeout bounds the load of one database in a drill or a restore.
	DrillTimeout = 6 * time.Hour
	// ExactMaxRows: a table whose reltuples is below it is counted exactly.
	ExactMaxRows int64 = 1_000_000
)

var reDockerMem = regexp.MustCompile(`^[0-9]+[bkmgBKMG]?$`)

// ValidDockerMemory is the form `docker run --memory` takes (512m, 2g).
func ValidDockerMemory(s string) bool { return reDockerMem.MatchString(s) }

// Drill is the app-wide drill configuration (spec drill § Data model). The
// image and whether drill runs at all are per instance (InstanceDrill).
type Drill struct {
	Memory string `yaml:"memory,omitempty"` // docker run --memory, e.g. 2g
}

// InstanceDrill is the drill of one instance (ADR-0019).
type InstanceDrill struct {
	Runtime string `yaml:"runtime,omitempty"` // "", none: none turns drill off for this instance
	Image   string `yaml:"image,omitempty"`   // default: like the source (spec drill § Flow 3b)
}

// DrillRuntimeNone turns drill off for an instance.
const DrillRuntimeNone = "none"

// DrillOff reports that no instance is drilled: every one has drill.runtime: none.
func (a *App) DrillOff() bool {
	for _, in := range a.Instances {
		if !in.DrillOff() {
			return false
		}
	}
	return true
}

// DrillMemory is drill.memory, "" for no limit.
func (a *App) DrillMemory() string {
	if a.Drill == nil {
		return ""
	}
	return a.Drill.Memory
}

// DrillOff reports drill.runtime: none on this instance.
func (in Instance) DrillOff() bool {
	return in.Drill != nil && in.Drill.Runtime == DrillRuntimeNone
}

// DrillImage is drill.image of this instance, "" when drill picks it.
func (in Instance) DrillImage() string {
	if in.Drill == nil {
		return ""
	}
	return in.Drill.Image
}

// SetDrillOff turns drill off (or back on) for this instance.
func (in *Instance) SetDrillOff(off bool) {
	if in.Drill == nil {
		in.Drill = &InstanceDrill{}
	}
	in.Drill.Runtime = ""
	if off {
		in.Drill.Runtime, in.Drill.Image = DrillRuntimeNone, ""
	}
	if *in.Drill == (InstanceDrill{}) {
		in.Drill = nil
	}
}

// SetDrillImage sets drill.image of this instance (drill on).
func (in *Instance) SetDrillImage(image string) {
	in.Drill = &InstanceDrill{Image: image}
	if image == "" {
		in.Drill = nil
	}
}

// Instance is one Postgres server: how to reach it (Connection, written by
// init) and what the customer chose to back up (ADR-0018).
type Instance struct {
	Name       string `yaml:"name"`
	Connection `yaml:"connection"`
	Databases  Databases `yaml:"databases"`
	// Drill is how this instance is drilled (ADR-0019).
	Drill *InstanceDrill `yaml:"drill,omitempty"`
}

// Connection is how bracer reaches an instance. init writes it whole and
// replaces it whole when the instance is picked again (ADR-0018). Exactly one
// of Container | Local | URLEnv must be set. → ADR-0013, ADR-0015
type Connection struct {
	Container   string `yaml:"container,omitempty"`
	Local       *Local `yaml:"local,omitempty"`
	URLEnv      string `yaml:"url_env,omitempty"`
	User        string `yaml:"user,omitempty"`         // container and local
	PasswordEnv string `yaml:"password_env,omitempty"` // container and local, when the socket asks for a password (Bitnami, pg_hba md5)
	PgMajor     int    `yaml:"pg_major"`
	// SystemID is the system_identifier of the cluster (pg_control_system()),
	// or SystemIDNone when the server does not let it be read (managed).
	SystemID string `yaml:"system_id"`
}

// SystemIDNone is system_id of a server that does not let pg_control_system()
// be read: backup does not compare identities for it (ADR-0022).
const SystemIDNone = "none"

// Local is Postgres installed on this machine (apt, PGDG), reached over its
// Unix socket by the tools in a postgres image (ADR-0015).
type Local struct {
	Port      int    `yaml:"port"`
	SocketDir string `yaml:"socket_dir,omitempty"` // default DefaultSocketDir
}

// DefaultSocketDir is where Debian/Ubuntu packages put the Postgres socket.
const DefaultSocketDir = "/var/run/postgresql"

// Dir returns socket_dir with the default applied.
func (l *Local) Dir() string {
	if l == nil || l.SocketDir == "" {
		return DefaultSocketDir
	}
	return l.SocketDir
}

// Databases is the list of databases of an instance: always a list, every
// entry `- name: X` (ADR-0016, ADR-0018).
type Databases []Database

// Names lists the database names.
func (ds Databases) Names() []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Name)
	}
	return out
}

// Database is the unit of pg_dump.
type Database struct {
	Name string `yaml:"name"`
	// AcceptedExternal lists what depends on something outside Postgres
	// (external of run.json) that the customer accepted at init (ADR-0019,
	// ADR-0029).
	AcceptedExternal []string `yaml:"accepted_external,omitempty"`
}

// BucketPrefix is where bracer lives in any bucket: every app's folder is
// BucketPrefix/<folder>/ (ADR-0027).
const BucketPrefix = "bracer"

// Storage is the customer's S3-compatible bucket and the app's folder in it.
// None of the three (endpoint, bucket, folder) ever changes for an app:
// another place is another app (ADR-0023).
type Storage struct {
	Endpoint     string `yaml:"endpoint"`
	Bucket       string `yaml:"bucket"`
	Folder       string `yaml:"folder"`
	Region       string `yaml:"region,omitempty"`
	AccessKeyEnv string `yaml:"access_key_env"`
	SecretKeyEnv string `yaml:"secret_key_env"`
	PathStyle    bool   `yaml:"path_style,omitempty"`
}

// reFolder is <app>-<6 random [a-z0-9]> (ADR-0023).
var reFolder = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*-[a-z0-9]{6}$`)

// ValidFolder reports whether s has the form init gives storage.folder.
func ValidFolder(s string) bool { return reFolder.MatchString(s) }

// Hosted is the server receiving POST /report; when absent, run offline. There
// is no heartbeat_id: the server treats (app, kind) as an implicit heartbeat (spec ping).
type Hosted struct {
	Server   string `yaml:"server"`
	TokenEnv string `yaml:"token_env"`
}

// Kind tells how an instance is connected to.
type Kind int

const (
	KindInvalid Kind = iota
	KindContainer
	KindURL
	KindLocal
)

// String is the kind as written in run.json and the environment file.
func (k Kind) String() string {
	switch k {
	case KindContainer:
		return "container"
	case KindLocal:
		return "local"
	case KindURL:
		return "url"
	}
	return "invalid"
}

// Full reports whether bracer backs up the whole instance (databases, globals
// and environment) for this kind; a URL gives databases only (ADR-0015).
func (k Kind) Full() bool { return k == KindContainer || k == KindLocal }

// Kind returns the connection method; KindInvalid unless exactly one is set.
func (in Instance) Kind() Kind {
	n := 0
	for _, set := range []bool{in.Container != "", in.Local != nil, in.URLEnv != ""} {
		if set {
			n++
		}
	}
	switch {
	case n != 1:
		return KindInvalid
	case in.Container != "":
		return KindContainer
	case in.Local != nil:
		return KindLocal
	}
	return KindURL
}

// ValidationError collects every error of one file, each with its field path.
// Fix, when set, is printed as the last line (conventions § error handling).
type ValidationError struct {
	Errs []string
	Fix  string
}

func (e *ValidationError) Error() string {
	msg := "app.yaml: " + strings.Join(e.Errs, "; ")
	if e.Fix != "" {
		msg += "\nFix: " + e.Fix
	}
	return msg
}

var (
	reName   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	reEnvVar = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Parse reads the YAML and validates it. Parse errors are returned with line/column;
// content errors are returned as *ValidationError.
func Parse(data []byte) (*App, error) {
	if err := rejectPlaintextSecrets(data); err != nil {
		return nil, err
	}
	var app App
	if err := yaml.UnmarshalWithOptions(data, &app, yaml.Strict()); err != nil {
		return nil, &ValidationError{Errs: []string{yaml.FormatError(err, false, true)}}
	}
	if err := app.Validate(); err != nil {
		return nil, err
	}
	return &app, nil
}

// ParseRun reads the copy of app.yaml a backup run left in the bucket
// (<ts>/app.yaml): the same strict format, nothing older (ADR-0022).
func ParseRun(data []byte) (*App, error) { return Parse(data) }

// rejectPlaintextSecrets catches `password:` / `url:` before the strict decode, so
// the real reason is reported instead of "unknown field".
func rejectPlaintextSecrets(data []byte) error {
	var raw struct {
		Instances []map[string]interface{} `yaml:"instances"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil // let the strict decode report the detailed parse error
	}
	var errs []string
	for i, in := range raw.Instances {
		p := fmt.Sprintf("instances[%d]", i)
		maps := []struct {
			path string
			m    map[string]interface{}
		}{{p, in}}
		if c, ok := in["connection"].(map[string]interface{}); ok {
			maps = append(maps, struct {
				path string
				m    map[string]interface{}
			}{p + ".connection", c})
		}
		for _, x := range maps {
			for _, k := range []string{"password", "url"} {
				if _, ok := x.m[k]; ok {
					errs = append(errs, fmt.Sprintf("%s.%s: secrets must not be written to app.yaml, use %s_env", x.path, k, k))
				}
			}
		}
	}
	if len(errs) > 0 {
		return &ValidationError{Errs: errs}
	}
	return nil
}

// Validate checks the whole file and reports every error at once.
func (a *App) Validate() error {
	var errs []string
	add := func(format string, args ...interface{}) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}
	if a.App == "" {
		add("app: required")
	} else if !reName.MatchString(a.App) {
		add("app: lowercase letters, digits, `-`, `_` only")
	}
	if a.SecretsFile == "" {
		add("secrets_file: required")
	} else if !filepath.IsAbs(a.SecretsFile) {
		add("secrets_file: must be an absolute path")
	}
	if len(a.Instances) == 0 {
		add("instances: at least one instance is required")
	}
	names := map[string]int{}
	envOwner := map[string]string{}
	for i, in := range a.Instances {
		p := fmt.Sprintf("instances[%d]", i)
		if in.Name == "" {
			add("%s.name: required", p)
		} else if !reName.MatchString(in.Name) {
			add("%s.name: lowercase letters, digits, `-`, `_` only", p)
		} else if j, dup := names[in.Name]; dup {
			add("%s.name: duplicates instances[%d]", p, j)
		} else {
			names[in.Name] = i
		}
		c := p + ".connection"
		switch in.Kind() {
		case KindInvalid:
			add("%s: exactly one of container | local | url_env is required", c)
		case KindURL:
			if in.PasswordEnv != "" || in.User != "" {
				add("%s: url_env already carries user/password; drop user/password_env", c)
			}
		case KindLocal:
			if in.Local.Port < 1 || in.Local.Port > 65535 {
				add("%s.local.port: must be 1–65535", c)
			}
			if in.Local.SocketDir != "" && !filepath.IsAbs(in.Local.SocketDir) {
				add("%s.local.socket_dir: must be an absolute path", c)
			}
		}
		switch in.SystemID {
		case "":
			add("%s.system_id: required (the decimal system_identifier, or none)", c)
		case SystemIDNone:
		default:
			if _, err := strconv.ParseUint(in.SystemID, 10, 64); err != nil {
				add("%s.system_id: expected the decimal system_identifier, or none", c)
			}
		}
		for field, v := range map[string]string{"password_env": in.PasswordEnv, "url_env": in.URLEnv} {
			if v == "" {
				continue
			}
			if !reEnvVar.MatchString(v) {
				add("%s.%s: invalid environment variable name", c, field)
			}
			if owner, dup := envOwner[v]; dup {
				add("%s.%s: variable %s is already used by %s (two instances cannot share a variable)", c, field, v, owner)
			} else {
				envOwner[v] = p
			}
		}
		if in.PgMajor < 10 || in.PgMajor > 30 {
			add("%s.pg_major: required, the major version of the server", c)
		}
		if len(in.Databases) == 0 {
			add("%s.databases: expected at least one database", p)
		}
		dbNames := map[string]bool{}
		for j, d := range in.Databases {
			dp := fmt.Sprintf("%s.databases[%d]", p, j)
			if d.Name == "" {
				add("%s.name: required", dp)
			} else if dbNames[d.Name] {
				add("%s.name: duplicated within the instance", dp)
			}
			dbNames[d.Name] = true
		}
		if dr := in.Drill; dr != nil {
			switch dr.Runtime {
			case "":
			case DrillRuntimeNone:
				if dr.Image != "" {
					add("%s.drill: runtime is none but image is set", p)
				}
			default:
				add("%s.drill.runtime: must be none, or absent", p)
			}
			if IsBitnamiImage(dr.Image) {
				add("%s.drill.image: %s", p, bitnamiDrill(dr.Image))
			}
		}
	}
	switch a.Encryption {
	case "none":
		if a.AgeRecipient != "" || a.AgeIdentityFile != "" {
			add("encryption: is none but age_recipient/age_identity_file are set")
		}
	case "age":
		if !strings.HasPrefix(a.AgeRecipient, "age1") {
			add("age_recipient: expected an age1… public key")
		}
		if a.AgeIdentityFile == "" {
			add("age_identity_file: required with encryption: age")
		} else if !filepath.IsAbs(a.AgeIdentityFile) {
			add("age_identity_file: must be an absolute path")
		}
	case "":
		add("encryption: required (none | age)")
	default:
		add("encryption: must be none | age")
	}
	if s := a.Storage; s == nil {
		add("storage: required")
	} else {
		if s.Endpoint == "" {
			add("storage.endpoint: required")
		}
		if s.Bucket == "" {
			add("storage.bucket: required")
		}
		if !ValidFolder(s.Folder) {
			add("storage.folder: required, <app>-<6 letters or digits> as init writes it")
		}
		for field, v := range map[string]string{"access_key_env": s.AccessKeyEnv, "secret_key_env": s.SecretKeyEnv} {
			if v == "" {
				add("storage.%s: required", field)
			} else if !reEnvVar.MatchString(v) {
				add("storage.%s: invalid environment variable name", field)
			}
		}
	}
	if dr := a.Drill; dr != nil && dr.Memory != "" && !reDockerMem.MatchString(dr.Memory) {
		add("drill.memory: must look like 512m or 2g")
	}
	if v := a.HeartbeatURLEnv; v != "" && !reEnvVar.MatchString(v) {
		add("heartbeat_url_env: invalid environment variable name")
	}
	if h := a.Hosted; h != nil {
		if h.Server == "" {
			add("hosted.server: required")
		}
		if h.TokenEnv == "" {
			add("hosted.token_env: required")
		}
	}
	if sc := a.Schedule; sc == nil {
		add("schedule: required")
	} else {
		for _, p := range sc.problems() {
			add("schedule.%s", p)
		}
	}
	if rt := a.Runtime; rt == nil {
		add("runtime: required")
	} else {
		if rt.User == "" {
			add("runtime.user: required")
		}
		if rt.Path == "" {
			add("runtime.path: required")
		}
		if !filepath.IsAbs(rt.Docker) {
			add("runtime.docker: required, an absolute path")
		}
	}
	if len(errs) > 0 {
		return &ValidationError{Errs: errs}
	}
	return nil
}

// Load reads and validates app.yaml from disk. app.yaml is written only by
// init, so a file that does not read is made again: the error ends
// with `Fix: bracer remove <app>, then bracer init <app>` (ADR-0022, ADR-0023).
func Load(path string) (*App, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	a, err := Parse(data)
	var ve *ValidationError
	if errors.As(err, &ve) {
		app := AppOf(path)
		ve.Fix = "bracer remove " + app + ", then bracer init " + app
	}
	return a, err
}

// Marshal writes App as YAML.
func Marshal(a *App) ([]byte, error) {
	return yaml.MarshalWithOptions(a, yaml.IndentSequence(true))
}

// Save writes app.yaml atomically: write a temp file in the same directory, then
// rename, so a half-written file is never left behind (spec init, Ctrl-C edge case).
func Save(path string, a *App) error {
	if err := a.Validate(); err != nil {
		return err
	}
	data, err := Marshal(a)
	if err != nil {
		return err
	}
	return writeAtomic(path, data, 0o644)
}

// writeAtomic writes through a temp file, then renames. If the file already exists,
// its mode is kept and, when running as root (sudo), so is its owner, so another
// user's cron can still read it (spec rotate-key § Edge cases).
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	old, oldErr := os.Stat(path)
	if oldErr == nil {
		perm = old.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if oldErr == nil && os.Geteuid() == 0 {
		if uid, gid, ok := fileOwner(old); ok {
			_ = tmp.Chown(uid, gid)
		}
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// EnvVarName names a variable as BRACER_<APP>_<PART>_<SUFFIX>: uppercased, any
// character outside [A-Z0-9] becomes `_` (spec init § Data model).
func EnvVarName(parts ...string) string {
	var b strings.Builder
	b.WriteString("BRACER")
	for _, p := range parts {
		b.WriteByte('_')
		for _, r := range strings.ToUpper(p) {
			if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
			} else {
				b.WriteByte('_')
			}
		}
	}
	return b.String()
}

// ExpandHome replaces a leading `~/` in the path with the home directory.
func ExpandHome(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, p[1:]), nil
	}
	return p, nil
}

// ErrMissingSecret reports a variable missing from secrets_file.
var ErrMissingSecret = errors.New("missing secret")

// FindInstance returns the instance by name, nil if there is none.
func (a *App) FindInstance(name string) *Instance {
	for i := range a.Instances {
		if a.Instances[i].Name == name {
			return &a.Instances[i]
		}
	}
	return nil
}

// bitnamiDrill says why a Bitnami image cannot be the drill image.
func bitnamiDrill(image string) string {
	return image + " is not supported as the drill image: Bitnami's entrypoint runs a temporary server that already listens on TCP, so Bracer cannot tell when the real server is up; use the official postgres image or one built on it (postgis/postgis, timescale/timescaledb)"
}

// IsBitnamiImage recognises Bitnami images (`bitnami/postgresql*`,
// `bitnamilegacy/postgresql*`, with or without a registry in front, tag or
// digest). Not supported as drill.image: Bitnami's entrypoint runs a temporary
// server that listens on TCP, so drill cannot tell it from the real server (spec drill § Edge cases).
func IsBitnamiImage(image string) bool {
	ref := image
	// Drop the tag/digest: only the part after the last "/" can hold the tag's ":" or "@".
	start := strings.LastIndex(ref, "/") + 1
	if j := strings.IndexAny(ref[start:], ":@"); j >= 0 {
		ref = ref[:start+j]
	}
	parts := strings.Split(ref, "/")
	if len(parts) < 2 {
		return false
	}
	org, repo := parts[len(parts)-2], parts[len(parts)-1]
	return (org == "bitnami" || org == "bitnamilegacy") && strings.HasPrefix(repo, "postgresql")
}

// SaveDraft writes app.yaml as a draft next to the real one (mode 600):
// init proves it before it replaces app.yaml (ADR-0019).
func SaveDraft(path string, a *App) error {
	if err := a.Validate(); err != nil {
		return err
	}
	data, err := Marshal(a)
	if err != nil {
		return err
	}
	_ = os.Remove(path)
	return writeAtomic(path, data, 0o600)
}
