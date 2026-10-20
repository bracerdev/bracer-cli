package pg

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Env is what restore needs to know whether a target is enough for the app:
// the environment file of one backup run (ADR-0015, spec backup § environment
// file). The same reader describes a restore target, so the two compare
// field by field.
type Env struct {
	Schema             int       `json:"schema"`
	SourceKind         string    `json:"source_kind"` // container | local | url
	TS                 string    `json:"ts,omitempty"`
	ServerVersionNum   int       `json:"server_version_num"`
	ServerVersion      string    `json:"server_version"`
	BootstrapSuperuser string    `json:"bootstrap_superuser,omitempty"` // role with oid 10
	CurrentUser        string    `json:"current_user,omitempty"`
	Superuser          bool      `json:"superuser"` // the connected user is one
	PreloadLibraries   *string   `json:"shared_preload_libraries,omitempty"`
	HasFileSettings    bool      `json:"has_file_settings"`
	Settings           []Setting `json:"settings"`
	// Modules are the settings of loaded modules (dotted names: cron.database_name),
	// server-level. A backup keeps those not at their default; a target reads all,
	// so restore compares those whose module the target has loaded.
	Modules []Setting `json:"module_settings,omitempty"`
	// DBSettings are the values in effect in each database for the connected
	// user (server + database + role): what a URL source can tell (data mode).
	DBSettings     map[string][]Setting `json:"db_settings,omitempty"`
	SSL            bool                 `json:"ssl"`
	MaxConnections int                  `json:"max_connections,omitempty"`
	ClusterName    string               `json:"cluster_name,omitempty"` // Debian: "18/main"
	Tablespaces    []Tablespace         `json:"tablespaces,omitempty"`
	Container      *ContainerEnv        `json:"container,omitempty"`
	Local          *LocalEnv            `json:"local,omitempty"`
}

// EnvSchema is the version of the environment file format.
const EnvSchema = 1

// Setting is one setting that changes what queries do (ADR-0015 § 2).
type Setting struct {
	Name    string `json:"name"`
	Value   string `json:"value"` // in the base unit
	Unit    string `json:"unit,omitempty"`
	Vartype string `json:"vartype,omitempty"`
	Custom  bool   `json:"custom,omitempty"` // a placeholder such as app.tenant, only in configuration files
	// Source and Context are read from the target only, to tell how to fix a difference.
	Source  string `json:"-"`
	Context string `json:"-"`
}

// Tablespace is a tablespace other than pg_default/pg_global.
type Tablespace struct {
	Name       string `json:"name"`
	HasObjects bool   `json:"has_objects"`
}

// ContainerEnv is what docker inspect says about the source container (never its password).
type ContainerEnv struct {
	Name           string   `json:"name,omitempty"`
	Image          string   `json:"image"`
	ImageID        string   `json:"image_id,omitempty"`
	Digest         string   `json:"digest,omitempty"`
	Cmd            []string `json:"cmd,omitempty"`
	ComposeProject string   `json:"compose_project,omitempty"`
	ComposeService string   `json:"compose_service,omitempty"`
	PostgresUser   string   `json:"postgres_user,omitempty"`
	PostgresDB     string   `json:"postgres_db,omitempty"`
	Networks       []string `json:"networks,omitempty"`
}

// LocalEnv is what the host says about a local cluster.
type LocalEnv struct {
	SocketDir string    `json:"socket_dir"`
	Port      int       `json:"port"`
	OSRelease string    `json:"os_release,omitempty"`
	Packages  []Package `json:"packages,omitempty"`
}

// Package is the Debian package holding an extension's control file.
type Package struct {
	Extension string `json:"extension"`
	Package   string `json:"package"`
	Version   string `json:"version,omitempty"` // for reference only: Ubuntu and PGDG number them differently
}

