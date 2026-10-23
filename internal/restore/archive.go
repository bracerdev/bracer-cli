package restore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"bracer.dev/bracer/internal/pg"
)

// What the archive's table of contents says, read before anything is created
// on the target (spec restore § Flow 5): which users the database needs
// (data_only) and what needs a superuser (preview).

// headSizes: the table of contents is usually small; try a small head first and
// read up to MaxTOC only when pg_restore needs more.
var headSizes = []int64{1 << 20, MaxTOC}

// ArchiveInfo is what one database's archive needs from a target.
type ArchiveInfo struct {
	SourceDB string     // from CREATE DATABASE
	AttrSQL  []byte     // pg_restore --create -n <none>: the database's own entries
	Entries  []TOCEntry // pg_restore --create --schema-only: every entry
	Warnings []string   // entries that could not be read
}

// TOCEntry is one archive entry in pg_restore's SQL output.
type TOCEntry struct {
	Name, Type, Schema, Owner string
	Body                      string
}

// ReadArchive reads the head of the archive opened by open (twice at most: a
// small head, then up to MaxTOC) and asks pg_restore for its SQL. No
// connection is made.
func ReadArchive(ctx context.Context, runner pg.Runner, conn pg.Conn, open func() (io.ReadCloser, error)) (*ArchiveInfo, error) {
	var lastErr error
	for i, size := range headSizes {
		head, err := readHead(open, size)
		if err != nil {
			return nil, err
		}
		short := int64(len(head)) < size // the whole archive is in head
		attr, aerr := DatabaseSQL(ctx, runner, conn, head, nil)
		var schema bytes.Buffer
		var serr error
		if aerr == nil {
			var stderr []byte
			stderr, serr = pg.RunCapture(ctx, runner, conn, "pg_restore", []string{"--create", "--schema-only", "-f", "-"}, bytes.NewReader(head), &schema)
			if serr != nil {
				msg := Tail(stderr, 5)
				if msg == "" {
					msg = serr.Error()
				}
				serr = errors.New(msg)
			}
		}
		if aerr == nil && serr == nil {
			info := &ArchiveInfo{AttrSQL: attr, Entries: ParseEntries(schema.String())}
			if a, err := ParseDatabaseSQL(attr, "x", nil); err == nil {
				info.SourceDB = a.SourceDB
			}
			return info, nil
		}
		lastErr = aerr
		if lastErr == nil {
			lastErr = serr
		}
		if short || i == len(headSizes)-1 {
			break
		}
	}
	return nil, lastErr
}

func readHead(open func() (io.ReadCloser, error), size int64) ([]byte, error) {
	rc, err := open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, size))
}

var reHeader = regexp.MustCompile(`^-- Name: (.*); Type: (.*); Schema: (.*); Owner: (.*)$`)

// ParseEntries splits pg_restore's SQL output at its "-- Name: …; Type: …;
// Schema: …; Owner: …" comments.
func ParseEntries(sql string) []TOCEntry {
	var out []TOCEntry
	var cur *TOCEntry
	var body strings.Builder
	flush := func() {
		if cur != nil {
			cur.Body = body.String()
			out = append(out, *cur)
		}
		body.Reset()
	}
	for _, line := range strings.Split(sql, "\n") {
		if m := reHeader.FindStringSubmatch(line); m != nil {
			flush()
			cur = &TOCEntry{Name: m[1], Type: m[2], Schema: m[3], Owner: m[4]}
			continue
		}
		if cur != nil {
			body.WriteString(line + "\n")
		}
	}
	flush()
	return out
}

// NeededRoles lists the users the archive names: owners, GRANT/REVOKE grantees
// and grantors, ALTER DEFAULT PRIVILEGES FOR ROLE, CREATE POLICY … TO, ALTER
// ROLE … IN DATABASE, USER MAPPING FOR. PUBLIC and built-in pg_* roles are left out.
func NeededRoles(entries []TOCEntry) (roles map[string]bool, warnings []string) {
	roles = map[string]bool{}
	add := func(r string) {
		if r != "" && r != "-" && !strings.EqualFold(r, "public") && !strings.HasPrefix(r, "pg_") {
			roles[r] = true
		}
	}
	for _, e := range entries {
		add(e.Owner)
		switch e.Type {
		case "ACL", "DEFAULT ACL", "POLICY", "DATABASE PROPERTIES", "USER MAPPING":
		default:
			continue
		}
		stmts, err := splitStatements(stripComments(e.Body))
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("could not read the %s entry %s: %v", e.Type, e.Name, err))
			continue
		}
		for _, st := range stmts {
			if st.meta {
				continue
			}
			toks, err := tokenizeLoose(st.text)
			if err != nil || len(toks) == 0 {
				warnings = append(warnings, fmt.Sprintf("could not read the %s entry %s", e.Type, e.Name))
				continue
			}
			for _, r := range rolesIn(toks) {
				add(r)
			}
		}
	}
	return roles, warnings
}

// stripComments drops whole-line -- comments (pg_restore's own), which the
// statement splitter would otherwise keep as statement text.
func stripComments(body string) string {
	var b strings.Builder
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "--") {
			continue
		}
		b.WriteString(l + "\n")
	}
	return b.String()
}

