package restore

import (
	"context"
	"fmt"
	"strings"

	"bracer.dev/bracer/internal/pg"
)

// TargetState is everything restore reads from a target before changing
// anything (ADR-0015 § 3). Plan compares it with the backup run.
type TargetState struct {
	Kind      string // container | local | url
	Container string
	Local     *pg.Local
	Env       *pg.Env
	Major     int
	Roles     *TargetRoles // full mode only
	HBA       []HBARule    // nil: not readable
	Avail     map[string]ExtAvail
	// PkgLib lists the files in the server's library directory; nil when it
	// cannot be read (not a superuser).
	PkgLib map[string]bool
	// TSData lists SHAREDIR/tsearch_data; nil when it cannot be read.
	TSData map[string]bool
	// Collations are the pg_catalog collations of the maintenance database
	// (the ones a new database gets): name → actual version.
	Collations map[string]string
	AutoConf   map[string]string // postgresql.auto.conf now; nil when not readable
	// Locks sizes the lock table; nil when the settings cannot be read.
	Locks *pg.LockCapacity
	// DBs are the databases restore loaded into, read after the load.
	DBs map[string]*TargetDB
	// Names is every database on the target.
	Names map[string]bool
	// CreateDB: the connected user may create databases.
	CreateDB bool
	// Effective are the settings in effect for the connected user in the
	// maintenance database (data mode).
	Effective []pg.Setting
	LocaleOK  map[string]string // localeKey → "" when the target can create it, else the error
}

// ExtAvail is an extension the target's server has files for.
type ExtAvail struct {
	Default string
	Trusted bool
}

// HBARule is one line of pg_hba.conf as pg_hba_file_rules reads it.
type HBARule struct {
	Type, Method     string
	Databases, Users []string
}

// TargetDB is a database restore loaded into.
type TargetDB struct {
	Name        string
	Exists      bool
	Objects     int
	Locale      *pg.Locale
	Owner       string
	Settings    map[string]string // role "\x00" lowercase name → value as stored; role "" for the database
	SettingName map[string]string // same key → name as stored
	ACL         map[string]bool   // "grantee privilege" with grantee PUBLIC or a role
	ACLNull     bool              // datacl is NULL: the default privileges
	Comment     *string
	Effective   []pg.Setting // data mode: settings in effect in this database for the connected user
}

// TargetSpec says where the target is, for what SQL cannot say.
type TargetSpec struct {
	Kind      string
	Container string
	Local     *pg.Local
	Shell     pg.Shell
	Password  bool // the socket asked for a password: the printed commands pass PGPASSWORD
}

// Wants is what a target is read for: the source locales to try (databases
// and created collations).
type Wants struct {
	Locales []*pg.Locale
}

// ReadTarget reads the target through admin (connected to its maintenance
// database). dbs are the databases restore loaded into: none before the load.
func ReadTarget(ctx context.Context, admin *pg.Client, spec TargetSpec, full bool, dbs []string, wants Wants) (*TargetState, error) {
	locales := wants.Locales
	env, _, err := pg.CollectEnv(ctx, admin, pg.EnvSource{Kind: spec.Kind, Container: spec.Container, Local: spec.Local, Shell: spec.Shell}, nil)
	if err != nil {
		return nil, err
	}
	st := &TargetState{Kind: spec.Kind, Container: spec.Container, Local: spec.Local, Env: env, Major: env.ServerVersionNum / 10000,
		DBs: map[string]*TargetDB{}, Names: map[string]bool{}, LocaleOK: map[string]string{}}
	if full {
		if st.Roles, err = ReadTargetRoles(ctx, admin, st.Major); err != nil {
			return nil, err
		}
	}
	if env.Superuser {
		if rows, err := admin.Query(ctx, "select type, database::text, user_name::text, auth_method from pg_hba_file_rules where error is null order by line_number"); err == nil {
			for _, r := range rows {
				if len(r) == 4 {
					st.HBA = append(st.HBA, HBARule{Type: r[0], Databases: pgArray(r[1]), Users: pgArray(r[2]), Method: r[3]})
				}
			}
		}
		st.PkgLib = listDir(ctx, admin, "PKGLIBDIR", "")
		st.TSData = listDir(ctx, admin, "SHAREDIR", "/tsearch_data")
		if ac, err := admin.AutoConfSettings(ctx); err == nil {
			st.AutoConf = ac
		}
	}
	if l, err := admin.LockCapacity(ctx); err == nil {
		st.Locks = l
	}
	rows, err := admin.Query(ctx, "select collname, coalesce(pg_collation_actual_version(oid), '') from pg_collation where collnamespace = 'pg_catalog'::regnamespace")
	if err != nil {
		return nil, fmt.Errorf("read collations: %w", err)
	}
	st.Collations = map[string]string{}
	for _, r := range rows {
		if len(r) == 2 {
			st.Collations[r[0]] = r[1]
		}
	}
	sql := "select name, coalesce(default_version, ''), false from pg_available_extensions"
	if st.Major >= 13 {
		sql = "select e.name, coalesce(e.default_version, ''), coalesce(v.trusted, false) from pg_available_extensions e left join pg_available_extension_versions v on v.name = e.name and v.version = e.default_version"
	}
	rows, err = admin.Query(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("read available extensions: %w", err)
	}
	st.Avail = map[string]ExtAvail{}
	for _, r := range rows {
		if len(r) == 3 {
			st.Avail[r[0]] = ExtAvail{Default: r[1], Trusted: r[2] == "t"}
		}
	}
	rows, err = admin.Query(ctx, "select datname from pg_database")
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	for _, r := range rows {
		st.Names[r[0]] = true
	}
	priv, err := admin.AdminPrivileges(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the privileges of %s: %w", env.CurrentUser, err)
	}
	st.CreateDB = priv.CreateDB
	if !full {
		if st.Effective, err = admin.EffectiveSettings(ctx); err != nil {
			return nil, fmt.Errorf("read the settings in effect: %w", err)
		}
	}
	for _, n := range dbs {
		td, err := readTargetDB(ctx, admin, n, st.Names[n], st.Major, !full)
		if err != nil {
			return nil, err
		}
		st.DBs[n] = td
	}
	for _, l := range locales {
		if l == nil {
			continue
		}
		k := localeKey(l)
		if _, done := st.LocaleOK[k]; done {
			continue
		}
		st.LocaleOK[k] = tryLocale(ctx, admin, l)
	}
	return st, nil
}