// SettingsFilter is the pg_settings rows compared (ADR-0015 § 2): statement
// behaviour, locale and formatting, compatibility. client_encoding is set by
// every client, allow_alter_system is not about queries, transaction_* only
// mirror default_transaction_* inside a session (ALTER SYSTEM refuses them),
// and dotted names are options of a loaded module (preload covers them).
const SettingsFilter = `(category in ('Client Connection Defaults / Statement Behavior', 'Client Connection Defaults / Locale and Formatting') or category like 'Version and Platform Compatibility%') and name not in ('client_encoding', 'allow_alter_system', 'transaction_isolation', 'transaction_read_only', 'transaction_deferrable') and name not like '%.%'`

// SessionOnly: parameters that mirror default_transaction_* inside a session.
// Backups made before they were filtered out still list them.
func SessionOnly(name string) bool {
	switch strings.ToLower(name) {
	case "transaction_isolation", "transaction_read_only", "transaction_deferrable":
		return true
	}
	return false
}

const moduleSettingsSQL = "select name, setting, coalesce(unit, ''), vartype, source, coalesce(boot_val, ''), context from pg_settings where name like '%.%' order by name"

// NonDefaultModules keeps the module settings a backup records: those the
// server does not have at their built-in value.
func NonDefaultModules(all []Setting) []Setting {
	var out []Setting
	for _, s := range all {
		if s.Source != "default" {
			out = append(out, s)
		}
	}
	return out
}

// Module returns the named module setting, nil if the module is not loaded.
func (e *Env) Module(name string) *Setting {
	for i := range e.Modules {
		if strings.EqualFold(e.Modules[i].Name, name) {
			return &e.Modules[i]
		}
	}
	return nil
}

const settingsSQL = "select name, setting, coalesce(unit, ''), vartype, source, coalesce(boot_val, ''), context from pg_settings where " + SettingsFilter + " order by name"

// serverSources are the pg_settings sources whose value is the server's own;
// any other source (database, user, client, session) masks it in this session.
var serverSources = map[string]bool{"default": true, "configuration file": true, "command line": true, "environment variable": true, "override": true}

// EnvSource tells CollectEnv where the server is, for what SQL cannot say.
type EnvSource struct {
	Kind      string // container | local | url
	Container string // docker inspect of this container
	Local     *Local
	Shell     Shell
}

