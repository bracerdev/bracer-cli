package restore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"bracer.dev/bracer/internal/pg"
)

// Database-level attributes (owner, ALTER DATABASE … SET, ALTER ROLE … IN
// DATABASE … SET, comment, privileges) live in archive entries pg_restore only
// applies with --create. Bracer creates the database itself, so it reads those
// entries from the archive's start and applies them after the data
// (spec fetch-restore § Flow 5b).

// MaxTOC is how much of the archive's start is kept in memory: the table of
// contents sits there and these entries need no data.
const MaxTOC = 32 << 20

// noSchema matches no schema: with -n every object is filtered out, while
// pg_restore still emits the database's own entries because they follow
// --create, not the selection filters.
const noSchema = "bracer_no_such_schema"

// Head reads up to MaxTOC bytes of r and returns them with a reader that
// replays them followed by the rest of r.
func Head(r io.Reader) ([]byte, io.Reader, error) {
	head, err := io.ReadAll(io.LimitReader(r, MaxTOC))
	if err != nil {
		return nil, nil, err
	}
	return head, io.MultiReader(bytes.NewReader(head), r), nil
}

// DatabaseSQL asks pg_restore for the SQL of the database's own entries.
// extra carries --no-owner / --no-acl. No connection is made; pg_restore 17
// needs -f - to print to stdout.
func DatabaseSQL(ctx context.Context, runner pg.Runner, conn pg.Conn, head []byte, extra []string) ([]byte, error) {
	var out bytes.Buffer
	args := append([]string{"--create", "-n", noSchema, "-f", "-"}, extra...)
	stderr, err := pg.RunCapture(ctx, runner, conn, "pg_restore", args, bytes.NewReader(head), &out)
	if err != nil {
		msg := Tail(stderr, 5)
		if msg == "" {
			msg = err.Error()
		}
		if len(head) >= MaxTOC {
			msg += fmt.Sprintf(" (the archive's table of contents may be larger than %d MB)", MaxTOC>>20)
		}
		if !strings.HasPrefix(msg, "pg_restore:") {
			msg = "pg_restore: " + msg
		}
		return nil, errors.New(msg)
	}
	return out.Bytes(), nil
}

// Setting is one ALTER DATABASE … SET (Role empty) or ALTER ROLE … IN DATABASE … SET.
type Setting struct {
	Role  string
	Name  string
	Value string // tokens joined, literals unquoted
	SQL   string // the value as written after TO, for rebuilding the statement
}

type aclStmt struct {
	grant       bool
	optionOnly  bool // REVOKE GRANT OPTION FOR: privileges stay
	privileges  []string
	grantees    []string
	description string
}

// Attributes is what the archive says about its database, rewritten for the
// target name and filtered against the roles on the target.
type Attributes struct {
	SourceDB   string
	Target     string
	Statements []string // to run after the data
	Owner      string   // set by Statements; empty: left to whoever created the database
	Settings   []Setting
	ACL        bool // Statements carry GRANT/REVOKE on the database
	Comment    *string
	Warnings   []string

	acl []aclStmt
}

