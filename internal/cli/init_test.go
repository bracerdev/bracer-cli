package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/storage"
)

// fakeShell fakes Docker and psql: matches by command prefix (after dropping the -e flags).
type fakeShell struct {
	out  map[string]string
	fail map[string]string
	env  [][]string
}

func (f *fakeShell) Run(_ context.Context, c pg.Cmd) (pg.Result, error) {
	var args []string
	for i := 0; i < len(c.Args); i++ {
		if c.Args[i] == "-e" && i+1 < len(c.Args) {
			i++
			continue
		}
		args = append(args, c.Args[i])
	}
	key := c.Name + " " + strings.Join(args, " ")
	f.env = append(f.env, c.Env)
	for p, stderr := range f.fail {
		if strings.HasPrefix(key, p) {
			return pg.Result{}, &pg.ExitError{Cmd: p, Code: 1, Stderr: []byte(stderr)}
		}
	}
	for p, o := range f.out {
		if strings.HasPrefix(key, p) {
			if c.Stdout != nil {
				io.WriteString(c.Stdout, o)
				return pg.Result{}, nil
			}
			return pg.Result{Stdout: []byte(o)}, nil
		}
	}
	return pg.Result{}, fmt.Errorf("fake: unexpected command %q", key)
}

type fakeStore struct{ err error }

func (s fakeStore) Check(context.Context) error                            { return s.err }
func (s fakeStore) Put(context.Context, string, io.Reader, int64) error    { return nil }
func (s fakeStore) Get(context.Context, string) (io.ReadCloser, error)     { return nil, nil }
func (s fakeStore) List(context.Context, string) ([]storage.Object, error) { return nil, nil }
func (s fakeStore) Delete(context.Context, string) error                   { return nil }
func (s fakeStore) Describe() string                                       { return "fake" }

const psql = "docker exec -i shop-db psql -X -A -t -q -v ON_ERROR_STOP=1 -F \x1f -c "

func containerWorld() *fakeShell {
	return &fakeShell{out: map[string]string{
		"docker version": "29.0\n",
		"docker ps":      `{"ID":"c1","Names":"shop-db","Image":"postgres:17-alpine","State":"running","Status":"Up"}` + "\n",
		"docker inspect": `[{"Id":"c1","Name":"/shop-db","State":{"Running":true,"Status":"running"},"Config":{"Image":"postgres:17-alpine","Env":["POSTGRES_PASSWORD=pw","POSTGRES_DB=shop"]},"NetworkSettings":{"Ports":{"5432/tcp":[{"HostIp":"0.0.0.0","HostPort":"15432"}]},"Networks":{"bridge":{"IPAddress":"172.17.0.2"}}}}]`,
		"docker top c1":  "UID PID PPID C STIME TTY TIME CMD\n70 1 0 0 x ? 0 postgres\n",
		"docker image inspect --format {{json .Config.Entrypoint}}\t{{json .Config.Cmd}} postgres:17-alpine": "[\"docker-entrypoint.sh\"]\t[\"postgres\"]\n",
		"ss -ltnpH": "LISTEN 0 128 *:15432 *:* users:((\"ssh\",pid=1,fd=5))\n",
		"docker exec shop-db sh -c command -v pg_dump": "/usr/local/bin/pg_dump\n",
		psql + "select 1":                              "1\n",
		psql + "show server_version_num":               "170011\n",
		psql + "select version()":                      "PostgreSQL 17.11 on aarch64\n",
		psql + "select datname":                        "postgres\nshop\n",
		psql + "select current_user":                   "postgres\x1ft\x1ft\x1ft\n",
		psql + "select n.nspname":                      "",
		"docker exec -i shop-db pg_dump --schema-only": "",
	}, fail: map[string]string{
		psql + "show pool_mode": `ERROR:  unrecognized configuration parameter "pool_mode"`,
	}}
}

