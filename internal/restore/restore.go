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

// RowMismatch is a table whose row count differs from run.json.
type RowMismatch struct {
	Table     string `json:"table"`
	Expected  int64  `json:"expected"`
	Found     int64  `json:"found"`
	Estimated bool   `json:"estimated,omitempty"` // compared via reltuples with a tolerance
	Extension string `json:"extension,omitempty"` // extension-owned table: warn only, do not fail
}

// Blocking: whether this mismatch makes restore/drill fail (an extension table does not).
func (m RowMismatch) Blocking() bool { return m.Extension == "" }

// Comparison is the result of comparing tables against run.json.
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

// Hint suggests a fix for common pg_restore errors (spec drill, restore § Edge cases).
func Hint(stderr []byte) string {
	switch {
	case bytes.Contains(stderr, []byte("No space left")):
		return "\nhint: Docker is out of disk space; check with: docker system df"
	case bytes.Contains(stderr, []byte("extension")) && bytes.Contains(stderr, []byte("not available")),
		bytes.Contains(stderr, []byte("could not open extension control file")):
		return "\nhint: the dump needs extensions this image lacks; set drill.image of the instance in app.yaml to an image that has them"
	case bytes.Contains(stderr, []byte("role")) && bytes.Contains(stderr, []byte("does not exist")):
		return "\nhint: users from the source server are missing on this server"
	}
	return ""
}

// MaxGlobals is the maximum size of globals.sql read into memory (a few KB per user).
const MaxGlobals = 16 << 20

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

// CheckDisk queries data_directory, then measures df through the runner if the runner can.
// Cannot measure → Checked=false with Error set; not a failure.
func CheckDisk(ctx context.Context, runner pg.Runner, admin *pg.Client, dumpBytes int64) DiskCheck {
	dc := DiskCheck{NeedBytes: dumpBytes * DiskFactor}
	df, ok := runner.(pg.DiskFreer)
	if !ok {
		dc.Error = "could not check free disk space on target (tools run on another machine)"
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
