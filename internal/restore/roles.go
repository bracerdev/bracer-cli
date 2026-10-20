package restore

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"bracer.dev/bracer/internal/pg"
)

// The users of a full backup (pg_dumpall --globals-only) are compared with the
// target: the target must have every one of them with the same or more
// (ADR-0015). Bracer writes the SQL that gets it there; it never runs it on a
// customer's server.

// roleFlags are the attributes pg_dumpall writes on every ALTER ROLE … WITH, in
// pg_roles column order.
var roleFlags = []string{"SUPERUSER", "INHERIT", "CREATEROLE", "CREATEDB", "LOGIN", "REPLICATION", "BYPASSRLS"}

// RoleSpec is one user as the backup's globals describe it, or as it is on a target.
type RoleSpec struct {
	Name       string
	Flags      map[string]bool // roleFlags → value
	ConnLimit  int             // -1: none
	ValidUntil string          // timestamptz text; "": none
	Password   string          // the hash literal as written, '…'; "": none
	Settings   map[string]roleSetting
	MemberOf   map[string]Membership
}

type roleSetting struct {
	Name, Value string // Value as pg_db_role_setting stores it
	SQL         string // the value part rebuilt from the globals: TO '…' or a list
}

// Membership is one GRANT role TO member with its options. Inherit and Set
// are nil when the source predates PostgreSQL 16 (they follow the member's
// INHERIT and are always on).
type Membership struct {
	Admin   bool
	Inherit *bool
	Set     *bool
}

// ParamGrant is GRANT SET | ALTER SYSTEM ON PARAMETER (PostgreSQL 15+).
type ParamGrant struct {
	Param, Privilege, Grantee string
	GrantOption               bool
}

func (p ParamGrant) key() string {
	return strings.ToLower(p.Param) + "\x00" + p.Privilege + "\x00" + p.Grantee
}

// RoleGlobals is the parsed globals dump: users in file order.
type RoleGlobals struct {
	Roles       map[string]*RoleSpec
	Order       []string
	ParamGrants []ParamGrant
}

func newRole(name string) *RoleSpec {
	return &RoleSpec{Name: name, Flags: map[string]bool{}, ConnLimit: -1, Settings: map[string]roleSetting{}, MemberOf: map[string]Membership{}}
}

// ParseGlobals reads the users out of pg_dumpall --globals-only. Statements it
// does not need (SET, tablespaces, comments) are skipped.
func ParseGlobals(data []byte) (*RoleGlobals, error) {
	stmts, err := splitStatements(string(data))
	if err != nil {
		return nil, fmt.Errorf("read globals: %w", err)
	}
	g := &RoleGlobals{Roles: map[string]*RoleSpec{}}
	role := func(name string) *RoleSpec {
		r := g.Roles[name]
		if r == nil {
			r = newRole(name)
			g.Roles[name] = r
			g.Order = append(g.Order, name)
		}
		return r
	}
	for _, st := range stmts {
		if st.meta {
			continue
		}
		toks, err := tokenize(st.text)
		if err != nil || len(toks) < 3 {
			continue
		}
		kw := func(i int, k string) bool { return i < len(toks) && toks[i].isKeyword(k) }
		isName := func(i int) bool { return i < len(toks) && (toks[i].kind == tokWord || toks[i].kind == tokQuoted) }
		switch {
		case kw(0, "CREATE") && (kw(1, "ROLE") || kw(1, "USER")) && isName(2):
			role(toks[2].ident())
		case kw(0, "ALTER") && (kw(1, "ROLE") || kw(1, "USER")) && isName(2) && kw(3, "WITH"):
			r := role(toks[2].ident())
			for i := 4; i < len(toks); i++ {
				t := toks[i]
				up := strings.ToUpper(t.val)
				switch {
				case t.kind != tokWord:
				case up == "PASSWORD":
					if i+1 < len(toks) && toks[i+1].kind == tokLiteral {
						r.Password = toks[i+1].val
					}
					i++
				case up == "CONNECTION" && kw(i+1, "LIMIT") && i+2 < len(toks):
					r.ConnLimit, _ = strconv.Atoi(toks[i+2].val)
					i += 2
				case up == "VALID" && kw(i+1, "UNTIL") && i+2 < len(toks):
					r.ValidUntil = unquoteLiteral(toks[i+2].val)
					i += 2
				default:
					for _, f := range roleFlags {
						if up == f {
							r.Flags[f] = true
						} else if up == "NO"+f {
							r.Flags[f] = false
						}
					}
				}
			}
		case kw(0, "ALTER") && (kw(1, "ROLE") || kw(1, "USER")) && isName(2) && kw(3, "SET"):
			s, ok := parseSetting(toks[4:])
			if !ok {
				continue
			}
			r := role(toks[2].ident())
			at := indexKeyword(toks[4:], "TO")
			if at < 0 {
				at = indexPunct(toks[4:], "=")
			}
			r.Settings[strings.ToLower(s.Name)] = roleSetting{Name: s.Name, Value: s.Value, SQL: rebuild(toks[4+at+1:])}
		case kw(0, "GRANT") && isName(1) && kw(2, "TO") && isName(3):
			r := role(toks[3].ident())
			r.MemberOf[toks[1].ident()] = parseGrantOptions(toks[4:])
		case kw(0, "GRANT"):
			g.ParamGrants = append(g.ParamGrants, parseParamGrant(toks)...)
		}
	}
	return g, nil
}

