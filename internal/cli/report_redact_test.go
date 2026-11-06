package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/storage"
)

// Secrets of the BR-014 tests: a secrets_file value, a URL password and a
// SQL verifier that are not, and an environment password.
const (
	br014Value    = "fakepw-br014-value"
	br014URLPW    = "url-pw-br014"
	br014Verifier = "SCRAM-SHA-256$4096:verifier-br014"
	br014EnvPW    = "env-pw-br014"
)

var br014Secrets = []string{br014Value, br014URLPW, br014Verifier, br014EnvPW}

// br014Stderr is what psql says when it cannot connect: it quotes all four.
const br014Stderr = "psql: error: connection to postgres://app:" + br014URLPW + "@db:5432/postgres failed: password " + br014Value +
	"\nLINE 1: ALTER ROLE app PASSWORD '" + br014Verifier + "'\nPGPASSWORD=" + br014EnvPW

// hbRecorder is a Healthchecks-style server that keeps each request with its body.
type hbRecorder struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []string
}

func newHBRecorder(t *testing.T) *hbRecorder {
	h := &hbRecorder{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.reqs = append(h.reqs, r.Method+" "+r.URL.RequestURI()+"\n"+string(body))
		h.mu.Unlock()
		w.Write([]byte("OK"))
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *hbRecorder) got() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.reqs, "\n---\n")
}

