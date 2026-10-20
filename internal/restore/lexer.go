package restore

import (
	"fmt"
	"strings"

	"bracer.dev/bracer/internal/pg"
)

// A small SQL lexer for pg_dumpall globals and the database entries of an
// archive: statements, words, quoted identifiers, literals. It does not
// understand dollar quoting; callers only feed it statements that have none.

func indexKeyword(toks []token, k string) int {
	for i, t := range toks {
		if t.isKeyword(k) {
			return i
		}
	}
	return -1
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
func tokenize(s string) ([]token, error) { return tokenizeMode(s, false) }

// tokenizeLoose is tokenize where any other character (operators, casts in a
// policy expression) becomes a punctuation token instead of an error.
func tokenizeLoose(s string) ([]token, error) { return tokenizeMode(s, true) }

func tokenizeMode(s string, loose bool) ([]token, error) {
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
		case loose:
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
