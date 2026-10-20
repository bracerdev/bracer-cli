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
		"docker info":    "29.0\n",
		"docker context inspect --format {{.Endpoints.docker.Host}}": "unix:///var/run/docker.sock\n",
		"docker ps":      `{"ID":"c1","Names":"shop-db","Image":"postgres:17-alpine","State":"running","Status":"Up"}` + "\n",
		"docker inspect": `[{"Id":"c1","Name":"/shop-db","State":{"Running":true,"Status":"running"},"Config":{"Image":"postgres:17-alpine","Env":["POSTGRES_PASSWORD=pw","POSTGRES_DB=shop"]},"NetworkSettings":{"Ports":{"5432/tcp":[{"HostIp":"0.0.0.0","HostPort":"15432"}]},"Networks":{"bridge":{"IPAddress":"172.17.0.2"}}}}]`,
		"docker top c1":  "UID PID PPID C STIME TTY TIME CMD\n70 1 0 0 x ? 0 postgres\n",
		"docker image inspect --format {{json .Config.Entrypoint}}\t{{json .Config.Cmd}} postgres:17-alpine": "[\"docker-entrypoint.sh\"]\t[\"postgres\"]\n",
		"ss -ltnpH": "LISTEN 0 128 *:15432 *:* users:((\"ssh\",pid=1,fd=5))\n",
		"docker exec shop-db sh -c command -v pg_dump": "/usr/local/bin/pg_dump\n",
		psql + "select 1":                              "1\n",
		psql + "show server_version_num":               "170011\n",
		psql + "select version()":                      "PostgreSQL 17.11 on aarch64\n",
		psql + "select system_identifier":              "7698217040907026472\n",
		psql + "select datname":                        "postgres\nshop\n",
		psql + "select current_user":                   "postgres\x1ft\x1ft\x1ft\n",
		psql + "select n.nspname":                      "",
		"docker exec -i shop-db pg_dump --schema-only": "",
	}, fail: map[string]string{
		psql + "show pool_mode": `ERROR:  unrecognized configuration parameter "pool_mode"`,
	}}
}

// world is what init sees around it besides the shell.
type world struct {
	sh      pg.Shell
	store   error // the first bucket check fails with it, later ones pass
	systemd bool
	selfRun func(args []string) int // nil: every self-check passes
	selfLog [][]string              // args then env of each self-check
	tty     []string                // the command run on the terminal
}

func runScripted(t *testing.T, dir string, sh pg.Shell, storeErr error, answers ...string) (int, string, string) {
	return runWorld(t, dir, &world{sh: sh, store: storeErr}, nil, answers...)
}

func runWorld(t *testing.T, dir string, w *world, extra []string, answers ...string) (int, string, string) {
	t.Helper()
	pg.KubernetesDirs = []string{filepath.Join(dir, "no-k3s")}
	var out, errb bytes.Buffer
	in := strings.NewReader(strings.Join(answers, "\n") + "\n")
	d := initDeps{
		shell: w.sh,
		probe: func(context.Context, string, int) (bool, error) { return true, nil },
		newStore: func(storage.S3Config) (storage.Store, error) {
			err := w.store
			w.store = nil
			return fakeStore{err: err}, nil
		},
		env: envDeps{
			lookPath:   func(name string) (string, error) { return "/usr/bin/" + name, nil },
			getenv:     func(string) string { return "" },
			userName:   func() string { return "khanh" },
			hasSystemd: func() bool { return w.systemd },
		},
		home: func() (string, error) { return dir, nil },
		runSelf: func(_ context.Context, args, env []string, _, _ io.Writer) int {
			w.selfLog = append(w.selfLog, args, env)
			if w.selfRun != nil {
				return w.selfRun(args)
			}
			return 0
		},
		runTTY: func(_ context.Context, name string, args ...string) error {
			w.tty = append([]string{name}, args...)
			return nil
		},
		stdin: in, stdout: &out, stderr: &errb,
	}
	code := runInit(context.Background(), append([]string{"-c", filepath.Join(dir, "app.yaml")}, extra...), d)
	return code, out.String(), errb.String()
}

var s3Answers = []string{"https://s3.example.com", "backups", "", "", "", "AK", "SK"}

