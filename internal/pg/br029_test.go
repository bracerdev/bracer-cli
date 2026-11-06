package pg

import (
	"context"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

// BR-029: a container is listed only when it is a Postgres server. A stopped
// container cannot be checked with docker top, so its command decides; the
// image name and POSTGRES_* env do not (a migration job that connects to the
// database carries them too).
func TestBR029ContainersAreServersNotClients(t *testing.T) {
	const ps = `{"ID":"c1","Names":"shop-migrate-1","Image":"shop-migrate","State":"exited","Status":"exited"}
{"ID":"c2","Names":"shop-postgres-1","Image":"postgres:18-alpine","State":"running","Status":"Up"}
{"ID":"c3","Names":"shop-old-postgres","Image":"postgres:18-alpine","State":"exited","Status":"exited"}
{"ID":"c4","Names":"shop-seed-1","Image":"postgres:17","State":"exited","Status":"exited"}
{"ID":"c5","Names":"geo-db","Image":"postgis/postgis:17-3.5","State":"exited","Status":"exited"}
{"ID":"c6","Names":"metrics-db","Image":"timescale/timescaledb:latest-pg17","State":"exited","Status":"exited"}
{"ID":"c7","Names":"vector-db","Image":"pgvector/pgvector:pg17","State":"exited","Status":"exited"}
{"ID":"c8","Names":"legacy-db","Image":"bitnami/postgresql:17","State":"exited","Status":"exited"}
{"ID":"c9","Names":"tuned-db","Image":"postgres:16","State":"exited","Status":"exited"}
{"ID":"c10","Names":"shop-worker-1","Image":"shop-worker","State":"running","Status":"Up"}
`
	const inspect = `[
 {"Id":"c1","Name":"/shop-migrate-1","State":{"Running":false,"Status":"exited"},
  "Config":{"Image":"shop-migrate","Env":["POSTGRES_USER=shop","POSTGRES_DB=shop","POSTGRES_PASSWORD=x","DATABASE_URL=postgres://shop:x@db/shop"],
   "Entrypoint":["docker-entrypoint.sh"],"Cmd":["node","./migrate.js"]},
  "NetworkSettings":{"Ports":{},"Networks":{}}},
 {"Id":"c2","Name":"/shop-postgres-1","State":{"Running":true,"Status":"running"},
  "Config":{"Image":"postgres:18-alpine","Env":["POSTGRES_PASSWORD=x"],"Entrypoint":["docker-entrypoint.sh"],"Cmd":["postgres"]},
  "NetworkSettings":{"Ports":{},"Networks":{}}},
 {"Id":"c3","Name":"/shop-old-postgres","State":{"Running":false,"Status":"exited"},
  "Config":{"Image":"postgres:18-alpine","Env":["POSTGRES_PASSWORD=x","PGDATA=/var/lib/postgresql/18/docker"],"Entrypoint":["docker-entrypoint.sh"],"Cmd":["postgres"]},
  "NetworkSettings":{"Ports":{},"Networks":{}}},
 {"Id":"c4","Name":"/shop-seed-1","State":{"Running":false,"Status":"exited"},
  "Config":{"Image":"postgres:17","Env":["PGPASSWORD=x","PGDATA=/var/lib/postgresql/data"],"Entrypoint":["docker-entrypoint.sh"],"Cmd":["psql","-h","db","-U","postgres","-f","/seed.sql"]},
  "NetworkSettings":{"Ports":{},"Networks":{}}},
 {"Id":"c5","Name":"/geo-db","State":{"Running":false,"Status":"exited"},
  "Config":{"Image":"postgis/postgis:17-3.5","Entrypoint":["docker-entrypoint.sh"],"Cmd":["postgres"]},
  "NetworkSettings":{"Ports":{},"Networks":{}}},
 {"Id":"c6","Name":"/metrics-db","State":{"Running":false,"Status":"exited"},
  "Config":{"Image":"timescale/timescaledb:latest-pg17","Entrypoint":["docker-entrypoint.sh"],"Cmd":["postgres"]},
  "NetworkSettings":{"Ports":{},"Networks":{}}},
 {"Id":"c7","Name":"/vector-db","State":{"Running":false,"Status":"exited"},
  "Config":{"Image":"pgvector/pgvector:pg17","Entrypoint":["docker-entrypoint.sh"],"Cmd":["postgres"]},
  "NetworkSettings":{"Ports":{},"Networks":{}}},
 {"Id":"c8","Name":"/legacy-db","State":{"Running":false,"Status":"exited"},
  "Config":{"Image":"bitnami/postgresql:17","Entrypoint":["/opt/bitnami/scripts/postgresql/entrypoint.sh"],"Cmd":["/opt/bitnami/scripts/postgresql/run.sh"]},
  "NetworkSettings":{"Ports":{},"Networks":{}}},
 {"Id":"c9","Name":"/tuned-db","State":{"Running":false,"Status":"exited"},
  "Config":{"Image":"postgres:16","Env":["PG_MAJOR=16","PGDATA=/var/lib/postgresql/data"],"Entrypoint":["docker-entrypoint.sh"],"Cmd":["-c","shared_buffers=1GB"]},
  "NetworkSettings":{"Ports":{},"Networks":{}}},
 {"Id":"c10","Name":"/shop-worker-1","State":{"Running":true,"Status":"running"},
  "Config":{"Image":"shop-worker","Env":["POSTGRES_USER=shop","POSTGRES_PASSWORD=x"],"Entrypoint":null,"Cmd":["node","worker.js"]},
  "NetworkSettings":{"Ports":{},"Networks":{}}}
]`
	sh := &fakeShell{
		out: map[string]string{
			"docker ps":      ps,
			"docker inspect": inspect,
			"docker top c2":  topOut,
		},
		// docker top failing on a client container must not list it as "not verified".
		fail: map[string]string{"docker top c10": "permission denied"},
	}
	cs, err := Docker{Shell: sh}.ListPostgresContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range cs {
		names = append(names, c.Name)
	}
	want := []string{"geo-db", "legacy-db", "metrics-db", "shop-old-postgres", "shop-postgres-1", "tuned-db", "vector-db"}
	if !slices.Equal(names, want) {
		t.Errorf("listed %v, want %v", names, want)
	}
}

// BR-029: the command decides, judged through docker inspect's Config.
// docker-entrypoint.sh is shared by mysql, mongo, redis and node images, so a
// leading flag counts only with the official image's PG_MAJOR; Bitnami's
// entrypoint runs the server only with its run.sh; init wrappers and sh -c
// are looked through.
func TestBR029RunsPostgresServer(t *testing.T) {
	official := []string{"PG_MAJOR=18", "PGDATA=/var/lib/postgresql/18/docker"}
	client := []string{"POSTGRES_USER=shop", "POSTGRES_DB=shop", "POSTGRES_PASSWORD=x", "DATABASE_URL=postgres://shop:x@db/shop"}
	ep := []string{"docker-entrypoint.sh"}
	bitnami := []string{"/opt/bitnami/scripts/postgresql/entrypoint.sh"}
	for _, tc := range []struct {
		name            string
		entrypoint, cmd []string
		env             []string
		want            bool
	}{
		{"official", ep, []string{"postgres"}, official, true},
		{"official with flags", ep, []string{"-c", "max_connections=50"}, official, true},
		{"supabase", []string{"/usr/local/bin/docker-entrypoint.sh"}, []string{"postgres", "-c", "config_file=/etc/postgresql/postgresql.conf"}, nil, true},
		{"binary path", nil, []string{"/usr/lib/postgresql/17/bin/postgres", "-D", "/data"}, nil, true},
		{"redis flags", ep, []string{"--appendonly", "yes"}, nil, false},
		{"redis flags, client env", ep, []string{"--appendonly", "yes"}, client, false},
		{"mysql flags", ep, []string{"--default-authentication-plugin=mysql_native_password"}, client, false},
		{"mongo flags", ep, []string{"--replSet", "rs0"}, nil, false},
		{"version flag", ep, []string{"--version"}, client, false},
		{"node job", ep, []string{"node", "./migrate.js"}, client, false},
		{"psql job on the official image", ep, []string{"psql", "-U", "postgres"}, official, false},
		{"pg_dump", nil, []string{"pg_dump", "-U", "postgres"}, nil, false},
		{"bitnami", bitnami, []string{"/opt/bitnami/scripts/postgresql/run.sh"}, nil, true},
		{"bitnami run.sh only", nil, []string{"/opt/bitnami/scripts/postgresql/run.sh"}, nil, true},
		{"bitnami repmgr", []string{"/opt/bitnami/scripts/postgresql-repmgr/entrypoint.sh"}, []string{"/opt/bitnami/scripts/postgresql-repmgr/run.sh"}, nil, true},
		{"bitnami pg_dump sidecar", bitnami, []string{"pg_dump", "-h", "db", "-U", "postgres"}, nil, false},
		{"bitnami psql sidecar", bitnami, []string{"psql", "-h", "db"}, nil, false},
		{"tini", []string{"/sbin/tini", "--", "docker-entrypoint.sh"}, []string{"postgres"}, official, true},
		{"tini flags", []string{"tini", "-g", "--"}, []string{"postgres"}, nil, true},
		{"dumb-init", []string{"dumb-init"}, []string{"postgres", "-c", "fsync=off"}, nil, true},
		{"docker-init", []string{"/sbin/docker-init", "--", "docker-entrypoint.sh", "-c", "fsync=off"}, nil, official, true},
		{"tini node", []string{"tini", "--"}, []string{"node", "server.js"}, client, false},
		{"sh -c exec", nil, []string{"sh", "-c", "chown -R postgres /data && exec docker-entrypoint.sh postgres -c fsync=off"}, official, true},
		{"bash -c postgres", nil, []string{"/bin/bash", "-c", `PGOPTIONS=x exec postgres -D "/data"`}, nil, true},
		{"sh -c exporter", nil, []string{"sh", "-c", "exec postgres_exporter --web.listen-address=:9187"}, nil, false},
		{"sh -c backup path", nil, []string{"sh", "-c", "pg_dump -U postgres shop > /backups/postgres/shop.sql"}, nil, false},
		{"sh -c psql", nil, []string{"sh", "-c", "until psql -U postgres -c 'select 1'; do sleep 1; done"}, official, false},
		{"entrypoint alone", ep, nil, official, false},
		{"nothing", nil, nil, nil, false},
		// compose `command:` replaces Cmd, the image's entrypoint stays.
		{"compose sh -c after the entrypoint", ep, []string{"sh", "-c", "chown -R postgres /tmp && exec docker-entrypoint.sh postgres"}, official, true},
		{"compose sh -c job after the entrypoint", ep, []string{"sh", "-c", "until pg_isready -h postgres; do sleep 1; done; node ./migrate.js"}, official, false},
		{"tini after the entrypoint", ep, []string{"tini", "--", "postgres"}, nil, true},
		{"sh -ec", nil, []string{"sh", "-ec", "exec postgres -c fsync=off"}, nil, true},
		{"bash -lc", nil, []string{"bash", "-lc", "exec docker-entrypoint.sh postgres"}, nil, true},
		{"sh with a non-c flag", nil, []string{"sh", "-e", "exec postgres"}, nil, false},
	} {
		var e inspectEntry
		e.Config.Entrypoint, e.Config.Cmd, e.Config.Env = tc.entrypoint, tc.cmd, tc.env
		if got := containerFromInspect(e).RunsServer; got != tc.want {
			t.Errorf("%s: %v %v: got %v", tc.name, tc.entrypoint, tc.cmd, got)
		}
	}
}

// BR-029: one 2 s budget per port covers dial, write, the answer and the
// silence check: a silent listener costs 2 s, and an answer late in the
// budget is not followed by a further 300 ms.
func TestBR029ProbeOneBudget(t *testing.T) {
	silent := serve(t, func(net.Conn) { time.Sleep(4 * time.Second) })
	late := serve(t, func(c net.Conn) {
		time.Sleep(1950 * time.Millisecond)
		c.Write([]byte("N"))
		time.Sleep(2 * time.Second)
	})
	for _, tc := range []struct {
		name string
		port int
	}{{"silent", silent}, {"late answer", late}} {
		start := time.Now()
		ProbeGSSENCRequest(context.Background(), "127.0.0.1", tc.port)
		if took := time.Since(start); took > probeBudget+150*time.Millisecond {
			t.Errorf("%s: took %v, budget %v", tc.name, took, probeBudget)
		}
	}
}

// BR-029: docker top counts a process only when its command is the postgres
// binary or one of its children (`postgres: checkpointer`). The layout is the
// one docker top prints: ps -ef columns padded to the same width, CMD last.
func TestBR029TopHasPostgres(t *testing.T) {
	const header = "UID                 PID                 PPID                C                   STIME               TTY                 TIME                CMD\n"
	row := func(uid, cmd string) string {
		return fmt.Sprintf("%-20s%-20s%-20s%-20s%-20s%-20s%-20s%s\n", uid, "45701", "45674", "0", "15:48", "?", "00:00:00", cmd)
	}
	for _, tc := range []struct {
		name string
		rows []string
		want bool
	}{
		{"postmaster", []string{row("70", "postgres")}, true},
		{"postmaster by path", []string{row("postgres", "/usr/lib/postgresql/17/bin/postgres -D /var/lib/postgresql/17/main")}, true},
		{"child only", []string{row("70", "postgres: checkpointer")}, true},
		{"wait loop naming the db host", []string{row("root", "sh -c until pg_isready -q -h postgres; do sleep 1; done"), row("root", "sleep 1")}, false},
		{"exporter", []string{row("nobody", "postgres_exporter --web.listen-address=:9187")}, false},
		{"app", []string{row("1000", "node server.js --db postgres")}, false},
	} {
		if got := topHasPostgres(header + strings.Join(tc.rows, "")); got != tc.want {
			t.Errorf("%s: got %v", tc.name, got)
		}
	}
	if !topHasPostgres("UID PID PPID C STIME TTY TIME CMD\n70 2 1 0 Nov06 ? 0 postgres: checkpointer\n") {
		t.Error("columns not padded: child process missed")
	}
}

// BR-029: after its one byte Postgres keeps the connection open for the
// startup packet; a peer that hangs up or keeps talking is not Postgres.
func TestBR029ProbeAfterTheByte(t *testing.T) {
	reply := func(b []byte, wait time.Duration) func(net.Conn) {
		return func(c net.Conn) {
			io.ReadFull(c, make([]byte, 8))
			c.Write(b)
			time.Sleep(wait)
		}
	}
	for _, tc := range []struct {
		name   string
		handle func(net.Conn)
		want   bool
	}{
		{"N then close", reply([]byte("N"), 0), false},
		{"G then close", reply([]byte("G"), 0), false},
		{"N then more at once", reply([]byte("N\x00\x01"), time.Second), false},
		{"N then silence", reply([]byte("N"), time.Second), true},
	} {
		if ok, _ := ProbeGSSENCRequest(context.Background(), "127.0.0.1", serve(t, tc.handle)); ok != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, ok, tc.want)
		}
	}
}
