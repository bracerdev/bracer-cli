package pg

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

// fakeShell returns results by command prefix; records the commands run.
type fakeShell struct {
	out   map[string]string
	fail  map[string]string
	calls []Cmd
}

func (f *fakeShell) Run(_ context.Context, c Cmd) (Result, error) {
	f.calls = append(f.calls, c)
	key := c.Name + " " + strings.Join(c.Args, " ")
	for p, stderr := range f.fail {
		if strings.HasPrefix(key, p) {
			return Result{}, &ExitError{Cmd: p, Code: 1, Stderr: []byte(stderr)}
		}
	}
	for p, o := range f.out {
		if strings.HasPrefix(key, p) {
			if c.Stdout != nil {
				io.WriteString(c.Stdout, o)
				return Result{}, nil
			}
			return Result{Stdout: []byte(o)}, nil
		}
	}
	return Result{}, fmt.Errorf("fake: unexpected command %q", key)
}

const psOut = `{"ID":"be65","Names":"bracer-test-pg","Image":"postgres:17-alpine","State":"running","Status":"Up 4 seconds"}
{"ID":"2e20","Names":"k8s_dump_postgres-backup-1_ns_uid_0","Image":"b0f9560a2de0","State":"exited","Status":"Exited (1) 25 hours ago"}
{"ID":"aaaa","Names":"web","Image":"nginx:1.27","State":"running","Status":"Up"}
`

const inspectOut = `[
 {"Id":"be65","Name":"/bracer-test-pg","State":{"Running":true,"Status":"running"},
  "Config":{"Image":"postgres:17-alpine","Env":["POSTGRES_PASSWORD=pw","POSTGRES_DB=shop","PG_MAJOR=17"]},
  "NetworkSettings":{"Ports":{"5432/tcp":[{"HostIp":"0.0.0.0","HostPort":"15432"},{"HostIp":"::","HostPort":"15432"}]},"Networks":{"bridge":{"IPAddress":"172.17.0.32"}}}},
 {"Id":"2e20","Name":"/k8s_dump_postgres-backup-1_ns_uid_0","State":{"Running":false,"Status":"exited"},
  "Config":{"Image":"b0f9560a2de0","Env":["PGHOST=db","POSTGRES_PASSWORD=x"]},
  "NetworkSettings":{"Ports":{},"Networks":{}}},
 {"Id":"aaaa","Name":"/web","State":{"Running":true,"Status":"running"},
  "Config":{"Image":"nginx:1.27","Env":["NGINX_VERSION=1.27","POSTGRES_SERVICE_HOST=10.43.0.1"]},
  "NetworkSettings":{"Ports":{"80/tcp":[{"HostIp":"0.0.0.0","HostPort":"8080"}]},"Networks":{"bridge":{"IPAddress":"172.17.0.2"}}}}
]`

const topOut = `UID   PID   PPID  C STIME TTY TIME     CMD
70    1     0     1 21:36  ?   00:00:00 postgres
70    2     1     0 21:36  ?   00:00:00 postgres: checkpointer
`

func TestListPostgresContainers(t *testing.T) {
	sh := &fakeShell{out: map[string]string{
		"docker ps":       psOut,
		"docker inspect":  inspectOut,
		"docker top be65": topOut,
		"docker top aaaa": "UID PID PPID C STIME TTY TIME CMD\n0 1 0 0 x ? 0 nginx: master\n",
	}}
	cs, err := Docker{Shell: sh}.ListPostgresContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("want 2 Postgres containers (nginx skipped), got %d: %+v", len(cs), cs)
	}
	var pg, k8s *Container
	for _, c := range cs {
		if c.Name == "bracer-test-pg" {
			pg = c
		} else {
			k8s = c
		}
	}
	if pg == nil || !pg.Running || !pg.HasPgProc || pg.Env["POSTGRES_DB"] != "shop" || len(pg.HostPorts) != 1 || pg.HostPorts[0] != 15432 {
		t.Errorf("container pg sai: %+v", pg)
	}
	if pg.DefaultUser() != "postgres" {
		t.Errorf("DefaultUser = %s", pg.DefaultUser())
	}
	if k8s == nil || !k8s.Kubernetes || k8s.Running {
		t.Errorf("k8s container sai: %+v", k8s)
	}
	for _, c := range sh.calls {
		if c.Name == "docker" && c.Args[0] == "top" && c.Args[1] == "2e20" {
			t.Error("must not docker top a stopped container")
		}
	}
}

