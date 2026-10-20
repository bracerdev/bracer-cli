package pg

import (
	"context"
	"fmt"
	"strings"
)

// Locale is the encoding and locale of a database (pg_database), enough to
// recreate an identical database on another server, as `pg_restore --create`
// does. Written to the manifest of every dump (spec backup § Flow 3c).
type Locale struct {
	Encoding string `json:"encoding"` // pg_encoding_to_char: UTF8, SQL_ASCII…
	Collate  string `json:"collate"`  // datcollate (libc)
	Ctype    string `json:"ctype"`    // datctype (libc)
	// Provider: "" or "libc" (default), "icu" (PG15+), "builtin" (PG17+).
	Provider string `json:"provider,omitempty"`
	// Locale is ICU_LOCALE (icu) or BUILTIN_LOCALE (builtin).
	Locale   string `json:"locale,omitempty"`
	ICURules string `json:"icu_rules,omitempty"` // ICU_RULES, PG16+
	// CollVersion is datcollversion (PG15+): the collation library's version.
	// Another one on the target only changes sort order, so it is a warning.
	CollVersion string `json:"collversion,omitempty"`
}

// providerNames maps the pg_database.datlocprovider letter to the name used in CREATE DATABASE.
var providerNames = map[string]string{"c": "libc", "i": "icu", "b": "builtin"}

// DatabaseLocale reads the encoding and locale of database db from pg_database
// (connecting to the maintenance database is enough). Columns differ by major, so it
// queries the connected server's columns: PG < 15 libc only; 15–16 `daticulocale`; 17+ `datlocale`.
func (c *Client) DatabaseLocale(ctx context.Context, db string, major int) (*Locale, error) {
	cols := "pg_encoding_to_char(encoding), datcollate, datctype"
	switch {
	case major >= 17:
		cols += ", datlocprovider::text, coalesce(datlocale, ''), coalesce(daticurules, ''), coalesce(datcollversion, '')"
	case major == 16:
		cols += ", datlocprovider::text, coalesce(daticulocale, ''), coalesce(daticurules, ''), coalesce(datcollversion, '')"
	case major == 15:
		cols += ", datlocprovider::text, coalesce(daticulocale, ''), '', coalesce(datcollversion, '')"
	default:
		cols += ", 'c', '', '', ''"
	}
	rows, err := c.Query(ctx, "select "+cols+" from pg_database where datname = "+QuoteLiteral(db))
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || len(rows[0]) < 6 {
		return nil, fmt.Errorf("unexpected pg_database row for %s: %q", db, rows)
	}
	r := rows[0]
	l := &Locale{Encoding: r[0], Collate: r[1], Ctype: r[2], Locale: r[4], ICURules: r[5]}
	if len(r) > 6 {
		l.CollVersion = r[6]
	}
	if r[0] == "" || r[1] == "" || r[2] == "" {
		return nil, fmt.Errorf("pg_database has no encoding/locale for %s: %q", db, r)
	}
	switch r[3] {
	case "c", "":
		// libc: leave empty; the manifest stays compact and CREATE DATABASE needs no LOCALE_PROVIDER (PG < 15 lacks that keyword).
	case "i", "b":
		l.Provider = providerNames[r[3]]
	default:
		return nil, fmt.Errorf("unknown locale provider %q for %s", r[3], db)
	}
	return l, nil
}

// MinMajor returns the smallest Postgres major that can create a database with
// this locale: ICU from 15, ICU_RULES from 16, builtin from 17.
func (l *Locale) MinMajor() int {
	switch {
	case l.Provider == "builtin":
		return 17
	case l.Provider == "icu" && l.ICURules != "":
		return 16
	case l.Provider == "icu":
		return 15
	}
	return 0
}

// CreateOptions returns the option part of CREATE DATABASE (after TEMPLATE template0)
// that recreates exactly this encoding/locale on a server of major targetMajor; errors
// when that server lacks the required provider (checked before creating anything).
func (l *Locale) CreateOptions(targetMajor int) (string, error) {
	if need := l.MinMajor(); targetMajor > 0 && targetMajor < need {
		return "", fmt.Errorf("database uses the %s locale provider (PostgreSQL %d+); target is PostgreSQL %d", l.Provider, need, targetMajor)
	}
	var b strings.Builder
	fmt.Fprintf(&b, " encoding %s lc_collate %s lc_ctype %s", QuoteLiteral(l.Encoding), QuoteLiteral(l.Collate), QuoteLiteral(l.Ctype))
	switch l.Provider {
	case "icu":
		fmt.Fprintf(&b, " locale_provider icu icu_locale %s", QuoteLiteral(l.Locale))
		if l.ICURules != "" {
			fmt.Fprintf(&b, " icu_rules %s", QuoteLiteral(l.ICURules))
		}
	case "builtin":
		fmt.Fprintf(&b, " locale_provider builtin builtin_locale %s", QuoteLiteral(l.Locale))
	}
	return b.String(), nil
}

// String describes the locale for messages: "UTF8, C.UTF-8" or "UTF8, icu en-US".
func (l *Locale) String() string {
	switch l.Provider {
	case "icu", "builtin":
		return fmt.Sprintf("%s, %s %s", l.Encoding, l.Provider, l.Locale)
	}
	if l.Collate == l.Ctype {
		return l.Encoding + ", " + l.Collate
	}
	return fmt.Sprintf("%s, collate %s, ctype %s", l.Encoding, l.Collate, l.Ctype)
}
