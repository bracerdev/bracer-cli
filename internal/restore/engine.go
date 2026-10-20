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
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/storage"
)

// The steps restore and drill share once a plan is ok (ADR-0015 § 3–4):
// load each database in one transaction, ANALYZE, then prove it.

// ApplySQL runs statements with psql -v ON_ERROR_STOP=1 on the maintenance
// database; drill applies env-up, data-down and env-down this way.
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

// LoadDB streams one dump into an existing empty database with
// pg_restore --single-transaction: any error leaves nothing behind. Data mode
// adds --no-owner --no-acl. ANALYZE follows.
func LoadDB(ctx context.Context, st storage.Store, dp *backup.Dump, ids *Identities, runner pg.Runner, conn pg.Conn, target, mode string, timeout time.Duration) error {
	src, err := OpenDump(ctx, st, dp, ids)
	if err != nil {
		return err
	}
	defer src.Close()
	body := bufio.NewReader(src)
	if head, _ := body.Peek(len(pgdmpMagic)); !bytes.Equal(head, []byte(pgdmpMagic)) {
		return fmt.Errorf("%s is not a pg_dump custom-format archive", dp.Key)
	}
	extra := []string{"--single-transaction"}
	if mode == ModeData {
		extra = append(extra, "--no-owner", "--no-acl")
	}
	rerr := Database(ctx, runner, conn, target, extra, body, timeout)
	if ce := checksumErr(src); ce != nil {
		rerr = ce
	}
	if rerr != nil {
		return rerr
	}
	c := &pg.Client{Runner: runner, Conn: conn.WithDatabase(target)}
	if _, err := c.Query(ctx, "analyze"); err != nil {
		return fmt.Errorf("analyze: %w", err)
	}
	return nil
}

// CheckResult is the outcome of one drill.checks entry.
type CheckResult struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// RunChecks runs the drill.checks entries for database db: ≥ 1 row passes.
// Results are not logged (security NFR).
func RunChecks(ctx context.Context, c *pg.Client, cfg *config.App, db string) []CheckResult {
	if cfg.Drill == nil {
		return nil
	}
	var out []CheckResult
	for _, chk := range cfg.Drill.Checks {
		if !chk.AppliesTo(db) {
			continue
		}
		cr := CheckResult{Name: chk.Name}
		rows, err := c.Query(ctx, chk.SQL)
		switch {
		case err != nil:
			cr.Error = firstLineOf(errText(err))
		case len(rows) == 0:
			cr.Error = "no rows returned"
		default:
			cr.OK = true
		}
		out = append(out, cr)
	}
	return out
}

// AppCheck is connecting the way the app does (ADR-0015 § 3).
type AppCheck struct {
	OK       bool   `json:"ok"`
	User     string `json:"user,omitempty"`
	Database string `json:"database,omitempty"`
	Network  string `json:"network,omitempty"`
	// Tables are the manifest tables; Readable those the app's user has
	// SELECT on (privileges came with the dump), each read once.
	Tables   int    `json:"tables"`
	Readable int    `json:"readable"`
	Error    string `json:"error,omitempty"`
}

// CheckApp connects with the app's URL from network (a container's network,
// or host) and reads every manifest table its user has SELECT on.
func CheckApp(ctx context.Context, sh pg.Shell, image, network, rawURL string, loaded map[string]*backup.Dump) *AppCheck {
	ac := &AppCheck{Network: network}
	conn, err := pg.ParseURL(rawURL)
	if err != nil {
		ac.Error = "app URL: " + err.Error()
		return ac
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
		sql.WriteString("(" + pg.QuoteLiteral(t.Ident()) + ")")
	}
	sql.WriteString(") v(t) where has_table_privilege(t, 'SELECT')")
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

// ErrVerify: the data is loaded but something does not match the backup.
var ErrVerify = errors.New("the restored databases do not match the backup")
