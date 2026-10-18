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
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

// App is the whole of app.yaml.
type App struct {
	App             string     `yaml:"app"`
	SecretsFile     string     `yaml:"secrets_file,omitempty"`
	Instances       []Instance `yaml:"instances"`
	Storage         *Storage   `yaml:"storage,omitempty"`
	Encryption      string     `yaml:"encryption,omitempty"` // "", none, age
	AgeRecipient    string     `yaml:"age_recipient,omitempty"`
	AgeIdentityFile string     `yaml:"age_identity_file,omitempty"`
	Retention       *Retention `yaml:"retention,omitempty"`
	RowCounts       string     `yaml:"row_counts,omitempty"`     // "", auto, exact, estimate
	ExactMaxRows    int64      `yaml:"exact_max_rows,omitempty"` // auto: reltuples threshold below which count(*) is used
	Hosted          *Hosted    `yaml:"hosted,omitempty"`
	Backup          *Backup    `yaml:"backup,omitempty"`
	Drill           *Drill     `yaml:"drill,omitempty"`
}

// Backup is the `bracer backup` configuration (spec backup § Data model). Every field is optional.
type Backup struct {
	Timeout     string `yaml:"timeout,omitempty"`      // per database: count + pg_dump + upload; default 6h
	LockTimeout string `yaml:"lock_timeout,omitempty"` // maximum lock wait (pg_dump --lock-wait-timeout, session lock_timeout); default 1m
}

// DefaultBackupTimeout and DefaultLockTimeout apply when backup.* is empty.
const (
	DefaultBackupTimeout = 6 * time.Hour
	DefaultLockTimeout   = time.Minute
)

// BackupTimeout returns backup.timeout with the default applied; a malformed string is already rejected by Validate.
func (a *App) BackupTimeout() time.Duration {
	if a.Backup == nil {
		return DefaultBackupTimeout
	}
	return durationOr(a.Backup.Timeout, DefaultBackupTimeout)
}

// BackupLockTimeout returns backup.lock_timeout with the default applied.
func (a *App) BackupLockTimeout() time.Duration {
	if a.Backup == nil {
		return DefaultLockTimeout
	}
	return durationOr(a.Backup.LockTimeout, DefaultLockTimeout)
}