func TestInitContainerFullFlow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	w := &world{sh: containerWorld()}
	code, out, errOut := runWorld(t, dir, w, nil, append(append([]string{
		"shop", // app name
		"1",    // pick container
		"",     // instance name = shop-db
		"",     // all databases
	}, s3Answers...),
		"",       // encrypt: yes
		"",       // generate key
		"",       // key path
		"later",  // not "copied": asked again
		"copied", // key copied off the machine (ADR-0015)
	)...)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if strings.Count(out, "Type copied when done") != 2 {
		t.Errorf("the key must be confirmed copied:\n%s", out)
	}
	cfg, err := config.Load(filepath.Join(dir, "app.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	in := cfg.Instances[0]
	if len(cfg.Instances) != 1 || in.Container != "shop-db" || in.PgMajor != 17 || in.SystemID != "7698217040907026472" || !in.Databases.All || in.PasswordEnv != "" {
		t.Errorf("instance: %+v", in)
	}
	if cfg.Storage == nil || cfg.Storage.AccessKeyEnv != "BRACER_SHOP_S3_ACCESS_KEY" || cfg.Storage.Prefix != "bracer" {
		t.Errorf("storage: %+v", cfg.Storage)
	}
	// 11 (ADR-0015): drill runs the source container's own image from the environment record; drill.image stays unset.
	if (cfg.Drill != nil && cfg.Drill.Image != "") || !strings.Contains(out, "drill will use the same image as this Postgres (postgres:17-alpine)") {
		t.Errorf("drill.image: %+v", cfg.Drill)
	}
	// Absolute paths: timers have no shell to expand `~`.
	key := filepath.Join(dir, ".config", "bracer", "shop.key")
	envPath := filepath.Join(dir, ".config", "bracer", "shop.env")
	if cfg.Encryption != "age" || !strings.HasPrefix(cfg.AgeRecipient, "age1") || cfg.AgeIdentityFile != key {
		t.Errorf("encryption: %s %s %s", cfg.Encryption, cfg.AgeRecipient, cfg.AgeIdentityFile)
	}
	if cfg.SecretsFile != envPath || !strings.Contains(out, "Secrets saved to "+envPath) {
		t.Errorf("secrets_file = %q", cfg.SecretsFile)
	}
	rt := cfg.Runtime
	if rt == nil || rt.User != "khanh" || rt.Docker != "/usr/bin/docker" || rt.DockerHost != "unix:///var/run/docker.sock" || rt.Path != "/usr/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/sbin:/bin" {
		t.Errorf("runtime: %+v", rt)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "app.yaml"))
	for _, secret := range []string{"AK", "SK", "AGE-SECRET-KEY"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("secret %s leaked into app.yaml:\n%s", secret, raw)
		}
	}
	env, _ := os.ReadFile(envPath)
	if string(env) != "BRACER_SHOP_S3_ACCESS_KEY=AK\nBRACER_SHOP_S3_SECRET_KEY=SK\n" {
		t.Errorf("env file:\n%s", env)
	}
	st, _ := os.Stat(key)
	if st == nil || st.Mode().Perm() != 0o600 {
		t.Error("key file missing or not 600")
	}
	if !strings.Contains(out, "published on localhost:15432") || strings.Contains(out, "15432 (on this host") {
		t.Errorf("mapped port must merge into the container:\n%s", out)
	}
	if !strings.Contains(out, "Shown once") || !strings.Contains(out, "✓ docker: docker info answers as khanh") {
		t.Errorf("key shown once, environment table first:\n%s", out)
	}
	// 14: both dry runs, with only HOME and runtime.path; 15: no systemd → crontab hint.
	abs := filepath.Join(dir, "app.yaml")
	wantEnv := "HOME=" + dir + " PATH=" + rt.Path
	if len(w.selfLog) != 4 || strings.Join(w.selfLog[0], " ") != "backup --dry-run -c "+abs || strings.Join(w.selfLog[1], " ") != wantEnv || strings.Join(w.selfLog[2], " ") != "drill --dry-run -c "+abs {
		t.Errorf("self-check: %q", w.selfLog)
	}
	if !strings.Contains(out, "schedule print -c "+abs) {
		t.Errorf("without systemd init points at schedule print:\n%s", out)
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

// localWorld: no container; Postgres on this machine at 127.0.0.1:5433 that wants a password.
func localWorld(password string) (*gateShell, *fakeShell) {
	sh := containerWorld()
	sh.out["docker ps"] = ""
	sh.out["ss -ltnpH"] = "LISTEN 0 244 127.0.0.1:5433 0.0.0.0:* users:((\"postgres\",pid=9,fd=7))\n"
	hostPsql := "docker run --rm -i --network host postgres:18-alpine psql -X -A -t -q -v ON_ERROR_STOP=1 -F \x1f -c "
	hostPsql17 := "docker run --rm -i --network host postgres:17-alpine psql -X -A -t -q -v ON_ERROR_STOP=1 -F \x1f -c "
	sh.out[hostPsql+"show server_version_num"] = "170011\n"
	sh.out[hostPsql17+"select version()"] = "PostgreSQL 17.11\n"
	sh.out[hostPsql17+"select datname"] = "app\n"
	sh.out[hostPsql17+"select current_user"] = "bracer_backup\x1ff\x1ft\x1ft\n"
	sh.out[hostPsql17+"select n.nspname"] = ""
	sh.out[hostPsql17+"select extname"] = "uuid-ossp\x1f1.1\n"
	sh.out["docker run --rm -i --network host postgres:17-alpine pg_dump --schema-only"] = ""
	sh.out["docker pull --quiet postgres:17-alpine"] = ""
	sh.fail[hostPsql17+"show pool_mode"] = "ERROR"
	sh.fail["docker image inspect --format {{.Id}} postgres:17-alpine"] = "No such image"
	sh.fail[hostPsql17+"select system_identifier"] = "ERROR:  permission denied for function pg_control_system"
	// 11: uuid-ossp is in the default image → no question about the drill image.
	sh.out["docker run --rm --network none postgres:17-alpine sh -c ls"] = "plpgsql.control\nuuid-ossp.control\nuuid-ossp--1.1.sql\n"
	gated := &gateShell{inner: sh, gate: func(c pg.Cmd) (pg.Result, error, bool) {
		if strings.Contains(strings.Join(c.Args, " "), "select 1") {
			if !hasEnv(c.Env, "PGPASSWORD="+password) {
				return pg.Result{}, &pg.ExitError{Cmd: "psql", Code: 2, Stderr: []byte(`psql: error: connection to server at "127.0.0.1", port 5433 failed: FATAL:  password authentication failed for user "bracer_backup"`)}, true
			}
			if c.Stdout != nil {
				io.WriteString(c.Stdout, "1\n")
				return pg.Result{}, nil, true
			}
			return pg.Result{Stdout: []byte("1\n")}, nil, true
		}
		return pg.Result{}, nil, false
	}}
	return gated, sh
}

// spec init AC: Postgres on this machine → user + password → URL built by init
// (percent-encoded) in secrets_file, app.yaml has only url_env. → ADR-0013
func TestInitLocalPostgresBuildsURL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	gated, sh := localWorld("p@ss:w/rd")
	answers := []string{
		"shop",
		"1",     // localhost:5433
		"",      // name local-5433
		"",      // user bracer_backup
		"wrong", // password: rejected
		"",      // user again
		"p@ss:w/rd",
		"", // all databases
	}
	answers = append(answers, s3Answers...)
	answers = append(answers, "r") // bucket check failed → retry
	answers = append(answers, s3Answers...)
	answers = append(answers, "n") // no encryption
	code, out, errOut := runWorld(t, dir, &world{sh: gated, store: errors.New("boom")}, nil, answers...)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	cfg, err := config.Load(filepath.Join(dir, "app.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage == nil || cfg.Storage.Bucket != "backups" {
		t.Errorf("storage after retry: %+v", cfg.Storage)
	}
	in := cfg.Instances[0]
	if in.Name != "local-5433" || in.URLEnv != "BRACER_SHOP_LOCAL_5433_URL" || in.User != "" || in.PasswordEnv != "" || in.PgMajor != 17 || in.SystemID != "" {
		t.Errorf("instance: %+v", in)
	}
	env, _ := os.ReadFile(filepath.Join(dir, ".config", "bracer", "shop.env"))
	if string(env) != "BRACER_SHOP_LOCAL_5433_URL=postgres://bracer_backup:p%40ss%3Aw%2Frd@127.0.0.1:5433/postgres\nBRACER_SHOP_S3_ACCESS_KEY=AK\nBRACER_SHOP_S3_SECRET_KEY=SK\n" {
		t.Errorf("env file:\n%s", env)
	}
	if !strings.Contains(out, "sudo -u postgres psql -p 5433\n") || !strings.Contains(out, "GRANT pg_read_all_data TO bracer_backup") || !strings.Contains(out, "Pulling postgres:17-alpine") {
		t.Errorf("must show how to create the user and pull the image:\n%s", out)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "app.yaml"))
	if strings.Contains(string(raw), "p@ss") || strings.Contains(string(raw), "5433/") || strings.Contains(string(raw), " host:") {
		t.Errorf("app.yaml must hold only url_env:\n%s", raw)
	}
	for _, env := range sh.env {
		for _, kv := range env {
			if strings.HasPrefix(kv, "PGPASSWORD=") && kv != "PGPASSWORD=p@ss:w/rd" && kv != "PGPASSWORD=wrong" {
				t.Errorf("unexpected PGPASSWORD env: %s", kv)
			}
		}
	}
}

