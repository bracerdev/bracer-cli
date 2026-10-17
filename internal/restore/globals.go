package restore

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"bracer.dev/bracer/internal/pg"
)

// GlobalsPlan is the result of filtering globals.sql for a restore into a real server
// (spec fetch-restore, AC --with-globals).
type GlobalsPlan struct {
	SQL             []byte
	Created         []string // users to create, in file order
	SkippedExisting int      // users already on the target, left untouched
	SkippedGrants   []string // "a -> x": a is the customer's pre-existing user, membership not granted
	Downgraded      []string // "x: SUPERUSER": attribute downgraded
	PgGrants        []string // "pg_read_all_data -> x": grant of a built-in role, kept for warning
	SkippedMissing  []string // "pg_maintain -> x": built-in pg_* role missing on the target (older Postgres version)
}

// ErrGlobalsUnsupported: the file has a statement Bracer does not understand; all globals are dropped (fail closed).
var ErrGlobalsUnsupported = errors.New("globals dump has a statement bracer does not understand")

// RiskyPgRoles are the built-in roles that grant power beyond the restored database.
var RiskyPgRoles = map[string]bool{
	"pg_read_all_data": true, "pg_write_all_data": true,
	"pg_read_server_files": true, "pg_write_server_files": true, "pg_execute_server_program": true,
	"pg_signal_backend": true, "pg_maintain": true, "pg_create_subscription": true,
}

