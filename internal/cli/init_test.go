package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/crypto"
	"bracer.dev/bracer/internal/pg"
	"bracer.dev/bracer/internal/storage"
)

// fakeShell fakes Docker and psql: matches by command prefix (after dropping
// the -e flags); the longest matching prefix wins.
type fakeShell struct {
	out  map[string]string
	fail map[string]string
	env  [][]string
}

func longestPrefix(m map[string]string, key string) (string, bool) {
	best, found := "", false
	for p := range m {
		if strings.HasPrefix(key, p) && (!found || len(p) > len(best)) {
			best, found = p, true
		}
	}
	return best, found
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
	if p, ok := longestPrefix(f.fail, key); ok {
		return pg.Result{}, &pg.ExitError{Cmd: p, Code: 1, Stderr: []byte(f.fail[p])}
	}
	if p, ok := longestPrefix(f.out, key); ok {
		o := f.out[p]
		if strings.Contains(key, " psql ") && hasArg(c.Args, "-0") && !strings.Contains(o, "\x00") {
			o = strings.ReplaceAll(o, "\n", "\x00")
		}
		if c.Stdout != nil {
			io.WriteString(c.Stdout, o)
			return pg.Result{}, nil
		}
		return pg.Result{Stdout: []byte(o)}, nil
	}
	return pg.Result{}, fmt.Errorf("fake: unexpected command %q", key)
}

// fakeStore is a bucket with nothing in it.
type fakeStore struct{ err error }

func (s fakeStore) Check(context.Context) error                         { return s.err }
func (s fakeStore) Put(context.Context, string, io.Reader, int64) error { return nil }
func (s fakeStore) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, storage.ErrNotFound
}
func (s fakeStore) List(context.Context, string) ([]storage.Object, error) { return nil, nil }
func (s fakeStore) Delete(context.Context, string) error                   { return nil }
func (s fakeStore) Describe() string                                       { return "fake" }

const psql = "docker exec -i shop-db psql -X -A -t -q -v ON_ERROR_STOP=1 -F \x1f -0 -c "

func containerWorld() *fakeShell {
	return &fakeShell{out: map[string]string{
		"docker version": "29.0\n",
		"docker info":    "29.0\n",
		"docker info --format {{.DockerRootDir}}":                    "/var/lib/docker\n",
		"df -Pk /var/lib/docker":                                     "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 20971520 10485760 10485760 50% /\n",
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
		psql + "select 'function '":                    "",
		psql + "select coalesce(sum(pg_database_size":  "1048576\n",
		"docker exec -i shop-db pg_dump --schema-only": "",
	}, fail: map[string]string{
		psql + "show pool_mode": `ERROR:  unrecognized configuration parameter "pool_mode"`,
	}}
}

// world is what init sees around it besides the shell.
type world struct {
	// args replaces the default arguments, "shop".
	args []string
	sh   pg.Shell
	// store: the first bucket check fails with it, later ones pass.
	store error
	// mem is the bucket; nil: an empty bucket that keeps nothing.
	mem       *storage.Memory
	noSystemd bool
	root      bool // euid 0
	noTTY     bool
	selfRun   func(args []string) int // nil: every self-check passes
	// preflight is the JSON `backup --dry-run --json` prints; nil: ok, or
	// one failed line when selfRun fails it.
	preflight  func(n int) *preflightResult
	selfLog    [][]string               // args then env of each self-check
	tty        [][]string               // every command run on the terminal
	ttyErr     func(cmd []string) error // nil: every terminal command succeeds
	dir        string                   // home, set by runWorld
	units      []string                 // app.yaml paths whose timer was installed
	installErr error
	// heartbeat answers GET <url>/log; nil: every URL answers.
	heartbeat func(url string) error
	hbLog     []string // URLs checked
	// pair is the hosted server of init --web; server is the URL init used.
	pair   pairAPI
	server string
}

func runWorld(t *testing.T, dir string, w *world, answers ...string) (int, string, string) {
	t.Helper()
	w.dir = dir
	pg.KubernetesDirs = []string{filepath.Join(dir, "no-k3s")}
	var out, errb bytes.Buffer
	in := strings.NewReader(strings.Join(answers, "\n") + "\n")
	euid := 1000
	if w.root {
		euid = 0
	}
	d := initDeps{
		shell: w.sh,
		probe: func(context.Context, string, int) (bool, error) { return true, nil },
		newStore: func(storage.S3Config) (storage.Store, error) {
			err := w.store
			w.store = nil
			switch {
			case err != nil:
				return fakeStore{err: err}, nil
			case w.mem != nil:
				return w.mem, nil
			}
			return fakeStore{}, nil
		},
		env: envDeps{
			lookPath:   func(name string) (string, error) { return "/usr/bin/" + name, nil },
			getenv:     func(string) string { return "" },
			userName:   func() string { return "khanh" },
			hasSystemd: func() bool { return !w.noSystemd },
		},
		home: func() (string, error) { return dir, nil },
		runSelf: func(_ context.Context, args, env []string, stdout, _ io.Writer) int {
			w.selfLog = append(w.selfLog, args, env)
			code := 0
			if w.selfRun != nil {
				code = w.selfRun(args)
			}
			if hasArg(args, "--json") {
				pf := &preflightResult{Kind: "backup-dry-run", App: "shop", OK: code == 0}
				if w.preflight != nil {
					pf = w.preflight(len(w.selfLog) / 2)
				} else if code != 0 {
					pf.Lines = []preflightLine{{Section: "instance shop-db", Instance: "shop-db", Area: "connect", Mark: "✗", Text: "connect: boom"}}
				}
				if !pf.OK && code == 0 {
					code = 1
				}
				json.NewEncoder(stdout).Encode(pf)
			}
			return code
		},
		runTTY: func(_ context.Context, name string, args ...string) error {
			cmd := append([]string{name}, args...)
			w.tty = append(w.tty, cmd)
			if w.ttyErr != nil {
				return w.ttyErr(cmd)
			}
			return nil
		},
		installTimer: func(_ context.Context, cfg *config.App, cfgPath string) error {
			w.units = append(w.units, cfgPath)
			if w.installErr != nil {
				return w.installErr
			}
			return nil
		},
		checkHeartbeat: func(_ context.Context, url string) error {
			w.hbLog = append(w.hbLog, url)
			if w.heartbeat != nil {
				return w.heartbeat(url)
			}
			return nil
		},
		newPair: func(server string, _ io.Writer) pairAPI {
			w.server = server
			return w.pair
		},
		hostname: func() (string, error) { return "vps-1", nil },
		euid:     euid,
		tty:      !w.noTTY,
		stdin:    in, stdout: &out, stderr: &errb,
	}
	args := []string{"shop"}
	if w.args != nil {
		args = w.args
	}
	code := runInit(context.Background(), args, d)
	return code, out.String(), errb.String()
}