// br014App writes app shop: one container instance whose password is
// br014Value, drill on, a heartbeat URL, hosted: srv; a bucket whose folder
// has one run written from here. The fake shell is init's container world
// in which psql cannot connect (br014Stderr) and the drill's cleanup fails
// with a stderr that quotes the environment password.
func br014App(t *testing.T, srv *reportServer, hb *hbRecorder) (*config.App, backupDeps, *bytes.Buffer, *bytes.Buffer, *storage.Memory) {
	t.Helper()
	home := testHome(t)
	cfg := testApp(home, "shop")
	cfg.Hosted = &config.Hosted{Server: srv.URL, TokenEnv: "BRACER_TOKEN"}
	cfg.HeartbeatURLEnv = "BRACER_SHOP_HEARTBEAT_URL"
	cfg.Instances[0].PasswordEnv = "BRACER_SHOP_MAIN_PASSWORD"
	p := writeApp(t, cfg)
	writeSecrets(t, cfg)
	f, err := os.OpenFile(cfg.SecretsFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("BRACER_TOKEN=" + earlyToken + "\nBRACER_SHOP_HEARTBEAT_URL=" + hb.URL + "/ping/abc\nBRACER_SHOP_MAIN_PASSWORD=" + br014Value + "\n")
	f.Close()
	mid := home + "/machine-id"
	os.WriteFile(mid, []byte("0123456789abcdef\n"), 0o644)
	old := backup.MachineIDPath
	backup.MachineIDPath = mid
	t.Cleanup(func() { backup.MachineIDPath = old })
	here, err := backup.Here(cfg.Storage.Folder, p)
	if err != nil {
		t.Fatal(err)
	}
	mem := storage.NewMemory(0)
	putRun(t, mem, cfg, backup.Run{TS: time.Now().UTC().Add(-24 * time.Hour).Format(backupTSLayout), Writer: here, Backup: backup.Outcome{Result: backup.ResultOK}})
	sh := containerWorld()
	sh.fail[psql+"select 1"] = br014Stderr
	sh.fail["docker ps -aq"] = "permission denied; PGPASSWORD=" + br014EnvPW
	var out, errb bytes.Buffer
	d := backupDeps{shell: sh, newStore: func(storage.S3Config) (storage.Store, error) { return mem, nil },
		http: srv.Client(), stdout: &out, stderr: &errb}
	return cfg, d, &out, &errb, mem
}

// noSecret fails the test when text holds one of the BR-014 secrets.
func noSecret(t *testing.T, what, text string) {
	t.Helper()
	for _, s := range br014Secrets {
		if strings.Contains(text, s) {
			t.Errorf("%s holds %q:\n%s", what, s, text)
		}
	}
}

// BR-014: a secret quoted by the stderr of a tool (a secrets_file value, a
// URL password, a SQL verifier, PGPASSWORD=) is redacted where the run's
// text is made, so it is in none of stdout, --json, the journal (stderr),
// the heartbeat, the report the strict server accepts, and the run.json
// object in the bucket; the redacted text still says what failed.
func TestBR014BackupRedactsEverywhere(t *testing.T) {
	for _, args := range [][]string{{"shop"}, {"shop", "--json"}} {
		srv := newStrictReportServer(t)
		hb := newHBRecorder(t)
		cfg, d, out, errb, mem := br014App(t, srv, hb)
		code := runBackup(context.Background(), args, d)
		if code != exitError {
			t.Fatalf("%v: exit %d\n%s%s", args, code, out, errb)
		}
		noSecret(t, "stdout", out.String())
		noSecret(t, "stderr", errb.String())
		noSecret(t, "heartbeat", hb.got())
		if !strings.Contains(hb.got(), "GET /ping/abc/1") {
			t.Errorf("heartbeat: %s", hb.got())
		}
		bodies := srv.got()
		if len(bodies) != 1 {
			t.Fatalf("%v: %d reports accepted\n%s", args, len(bodies), out)
		}
		noSecret(t, "report", bodies[0])
		m := decodeBody(t, []byte(bodies[0]))
		ir := m["instances"].([]any)[0].(map[string]any)
		if e, _ := ir["error"].(string); !strings.Contains(e, "postgres://***@db:5432/postgres") || !strings.Contains(e, "PASSWORD ***") || !strings.Contains(e, "PGPASSWORD=***") {
			t.Errorf("report error: %q", e)
		}
		run, _, err := backup.Newest(context.Background(), mem, backup.Root(cfg))
		if err != nil {
			t.Fatal(err)
		}
		rc, err := mem.Get(context.Background(), backup.RunKey(backup.Root(cfg), run.TS))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(rc)
		rc.Close()
		noSecret(t, "run.json", string(raw))
		if w := strings.Join(run.Backup.Warnings, "\n"); !strings.Contains(w, "PGPASSWORD=***") || !strings.Contains(strings.Join(run.Backup.Reasons, "\n"), "PASSWORD ***") {
			t.Errorf("run.json: %s", raw)
		}
		if len(args) == 2 {
			var res backup.Result
			if err := json.Unmarshal(out.Bytes(), &res); err != nil || res.Instances[0].Error != ir["error"] {
				t.Errorf("--json: %v\n%s", err, out)
			}
		} else if !strings.Contains(out.String(), "warning: could not remove leftover drill resources: docker ps -aq: exit 1: permission denied; PGPASSWORD=***") {
			t.Errorf("stdout:\n%s", out)
		}
	}
}

// BR-014: backup --dry-run, plain and --json, prints the instance's error
// and the error of a check redacted.
func TestBR014DryRunRedacts(t *testing.T) {
	srv := newReportServer(t)
	hb := newHBRecorder(t)
	_, d, out, errb, _ := br014App(t, srv, hb)
	for _, args := range [][]string{{"shop", "--dry-run"}, {"shop", "--dry-run", "--json"}} {
		out.Reset()
		errb.Reset()
		if code := runBackup(context.Background(), args, d); code != exitError {
			t.Fatalf("%v: exit %d\n%s%s", args, code, out, errb)
		}
		noSecret(t, strings.Join(args, " "), out.String()+errb.String())
		if !strings.Contains(out.String(), "PASSWORD ***") {
			t.Errorf("%v: unreadable:\n%s", args, out)
		}
	}
	// A check that fails before the run: the bucket quotes a credential.
	d.newStore = func(storage.S3Config) (storage.Store, error) {
		return fakeStore{err: errors.New("dial https://key:" + br014Value + "@s3.example: refused")}, nil
	}
	out.Reset()
	runBackup(context.Background(), []string{"shop", "--dry-run"}, d)
	noSecret(t, "bucket check", out.String())
	if !strings.Contains(out.String(), "https://***@s3.example") {
		t.Errorf("bucket check:\n%s", out)
	}
	if len(srv.got()) != 0 || hb.got() != "" {
		t.Errorf("a dry run sent something: %v %s", srv.got(), hb.got())
	}
}

// BR-014: the report body is the run as backup.Backup left it: names equal
// to a secret value (a database, the container, digits of system_id, the
// kind) are sent as they are; the free text was redacted at its source.
func TestBR014ReportBodyKeepsNames(t *testing.T) {
	res := &backup.Result{Kind: "backup", App: "shop", TS: "2026-11-05T030000Z", Status: backup.ResultOK, OK: true,
		Instances: []*backup.InstanceResult{{Name: "main", Runner: "docker exec shop-db", SystemID: "7698217040907026472", OK: true,
			Databases: []*backup.DatabaseResult{{Name: "postgres", OK: true}, {Name: "shop", OK: true}}}}}
	data, err := reportBody(&config.App{App: "shop"}, res)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(data, &m)
	ir := m["instances"].([]any)[0].(map[string]any)
	dbs := ir["databases"].([]any)
	if m["kind"] != "backup" || m["app"] != "shop" || ir["runner"] != "docker exec shop-db" || ir["system_id"] != "7698217040907026472" ||
		dbs[0].(map[string]any)["db"] != "postgres" || dbs[1].(map[string]any)["db"] != "shop" || strings.Contains(string(data), "***") {
		t.Fatalf("body: %s", data)
	}
}