// FilterGlobals splits globals.sql into statements (the lexer understands '…', E'…', "…",
// -- comments, ; terminators) and keeps only statements about users not in have, per
// the rules in the spec. A statement it cannot understand → ErrGlobalsUnsupported with the position.
func FilterGlobals(data []byte, have map[string]bool) (*GlobalsPlan, error) {
	stmts, err := splitStatements(string(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGlobalsUnsupported, err)
	}
	plan := &GlobalsPlan{}
	var out bytes.Buffer
	created := map[string]bool{}
	skipped := map[string]bool{}
	encoding := ""
	emit := func(s string) { out.WriteString(s + "\n") }
	for _, st := range stmts {
		if st.meta {
			word, _, _ := strings.Cut(st.text, " ")
			if word == `\restrict` || word == `\unrestrict` {
				// Drop the whole line: a psql older than the 8/2025 pg_dumpall does not understand it and with
				// ON_ERROR_STOP would abort the transaction; the output has only statements Bracer
				// builds itself, so restrict mode protects nothing (and one line may
				// contain a second meta-command).
				continue
			}
			return nil, fmt.Errorf("%w: line %d: meta-command %s", ErrGlobalsUnsupported, st.line, word)
		}
		toks, err := tokenize(st.text)
		if err != nil {
			return nil, fmt.Errorf("%w: line %d: %v", ErrGlobalsUnsupported, st.line, err)
		}
		if len(toks) == 0 {
			continue
		}
		kw := func(i int, k string) bool { return i < len(toks) && toks[i].isKeyword(k) }
		unsupported := func(what string) error {
			return fmt.Errorf("%w: line %d: %s", ErrGlobalsUnsupported, st.line, what)
		}
		// roleName: the user name at position i. A bare word such as ALL or CURRENT_USER means
		// the customer's users (every user / the superuser doing the restore), not a name.
		roleName := func(i int) (string, error) {
			if i >= len(toks) || (toks[i].kind != tokWord && toks[i].kind != tokQuoted) {
				return "", fmt.Errorf("user name expected")
			}
			if toks[i].kind == tokWord && roleKeywords[strings.ToLower(toks[i].val)] {
				return "", fmt.Errorf("%s is not a user name", toks[i].val)
			}
			return toks[i].ident(), nil
		}
		switch {
		case kw(0, "SET"):
			// Not copied: Bracer emits its own preamble (see the end of the function); only validated.
			enc, err := settingAllowed(toks)
			if err != nil {
				return nil, unsupported(err.Error())
			}
			if enc != "" {
				encoding = enc
			}
		case kw(0, "SELECT"):
			if !isSetSearchPath(toks) {
				return nil, unsupported("SELECT other than pg_catalog.set_config('search_path', '', false)")
			}
			// Users do not need search_path: dropped.
		case (kw(0, "CREATE") || kw(0, "ALTER")) && kw(1, "TABLESPACE"),
			kw(0, "COMMENT") && kw(1, "ON") && kw(2, "TABLESPACE"),
			kw(0, "GRANT") && hasKeyword(toks, "ON") && hasKeyword(toks, "TABLESPACE"):
			// A tablespace belongs to another server's paths: dropped.
		case kw(0, "GRANT") && hasKeyword(toks, "ON"):
			// GRANT SET|ALTER SYSTEM ON PARAMETER p TO x (PG15+): cluster-wide privilege, dropped and recorded.
			to := indexKeyword(toks, "TO")
			if !kw(1, "SET") && !kw(1, "ALTER") && !kw(1, "ALL") || !hasKeyword(toks, "PARAMETER") || to < 0 {
				return nil, unsupported("GRANT ON something other than a tablespace or a parameter")
			}
			grantee, err := roleName(to + 1)
			if err != nil {
				return nil, unsupported(err.Error())
			}
			if have[grantee] {
				skipped[grantee] = true
				continue
			}
			plan.Downgraded = append(plan.Downgraded, grantee+": GRANT "+rebuild(toks[1:to]))
		case kw(0, "CREATE") && kw(1, "ROLE"), kw(0, "ALTER") && kw(1, "ROLE"):
			role, err := roleName(2)
			if err != nil {
				return nil, unsupported(err.Error())
			}
			if have[role] {
				skipped[role] = true
				continue
			}
			rest := toks[3:]
			if kw(0, "ALTER") && roleSetting(rest) {
				// ALTER ROLE x [IN DATABASE d] SET …: only affects the new user's sessions.
				emit("ALTER ROLE " + pg.QuoteIdent(role) + " " + rebuild(rest) + ";")
				continue
			}
			if len(rest) > 0 && rest[0].isKeyword("WITH") {
				rest = rest[1:]
			}
			opts, down, err := roleOptions(rest)
			if err != nil {
				return nil, unsupported(err.Error())
			}
			for _, d := range down {
				plan.Downgraded = append(plan.Downgraded, role+": "+d)
			}
			if kw(0, "CREATE") && !created[role] {
				created[role] = true
				plan.Created = append(plan.Created, role)
			}
			s := toks[0].val + " ROLE " + pg.QuoteIdent(role)
			if opts != "" {
				s += " WITH " + opts
			}
			emit(s + ";")
		case kw(0, "COMMENT") && kw(1, "ON") && kw(2, "ROLE"):
			role, err := roleName(3)
			if err != nil || !kw(4, "IS") || len(toks) != 6 || toks[5].kind != tokLiteral && !toks[5].isKeyword("NULL") {
				return nil, unsupported("COMMENT ON ROLE not of the form COMMENT ON ROLE x IS '…'")
			}
			if have[role] {
				skipped[role] = true
				continue
			}
			emit("COMMENT ON ROLE " + pg.QuoteIdent(role) + " IS " + toks[5].val + ";")
		case kw(0, "GRANT"):
			granted, err1 := roleName(1)
			grantee, err2 := roleName(3)
			if err1 != nil || err2 != nil || !kw(2, "TO") {
				return nil, unsupported("GRANT not of the form GRANT a TO x")
			}
			if have[grantee] {
				skipped[grantee] = true
				continue
			}
			isPg := strings.HasPrefix(granted, "pg_")
			if !created[granted] && !isPg {
				plan.SkippedGrants = append(plan.SkippedGrants, granted+" -> "+grantee)
				continue
			}
			if isPg && !have[granted] {
				// Built-in role of a Postgres newer than the target: the GRANT would fail and abort the whole transaction.
				plan.SkippedMissing = append(plan.SkippedMissing, granted+" -> "+grantee)
				continue
			}
			opts, err := grantOptions(toks[4:], isPg)
			if err != nil {
				return nil, unsupported(err.Error())
			}
			if isPg {
				plan.PgGrants = append(plan.PgGrants, granted+" -> "+grantee)
			}
			s := "GRANT " + pg.QuoteIdent(granted) + " TO " + pg.QuoteIdent(grantee)
			if opts != "" {
				s += " WITH " + opts
			}
			emit(s + ";")
		default:
			return nil, unsupported("statement starting with " + toks[0].val)
		}
	}
	// Preamble emitted by Bracer: the server reads '…' literals exactly as the lexer above does, whatever
	// the customer's postgresql.conf sets; client_encoding is taken from the file.
	var sql bytes.Buffer
	sql.WriteString("SET standard_conforming_strings = on;\nSET default_transaction_read_only = off;\n")
	if encoding != "" {
		sql.WriteString("SET client_encoding = '" + encoding + "';\n")
	}
	sql.Write(out.Bytes())
	plan.SQL = sql.Bytes()
	plan.SkippedExisting = len(skipped)
	return plan, nil
}

