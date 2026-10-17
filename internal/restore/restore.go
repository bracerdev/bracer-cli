package restore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"bracer.dev/bracer/internal/pg"
)

// GlobalsResult is the result of restoring globals.
type GlobalsResult struct {
	OK              bool     `json:"ok"`
	IgnoredErrors   int      `json:"ignored_errors,omitempty"`   // drill: "already exists" ignored
	Created         []string `json:"created,omitempty"`          // restore: users created
	SkippedExisting int      `json:"skipped_existing,omitempty"` // restore: users already present, left untouched
	SkippedGrants   []string `json:"skipped_grants,omitempty"`   // restore: "a -> x" skipped because a is the customer's pre-existing user
	Downgraded      []string `json:"downgraded,omitempty"`       // restore: "x: SUPERUSER" downgraded
	PgGrants        []string `json:"pg_grants,omitempty"`        // restore: "pg_read_all_data -> x"
	SkippedMissing  []string `json:"skipped_missing,omitempty"`  // restore: "pg_maintain -> x" skipped because the built-in pg_* role does not exist on the target
	Error           string   `json:"error,omitempty"`
}

// RowMismatch is a table whose row count differs from the manifest.
type RowMismatch struct {
	Table     string `json:"table"`
	Expected  int64  `json:"expected"`
	Found     int64  `json:"found"`
	Estimated bool   `json:"estimated,omitempty"` // compared via reltuples with a tolerance
	Extension string `json:"extension,omitempty"` // extension-owned table: warn only, do not fail
}

// Blocking: whether this mismatch makes restore/drill fail (an extension table does not).
func (m RowMismatch) Blocking() bool { return m.Extension == "" }

// Comparison is the result of comparing tables against the manifest.
type Comparison struct {
	Found      int
	Missing    []string
	Mismatches []RowMismatch
}

// OK when no table is missing and no regular table has a row mismatch; a mismatch
// in an extension table (content managed by the extension at run time) is only a warning.
func (c Comparison) OK() bool {
	if len(c.Missing) > 0 {
		return false
	}
	for _, m := range c.Mismatches {
		if m.Blocking() {
			return false
		}
	}
	return true
}

// StderrTail is the number of stderr lines kept in the error message.
const StderrTail = 20

// Globals runs globals.sql with psql -v ON_ERROR_STOP=0, ignoring "already
// exists" (the postgres user is pre-existing), recording other errors without blocking the database.
func Globals(ctx context.Context, runner pg.Runner, conn pg.Conn, src io.Reader) *GlobalsResult {
	gr := &GlobalsResult{}
	conn.Database = "postgres"
	stderr, err := pg.RunCapture(ctx, runner, conn, "psql", []string{"-X", "-q", "-v", "ON_ERROR_STOP=0", "-f", "-"}, src, io.Discard)
	ignored, other := splitErrors(stderr)
	gr.IgnoredErrors = ignored
	switch {
	case err != nil:
		gr.Error = "psql: " + Tail(stderr, StderrTail)
		if gr.Error == "psql: " {
			gr.Error = "psql: " + err.Error()
		}
	case len(other) > 0:
		gr.Error = strings.Join(other, "\n")
	default:
		gr.OK = true
	}
	return gr
}

// splitErrors counts psql ERROR lines: "already exists" is ignored, the rest are returned.
func splitErrors(stderr []byte) (ignored int, other []string) {
	for _, line := range strings.Split(string(stderr), "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "ERROR:") {
			continue
		}
		if strings.Contains(line, "already exists") {
			ignored++
			continue
		}
		other = append(other, line)
	}
	return ignored, other
}

// CreateDatabase creates the database from template0 (avoiding template1's
// locale/extensions) with the source's exact encoding/locale when the manifest has it (loc != nil;
// spec fetch-restore § Flow 5). targetMajor > 0 checks the provider before creating;
// 0 skips the check (drill: the chosen image is ≥ the source server's major).
func CreateDatabase(ctx context.Context, c *pg.Client, db string, loc *pg.Locale, targetMajor int) error {
	sql := "create database " + pg.QuoteIdent(db) + " template template0"
	if loc != nil {
		opts, err := loc.CreateOptions(targetMajor)
		if err != nil {
			return err
		}
		sql += opts
	}
	if _, err := c.Query(ctx, sql); err != nil {
		if loc != nil {
			return fmt.Errorf("%w (the source database has locale %s, which this server must support: install the locale there, or restore into a server that has it; the postgres:N-alpine image accepts any locale name)", err, loc)
		}
		return err
	}
	return nil
}

// DatabaseExists queries pg_database.
func DatabaseExists(ctx context.Context, c *pg.Client, db string) (bool, error) {
	rows, err := c.Query(ctx, "select 1 from pg_database where datname = "+pg.QuoteLiteral(db))
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}

// ErrTimeout: pg_restore exceeded the timeout.
var ErrTimeout = errors.New("pg_restore timed out")

