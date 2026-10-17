package pg

import (
	"context"
	"io"
	"strings"
	"testing"
)

// localeRunner returns a fixed pg_database row and records the SQL it was asked.
type localeRunner struct {
	row string
	sql string
}

func (r *localeRunner) Run(_ context.Context, _ Conn, tool string, args []string, _ io.Reader, stdout io.Writer) error {
	r.sql = args[len(args)-1]
	io.WriteString(stdout, r.row+"\n")
	return nil
}
func (r *localeRunner) Describe() string { return "fake" }

func TestDatabaseLocaleByMajor(t *testing.T) {
	cases := []struct {
		major int
		row   string
		want  Locale
		col   string // column that must appear in the SQL
	}{
		{12, "UTF8\x1fC.UTF-8\x1fC.UTF-8\x1fc\x1f\x1f", Locale{Encoding: "UTF8", Collate: "C.UTF-8", Ctype: "C.UTF-8"}, "'c'"},
		{15, "UTF8\x1fen_US.utf8\x1fen_US.utf8\x1fi\x1fen-US\x1f", Locale{Encoding: "UTF8", Collate: "en_US.utf8", Ctype: "en_US.utf8", Provider: "icu", Locale: "en-US"}, "daticulocale"},
		{16, "UTF8\x1fC\x1fC\x1fi\x1fde-DE\x1f&a < b", Locale{Encoding: "UTF8", Collate: "C", Ctype: "C", Provider: "icu", Locale: "de-DE", ICURules: "&a < b"}, "daticurules"},
		{17, "UTF8\x1fC\x1fC\x1fb\x1fC.UTF-8\x1f", Locale{Encoding: "UTF8", Collate: "C", Ctype: "C", Provider: "builtin", Locale: "C.UTF-8"}, "datlocale"},
	}
	for _, c := range cases {
		r := &localeRunner{row: c.row}
		got, err := (&Client{Runner: r, Conn: Conn{Database: "postgres"}}).DatabaseLocale(context.Background(), "shop", c.major)
		if err != nil {
			t.Fatalf("major %d: %v", c.major, err)
		}
		if *got != c.want {
			t.Errorf("major %d: got %+v want %+v", c.major, *got, c.want)
		}
		if !strings.Contains(r.sql, c.col) || !strings.Contains(r.sql, "datname = 'shop'") {
			t.Errorf("major %d: sql %q", c.major, r.sql)
		}
	}
}

func TestLocaleCreateOptions(t *testing.T) {
	libc := Locale{Encoding: "UTF8", Collate: "C.UTF-8", Ctype: "C.UTF-8"}
	if got, err := libc.CreateOptions(12); err != nil || got != " encoding 'UTF8' lc_collate 'C.UTF-8' lc_ctype 'C.UTF-8'" {
		t.Errorf("libc on 12: %q, %v", got, err)
	}
	icu := Locale{Encoding: "UTF8", Collate: "C", Ctype: "C", Provider: "icu", Locale: "en-US"}
	if got, err := icu.CreateOptions(16); err != nil || got != " encoding 'UTF8' lc_collate 'C' lc_ctype 'C' locale_provider icu icu_locale 'en-US'" {
		t.Errorf("icu on 16: %q, %v", got, err)
	}
	if _, err := icu.CreateOptions(14); err == nil || !strings.Contains(err.Error(), "icu locale provider (PostgreSQL 15+); target is PostgreSQL 14") {
		t.Errorf("icu on 14: %v", err)
	}
	rules := icu
	rules.ICURules = "&a < b"
	if _, err := rules.CreateOptions(15); err == nil {
		t.Error("icu_rules on 15 should fail")
	}
	if got, _ := rules.CreateOptions(0); !strings.HasSuffix(got, " icu_rules '&a < b'") {
		t.Errorf("rules: %q", got)
	}
	builtin := Locale{Encoding: "UTF8", Collate: "C", Ctype: "C", Provider: "builtin", Locale: "C.UTF-8"}
	if got, err := builtin.CreateOptions(18); err != nil || !strings.HasSuffix(got, " locale_provider builtin builtin_locale 'C.UTF-8'") {
		t.Errorf("builtin on 18: %q, %v", got, err)
	}
	if _, err := builtin.CreateOptions(16); err == nil {
		t.Error("builtin on 16 should fail")
	}
	quote := Locale{Encoding: "UTF8", Collate: "it's", Ctype: "it's"}
	if got, _ := quote.CreateOptions(0); !strings.Contains(got, "lc_collate 'it''s'") {
		t.Errorf("quoting: %q", got)
	}
	if s := icu.String(); s != "UTF8, icu en-US" {
		t.Errorf("String: %q", s)
	}
	if s := libc.String(); s != "UTF8, C.UTF-8" {
		t.Errorf("String: %q", s)
	}
}
