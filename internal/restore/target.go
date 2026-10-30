package restore

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// maxIdent is PostgreSQL's NAMEDATALEN - 1.
const maxIdent = 63

// tsLayout is the dump timestamp in the manifest (2026-10-16T030000Z).
const tsLayout = "2006-01-02T150405Z"

// SideName is the name of a database restored next to another one:
// <db>_<kind>_<YYYYMMDD_HHMM> of the backup's time (UTC), then _2, _3… while
// taken says the name exists. <db> is cut (never mid-character) to stay within
// 63 bytes.
func SideName(db, kind string, at time.Time, taken func(string) (bool, error)) (string, error) {
	// UTC, said so: run ids are UTC keys, while the CLI prints local time.
	base := "_" + kind + "_" + at.UTC().Format("20060102_1504Z")
	for n := 1; n < 100; n++ {
		suffix := base
		if n > 1 {
			suffix += fmt.Sprintf("_%d", n)
		}
		name := cutBytes(db, maxIdent-len(suffix)) + suffix
		ok, err := taken(name)
		if err != nil {
			return "", err
		}
		if !ok {
			return name, nil
		}
	}
	return "", fmt.Errorf("no free name for %s%s", db, base)
}

// cutBytes shortens s to at most n bytes without splitting a UTF-8 character.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// backupTime parses the manifest timestamp; "" or unknown → now.
func backupTime(ts string, now time.Time) time.Time {
	if t, err := time.Parse(tsLayout, ts); err == nil {
		return t
	}
	return now
}

func column(rows [][]string) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if len(r) > 0 {
			out = append(out, r[0])
		}
	}
	return out
}

// listFirst joins up to n names, then "(+k more)".
func listFirst(names []string, n int) string {
	if len(names) <= n {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:n], ", ") + fmt.Sprintf(" (+%d more)", len(names)-n)
}