// Answers in the order init asks (spec init § Flow, ADR-0022).
var (
	// s3Answers: endpoint, bucket, region, path style, access key, secret key (no prefix, ADR-0027).
	s3Answers = []string{"https://s3.example.com", "backups", "", "", "AK", "SK"}
	// noEncryption answers the age question.
	noEncryption = []string{"n"}
	// pickAll: the first Postgres listed, its default name, every database.
	pickAll = []string{"1", "", ""}
	// drillOn: test-restore here, no memory limit.
	drillOn = []string{"", ""}
	// drillOff: no test-restore here.
	drillOff = []string{"n"}
	// sched: the rhythm, the time and the keep as offered (ADR-0029; no
	// drill time, the drill is inside backup).
	sched = []string{"", "", ""}
	// noHeartbeat: no heartbeat URL.
	noHeartbeat = []string{""}
	// schedDefault: the schedule as offered, then no heartbeat URL.
	schedDefault = answers(sched, noHeartbeat)
)

// answers joins answer lists.
func answers(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// installed: init ended by installing the timer of shop, once,
// and sudo was asked once before any question (ADR-0027).
func (w *world) installed() bool {
	return len(w.tty) == 1 && strings.Join(w.tty[0], " ") == "sudo true" && len(w.units) == 1 && w.units[0] == appYAML(w.dir)
}

func TestInitContainerFullFlow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	w := &world{sh: containerWorld()}
	code, out, errOut := runWorld(t, dir, w, answers(s3Answers,
		[]string{"", ""}, // encrypt: yes; generate a key
		pickAll, drillOn, schedDefault,
		[]string{"later", "copied"}, // not "copied": asked again
	)...)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	cfg, err := config.Load(appYAML(dir))
	if err != nil {
		t.Fatal(err)
	}
	appDir := filepath.Dir(appYAML(dir))
	in := cfg.Instances[0]
	if len(cfg.Instances) != 1 || in.Name != "shop-db" || in.Container != "shop-db" || in.PgMajor != 17 || in.SystemID != "7698217040907026472" || strings.Join(in.Databases.Names(), ",") != "postgres,shop" || in.PasswordEnv != "" || in.Drill != nil {
		t.Errorf("instance: %+v", in)
	}
	// ADR-0023: a new folder <app>-<6 random> in the prefix.
	s := cfg.Storage
	if s == nil || s.Endpoint != "https://s3.example.com" || s.Bucket != "backups" || s.Region != "auto" || s.PathStyle || !strings.HasPrefix(s.Folder, "shop-") || !config.ValidFolder(s.Folder) || s.AccessKeyEnv != "BRACER_SHOP_S3_ACCESS_KEY" {
		t.Errorf("storage: %+v", s)
	}
	if !strings.Contains(out, "✓ bucket reachable; this app's backups go to backups/bracer/"+s.Folder+"/") {
		t.Errorf("bucket line:\n%s", out)
	}
	if cfg.Drill != nil {
		t.Errorf("drill: %+v", cfg.Drill)
	}
	// Absolute paths: timers have no shell to expand `~`.
	key := filepath.Join(appDir, "age.key")
	envPath := filepath.Join(appDir, "secrets.env")
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
	if cfg.Schedule == nil || !reflect.DeepEqual(*cfg.Schedule, config.DefaultSchedule) {
		t.Errorf("schedule: %+v", cfg.Schedule)
	}
	raw, _ := os.ReadFile(appYAML(dir))
	for _, secret := range []string{"AK", "SK", "AGE-SECRET-KEY"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("secret %s leaked into app.yaml:\n%s", secret, raw)
		}
	}
	env, _ := os.ReadFile(envPath)
	if string(env) != "BRACER_SHOP_S3_ACCESS_KEY=AK\nBRACER_SHOP_S3_SECRET_KEY=SK\n" {
		t.Errorf("env file:\n%s", env)
	}
	for path, mode := range map[string]os.FileMode{key: 0o600, envPath: 0o600, appYAML(dir): 0o644, appDir: 0o700} {
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != mode {
			t.Errorf("%s: %v, want mode %o", path, err, mode)
		}
	}
	// The private key is never printed (ADR-0022).
	if strings.Contains(out+errOut, "AGE-SECRET-KEY") || !strings.Contains(out, "Private key written to "+key+" (mode 600); it is never printed.") {
		t.Errorf("key:\n%s", out)
	}
	if !strings.Contains(out, "published on localhost:15432") || strings.Contains(out, "15432 (on this host") {
		t.Errorf("mapped port must merge into the container:\n%s", out)
	}
	// Step 0 first, and sudo asked while nothing is written; the timers last.
	if !strings.HasPrefix(out, "Checking this machine:\n  ✓ docker: docker info answers as khanh") || !strings.Contains(out, "✓ systemd") {
		t.Errorf("environment table first:\n%s", out)
	}
	if !w.installed() {
		t.Errorf("terminal commands: %q", w.tty)
	}
	// The preflight, then a real backup (its drill inside, ADR-0026), on the
	// draft as the timer runs them: only HOME and runtime.path (ADR-0019, ADR-0022).
	wantEnv := "HOME=" + dir + " PATH=" + rt.Path
	if len(w.selfLog) != 4 || strings.Join(w.selfLog[0], " ") != "backup shop --init-draft --dry-run --json" || strings.Join(w.selfLog[2], " ") != "backup shop --init-draft" {
		t.Errorf("self-check: %q", w.selfLog)
	}
	for i := 1; i < len(w.selfLog); i += 2 {
		if strings.Join(w.selfLog[i], " ") != wantEnv {
			t.Errorf("env of run %d: %q", i/2, w.selfLog[i])
		}
	}
	entries, _ := os.ReadDir(appDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("the draft %s must be renamed", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(appDir, "app.schedule.yaml")); !os.IsNotExist(err) {
		t.Error("the schedule is a block of app.yaml, not a file (ADR-0026)")
	}
	for _, want := range []string{
		"A real backup now proves the data too:",
		"the databases hold 1.0 MB",
		"drill restores them into a temporary Postgres in Docker: 10.0 GB free, about 5.0 MB needed",
		"Wrote " + appYAML(dir) + ".",
		"Installing the nightly timer (sudo)",
		"If this machine is lost, restore needs:\n  endpoint  https://s3.example.com\n  bucket    backups\n  folder    bracer/" + cfg.Storage.Folder + "\n  age key   " + key + "\n",
		"Results of the nightly jobs:\n  bracer status shop\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Count(out, "Type copied when done") != 2 {
		t.Errorf("a new key must be confirmed copied:\n%s", out)
	}

	// init creates an app; an existing one is removed and created again (ADR-0030).
	w = &world{sh: containerWorld()}
	code, out, errOut = runWorld(t, dir, w)
	if code != exitUsage || out != "" || !strings.Contains(errOut, "bracer init: app shop exists on this machine\nFix: bracer remove shop, then bracer init shop") || w.tty != nil {
		t.Errorf("init again: %d\n%s\n%s", code, out, errOut)
	}
	// --web refuses the same way, before any network (spec init-web).
	fp := newFakePair()
	w = &world{sh: containerWorld(), args: webArgs, pair: fp}
	code, out, errOut = runWorld(t, dir, w)
	if code != exitUsage || out != "" || !strings.Contains(errOut, "app shop exists on this machine") || fp.opened != 0 {
		t.Errorf("init --web again: %d opened %d\n%s\n%s", code, fp.opened, out, errOut)
	}
}