// parseGrantOptions reads WITH ADMIN OPTION | ADMIN TRUE, INHERIT TRUE, SET FALSE [GRANTED BY …].
func parseGrantOptions(toks []token) Membership {
	var m Membership
	tf := func(i int) (bool, bool) {
		if i < len(toks) && (toks[i].isKeyword("TRUE") || toks[i].isKeyword("OPTION")) {
			return true, true
		}
		if i < len(toks) && toks[i].isKeyword("FALSE") {
			return false, true
		}
		return false, false
	}
	for i := 0; i < len(toks); i++ {
		switch {
		case toks[i].isKeyword("GRANTED"):
			return m
		case toks[i].isKeyword("ADMIN"):
			m.Admin, _ = tf(i + 1)
		case toks[i].isKeyword("INHERIT"):
			if v, ok := tf(i + 1); ok {
				m.Inherit = &v
			}
		case toks[i].isKeyword("SET"):
			if v, ok := tf(i + 1); ok {
				m.Set = &v
			}
		}
	}
	return m
}

// parseParamGrant reads GRANT priv[, priv] ON PARAMETER name[, name] TO role[, role] [WITH GRANT OPTION].
func parseParamGrant(toks []token) []ParamGrant {
	on := indexKeyword(toks, "ON")
	if on < 0 || on+1 >= len(toks) || !toks[on+1].isKeyword("PARAMETER") {
		return nil
	}
	var privs []string
	for i := 1; i < on; i++ {
		switch {
		case toks[i].isKeyword("SET"):
			privs = append(privs, "SET")
		case toks[i].isKeyword("ALTER") && i+1 < on && toks[i+1].isKeyword("SYSTEM"):
			privs = append(privs, "ALTER SYSTEM")
			i++
		case toks[i].isKeyword("ALL"):
			privs = append(privs, "SET", "ALTER SYSTEM")
		}
	}
	to := indexKeyword(toks, "TO")
	if to < 0 {
		return nil
	}
	var params []string
	var cur strings.Builder
	for _, t := range toks[on+2 : to] {
		if t.kind == tokPunct && t.val == "," {
			params = append(params, cur.String())
			cur.Reset()
			continue
		}
		if t.kind == tokQuoted {
			cur.WriteString(t.ident())
		} else {
			cur.WriteString(t.val)
		}
	}
	if cur.Len() > 0 {
		params = append(params, cur.String())
	}
	grantOpt := false
	var grantees []string
	for i := to + 1; i < len(toks); i++ {
		t := toks[i]
		switch {
		case t.isKeyword("WITH"):
			grantOpt = i+1 < len(toks) && toks[i+1].isKeyword("GRANT")
			i = len(toks)
		case t.isKeyword("GRANTED"):
			i = len(toks)
		case t.kind == tokWord || t.kind == tokQuoted:
			grantees = append(grantees, t.ident())
		}
	}
	var out []ParamGrant
	for _, p := range params {
		for _, pr := range privs {
			for _, g := range grantees {
				out = append(out, ParamGrant{Param: strings.ToLower(p), Privilege: pr, Grantee: g, GrantOption: grantOpt})
			}
		}
	}
	return out
}

func indexPunct(toks []token, p string) int {
	for i, t := range toks {
		if t.kind == tokPunct && t.val == p {
			return i
		}
	}
	return -1
}

// TargetRoles is every user on a target and its parameter grants.
type TargetRoles struct {
	Roles       map[string]*RoleSpec
	ParamGrants map[string]ParamGrant // ParamGrant.key()
	// HasGrantOptions: PostgreSQL 16+, memberships carry INHERIT and SET.
	HasGrantOptions bool
}