// spec init AC step 0: nothing is asked or written when Docker cannot be used;
// the fixes come in order and the last line is the command typed.
func TestInitEnvironmentBlocks(t *testing.T) {
	dir := t.TempDir()
	sh := &fakeShell{out: map[string]string{
		"docker context inspect": "unix:///var/run/docker.sock\n",
	}, fail: map[string]string{
		"docker info": "permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock",
	}}
	code, out, _ := runWorld(t, dir, &world{sh: sh}, []string{"--no-drill"})
	abs := filepath.Join(dir, "app.yaml")
	if code != exitError || !strings.Contains(out, "✗ docker: khanh may not use Docker") || !strings.Contains(out, "1. sudo usermod -aG docker khanh   # the docker group is equivalent to root") ||
		!strings.Contains(out, "2. log out and back in") || !strings.Contains(out, "3. bracer init -c "+abs+" --no-drill\n") || strings.Contains(out, "App name") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Error("nothing must be written")
	}

	// Rootless Docker without lingering: enable-linger, no logout needed.
	sh = &fakeShell{out: map[string]string{
		"docker info":                 "29.0\n",
		"docker context inspect":      "unix:///run/user/1000/docker.sock\n",
		"loginctl show-user khanh -p": "Linger=no\n",
	}}
	code, out, _ = runWorld(t, dir, &world{sh: sh, systemd: true}, nil)
	if code != exitError || !strings.Contains(out, "✗ rootless docker") || !strings.Contains(out, "1. sudo loginctl enable-linger khanh\n  2. bracer init -c "+abs) || !strings.Contains(out, "✓ systemd") {
		t.Errorf("rootless: exit %d:\n%s", code, out)
	}

	// bracer check: same table, exit 0 when nothing blocks.
	sh.out["loginctl show-user khanh -p"] = "Linger=yes\n"
	var buf bytes.Buffer
	ed := envDeps{shell: sh, lookPath: func(n string) (string, error) { return "/usr/bin/" + n, nil }, getenv: func(string) string { return "" }, userName: func() string { return "khanh" }, hasSystemd: func() bool { return false }}
	if c := runCheck(context.Background(), nil, ed, &buf, &buf); c != exitOK || !strings.Contains(buf.String(), "✓ rootless docker: lingering is on for khanh") || !strings.Contains(buf.String(), "✗ systemd: not found") {
		t.Errorf("check: exit %d:\n%s", c, buf.String())
	}
}

