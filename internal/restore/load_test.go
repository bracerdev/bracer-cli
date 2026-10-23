package restore

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/crypto"
	"bracer.dev/bracer/internal/pg"
)

// restoreRunner fakes pg_restore: it reads stdin, up to limit bytes when
// limit > 0, and commits when that read ends without error, as pg_restore
// does when it reaches the end of the last data block.
type restoreRunner struct {
	limit     int64
	read      int64
	committed bool
}

func (r *restoreRunner) Run(_ context.Context, _ pg.Conn, tool string, _ []string, stdin io.Reader, _ io.Writer) error {
	if tool != "pg_restore" {
		return nil // ANALYZE
	}
	src := stdin
	if r.limit > 0 {
		src = io.LimitReader(stdin, r.limit)
	}
	n, err := io.Copy(io.Discard, src)
	r.read = n
	if err != nil {
		return &pg.ExitError{Cmd: "pg_restore", Code: 1, Stderr: []byte("pg_restore: error: could not read from input file: end of file")}
	}
	r.committed = true
	return nil
}

func (r *restoreRunner) Describe() string { return "fake" }

// spec restore § Load: pg_restore never commits a dump whose SHA-256
// does not match; when it committed all the same, the error says so.
func TestLoadDBChecksumBeforeCommit(t *testing.T) {
	ctx := context.Background()
	plain := make([]byte, 300<<10)
	rand.Read(plain)
	copy(plain, pgdmpMagic)

	kp, _ := crypto.Generate()
	keyPath := filepath.Join(t.TempDir(), "k")
	crypto.WriteIdentityFile(keyPath, kp)
	ids := ReadIdentities(keyPath, crypto.ReadIdentities)
	var enc bytes.Buffer
	w, err := crypto.Encrypt(&enc, kp.Recipient)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(plain)
	w.Close()

	for _, c := range []struct {
		name      string
		obj       []byte
		encrypted bool
	}{{"plain", plain, false}, {"age", enc.Bytes(), true}} {
		st := newMemStore()
		st.Put(ctx, "k", bytes.NewReader(c.obj), int64(len(c.obj)))
		dp := &backup.Dump{Key: "k", SHA256: sha(c.obj), Encrypted: c.encrypted}
		if c.encrypted {
			dp.Recipient = &kp.Recipient
		}

		r := &restoreRunner{}
		if err := LoadDB(ctx, st, dp, ids, r, pg.Conn{}, "shop", ModeFull, 0); err != nil || !r.committed || r.read != int64(len(plain)) {
			t.Errorf("%s matching: %v committed=%v read=%d", c.name, err, r.committed, r.read)
		}

		dp.SHA256 = "deadbeef"
		r = &restoreRunner{}
		err := LoadDB(ctx, st, dp, ids, r, pg.Conn{}, "shop", ModeFull, 0)
		var ce *ErrChecksum
		if !errors.As(err, &ce) || errors.As(err, new(*ErrCommitted)) || r.committed {
			t.Errorf("%s mismatch: %v committed=%v", c.name, err, r.committed)
		}
		if r.read > int64(len(plain)-heldTail) {
			t.Errorf("%s mismatch: pg_restore read %d bytes, the last %d must be held back", c.name, r.read, heldTail)
		}

		// pg_restore that stops reading early and commits: the rest is
		// still checked, and the error says the database was loaded.
		r = &restoreRunner{limit: 1000}
		err = LoadDB(ctx, st, dp, ids, r, pg.Conn{}, "shop", ModeFull, 0)
		if !errors.As(err, new(*ErrCommitted)) || !errors.As(err, &ce) || !r.committed {
			t.Errorf("%s committed then mismatch: %v", c.name, err)
		}
	}
}

func TestHoldTailSmallInput(t *testing.T) {
	h := &holdTail{r: bytes.NewReader([]byte("PGDMP-small")), verify: func() error { return nil }}
	got, err := io.ReadAll(h)
	if err != nil || string(got) != "PGDMP-small" {
		t.Errorf("small: %q %v", got, err)
	}
	h = &holdTail{r: bytes.NewReader([]byte("PGDMP-small")), verify: func() error { return &ErrChecksum{} }}
	if got, err := io.ReadAll(h); err == nil || len(got) != 0 {
		t.Errorf("small mismatch: %q %v", got, err)
	}
}

// sqlLog fakes psql and pg_restore for the steps around the load and
// records them.
type sqlLog struct {
	log      []string
	failLoad bool
}

func (r *sqlLog) Run(_ context.Context, _ pg.Conn, tool string, args []string, stdin io.Reader, _ io.Writer) error {
	if tool == "pg_restore" {
		io.Copy(io.Discard, stdin)
		r.log = append(r.log, "pg_restore")
		if r.failLoad {
			return &pg.ExitError{Cmd: "pg_restore", Code: 1, Stderr: []byte("pg_restore: error: COPY failed")}
		}
		return nil
	}
	r.log = append(r.log, args[len(args)-1])
	return nil
}

func (r *sqlLog) Describe() string { return "fake" }

// spec restore § Load: timescaledb loads with restoring on, then
// timescaledb_post_restore().
func TestLoadDBTimescale(t *testing.T) {
	ctx := context.Background()
	obj := append([]byte(pgdmpMagic), make([]byte, 100)...)
	dp := &backup.Dump{Key: "k", SHA256: sha(obj), Extensions: []pg.Extension{
		{Name: "timescaledb", Version: "2.29.2", Schema: "public", Config: true},
		{Name: "uuid-ossp", Version: "1.1", Schema: "public"}}}
	st := newMemStore()
	st.Put(ctx, "k", bytes.NewReader(obj), int64(len(obj)))

	r := &sqlLog{}
	if err := LoadDB(ctx, st, dp, nil, r, pg.Conn{}, "metrics", ModeFull, 0); err != nil {
		t.Fatal(err)
	}
	want := `alter database "metrics" set timescaledb.restoring = on
pg_restore
alter database "metrics" reset timescaledb.restoring
select "public".timescaledb_post_restore()
analyze`
	if got := strings.Join(r.log, "\n"); got != want {
		t.Errorf("steps:\n%s", got)
	}

	// A failed load resets the setting and skips post_restore.
	r = &sqlLog{failLoad: true}
	if err := LoadDB(ctx, st, dp, nil, r, pg.Conn{}, "metrics", ModeFull, 0); err == nil {
		t.Fatal("the load failed")
	}
	if got := strings.Join(r.log, "\n"); got != "alter database \"metrics\" set timescaledb.restoring = on\npg_restore\nalter database \"metrics\" reset timescaledb.restoring" {
		t.Errorf("after a failed load:\n%s", got)
	}
}