// roleKeywords: bare words in the user-name position that Postgres reads as the session's user
// or every user (ALTER ROLE ALL SET …, ALTER ROLE current_user …).
var roleKeywords = map[string]bool{
	"all": true, "public": true, "none": true,
	"current_user": true, "current_role": true, "session_user": true,
}

// serverEncodings: accepted client_encoding values — only encodings usable on the
// server side, in which no multi-byte character contains the byte \ or ', so the
// byte-wise lexer above reads literals exactly as the server does. SJIS, BIG5, GBK, UHC,
// GB18030, JOHAB (client-side only) are not in the list.
var serverEncodings = map[string]bool{
	"SQL_ASCII": true, "UTF8": true, "MULE_INTERNAL": true,
	"EUC_JP": true, "EUC_CN": true, "EUC_KR": true, "EUC_TW": true, "EUC_JIS_2004": true,
	"LATIN1": true, "LATIN2": true, "LATIN3": true, "LATIN4": true, "LATIN5": true,
	"LATIN6": true, "LATIN7": true, "LATIN8": true, "LATIN9": true, "LATIN10": true,
	"ISO_8859_5": true, "ISO_8859_6": true, "ISO_8859_7": true, "ISO_8859_8": true,
	"KOI8R": true, "KOI8U": true, "WIN866": true, "WIN874": true,
	"WIN1250": true, "WIN1251": true, "WIN1252": true, "WIN1253": true, "WIN1254": true,
	"WIN1255": true, "WIN1256": true, "WIN1257": true, "WIN1258": true,
}

// settingAllowed checks `SET name = value` against a whitelist; returns the
// normalized encoding name if it is client_encoding.
func settingAllowed(toks []token) (string, error) {
	if len(toks) != 4 || toks[1].kind != tokWord || !(toks[2].kind == tokPunct && toks[2].val == "=" || toks[2].isKeyword("TO")) {
		return "", fmt.Errorf("SET not of the form SET name = value")
	}
	v := settingValue(toks[3])
	switch name := strings.ToLower(toks[1].val); name {
	case "default_transaction_read_only":
		if l := strings.ToLower(v); l == "off" || l == "false" {
			return "", nil
		}
	case "standard_conforming_strings":
		if l := strings.ToLower(v); l == "on" || l == "true" {
			return "", nil
		}
	case "client_encoding":
		if enc := strings.ToUpper(v); serverEncodings[enc] {
			return enc, nil
		}
	default:
		return "", fmt.Errorf("SET %s", toks[1].val)
	}
	return "", fmt.Errorf("SET %s = %s", toks[1].val, toks[3].val)
}

// settingValue: a bare value or a '…' literal (quotes stripped; valid values have no escapes).
func settingValue(t token) string {
	if t.kind == tokLiteral {
		return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(t.val, "E"), "'"), "'")
	}
	return t.val
}

// isSetSearchPath: exactly pg_dumpall's `SELECT pg_catalog.set_config('search_path', '…', false)` statement, nothing more.
func isSetSearchPath(toks []token) bool {
	if len(toks) != 11 {
		return false
	}
	is := func(i int, kind tokKind, val string) bool {
		return toks[i].kind == kind && (val == "" || strings.EqualFold(toks[i].val, val))
	}
	return is(0, tokWord, "SELECT") && is(1, tokWord, "pg_catalog") && is(2, tokPunct, ".") && is(3, tokWord, "set_config") &&
		is(4, tokPunct, "(") && is(5, tokLiteral, "'search_path'") && is(6, tokPunct, ",") && is(7, tokLiteral, "") &&
		is(8, tokPunct, ",") && is(9, tokWord, "false") && is(10, tokPunct, ")")
}