func TestParseListeners(t *testing.T) {
	ss := `LISTEN 0 244 127.0.0.1:5432 0.0.0.0:* users:(("postgres",pid=123,fd=7))
LISTEN 0 244 [::1]:5432 [::]:* users:(("postgres",pid=123,fd=8))
LISTEN 0 4096 *:22 *:*
`
	ls := parseSS(ss)
	if len(ls) != 3 || ls[0].Port != 5432 || ls[0].Process != "postgres" || ls[1].Addr != "::1" || ls[2].Addr != "0.0.0.0" || ls[2].Process != "" {
		t.Errorf("parseSS = %+v", ls)
	}
}

func TestDiscoverMergesMappedPort(t *testing.T) {
	ct := &Container{Name: "shop-db", Image: "postgres:17", Running: true, HostPorts: []int{15432}, Port: 5432}
	hs := HostScan{
		Listeners: []Listener{
			{Addr: "0.0.0.0", Port: 15432, Process: "ssh"},       // the container's mapped port → merged
			{Addr: "::1", Port: 5433, Process: "postgres"},       // on the host
			{Addr: "127.0.0.1", Port: 5433, Process: "postgres"}, // same port, different address → one row
			{Addr: "0.0.0.0", Port: 8080, Process: "node"},       // unrelated
		},
		Sockets: map[int]string{5433: "/tmp/.s.PGSQL.5433", 5440: "/var/run/postgresql/.s.PGSQL.5440"},
	}
	probed := map[string]bool{}
	probe := func(_ context.Context, host string, port int) (bool, error) {
		probed[fmt.Sprintf("%s:%d", host, port)] = true
		return true, nil
	}
	d := Discover(context.Background(), []*Container{ct}, hs, probe)
	cands := d.Candidates
	// The sockets are the clusters (ADR-0015): the TCP row of 5433 is folded into its socket.
	if len(cands) != 3 {
		t.Fatalf("want 3 candidates (container, local 5433, local 5440), got %d: %+v", len(cands), cands)
	}
	if cands[0].Container != ct || !strings.Contains(cands[0].Label(), "published on localhost:15432") {
		t.Errorf("candidate 0: %s", cands[0].Label())
	}
	if l := cands[1].Local; l == nil || l.Port != 5433 || l.Dir != "/tmp" {
		t.Errorf("candidate 1: %+v", cands[1])
	}
	if l := cands[2].Local; l == nil || l.Port != 5440 || l.Dir != "/var/run/postgresql" {
		t.Errorf("candidate 2: %+v", cands[2])
	}
	if !probed["127.0.0.1:5433"] || len(probed) != 1 {
		t.Errorf("probe sai: %v", probed)
	}
	if cands[0].SuggestName() != "shop-db" || cands[1].SuggestName() != "local-5433" {
		t.Errorf("SuggestName: %s, %s", cands[0].SuggestName(), cands[1].SuggestName())
	}
}

func TestParseURL(t *testing.T) {
	c, err := ParseURL("postgresql://app:s3%23cret@db.example.com:6543/shop?sslmode=require&application_name=bracer")
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != "db.example.com" || c.Port != 6543 || c.User != "app" || c.Password != "s3#cret" || c.Database != "shop" || c.Params["sslmode"] != "require" {
		t.Errorf("ParseURL = %+v", c)
	}
	env := strings.Join(c.Env(), " ")
	for _, want := range []string{"PGHOST=db.example.com", "PGPORT=6543", "PGUSER=app", "PGPASSWORD=s3#cret", "PGDATABASE=shop", "PGSSLMODE=require", "PGAPPNAME=bracer", "PGCONNECT_TIMEOUT=10"} {
		if !strings.Contains(env, want) {
			t.Errorf("Env missing %s: %s", want, env)
		}
	}
	if c2, err := ParseURL("postgres://h/db"); err != nil || c2.Port != 5432 || c2.Database != "db" {
		t.Errorf("defaults: %+v %v", c2, err)
	}
	if _, err := ParseURL("postgres://h/db?sslmod=require"); err == nil {
		t.Error("a misspelled parameter must be an error")
	}
	if _, err := ParseURL("mysql://h/db"); err == nil {
		t.Error("an unknown scheme must be an error")
	}
}