// Encryption needs someone to confirm the key's copy: without a terminal
// init stops before any key is written.
func TestInitNewKeyNeedsTerminal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	code, _, errOut := runWorld(t, dir, &world{sh: containerWorld(), noTTY: true}, answers(s3Answers, []string{""})...)
	if code != exitUsage || !strings.Contains(errOut, "init must confirm the age key was copied; run it in a terminal") {
		t.Errorf("exit %d: %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(appYAML(dir)), "age.key")); !os.IsNotExist(err) {
		t.Error("no key may be written")
	}
}

// The private key is always age.key in the app directory (ADR-0021): a
// pasted public key needs its private key there, and a key already there
// is used; either way the copy off the machine is confirmed.
func TestInitExistingKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := filepath.Join(filepath.Dir(appYAML(dir)), "age.key")
	kp, _ := crypto.Generate()
	other, _ := crypto.Generate()
	code, out, _ := runWorld(t, dir, &world{sh: containerWorld()}, answers(s3Answers, []string{"", other.Recipient, "q"})...)
	if code != exitError || !strings.Contains(out, "✗ "+key+" is not readable") || !strings.Contains(out, "No app.yaml written. Run again when ready:\n  bracer init shop\n") {
		t.Errorf("pasted key without its file: %d\n%s", code, out)
	}
	os.MkdirAll(filepath.Dir(key), 0o700)
	if err := crypto.WriteIdentityFile(key, kp); err != nil {
		t.Fatal(err)
	}
	code, out, _ = runWorld(t, dir, &world{sh: containerWorld()}, answers(s3Answers, []string{"", other.Recipient, "q"})...)
	if code != exitError || !strings.Contains(out, "✗ "+key+" does not hold the private key for "+other.Recipient) {
		t.Errorf("pasted key not in the file: %d\n%s", code, out)
	}
	// Nobody has confirmed this key is off the machine either: copied is asked.
	code, out, errOut := runWorld(t, dir, &world{sh: containerWorld()}, answers(s3Answers, []string{"", kp.Recipient}, pickAll, drillOff, schedDefault, []string{"copied"})...)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	cfg, _ := config.Load(appYAML(dir))
	if cfg.AgeRecipient != kp.Recipient || cfg.AgeIdentityFile != key || strings.Count(out, "Type copied when done") != 1 || !strings.Contains(out, "  age key   "+key) {
		t.Errorf("recipient %s\n%s", cfg.AgeRecipient, out)
	}
	os.Remove(appYAML(dir))
	code, out, _ = runWorld(t, dir, &world{sh: containerWorld()}, answers(s3Answers, []string{"", ""}, pickAll, drillOff, schedDefault, []string{"copied"})...)
	if code != 0 || !strings.Contains(out, "Using the key already in "+key+" (public key "+kp.Recipient+").") || strings.Count(out, "Type copied when done") != 1 {
		t.Errorf("key already there: %d\n%s", code, out)
	}
}

// Only the databases picked are written; the others are not recorded
// anywhere (ADR-0016, ADR-0024).
func TestInitWritesOnlyPicked(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	code, out, errOut := runWorld(t, dir, &world{sh: containerWorld()}, answers(s3Answers, noEncryption, []string{"1", "", "2"}, drillOff, schedDefault)...)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	cfg, err := config.Load(appYAML(dir))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Instances[0].Databases.Names(), ","); got != "shop" {
		t.Errorf("databases %s", got)
	}
	raw, _ := os.ReadFile(appYAML(dir))
	if strings.Contains(string(raw), "ignore") || strings.Contains(out, "ignore") {
		t.Errorf("nothing about left-out databases:\n%s\n%s", raw, out)
	}
}

// authWorld is containerWorld with env for the container and a server that
// knows only the roles in super (name → superuser) with the password pw
// (empty: socket trust).
func authWorld(env string, roles map[string]bool, pw string) *gateShell {
	sh := containerWorld()
	sh.out["docker inspect"] = `[{"Id":"c1","Name":"/shop-db","State":{"Running":true,"Status":"running"},"Config":{"Image":"postgres:17-alpine","Env":[` + env + `]},"NetworkSettings":{"Ports":{"5432/tcp":[{"HostIp":"0.0.0.0","HostPort":"15432"}]},"Networks":{"bridge":{"IPAddress":"172.17.0.2"}}}}]`
	return &gateShell{inner: sh, gate: func(c pg.Cmd) (pg.Result, error, bool) {
		args := strings.Join(c.Args, " ")
		user := envOf(c.Env, "PGUSER")
		if c.Name != "docker" || user == "" || !strings.Contains(args, " psql ") && !strings.Contains(args, " pg_dump") {
			return pg.Result{}, nil, false
		}
		super, ok := roles[user]
		if !ok {
			return pg.Result{}, &pg.ExitError{Cmd: "psql", Code: 2, Stderr: []byte(`psql: error: connection to server on socket "/var/run/postgresql/.s.PGSQL.5432" failed: FATAL:  role "` + user + `" does not exist`)}, true
		}
		if pw != "" && envOf(c.Env, "PGPASSWORD") != pw {
			return pg.Result{}, &pg.ExitError{Cmd: "psql", Code: 2, Stderr: []byte(`psql: error: connection to server on socket "/tmp/.s.PGSQL.5432" failed: FATAL:  password authentication failed for user "` + user + `"`)}, true
		}
		out := func(s string) (pg.Result, error, bool) {
			s = strings.ReplaceAll(s, "\n", "\x00") // psql -0
			if c.Stdout != nil {
				io.WriteString(c.Stdout, s)
				return pg.Result{}, nil, true
			}
			return pg.Result{Stdout: []byte(s)}, nil, true
		}
		switch {
		case strings.Contains(args, "select current_user"):
			su := "f"
			if super {
				su = "t"
			}
			return out(user + "\x1f" + su + "\x1ft\x1ft\n")
		case strings.Contains(args, "where oid = 10"):
			for n, s := range roles {
				if s {
					return out(n + "\n")
				}
			}
		}
		return pg.Result{}, nil, false
	}}
}

func envOf(env []string, key string) string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, key+"="); ok {
			return v
		}
	}
	return ""
}

// initOnce runs init on a fresh directory: the bucket, no encryption,
// container 1 with its default name, then pre (the password questions),
// every database, no drill, the default schedule.
func initOnce(t *testing.T, sh pg.Shell, pre ...string) (*config.App, string, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	code, out, errOut := runWorld(t, dir, &world{sh: sh}, answers(s3Answers, noEncryption, []string{"1", ""}, pre, []string{""}, drillOff, schedDefault)...)
	cfg, _ := config.Load(appYAML(dir))
	secrets, _ := os.ReadFile(filepath.Join(dir, ".config", "bracer", "shop", "secrets.env"))
	if code != 0 && cfg != nil {
		t.Logf("exit %d", code)
	}
	return cfg, out + errOut, string(secrets)
}