// AttrResult is the outcome after applying and checking (`--json`: "attributes").
type AttrResult struct {
	Owner      string   `json:"owner,omitempty"`
	Applied    int      `json:"applied"`
	Mismatches []string `json:"attribute_mismatches,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// OK when every attribute read back matches the archive.
func (r *AttrResult) OK() bool { return r == nil || (r.Error == "" && len(r.Mismatches) == 0) }

// ParseDatabaseSQL keeps the statements listed in spec § Flow 5b, rebuilt from
// tokens with the source name swapped for target. roles are the names on the
// target; a statement naming a missing role is dropped with a warning.
func ParseDatabaseSQL(sql []byte, target string, roles map[string]bool) (*Attributes, error) {
	stmts, err := splitStatements(string(sql))
	if err != nil {
		return nil, err
	}
	a := &Attributes{Target: target}
	skip := func(why string, toks []token) {
		a.Warnings = append(a.Warnings, fmt.Sprintf("database attributes: not applied, %s: %s", describe(toks), why))
	}
	for _, st := range stmts {
		if st.meta {
			continue // \connect, \restrict
		}
		toks, err := tokenize(st.text)
		if err != nil || len(toks) == 0 {
			a.Warnings = append(a.Warnings, fmt.Sprintf("database attributes: could not read a statement (line %d), not applied", st.line))
			continue
		}
		switch {
		case toks[0].isKeyword("SET"), toks[0].isKeyword("RESET"), toks[0].isKeyword("SELECT"):
			continue // session setup, SET/RESET SESSION AUTHORIZATION
		case len(toks) >= 3 && toks[0].isKeyword("CREATE") && toks[1].isKeyword("DATABASE"):
			a.SourceDB = toks[2].ident()
			continue
		case len(toks) >= 2 && toks[0].isKeyword("SECURITY") && toks[1].isKeyword("LABEL"):
			skip("security labels are not restored", toks)
			continue
		}
		if a.SourceDB == "" {
			skip("comes before CREATE DATABASE", toks)
			continue
		}
		switch {
		case len(toks) >= 4 && toks[0].isKeyword("ALTER") && toks[1].isKeyword("DATABASE"):
			if !a.isSource(toks[2]) {
				skip("names another database", toks)
				continue
			}
			toks[2] = token{kind: tokQuoted, val: target}
			switch {
			case len(toks) == 6 && toks[3].isKeyword("OWNER") && toks[4].isKeyword("TO"):
				role := toks[5].ident()
				if !roles[role] {
					a.Warnings = append(a.Warnings, fmt.Sprintf("database owner %s does not exist on the target; the app user will not be able to create objects in schema public (PostgreSQL 15+)\nFix: create the user, then: ALTER DATABASE %s OWNER TO %s;", role, pg.QuoteIdent(target), pg.QuoteIdent(role)))
					continue
				}
				a.Owner = role
			case toks[3].isKeyword("SET"):
				s, ok := parseSetting(toks[4:])
				if !ok {
					skip("unexpected SET form", toks)
					continue
				}
				a.Settings = append(a.Settings, s)
			case len(toks) >= 5 && toks[3].isKeyword("CONNECTION") && toks[4].isKeyword("LIMIT"):
			case toks[3].isKeyword("IS_TEMPLATE") && a.SourceDB == "template1" && target == "template1":
				// template1 restored into template1 stays the template it is.
			case toks[3].isKeyword("IS_TEMPLATE"), toks[3].isKeyword("ALLOW_CONNECTIONS"):
				skip("the restored copy is not made a template nor closed to connections; set it yourself if needed", toks)
				continue
			default:
				skip("unknown ALTER DATABASE form", toks)
				continue
			}
		case len(toks) >= 7 && toks[0].isKeyword("ALTER") && (toks[1].isKeyword("ROLE") || toks[1].isKeyword("USER")) &&
			toks[3].isKeyword("IN") && toks[4].isKeyword("DATABASE") && toks[6].isKeyword("SET"):
			if !a.isSource(toks[5]) {
				skip("names another database", toks)
				continue
			}
			role := toks[2].ident()
			if !roles[role] {
				skip("user "+role+" does not exist on the target", toks)
				continue
			}
			toks[5] = token{kind: tokQuoted, val: target}
			s, ok := parseSetting(toks[7:])
			if !ok {
				skip("unexpected SET form", toks)
				continue
			}
			s.Role = role
			a.Settings = append(a.Settings, s)
		case len(toks) >= 6 && toks[0].isKeyword("COMMENT") && toks[1].isKeyword("ON") && toks[2].isKeyword("DATABASE") && toks[4].isKeyword("IS"):
			if !a.isSource(toks[3]) {
				skip("names another database", toks)
				continue
			}
			toks[3] = token{kind: tokQuoted, val: target}
			if toks[5].kind == tokLiteral {
				c := unquoteLiteral(toks[5].val)
				a.Comment = &c
			}
		case toks[0].isKeyword("GRANT") || toks[0].isKeyword("REVOKE"):
			acl, at, err := parseACL(toks)
			if err != nil {
				skip(err.Error(), toks)
				continue
			}
			if !a.isSource(toks[at]) {
				skip("names another database", toks)
				continue
			}
			missing := ""
			for _, g := range acl.grantees {
				if g != "PUBLIC" && !roles[g] {
					missing = g
				}
			}
			if missing != "" {
				skip("user "+missing+" does not exist on the target", toks)
				continue
			}
			toks[at] = token{kind: tokQuoted, val: target}
			a.acl = append(a.acl, acl)
			a.ACL = true
		default:
			skip("not a database attribute bracer restores", toks)
			continue
		}
		a.Statements = append(a.Statements, rebuild(toks))
	}
	if a.SourceDB == "" {
		return nil, errors.New("the archive has no CREATE DATABASE entry")
	}
	return a, nil
}

// isSource: the token names the archive's database (pg_dump quotes it as needed).
func (a *Attributes) isSource(t token) bool {
	return (t.kind == tokWord || t.kind == tokQuoted) && t.ident() == a.SourceDB
}

// parseSetting reads `name TO|= value[, value…]`; name may be dotted (app.settings.x).
func parseSetting(toks []token) (Setting, bool) {
	var name strings.Builder
	i := 0
	for ; i < len(toks); i++ {
		t := toks[i]
		if t.isKeyword("TO") || (t.kind == tokPunct && t.val == "=") {
			break
		}
		if t.kind == tokPunct && t.val != "." {
			return Setting{}, false
		}
		name.WriteString(t.val)
	}
	if name.Len() == 0 || i >= len(toks)-1 {
		return Setting{}, false
	}
	var val strings.Builder
	for _, t := range toks[i+1:] {
		switch t.kind {
		case tokLiteral:
			val.WriteString(unquoteLiteral(t.val))
		default:
			val.WriteString(t.val)
		}
	}
	return Setting{Name: name.String(), Value: val.String(), SQL: rebuild(toks[i+1:])}, true
}

// unquoteLiteral turns '…' (or E'…') into its text.
func unquoteLiteral(s string) string {
	if strings.HasPrefix(s, "E") || strings.HasPrefix(s, "e") {
		s = s[1:]
	}
	if len(s) >= 2 {
		s = s[1 : len(s)-1]
	}
	return strings.ReplaceAll(s, "''", "'")
}

var dbPrivileges = []string{"CREATE", "CONNECT", "TEMPORARY"}

// parseACL reads GRANT|REVOKE [GRANT OPTION FOR] privs ON DATABASE db TO|FROM
// grantees [WITH GRANT OPTION] [CASCADE]; at is the index of db.
func parseACL(toks []token) (aclStmt, int, error) {
	acl := aclStmt{grant: toks[0].isKeyword("GRANT")}
	i := 1
	if !acl.grant && len(toks) > 4 && toks[1].isKeyword("GRANT") && toks[2].isKeyword("OPTION") && toks[3].isKeyword("FOR") {
		acl.optionOnly = true
		i = 4
	}
	on := indexKeyword(toks, "ON")
	if on < 0 || on+3 >= len(toks) || !toks[on+1].isKeyword("DATABASE") {
		return acl, 0, errors.New("not a privilege on a database")
	}
	for _, t := range toks[i:on] {
		switch {
		case t.kind == tokPunct && t.val == ",":
		case t.isKeyword("ALL"):
			acl.privileges = append(acl.privileges, dbPrivileges...)
		case t.isKeyword("PRIVILEGES"):
		case t.isKeyword("TEMP"), t.isKeyword("TEMPORARY"):
			acl.privileges = append(acl.privileges, "TEMPORARY")
		case t.isKeyword("CREATE"), t.isKeyword("CONNECT"):
			acl.privileges = append(acl.privileges, strings.ToUpper(t.val))
		default:
			return acl, 0, fmt.Errorf("unknown database privilege %s", t.val)
		}
	}
	at := on + 2
	if !(acl.grant && toks[at+1].isKeyword("TO")) && !(!acl.grant && toks[at+1].isKeyword("FROM")) {
		return acl, 0, errors.New("unexpected privilege form")
	}
	for _, t := range toks[at+2:] {
		switch {
		case t.isKeyword("WITH"), t.isKeyword("CASCADE"), t.isKeyword("RESTRICT"), t.isKeyword("GRANTED"):
			return acl, at, nil
		case t.kind == tokPunct && t.val == ",":
		case t.isKeyword("PUBLIC"):
			acl.grantees = append(acl.grantees, "PUBLIC")
		case t.isKeyword("GROUP"):
		case t.kind == tokWord || t.kind == tokQuoted:
			acl.grantees = append(acl.grantees, t.ident())
		default:
			return acl, 0, errors.New("unexpected grantee")
		}
	}
	return acl, at, nil
}

// describe shows a statement for a warning: at most 8 tokens, literals masked
// (a setting such as app.settings.jwt_secret may hold a secret).
func describe(toks []token) string {
	n := len(toks)
	if n > 8 {
		n = 8
	}
	shown := make([]token, n)
	for i, t := range toks[:n] {
		if t.kind == tokLiteral {
			t = token{kind: tokWord, val: "'…'"}
		}
		shown[i] = t
	}
	out := rebuild(shown)
	if n < len(toks) {
		out += " …"
	}
	return out
}

// Apply runs the statements on the target's postgres database, each on its own
// (ON_ERROR_STOP=0: a setting of an extension not loaded there does not take
// the owner with it). Errors come back as warnings.
func (a *Attributes) Apply(ctx context.Context, runner pg.Runner, conn pg.Conn) (applied int, warnings []string) {
	if len(a.Statements) == 0 {
		return 0, nil
	}
	conn.Database = "postgres"
	sql := strings.Join(a.Statements, ";\n") + ";\n"
	stderr, err := pg.RunCapture(ctx, runner, conn, "psql", []string{"-X", "-q", "-v", "ON_ERROR_STOP=0", "-f", "-"}, strings.NewReader(sql), io.Discard)
	failed := 0
	for _, line := range strings.Split(string(stderr), "\n") {
		if strings.Contains(line, "ERROR:") {
			failed++
			warnings = append(warnings, "database attributes: "+strings.TrimSpace(line))
		}
	}
	if err != nil && failed == 0 {
		warnings = append(warnings, "database attributes: psql: "+firstLineOf(err.Error()))
		return 0, warnings
	}
	return len(a.Statements) - failed, warnings
}

// Verify reads owner, settings and privileges back from the target and lists
// what differs from the archive. admin is connected to any database there.
func (a *Attributes) Verify(ctx context.Context, admin *pg.Client) ([]string, error) {
	db := pg.QuoteLiteral(a.Target)
	var out []string
	rows, err := admin.Query(ctx, "select pg_get_userbyid(datdba) from pg_database where datname = "+db)
	if err != nil {
		return nil, fmt.Errorf("read owner: %w", err)
	}
	if len(rows) == 0 || len(rows[0]) == 0 {
		return nil, fmt.Errorf("database %s not found", a.Target)
	}
	owner := rows[0][0]
	if a.Owner != "" && owner != a.Owner {
		out = append(out, fmt.Sprintf("owner is %s, the archive has %s", owner, a.Owner))
	}

	if len(a.Settings) > 0 {
		rows, err = admin.Query(ctx, "select coalesce(r.rolname, ''), c from pg_db_role_setting s join pg_database d on d.oid = s.setdatabase left join pg_roles r on r.oid = s.setrole, unnest(s.setconfig) c where d.datname = "+db)
		if err != nil {
			return nil, fmt.Errorf("read settings: %w", err)
		}
		have := map[string]string{}
		for _, r := range rows {
			if len(r) < 2 {
				continue
			}
			name, val, _ := strings.Cut(r[1], "=")
			have[r[0]+"\x00"+strings.ToLower(name)] = val
		}
		for _, s := range a.Settings {
			who := ""
			if s.Role != "" {
				who = " for user " + s.Role
			}
			got, ok := have[s.Role+"\x00"+strings.ToLower(s.Name)]
			switch {
			case !ok:
				out = append(out, fmt.Sprintf("setting %s%s is missing", s.Name, who))
			case normSetting(got) != normSetting(s.Value):
				out = append(out, fmt.Sprintf("setting %s%s is %q, the archive has %q", s.Name, who, got, s.Value))
			}
		}
	}

	if a.ACL {
		rows, err = admin.Query(ctx, "select case when a.grantee = 0 then 'PUBLIC' else pg_get_userbyid(a.grantee) end, a.privilege_type from pg_database d, aclexplode(coalesce(d.datacl, acldefault('d', d.datdba))) a where d.datname = "+db)
		if err != nil {
			return nil, fmt.Errorf("read privileges: %w", err)
		}
		got := map[string]bool{}
		for _, r := range rows {
			if len(r) >= 2 {
				got[r[0]+" "+r[1]] = true
			}
		}
		want := a.expectedACL(owner)
		if g, w := privList(got), privList(want); g != w {
			out = append(out, fmt.Sprintf("privileges are %s, the archive has %s", g, w))
		}
	}
	return out, nil
}

// expectedACL replays the GRANT/REVOKE statements on acldefault for a database:
// the owner holds every privilege, PUBLIC has CONNECT and TEMPORARY.
func (a *Attributes) expectedACL(owner string) map[string]bool {
	acl := map[string]bool{}
	for _, p := range dbPrivileges {
		acl[owner+" "+p] = true
	}
	acl["PUBLIC CONNECT"], acl["PUBLIC TEMPORARY"] = true, true
	for _, s := range a.acl {
		if s.optionOnly {
			continue
		}
		for _, g := range s.grantees {
			for _, p := range s.privileges {
				if s.grant {
					acl[g+" "+p] = true
				} else {
					delete(acl, g+" "+p)
				}
			}
		}
	}
	return acl
}

// privList: "PUBLIC: CONNECT; shop_app: CONNECT, CREATE, TEMPORARY".
func privList(set map[string]bool) string {
	by := map[string][]string{}
	for k := range set {
		g, p := splitACL(k)
		by[g] = append(by[g], p)
	}
	var names []string
	for g := range by {
		names = append(names, g)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, g := range names {
		sort.Strings(by[g])
		parts[i] = g + ": " + strings.Join(by[g], ", ")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "; ")
}

// normSetting compares a stored setting with the archive's: pg_dump splits list
// settings into literals ('$user', 'public') while the catalog keeps "$user", public.
func normSetting(s string) string {
	return strings.ToLower(strings.NewReplacer(" ", "", `"`, "", "'", "").Replace(s))
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Finish applies the attributes and reads them back (after the data and the
// table comparison). conn is the target server; any database is fine.
func (a *Attributes) Finish(ctx context.Context, runner pg.Runner, conn pg.Conn) (*AttrResult, []string) {
	res := &AttrResult{Owner: a.Owner}
	applied, warnings := a.Apply(ctx, runner, conn)
	res.Applied = applied
	admin := &pg.Client{Runner: runner, Conn: conn}
	admin.Conn.Database = "postgres"
	mm, err := a.Verify(ctx, admin)
	if err != nil {
		res.Error = "check database attributes: " + err.Error()
	}
	res.Mismatches = mm
	return res, warnings
}

// PrepareAttributes is the part before the data: read the database's entries
// from head and filter them against the target's roles. A failure is not fatal
// (the data is still restored): it comes back as a warning and nil.
func PrepareAttributes(ctx context.Context, runner pg.Runner, conn pg.Conn, head []byte, target string, extra []string) (*Attributes, []string) {
	sql, err := DatabaseSQL(ctx, runner, conn, head, extra)
	if err != nil {
		return nil, []string{"database attributes (owner, settings, privileges) not applied: " + err.Error()}
	}
	admin := &pg.Client{Runner: runner, Conn: conn}
	admin.Conn.Database = "postgres"
	names, err := ListRoles(ctx, admin)
	if err != nil {
		return nil, []string{"database attributes not applied: list users on target: " + err.Error()}
	}
	roles := map[string]bool{}
	for _, n := range names {
		roles[n] = true
	}
	a, err := ParseDatabaseSQL(sql, target, roles)
	if err != nil {
		return nil, []string{"database attributes not applied: " + err.Error()}
	}
	return a, a.Warnings
}

// SettingsOnly keeps only ALTER DATABASE … SET (how the data reads: TimeZone,
// DateStyle) for a preview, where owner, privileges and users are not restored.
func (a *Attributes) SettingsOnly() {
	prefix := "ALTER DATABASE " + pg.QuoteIdent(a.Target) + " SET "
	var kept []string
	for _, st := range a.Statements {
		if strings.HasPrefix(st, prefix) {
			kept = append(kept, st)
		}
	}
	var settings []Setting
	for _, st := range a.Settings {
		if st.Role == "" {
			settings = append(settings, st)
		}
	}
	a.Statements, a.Settings, a.Owner, a.ACL, a.acl = kept, settings, "", false, nil
}