func durationOr(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

var reDockerMem = regexp.MustCompile(`^[0-9]+[bkmgBKMG]?$`)

// Drill is the `bracer drill` configuration (spec drill § Data model). Every field is optional.
type Drill struct {
	Runtime   string  `yaml:"runtime,omitempty"`   // "", docker, kube: where the temporary Postgres runs; empty picks automatically
	Namespace string  `yaml:"namespace,omitempty"` // runtime kube: namespace of the temporary pod; default "default"
	Image     string  `yaml:"image,omitempty"`     // default postgres:<major from the manifest>-alpine
	Timeout   string  `yaml:"timeout,omitempty"`   // per database, time.Duration; default 6h
	Memory    string  `yaml:"memory,omitempty"`    // docker run --memory / pod memory limit, e.g. 2g
	CPUs      string  `yaml:"cpus,omitempty"`      // docker run --cpus / pod cpu limit, e.g. "1"
	Checks    []Check `yaml:"checks,omitempty"`
}

// Drill runtimes (drill.runtime).
const (
	DrillRuntimeDocker = "docker"
	DrillRuntimeKube   = "kube"
)

// DefaultDrillNamespace is the namespace of the temporary pod when drill.namespace is empty.
const DefaultDrillNamespace = "default"

// DrillNamespace returns drill.namespace with the default applied.
func (a *App) DrillNamespace() string {
	if a.Drill == nil || a.Drill.Namespace == "" {
		return DefaultDrillNamespace
	}
	return a.Drill.Namespace
}

// DrillRuntime returns drill.runtime ("" when it is to be picked automatically).
func (a *App) DrillRuntime() string {
	if a.Drill == nil {
		return ""
	}
	return a.Drill.Runtime
}

// reDNSLabel is a Kubernetes namespace name (RFC 1123 label).
var reDNSLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Check is one SQL statement run on the restored database; ≥ 1 row is a pass.
type Check struct {
	Name      string   `yaml:"name"`
	SQL       string   `yaml:"sql"`
	Databases []string `yaml:"databases,omitempty"` // empty: every database
}

// DefaultDrillTimeout is the per-database restore timeout when drill.timeout is empty.
const DefaultDrillTimeout = 6 * time.Hour

// DrillTimeout returns drill.timeout with the default applied; a malformed string is already rejected by Validate.
func (a *App) DrillTimeout() time.Duration {
	if a.Drill == nil || a.Drill.Timeout == "" {
		return DefaultDrillTimeout
	}
	d, err := time.ParseDuration(a.Drill.Timeout)
	if err != nil || d <= 0 {
		return DefaultDrillTimeout
	}
	return d
}

// AppliesTo reports whether the check runs on this database.
func (c Check) AppliesTo(db string) bool {
	if len(c.Databases) == 0 {
		return true
	}
	for _, d := range c.Databases {
		if d == db {
			return true
		}
	}
	return false
}

// row_counts values (spec backup).
const (
	RowCountsAuto     = "auto"
	RowCountsExact    = "exact"
	RowCountsEstimate = "estimate"

	DefaultExactMaxRows int64 = 1_000_000
)

// RowCountsMode returns row_counts with the default applied.
func (a *App) RowCountsMode() string {
	if a.RowCounts == "" {
		return RowCountsAuto
	}
	return a.RowCounts
}

// ExactMaxRowsOrDefault returns exact_max_rows with the default applied.
func (a *App) ExactMaxRowsOrDefault() int64 {
	if a.ExactMaxRows <= 0 {
		return DefaultExactMaxRows
	}
	return a.ExactMaxRows
}

// Instance is one Postgres server and how to connect to it. Exactly one of
// Container | Pod | Socket | Host+Port | URLEnv must be set (Port may accompany
// Socket). → ADR-0009
type Instance struct {
	Name         string    `yaml:"name"`
	Container    string    `yaml:"container,omitempty"`
	Pod          string    `yaml:"pod,omitempty"`    // ns/pod[/container], reached with kubectl exec
	Socket       string    `yaml:"socket,omitempty"` // directory holding .s.PGSQL.<port> on this machine
	Host         string    `yaml:"host,omitempty"`
	Port         int       `yaml:"port,omitempty"`
	URLEnv       string    `yaml:"url_env,omitempty"`
	User         string    `yaml:"user,omitempty"`
	PasswordEnv  string    `yaml:"password_env,omitempty"`
	PasswordFile string    `yaml:"password_file,omitempty"`
	PgMajor      int       `yaml:"pg_major,omitempty"`
	Databases    Databases `yaml:"databases"`
}

// Databases is `all` or a list of databases.
type Databases struct {
	All  bool
	List []Database
}

// Database is the unit of pg_dump, with an optional retention override.
type Database struct {
	Name      string     `yaml:"name"`
	Retention *Retention `yaml:"retention,omitempty"`
}

// Storage is the customer's S3-compatible bucket.
type Storage struct {
	Endpoint     string `yaml:"endpoint"`
	Bucket       string `yaml:"bucket"`
	Prefix       string `yaml:"prefix,omitempty"`
	Region       string `yaml:"region,omitempty"`
	AccessKeyEnv string `yaml:"access_key_env"`
	SecretKeyEnv string `yaml:"secret_key_env"`
	PathStyle    bool   `yaml:"path_style,omitempty"`
}

// Retention is the number of backups kept per database.
type Retention struct {
	Keep int `yaml:"keep"`
}

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
	KindPod
	KindSocket
	KindHost
	KindURL
)

// Kind returns the connection method; KindInvalid unless exactly one is set.
func (in Instance) Kind() Kind {
	n := 0
	k := KindInvalid
	if in.Container != "" {
		n++
		k = KindContainer
	}
	if in.Pod != "" {
		n++
		k = KindPod
	}
	if in.Socket != "" {
		n++
		k = KindSocket
	}
	if in.Host != "" || (in.Port != 0 && in.Socket == "") {
		n++
		k = KindHost
	}
	if in.URLEnv != "" {
		n++
		k = KindURL
	}
	if n != 1 {
		return KindInvalid
	}
	return k
}

// PodRef splits `ns/pod[/container]`. ok=false if malformed.
func (in Instance) PodRef() (namespace, pod, container string, ok bool) {
	parts := strings.Split(in.Pod, "/")
	if len(parts) < 2 || len(parts) > 3 {
		return "", "", "", false
	}
	for _, p := range parts {
		if p == "" {
			return "", "", "", false
		}
	}
	if len(parts) == 3 {
		container = parts[2]
	}
	return parts[0], parts[1], container, true
}

// socketFilePrefix is the Postgres socket file name.
const socketFilePrefix = ".s.PGSQL."

