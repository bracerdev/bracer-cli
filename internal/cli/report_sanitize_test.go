package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/hostedfilter"
	"bracer.dev/bracer/internal/storage"
)

// br008Password is the fake password of the BR-008 tests.
const br008Password = "fakepw-br008-xyz"

// looksLikeCredentials is the hosted server's check of a /report body: a
// body it refuses is lost.
func looksLikeCredentials(body string) bool { return hostedfilter.RefusesJSON([]byte(body)) }

func TestBR008Redaction(t *testing.T) {
	secrets := config.NewSecrets(map[string]string{}, "test")
	cases := []struct{ name, in, want string }{
		{"userinfo password", "dial postgres://u:" + br008Password + "@10.0.0.1:5432/db failed", "dial postgres://***@10.0.0.1:5432/db failed"},
		{"password with @", "postgres://u:p@ss@host/db", "postgres://***@host/db"},
		{"password with @ and :", "postgres://u:p@s:s%rd@host:5432/db?sslmode=require", "postgres://***@host:5432/db?sslmode=require"},
		{"escaped @", "postgres://u:p%40ss@host/db", "postgres://***@host/db"},
		{"bad escape", "postgres://u:" + br008Password + "%zz@host/db", "postgres://***@host/db"},
		{"query password", "postgres://u@h:abc/db?password=" + br008Password, "postgres://u@h:abc/db?password=***"},
		{"query password not first", "postgres://u@h/db?sslmode=require&password=" + br008Password + "&x=1", "postgres://u@h/db?sslmode=require&password=***"},
		// The value runs to the next blank (BR-014): a ; & or # may be the password's.
		{"semicolon", "Host=h;Password=" + br008Password + ";Database=db", "Host=h;Password=***"},
		{"key/value", "host=h password=" + br008Password + " dbname=db", "host=h password=*** dbname=db"},
		{"key/value at start", "password=" + br008Password, "password=***"},
		{"uppercase", "x PASSWORD=" + br008Password, "x PASSWORD=***"},
		{"sslpassword", "host=h sslpassword=" + br008Password, "host=h sslpassword=***"},
		{"url-encoded value", "?password=p%40ss%zz'", "?password=***"},
		{"url without password", "postgres://app@db/shop", "postgres://app@db/shop"},
		{"plain", "pg_dump: error: connection refused\nFix: start it", "pg_dump: error: connection refused\nFix: start it"},
		{"password word", "the password is wrong", "the password is wrong"},
	}
	for _, c := range cases {
		once := sanitizeError(c.in, secrets)
		if once != c.want {
			t.Errorf("%s: got %q, want %q", c.name, once, c.want)
		}
		if twice := sanitizeError(once, secrets); twice != once {
			t.Errorf("%s: not idempotent: %q then %q", c.name, once, twice)
		}
		if hostedfilter.Refuses(once) {
			t.Errorf("%s: the server would refuse %q", c.name, once)
		}
	}
}