func indexKeyword(toks []token, k string) int {
	for i, t := range toks {
		if t.isKeyword(k) {
			return i
		}
	}
	return -1
}

// roleOptions reads the attributes of CREATE/ALTER ROLE, downgrading SUPERUSER/REPLICATION/
// BYPASSRLS/CREATEROLE (CREATEROLE on PG ≤ 15 can alter/drop/grant any user that is
// not a superuser); an unknown attribute → error. Returns the attribute string and the list of downgrades.
func roleOptions(toks []token) (string, []string, error) {
	var out, down []string
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.kind != tokWord {
			return "", nil, fmt.Errorf("unexpected %s in user options", t.val)
		}
		switch strings.ToUpper(t.val) {
		case "SUPERUSER":
			down = append(down, "SUPERUSER")
			out = append(out, "NOSUPERUSER")
		case "REPLICATION":
			down = append(down, "REPLICATION")
			out = append(out, "NOREPLICATION")
		case "BYPASSRLS":
			down = append(down, "BYPASSRLS")
			out = append(out, "NOBYPASSRLS")
		case "CREATEROLE":
			down = append(down, "CREATEROLE")
			out = append(out, "NOCREATEROLE")
		case "NOSUPERUSER", "NOREPLICATION", "NOBYPASSRLS", "INHERIT", "NOINHERIT",
			"NOCREATEROLE", "CREATEDB", "NOCREATEDB", "LOGIN", "NOLOGIN":
			out = append(out, strings.ToUpper(t.val))
		case "CONNECTION":
			if i+2 >= len(toks) || !toks[i+1].isKeyword("LIMIT") || toks[i+2].kind != tokNumber {
				return "", nil, fmt.Errorf("CONNECTION LIMIT without a number")
			}
			out = append(out, "CONNECTION LIMIT "+toks[i+2].val)
			i += 2
		case "ENCRYPTED":
			if i+1 >= len(toks) || !toks[i+1].isKeyword("PASSWORD") {
				return "", nil, fmt.Errorf("ENCRYPTED without PASSWORD")
			}
			// Drop the word ENCRYPTED (a no-op since PG10); the next iteration handles PASSWORD.
		case "PASSWORD":
			if i+1 >= len(toks) {
				return "", nil, fmt.Errorf("PASSWORD without a value")
			}
			v := toks[i+1]
			switch {
			case v.kind == tokLiteral:
				out = append(out, "PASSWORD "+v.val)
			case v.isKeyword("NULL"):
				out = append(out, "PASSWORD NULL")
			default:
				return "", nil, fmt.Errorf("PASSWORD without a literal")
			}
			i++
		case "VALID":
			if i+2 >= len(toks) || !toks[i+1].isKeyword("UNTIL") || toks[i+2].kind != tokLiteral {
				return "", nil, fmt.Errorf("VALID UNTIL without a literal")
			}
			out = append(out, "VALID UNTIL "+toks[i+2].val)
			i += 2
		default:
			return "", nil, fmt.Errorf("user option %s", t.val)
		}
	}
	return strings.Join(out, " "), down, nil
}

// grantOptions reads what follows GRANT a TO x: [WITH opt, …] [GRANTED BY g]. GRANTED BY
// is dropped (the source grantor usually does not exist on the target); for built-in pg_* roles all
// options are dropped: ADMIN because a new user must not hand out cluster privileges to other users,
// and ADMIN FALSE/INHERIT/SET is PG16 syntax that makes an older target fail the whole transaction.
func grantOptions(toks []token, pgRole bool) (string, error) {
	var out []string
	i := 0
	if i < len(toks) && toks[i].isKeyword("WITH") {
		i++
		for i < len(toks) && !toks[i].isKeyword("GRANTED") {
			t := toks[i]
			if t.kind == tokPunct && t.val == "," {
				i++
				continue
			}
			if t.kind != tokWord {
				return "", fmt.Errorf("unexpected %s in GRANT options", t.val)
			}
			name := strings.ToUpper(t.val)
			switch name {
			case "ADMIN", "INHERIT", "SET":
				if i+1 >= len(toks) {
					return "", fmt.Errorf("GRANT option %s without a value", name)
				}
				v := strings.ToUpper(toks[i+1].val)
				if v != "TRUE" && v != "FALSE" && !(name == "ADMIN" && v == "OPTION") {
					return "", fmt.Errorf("GRANT option %s %s", name, toks[i+1].val)
				}
				if name == "ADMIN" && v == "OPTION" {
					// Old syntax: kept as is so an old server understands it.
					out = append(out, "ADMIN OPTION")
					i += 2
					continue
				}
				out = append(out, name+" "+v)
				i += 2
			default:
				return "", fmt.Errorf("GRANT option %s", t.val)
			}
		}
	}
	if i < len(toks) {
		if !toks[i].isKeyword("GRANTED") || i+2 >= len(toks) || !toks[i+1].isKeyword("BY") || (toks[i+2].kind != tokWord && toks[i+2].kind != tokQuoted) || i+3 != len(toks) {
			return "", fmt.Errorf("unexpected %s after GRANT", toks[i].val)
		}
	}
	if pgRole {
		return "", nil
	}
	return strings.Join(out, ", "), nil
}