// noBinary: the test machine has no kubectl or pg_dump on the host.
func noBinary(string) (string, error) { return "", errors.New("not found") }

// lookIn fakes a PATH that has only the given commands.
func lookIn(paths map[string]string) func(string) (string, error) {
	return func(name string) (string, error) {
		if p, ok := paths[name]; ok {
			return p, nil
		}
		return "", errors.New("not found")
	}
}

func runScripted(t *testing.T, dir string, sh pg.Shell, storeErr error, answers ...string) (int, string, string) {
	return runScriptedLook(t, dir, sh, storeErr, noBinary, answers...)
}

func runScriptedLook(t *testing.T, dir string, sh pg.Shell, storeErr error, lookPath func(string) (string, error), answers ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	in := strings.NewReader(strings.Join(answers, "\n") + "\n")
	d := initDeps{
		shell:    sh,
		probe:    func(context.Context, string, int) (bool, error) { return true, nil },
		newStore: func(storage.S3Config) (storage.Store, error) { return fakeStore{err: storeErr}, nil },
		goos:     "linux",
		lookPath: lookPath,
		glob:     func(string) ([]string, error) { return nil, nil },
		stdin:    in, stdout: &out, stderr: &errb,
	}
	code := runInit(context.Background(), []string{"-c", filepath.Join(dir, "app.yaml")}, d)
	return code, out.String(), errb.String()
}

func TestInitContainerFullFlow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	code, out, errOut := runScripted(t, dir, containerWorld(), nil,
		"shop",                                                      // app name
		"1",                                                         // pick container
		"",                                                          // instance name = shop-db
		"",                                                          // all databases
		"https://s3.example.com", "backups", "", "", "", "AK", "SK", // bucket
		"",  // encrypt: yes
		"",  // generate key
		"",  // key path
		"1", // bracer keeps secrets
		"",  // env path
	)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	cfg, err := config.Load(filepath.Join(dir, "app.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Instances) != 1 || cfg.Instances[0].Container != "shop-db" || cfg.Instances[0].PgMajor != 17 || !cfg.Instances[0].Databases.All || cfg.Instances[0].PasswordEnv != "" {
		t.Errorf("instance: %+v", cfg.Instances[0])
	}
	if cfg.Storage == nil || cfg.Storage.AccessKeyEnv != "BRACER_SHOP_S3_ACCESS_KEY" || cfg.Storage.Prefix != "bracer" {
		t.Errorf("storage: %+v", cfg.Storage)
	}
	// 11b: the container image is of the official family → copied into drill.image, the user need not know.
	if cfg.Drill == nil || cfg.Drill.Image != "postgres:17-alpine" || !strings.Contains(out, "drill will use image postgres:17-alpine") {
		t.Errorf("drill.image: %+v", cfg.Drill)
	}
	if cfg.Encryption != "age" || !strings.HasPrefix(cfg.AgeRecipient, "age1") || cfg.AgeIdentityFile != "~/.config/bracer/shop.key" {
		t.Errorf("encryption: %s %s %s", cfg.Encryption, cfg.AgeRecipient, cfg.AgeIdentityFile)
	}
	if cfg.SecretsFile != "~/.config/bracer/shop.env" {
		t.Errorf("secrets_file = %q", cfg.SecretsFile)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "app.yaml"))
	for _, secret := range []string{"AK", "SK", "AGE-SECRET-KEY"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("secret %s leaked into app.yaml:\n%s", secret, raw)
		}
	}
	env, _ := os.ReadFile(filepath.Join(dir, ".config", "bracer", "shop.env"))
	if string(env) != "BRACER_SHOP_S3_ACCESS_KEY=AK\nBRACER_SHOP_S3_SECRET_KEY=SK\n" {
		t.Errorf("env file:\n%s", env)
	}
	st, _ := os.Stat(filepath.Join(dir, ".config", "bracer", "shop.key"))
	if st == nil || st.Mode().Perm() != 0o600 {
		t.Error("key file missing or not 600")
	}
	if !strings.Contains(out, "published on localhost:15432") || strings.Contains(out, "15432 (on this host") {
		t.Errorf("mapped port must merge into the container:\n%s", out)
	}
	if !strings.Contains(out, "Shown once") {
		t.Error("private key must be shown once with a warning")
	}
	// Second run: add databases to the existing instance, do not ask about bucket/key again.
	code, out, errOut = runScripted(t, dir, containerWorld(), nil,
		"1", // same container
		"",  // add to existing: yes
		"2", // only shop
	)
	if code != 0 {
		t.Fatalf("second run exit %d\n%s\n%s", code, out, errOut)
	}
	cfg, _ = config.Load(filepath.Join(dir, "app.yaml"))
	if len(cfg.Instances) != 1 || !cfg.Instances[0].Databases.All {
		t.Errorf("second run must keep one instance with all: %+v", cfg.Instances)
	}
	if !strings.Contains(out, "✓ shop-db") {
		t.Errorf("existing instance must be re-tested:\n%s", out)
	}
}