// decodeBody checks a /report body against the server and decodes it.
func decodeBody(t *testing.T, data []byte) map[string]any {
	t.Helper()
	if strings.Contains(string(data), br008Password) {
		t.Fatalf("body holds the password: %s", data)
	}
	if looksLikeCredentials(string(data)) {
		t.Fatalf("the server would refuse the body: %s", data)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// newStrictReportServer is /report as hosted runs it: a body that looks
// like it holds credentials is refused with 400 and not kept. Before
// BR-008 a bad URL lost its report this way.
func newStrictReportServer(t *testing.T) *reportServer {
	r := &reportServer{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		data, _ := io.ReadAll(req.Body)
		if looksLikeCredentials(string(data)) {
			http.Error(w, "CREDENTIALS_IN_BODY", http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		r.bodies = append(r.bodies, string(data))
		r.auth = append(r.auth, req.Header.Get("Authorization"))
		r.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(r.Close)
	return r
}

// urlApp writes app shop with URL instances (url_env NAME_URL, drill off)
// reporting to srv, its secrets.env holding urls, and a bucket whose folder
// has one run written from here.
func urlApp(t *testing.T, srv *reportServer, urls map[string]string) (backupDeps, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	home := testHome(t)
	cfg := testApp(home, "shop")
	cfg.Hosted = &config.Hosted{Server: srv.URL, TokenEnv: "BRACER_TOKEN"}
	cfg.Instances = nil
	data := "BRACER_TOKEN=" + earlyToken + "\n"
	for _, name := range []string{"main", "other"} {
		u, ok := urls[name]
		if !ok {
			continue
		}
		env := strings.ToUpper(name) + "_URL"
		in := config.Instance{Name: name, Connection: config.Connection{URLEnv: env, PgMajor: 17, SystemID: config.SystemIDNone}, Databases: config.Databases{{Name: "db"}}}
		in.SetDrillOff(true)
		cfg.Instances = append(cfg.Instances, in)
		data += env + "=" + u + "\n"
	}
	p := writeApp(t, cfg)
	writeSecrets(t, cfg)
	f, err := os.OpenFile(cfg.SecretsFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(data)
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
	var out, errb bytes.Buffer
	d := backupDeps{shell: &fakeShell{}, newStore: func(storage.S3Config) (storage.Store, error) { return mem, nil },
		http: srv.Client(), stdout: &out, stderr: &errb}
	return d, &out, &errb
}

// BR-008: a URL instance whose URL does not parse fails the run; its
// report reaches the strict server with a readable error and the password
// is in nothing backup prints, logs or sends.
func TestBR008BackupBadURLReported(t *testing.T) {
	urls := map[string]string{
		"bad port":    "postgres://u:" + br008Password + "@10.0.0.1:abc/db",
		"query":       "postgres://u@10.0.0.1:abc/db?password=" + br008Password,
		"bad escape":  "postgres://u:" + br008Password + "%zz@10.0.0.1/db",
		"@ in secret": "postgres://u:" + br008Password + "@x@10.0.0.1:abc/db",
	}
	for name, u := range urls {
		t.Run(name, func(t *testing.T) {
			for _, args := range [][]string{{"shop", "--verbose"}, {"shop", "--json"}} {
				srv := newStrictReportServer(t)
				d, out, errb := urlApp(t, srv, map[string]string{"main": u})
				code := runBackup(context.Background(), args, d)
				all := out.String() + errb.String()
				if code != exitError || strings.Contains(all, br008Password) {
					t.Fatalf("%v: exit %d\n%s", args, code, all)
				}
				bodies := srv.got()
				if len(bodies) != 1 {
					t.Fatalf("%v: %d reports accepted\n%s", args, len(bodies), all)
				}
				m := decodeBody(t, []byte(bodies[0]))
				ir := m["instances"].([]any)[0].(map[string]any)
				if m["status"] != "failed" || m["kind"] != "backup" || !strings.HasPrefix(ir["error"].(string), "instance main: MAIN_URL: invalid URL: ") {
					t.Fatalf("%v: report %s", args, bodies[0])
				}
				if args[1] == "--json" {
					var res backup.Result
					if err := json.Unmarshal(out.Bytes(), &res); err != nil || res.Instances[0].Error != ir["error"] {
						t.Fatalf("--json: %v\n%s", err, out)
					}
				} else if !strings.Contains(out.String(), "✓ reported to "+srv.URL) || !strings.Contains(out.String(), "✗ main: instance main: MAIN_URL: invalid URL") {
					t.Fatalf("printed:\n%s", out)
				}
			}
		})
	}
}

// BR-008: of two URL instances, the bad one's error is redacted; the other
// fails its own way (the fake shell has no Docker) and keeps its error.
func TestBR008BackupTwoURLInstances(t *testing.T) {
	srv := newStrictReportServer(t)
	d, out, errb := urlApp(t, srv, map[string]string{
		"main":  "postgres://u:" + br008Password + "@10.0.0.1:abc/db",
		"other": "postgres://app:other-pw-1234@db.internal:5432/db",
	})
	code := runBackup(context.Background(), []string{"shop"}, d)
	all := out.String() + errb.String()
	if code != exitError || strings.Contains(all, br008Password) || strings.Contains(all, "other-pw-1234") {
		t.Fatalf("exit %d\n%s", code, all)
	}
	bodies := srv.got()
	if len(bodies) != 1 {
		t.Fatalf("%d reports\n%s", len(bodies), all)
	}
	m := decodeBody(t, []byte(bodies[0]))
	ins := m["instances"].([]any)
	main, other := ins[0].(map[string]any), ins[1].(map[string]any)
	if e := main["error"].(string); e != "instance main: MAIN_URL: invalid URL: invalid port after host" {
		t.Fatalf("main: %q", e)
	}
	if e, _ := other["error"].(string); other["name"] != "other" || !strings.HasPrefix(e, "instance other: ") || strings.Contains(e, "***") {
		t.Fatalf("other: %v", other)
	}
}