// Database runs pg_restore -d db over stdin with a timeout. Errors keep the stderr tail
// plus a hint; timeout exceeded → ErrTimeout; parent ctx cancelled → ctx.Err().
func Database(ctx context.Context, runner pg.Runner, conn pg.Conn, db string, extra []string, src io.Reader, timeout time.Duration) error {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn.Database = db
	// libpq treats pg_restore's -d as a connection string when it contains "=" or starts
	// with postgres://; wrap it as dbname='…' so the database name is always a database name.
	args := append([]string{"-d", DBNameConninfo(db), "--no-data-for-failed-tables"}, extra...)
	stderr, err := pg.RunCapture(rctx, runner, conn, "pg_restore", args, src, io.Discard)
	if err == nil {
		return nil
	}
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(rctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("%w after %s", ErrTimeout, timeout)
	}
	msg := Tail(stderr, StderrTail)
	if msg == "" {
		msg = err.Error()
	}
	return fmt.Errorf("pg_restore: %s%s", msg, Hint(stderr))
}

// DBNameConninfo returns "dbname='<db>'" with \ and ' escaped per libpq conninfo syntax.
func DBNameConninfo(db string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	return "dbname='" + r.Replace(db) + "'"
}

// Compare: missing table → Missing; `rows` is compared exactly via count(*);
// `rows_est` is compared via reltuples after ANALYZE with a tolerance of max(10%, 100 rows).
func Compare(ctx context.Context, c *pg.Client, expected []pg.Table) (Comparison, error) {
	var cmp Comparison
	needAnalyze := false
	for _, e := range expected {
		if e.Rows == nil && e.RowsEst != nil {
			needAnalyze = true
		}
	}
	if needAnalyze {
		if _, err := c.Query(ctx, "analyze"); err != nil {
			return cmp, fmt.Errorf("analyze: %w", err)
		}
	}
	found, err := c.Tables(ctx)
	if err != nil {
		return cmp, err
	}
	have := make(map[string]pg.Table, len(found))
	for _, f := range found {
		have[f.Ident()] = f
	}
	for _, e := range expected {
		f, ok := have[e.Ident()]
		if !ok {
			cmp.Missing = append(cmp.Missing, e.Ident())
			continue
		}
		cmp.Found++
		switch {
		case e.Rows != nil:
			n, err := countRows(ctx, c, e)
			if err != nil {
				return cmp, err
			}
			if n != *e.Rows {
				cmp.Mismatches = append(cmp.Mismatches, RowMismatch{Table: e.Ident(), Expected: *e.Rows, Found: n, Extension: e.Extension})
			}
		case e.RowsEst != nil:
			got := int64(-1)
			if f.RowsEst != nil {
				got = *f.RowsEst
			}
			if !WithinTolerance(*e.RowsEst, got) {
				cmp.Mismatches = append(cmp.Mismatches, RowMismatch{Table: e.Ident(), Expected: *e.RowsEst, Found: got, Estimated: true, Extension: e.Extension})
			}
		}
	}
	return cmp, nil
}

func countRows(ctx context.Context, c *pg.Client, t pg.Table) (int64, error) {
	rows, err := c.Query(ctx, "select count(*) from "+t.Ident())
	if err != nil {
		return 0, fmt.Errorf("count %s: %w", t.Ident(), err)
	}
	if len(rows) == 0 || len(rows[0]) == 0 {
		return 0, fmt.Errorf("count %s: no result", t.Ident())
	}
	var n int64
	if _, err := fmt.Sscan(rows[0][0], &n); err != nil {
		return 0, fmt.Errorf("count %s: bad result %q", t.Ident(), rows[0][0])
	}
	return n, nil
}

// WithinTolerance: |found − expected| ≤ max(10% expected, 100).
func WithinTolerance(expected, found int64) bool {
	tol := expected / 10
	if tol < 100 {
		tol = 100
	}
	diff := found - expected
	if diff < 0 {
		diff = -diff
	}
	return diff <= tol
}

// Tail keeps the last n lines, dropping blank lines.
func Tail(b []byte, n int) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var kept []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			kept = append(kept, strings.TrimRight(l, "\r"))
		}
	}
	if len(kept) > n {
		kept = kept[len(kept)-n:]
	}
	return strings.Join(kept, "\n")
}

// Hint suggests a fix for common pg_restore errors (spec drill, fetch-restore § Edge cases).
func Hint(stderr []byte) string {
	switch {
	case bytes.Contains(stderr, []byte("No space left")):
		return "\nhint: Docker is out of disk space; check with: docker system df"
	case bytes.Contains(stderr, []byte("extension")) && bytes.Contains(stderr, []byte("not available")),
		bytes.Contains(stderr, []byte("could not open extension control file")):
		return "\nhint: the dump needs extensions this image lacks; set drill.image in app.yaml to an image that has them"
	case bytes.Contains(stderr, []byte("role")) && bytes.Contains(stderr, []byte("does not exist")):
		return "\nhint: users from the source server are missing; restore them first with --with-globals, or pass --no-owner"
	}
	return ""
}