// ADR-0017: Bitnami's superuser is postgres, never POSTGRESQL_USERNAME, and
// its password is taken from POSTGRESQL_POSTGRES_PASSWORD without a question.
func TestInitBitnamiSuperuserFromEnv(t *testing.T) {
	env := `"POSTGRESQL_USERNAME=app","POSTGRESQL_PASSWORD=apppw","POSTGRESQL_DATABASE=shop","POSTGRESQL_POSTGRES_PASSWORD=su"`
	cfg, out, secrets := initOnce(t, authWorld(env, map[string]bool{"postgres": true, "app": false}, "su"))
	if cfg == nil || len(cfg.Instances) != 1 {
		t.Fatalf("no instance:\n%s", out)
	}
	in := cfg.Instances[0]
	if in.User != "postgres" || in.PasswordEnv != "BRACER_SHOP_SHOP_DB_PASSWORD" || !strings.Contains(secrets, "BRACER_SHOP_SHOP_DB_PASSWORD=su") {
		t.Errorf("instance %+v secrets %q", in, secrets)
	}
	if strings.Contains(out, "Password:") || strings.Contains(out, "User:") {
		t.Errorf("nothing may be asked:\n%s", out)
	}
}

// The variable no longer matches the password in use: say so and ask.
func TestInitBitnamiEnvPasswordStale(t *testing.T) {
	env := `"POSTGRESQL_USERNAME=app","POSTGRESQL_POSTGRES_PASSWORD=old"`
	cfg, out, secrets := initOnce(t, authWorld(env, map[string]bool{"postgres": true, "app": false}, "su"), "su")
	if !strings.Contains(out, "POSTGRESQL_POSTGRES_PASSWORD of shop-db is not the password postgres uses now") {
		t.Errorf("must say the variable is stale:\n%s", out)
	}
	if cfg == nil || len(cfg.Instances) != 1 || !strings.Contains(secrets, "BRACER_SHOP_SHOP_DB_PASSWORD=su") || strings.Contains(secrets, "=old") {
		t.Errorf("cfg %+v secrets %q\n%s", cfg, secrets, out)
	}
}

// No password variable: ask; Enter skips the instance and keeps no secret.
func TestInitBitnamiNoPasswordSkips(t *testing.T) {
	env := `"POSTGRESQL_USERNAME=app","POSTGRESQL_PASSWORD=apppw"`
	cfg, out, secrets := initOnce(t, authWorld(env, map[string]bool{"postgres": true, "app": false}, "su"), "")
	if !strings.Contains(out, "shop-db sets no password for its superuser postgres; enter it if you set one yourself (Enter to skip this instance)") {
		t.Errorf("output:\n%s", out)
	}
	if secrets != "" || cfg != nil {
		t.Errorf("a skipped instance writes nothing: %+v %q", cfg, secrets)
	}
	if !strings.Contains(out, "no instance to back up; nothing was written. When it is fixed, run: bracer init shop") {
		t.Errorf("init must stop when no instance is left:\n%s", out)
	}
}

// POSTGRES_USER changed after the volume was created: try postgres, never ask.
func TestInitContainerUserGoneTriesPostgres(t *testing.T) {
	env := `"POSTGRES_USER=renamed","POSTGRES_PASSWORD=pw"`
	cfg, out, _ := initOnce(t, authWorld(env, map[string]bool{"postgres": true}, ""))
	if cfg == nil || len(cfg.Instances) != 1 || cfg.Instances[0].User != "postgres" || !strings.Contains(out, "User renamed does not exist on this server; trying postgres.") {
		t.Fatalf("cfg %+v\n%s", cfg, out)
	}
	_, out, _ = initOnce(t, authWorld(env, map[string]bool{"original": true}, ""))
	if !strings.Contains(out, "✗ shop-db: cannot find its superuser: POSTGRES_USER=renamed does not exist on this server") || strings.Contains(out, "\nUser [") {
		t.Errorf("no superuser to find:\n%s", out)
	}
}

// Connected as a user that is not a superuser: switch to the bootstrap
// superuser (oid 10); none reachable → not supported, no bracer_backup SQL.
func TestInitContainerNotSuperuser(t *testing.T) {
	env := `"POSTGRES_USER=app"`
	cfg, out, _ := initOnce(t, authWorld(env, map[string]bool{"app": false, "postgres": true}, ""))
	if cfg == nil || len(cfg.Instances) != 1 || cfg.Instances[0].User != "postgres" || !strings.Contains(out, "app is not a superuser; using the superuser postgres.") {
		t.Fatalf("cfg %+v\n%s", cfg, out)
	}
	_, out, _ = initOnce(t, authWorld(env, map[string]bool{"app": false}, ""))
	if !strings.Contains(out, "not supported: a full backup (users, settings) needs the superuser, and app is not one") || strings.Contains(out, "bracer_backup") {
		t.Errorf("output:\n%s", out)
	}
}