func TestInitHostNeedsPasswordAndSelfManagedSecrets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	sh := containerWorld()
	// No container; Postgres on the host at 5433, requires a password.
	sh.out["docker ps"] = ""
	sh.out["ss -ltnpH"] = "LISTEN 0 244 127.0.0.1:5433 0.0.0.0:* users:((\"postgres\",pid=9,fd=7))\n"
	hostPsql := "docker run --rm -i --network host postgres:18-alpine psql -X -A -t -q -v ON_ERROR_STOP=1 -F \x1f -c "
	hostPsql17 := "docker run --rm -i --network host postgres:17-alpine psql -X -A -t -q -v ON_ERROR_STOP=1 -F \x1f -c "
	authFail := true
	sh.out[hostPsql+"show server_version_num"] = "170011\n"
	for _, p := range []string{hostPsql17 + "select version()", hostPsql17 + "select datname", hostPsql17 + "select current_user", hostPsql17 + "select n.nspname", hostPsql17 + "select extname", "docker run --rm -i --network host postgres:17-alpine pg_dump --schema-only"} {
		sh.out[p] = map[string]string{
			hostPsql17 + "select version()":    "PostgreSQL 17.11\n",
			hostPsql17 + "select datname":      "app\n",
			hostPsql17 + "select current_user": "bracer_backup\x1ff\x1ft\x1ft\n",
			hostPsql17 + "select extname":      "uuid-ossp\x1f1.1\n",
		}[p]
	}
	sh.fail[hostPsql17+"show pool_mode"] = "ERROR"
	// 11b: uuid-ossp is in the default image → no question about the drill image.
	sh.out["docker run --rm --network none postgres:17-alpine sh -c ls"] = "plpgsql.control\nuuid-ossp.control\nuuid-ossp--1.1.sql\n"
	// select 1: auth error the first time, ok once PGPASSWORD is set.
	gated := &gateShell{inner: sh, gate: func(c pg.Cmd) (pg.Result, error, bool) {
		if strings.Contains(strings.Join(c.Args, " "), "select 1") {
			if authFail && !hasEnv(c.Env, "PGPASSWORD=s3cret") {
				return pg.Result{}, &pg.ExitError{Code: 2, Stderr: []byte("psql: error: connection to server at \"localhost\" (127.0.0.1), port 5433 failed: fe_sendauth: no password supplied")}, true
			}
			authFail = false
			if c.Stdout != nil {
				io.WriteString(c.Stdout, "1\n")
			}
			return pg.Result{Stdout: []byte("1\n")}, nil, true
		}
		return pg.Result{}, nil, false
	}}
	code, out, errOut := runScripted(t, dir, gated, errors.New("boom"),
		"shop",
		"1",             // local 5433
		"",              // name local-5433
		"bracer_backup", // user after auth failure
		"s3cret",        // password
		"",              // all databases
		"https://s3.example.com", "backups", "", "", "", "AK", "SK",
		"s", // bucket check failed → skip
		"n", // no encryption
		"2", // I manage secrets
	)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	cfg, err := config.Load(filepath.Join(dir, "app.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	in := cfg.Instances[0]
	if in.Name != "local-5433" || in.Host != "localhost" || in.Port != 5433 || in.User != "bracer_backup" || in.PasswordEnv != "BRACER_SHOP_LOCAL_5433_PASSWORD" || in.PgMajor != 17 {
		t.Errorf("instance: %+v", in)
	}
	if cfg.Storage != nil || cfg.Encryption != "none" || cfg.SecretsFile != "" {
		t.Errorf("storage skipped, no encryption, self-managed: %+v %s %s", cfg.Storage, cfg.Encryption, cfg.SecretsFile)
	}
	if _, err := os.Stat(filepath.Join(dir, ".config", "bracer", "shop.env")); !os.IsNotExist(err) {
		t.Error("self-managed: no env file must be written")
	}
	if !strings.Contains(out, "--env-file") || !strings.Contains(out, "BRACER_SHOP_LOCAL_5433_PASSWORD=...") {
		t.Errorf("must print both templates:\n%s", out)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "app.yaml"))
	if strings.Contains(string(raw), "s3cret") {
		t.Error("password leaked into app.yaml")
	}
	for _, env := range sh.env {
		for _, kv := range env {
			if strings.HasPrefix(kv, "PGPASSWORD=") && kv != "PGPASSWORD=s3cret" {
				t.Errorf("unexpected PGPASSWORD env: %s", kv)
			}
		}
	}
}

