package pg

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func lookIn(paths map[string]string) func(string) (string, error) {
	return func(name string) (string, error) {
		if p, ok := paths[name]; ok {
			return p, nil
		}
		return "", errors.New("not found")
	}
}

func noGlob(string) ([]string, error) { return nil, nil }

func TestParseToolVersion(t *testing.T) {
	for in, want := range map[string]struct {
		ver   string
		major int
	}{
		"pg_dump (PostgreSQL) 17.11 (Debian 17.11-1.pgdg12+2)\n": {"17.11", 17},
		"pg_dump (PostgreSQL) 18.1\n":                            {"18.1", 18},
		"pg_dump (PostgreSQL) 9.6.24\n":                          {"9.6", 9},
	} {
		ver, major, ok := ParseToolVersion(in)
		if !ok || ver != want.ver || major != want.major {
			t.Errorf("%q → %q %d %v", in, ver, major, ok)
		}
	}
	if _, _, ok := ParseToolVersion("psql: command not found"); ok {
		t.Error("garbage must not parse")
	}
}

func TestLocalFinderPicksNewestAndRunnerKeepsSecretsInEnv(t *testing.T) {
	sh := &fakeShell{out: map[string]string{
		"/usr/bin/pg_dump --version":                   "pg_dump (PostgreSQL) 15.8\n",
		"/usr/lib/postgresql/17/bin/pg_dump --version": "pg_dump (PostgreSQL) 17.11\n",
		"/usr/lib/postgresql/16/bin/pg_dump --version": "pg_dump (PostgreSQL) 16.4\n",
	}}
	look := lookIn(map[string]string{
		"pg_dump":                            "/usr/bin/pg_dump",
		"/usr/lib/postgresql/17/bin/pg_dump": "/usr/lib/postgresql/17/bin/pg_dump",
		"/usr/lib/postgresql/16/bin/pg_dump": "/usr/lib/postgresql/16/bin/pg_dump",
	})
	glob := func(pattern string) ([]string, error) {
		if pattern == "/usr/lib/postgresql/*/bin" {
			return []string{"/usr/lib/postgresql/16/bin", "/usr/lib/postgresql/17/bin"}, nil
		}
		return nil, nil
	}
	lt, err := LocalFinder{Shell: sh, LookPath: look, Glob: glob}.Find(context.Background())
	if err != nil || lt == nil || lt.Dir != "/usr/lib/postgresql/17/bin" || lt.Major != 17 || lt.Version != "17.11" {
		t.Fatalf("Find = %+v %v", lt, err)
	}
	if lt, _ := (LocalFinder{Shell: sh, LookPath: lookIn(nil), Glob: noGlob}).Find(context.Background()); lt != nil {
		t.Errorf("without pg_dump the result must be nil: %+v", lt)
	}

	sh = &fakeShell{out: map[string]string{"/usr/lib/postgresql/17/bin/psql": "1\n"}}
	r := LocalRunner{Shell: sh, Tools: *lt}
	conn := Conn{Host: "/var/run/postgresql", Port: 5433, User: "postgres", Password: "pw", Database: "shop"}
	if err := r.Run(context.Background(), conn, "psql", []string{"-c", "select 1"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	c := sh.calls[0]
	if c.Name != "/usr/lib/postgresql/17/bin/psql" || strings.Join(c.Args, " ") != "-c select 1" {
		t.Errorf("command: %s %v", c.Name, c.Args)
	}
	if strings.Contains(strings.Join(c.Args, " "), "pw") || !contains(c.Env, "PGPASSWORD=pw") || !contains(c.Env, "PGHOST=/var/run/postgresql") || !contains(c.Env, "PGPORT=5433") {
		t.Errorf("password and socket must go through env: %v %v", c.Args, c.Env)
	}
	if r.Describe() != "host pg_dump 17.11 (/usr/lib/postgresql/17/bin)" {
		t.Errorf("Describe = %s", r.Describe())
	}
}

const podsJSON = `{"items":[
 {"metadata":{"namespace":"attendy-dev","name":"postgres-0"},
  "spec":{"containers":[{"name":"postgres","image":"attendy-postgres:sha","env":[{"name":"POSTGRES_USER","value":"attendy"},{"name":"POSTGRES_PASSWORD","valueFrom":{"secretKeyRef":{"name":"pg","key":"pw"}}}]},
                        {"name":"exporter","image":"wrouesnel/postgres_exporter","env":[]}]},
  "status":{"phase":"Running","containerStatuses":[{"name":"postgres","ready":true},{"name":"exporter","ready":true}]}},
 {"metadata":{"namespace":"attendy-dev","name":"attendy-api-1"},
  "spec":{"containers":[{"name":"api","image":"attendy-api:sha","env":[{"name":"POSTGRES_SERVICE_HOST","value":"10.43.0.1"}]}]},
  "status":{"phase":"Running","containerStatuses":[{"name":"api","ready":true}]}},
 {"metadata":{"namespace":"slidemaker","name":"slidemaker-app-1"},
  "spec":{"containers":[{"name":"app","image":"slidemaker-app:61bf454","env":[{"name":"POSTGRES_USER"},{"name":"POSTGRES_PASSWORD"},{"name":"POSTGRES_DB"},{"name":"DATABASE_URL"}]}]},
  "status":{"phase":"Running","containerStatuses":[{"name":"app","ready":true}]}},
 {"metadata":{"namespace":"jumpsmash","name":"jumpsmash-api-1"},
  "spec":{"containers":[{"name":"api","image":"jumpsmash-api:8cdf540","env":[{"name":"POSTGRES_USER"},{"name":"POSTGRES_PASSWORD"},{"name":"DATABASE_URL"}]}]},
  "status":{"phase":"Running","containerStatuses":[{"name":"api","ready":true}]}},
 {"metadata":{"namespace":"legacy","name":"db-0"},
  "spec":{"containers":[{"name":"db","image":"mydb:1","ports":[{"containerPort":5432}],"env":[{"name":"DB_USER","value":"legacy"}]}]},
  "status":{"phase":"Running","containerStatuses":[{"name":"db","ready":true}]}},
 {"metadata":{"namespace":"slidemaker-dev","name":"postgres-backup-1"},
  "spec":{"containers":[{"name":"dump","image":"postgres:17"}]},
  "status":{"phase":"Succeeded"}}
]}`

func TestKubeListPodsAndExecRunner(t *testing.T) {
	if k, _ := FindKubectl(nil, lookIn(nil)); k != nil {
		t.Error("without kubectl or k3s the result must be nil")
	}
	k, _ := FindKubectl(nil, lookIn(map[string]string{"k3s": "/usr/local/bin/k3s"}))
	if k == nil || strings.Join(k.Cmd, " ") != "k3s kubectl" {
		t.Fatalf("k3s kubectl: %+v", k)
	}
	sh := &fakeShell{out: map[string]string{
		"k3s kubectl get pods -A -o json": podsJSON,
		"k3s kubectl exec -n attendy-dev postgres-0 -c postgres -- sh -c command -v pg_dump": "/usr/bin/pg_dump\n",
		"k3s kubectl exec -i -n attendy-dev postgres-0 -c postgres -- env":                   "1\n",
		// Process check: the real server has one; the exporter and the app pod (with POSTGRES_* to reach the DB) do not.
		"k3s kubectl exec -n attendy-dev postgres-0 -c postgres -- sh -c for f in /proc": "1\n",
		"k3s kubectl exec -n attendy-dev postgres-0 -c exporter -- sh -c for f in /proc": "none\n",
		"k3s kubectl exec -n slidemaker slidemaker-app-1 -c app -- sh -c for f in /proc": "none\n",
		// Env resolved inside the container, then the OS user of the process.
		"k3s kubectl exec -n attendy-dev postgres-0 -c postgres -- sh -c u=": "\n",
		"k3s kubectl exec -n legacy db-0 -c db -- sh -c u=":                  "legacy\n",
	}, fail: map[string]string{
		"k3s kubectl exec -n legacy db-0 -c db -- sh -c for f in /proc":                `exec failed: "sh": executable file not found`,
		"k3s kubectl exec -n jumpsmash jumpsmash-api-1 -c api -- sh -c for f in /proc": `exec failed: "sh": executable file not found`,
	}}
	k.Shell = sh
	pods, err := k.ListPostgresPods(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Only containers with a postgres process; db-0 (port 5432, no sh) is listed but Unverified;
	// the exporter, the app pod, the api pod and the Succeeded job are not.
	// jumpsmash-api: env hint only, no sh → Unverified + EnvOnly (init leaves it off the list).
	if len(pods) != 3 || pods[0].Ref.String() != "attendy-dev/postgres-0/postgres" || pods[1].Ref.String() != "jumpsmash/jumpsmash-api-1/api" || pods[2].Ref.String() != "legacy/db-0/db" {
		t.Fatalf("pods: %+v", pods)
	}
	pg, api, legacy := pods[0], pods[1], pods[2]
	if !pg.Ready || !pg.HasPgProc || pg.PgPID != "1" || pg.Unverified || pg.EnvOnly || pg.DefaultUser() != "attendy" || pg.Env["POSTGRES_PASSWORD"] != "" {
		t.Errorf("pod: %+v", pg)
	}
	if !api.Unverified || !api.EnvOnly || api.HasPgProc {
		t.Errorf("api: %+v", api)
	}
	if !legacy.Unverified || legacy.EnvOnly || legacy.HasPgProc {
		t.Errorf("legacy: %+v", legacy)
	}
	if u := k.ContainerUser(context.Background(), legacy.Ref, "7"); u != "legacy" {
		t.Errorf("ContainerUser = %q", u)
	}
	if u := k.ContainerUser(context.Background(), pg.Ref, "1"); u != "" {
		t.Errorf("ContainerUser with empty output = %q", u)
	}
	for _, c := range sh.calls {
		if strings.Contains(strings.Join(c.Args, " "), "attendy-api-1") {
			t.Error("a pod with no Postgres evidence must not be exec'd into")
		}
	}
	if !k.HasTool(context.Background(), pg.Ref, "pg_dump") {
		t.Error("HasTool must find pg_dump")
	}

	r := KubeExecRunner{Kube: *k, Pod: pg.Ref}
	if r.Describe() != "k3s kubectl exec attendy-dev/postgres-0/postgres" {
		t.Errorf("Describe = %s", r.Describe())
	}
	conn := Conn{User: "attendy", Database: "shop"}
	if err := r.Run(context.Background(), conn, "psql", []string{"-c", "select 1"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	c := sh.calls[len(sh.calls)-1]
	if c.Name != "k3s" || strings.Join(c.Args, " ") != "kubectl exec -i -n attendy-dev postgres-0 -c postgres -- env PGUSER=attendy PGDATABASE=shop PGCONNECT_TIMEOUT=10 psql -c select 1" {
		t.Errorf("args: %s %v", c.Name, c.Args)
	}
	conn.Password = "pw"
	err = r.Run(context.Background(), conn, "psql", nil, nil, io.Discard)
	if !errors.Is(err, ErrKubePassword) {
		t.Errorf("a password over kubectl exec must be refused: %v", err)
	}
	for _, c := range sh.calls {
		if strings.Contains(strings.Join(c.Args, " "), "pw") {
			t.Error("password leaked into argv")
		}
	}

	if _, err := ParsePodRef("ns"); err == nil {
		t.Error("missing pod name must be an error")
	}
	if ref, err := ParsePodRef("ns/pod"); err != nil || ref.Container != "" || ref.String() != "ns/pod" {
		t.Errorf("ns/pod: %+v %v", ref, err)
	}
	hint := KubeHint(&ExitError{Stderr: []byte("error: open /etc/rancher/k3s/k3s.yaml: permission denied")})
	if !strings.Contains(hint, "KUBECONFIG") || !strings.Contains(hint, "sudo") {
		t.Errorf("hint = %q", hint)
	}
}

func TestToolsPickFollowsADR0009(t *testing.T) {
	ctx := context.Background()
	hostBin := "/usr/lib/postgresql/16/bin/"
	newTools := func(sh *fakeShell, paths map[string]string) *Tools {
		return &Tools{Shell: sh, GOOS: "linux", LookPath: lookIn(paths), Glob: noGlob}
	}
	// Container has pg_dump → docker exec; without it → image on the same network.
	sh := &fakeShell{out: map[string]string{"docker exec shop-db sh -c command -v pg_dump": "/usr/local/bin/pg_dump\n"}, fail: map[string]string{"docker exec slim sh": "not found"}}
	tl := newTools(sh, nil)
	if r, err := tl.Pick(ctx, Where{Container: "shop-db"}, 17); err != nil || r.Describe() != "docker exec shop-db" {
		t.Errorf("container: %v %v", r, err)
	}
	if r, err := tl.Pick(ctx, Where{Container: "slim"}, 17); err != nil || r.Describe() != "docker run postgres:17-alpine --network container:slim" {
		t.Errorf("container without pg_dump: %v %v", r, err)
	}
	// Pod: no kubectl → error; present → kubectl exec; pg_dump missing → error.
	pod := PodRef{Namespace: "ns", Name: "pg", Container: "db"}
	if _, err := tl.Pick(ctx, Where{Pod: pod}, 17); err == nil || !strings.Contains(err.Error(), "kubectl not found") {
		t.Errorf("pod without kubectl: %v", err)
	}
	sh = &fakeShell{out: map[string]string{"kubectl exec -n ns pg -c db -- sh": "ok\n"}, fail: map[string]string{"kubectl exec -n ns slim -c db -- sh": "not found"}}
	tl = newTools(sh, map[string]string{"kubectl": "/usr/bin/kubectl"})
	if r, err := tl.Pick(ctx, Where{Pod: pod}, 17); err != nil || r.Describe() != "kubectl exec ns/pg/db" {
		t.Errorf("pod: %v %v", r, err)
	}
	if _, err := tl.Pick(ctx, Where{Pod: PodRef{Namespace: "ns", Name: "slim", Container: "db"}}, 17); err == nil || !strings.Contains(err.Error(), "no pg_dump") {
		t.Errorf("pod without pg_dump: %v", err)
	}
	// Socket: no host binary → error naming the client package; present but old → error; new enough → LocalRunner.
	tl = newTools(&fakeShell{}, nil)
	if _, err := tl.Pick(ctx, Where{SocketDir: "/var/run/postgresql"}, 17); err == nil || !strings.Contains(err.Error(), "client package for PostgreSQL 17") {
		t.Errorf("socket without a host binary: %v", err)
	}
	sh = &fakeShell{out: map[string]string{hostBin + "pg_dump --version": "pg_dump (PostgreSQL) 16.4\n", "docker version": "29\n"}}
	tl = newTools(sh, map[string]string{"pg_dump": hostBin + "pg_dump"})
	if _, err := tl.Pick(ctx, Where{SocketDir: "/var/run/postgresql"}, 17); err == nil || !strings.Contains(err.Error(), "older than PostgreSQL 17") {
		t.Errorf("socket with an older binary: %v", err)
	}
	if r, err := tl.Pick(ctx, Where{SocketDir: "/var/run/postgresql"}, 16); err != nil || r.Describe() != "host pg_dump 16.4 (/usr/lib/postgresql/16/bin)" {
		t.Errorf("socket: %v %v", r, err)
	}
	// TCP: binary older than the server → Docker image; major unknown → binary; no Docker → error naming both.
	if r, err := tl.Pick(ctx, Where{}, 17); err != nil || r.Describe() != "docker run postgres:17-alpine --network host" {
		t.Errorf("tcp with an older binary: %v %v", r, err)
	}
	if r, err := tl.Pick(ctx, Where{}, 0); err != nil || !strings.HasPrefix(r.Describe(), "host pg_dump 16.4") {
		t.Errorf("tcp with unknown major: %v %v", r, err)
	}
	// TCP: binary of the same major → binary; binary NEWER than the server → image of the server's major
	// (pg_restore 15 cannot read a pg_dump 16 archive); no Docker → the newer binary.
	if r, err := tl.Pick(ctx, Where{}, 16); err != nil || !strings.HasPrefix(r.Describe(), "host pg_dump 16.4") {
		t.Errorf("tcp with the same major: %v %v", r, err)
	}
	if r, err := tl.Pick(ctx, Where{}, 15); err != nil || r.Describe() != "docker run postgres:15-alpine --network host" {
		t.Errorf("tcp with a binary newer than the server: %v %v", r, err)
	}
	shNoDocker := &fakeShell{out: map[string]string{hostBin + "pg_dump --version": "pg_dump (PostgreSQL) 16.4\n"}, fail: map[string]string{"docker version": "no docker"}}
	if r, err := newTools(shNoDocker, map[string]string{"pg_dump": hostBin + "pg_dump"}).Pick(ctx, Where{}, 15); err != nil || !strings.HasPrefix(r.Describe(), "host pg_dump 16.4") {
		t.Errorf("tcp with a newer binary and no Docker: %v %v", r, err)
	}
	sh = &fakeShell{out: map[string]string{hostBin + "pg_dump --version": "pg_dump (PostgreSQL) 16.4\n"}, fail: map[string]string{"docker version": "no docker"}}
	tl = newTools(sh, map[string]string{"pg_dump": hostBin + "pg_dump"})
	_, err := tl.Pick(ctx, Where{}, 17)
	if err == nil || !strings.Contains(err.Error(), "older than PostgreSQL 17") || !strings.Contains(err.Error(), "Docker is not available") {
		t.Errorf("tcp with nothing usable: %v", err)
	}
	// Look up pg_dump and ask docker only once per Tools.
	n := 0
	for _, c := range sh.calls {
		if strings.HasSuffix(c.Name, "pg_dump") || c.Name == "docker" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("probe results must be cached: %d commands", n)
	}
}

func TestToolsResolveAsksVersionThenRepicks(t *testing.T) {
	hostBin := "/opt/homebrew/opt/libpq/bin/"
	psql := hostBin + "psql -X -A -t -q -v ON_ERROR_STOP=1 -F \x1f -c show server_version_num"
	sh := &fakeShell{out: map[string]string{
		hostBin + "pg_dump --version": "pg_dump (PostgreSQL) 16.4\n",
		psql:                          "170011\n",
		"docker version":              "29\n",
	}}
	tl := &Tools{Shell: sh, GOOS: "darwin", LookPath: lookIn(map[string]string{"pg_dump": hostBin + "pg_dump"}), Glob: noGlob}
	r, major, err := tl.Resolve(context.Background(), Where{}, Conn{Host: "localhost", Port: 15432, User: "postgres", Database: "postgres"}, 0)
	if err != nil || major != 17 || r.Describe() != "docker run postgres:17-alpine via host.docker.internal" {
		t.Errorf("resolve: %v %d %v", r, major, err)
	}
}

func TestDiscoverListsPodsAndHidesCriDockerd(t *testing.T) {
	k8s := &Container{Name: "k8s_postgres_postgres-0_ns_u_0", Image: "postgres:17", Running: true, Kubernetes: true}
	pod := &Pod{Ref: PodRef{Namespace: "ns", Name: "postgres-0", Container: "postgres"}, Image: "postgres:17", Ready: true}
	hs := HostScan{Sockets: map[int]string{5433: "/var/run/postgresql/.s.PGSQL.5433"}}
	cands, _ := Discover(context.Background(), []*Container{k8s}, []*Pod{pod}, hs, nil)
	if len(cands) != 2 || cands[0].Pod != pod || cands[1].Socket == "" {
		t.Fatalf("cands: %+v", cands)
	}
	if cands[0].Label() != "ns/postgres-0 (pod; postgres:17; container postgres)" || cands[0].SuggestName() != "postgres-0" {
		t.Errorf("pod label: %s %s", cands[0].Label(), cands[0].SuggestName())
	}
	if cands[1].Label() != "unix socket /var/run/postgresql (port 5433)" {
		t.Errorf("socket label: %s", cands[1].Label())
	}
}