// ListRoles returns the names of every user on the target (built-in pg_* roles
// included), so GlobalsCreateMissing knows which ones already exist.
func ListRoles(ctx context.Context, c *pg.Client) ([]string, error) {
	rows, err := c.Query(ctx, "select rolname from pg_roles order by rolname")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, r := range rows {
		if len(r) > 0 && r[0] != "" {
			names = append(names, r[0])
		}
	}
	return names, nil
}

// MaxGlobals is the maximum size of globals.sql read into memory (a few KB per user).
const MaxGlobals = 16 << 20

// GlobalsCreateMissing is the variant for restoring into a real server: filter with
// FilterGlobals, then run psql in a single transaction with ON_ERROR_STOP=1: create all
// missing users or create nothing. A file it cannot understand → nothing runs, Error
// states why. drill uses the unfiltered Globals.
func GlobalsCreateMissing(ctx context.Context, runner pg.Runner, conn pg.Conn, existing []string, src io.Reader) *GlobalsResult {
	gr := &GlobalsResult{}
	data, err := io.ReadAll(io.LimitReader(src, MaxGlobals+1))
	if err != nil {
		gr.Error = "read globals: " + err.Error()
		return gr
	}
	if len(data) > MaxGlobals {
		gr.Error = "globals dump larger than 16 MB; refusing to parse"
		return gr
	}
	have := map[string]bool{}
	for _, r := range existing {
		have[r] = true
	}
	plan, err := FilterGlobals(data, have)
	if err != nil {
		gr.Error = err.Error() + "; no user was created"
		return gr
	}
	gr.SkippedExisting, gr.SkippedGrants, gr.Downgraded, gr.PgGrants, gr.SkippedMissing = plan.SkippedExisting, plan.SkippedGrants, plan.Downgraded, plan.PgGrants, plan.SkippedMissing
	if len(plan.Created) == 0 {
		gr.OK = true
		return gr
	}
	conn.Database = "postgres"
	stderr, err := pg.RunCapture(ctx, runner, conn, "psql", []string{"-X", "-q", "-v", "ON_ERROR_STOP=1", "--single-transaction", "-f", "-"}, bytes.NewReader(plan.SQL), io.Discard)
	if err != nil {
		gr.Error = "psql: " + Tail(stderr, StderrTail)
		if gr.Error == "psql: " {
			gr.Error = "psql: " + err.Error()
		}
		gr.Error += "; the transaction was rolled back, no user was created"
		return gr
	}
	gr.Created = plan.Created
	gr.OK = true
	return gr
}

// DiskCheck is the result of measuring free disk on the target before restoring.
type DiskCheck struct {
	Checked       bool   `json:"checked"`
	DataDirectory string `json:"data_directory,omitempty"`
	FreeBytes     int64  `json:"free_bytes,omitempty"`
	NeedBytes     int64  `json:"need_bytes,omitempty"`
	Error         string `json:"error,omitempty"`
}

// DiskFactor: actual data ≈ DiskFactor × the compressed -Fc dump.
const DiskFactor = 5

// ErrDiskSpace: the target does not have enough free space.
var ErrDiskSpace = errors.New("not enough disk space on target")

// CheckDisk queries data_directory, then measures df through the runner if the runner can.
// Cannot measure → Checked=false with Error set; not a failure.
func CheckDisk(ctx context.Context, runner pg.Runner, admin *pg.Client, dumpBytes int64) DiskCheck {
	dc := DiskCheck{NeedBytes: dumpBytes * DiskFactor}
	df, ok := runner.(pg.DiskFreer)
	if !ok {
		dc.Error = "could not check free disk space on target (tools run on another machine)"
		return dc
	}
	if _, local := runner.(pg.LocalRunner); local && !pg.IsLocalHost(admin.Conn.Host) {
		dc.Error = fmt.Sprintf("could not check free disk space on target (%s is not this machine)", admin.Conn.Host)
		return dc
	}
	rows, err := admin.Query(ctx, "show data_directory")
	if err != nil || len(rows) == 0 || len(rows[0]) == 0 || rows[0][0] == "" {
		dc.Error = "could not read data_directory from target"
		return dc
	}
	dc.DataDirectory = rows[0][0]
	free, err := df.DiskFree(ctx, dc.DataDirectory)
	if err != nil {
		dc.Error = fmt.Sprintf("could not check free disk space at %s: %v", dc.DataDirectory, err)
		return dc
	}
	dc.Checked, dc.FreeBytes = true, free
	return dc
}

// Enough reports whether the disk suffices (or could not be measured).
func (dc DiskCheck) Enough() bool { return !dc.Checked || dc.FreeBytes >= dc.NeedBytes }