func readTargetDB(ctx context.Context, admin *pg.Client, name string, exists bool, major int, effective bool) (*TargetDB, error) {
	td := &TargetDB{Name: name, Exists: exists, Settings: map[string]string{}, SettingName: map[string]string{}, ACL: map[string]bool{}}
	if !exists {
		return td, nil
	}
	lit := pg.QuoteLiteral(name)
	rows, err := admin.Query(ctx, "select pg_get_userbyid(datdba), datacl is null, coalesce(shobj_description(oid, 'pg_database'), E'\\x01'), datallowconn from pg_database where datname = "+lit)
	if err != nil || len(rows) != 1 || len(rows[0]) != 4 {
		return nil, fmt.Errorf("read database %s: %v", name, err)
	}
	td.Owner, td.ACLNull = rows[0][0], rows[0][1] == "t"
	allowConn := rows[0][3] == "t"
	if c := rows[0][2]; c != "\x01" {
		td.Comment = &c
	}
	if td.Locale, err = admin.DatabaseLocale(ctx, name, major); err != nil {
		return nil, fmt.Errorf("read locale of %s: %w", name, err)
	}
	rows, err = admin.Query(ctx, "select coalesce(r.rolname, ''), c from pg_db_role_setting s join pg_database d on d.oid = s.setdatabase left join pg_roles r on r.oid = s.setrole, unnest(s.setconfig) c where d.datname = "+lit)
	if err != nil {
		return nil, fmt.Errorf("read settings of %s: %w", name, err)
	}
	for _, r := range rows {
		n, v, _ := strings.Cut(r[1], "=")
		k := r[0] + "\x00" + strings.ToLower(n)
		td.Settings[k], td.SettingName[k] = v, n
	}
	rows, err = admin.Query(ctx, "select case when a.grantee = 0 then 'PUBLIC' else pg_get_userbyid(a.grantee) end, a.privilege_type from pg_database d, aclexplode(coalesce(d.datacl, acldefault('d', d.datdba))) a where d.datname = "+lit)
	if err != nil {
		return nil, fmt.Errorf("read privileges of %s: %w", name, err)
	}
	for _, r := range rows {
		td.ACL[r[0]+" "+r[1]] = true
	}
	if effective && allowConn {
		if td.Effective, err = admin.WithDB(name).EffectiveSettings(ctx); err != nil {
			return nil, fmt.Errorf("read settings in %s: %w", name, err)
		}
	}
	return td, nil
}

// listDir lists a directory under a pg_config directory (superuser); nil
// when it cannot be read.
func listDir(ctx context.Context, admin *pg.Client, dir, sub string) map[string]bool {
	rows, err := admin.Query(ctx, "select f from pg_ls_dir((select setting from pg_config where name = "+pg.QuoteLiteral(dir)+") || "+pg.QuoteLiteral(sub)+") f")
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, r := range rows {
		out[r[0]] = true
	}
	return out
}

// tryLocale creates a collation with the locale in a transaction that rolls
// back: the only sure way to know a server has it ("" when it does).
func tryLocale(ctx context.Context, admin *pg.Client, l *pg.Locale) string {
	var opts string
	switch l.Provider {
	case "icu":
		opts = "provider = icu, locale = " + pg.QuoteLiteral(l.Locale)
	case "builtin":
		opts = "provider = builtin, locale = " + pg.QuoteLiteral(l.Locale)
	default:
		opts = "provider = libc, lc_collate = " + pg.QuoteLiteral(l.Collate) + ", lc_ctype = " + pg.QuoteLiteral(l.Ctype)
	}
	if _, err := admin.Query(ctx, "begin; create collation pg_temp.bracer_locale_check ("+opts+"); rollback"); err != nil {
		return firstLineOf(strings.TrimSpace(strings.TrimPrefix(errText(err), "ERROR:")))
	}
	return ""
}

func localeKey(l *pg.Locale) string {
	return strings.Join([]string{l.Provider, l.Collate, l.Ctype, l.Locale, l.ICURules}, "\x00")
}

// errText is psql's stderr when there is one.
func errText(err error) string {
	if ee, ok := err.(*pg.ExitError); ok && len(ee.Stderr) > 0 {
		s := string(ee.Stderr)
		if i := strings.Index(s, "ERROR:"); i >= 0 {
			s = s[i:]
		}
		return strings.TrimSpace(s)
	}
	return err.Error()
}

// pgArray splits a text[] literal such as {all} or {app,"a b"}.
func pgArray(s string) []string {
	s = strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}")
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		out = append(out, strings.Trim(p, `"`))
	}
	return out
}