// localWorld: no container; Postgres on this machine at 127.0.0.1:5433 that wants a password.
func localWorld(password string) (*gateShell, *fakeShell) {
	sh := containerWorld()
	sh.out["docker ps"] = ""
	sh.out["ss -ltnpH"] = "LISTEN 0 244 127.0.0.1:5433 0.0.0.0:* users:((\"postgres\",pid=9,fd=7))\n"
	hostPsql := "docker run --rm -i --network host postgres:18-alpine psql -X -A -t -q -v ON_ERROR_STOP=1 -F \x1f -0 -c "
	hostPsql17 := "docker run --rm -i --network host postgres:17-alpine psql -X -A -t -q -v ON_ERROR_STOP=1 -F \x1f -0 -c "
	sh.out[hostPsql+"show server_version_num"] = "170011\n"
	sh.out[hostPsql17+"select version()"] = "PostgreSQL 17.11\n"
	sh.out[hostPsql17+"select datname"] = "app\n"
	sh.out[hostPsql17+"select current_user"] = "bracer_backup\x1ff\x1ft\x1ft\n"
	sh.out[hostPsql17+"select n.nspname"] = ""
	sh.out[hostPsql17+"select 'function '"] = ""
	sh.out[hostPsql17+"select coalesce(sum(pg_database_size"] = "1048576\n"
	sh.out[hostPsql17+"select extname"] = "uuid-ossp\x1f1.1\n"
	sh.out["docker run --rm -i --network host postgres:17-alpine pg_dump --schema-only"] = ""
	sh.out["docker pull --quiet postgres:17-alpine"] = ""
	sh.fail[hostPsql17+"show pool_mode"] = "ERROR"
	sh.fail["docker image inspect --format {{.Id}} postgres:17-alpine"] = "No such image"
	sh.fail[hostPsql17+"select system_identifier"] = "ERROR:  permission denied for function pg_control_system"
	gated := &gateShell{inner: sh, gate: func(c pg.Cmd) (pg.Result, error, bool) {
		if len(c.Args) > 0 && c.Args[len(c.Args)-1] == "select 1" {
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
// A server that hides system_identifier is written system_id: none (ADR-0022).
func TestInitLocalPostgresBuildsURL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	gated, sh := localWorld("p@ss:w/rd")
	code, out, errOut := runWorld(t, dir, &world{sh: gated, store: errors.New("boom")}, answers(
		s3Answers, []string{"r"}, s3Answers, // bucket check failed → retry
		noEncryption,
		[]string{
			"1",     // localhost:5433
			"",      // name 127-0-0-1-5433
			"",      // user bracer_backup
			"wrong", // password: rejected
			"",      // user again
			"p@ss:w/rd",
			"", // all databases
		}, drillOff, schedDefault)...)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	cfg, err := config.Load(appYAML(dir))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage == nil || cfg.Storage.Bucket != "backups" || !strings.Contains(out, "  ✗ boom\n") {
		t.Errorf("storage after retry: %+v\n%s", cfg.Storage, out)
	}
	in := cfg.Instances[0]
	if in.URLEnv != "BRACER_SHOP_"+strings.ToUpper(strings.ReplaceAll(in.Name, "-", "_"))+"_URL" || in.User != "" || in.PasswordEnv != "" || in.PgMajor != 17 || in.SystemID != config.SystemIDNone {
		t.Errorf("instance: %+v", in)
	}
	env, _ := os.ReadFile(filepath.Join(dir, ".config", "bracer", "shop", "secrets.env"))
	if !strings.Contains(string(env), in.URLEnv+"=postgres://bracer_backup:p%40ss%3Aw%2Frd@127.0.0.1:5433/postgres\n") {
		t.Errorf("env file:\n%s", env)
	}
	for _, want := range []string{"sudo -u postgres psql -p 5433\n", "GRANT pg_read_all_data TO bracer_backup", "Pulling postgres:17-alpine",
		"URL source: bracer backs up the databases only", "system_identifier is not readable here"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	raw, _ := os.ReadFile(appYAML(dir))
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

// ADR-0022: an address typed by hand is always a URL source, even one on
// this machine.
func TestInitTypedAddressIsURL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	gated, _ := localWorld("pw")
	code, out, errOut := runWorld(t, dir, &world{sh: gated}, answers(s3Answers, noEncryption,
		[]string{"m", "127.0.0.1:5433", "", "", "pw", ""}, drillOff, schedDefault)...)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	cfg, _ := config.Load(appYAML(dir))
	in := cfg.Instances[0]
	if in.Name != "127-0-0-1-5433" || in.Local != nil || in.URLEnv != "BRACER_SHOP_127_0_0_1_5433_URL" {
		t.Errorf("instance: %+v", in)
	}
}

// spec init AC step 0: nothing is asked or written when the machine cannot
// run the jobs; the fixes come in order and the last line is the command typed.
func TestInitEnvironmentBlocks(t *testing.T) {
	dir := t.TempDir()
	sh := &fakeShell{out: map[string]string{
		"docker context inspect": "unix:///var/run/docker.sock\n",
	}, fail: map[string]string{
		"docker info": "permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock",
	}}
	w := &world{sh: sh}
	code, out, _ := runWorld(t, dir, w)
	if code != exitError || !strings.Contains(out, "✗ docker: khanh may not use Docker") || !strings.Contains(out, "1. sudo usermod -aG docker khanh   # the docker group is equivalent to root") ||
		!strings.Contains(out, "2. log out and back in") || !strings.Contains(out, "3. bracer init shop\n") || !strings.Contains(out, "Nothing written.") || strings.Contains(out, "S3 endpoint") || w.tty != nil {
		t.Errorf("exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(config.AppsDir(dir)); !os.IsNotExist(err) {
		t.Error("nothing must be written")
	}

	// Rootless Docker without lingering: enable-linger, no logout needed.
	sh = &fakeShell{out: map[string]string{
		"docker info":                 "29.0\n",
		"docker context inspect":      "unix:///run/user/1000/docker.sock\n",
		"loginctl show-user khanh -p": "Linger=no\n",
	}}
	code, out, _ = runWorld(t, dir, &world{sh: sh})
	if code != exitError || !strings.Contains(out, "✗ rootless docker") || !strings.Contains(out, "1. sudo loginctl enable-linger khanh\n  2. bracer init shop") || !strings.Contains(out, "✓ systemd") {
		t.Errorf("rootless: exit %d:\n%s", code, out)
	}

	// No systemd: the jobs have nothing to run them.
	code, out, _ = runWorld(t, dir, &world{sh: containerWorld(), noSystemd: true})
	if code != exitError || !strings.Contains(out, "✗ systemd: not found; bracer runs its nightly jobs with systemd timers") || !strings.Contains(out, "1. run bracer on a Linux machine where systemd is PID 1\n  2. bracer init shop") {
		t.Errorf("no systemd: exit %d:\n%s", code, out)
	}

	// sudo is asked before any question; refused, nothing is written.
	w = &world{sh: containerWorld(), ttyErr: func([]string) error { return errors.New("exit status 1") }}
	code, out, _ = runWorld(t, dir, w, s3Answers...)
	if code != exitError || !strings.Contains(out, "✗ sudo: init installs the nightly timer with sudo, and sudo failed: exit status 1") || !strings.Contains(out, "Nothing written.") || strings.Contains(out, "S3 endpoint") {
		t.Errorf("sudo: exit %d:\n%s", code, out)
	}

	// As root: no sudo.
	w = &world{sh: containerWorld(), root: true}
	runWorld(t, dir, w)
	if len(w.tty) != 0 {
		t.Errorf("root runs no sudo: %q", w.tty)
	}
}

// Step 0 reads the machine only: the table, and runtime: when nothing blocks.
func TestCheckEnv(t *testing.T) {
	sh := &fakeShell{out: map[string]string{
		"docker info":                 "29.0\n",
		"docker context inspect":      "unix:///run/user/1000/docker.sock\n",
		"loginctl show-user khanh -p": "Linger=yes\n",
	}}
	ed := envDeps{shell: sh, lookPath: func(n string) (string, error) { return "/opt/docker/bin/" + n, nil }, getenv: func(string) string { return "" },
		userName: func() string { return "khanh" }, hasSystemd: func() bool { return true }}
	rep := checkEnv(context.Background(), ed)
	var buf bytes.Buffer
	printEnv(&buf, rep, "bracer init shop")
	if rep.Blocked() || buf.String() != "  ✓ docker: docker info answers as khanh (/opt/docker/bin/docker)\n  ✓ rootless docker: lingering is on for khanh\n  ✓ systemd: the nightly jobs run as systemd timers\n" {
		t.Errorf("ok:\n%s", buf.String())
	}
	if rt := rep.Runtime; rt == nil || *rt != (config.Runtime{User: "khanh", Path: "/opt/docker/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", Docker: "/opt/docker/bin/docker", DockerHost: "unix:///run/user/1000/docker.sock"}) {
		t.Errorf("runtime: %+v", rep.Runtime)
	}
	// DOCKER_HOST wins over the context.
	ed.getenv = func(k string) string {
		if k == "DOCKER_HOST" {
			return "tcp://10.0.0.5:2375"
		}
		return ""
	}
	if rep := checkEnv(context.Background(), ed); rep.Runtime == nil || rep.Runtime.DockerHost != "tcp://10.0.0.5:2375" {
		t.Errorf("DOCKER_HOST: %+v", rep.Runtime)
	}
	ed.getenv = func(string) string { return "" }
	ed.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	ed.hasSystemd = func() bool { return false }
	rep = checkEnv(context.Background(), ed)
	buf.Reset()
	printEnv(&buf, rep, "bracer init shop")
	if !rep.Blocked() || rep.Runtime != nil || !strings.Contains(buf.String(), "  1. install Docker Engine: https://docs.docker.com/engine/install/\n  2. run bracer on a Linux machine where systemd is PID 1\n  3. bracer init shop\n") {
		t.Errorf("blocked:\n%s", buf.String())
	}
	if got := commandLine("init", []string{"shop", "--verbose", "a b", ""}); got != "bracer init shop --verbose 'a b' ''" {
		t.Errorf("commandLine: %s", got)
	}
}

// drill off on this machine → drill.runtime: none on the instance, no
// memory question; the proof is the same two runs, without the disk check.
func TestInitNoDrill(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	w := &world{sh: containerWorld()}
	code, out, errOut := runWorld(t, dir, w, answers(s3Answers, noEncryption, pickAll, drillOff, schedDefault)...)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	cfg, err := config.Load(appYAML(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DrillOff() || !cfg.Instances[0].DrillOff() || cfg.Drill != nil || !strings.Contains(out, "Backups on this machine will not be test-restored; every report will say so.") {
		t.Errorf("drill: %+v\n%s", cfg.Instances[0].Drill, out)
	}
	if strings.Contains(out, "Limit the drill's memory") || strings.Contains(out, "When should drill run?") || strings.Contains(out, "free, about") {
		t.Errorf("nothing about drill may be asked:\n%s", out)
	}
	if cfg.Schedule == nil || !reflect.DeepEqual(*cfg.Schedule, config.DefaultSchedule) {
		t.Errorf("schedule: %+v", cfg.Schedule)
	}
	if len(w.selfLog) != 4 || strings.Join(w.selfLog[0], " ") != "backup shop --init-draft --dry-run --json" || strings.Join(w.selfLog[2], " ") != "backup shop --init-draft" {
		t.Errorf("self-check: %q", w.selfLog)
	}
	if !w.installed() {
		t.Errorf("terminal %q units %q", w.tty, w.units)
	}
}

// The drill's memory limit and the schedule are checked as they are typed.
func TestInitDrillMemoryAndSchedule(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	code, out, errOut := runWorld(t, dir, &world{sh: containerWorld()}, answers(s3Answers, noEncryption, pickAll,
		[]string{"", "lots", "2g"},
		[]string{"w", "25:00, 4:00", "04:00, 22:00", "x", "mon, Fri", "0", "6"}, noHeartbeat)...)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	cfg, _ := config.Load(appYAML(dir))
	if cfg.DrillMemory() != "2g" || cfg.Schedule == nil || !reflect.DeepEqual(*cfg.Schedule, config.Schedule{Every: "weekly", At: []string{"04:00", "22:00"}, Days: []string{"mon", "fri"}, Keep: 6}) {
		t.Errorf("memory %q schedule %+v\n%s", cfg.DrillMemory(), cfg.Schedule, out)
	}
	for _, want := range []string{`"lots" is not a docker --memory value`, "[d] daily  [w] weekly  [m] monthly  [Enter] daily", "Time of day (HH:MM, several separated by commas) [03:00]", "4:00 is not HH:MM",
		"Days of the week (mon…sun, several separated by commas) [mon]", "x is not a day of the week", "Keep every backup of the last N weeks [4]", "a whole number ≥ 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "When should drill run?") {
		t.Errorf("no drill time: the drill is inside backup (ADR-0026):\n%s", out)
	}
}

// A proof that fails leaves nothing behind: no app.yaml, no drafts, no
// timers; the command to run again is printed (ADR-0019).
func TestInitSelfCheckFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(args []string) bool
		want string
	}{
		{"preflight", func(args []string) bool { return hasArg(args, "--dry-run") }, "the check failed (exit 1). No app.yaml written. Fix the above, then run: bracer init shop"},
		{"real run", func(args []string) bool { return !hasArg(args, "--dry-run") }, "the real run failed (exit 1). No app.yaml written. Fix the above, then run: bracer init shop"},
	} {
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		w := &world{sh: containerWorld(), selfRun: func(args []string) int {
			if tc.fail(args) {
				return 1
			}
			return 0
		}}
		code, out, _ := runWorld(t, dir, w, answers(s3Answers, noEncryption, pickAll, drillOn, schedDefault)...)
		if code != exitError || !strings.Contains(out, tc.want) || strings.Contains(out, "Installing the nightly timer") || len(w.tty) != 1 || len(w.units) != 0 {
			t.Errorf("%s: exit %d %q:\n%s", tc.name, code, w.tty, out)
		}
		for _, f := range []string{"app.yaml", ".app.yaml.init"} {
			if _, err := os.Stat(filepath.Join(filepath.Dir(appYAML(dir)), f)); !os.IsNotExist(err) {
				t.Errorf("%s: %s must not be left", tc.name, f)
			}
		}
	}
}

// Drill on needs DiskFactor × the databases free for Docker: too little, or
// a size that cannot be read, stops before the real run.
func TestInitDiskCheck(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	sh := containerWorld()
	sh.out["df -Pk /var/lib/docker"] = "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 2048 1024 1024 50% /\n"
	w := &world{sh: sh}
	code, out, _ := runWorld(t, dir, w, answers(s3Answers, noEncryption, pickAll, drillOn, schedDefault)...)
	if code != exitError || !strings.Contains(out, "✗ free up 4.0 MB for the drill (1.0 MB free, about 5.0 MB needed: 5 × the databases), or answer n to test-restore on this machine") || len(w.selfLog) != 2 {
		t.Errorf("exit %d %q:\n%s", code, w.selfLog, out)
	}
	if _, err := os.Stat(appYAML(dir)); !os.IsNotExist(err) {
		t.Error("nothing may be written")
	}
	sh = containerWorld()
	delete(sh.out, "df -Pk /var/lib/docker")
	sh.fail["df -Pk"] = "df: no such file"
	code, out, _ = runWorld(t, dir, &world{sh: sh}, answers(s3Answers, noEncryption, pickAll, drillOn, schedDefault)...)
	if code != exitError || !strings.Contains(out, "✗ cannot check free disk space for the drill") {
		t.Errorf("df fails: %d\n%s", code, out)
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
	_, out, _ := runWorld(t, dir, &world{sh: sh}, answers(s3Answers, noEncryption)...)
	if !strings.Contains(out, "Kubernetes is not supported (seen: 1 Kubernetes container(s) seen through Docker)") || strings.Contains(out, "1) k8s_") || !strings.Contains(out, "No Postgres found.") {
		t.Errorf("out:\n%s", out)
	}
}

// spec init AC: the bucket is mandatory and comes first; a failing check
// offers retry or quit, never skip, before anything is discovered.
func TestInitBucketFailQuits(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	code, out, _ := runWorld(t, dir, &world{sh: containerWorld(), store: errors.New("403 Forbidden")}, answers(s3Answers, []string{"q"})...)
	if code != exitError || !strings.Contains(out, "  ✗ 403 Forbidden\n[r] fix and retry  [q] quit: ") || !strings.Contains(out, "No app.yaml written.") || strings.Contains(out, "skip") || strings.Contains(out, "Looking for Postgres") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(appYAML(dir)); !os.IsNotExist(err) {
		t.Error("nothing must be written without a bucket")
	}
}

func TestInitInterruptedWritesNothing(t *testing.T) {
	dir := t.TempDir()
	code, _, errOut := runWorld(t, dir, &world{sh: containerWorld()}, answers(s3Answers, noEncryption, []string{"1"})...) // input ends midway
	if code != exitUsage || !strings.Contains(errOut, "interactive terminal") {
		t.Errorf("exit %d, stderr: %s", code, errOut)
	}
	if _, err := os.Stat(appYAML(dir)); !os.IsNotExist(err) {
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
		if refused && len(c.Args) > 0 && c.Args[len(c.Args)-1] == "select 1" {
			refused = false
			return pg.Result{}, &pg.ExitError{Cmd: "psql", Code: 2, Stderr: []byte(`psql: error: connection to server at "127.0.0.1", port 5433 failed: FATAL:  no pg_hba.conf entry for host "127.0.0.1", user "bracer_backup", database "postgres", no encryption`)}, true
		}
		return pg.Result{}, nil, false
	}}
	code, out, errOut := runWorld(t, dir, &world{sh: gated}, answers(s3Answers, noEncryption, []string{"1", "", "", "pw", "r", ""}, drillOff, schedDefault)...)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"pg_hba.conf has no line for 127.0.0.1", "host all bracer_backup 127.0.0.1/32 scram-sha-256", "sudo -u postgres psql -p 5433 -Atc 'show hba_file'"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func hasArg(args []string, a string) bool {
	for _, x := range args {
		if x == a {
			return true
		}
	}
	return false
}

// ADR-0019: the preflight drill of an instance fails → another image ([i],
// checked again) or no drill for it ([n]); nothing is written before.
func TestInitDrillFailAsks(t *testing.T) {
	for _, tc := range []struct {
		answers []string
		check   func(in config.Instance) bool
		runs    string
	}{
		{[]string{"i", "bitnami/postgresql:17", "i", "postgis/postgis:17-3.5"}, func(in config.Instance) bool { return in.DrillImage() == "postgis/postgis:17-3.5" }, "backup,backup,backup"},
		{[]string{"n"}, func(in config.Instance) bool { return in.DrillOff() }, "backup,backup,backup"},
	} {
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		w := &world{sh: containerWorld(), preflight: func(n int) *preflightResult {
			pf := &preflightResult{Kind: "backup-dry-run", App: "shop", OK: true}
			if n == 1 {
				pf.OK = false
				pf.Lines = []preflightLine{{Section: "drill", Instance: "shop-db", Area: "drill", Mark: "✗", Text: "shop-db (image postgres:17-alpine): extension postgis is not in the image"}}
			}
			return pf
		}}
		code, out, errOut := runWorld(t, dir, w, answers(s3Answers, noEncryption, pickAll, drillOn, schedDefault, tc.answers)...)
		if code != 0 {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		cfg, err := config.Load(appYAML(dir))
		if err != nil || !tc.check(cfg.Instances[0]) || !strings.Contains(out, "[i] enter a Docker image for this instance's drill  [n] no drill for this instance  [q] quit") {
			t.Errorf("%v: %v %+v\n%s", tc.answers, err, cfg.Instances[0].Drill, out)
		}
		var runs []string
		for i := 0; i < len(w.selfLog); i += 2 {
			runs = append(runs, w.selfLog[i][0])
		}
		if strings.Join(runs, ",") != tc.runs {
			t.Errorf("%v: runs %v", tc.answers, runs)
		}
		if len(tc.answers) > 1 && !strings.Contains(out, "Bitnami images cannot be the drill image") {
			t.Errorf("bitnami must be refused:\n%s", out)
		}
	}
}

// ADR-0019 step 10: a pooler is never written.
func TestInitPoolerBlocks(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	sh := containerWorld()
	delete(sh.fail, psql+"show pool_mode")
	sh.out[psql+"show pool_mode"] = "transaction\n"
	code, out, errOut := runWorld(t, dir, &world{sh: sh}, answers(s3Answers, noEncryption, []string{"1", ""})...)
	if code != exitError || !strings.Contains(out, "✗ this looks like a connection pooler") || !strings.Contains(errOut, "no instance to back up") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(appYAML(dir)); !os.IsNotExist(err) {
		t.Error("nothing may be written")
	}
}

// ADR-0019 step 9: what a restore cannot guarantee is accepted per database
// and written as accepted_external; [s] leaves the database out.
func TestInitExternalAccept(t *testing.T) {
	for _, tc := range []struct{ answer, dbs string }{{"a", "postgres,shop"}, {"s", "postgres"}} {
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		sh := containerWorld()
		sh.out[psql+"select 'function '"] = "function f() (language plpython3u)\n"
		gated := &gateShell{inner: sh, gate: func(c pg.Cmd) (pg.Result, error, bool) {
			// only shop has the function
			if strings.Contains(strings.Join(c.Args, " "), "select 'function '") && envOf(c.Env, "PGDATABASE") != "shop" {
				return pg.Result{}, nil, true
			}
			return pg.Result{}, nil, false
		}}
		code, out, errOut := runWorld(t, dir, &world{sh: gated}, answers(s3Answers, noEncryption, pickAll, []string{tc.answer}, drillOff, schedDefault)...)
		if code != 0 {
			t.Fatalf("%s: exit %d\n%s\n%s", tc.answer, code, out, errOut)
		}
		cfg, _ := config.Load(appYAML(dir))
		in := cfg.Instances[0]
		if strings.Join(in.Databases.Names(), ",") != tc.dbs {
			t.Errorf("%s: databases %v\n%s", tc.answer, in.Databases.Names(), out)
		}
		if tc.answer == "a" && strings.Join(in.Databases[1].AcceptedExternal, ",") != "function f() (language plpython3u)" {
			t.Errorf("accepted: %+v", in.Databases)
		}
	}
}

// ADR-0021: bracer init <app> keeps every file of the app in
// ~/.config/bracer/<app>/ and does not ask the name; without a valid name
// nothing is asked.
func TestInitByName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	code, out, errOut := runWorld(t, dir, &world{sh: containerWorld()}, answers(s3Answers, noEncryption, pickAll, drillOff, schedDefault)...)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	appDir := filepath.Join(dir, ".config", "bracer", "shop")
	cfg, err := config.Load(filepath.Join(appDir, "app.yaml"))
	if err != nil || cfg.App != "shop" || cfg.SecretsFile != filepath.Join(appDir, "secrets.env") {
		t.Fatalf("%v %+v", err, cfg)
	}
	if strings.Contains(out, "App name") || !strings.Contains(out, "New app shop: its files go to "+appDir+".\nHave a Bracer account? bracer init shop --web picks the Postgres in the browser, and every run reports to https://app.bracer.dev.\n") {
		t.Errorf("output:\n%s", out)
	}
	for args, want := range map[string]string{
		"":                "bracer init: name the app: bracer init <app> (apps here: shop)",
		"Shop":            `bracer init: "Shop" is not an app name`,
		"shop blog":       "bracer init: one app at a time",
		"shop --no-drill": "flag provided but not defined: -no-drill",
	} {
		w := &world{sh: containerWorld(), args: strings.Fields(args)}
		if w.args == nil {
			w.args = []string{}
		}
		code, out, errOut := runWorld(t, dir, w)
		if code != exitUsage || out != "" || !strings.Contains(errOut, want) || w.tty != nil {
			t.Errorf("%q: %d\n%s\n%s", args, code, out, errOut)
		}
	}
}

// seedTS is the ts of the run initSeeded puts in the bucket (init's real
// run is faked, so it writes none).
const seedTS = "2026-10-20T030000Z"

// initSeeded runs init in w with answers, then gives the app's folder a run
// written from this machine (ADR-0023, ADR-0026).
func initSeeded(t *testing.T, dir string, w *world, answers ...string) *config.App {
	t.Helper()
	mem := w.mem
	t.Setenv("HOME", dir)
	mid := filepath.Join(dir, "machine-id")
	os.WriteFile(mid, []byte("0123456789abcdef\n"), 0o644)
	old := backup.MachineIDPath
	backup.MachineIDPath = mid
	t.Cleanup(func() { backup.MachineIDPath = old })
	code, out, errOut := runWorld(t, dir, w, answers...)
	if code != 0 {
		t.Fatalf("init: exit %d\n%s\n%s", code, out, errOut)
	}
	cfg, err := config.Load(appYAML(dir))
	if err != nil {
		t.Fatal(err)
	}
	here, err := backup.Here(cfg.Storage.Folder, appYAML(dir))
	if err != nil {
		t.Fatal(err)
	}
	putRun(t, mem, cfg, backup.Run{TS: seedTS, Writer: here, Backup: backup.Outcome{Result: backup.ResultOK}})
	return cfg
}

// appYAML is the app.yaml of app shop in home dir (ADR-0021).
func appYAML(dir string) string { return config.AppPath(dir, "shop") }

// ADR-0025: init asks an optional heartbeat URL after the schedule, checks
// it with /log (asking again when it does not answer), keeps it in
// secrets.env and prints what to set on the check.
func TestInitHeartbeat(t *testing.T) {
	dir := t.TempDir()
	good := "https://hc-ping.com/0b6a7c1e-5d2f-4e8a-9c3b-1f2e3d4c5b6a"
	w := &world{sh: containerWorld(), mem: storage.NewMemory(0), heartbeat: func(u string) error {
		if u != good {
			return errors.New("unknown heartbeat URL (404)")
		}
		return nil
	}}
	cfg := initSeeded(t, dir, w, answers(s3Answers, noEncryption, pickAll, drillOn,
		sched, []string{"ftp://x", "https://hc-ping.com/wrong", good})...)
	secrets, _, _ := config.LoadSecrets(cfg)
	if v, _ := secrets.Require("BRACER_SHOP_HEARTBEAT_URL"); cfg.HeartbeatURLEnv != "BRACER_SHOP_HEARTBEAT_URL" || v != good {
		t.Errorf("heartbeat_url_env %q, secret %q", cfg.HeartbeatURLEnv, v)
	}
	if strings.Join(w.hbLog, " ") != "https://hc-ping.com/wrong "+good {
		t.Errorf("checked: %q", w.hbLog)
	}

	// The same run again shows the questions and what to set on the check.
	dir2 := t.TempDir()
	t.Setenv("HOME", dir2)
	w2 := &world{sh: containerWorld(), heartbeat: w.heartbeat}
	// The check follows the backup time: the drill is inside backup (ADR-0026).
	code, out, errOut := runWorld(t, dir2, w2, answers(s3Answers, noEncryption, pickAll, drillOn,
		[]string{"", "04:30", ""}, []string{"ftp://x", "https://hc-ping.com/wrong", good})...)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"the heartbeat URL must be an http(s) URL with a host", "✗ hc-ping.com: unknown heartbeat URL (404)", "✓ the heartbeat URL at hc-ping.com answers",
		"Heartbeat: set the check at hc-ping.com to\n  schedule (cron): 30 4 * * *\n  time zone: ", "bracer only sends the signal; hc-ping.com is what notices a missing one."} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, good) || strings.Contains(errOut, good) {
		t.Errorf("the URL is a secret: never printed")
	}
}

// ADR-0029: the heartbeat hint gives one cron expression when one says the
// schedule (same minute everywhere, no `last`), else the schedule in words.
func TestScheduleCron(t *testing.T) {
	for _, tc := range []struct {
		sc   config.Schedule
		want string
		ok   bool
	}{
		{config.Schedule{Every: "daily", At: []string{"03:00"}}, "0 3 * * *", true},
		{config.Schedule{Every: "daily", At: []string{"03:30", "15:30"}}, "30 3,15 * * *", true},
		{config.Schedule{Every: "weekly", At: []string{"04:05"}, Days: []string{"mon", "sun"}}, "5 4 * * 1,0", true},
		{config.Schedule{Every: "monthly", At: []string{"00:00"}, Days: []string{"1", "15"}}, "0 0 1,15 * *", true},
		{config.Schedule{Every: "monthly", At: []string{"03:00"}, Days: []string{"last"}}, "", false},
		{config.Schedule{Every: "daily", At: []string{"03:00", "15:30"}}, "", false},
	} {
		if got, ok := scheduleCron(tc.sc); got != tc.want || ok != tc.ok {
			t.Errorf("scheduleCron(%+v) = %q, %v; want %q, %v", tc.sc, got, ok, tc.want, tc.ok)
		}
	}
	if got := describeSchedule(config.Schedule{Every: "monthly", At: []string{"03:00"}, Days: []string{"1", "last"}, Keep: 3}); got != "monthly on 1, last at 03:00, keep every backup of the last 3 months" {
		t.Error(got)
	}
}
