package pg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Client runs queries and checks through Runner using psql.
type Client struct {
	Runner Runner
	Conn   Conn
}

// Query runs one SQL statement with psql -At, returning rows with columns separated by \x1f.
func (c *Client) Query(ctx context.Context, sql string) ([][]string, error) {
	var out bytes.Buffer
	args := []string{"-X", "-A", "-t", "-q", "-v", "ON_ERROR_STOP=1", "-F", "\x1f", "-0", "-c", sql}
	if err := c.Runner.Run(ctx, c.Conn, "psql", args, nil, &out); err != nil {
		return nil, err
	}
	return parseRows(out.String()), nil
}

// parseRows splits the output of psql -A -t -F \x1f -0: each row ends with a
// NUL byte, columns are separated by \x1f. A value may hold newlines (a
// multi-line comment on a role or database), never a NUL.
func parseRows(out string) [][]string {
	var rows [][]string
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		rows = append(rows, strings.Split(rec, "\x1f"))
	}
	return rows
}

// scalarOrEmpty is scalar where an empty value is a valid answer.
func (c *Client) scalarOrEmpty(ctx context.Context, sql string) (string, error) {
	rows, err := c.Query(ctx, sql)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 || len(rows[0]) == 0 {
		return "", nil
	}
	return rows[0][0], nil
}

func (c *Client) scalar(ctx context.Context, sql string) (string, error) {
	rows, err := c.Query(ctx, sql)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 || len(rows[0]) == 0 {
		return "", fmt.Errorf("%s: no result", sql)
	}
	return rows[0][0], nil
}

// Ping tries to connect; the error keeps psql's stderr intact.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.scalar(ctx, "select 1")
	return err
}

// ServerVersionNum returns server_version_num (170011) and the major (17).
func (c *Client) ServerVersionNum(ctx context.Context) (num, major int, err error) {
	s, err := c.scalar(ctx, "show server_version_num")
	if err != nil {
		return 0, 0, err
	}
	num, err = strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, 0, fmt.Errorf("unexpected server_version_num: %q", s)
	}
	return num, num / 10000, nil
}

// SystemIdentifier returns system_identifier from pg_control_system(): set at
// initdb, unchanged by restarts, container recreation on the same volume and
// physical failover, so it tells whether the Postgres at a place is still the
// same cluster (spec backup § Flow 3a2). Callable by any role on PG14–18.
func (c *Client) SystemIdentifier(ctx context.Context) (string, error) {
	s, err := c.scalar(ctx, "select system_identifier from pg_control_system()")
	if err != nil {
		return "", err
	}
	s = strings.TrimSpace(s)
	if _, err := strconv.ParseUint(s, 10, 64); err != nil {
		return "", fmt.Errorf("unexpected system_identifier: %q", s)
	}
	return s, nil
}

// InitdbTime decodes the time of initdb from the high 32 bits of a
// system_identifier (seconds since the epoch); zero if it cannot be parsed.
func InitdbTime(systemID string) time.Time {
	n, err := strconv.ParseUint(systemID, 10, 64)
	if err != nil || n>>32 == 0 {
		return time.Time{}
	}
	return time.Unix(int64(n>>32), 0).UTC()
}

// ListCandidates lists the databases init offers to back up: every
// database that allows connections but template0, and template1 only when it
// holds objects (it is copied into every new database). ADR-0024
func (c *Client) ListCandidates(ctx context.Context) ([]string, error) {
	rows, err := c.Query(ctx, "select datname from pg_database where datallowconn and datname <> 'template0' order by datname")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		if len(r) != 1 {
			return nil, fmt.Errorf("pg_database: unexpected row %v", r)
		}
		name := r[0]
		if name == "template1" {
			used, err := TemplateInUse(ctx, c.WithDB(name))
			if err != nil {
				return nil, fmt.Errorf("check template1 for objects: %w", err)
			}
			if !used {
				continue
			}
		}
		out = append(out, name)
	}
	return out, nil
}

// TemplateInUse: the connected database (template1) holds objects of a user
// or an extension besides plpgsql.
func TemplateInUse(ctx context.Context, c *Client) (bool, error) {
	n, err := c.UserObjects(ctx)
	if err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	exts, err := c.Extensions(ctx)
	if err != nil {
		return false, err
	}
	return len(exts) > 0, nil
}

// Privileges are the privileges of the connected user. → ADR-0006
type Privileges struct {
	Role        string
	Superuser   bool
	ReadAllData bool
	BypassRLS   bool
}

// CanDump: superuser, or pg_read_all_data + BYPASSRLS.
func (p Privileges) CanDump() bool {
	return p.Superuser || (p.ReadAllData && p.BypassRLS)
}

