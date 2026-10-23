package restore

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/storage"
)

// The steps restore and drill share once a plan is ok (ADR-0015 § 3–4):
// load each database in one transaction, ANALYZE, then prove it.

// ApplySQL runs statements with psql -v ON_ERROR_STOP=1 on the maintenance
// database; drill applies env-up and env-down this way, and both create the
// new databases this way.
func ApplySQL(ctx context.Context, runner pg.Runner, conn pg.Conn, stmts []string) error {
	if len(stmts) == 0 {
		return nil
	}
	conn.Database = "postgres"
	stderr, err := pg.RunCapture(ctx, runner, conn, "psql", []string{"-X", "-q", "-v", "ON_ERROR_STOP=1", "-f", "-"}, strings.NewReader(SQLText(stmts)), io.Discard)
	if err != nil {
		msg := Tail(stderr, StderrTail)
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("psql: %s", msg)
	}
	return nil
}

// LoadNew creates the new database of dp (dp.Create) and loads the dump into
// it (LoadDB). CREATE DATABASE runs alone first: when it fails nothing was
// created and nothing is dropped, so a database of that name that already
// exists is left alone (*ErrNotCreated). When anything after it fails before
// the load committed, the database this run created is dropped again
// (ADR-0022). A database that could not be dropped is an *ErrLeft.
func LoadNew(ctx context.Context, st storage.Store, dump *backup.Dump, dp *DBPlan, ids *Identities, runner pg.Runner, conn pg.Conn, mode string, major int, timeout time.Duration) error {
	if len(dp.Create) == 0 {
		return fmt.Errorf("create %s: no statement planned", dp.Target)
	}
	if err := ApplySQL(ctx, runner, conn, dp.Create[:1]); err != nil {
		return &ErrNotCreated{Target: dp.Target, Err: err}
	}
	err := ApplySQL(ctx, runner, conn, dp.Create[1:])
	if err == nil {
		err = LoadDB(ctx, st, dump, ids, runner, conn, dp.Target, mode, timeout)
		var after *ErrAfterLoad
		var committed *ErrCommitted
		if err == nil || errors.As(err, &after) || errors.As(err, &committed) {
			return err
		}
	} else {
		err = fmt.Errorf("set up %s: %w", dp.Target, err)
	}
	// Ctrl-C cancelled ctx: the drop still runs.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	drop := DropDB(dp.Target, major)
	if derr := ApplySQL(dctx, runner, conn, []string{drop}); derr != nil {
		return &ErrLeft{Err: err, Drop: drop}
	}
	return err
}

// ErrNotCreated: CREATE DATABASE failed, so there is nothing to drop.
type ErrNotCreated struct {
	Target string
	Err    error
}

func (e *ErrNotCreated) Error() string { return "create " + e.Target + ": " + e.Err.Error() }

func (e *ErrNotCreated) Unwrap() error { return e.Err }

// ErrLeft: the load failed and the database it created could not be dropped.
type ErrLeft struct {
	Err  error
	Drop string // for the customer to run
}

func (e *ErrLeft) Error() string {
	return e.Err.Error() + "; the new database could not be dropped: run " + e.Drop
}

func (e *ErrLeft) Unwrap() error { return e.Err }