// Without Docker: init warns and still scans the host; Postgres on 5433 uses
// the host's pg_dump binary (ADR-0009).
func TestInitWithoutDockerUsesHostBinaries(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	bin := "/usr/lib/postgresql/17/bin/"
	psql := bin + "psql -X -A -t -q -v ON_ERROR_STOP=1 -F \x1f -c "
	sh := &fakeShell{out: map[string]string{
		"ss -ltnpH":                      "LISTEN 0 244 127.0.0.1:5433 0.0.0.0:* users:((\"postgres\",pid=9,fd=7))\n",
		bin + "pg_dump --version":        "pg_dump (PostgreSQL) 17.11 (Debian 17.11-1.pgdg12+2)\n",
		psql + "select 1":                "1\n",
		psql + "show server_version_num": "170011\n",
		psql + "select version()":        "PostgreSQL 17.11\n",
		psql + "select datname":          "app\n",
		psql + "select current_user":     "postgres\x1ft\x1ft\x1ft\n",
		psql + "select n.nspname":        "",
		psql + "select extname":          "postgis\x1f3.4.2\n",
		bin + "pg_dump --schema-only":    "",
	}, fail: map[string]string{
		"docker version":        "Cannot connect to the Docker daemon",
		psql + "show pool_mode": "ERROR",
	}}
	code, out, errOut := runScriptedLook(t, dir, sh, nil, lookIn(map[string]string{"pg_dump": bin + "pg_dump"}),
		"shop",
		"1", // local 5433
		"",  // name
		"",  // all databases
		"https://s3.example.com", "backups", "", "", "", "AK", "SK",
		"n", // no encryption
		"2", // I manage secrets
	)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "Docker is not available") || !strings.Contains(errOut, "drill") {
		t.Errorf("must warn about Docker once and go on:\n%s", errOut)
	}
	if !strings.Contains(out, "host pg_dump 17.11 (/usr/lib/postgresql/17/bin)") {
		t.Errorf("runner must be the host binary:\n%s", out)
	}
	cfg, err := config.Load(filepath.Join(dir, "app.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if in := cfg.Instances[0]; in.Host != "localhost" || in.Port != 5433 || in.PgMajor != 17 {
		t.Errorf("instance: %+v", in)
	}
	for _, env := range sh.env {
		for _, kv := range env {
			if strings.HasPrefix(kv, "PGPASSWORD=") {
				t.Error("no password was given; none must be passed")
			}
		}
	}
}