// SocketDir returns the socket directory and port. If `socket:` names the
// `.s.PGSQL.N` file itself, it is split into directory + N; an empty port is 5432.
func (in Instance) SocketDir() (dir string, port int) {
	dir, port = in.Socket, in.Port
	if base := filepath.Base(dir); strings.HasPrefix(base, socketFilePrefix) {
		if n, err := strconv.Atoi(strings.TrimPrefix(base, socketFilePrefix)); err == nil {
			port = n
		}
		dir = filepath.Dir(dir)
	}
	if port == 0 {
		port = 5432
	}
	return dir, port
}

// databaseEntry accepts both the short form (string) and the full form (map).
type databaseEntry Database

func (d *databaseEntry) UnmarshalYAML(b []byte) error {
	var s string
	if err := yaml.Unmarshal(b, &s); err == nil {
		*d = databaseEntry{Name: s}
		return nil
	}
	var full struct {
		Name      string     `yaml:"name"`
		Retention *Retention `yaml:"retention,omitempty"`
	}
	if err := yaml.UnmarshalWithOptions(b, &full, yaml.Strict()); err != nil {
		return err
	}
	*d = databaseEntry(full)
	return nil
}

// UnmarshalYAML accepts `all` or a list.
func (ds *Databases) UnmarshalYAML(b []byte) error {
	if strings.TrimSpace(string(b)) == "all" {
		*ds = Databases{All: true}
		return nil
	}
	var list []databaseEntry
	if err := yaml.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("databases: expected `all` or a list: %w", err)
	}
	ds.All = false
	ds.List = ds.List[:0]
	for _, e := range list {
		ds.List = append(ds.List, Database(e))
	}
	return nil
}

// MarshalYAML writes `all`, or a list using the short form when there is no override.
func (ds Databases) MarshalYAML() (interface{}, error) {
	if ds.All {
		return "all", nil
	}
	out := make([]interface{}, 0, len(ds.List))
	for _, d := range ds.List {
		if d.Retention == nil {
			out = append(out, d.Name)
		} else {
			out = append(out, d)
		}
	}
	return out, nil
}

// ValidationError collects every error of one file, each with its field path.
type ValidationError struct {
	Errs []string
}