// Privileges reads rolsuper, pg_read_all_data, rolbypassrls of current_user.
// pg_read_all_data exists only from PG14: pg_roles must be looked up first (CASE
// guarantees the order); calling pg_has_role directly on PG12/13 errors "role does not exist".
func (c *Client) Privileges(ctx context.Context) (Privileges, error) {
	rows, err := c.Query(ctx, "select current_user, rolsuper, case when exists (select rolname from pg_roles where rolname = 'pg_read_all_data') then pg_has_role(current_user,'pg_read_all_data','member') else false end, rolbypassrls from pg_roles where rolname = current_user")
	if err != nil {
		return Privileges{}, err
	}
	if len(rows) != 1 || len(rows[0]) != 4 {
		return Privileges{}, fmt.Errorf("pg_roles: unexpected result %v", rows)
	}
	r := rows[0]
	return Privileges{Role: r[0], Superuser: r[1] == "t", ReadAllData: r[2] == "t", BypassRLS: r[3] == "t"}, nil
}

// BootstrapSuperuser is the role created by initdb (oid 10): always a
// superuser and impossible to drop. pg_roles is readable by every user.
func (c *Client) BootstrapSuperuser(ctx context.Context) (string, error) {
	rows, err := c.Query(ctx, "select rolname from pg_roles where oid = 10")
	if err != nil {
		return "", err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return "", fmt.Errorf("pg_roles: unexpected result %v", rows)
	}
	return rows[0][0], nil
}

// AdminPrivileges are current_user's privileges to create databases/users, for the restore target.
type AdminPrivileges struct {
	Role       string
	Superuser  bool
	CreateDB   bool
	CreateRole bool
}

// AdminPrivileges reads rolsuper, rolcreatedb, rolcreaterole of current_user.
func (c *Client) AdminPrivileges(ctx context.Context) (AdminPrivileges, error) {
	rows, err := c.Query(ctx, "select current_user, rolsuper, rolcreatedb, rolcreaterole from pg_roles where rolname = current_user")
	if err != nil {
		return AdminPrivileges{}, err
	}
	if len(rows) != 1 || len(rows[0]) != 4 {
		return AdminPrivileges{}, fmt.Errorf("pg_roles: unexpected result %v", rows)
	}
	r := rows[0]
	return AdminPrivileges{Role: r[0], Superuser: r[1] == "t", CreateDB: r[2] == "t", CreateRole: r[3] == "t"}, nil
}

// ToolVersion runs `pg_dump --version` through the runner: the version of the
// tool actually used to dump (may differ from the server). An archive from
// pg_dump N can only be read by pg_restore ≥ N.
func (c *Client) ToolVersion(ctx context.Context) (version string, major int, err error) {
	var out bytes.Buffer
	if err := c.Runner.Run(ctx, c.Conn, "pg_dump", []string{"--version"}, nil, &out); err != nil {
		return "", 0, fmt.Errorf("pg_dump --version: %w", err)
	}
	v, m, ok := ParseToolVersion(out.String())
	if !ok {
		return "", 0, fmt.Errorf("pg_dump --version: unexpected output %q", strings.TrimSpace(out.String()))
	}
	return v, m, nil
}

var reVersion = regexp.MustCompile(`\(PostgreSQL\) (\d+)(?:\.(\d+))?`)

// ParseToolVersion parses the `--version` output of a Postgres tool.
func ParseToolVersion(out string) (version string, major int, ok bool) {
	m := reVersion.FindStringSubmatch(out)
	if m == nil {
		return "", 0, false
	}
	major, _ = strconv.Atoi(m[1])
	version = m[1]
	if m[2] != "" {
		version += "." + m[2]
	}
	return version, major, true
}

// MajorOf extracts the major from a version string "17.11" / "17.11 (Debian…)"; 0 if unparsable.
func MajorOf(version string) int {
	f := strings.FieldsFunc(version, func(r rune) bool { return r == '.' || r == ' ' })
	if len(f) == 0 {
		return 0
	}
	n, err := strconv.Atoi(f[0])
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// RLSTables lists the tables with row-level security enabled in the current database.
func (c *Client) RLSTables(ctx context.Context) ([]string, error) {
	rows, err := c.Query(ctx, "select n.nspname || '.' || c.relname from pg_class c join pg_namespace n on n.oid = c.relnamespace where c.relrowsecurity and c.relkind in ('r','p') order by 1")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r[0])
	}
	return out, nil
}

// SchemaOnlyDump runs pg_dump --schema-only into the void: the cheapest test
// of privileges and version.
func (c *Client) SchemaOnlyDump(ctx context.Context) error {
	return c.Runner.Run(ctx, c.Conn, "pg_dump", []string{"--schema-only", "--no-owner"}, nil, io.Discard)
}

// DetectPooler recognizes PgBouncer: version() mentions PgBouncer, or SHOW pool_mode works.
func (c *Client) DetectPooler(ctx context.Context) (bool, string) {
	if v, err := c.scalar(ctx, "select version()"); err == nil && strings.Contains(strings.ToLower(v), "pgbouncer") {
		return true, v
	}
	if m, err := c.scalar(ctx, "show pool_mode"); err == nil && m != "" {
		return true, "pool_mode=" + m
	}
	return false, ""
}