// Postgres in a k3s pod: discovered via kubectl, connected with kubectl exec, no password.
func TestInitPodFlow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	exec := "kubectl exec -i -n attendy-dev postgres-0 -c postgres -- env PGUSER=attendy PGDATABASE=postgres PGCONNECT_TIMEOUT=10 "
	psql := exec + "psql -X -A -t -q -v ON_ERROR_STOP=1 -F \x1f -c "
	sh := &fakeShell{out: map[string]string{
		"docker version": "29.0\n",
		"docker ps":      `{"ID":"k1","Names":"k8s_postgres_postgres-0_attendy-dev_u_0","Image":"attendy-postgres:sha","State":"running","Status":"Up"}` + "\n",
		"docker inspect": `[{"Id":"k1","Name":"/k8s_postgres_postgres-0_attendy-dev_u_0","State":{"Running":true,"Status":"running"},"Config":{"Image":"attendy-postgres:sha","Env":["POSTGRES_USER=attendy"]},"NetworkSettings":{"Ports":{},"Networks":{}}}]`,
		"ss -ltnpH":      "",
		"kubectl get pods -A -o json": `{"items":[
		  {"metadata":{"namespace":"attendy-dev","name":"postgres-0"},"spec":{"containers":[{"name":"postgres","image":"attendy-postgres:sha","env":[{"name":"POSTGRES_USER","value":"attendy"},{"name":"POSTGRES_PASSWORD","valueFrom":{"secretKeyRef":{"name":"pg","key":"pw"}}}]}]},"status":{"phase":"Running","containerStatuses":[{"name":"postgres","ready":true}]}},
		  {"metadata":{"namespace":"attendy-dev","name":"attendy-api-1"},"spec":{"containers":[{"name":"api","image":"attendy-api:sha","env":[{"name":"POSTGRES_SERVICE_HOST","value":"10.43.0.1"}]}]},"status":{"phase":"Running","containerStatuses":[{"name":"api","ready":true}]}},
		  {"metadata":{"namespace":"slidemaker-dev","name":"postgres-backup-1"},"spec":{"containers":[{"name":"dump","image":"postgres:17"}]},"status":{"phase":"Succeeded"}}
		]}`,
		"kubectl exec -n attendy-dev postgres-0 -c postgres -- sh -c command -v pg_dump": "/usr/bin/pg_dump\n",
		"kubectl exec -n attendy-dev postgres-0 -c postgres -- sh -c for f in /proc":     "1\n",
		psql + "select 1":                "1\n",
		psql + "show server_version_num": "170011\n",
		psql + "select version()":        "PostgreSQL 17.11\n",
		psql + "select datname":          "attendy\npostgres\n",
		psql + "select current_user":     "attendy\x1ft\x1ft\x1ft\n",
		strings.Replace(psql, "PGDATABASE=postgres", "PGDATABASE=attendy", 1) + "select n.nspname":      "",
		strings.Replace(exec, "PGDATABASE=postgres", "PGDATABASE=attendy", 1) + "pg_dump --schema-only": "",
	}, fail: map[string]string{
		psql + "show pool_mode": "ERROR",
	}}
	code, out, errOut := runScriptedLook(t, dir, sh, nil, lookIn(map[string]string{"kubectl": "/usr/local/bin/kubectl"}),
		"shop",
		"1", // the pod
		"",  // name postgres-0
		"1", // database attendy
		"https://s3.example.com", "backups", "", "", "", "AK", "SK",
		"n", // no encryption
		"2", // I manage secrets
	)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "1) attendy-dev/postgres-0 (pod; attendy-postgres:sha; container postgres)") || strings.Contains(out, "k8s_") || strings.Contains(out, "attendy-api") {
		t.Errorf("only the Postgres pod must be listed, not the cri-dockerd container or other pods:\n%s", out)
	}
	if !strings.Contains(out, "via kubectl exec attendy-dev/postgres-0/postgres") {
		t.Errorf("runner must be kubectl exec:\n%s", out)
	}
	cfg, err := config.Load(filepath.Join(dir, "app.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	in := cfg.Instances[0]
	if in.Name != "postgres-0" || in.Pod != "attendy-dev/postgres-0/postgres" || in.User != "attendy" || in.PgMajor != 17 || in.PasswordEnv != "" || len(in.Databases.List) != 1 || in.Databases.List[0].Name != "attendy" {
		t.Errorf("instance: %+v", in)
	}
}

// A node without Docker (k3s with containerd): init writes drill.runtime: kube and
// the pod's namespace once kubectl confirms it may create pods and network policies there.
func TestInitPodFlowWithoutDockerSetsDrillRuntime(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	exec := "kubectl exec -i -n attendy-dev postgres-0 -c postgres -- env PGUSER=attendy PGDATABASE=postgres PGCONNECT_TIMEOUT=10 "
	psql := exec + "psql -X -A -t -q -v ON_ERROR_STOP=1 -F \x1f -c "
	sh := &fakeShell{out: map[string]string{
		"ss -ltnpH": "",
		"kubectl get pods -A -o json": `{"items":[
		  {"metadata":{"namespace":"attendy-dev","name":"postgres-0"},"spec":{"containers":[{"name":"postgres","image":"postgres:17-alpine","env":[{"name":"POSTGRES_USER","value":"attendy"}]}]},"status":{"phase":"Running","containerStatuses":[{"name":"postgres","ready":true}]}}]}`,
		"kubectl exec -n attendy-dev postgres-0 -c postgres -- sh -c command -v pg_dump": "/usr/bin/pg_dump\n",
		"kubectl exec -n attendy-dev postgres-0 -c postgres -- sh -c for f in /proc":     "1\n",
		"kubectl get pod -n attendy-dev postgres-0 -o json":                              `{"spec":{"containers":[{"name":"postgres","image":"postgres:17-alpine"}]}}`,
		"kubectl auth can-i create pods -n attendy-dev":                                  "yes\n",
		"kubectl auth can-i create networkpolicies.networking.k8s.io -n attendy-dev":     "yes\n",
		psql + "select 1":                "1\n",
		psql + "show server_version_num": "170011\n",
		psql + "select version()":        "PostgreSQL 17.11 on x86_64\n",
		psql + "select datname":          "postgres\nattendy\n",
		psql + "select current_user":     "attendy\x1ft\x1ft\x1ft\n",
		strings.Replace(psql, "PGDATABASE=postgres", "PGDATABASE=attendy", 1) + "select n.nspname":      "",
		strings.Replace(exec, "PGDATABASE=postgres", "PGDATABASE=attendy", 1) + "pg_dump --schema-only": "",
	}, fail: map[string]string{
		"docker version":        "Cannot connect to the Docker daemon",
		psql + "show pool_mode": "ERROR",
	}}
	code, out, errOut := runScriptedLook(t, dir, sh, nil, lookIn(map[string]string{"kubectl": "/usr/local/bin/kubectl"}),
		"shop",
		"1", // the pod
		"",  // name postgres-0
		"2", // database attendy
		"https://s3.example.com", "backups", "", "", "", "AK", "SK",
		"n", // no encryption
		"2", // I manage secrets
	)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "Docker is not available") || !strings.Contains(out, "drill will run its temporary Postgres as a pod in namespace attendy-dev through kubectl") {
		t.Errorf("must warn about Docker and announce the pod runtime:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	cfg, err := config.Load(filepath.Join(dir, "app.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Drill == nil || cfg.Drill.Runtime != "kube" || cfg.Drill.Namespace != "attendy-dev" || cfg.Drill.Image != "postgres:17-alpine" {
		t.Errorf("drill: %+v", cfg.Drill)
	}
}

// The pod gets POSTGRES_USER via an envFrom Secret: the pod spec has no value. init
// asks the container; if that fails, the server reports the user does not exist → ask for the user.
func TestInitPodUserFromSecret(t *testing.T) {
	podJSON := `{"items":[{"metadata":{"namespace":"slidemaker","name":"postgres-0"},"spec":{"containers":[{"name":"postgres","image":"postgres:17-alpine","envFrom":[{"secretRef":{"name":"db-credentials"}}],"env":[{"name":"PGDATA","value":"/var/lib/postgresql/data/pgdata"}]}]},"status":{"phase":"Running","containerStatuses":[{"name":"postgres","ready":true}]}},
	  {"metadata":{"namespace":"jumpsmash","name":"jumpsmash-api-1"},"spec":{"containers":[{"name":"api","image":"jumpsmash-api:8cdf540","env":[{"name":"POSTGRES_USER"},{"name":"POSTGRES_PASSWORD"},{"name":"DATABASE_URL"}]}]},"status":{"phase":"Running","containerStatuses":[{"name":"api","ready":true}]}}]}`
	world := func(resolved string) *fakeShell {
		exec := "kubectl exec -i -n slidemaker postgres-0 -c postgres -- env PGUSER=slidemaker PGDATABASE=postgres PGCONNECT_TIMEOUT=10 "
		psql := exec + "psql -X -A -t -q -v ON_ERROR_STOP=1 -F \x1f -c "
		return &fakeShell{out: map[string]string{
			"ss -ltnpH":                   "",
			"kubectl get pods -A -o json": podJSON,
			"kubectl exec -n slidemaker postgres-0 -c postgres -- sh -c for f in /proc":     "1\n",
			"kubectl exec -n slidemaker postgres-0 -c postgres -- sh -c u=":                 resolved,
			"kubectl exec -n slidemaker postgres-0 -c postgres -- sh -c command -v pg_dump": "/usr/bin/pg_dump\n",
			psql + "select 1":                                  "1\n",
			psql + "show server_version_num":                   "170011\n",
			psql + "select version()":                          "PostgreSQL 17.11\n",
			psql + "select datname":                            "slidemaker\npostgres\n",
			"kubectl get pod -n slidemaker postgres-0 -o json": `{"spec":{"containers":[{"name":"postgres","image":"postgres:17-alpine"}]}}`,
			psql + "select current_user":                       "slidemaker\x1ft\x1ft\x1ft\n",
			strings.Replace(psql, "PGDATABASE=postgres", "PGDATABASE=slidemaker", 1) + "select n.nspname":      "",
			strings.Replace(exec, "PGDATABASE=postgres", "PGDATABASE=slidemaker", 1) + "pg_dump --schema-only": "",
		}, fail: map[string]string{
			"docker version": "Cannot connect to the Docker daemon",
			// App pod with only env hints and no sh: cannot be checked → not numbered.
			"kubectl exec -n jumpsmash jumpsmash-api-1 -c api -- sh -c for f in /proc": `exec failed: "sh": executable file not found`,
			psql + "show pool_mode": "ERROR",
			// Default postgres: user does not exist.
			"kubectl exec -i -n slidemaker postgres-0 -c postgres -- env PGUSER=postgres": `psql: error: connection to server on socket "/var/run/postgresql/.s.PGSQL.5432" failed: FATAL:  role "postgres" does not exist`,
		}}
	}
	check := func(t *testing.T, dir string, code int, out, errOut string) {
		t.Helper()
		if code != 0 {
			t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
		}
		if strings.Contains(out, "Try again?") {
			t.Errorf("must not loop on Try again:\n%s", out)
		}
		if strings.Contains(out, "2) jumpsmash") || !strings.Contains(out, "1 pod(s) have Postgres-like variables but could not be checked for a postgres process (no shell in the container): jumpsmash/jumpsmash-api-1/api") {
			t.Errorf("unchecked env-only pod must be a note, not a numbered candidate:\n%s", out)
		}
		cfg, err := config.Load(filepath.Join(dir, "app.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if in := cfg.Instances[0]; in.Pod != "slidemaker/postgres-0/postgres" || in.User != "slidemaker" || in.PasswordEnv != "" {
			t.Errorf("instance: %+v", in)
		}
		if cfg.Drill == nil || cfg.Drill.Image != "postgres:17-alpine" {
			t.Errorf("drill.image from pod spec: %+v", cfg.Drill)
		}
	}
	look := lookIn(map[string]string{"kubectl": "/usr/local/bin/kubectl"})
	s3 := []string{"https://s3.example.com", "backups", "", "", "", "AK", "SK", "n", "2"}

	// A. The container answers POSTGRES_USER → connect right away with that user, no questions.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	code, out, errOut := runScriptedLook(t, dir, world("slidemaker"), nil, look, append([]string{"shop", "1", "", "1"}, s3...)...)
	check(t, dir, code, out, errOut)
	if strings.Contains(out, "does not exist") {
		t.Errorf("user must come from the container, no role error expected:\n%s", out)
	}

	// B. The container does not answer → try postgres, the server reports the user does not exist → ask for the user.
	dir = t.TempDir()
	t.Setenv("HOME", dir)
	code, out, errOut = runScriptedLook(t, dir, world(""), nil, look, append([]string{"shop", "1", "", "slidemaker", "1"}, s3...)...)
	check(t, dir, code, out, errOut)
	if !strings.Contains(out, `User postgres does not exist on this server`) || !strings.Contains(out, "User [postgres]:") {
		t.Errorf("must explain and ask for the user:\n%s", out)
	}

	// C. Manually entered ns/pod/container (unknown image, not listed automatically): still check the process, then connect.
	dir = t.TempDir()
	t.Setenv("HOME", dir)
	code, out, errOut = runScriptedLook(t, dir, world("slidemaker"), nil, look, append([]string{"shop", "m", "slidemaker/postgres-0/postgres", "", "1"}, s3...)...)
	check(t, dir, code, out, errOut)

	// D. Manually entered pod without a postgres process → rejected.
	w := world("")
	w.out["kubectl exec -n slidemaker slidemaker-app-1 -c app -- sh -c for f in /proc"] = "none\n"
	dir = t.TempDir()
	t.Setenv("HOME", dir)
	code, out, _ = runScriptedLook(t, dir, w, nil, look, "shop", "m", "slidemaker/slidemaker-app-1/app")
	if code == 0 || !strings.Contains(out, "no postgres process is running in slidemaker/slidemaker-app-1/app") {
		t.Errorf("exit %d, out:\n%s", code, out)
	}
}

func TestInitInterruptedWritesNothing(t *testing.T) {
	dir := t.TempDir()
	code, _, errOut := runScripted(t, dir, containerWorld(), nil, "shop", "1") // input ends midway
	if code != exitUsage || !strings.Contains(errOut, "interactive terminal") {
		t.Errorf("exit %d, stderr: %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "app.yaml")); !os.IsNotExist(err) {
		t.Error("nothing must be written when input ends early")
	}
}

// gateShell intercepts some commands before they reach inner.
type gateShell struct {
	inner pg.Shell
	gate  func(pg.Cmd) (pg.Result, error, bool)
}

func (g *gateShell) Run(ctx context.Context, c pg.Cmd) (pg.Result, error) {
	if res, err, handled := g.gate(c); handled {
		return res, err
	}
	return g.inner.Run(ctx, c)
}

func hasEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}