// LoadDB streams one dump into a database restore just created with
// pg_restore --single-transaction: any error leaves it empty. Data mode
// adds --no-owner --no-acl. ANALYZE follows.
//
// The SHA-256 of the object is only known at its end, so the last
// heldTail bytes reach pg_restore only once the whole object has been read
// and matched: without them pg_restore cannot finish, so it cannot commit a
// dump that does not match (spec restore § Load). If it committed all
// the same, the error is *ErrCommitted.
func LoadDB(ctx context.Context, st storage.Store, dp *backup.Dump, ids *Identities, runner pg.Runner, conn pg.Conn, target, mode string, timeout time.Duration) error {
	src, err := OpenDump(ctx, st, dp, ids)
	if err != nil {
		return err
	}
	defer src.Close()
	body := bufio.NewReader(&holdTail{r: src, verify: func() error { return finish(src) }})
	if head, _ := body.Peek(len(pgdmpMagic)); !bytes.Equal(head, []byte(pgdmpMagic)) {
		return fmt.Errorf("%s is not a pg_dump custom-format archive", dp.Key)
	}
	extra := []string{"--single-transaction"}
	if mode == ModeData {
		extra = append(extra, "--no-owner", "--no-acl")
	}
	c := &pg.Client{Runner: runner, Conn: conn.WithDatabase(target)}
	ts := timescale(dp.Extensions)
	if ts != nil {
		if _, err := c.Query(ctx, "alter database "+pg.QuoteIdent(target)+" set timescaledb.restoring = on"); err != nil {
			return fmt.Errorf("set timescaledb.restoring before the load: %w", err)
		}
	}
	rerr := Database(ctx, runner, conn, target, extra, body, timeout)
	var after error
	if ts != nil {
		if _, err := c.Query(ctx, "alter database "+pg.QuoteIdent(target)+" reset timescaledb.restoring"); err != nil {
			after = fmt.Errorf("reset timescaledb.restoring: %w", err)
		}
	}
	if rerr != nil {
		if ce := checksumErr(src); ce != nil {
			return ce
		}
		return rerr
	}
	// pg_restore may stop before the end of the object: check the rest.
	if err := finish(src); err != nil {
		return &ErrCommitted{Err: err}
	}
	if after == nil && ts != nil {
		if _, err := c.Query(ctx, "select "+pg.QuoteIdent(ts.Schema)+".timescaledb_post_restore()"); err != nil {
			after = fmt.Errorf("timescaledb_post_restore: %w", err)
		}
	}
	if after == nil {
		if _, err := c.Query(ctx, "analyze"); err != nil {
			after = fmt.Errorf("analyze: %w", err)
		}
	}
	if after != nil {
		return &ErrAfterLoad{Err: after}
	}
	return nil
}

// timescale is the database's timescaledb: its chunks load only with
// timescaledb.restoring on, as timescaledb_pre_restore() sets it.
func timescale(exts []pg.Extension) *pg.Extension {
	for i := range exts {
		if exts[i].Name == "timescaledb" {
			e := exts[i]
			if e.Schema == "" {
				e.Schema = "public"
			}
			return &e
		}
	}
	return nil
}

// ErrAfterLoad: pg_restore committed and the dump matched, then a step after
// the load failed; the database holds the data.
type ErrAfterLoad struct{ Err error }

func (e *ErrAfterLoad) Error() string { return e.Err.Error() + " after the load" }

func (e *ErrAfterLoad) Unwrap() error { return e.Err }

// ErrCommitted: pg_restore committed, then the dump turned out not to match
// its checksum. The database holds what was loaded.
type ErrCommitted struct{ Err error }

func (e *ErrCommitted) Error() string {
	return e.Err.Error() + " after pg_restore committed"
}

func (e *ErrCommitted) Unwrap() error { return e.Err }

// heldTail is how much of the end of a dump holdTail keeps from pg_restore
// until the checksum is known. Any amount stops pg_restore from reaching the
// end of the last data block; 64 KiB is a margin.
const heldTail = 64 << 10

// holdTail passes r through but keeps its last heldTail bytes until r is at
// its end and verify returns nil. When verify fails the reader returns its
// error and the end is never passed on.
type holdTail struct {
	r      io.Reader
	verify func() error
	buf    []byte
	eof    bool
	err    error
}