// FixRoleSQL is the sample SQL that creates the backup user when privileges are missing. → ADR-0006
// major is the server's major; before PG14 there is no pg_read_all_data, so only
// a superuser can dump every database.
func FixRoleSQL(role string, major int) string {
	if major > 0 && major < 14 {
		return fmt.Sprintf("-- PostgreSQL %d has no pg_read_all_data (added in 14): the backup user must be a superuser\nCREATE ROLE %s LOGIN PASSWORD '<password>' SUPERUSER;", major, role)
	}
	return fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '<password>' BYPASSRLS;\nGRANT pg_read_all_data TO %s;\nGRANT pg_read_all_settings TO %s;", role, role, role)
}

// PreloadLibraries reads shared_preload_libraries ("pg_stat_statements,timescaledb"
// → both names). Only a superuser or a member of pg_read_all_settings may read it;
// anyone else gets an error.
func (c *Client) PreloadLibraries(ctx context.Context) (string, error) {
	return c.scalarOrEmpty(ctx, "show shared_preload_libraries")
}

// SplitLibraries splits a shared_preload_libraries value into names.
func SplitLibraries(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		p = strings.Trim(strings.TrimSpace(p), `"`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// IsAuthError recognizes a psql authentication error so the password can be asked for.
func IsAuthError(err error) bool {
	var ee *ExitError
	if !errors.As(err, &ee) {
		return false
	}
	s := string(ee.Stderr)
	return strings.Contains(s, "password authentication failed") ||
		strings.Contains(s, "no password supplied") ||
		strings.Contains(s, "fe_sendauth") ||
		strings.Contains(s, "peer authentication failed") ||
		strings.Contains(s, "Ident authentication failed")
}

// WithDB returns a Client connected to another database, with the same Runner.
func (c *Client) WithDB(db string) *Client {
	return &Client{Runner: c.Runner, Conn: c.Conn.WithDatabase(db)}
}

// ServerVersion returns the `show server_version` string (17.11) to write into the manifest.
func (c *Client) ServerVersion(ctx context.Context) (string, error) {
	return c.scalar(ctx, "show server_version")
}

// Table is one table's metadata for the manifest. Rows is the exact count in
// the pg_dump snapshot (if any); RowsEst is reltuples, nil when never ANALYZEd.
// The manifest writes exactly one of the two: when Rows is set, RowsEst is dropped.
type Table struct {
	Schema  string `json:"schema"`
	Name    string `json:"name"`
	Rows    *int64 `json:"rows,omitempty"`
	RowsEst *int64 `json:"rows_est,omitempty"`
	Kind    string `json:"-"` // relkind: r plain, p partitioned parent (not counted; the parent's reltuples is meaningless)
	// Extension is the extension owning the table (pg_depend deptype e): contents are
	// managed by the extension at runtime; restore/drill only warn on a row-count mismatch.
	Extension string `json:"extension,omitempty"`
}

// Ident returns the quoted table name for use in SQL.
func (t Table) Ident() string {
	return QuoteIdent(t.Schema) + "." + QuoteIdent(t.Name)
}

// QuoteLiteral quotes a string as an SQL literal.
func QuoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// QuoteIdent quotes a Postgres identifier.
func QuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

const tablesSQL = "select n.nspname, c.relname, c.reltuples::bigint, c.relkind, coalesce(e.extname, '') from pg_class c join pg_namespace n on n.oid = c.relnamespace left join pg_depend d on d.classid = 'pg_class'::regclass and d.objid = c.oid and d.deptype = 'e' left join pg_extension e on e.oid = d.refobjid where c.relkind in ('r','p') and n.nspname not in ('pg_catalog','information_schema') and n.nspname not like 'pg_toast%' order by 1, 2"

// parseTables parses the tablesSQL result (3–5 columns: older tests return only 3).
func parseTables(rows [][]string) []Table {
	out := make([]Table, 0, len(rows))
	for _, r := range rows {
		if len(r) < 3 || len(r) > 5 {
			continue
		}
		t := Table{Schema: r[0], Name: r[1]}
		if len(r) >= 4 {
			t.Kind = r[3]
		}
		if len(r) == 5 {
			t.Extension = r[4]
		}
		if n, err := strconv.ParseInt(r[2], 10, 64); err == nil && n >= 0 && t.Kind != "p" {
			t.RowsEst = &n
		}
		out = append(out, t)
	}
	return out
}

// Tables lists plain and partitioned tables outside the system schemas.
func (c *Client) Tables(ctx context.Context) ([]Table, error) {
	rows, err := c.Query(ctx, tablesSQL)
	if err != nil {
		return nil, err
	}
	return parseTables(rows), nil
}