func TestRunnersBuildDockerArgsWithoutSecrets(t *testing.T) {
	sh := &fakeShell{out: map[string]string{"docker": "170011\n"}}
	conn := Conn{User: "postgres", Password: "pw", Database: "shop"}
	err := ExecRunner{Shell: sh, Container: "shop-db"}.Run(context.Background(), conn, "psql", []string{"-c", "select 1"}, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(sh.calls[0].Args, " ")
	if !strings.HasPrefix(args, "exec -i -e PGUSER -e PGPASSWORD -e PGDATABASE -e PGCONNECT_TIMEOUT shop-db psql -c select 1") {
		t.Errorf("args exec: %s", args)
	}
	if strings.Contains(args, "pw") {
		t.Error("password leaked into argv")
	}
	if !contains(sh.calls[0].Env, "PGPASSWORD=pw") {
		t.Error("password must go into the docker process env")
	}

	r := NewURLRunner(sh, "postgres:17-alpine")
	conn = Conn{Host: "127.0.0.1", Port: 5433, User: "u", Database: "d"}
	if err := r.Run(context.Background(), conn, "pg_dump", []string{"-Fc"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	args = strings.Join(sh.calls[1].Args, " ")
	if !strings.Contains(args, "--network host") || !contains(sh.calls[1].Env, "PGHOST=127.0.0.1") {
		t.Errorf("URL runner must use the host network, 127.0.0.1 unchanged: %s %v", args, sh.calls[1].Env)
	}

	r = ImageRunner{Shell: sh, Image: "postgres:17-alpine", Network: "container:shop-db"}
	if err := r.Run(context.Background(), Conn{User: "u"}, "psql", nil, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	args = strings.Join(sh.calls[2].Args, " ")
	if !strings.Contains(args, "--network container:shop-db") || !contains(sh.calls[2].Env, "PGHOST=localhost") {
		t.Errorf("network container must connect to localhost: %s %v", args, sh.calls[2].Env)
	}
}

func TestClientQueries(t *testing.T) {
	sh := &fakeShell{out: map[string]string{
		"docker exec -i -e PGUSER -e PGCONNECT_TIMEOUT c psql -X -A -t -q -v ON_ERROR_STOP=1 -F \x1f -c show server_version_num": "170011\n",
		"docker exec -i -e PGUSER -e PGCONNECT_TIMEOUT c psql -X -A -t -q -v ON_ERROR_STOP=1 -F \x1f -c select datname":          "postgres\nshop\n",
		"docker exec -i -e PGUSER -e PGCONNECT_TIMEOUT c psql -X -A -t -q -v ON_ERROR_STOP=1 -F \x1f -c select current_user":     "bracer_backup\x1ff\x1ft\x1ff\n",
	}}
	cl := &Client{Runner: ExecRunner{Shell: sh, Container: "c"}, Conn: Conn{User: "bracer_backup"}}
	ctx := context.Background()
	if num, major, err := cl.ServerVersionNum(ctx); err != nil || num != 170011 || major != 17 {
		t.Errorf("version: %d %d %v", num, major, err)
	}
	if dbs, err := cl.ListDatabases(ctx); err != nil || len(dbs) != 2 || dbs[1] != "shop" {
		t.Errorf("dbs: %v %v", dbs, err)
	}
	p, err := cl.Privileges(ctx)
	if err != nil || p.Role != "bracer_backup" || p.Superuser || !p.ReadAllData || p.BypassRLS || p.CanDump() {
		t.Errorf("privileges: %+v %v", p, err)
	}
}

func TestIsAuthError(t *testing.T) {
	err := &ExitError{Code: 2, Stderr: []byte("psql: error: connection to server failed: FATAL:  password authentication failed for user \"x\"")}
	if !IsAuthError(err) {
		t.Error("must be an authentication error")
	}
	if IsAuthError(&ExitError{Code: 2, Stderr: []byte("connection refused")}) {
		t.Error("connection refused is not an authentication error")
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// PG < 14 has no pg_read_all_data: the sample SQL must say superuser, not GRANT a built-in role that does not exist.
func TestFixRoleSQLByMajor(t *testing.T) {
	if got := FixRoleSQL("b", 13); !strings.Contains(got, "SUPERUSER") || strings.Contains(got, "GRANT pg_read_all_data") {
		t.Errorf("PG13: %q", got)
	}
	for _, m := range []int{0, 14, 18} {
		if got := FixRoleSQL("b", m); !strings.Contains(got, "GRANT pg_read_all_data TO b") || !strings.Contains(got, "GRANT pg_read_all_settings TO b") || strings.Contains(got, "SUPERUSER") {
			t.Errorf("PG%d: %q", m, got)
		}
	}
}

// spec init § Flow 1b: Postgres from apt runs as user postgres, so `ss -ltnp`
// run by another user shows no process. Such ports are kept only when
// SSLRequest answers; the rest (sshd, DNS…) are dropped without a warning.
func TestDiscoverUnseenProcessNeedsSSLRequest(t *testing.T) {
	hs := HostScan{
		Listeners: []Listener{
			{Addr: "127.0.0.1", Port: 5433}, // apt Postgres, process not visible
			{Addr: "0.0.0.0", Port: 22},     // sshd (root)
			{Addr: "127.0.0.53", Port: 53},  // resolver
		},
		Sockets: map[int]string{5433: "/var/run/postgresql/.s.PGSQL.5433"},
	}
	var mu sync.Mutex
	probed := map[int]bool{}
	probe := func(_ context.Context, _ string, port int) (bool, error) {
		mu.Lock()
		probed[port] = true
		mu.Unlock()
		return port == 5433, nil
	}
	d := Discover(context.Background(), nil, hs, probe)
	if len(d.Candidates) != 1 || d.Candidates[0].Local == nil || d.Candidates[0].Local.Port != 5433 {
		t.Fatalf("candidates: %+v", d.Candidates)
	}
	if len(probed) != 3 {
		t.Errorf("every unseen port must be probed: %v", probed)
	}
	if len(d.Warnings) != 0 {
		t.Errorf("warnings %v", d.Warnings)
	}
}

// postgis.control is an update-alternatives symlink: looked up at its real
// path. Its library ships in another package than its scripts: both are
// listed. One extension without a package does not hide the others.
func TestExtensionPackagesFollowsAlternatives(t *testing.T) {
	d := "/usr/share/postgresql/18/extension/"
	lib := "/usr/lib/postgresql/18/lib/"
	sh := &fakeShell{
		fail: map[string]string{"dpkg-query -S " + d + "postgis.control": "no path found", "dpkg-query -S " + d + "mine.control": "no path found"},
		out: map[string]string{
			"cat " + d + "postgis.control":             "comment = 'PostGIS'\nmodule_pathname = '$libdir/postgis-3'\nrelocatable = false\n",
			"cat " + d + "mine.control":                "comment = 'mine'\n",
			"cat " + d + "vector.control":              "module_pathname = '$libdir/vector'\n",
			"readlink -f " + d + "postgis.control":     d + "postgis-3.control\n",
			"readlink -f " + d + "mine.control":        d + "mine.control\n",
			"dpkg-query -S " + d + "postgis-3.control": "postgresql-18-postgis-3-scripts: " + d + "postgis-3.control\n",
			"dpkg-query -S " + lib + "postgis-3.so":    "postgresql-18-postgis-3: " + lib + "postgis-3.so\n",
			"dpkg-query -S " + d + "vector.control":    "postgresql-18-pgvector: " + d + "vector.control\n",
			"dpkg-query -S " + lib + "vector.so":       "postgresql-18-pgvector: " + lib + "vector.so\n",
			"dpkg-query -W":                            "1.0",
		}}
	pkgs, err := ExtensionPackages(context.Background(), sh, 18, []string{"mine", "postgis", "vector"})
	if err == nil || !strings.Contains(err.Error(), "mine") || strings.Contains(err.Error(), "postgis") {
		t.Errorf("err %v", err)
	}
	var got []string
	for _, p := range pkgs {
		got = append(got, p.Extension+"="+p.Package)
	}
	if want := "postgis=postgresql-18-postgis-3-scripts postgis=postgresql-18-postgis-3 vector=postgresql-18-pgvector"; strings.Join(got, " ") != want {
		t.Errorf("pkgs %v, want %s", got, want)
	}
}