// CollectEnv reads the environment of the connected server. full: read what
// needs a superuser (configuration files, tablespaces); otherwise per-database
// effective values for dbs. Missing optional parts come back as warnings.
func CollectEnv(ctx context.Context, c *Client, src EnvSource, dbs []string) (*Env, []string, error) {
	var warns []string
	e := &Env{Schema: EnvSchema, SourceKind: src.Kind}
	num, _, err := c.ServerVersionNum(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read server_version_num: %w", err)
	}
	e.ServerVersionNum = num
	if e.ServerVersion, err = c.ServerVersion(ctx); err != nil {
		return nil, nil, err
	}
	rows, err := c.Query(ctx, "select (select rolname from pg_roles where oid = 10), current_user, (select rolsuper from pg_roles where rolname = current_user), current_setting('ssl'), current_setting('max_connections'), current_setting('cluster_name')")
	if err != nil || len(rows) != 1 || len(rows[0]) != 6 {
		return nil, nil, fmt.Errorf("read bootstrap superuser: %v", err)
	}
	e.BootstrapSuperuser, e.CurrentUser, e.Superuser = rows[0][0], rows[0][1], rows[0][2] == "t"
	e.SSL = rows[0][3] == "on"
	e.MaxConnections, _ = strconv.Atoi(rows[0][4])
	e.ClusterName = rows[0][5]
	if libs, err := c.PreloadLibraries(ctx); err == nil {
		e.PreloadLibraries = &libs
	} else {
		warns = append(warns, "cannot read shared_preload_libraries; fix: GRANT pg_read_all_settings TO "+QuoteIdent(e.CurrentUser))
	}

	if src.Container != "" && src.Shell != nil {
		ce, err := (Docker{Shell: src.Shell}).InspectEnv(ctx, src.Container)
		if err != nil {
			return nil, nil, fmt.Errorf("docker inspect %s: %w", src.Container, err)
		}
		e.Container = ce
	}
	var cmdSettings map[string]string
	if e.Container != nil {
		cmdSettings = CmdSettings(e.Container.Cmd)
	}

	rows, err = c.Query(ctx, settingsSQL)
	if err != nil {
		return nil, nil, fmt.Errorf("read settings: %w", err)
	}
	file, ferr := fileSettings(ctx, c)
	e.HasFileSettings = ferr == nil
	serverLevel := func(r []string) (Setting, error) {
		if len(r) != 7 {
			return Setting{}, fmt.Errorf("unexpected pg_settings row %q", r)
		}
		s := Setting{Name: r[0], Value: r[1], Unit: r[2], Vartype: r[3], Source: r[4], Context: r[6]}
		if !serverSources[r[4]] {
			// This session's value is masked by a database, role or client setting.
			switch v, ok := cmdSettings[strings.ToLower(s.Name)]; {
			case ok:
				s.Value = NormalizeValue(v, s.Unit, s.Vartype)
			case file[strings.ToLower(s.Name)] != "":
				s.Value = NormalizeValue(file[strings.ToLower(s.Name)], s.Unit, s.Vartype)
			default:
				s.Value = r[5]
			}
		}
		return s, nil
	}
	for _, r := range rows {
		s, err := serverLevel(r)
		if err != nil {
			return nil, nil, err
		}
		e.Settings = append(e.Settings, s)
	}
	rows, err = c.Query(ctx, moduleSettingsSQL)
	if err != nil {
		return nil, nil, fmt.Errorf("read module settings: %w", err)
	}
	for _, r := range rows {
		s, err := serverLevel(r)
		if err != nil {
			return nil, nil, err
		}
		if len(r) == 7 && s.Value == r[5] {
			s.Source = "default"
		}
		e.Modules = append(e.Modules, s)
	}
	if ferr == nil {
		known := map[string]bool{}
		names, err := c.Query(ctx, "select lower(name) from pg_settings where name like '%.%'")
		if err != nil {
			return nil, nil, fmt.Errorf("read settings: %w", err)
		}
		for _, n := range names {
			known[n[0]] = true
		}
		custom := map[string]string{}
		for n, v := range file {
			if strings.Contains(n, ".") && !known[n] {
				custom[n] = v
			}
		}
		for n, v := range cmdSettings {
			if strings.Contains(n, ".") && !known[n] {
				custom[n] = v
			}
		}
		for _, n := range sortedKeys(custom) {
			e.Settings = append(e.Settings, Setting{Name: n, Value: custom[n], Custom: true})
		}
	}

	if e.Superuser {
		rows, err := c.Query(ctx, "select spcname, exists (select 1 from pg_tablespace_databases(oid)) from pg_tablespace where spcname not in ('pg_default', 'pg_global') order by 1")
		if err != nil {
			return nil, nil, fmt.Errorf("read tablespaces: %w", err)
		}
		for _, r := range rows {
			e.Tablespaces = append(e.Tablespaces, Tablespace{Name: r[0], HasObjects: r[1] == "t"})
		}
	}
	if src.Kind == "url" {
		e.DBSettings = map[string][]Setting{}
		for _, db := range dbs {
			s, err := c.WithDB(db).EffectiveSettings(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("read settings in %s: %w", db, err)
			}
			e.DBSettings[db] = s
		}
	}
	if src.Local != nil {
		e.Local = &LocalEnv{SocketDir: src.Local.Dir, Port: src.Local.Port, OSRelease: osRelease()}
	}
	return e, warns, nil
}

// EffectiveSettings are the compared settings as they are in this session:
// server, database and the connected role together.
func (c *Client) EffectiveSettings(ctx context.Context) ([]Setting, error) {
	rows, err := c.Query(ctx, settingsSQL)
	if err != nil {
		return nil, err
	}
	out := make([]Setting, 0, len(rows))
	for _, r := range rows {
		if len(r) != 7 {
			return nil, fmt.Errorf("unexpected pg_settings row %q", r)
		}
		out = append(out, Setting{Name: r[0], Value: r[1], Unit: r[2], Vartype: r[3], Source: r[4], Context: r[6]})
	}
	return out, nil
}