// roleSetting: what follows ALTER ROLE x is [IN DATABASE d] SET … or RESET ….
func roleSetting(rest []token) bool {
	if len(rest) > 0 && (rest[0].isKeyword("SET") || rest[0].isKeyword("RESET")) {
		return true
	}
	return len(rest) > 3 && rest[0].isKeyword("IN") && rest[1].isKeyword("DATABASE") && rest[2].kind != tokPunct && (rest[3].isKeyword("SET") || rest[3].isKeyword("RESET"))
}

func hasKeyword(toks []token, k string) bool {
	for _, t := range toks {
		if t.isKeyword(k) {
			return true
		}
	}
	return false
}

// statement is one SQL statement (; stripped) or one psql meta-command line.
type statement struct {
	text string
	meta bool
	line int
}

// splitStatements splits SQL into statements: understands '…' (with ” as an escaped quote), E'…' (with
// \ escapes), "…", -- comments, \… meta-commands at line start. $…$ or a statement
// not terminated by ; is an error.
func splitStatements(s string) ([]statement, error) {
	var out []statement
	var cur strings.Builder
	pending := false
	start := 0
	lineOf := func(pos int) int { return 1 + strings.Count(s[:pos], "\n") }
	i := 0
	for i < len(s) {
		c := s[i]
		if !pending {
			switch {
			case c == ' ' || c == '\t' || c == '\r' || c == '\n':
				i++
				continue
			case strings.HasPrefix(s[i:], "--"):
				i = skipLine(s, i)
				continue
			case c == '\\' && atLineStart(s, i):
				end := skipLine(s, i)
				out = append(out, statement{text: strings.TrimRight(s[i:end], "\r\n"), meta: true, line: lineOf(i)})
				i = end
				continue
			}
			pending, start = true, i
		}
		switch c {
		case '\'':
			j, err := scanSingle(s, i, escapePrefix(cur.String()))
			if err != nil {
				return nil, fmt.Errorf("line %d: %v", lineOf(i), err)
			}
			cur.WriteString(s[i:j])
			i = j
			continue
		case '"':
			j := strings.IndexByte(s[i+1:], '"')
			for j >= 0 && i+1+j+1 < len(s) && s[i+1+j+1] == '"' { // "" inside the name
				k := strings.IndexByte(s[i+1+j+2:], '"')
				if k < 0 {
					j = -1
					break
				}
				j = j + 2 + k
			}
			if j < 0 {
				return nil, fmt.Errorf("line %d: unterminated quoted identifier", lineOf(i))
			}
			cur.WriteString(s[i : i+1+j+1])
			i = i + 1 + j + 1
			continue
		case '$':
			return nil, fmt.Errorf("line %d: dollar quoting is not supported", lineOf(i))
		case '-':
			if strings.HasPrefix(s[i:], "--") {
				i = skipLine(s, i)
				cur.WriteByte(' ')
				continue
			}
		case ';':
			out = append(out, statement{text: strings.TrimSpace(cur.String()), line: lineOf(start)})
			cur.Reset()
			pending = false
			i++
			continue
		}
		cur.WriteByte(c)
		i++
	}
	if pending {
		return nil, fmt.Errorf("line %d: statement without a terminating semicolon", lineOf(start))
	}
	return out, nil
}