func (h *holdTail) Read(p []byte) (int, error) {
	for {
		switch {
		case h.err != nil:
			return 0, h.err
		case h.eof && len(h.buf) == 0:
			return 0, io.EOF
		case h.eof:
			n := copy(p, h.buf)
			h.buf = h.buf[n:]
			return n, nil
		case len(h.buf) > heldTail:
			n := copy(p, h.buf[:len(h.buf)-heldTail])
			h.buf = h.buf[n:]
			return n, nil
		}
		chunk := make([]byte, 32<<10)
		n, err := h.r.Read(chunk)
		h.buf = append(h.buf, chunk[:n]...)
		switch {
		case err == io.EOF:
			if h.err = h.verify(); h.err == nil {
				h.eof = true
			}
		case err != nil:
			h.err = err
		}
	}
}

// AppCheck is connecting the way the app does (ADR-0015 § 3).
type AppCheck struct {
	OK       bool   `json:"ok"`
	User     string `json:"user,omitempty"`
	Database string `json:"database,omitempty"`
	Network  string `json:"network,omitempty"`
	// Tables are the run's tables; Readable those the app's user has
	// SELECT on, in a schema it has USAGE on (privileges came with the dump),
	// each read once.
	Tables   int    `json:"tables"`
	Readable int    `json:"readable"`
	Error    string `json:"error,omitempty"`
}

// CheckApp connects with the app's URL from network (a container's network,
// or host) and reads every table of the run its user can read: SELECT on the
// table and USAGE on its schema (pg_cron grants SELECT on cron.job to PUBLIC
// but no USAGE on cron). The URL names the source database as the app does;
// when restore loaded it under another name (<db>_restored_<ts>), renamed
// maps it there and the check runs on the loaded database (ADR-0016).
func CheckApp(ctx context.Context, sh pg.Shell, image, network, rawURL string, loaded map[string]*backup.Dump, renamed map[string]string) *AppCheck {
	ac := &AppCheck{Network: network}
	conn, err := pg.ParseURL(rawURL)
	if err != nil {
		ac.Error = "app URL: " + err.Error()
		return ac
	}
	if t, ok := renamed[conn.Database]; ok {
		conn.Database = t
	}
	ac.Database, ac.User = conn.Database, conn.User
	dp, ok := loaded[conn.Database]
	if !ok {
		ac.Error = fmt.Sprintf("the app URL names database %s, which this restore did not load (%s)", conn.Database, strings.Join(sortedKeys(loaded), ", "))
		return ac
	}
	c := &pg.Client{Runner: pg.ImageRunner{Shell: sh, Image: image, Network: network}, Conn: conn}
	var sql strings.Builder
	sql.WriteString("select t from (values ")
	for i, t := range dp.Tables {
		if i > 0 {
			sql.WriteString(", ")
		}
		sql.WriteString("(" + pg.QuoteLiteral(t.Ident()) + ", " + pg.QuoteLiteral(t.Schema) + ")")
	}
	sql.WriteString(") v(t, s) where has_schema_privilege(s, 'USAGE') and has_table_privilege(t, 'SELECT')")
	query := "select 1"
	if len(dp.Tables) > 0 {
		query = sql.String()
	}
	rows, err := c.Query(ctx, query)
	if err != nil {
		ac.Error = firstLineOf(errText(err))
		return ac
	}
	var reads strings.Builder
	n := 0
	for _, r := range rows {
		if len(dp.Tables) > 0 && len(r) == 1 {
			reads.WriteString("select 1 from " + r[0] + " limit 0;\n")
			n++
		}
	}
	ac.Tables = len(dp.Tables)
	if n > 0 {
		stderr, err := pg.RunCapture(ctx, c.Runner, conn, "psql", []string{"-X", "-q", "-v", "ON_ERROR_STOP=1", "-f", "-"}, strings.NewReader(reads.String()), io.Discard)
		if err != nil {
			msg := Tail(stderr, 3)
			if msg == "" {
				msg = err.Error()
			}
			ac.Error = msg
			return ac
		}
	}
	ac.OK, ac.Readable = true, n
	return ac
}