// fileSettings is the applied value of every name in the configuration files
// (postgresql.conf, conf.d, postgresql.auto.conf), lowercased names; superuser only.
func fileSettings(ctx context.Context, c *Client) (map[string]string, error) {
	rows, err := c.Query(ctx, "select lower(name), coalesce(setting, '') from pg_file_settings where error is null order by seqno")
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, r := range rows {
		if len(r) == 2 {
			out[r[0]] = r[1] // later lines win
		}
	}
	return out, nil
}

// AutoConfSettings is what postgresql.auto.conf (ALTER SYSTEM) holds now:
// name (lowercase) → value. env-down sets these back.
func (c *Client) AutoConfSettings(ctx context.Context) (map[string]string, error) {
	rows, err := c.Query(ctx, "select lower(name), coalesce(setting, '') from pg_file_settings where sourcefile like '%postgresql.auto.conf' and error is null order by seqno")
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, r := range rows {
		if len(r) == 2 {
			out[r[0]] = r[1]
		}
	}
	return out, nil
}

// CmdSettings reads `-c name=value` and `--name=value` from a postgres command
// line (the container's Cmd): name (lowercase) → value.
func CmdSettings(cmd []string) map[string]string {
	out := map[string]string{}
	for i := 0; i < len(cmd); i++ {
		a := cmd[i]
		var kv string
		switch {
		case a == "-c" && i+1 < len(cmd):
			kv = cmd[i+1]
			i++
		case strings.HasPrefix(a, "-c") && len(a) > 2:
			kv = a[2:]
		case strings.HasPrefix(a, "--") && strings.Contains(a, "="):
			kv = a[2:]
		default:
			continue
		}
		n, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		out[strings.ToLower(strings.ReplaceAll(strings.TrimSpace(n), "-", "_"))] = strings.Trim(strings.TrimSpace(v), `'"`)
	}
	return out
}

var memUnits = map[string]float64{"B": 1, "kB": 1024, "MB": 1 << 20, "GB": 1 << 30, "TB": 1 << 40}
var timeUnits = map[string]float64{"us": 0.001, "ms": 1, "s": 1000, "min": 60000, "h": 3600000, "d": 86400000}

// NormalizeValue writes a value from a configuration file or command line in
// the setting's base unit, as pg_settings.setting does ('30s' → '30000' for
// ms). Booleans become on/off. Anything it cannot read is returned trimmed.
func NormalizeValue(v, unit, vartype string) string {
	v = strings.TrimSpace(v)
	switch vartype {
	case "bool":
		switch strings.ToLower(v) {
		case "on", "true", "yes", "1", "t", "y":
			return "on"
		case "off", "false", "no", "0", "f", "n":
			return "off"
		}
		return v
	case "integer", "real":
	default:
		return v
	}
	i := 0
	for i < len(v) && (v[i] == '-' || v[i] == '+' || v[i] == '.' || (v[i] >= '0' && v[i] <= '9') || v[i] == 'e' || v[i] == 'E') {
		i++
	}
	num, err := strconv.ParseFloat(v[:i], 64)
	if err != nil {
		return v
	}
	suffix := strings.TrimSpace(v[i:])
	if suffix != "" && unit != "" {
		from, to, ok := unitFactor(suffix, unit)
		if !ok {
			return v
		}
		num = num * from / to
	}
	if vartype == "integer" {
		return strconv.FormatInt(int64(math.Round(num)), 10)
	}
	return strconv.FormatFloat(num, 'g', -1, 64)
}

