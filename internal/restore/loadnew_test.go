package restore

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"bracer.dev/bracer/internal/pg"
)

// sqlRunner fakes psql: it records the SQL of every call and fails the one
// whose text contains failOn.
type sqlRunner struct {
	failOn string
	calls  []string
}

func (r *sqlRunner) Run(_ context.Context, _ pg.Conn, tool string, _ []string, stdin io.Reader, _ io.Writer) error {
	b, _ := io.ReadAll(stdin)
	sql := string(b)
	r.calls = append(r.calls, sql)
	if tool == "psql" && r.failOn != "" && strings.Contains(sql, r.failOn) {
		return &pg.ExitError{Cmd: "psql", Code: 3, Stderr: []byte("ERROR:  " + r.failOn + " failed")}
	}
	return nil
}

func (r *sqlRunner) Describe() string { return "fake" }

// CREATE DATABASE runs alone first: when it fails nothing is dropped, so a
// database of the target's name that already exists is left alone. When a
// later statement fails, the database this run created is dropped.
func TestLoadNewDropsOnlyWhatItCreated(t *testing.T) {
	dp := &DBPlan{Source: "shop", Target: "shop_restored", Create: []string{
		`CREATE DATABASE "shop_restored" TEMPLATE template0 OWNER "app";`,
		`ALTER DATABASE "shop_restored" SET search_path TO 'x';`,
	}}
	conn := pg.Conn{Database: "postgres"}

	r := &sqlRunner{failOn: "CREATE DATABASE"}
	err := LoadNew(context.Background(), nil, nil, dp, nil, r, conn, ModeFull, 17, time.Minute)
	var notCreated *ErrNotCreated
	if !errors.As(err, &notCreated) || notCreated.Target != "shop_restored" || !strings.Contains(err.Error(), "create shop_restored: psql: ERROR:  CREATE DATABASE failed") {
		t.Fatalf("err = %v", err)
	}
	if len(r.calls) != 1 || strings.Contains(r.calls[0], "ALTER") {
		t.Errorf("CREATE must run alone and first: %q", r.calls)
	}
	for _, c := range r.calls {
		if strings.Contains(c, "DROP DATABASE") {
			t.Errorf("nothing was created, nothing may be dropped: %q", r.calls)
		}
	}

	r = &sqlRunner{failOn: "ALTER DATABASE"}
	err = LoadNew(context.Background(), nil, nil, dp, nil, r, conn, ModeFull, 17, time.Minute)
	if err == nil || errors.As(err, &notCreated) || !strings.Contains(err.Error(), "set up shop_restored: psql: ERROR:  ALTER DATABASE failed") {
		t.Fatalf("err = %v", err)
	}
	if len(r.calls) != 3 || !strings.Contains(r.calls[2], `DROP DATABASE IF EXISTS "shop_restored" WITH (FORCE)`) {
		t.Errorf("the created database must be dropped: %q", r.calls)
	}
}