// ReadTargetRoles reads every user of the target; rolpassword only when the
// connection may read pg_authid (a superuser).
func ReadTargetRoles(ctx context.Context, admin *pg.Client, major int) (*TargetRoles, error) {
	tr := &TargetRoles{Roles: map[string]*RoleSpec{}, ParamGrants: map[string]ParamGrant{}, HasGrantOptions: major >= 16}
	rows, err := admin.Query(ctx, "select rolname, rolsuper, rolinherit, rolcreaterole, rolcreatedb, rolcanlogin, rolreplication, rolbypassrls, rolconnlimit, coalesce(rolvaliduntil::text, '') from pg_roles")
	if err != nil {
		return nil, fmt.Errorf("read users: %w", err)
	}
	for _, r := range rows {
		if len(r) < 10 {
			continue
		}
		rs := newRole(r[0])
		for i, f := range roleFlags {
			rs.Flags[f] = r[1+i] == "t"
		}
		rs.ConnLimit, _ = strconv.Atoi(r[8])
		if r[9] != "infinity" {
			rs.ValidUntil = r[9]
		}
		tr.Roles[r[0]] = rs
	}
	if rows, err := admin.Query(ctx, "select rolname, coalesce(rolpassword, '') from pg_authid"); err == nil {
		for _, r := range rows {
			if len(r) == 2 && tr.Roles[r[0]] != nil && r[1] != "" {
				tr.Roles[r[0]].Password = pg.QuoteLiteral(r[1])
			}
		}
	}
	rows, err = admin.Query(ctx, "select r.rolname, c from pg_db_role_setting s join pg_roles r on r.oid = s.setrole, unnest(s.setconfig) c where s.setdatabase = 0")
	if err != nil {
		return nil, fmt.Errorf("read user settings: %w", err)
	}
	for _, r := range rows {
		if len(r) < 2 || tr.Roles[r[0]] == nil {
			continue
		}
		name, val, _ := strings.Cut(r[1], "=")
		tr.Roles[r[0]].Settings[strings.ToLower(name)] = roleSetting{Name: name, Value: val}
	}
	opts := "a.admin_option, null::bool, null::bool"
	if tr.HasGrantOptions {
		opts = "a.admin_option, a.inherit_option, a.set_option"
	}
	rows, err = admin.Query(ctx, "select m.rolname, g.rolname, "+opts+" from pg_auth_members a join pg_roles g on g.oid = a.roleid join pg_roles m on m.oid = a.member")
	if err != nil {
		return nil, fmt.Errorf("read memberships: %w", err)
	}
	for _, r := range rows {
		if len(r) < 5 || tr.Roles[r[0]] == nil {
			continue
		}
		// PostgreSQL 16 may hold several grants of one role by different
		// grantors: the member has the union of their options.
		m := tr.Roles[r[0]].MemberOf[r[1]]
		m.Admin = m.Admin || r[2] == "t"
		if r[3] != "" {
			v := r[3] == "t" || (m.Inherit != nil && *m.Inherit)
			m.Inherit = &v
		}
		if r[4] != "" {
			v := r[4] == "t" || (m.Set != nil && *m.Set)
			m.Set = &v
		}
		tr.Roles[r[0]].MemberOf[r[1]] = m
	}
	if major >= 15 {
		rows, err = admin.Query(ctx, "select p.parname, a.privilege_type, case when a.grantee = 0 then 'PUBLIC' else pg_get_userbyid(a.grantee) end, a.is_grantable from pg_parameter_acl p, aclexplode(p.paracl) a")
		if err != nil {
			return nil, fmt.Errorf("read parameter privileges: %w", err)
		}
		for _, r := range rows {
			if len(r) < 4 {
				continue
			}
			pgr := ParamGrant{Param: strings.ToLower(r[0]), Privilege: r[1], Grantee: r[2], GrantOption: r[3] == "t"}
			tr.ParamGrants[pgr.key()] = pgr
		}
	}
	return tr, nil
}

// inheritOf is the INHERIT of a membership: its own option, or (before
// PostgreSQL 16) the member's INHERIT attribute.
func inheritOf(m Membership, member *RoleSpec) bool {
	if m.Inherit != nil {
		return *m.Inherit
	}
	if v, ok := member.Flags["INHERIT"]; ok {
		return v
	}
	return true
}

func setOf(m Membership) bool { return m.Set == nil || *m.Set }

// sameTime compares two timestamptz texts loosely (pg_dumpall and pg_roles may
// format the zone differently): equal after dropping spaces and a +00 suffix.
func sameTime(a, b string) bool {
	norm := func(s string) string {
		s = strings.ReplaceAll(s, " ", "")
		return strings.TrimSuffix(strings.TrimSuffix(s, "+00:00"), "+00")
	}
	return norm(a) == norm(b)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