// --no-drill → drill.runtime: none, no drill.image, the self-check skips drill;
// with systemd init offers schedule install and runs it on the terminal.
func TestInitNoDrillAndSchedule(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	w := &world{sh: containerWorld(), systemd: true}
	code, out, errOut := runWorld(t, dir, w, []string{"--no-drill"}, append(append([]string{"shop", "1", "", ""}, s3Answers...), "n", "")...)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	cfg, err := config.Load(filepath.Join(dir, "app.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(dir, "app.yaml")
	if !cfg.DrillOff() || cfg.Drill.Image != "" || !strings.Contains(out, "backups on this machine will not be test-restored; every report will say so") {
		t.Errorf("drill: %+v\n%s", cfg.Drill, out)
	}
	if len(w.selfLog) != 2 || w.selfLog[0][0] != "backup" {
		t.Errorf("self-check must skip drill: %q", w.selfLog)
	}
	if len(w.tty) < 5 || w.tty[0] != "sudo" || strings.Join(w.tty[2:], " ") != "schedule install -c "+abs || !strings.Contains(out, "Install the nightly schedule now? It runs: sudo ") {
		t.Errorf("schedule: %q\n%s", w.tty, out)
	}
}

// A failed dry run keeps app.yaml, prints the command to run again and exits 1.
func TestInitSelfCheckFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	w := &world{sh: containerWorld(), systemd: true, selfRun: func(args []string) int {
		if args[0] == "drill" {
			return 1
		}
		return 0
	}}
	code, out, _ := runWorld(t, dir, w, nil, append(append([]string{"shop", "1", "", ""}, s3Answers...), "n")...)
	if code != exitError || !strings.Contains(out, "Self-check failed (exit 1)") || strings.Contains(out, "Install the nightly schedule") || w.tty != nil {
		t.Errorf("exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "app.yaml")); err != nil {
		t.Error("app.yaml must be kept")
	}
}

// spec init AC: an instance from before ADR-0013 is listed as "needs to be picked
// again"; the replacement keeps its name and databases; the rest of app.yaml stays.
func TestInitLegacyInstancePickedAgain(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	path := filepath.Join(dir, "app.yaml")
	os.WriteFile(path, []byte(`app: shop
instances:
  - name: main
    pod: attendy-dev/postgres-0
    databases: [shop]
  - name: old
    socket: /var/run/postgresql
    databases: all
storage:
  endpoint: https://s3.example.com
  bucket: backups
  prefix: bracer
  access_key_env: BRACER_SHOP_S3_ACCESS_KEY
  secret_key_env: BRACER_SHOP_S3_SECRET_KEY
encryption: none
drill:
  runtime: kube
  namespace: attendy-dev
`), 0o644)
	code, out, errOut := runScripted(t, dir, containerWorld(), nil,
		"1", // the container
		"1", // it replaces main
	)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "main (pod) needs to be picked again") || !strings.Contains(out, "Keeping the databases of main: shop") || !strings.Contains(errOut, "instance old (socket, before ADR-0013) was not picked again") {
		t.Errorf("stdout:\n%s\nstderr:\n%s", out, errOut)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	in := cfg.Instances[0]
	if len(cfg.Instances) != 1 || in.Name != "main" || in.Container != "shop-db" || len(in.Databases.List) != 1 || in.Databases.List[0].Name != "shop" || cfg.Storage.Bucket != "backups" {
		t.Errorf("app.yaml: %+v", cfg)
	}
	if cfg.Drill.Runtime != "" || cfg.Drill.Namespace != "" {
		t.Errorf("drill: %+v", cfg.Drill)
	}
}

// Kubernetes is never reached; the k8s_ containers seen through Docker become one notice.
func TestInitKubernetesNotice(t *testing.T) {
	dir := t.TempDir()
	sh := containerWorld()
	sh.out["docker ps"] = `{"ID":"k1","Names":"k8s_postgres_postgres-0_attendy-dev_u_0","Image":"postgres:17","State":"running","Status":"Up"}` + "\n"
	sh.out["docker inspect"] = `[{"Id":"k1","Name":"/k8s_postgres_postgres-0_attendy-dev_u_0","State":{"Running":true,"Status":"running"},"Config":{"Image":"postgres:17","Env":["POSTGRES_USER=attendy"]},"NetworkSettings":{"Ports":{},"Networks":{}}}]`
	sh.out["docker top k1"] = "UID PID PPID C STIME TTY TIME CMD\n70 1 0 0 x ? 0 postgres\n"
	sh.out["ss -ltnpH"] = ""
	_, out, _ := runScripted(t, dir, sh, nil, "shop")
	if !strings.Contains(out, "Kubernetes is not supported (seen: 1 Kubernetes container(s) seen through Docker)") || strings.Contains(out, "1) k8s_") || !strings.Contains(out, "No Postgres found.") {
		t.Errorf("out:\n%s", out)
	}
}

// spec init AC: the bucket is mandatory; a failing check offers retry or quit,
// never skip.
func TestInitBucketFailQuits(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	code, out, _ := runWorld(t, dir, &world{sh: containerWorld(), store: errors.New("403 Forbidden")}, nil, append(append([]string{
		"shop", "1", "", "",
	}, s3Answers...), "q")...)
	if code != exitError || !strings.Contains(out, "403 Forbidden") || !strings.Contains(out, "Nothing written") || strings.Contains(out, "skip") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "app.yaml")); !os.IsNotExist(err) {
		t.Error("nothing must be written without a bucket")
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

// spec init § Edge cases: pg_hba.conf has no host line → the line to add,
// where the file is (psql -p for a cluster not on 5432), [r] retries.
func TestInitLocalPostgresHbaHint(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	inner, _ := localWorld("pw")
	refused := true
	gated := &gateShell{inner: inner, gate: func(c pg.Cmd) (pg.Result, error, bool) {
		if refused && strings.Contains(strings.Join(c.Args, " "), "select 1") {
			refused = false
			return pg.Result{}, &pg.ExitError{Cmd: "psql", Code: 2, Stderr: []byte(`psql: error: connection to server at "127.0.0.1", port 5433 failed: FATAL:  no pg_hba.conf entry for host "127.0.0.1", user "bracer_backup", database "postgres", no encryption`)}, true
		}
		return pg.Result{}, nil, false
	}}
	answers := []string{"shop", "1", "", "", "pw", "r", ""}
	answers = append(answers, s3Answers...)
	answers = append(answers, "n")
	code, out, errOut := runWorld(t, dir, &world{sh: gated}, nil, answers...)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"pg_hba.conf has no line for 127.0.0.1", "host all bracer_backup 127.0.0.1/32 scram-sha-256", "sudo -u postgres psql -p 5433 -Atc 'show hba_file'"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}
