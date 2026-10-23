package pg

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Session is a long-lived psql session through Runner: stdin stays open to send
// many statements, results separated by a marker. Used to hold a REPEATABLE READ
// transaction and pg_export_snapshot() while pg_dump --snapshot runs (spec backup).
type Session struct {
	stdin  io.WriteCloser
	out    *bufio.Reader
	done   chan error
	mu     sync.Mutex
	closed bool
}

const sessionMarker = "__BRACER_END__"

// StartSession opens psql -At -q on conn and waits until it is ready.
func StartSession(ctx context.Context, runner Runner, conn Conn) (*Session, error) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &Session{stdin: inW, out: bufio.NewReaderSize(outR, 1<<20), done: make(chan error, 1)}
	// exec copies stdin with a goroutine reading inR; when ctx is cancelled the
	// process is killed but that goroutine stays stuck in Read (nobody closes the
	// pipe) and Wait never returns. Close inR as soon as ctx is cancelled so it exits.
	runCtx, stopWatch := context.WithCancel(ctx)
	go func() {
		<-runCtx.Done()
		inR.CloseWithError(runCtx.Err())
	}()
	go func() {
		args := []string{"-X", "-A", "-t", "-q", "-F", "\x1f", "-0", "-v", "ON_ERROR_STOP=0", "-f", "-"}
		err := runner.Run(ctx, conn, "psql", args, inR, outW)
		stopWatch()
		outW.CloseWithError(err)
		inR.CloseWithError(err)
		s.done <- err
	}()
	// The first statement checks that the session is alive.
	if _, err := s.Exec("select 1"); err != nil {
		s.Close()
		return nil, fmt.Errorf("open psql session: %w", err)
	}
	return s, nil
}

// Exec sends one SQL statement and returns the result rows (see parseRows).
// An SQL error is recorded by psql in :LAST_ERROR_MESSAGE and returned as an error.
func (s *Session) Exec(sql string) ([][]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("session closed")
	}
	sql = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sql), ";"))
	if _, err := fmt.Fprintf(s.stdin, "%s;\n\\echo %s :ERROR :LAST_ERROR_MESSAGE\n", sql, sessionMarker); err != nil {
		return nil, s.runErr(err)
	}
	// Rows end with NUL and may hold newlines; the marker line follows the
	// last NUL (or comes first when there is no row).
	var buf strings.Builder
	for {
		chunk, err := s.out.ReadString('\n')
		if err != nil {
			return nil, s.runErr(err)
		}
		buf.WriteString(chunk)
		all := buf.String()
		at := -1
		if strings.HasPrefix(all, sessionMarker) {
			at = 0
		} else if i := strings.LastIndex(all, "\x00"+sessionMarker); i >= 0 {
			at = i + 1
		}
		if at < 0 {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(all[at:], sessionMarker))
		if strings.HasPrefix(rest, "true") {
			return nil, fmt.Errorf("%s", strings.TrimSpace(strings.TrimPrefix(rest, "true")))
		}
		return parseRows(all[:at]), nil
	}
}

// Scalar is Exec taking the first cell.
func (s *Session) Scalar(sql string) (string, error) {
	rows, err := s.Exec(sql)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 || len(rows[0]) == 0 {
		return "", fmt.Errorf("%s: no result", sql)
	}
	return rows[0][0], nil
}

// runErr prefers the psql process's error (which carries stderr) if it has exited.
func (s *Session) runErr(err error) error {
	select {
	case rerr := <-s.done:
		s.done <- rerr
		if rerr != nil {
			return rerr
		}
	default:
	}
	return err
}

// Close closes stdin (psql exits, an uncommitted transaction rolls back) and waits.
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	s.stdin.Close()
	err := <-s.done
	s.done <- err
	return err
}

// Snapshot is a session holding a REPEATABLE READ transaction with an exported
// snapshot: row counts and pg_dump --snapshot see the same data state.
type Snapshot struct {
	*Session
	ID string
}

// BeginSnapshot opens a session, sets lock_timeout (0 = not set) and
// client_connection_check_interval, then begins a REPEATABLE READ transaction and
// exports a snapshot. Both SETs must come BEFORE begin: the second parameter exists
// only from PG14 and its error can be ignored — but a failing statement inside a
// transaction aborts it and every later statement fails (every backup on PG12/13 fails).
func BeginSnapshot(ctx context.Context, runner Runner, conn Conn, lockTimeout time.Duration) (*Snapshot, error) {
	s, err := StartSession(ctx, runner, conn)
	if err != nil {
		return nil, err
	}
	if lockTimeout > 0 {
		if _, err := s.Exec(fmt.Sprintf("set lock_timeout = '%dms'", lockTimeout.Milliseconds())); err != nil {
			s.Close()
			return nil, fmt.Errorf("set lock_timeout: %w", err)
		}
	}
	// When Bracer goes away (timeout), count(*) on the server stops too instead of
	// scanning on while holding locks. PG < 14 lacks this parameter: error, ignored.
	s.Exec("set client_connection_check_interval = '10s'")
	if _, err := s.Exec("begin isolation level repeatable read read only"); err != nil {
		s.Close()
		return nil, fmt.Errorf("begin snapshot transaction: %w", err)
	}
	id, err := s.Scalar("select pg_export_snapshot()")
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("pg_export_snapshot: %w", err)
	}
	return &Snapshot{Session: s, ID: id}, nil
}

// Tables lists the tables in the snapshot (same query as Client.Tables).
func (s *Snapshot) Tables() ([]Table, error) {
	rows, err := s.Exec(tablesSQL)
	if err != nil {
		return nil, err
	}
	return parseTables(rows), nil
}

// Count counts a table's rows exactly within the snapshot.
func (s *Snapshot) Count(t Table) (int64, error) {
	v, err := s.Scalar("select count(*) from " + t.Ident())
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("count %s: bad result %q", t.Ident(), v)
	}
	return n, nil
}

// End COMMITs, then closes the session; the snapshot is no longer valid afterwards.
func (s *Snapshot) End() error {
	if _, err := s.Exec("commit"); err != nil {
		s.Close()
		return err
	}
	return s.Close()
}

// Args returns the pg_dump flags to dump within this snapshot.
func (s *Snapshot) Args() []string { return []string{"--snapshot=" + s.ID} }