func skipLine(s string, i int) int {
	if j := strings.IndexByte(s[i:], '\n'); j >= 0 {
		return i + j
	}
	return len(s)
}

func atLineStart(s string, i int) bool {
	for j := i - 1; j >= 0; j-- {
		switch s[j] {
		case ' ', '\t', '\r':
			continue
		case '\n':
			return true
		default:
			return false
		}
	}
	return true
}

// escapePrefix: the text before the ' ends in a standalone E/e → E'…' with \ escapes.
func escapePrefix(before string) bool {
	n := len(before)
	if n == 0 || (before[n-1] != 'E' && before[n-1] != 'e') {
		return false
	}
	return n == 1 || !isIdentByte(before[n-2])
}

// scanSingle returns the position after the closing '.
func scanSingle(s string, i int, escapes bool) (int, error) {
	for j := i + 1; j < len(s); j++ {
		switch {
		case escapes && s[j] == '\\':
			j++
		case s[j] == '\'':
			if j+1 < len(s) && s[j+1] == '\'' {
				j++
				continue
			}
			return j + 1, nil
		}
	}
	return 0, fmt.Errorf("unterminated string literal")
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}

type tokKind int

const (
	tokWord    tokKind = iota // keyword or bare identifier
	tokQuoted                 // "identifier"
	tokLiteral                // '…' or E'…', kept verbatim
	tokNumber
	tokPunct
)

type token struct {
	kind tokKind
	val  string // literal: verbatim; quoted: name with quotes stripped
}

func (t token) isKeyword(k string) bool { return t.kind == tokWord && strings.EqualFold(t.val, k) }

// ident returns the user name: a bare one is ASCII-lowercased as Postgres does.
func (t token) ident() string {
	if t.kind == tokQuoted {
		return t.val
	}
	return strings.ToLower(t.val)
}

// tokenize splits one statement into words, quoted identifiers, literals, numbers, punctuation.
func tokenize(s string) ([]token, error) {
	var out []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case c == '\'' || (c == 'E' || c == 'e') && i+1 < len(s) && s[i+1] == '\'':
			esc := c != '\''
			start := i
			if esc {
				i++
			}
			j, err := scanSingle(s, i, esc)
			if err != nil {
				return nil, err
			}
			out = append(out, token{kind: tokLiteral, val: s[start:j]})
			i = j
		case c == '"':
			var b strings.Builder
			j := i + 1
			for ; j < len(s); j++ {
				if s[j] == '"' {
					if j+1 < len(s) && s[j+1] == '"' {
						b.WriteByte('"')
						j++
						continue
					}
					break
				}
				b.WriteByte(s[j])
			}
			if j >= len(s) {
				return nil, fmt.Errorf("unterminated quoted identifier")
			}
			out = append(out, token{kind: tokQuoted, val: b.String()})
			i = j + 1
		case c >= '0' && c <= '9' || c == '-' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9':
			j := i + 1
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.') {
				j++
			}
			out = append(out, token{kind: tokNumber, val: s[i:j]})
			i = j
		case isIdentByte(c):
			j := i + 1
			for j < len(s) && isIdentByte(s[j]) {
				j++
			}
			out = append(out, token{kind: tokWord, val: s[i:j]})
			i = j
		case c == ',' || c == '=' || c == '(' || c == ')' || c == '.':
			out = append(out, token{kind: tokPunct, val: string(c)})
			i++
		default:
			return nil, fmt.Errorf("unexpected character %q", c)
		}
	}
	return out, nil
}

// rebuild reassembles the statement from tokens (quoted identifiers are re-quoted).
func rebuild(toks []token) string {
	var b strings.Builder
	for i, t := range toks {
		if i > 0 && !(t.kind == tokPunct && (t.val == "," || t.val == ")" || t.val == ".")) && !(toks[i-1].kind == tokPunct && (toks[i-1].val == "(" || toks[i-1].val == ".")) {
			b.WriteByte(' ')
		}
		switch t.kind {
		case tokQuoted:
			b.WriteString(pg.QuoteIdent(t.val))
		default:
			b.WriteString(t.val)
		}
	}
	return b.String()
}