// unitFactor returns the size of one suffix and one base unit in a common
// scale (bytes or ms). base may carry a multiplier ("8kB").
func unitFactor(suffix, base string) (from, to float64, ok bool) {
	mult := 1.0
	j := 0
	for j < len(base) && base[j] >= '0' && base[j] <= '9' {
		j++
	}
	if j > 0 {
		mult, _ = strconv.ParseFloat(base[:j], 64)
	}
	b := base[j:]
	if f, ok1 := memUnits[suffix]; ok1 {
		if t, ok2 := memUnits[b]; ok2 {
			return f, t * mult, true
		}
	}
	if f, ok1 := timeUnits[suffix]; ok1 {
		if t, ok2 := timeUnits[b]; ok2 {
			return f, t * mult, true
		}
	}
	return 0, 0, false
}

// SameValue compares two values of one setting: lc_* treat UTF-8 and utf8
// alike, lists ignore spaces after commas, enums ignore case.
func SameValue(name, vartype, a, b string) bool {
	if a == b {
		return true
	}
	n := strings.ToLower(name)
	norm := func(s string) string {
		s = strings.TrimSpace(s)
		s = strings.ReplaceAll(s, ", ", ",")
		if strings.HasPrefix(n, "lc_") {
			s = strings.ToLower(strings.ReplaceAll(s, "-", ""))
		}
		if vartype == "enum" || n == "datestyle" {
			s = strings.ToLower(s)
		}
		return s
	}
	if vartype == "real" {
		x, e1 := strconv.ParseFloat(a, 64)
		y, e2 := strconv.ParseFloat(b, 64)
		if e1 == nil && e2 == nil {
			return x == y
		}
	}
	return norm(a) == norm(b)
}

// InspectEnv reads what the environment file keeps about a container.
func (d Docker) InspectEnv(ctx context.Context, name string) (*ContainerEnv, error) {
	res, err := d.Shell.Run(ctx, Cmd{Name: "docker", Args: []string{"inspect", "--type", "container", name}})
	if err != nil {
		return nil, err
	}
	var entries []struct {
		Name   string `json:"Name"`
		Image  string `json:"Image"`
		Config struct {
			Image  string            `json:"Image"`
			Cmd    []string          `json:"Cmd"`
			Env    []string          `json:"Env"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		NetworkSettings struct {
			Networks map[string]json.RawMessage `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal(res.Stdout, &entries); err != nil || len(entries) != 1 {
		return nil, fmt.Errorf("unexpected docker inspect output")
	}
	e := entries[0]
	ce := &ContainerEnv{Name: strings.TrimPrefix(e.Name, "/"), Image: e.Config.Image, ImageID: e.Image, Cmd: e.Config.Cmd,
		ComposeProject: e.Config.Labels["com.docker.compose.project"], ComposeService: e.Config.Labels["com.docker.compose.service"]}
	for _, kv := range e.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "POSTGRES_USER":
			ce.PostgresUser = v
		case "POSTGRES_DB":
			ce.PostgresDB = v
		}
	}
	for n := range e.NetworkSettings.Networks {
		ce.Networks = append(ce.Networks, n)
	}
	sort.Strings(ce.Networks)
	if r, err := d.Shell.Run(ctx, Cmd{Name: "docker", Args: []string{"image", "inspect", "--format", "{{json .RepoDigests}}", e.Image}}); err == nil {
		var digests []string
		if json.Unmarshal(r.Stdout, &digests) == nil && len(digests) > 0 {
			ce.Digest = digests[0]
		}
	}
	return ce, nil
}

// osRelease is "<ID> <VERSION_ID>" from /etc/os-release, "" when unreadable.
func osRelease() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	kv := map[string]string{}
	for _, l := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(l, "=")
		if ok {
			kv[k] = strings.Trim(v, `"`)
		}
	}
	return strings.TrimSpace(kv["ID"] + " " + kv["VERSION_ID"])
}

