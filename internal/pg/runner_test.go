package pg

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockingShell runs the tool command until ctx ends, as a long pg_dump does;
// any other command (the stop) returns at once and is recorded with the state
// of its context.
type blockingShell struct {
	mu      sync.Mutex
	started chan struct{}
	tool    Cmd
	stops   []Cmd
	stopErr []error
	stopDL  []time.Duration
}

func (s *blockingShell) Run(ctx context.Context, c Cmd) (Result, error) {
	if c.Stdout != nil { // the tool: its stdout is the dump
		s.mu.Lock()
		s.tool = c
		s.mu.Unlock()
		close(s.started)
		<-ctx.Done()
		return Result{}, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops = append(s.stops, c)
	s.stopErr = append(s.stopErr, ctx.Err())
	dl, ok := ctx.Deadline()
	if !ok {
		s.stopDL = append(s.stopDL, -1)
	} else {
		s.stopDL = append(s.stopDL, time.Until(dl))
	}
	return Result{}, nil
}

// cancelRun runs pg_dump through r, cancels it once started and returns what the shell saw.
func cancelRun(t *testing.T, r Runner, sh *blockingShell, conn Conn) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- r.Run(ctx, conn, "pg_dump", []string{"-Fc"}, nil, io.Discard) }()
	<-sh.started
	cancel()
	if err := <-errc; err == nil {
		t.Fatal("a cancelled run must fail")
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if len(sh.stops) != 1 {
		t.Fatalf("want one stop command after the cancel, got %d: %+v", len(sh.stops), sh.stops)
	}
	if sh.stopErr[0] != nil || sh.stopDL[0] <= 0 || sh.stopDL[0] > stopTimeout {
		t.Errorf("the stop must run under a live context bounded by %s: err %v, deadline in %s", stopTimeout, sh.stopErr[0], sh.stopDL[0])
	}
}

func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// BR-013: a cancel removes the docker run client container by its name;
// killing the docker CLI alone leaves pg_dump running on the source.
func TestBR013ClientContainerRemovedOnCancel(t *testing.T) {
	for _, mk := range []func(Shell) Runner{
		func(sh Shell) Runner { return NewURLRunner(sh, "postgres:17-alpine") },
		func(sh Shell) Runner {
			return ImageRunner{Shell: sh, Image: "postgres:17-alpine", Network: "container:shop-db"}
		},
		func(sh Shell) Runner {
			return SocketRunner{Shell: sh, Image: "postgres:17-alpine", Local: Local{Dir: "/var/run/postgresql", Port: 5432}, UID: 70, GID: 70}
		},
	} {
		sh := &blockingShell{started: make(chan struct{})}
		r := mk(sh)
		cancelRun(t, r, sh, Conn{User: "postgres", Database: "shop"})
		name := argAfter(sh.tool.Args, "--name")
		if !strings.HasPrefix(name, "bracer-pg-") || len(name) != len("bracer-pg-")+16 {
			t.Fatalf("%s: client container name %q", r.Describe(), name)
		}
		if got := sh.stops[0].Name + " " + strings.Join(sh.stops[0].Args, " "); got != "docker rm -f "+name {
			t.Errorf("%s: stop = %q", r.Describe(), got)
		}
	}
}

// BR-013: a cancel of a docker exec'd tool terminates its backend, found by
// the command's own application_name, through the same container.
func TestBR013ExecBackendTerminatedOnCancel(t *testing.T) {
	sh := &blockingShell{started: make(chan struct{})}
	conn := Conn{User: "postgres", Password: "pw", Database: "shop", Params: map[string]string{"application_name": "theirs"}}
	cancelRun(t, ExecRunner{Shell: sh, Container: "shop-db"}, sh, conn)
	app := envOf(sh.tool.Env, "PGAPPNAME")
	if !strings.HasPrefix(app, "bracer-") || len(app) != len("bracer-")+16 || !hasArg(sh.tool.Args, "PGAPPNAME") {
		t.Fatalf("the tool's application_name: %q (args %v)", app, sh.tool.Args)
	}
	stop := sh.stops[0]
	args := strings.Join(stop.Args, " ")
	if !strings.HasPrefix(args, "exec ") || !strings.Contains(args, " shop-db psql ") ||
		!strings.Contains(args, "pg_terminate_backend(pid) from pg_stat_activity where application_name = '"+app+"' and pid <> pg_backend_pid()") {
		t.Errorf("stop = %s", args)
	}
	if envOf(stop.Env, "PGPASSWORD") != "pw" || envOf(stop.Env, "PGDATABASE") != "shop" || envOf(stop.Env, "PGAPPNAME") == app || strings.Contains(args, "pw") {
		t.Errorf("the stop must connect as the tool did, under another name, secrets in env only: %v", stop.Env)
	}
}

// failingShell fails the tool at once with ctx still live, as the docker CLI
// dying of EPIPE when an upload breaks; it records the other commands (stops).
type failingShell struct {
	mu    sync.Mutex
	stops []Cmd
	live  []bool
}