// rolesIn returns the user names in one GRANT, REVOKE, ALTER DEFAULT
// PRIVILEGES, CREATE POLICY, ALTER ROLE … IN DATABASE, SET SESSION
// AUTHORIZATION or CREATE USER MAPPING statement.
func rolesIn(toks []token) []string {
	var out []string
	name := func(t token) bool { return t.kind == tokWord || t.kind == tokQuoted }
	// names reads a comma-separated list of users from i until a word in stop.
	names := func(i int, stop ...string) {
		for ; i < len(toks); i++ {
			t := toks[i]
			if t.kind == tokPunct && t.val == "," || t.isKeyword("GROUP") {
				continue
			}
			for _, s := range stop {
				if t.isKeyword(s) {
					return
				}
			}
			if !name(t) {
				return
			}
			out = append(out, t.ident())
		}
	}
	kw := func(i int, k string) bool { return i < len(toks) && toks[i].isKeyword(k) }
	switch {
	case kw(0, "SET") && kw(1, "SESSION") && kw(2, "AUTHORIZATION") && len(toks) > 3:
		names(3)
	case kw(0, "ALTER") && (kw(1, "ROLE") || kw(1, "USER")) && kw(3, "IN"):
		names(2, "IN")
	case kw(0, "CREATE") && kw(1, "USER") && kw(2, "MAPPING") && kw(3, "FOR"):
		names(4, "SERVER")
	case kw(0, "CREATE") && kw(1, "POLICY"):
		if i := indexKeyword(toks, "TO"); i > 0 {
			names(i+1, "USING", "WITH")
		}
	case kw(0, "GRANT"), kw(0, "REVOKE"), kw(0, "ALTER") && kw(1, "DEFAULT"):
		start := 0
		if kw(0, "ALTER") {
			// ALTER DEFAULT PRIVILEGES FOR ROLE a, b [IN SCHEMA s] GRANT … TO …
			if kw(3, "FOR") && (kw(4, "ROLE") || kw(4, "USER")) {
				names(5, "IN", "GRANT", "REVOKE")
			}
			for i, t := range toks {
				if t.isKeyword("GRANT") || t.isKeyword("REVOKE") {
					start = i
					break
				}
			}
		}
		to := "TO"
		if toks[start].isKeyword("REVOKE") {
			to = "FROM"
		}
		at := -1
		for i := len(toks) - 1; i > start; i-- {
			if toks[i].isKeyword(to) {
				at = i
				break
			}
		}
		if at > 0 {
			names(at+1, "WITH", "GRANTED", "CASCADE", "RESTRICT")
		}
		if i := indexKeyword(toks, "GRANTED"); i > 0 && kw(i+1, "BY") {
			names(i + 2)
		}
	}
	return out
}

// untrustedLanguages need a superuser to create a function in.
var untrustedLanguages = map[string]bool{
	"c": true, "internal": true, "plpythonu": true, "plpython2u": true, "plpython3u": true,
	"plperlu": true, "pltclu": true, "plsh": true,
}

var (
	reLanguage = regexp.MustCompile(`(?i)^\s*LANGUAGE\s+"?([A-Za-z0-9_]+)"?\s*$`)
	reForceRLS = regexp.MustCompile(`(?i)^ALTER TABLE .+ FORCE ROW LEVEL SECURITY;$`)
)

// SuperuserObjects lists what a restore by a user that is not a superuser
// cannot create (spec restore AC preview): extensions that are not
// trusted on the target, functions in untrusted languages, procedural
// languages, event triggers, foreign-data wrappers, and tables with FORCE ROW
// LEVEL SECURITY (the restoring user owns them, so row counts would be wrong).
// trusted maps an extension available on the target to whether its default
// version is trusted.
func SuperuserObjects(entries []TOCEntry, trusted map[string]bool) []string {
	var out []string
	for _, e := range entries {
		switch e.Type {
		case "EXTENSION":
			if t, ok := trusted[e.Name]; !ok {
				out = append(out, "extension "+e.Name+" (not available on the target)")
			} else if !t {
				out = append(out, "extension "+e.Name+" (not trusted)")
			}
		case "EVENT TRIGGER", "PROCEDURAL LANGUAGE", "FOREIGN DATA WRAPPER":
			out = append(out, strings.ToLower(e.Type)+" "+e.Name)
		case "FUNCTION", "PROCEDURE", "AGGREGATE":
			for _, l := range strings.Split(e.Body, "\n") {
				if strings.HasPrefix(strings.TrimSpace(l), "AS ") {
					break // the body follows; it may hold anything
				}
				if m := reLanguage.FindStringSubmatch(l); m != nil && untrustedLanguages[strings.ToLower(m[1])] {
					out = append(out, fmt.Sprintf("%s %s.%s (language %s)", strings.ToLower(e.Type), e.Schema, e.Name, strings.ToLower(m[1])))
					break
				}
			}
		}
		if e.Type == "TABLE" || e.Type == "ROW SECURITY" {
			for _, l := range strings.Split(e.Body, "\n") {
				if reForceRLS.MatchString(strings.TrimSpace(l)) {
					out = append(out, fmt.Sprintf("table %s.%s (FORCE ROW LEVEL SECURITY)", e.Schema, e.Name))
					break
				}
			}
		}
	}
	sort.Strings(out)
	return dedupe(out)
}

func dedupe(xs []string) []string {
	var out []string
	for i, x := range xs {
		if i == 0 || x != xs[i-1] {
			out = append(out, x)
		}
	}
	return out
}

// TrustedExtensions reads which extensions the target has and whether their
// default version is trusted (PostgreSQL 13+; older servers trust none).
func TrustedExtensions(ctx context.Context, admin *pg.Client, major int) (map[string]bool, error) {
	sql := "select name, false from pg_available_extensions"
	if major >= 13 {
		sql = "select e.name, coalesce(v.trusted, false) from pg_available_extensions e left join pg_available_extension_versions v on v.name = e.name and v.version = e.default_version"
	}
	rows, err := admin.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range rows {
		if len(r) >= 2 {
			out[r[0]] = r[1] == "t"
		}
	}
	return out, nil
}