// ExtensionPackages finds the Debian packages an extension needs on a local
// cluster of this major (dpkg-query -S): the one holding its control file and
// the one holding the library its module_pathname names ($libdir/postgis-3),
// which Debian may ship apart (postgresql-18-postgis-3-scripts and
// postgresql-18-postgis-3). Core packages (postgresql-<N>, postgresql-contrib)
// are left out: the server image has them. A file reached through
// update-alternatives (postgis) is looked up at its real path. Every extension
// is tried; the error names those not found.
func ExtensionPackages(ctx context.Context, sh Shell, major int, exts []string) ([]Package, error) {
	var out []Package
	var missing []string
	for _, ext := range exts {
		control := fmt.Sprintf("/usr/share/postgresql/%d/extension/%s.control", major, ext)
		paths := []string{control}
		res, err := sh.Run(ctx, Cmd{Name: "cat", Args: []string{control}})
		if err != nil {
			missing = append(missing, ext)
			continue
		}
		if lib := modulePathname(string(res.Stdout)); lib != "" {
			paths = append(paths, fmt.Sprintf("/usr/lib/postgresql/%d/lib/%s", major, lib))
		}
		var pkgs []string
		for _, path := range paths {
			pkg, ok := packageOf(ctx, sh, path)
			if !ok {
				pkgs = nil
				break
			}
			if !slices.Contains(pkgs, pkg) {
				pkgs = append(pkgs, pkg)
			}
		}
		if len(pkgs) == 0 {
			missing = append(missing, ext)
			continue
		}
		for _, pkg := range pkgs {
			if pkg == fmt.Sprintf("postgresql-%d", major) || strings.HasPrefix(pkg, "postgresql-contrib") {
				continue
			}
			p := Package{Extension: ext, Package: pkg}
			if r, err := sh.Run(ctx, Cmd{Name: "dpkg-query", Args: []string{"-W", "-f", "${Version}", pkg}}); err == nil {
				p.Version = strings.TrimSpace(string(r.Stdout))
			}
			out = append(out, p)
		}
	}
	if len(missing) > 0 {
		return out, fmt.Errorf("no Debian package holds the control file or library of %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// packageOf is the package dpkg-query -S names for a file, following an
// update-alternatives symlink to its real path.
func packageOf(ctx context.Context, sh Shell, path string) (string, bool) {
	res, err := sh.Run(ctx, Cmd{Name: "dpkg-query", Args: []string{"-S", path}})
	if err != nil {
		if r, rerr := sh.Run(ctx, Cmd{Name: "readlink", Args: []string{"-f", path}}); rerr == nil {
			if real := strings.TrimSpace(string(r.Stdout)); real != "" && real != path {
				res, err = sh.Run(ctx, Cmd{Name: "dpkg-query", Args: []string{"-S", real}})
			}
		}
	}
	if err != nil {
		return "", false
	}
	pkg, _, ok := strings.Cut(strings.TrimSpace(string(res.Stdout)), ":")
	if !ok {
		return "", false
	}
	return strings.TrimSpace(strings.Split(pkg, ",")[0]), true
}

// modulePathname is the library file a control file's module_pathname names
// under $libdir ("postgis-3.so"); empty when it names none.
func modulePathname(control string) string {
	for _, l := range strings.Split(control, "\n") {
		k, v, ok := strings.Cut(l, "=")
		if !ok || strings.TrimSpace(k) != "module_pathname" {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), "'")
		name, ok := strings.CutPrefix(v, "$libdir/")
		if !ok || name == "" || strings.Contains(name, "/") {
			return ""
		}
		if !strings.HasSuffix(name, ".so") {
			name += ".so"
		}
		return name
	}
	return ""
}

// Setting returns the named setting, nil if absent.
func (e *Env) Setting(name string) *Setting {
	for i := range e.Settings {
		if strings.EqualFold(e.Settings[i].Name, name) {
			return &e.Settings[i]
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RestartLocal is the command that restarts a local cluster: the Debian
// cluster by name when cluster_name says it ("18/main"), else the service.
func RestartLocal(e *Env) string {
	if e != nil {
		if ver, name, ok := strings.Cut(e.ClusterName, "/"); ok && ver != "" && name != "" && !strings.ContainsAny(e.ClusterName, " '\"$`;&|") {
			return "sudo pg_ctlcluster " + ver + " " + name + " restart"
		}
	}
	return "sudo systemctl restart postgresql"
}