func (s *failingShell) Run(ctx context.Context, c Cmd) (Result, error) {
	if c.Stdout != nil {
		return Result{}, &ExitError{Cmd: "docker run", Code: 141}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, dl := ctx.Deadline()
	s.stops = append(s.stops, c)
	s.live = append(s.live, ctx.Err() == nil && dl)
	return Result{}, nil
}

// BR-013 review: a tool that fails while ctx is live is stopped too: the
// docker CLI is gone, the client container or exec'd pg_dump may not be.
func TestBR013StopOnFailureWithoutCancel(t *testing.T) {
	for _, mk := range []func(Shell) Runner{
		func(sh Shell) Runner { return NewURLRunner(sh, "postgres:17-alpine") },
		func(sh Shell) Runner {
			return SocketRunner{Shell: sh, Image: "postgres:17-alpine", Local: Local{Dir: "/var/run/postgresql", Port: 5432}}
		},
		func(sh Shell) Runner { return ExecRunner{Shell: sh, Container: "shop-db"} },
	} {
		sh := &failingShell{}
		r := mk(sh)
		if err := r.Run(context.Background(), Conn{User: "postgres", Database: "shop"}, "pg_dump", []string{"-Fc"}, nil, io.Discard); err == nil {
			t.Fatalf("%s: the failure must be returned", r.Describe())
		}
		if len(sh.stops) != 1 || !sh.live[0] {
			t.Fatalf("%s: want one stop under a live bounded context, got %+v %v", r.Describe(), sh.stops, sh.live)
		}
		args := strings.Join(sh.stops[0].Args, " ")
		if !strings.HasPrefix(args, "rm -f bracer-pg-") && !strings.Contains(args, "pg_terminate_backend") {
			t.Errorf("%s: stop = %s", r.Describe(), args)
		}
	}
}

// BR-013: a command that ends by itself runs no stop, and each command gets
// a name of its own.
func TestBR013NoStopWithoutCancel(t *testing.T) {
	sh := &fakeShell{out: map[string]string{"docker": ""}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, r := range []Runner{ExecRunner{Shell: sh, Container: "c"}, NewURLRunner(sh, "postgres:17-alpine")} {
		for i := 0; i < 2; i++ {
			if err := r.Run(ctx, Conn{User: "u"}, "psql", nil, nil, io.Discard); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(sh.calls) != 4 {
		t.Fatalf("want only the 4 commands, got %d", len(sh.calls))
	}
	if envOf(sh.calls[0].Env, "PGAPPNAME") == envOf(sh.calls[1].Env, "PGAPPNAME") || argAfter(sh.calls[2].Args, "--name") == argAfter(sh.calls[3].Args, "--name") {
		t.Error("names must differ between commands")
	}
}

// BR-013: the stdout of a docker run client container is the dump; with a
// log driver dockerd writes it, unencrypted, under /var/lib/docker.
func TestBR013ClientContainerHasNoLogDriver(t *testing.T) {
	sh := &fakeShell{out: map[string]string{"docker": ""}}
	ctx := context.Background()
	runners := []Runner{
		NewURLRunner(sh, "postgres:17-alpine"),
		ImageRunner{Shell: sh, Image: "postgres:17-alpine", Network: "container:shop-db"},
		SocketRunner{Shell: sh, Image: "postgres:17-alpine", Local: Local{Dir: "/var/run/postgresql", Port: 5432}},
	}
	for i, r := range runners {
		if err := r.Run(ctx, Conn{User: "u"}, "pg_dump", []string{"-Fc"}, nil, io.Discard); err != nil {
			t.Fatal(err)
		}
		args := sh.calls[i].Args
		image := -1
		for j, a := range args {
			if a == "postgres:17-alpine" {
				image = j
			}
		}
		if argAfter(args[:image], "--log-driver") != "none" {
			t.Errorf("%s: docker args %v", r.Describe(), args)
		}
	}
}

func envOf(env []string, k string) string {
	v := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, k+"=") {
			v = strings.TrimPrefix(kv, k+"=")
		}
	}
	return v
}

// BR-013: temporary tables of other sessions (pg_temp_N) are not listed:
// count(*) on them fails, pg_dump never dumps them, and the drill would
// report them missing. The same holds for every catalog query that lists
// relations for a dump.
func TestBR013CatalogQueriesSkipTemporaryRelations(t *testing.T) {
	for name, q := range map[string]string{
		"tablesSQL":        tablesSQL,
		"rlsTablesSQL":     rlsTablesSQL,
		"userObjectsSQL":   userObjectsSQL,
		"collationUsesSQL": collationUsesSQL,
	} {
		want := 1
		if name == "collationUsesSQL" {
			want = 2 // column and index uses
		}
		if n := strings.Count(q, "relpersistence <> 't'"); n != want {
			t.Errorf("%s: %d temporary filters, want %d", name, n, want)
		}
	}
	// Snapshot.Tables and Client.Tables send tablesSQL itself.
	sh := &fakeShell{out: map[string]string{"docker": "public\x1fshop\x1f10\x1fr\x1f\n"}}
	cl := &Client{Runner: ExecRunner{Shell: sh, Container: "c"}, Conn: Conn{User: "u"}}
	if _, err := cl.Tables(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(sh.calls[0].Args, " "), "c.relpersistence <> 't'") {
		t.Error("Client.Tables must skip temporary tables")
	}
}