func (e *ValidationError) Error() string {
	return "app.yaml: " + strings.Join(e.Errs, "; ")
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
		return nil, fmt.Errorf("app.yaml: %s", yaml.FormatError(err, false, true))
	}
	if err := app.Validate(); err != nil {
		return nil, err
	}
	return &app, nil
}

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
		for _, k := range []string{"password", "url"} {
			if _, ok := in[k]; ok {
				errs = append(errs, fmt.Sprintf("instances[%d].%s: secrets must not be written to app.yaml, use %s_env", i, k, k))
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
		switch in.Kind() {
		case KindInvalid:
			add("%s: exactly one of container | pod | socket | host+port | url_env is required", p)
		case KindPod:
			if _, _, _, ok := in.PodRef(); !ok {
				add("%s.pod: expected namespace/pod or namespace/pod/container", p)
			}
			if in.PasswordEnv != "" || in.PasswordFile != "" {
				add("%s: a pod is reached with kubectl exec over its Unix socket; passwords cannot be passed, drop password_env/password_file", p)
			}
		case KindSocket:
			if !filepath.IsAbs(in.Socket) {
				add("%s.socket: must be an absolute path to the socket directory (e.g. /var/run/postgresql)", p)
			}
			if base := filepath.Base(in.Socket); strings.HasPrefix(base, socketFilePrefix) && in.Port != 0 {
				if _, sp := in.SocketDir(); sp != in.Port {
					add("%s.port: differs from the port in the socket file name", p)
				}
			}
			if in.Port != 0 && (in.Port < 1 || in.Port > 65535) {
				add("%s.port: must be 1–65535", p)
			}
		case KindHost:
			if in.Host == "" {
				add("%s.host: required when port is set", p)
			}
			if in.Port < 1 || in.Port > 65535 {
				add("%s.port: must be 1–65535", p)
			}
		case KindURL:
			if in.PasswordEnv != "" || in.PasswordFile != "" || in.User != "" {
				add("%s: url_env already carries user/password; drop user/password_env/password_file", p)
			}
		}
		if in.PasswordEnv != "" && in.PasswordFile != "" {
			add("%s: only one of password_env | password_file", p)
		}
		for field, v := range map[string]string{"password_env": in.PasswordEnv, "url_env": in.URLEnv} {
			if v == "" {
				continue
			}
			if !reEnvVar.MatchString(v) {
				add("%s.%s: invalid environment variable name", p, field)
			}
			if owner, dup := envOwner[v]; dup {
				add("%s.%s: variable %s is already used by %s (two instances cannot share a variable)", p, field, v, owner)
			} else {
				envOwner[v] = p
			}
		}
		if in.PgMajor != 0 && (in.PgMajor < 10 || in.PgMajor > 30) {
			add("%s.pg_major: implausible value", p)
		}
		if !in.Databases.All && len(in.Databases.List) == 0 {
			add("%s.databases: expected `all` or at least one database", p)
		}
		dbNames := map[string]bool{}
		for j, d := range in.Databases.List {
			dp := fmt.Sprintf("%s.databases[%d]", p, j)
			if d.Name == "" {
				add("%s.name: required", dp)
			} else if dbNames[d.Name] {
				add("%s.name: duplicated within the instance", dp)
			}
			dbNames[d.Name] = true
			if d.Retention != nil && d.Retention.Keep < 1 {
				add("%s.retention.keep: must be ≥ 1", dp)
			}
		}
	}
	switch a.Encryption {
	case "", "none":
		if a.AgeRecipient != "" || a.AgeIdentityFile != "" {
			add("encryption: is none but age_recipient/age_identity_file are set")
		}
	case "age":
		if !strings.HasPrefix(a.AgeRecipient, "age1") {
			add("age_recipient: expected an age1… public key")
		}
	default:
		add("encryption: must be none | age")
	}
	if s := a.Storage; s != nil {
		if s.Endpoint == "" {
			add("storage.endpoint: required")
		}
		if s.Bucket == "" {
			add("storage.bucket: required")
		}
		for field, v := range map[string]string{"access_key_env": s.AccessKeyEnv, "secret_key_env": s.SecretKeyEnv} {
			if v == "" {
				add("storage.%s: required", field)
			} else if !reEnvVar.MatchString(v) {
				add("storage.%s: invalid environment variable name", field)
			}
		}
	}
	if a.Retention != nil && a.Retention.Keep < 1 {
		add("retention.keep: must be ≥ 1")
	}
	switch a.RowCounts {
	case "", RowCountsAuto, RowCountsExact, RowCountsEstimate:
	default:
		add("row_counts: must be auto, exact or estimate")
	}
	if a.ExactMaxRows < 0 {
		add("exact_max_rows: must be ≥ 0")
	}
	if b := a.Backup; b != nil {
		for name, v := range map[string]string{"timeout": b.Timeout, "lock_timeout": b.LockTimeout} {
			if v != "" {
				if d, err := time.ParseDuration(v); err != nil || d <= 0 {
					add("backup.%s: must be a positive duration such as 30s, 1m or 6h", name)
				}
			}
		}
	}
	if dr := a.Drill; dr != nil {
		if dr.Runtime != "" && dr.Runtime != DrillRuntimeDocker && dr.Runtime != DrillRuntimeKube {
			add("drill.runtime: must be docker or kube (empty picks automatically)")
		}
		if dr.Namespace != "" && (len(dr.Namespace) > 63 || !reDNSLabel.MatchString(dr.Namespace)) {
			add("drill.namespace: must be a Kubernetes namespace name (lowercase letters, digits and -)")
		}
		if IsBitnamiImage(dr.Image) {
			add("drill.image: %s is not supported as the drill image: Bitnami's entrypoint runs a temporary server that already listens on TCP, so Bracer cannot tell when the real server is up; use the official postgres image or one built on it (postgis/postgis, timescale/timescaledb)", dr.Image)
		}
		if dr.Memory != "" && !reDockerMem.MatchString(dr.Memory) {
			add("drill.memory: must look like 512m or 2g")
		}
		if dr.CPUs != "" {
			if f, err := strconv.ParseFloat(dr.CPUs, 64); err != nil || f <= 0 {
				add("drill.cpus: must be a positive number such as 1 or 0.5")
			}
		}
		if dr.Timeout != "" {
			if d, err := time.ParseDuration(dr.Timeout); err != nil || d <= 0 {
				add("drill.timeout: must be a positive duration such as 30m or 6h")
			}
		}
		for i, c := range dr.Checks {
			if c.Name == "" {
				add("drill.checks[%d].name: required", i)
			}
			if strings.TrimSpace(c.SQL) == "" {
				add("drill.checks[%d].sql: required", i)
			}
		}
	}
	if h := a.Hosted; h != nil {
		if h.Server == "" {
			add("hosted.server: required")
		}
		if h.TokenEnv == "" {
			add("hosted.token_env: required")
		}
	}
	if len(errs) > 0 {
		return &ValidationError{Errs: errs}
	}
	return nil
}

// Load reads and validates app.yaml from disk.
func Load(path string) (*App, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
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

// ErrMissingSecret reports a variable missing from all three sources.
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
