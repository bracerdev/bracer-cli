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
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/hostedfilter"
	"bracer.dev/bracer/internal/storage"
)

// reportServer is the hosted /report endpoint: it keeps every body.
type reportServer struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []string
	auth   []string
}

func newReportServer(t *testing.T) *reportServer {
	r := &reportServer{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		data, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.bodies = append(r.bodies, string(data))
		r.auth = append(r.auth, req.Header.Get("Authorization"))
		r.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *reportServer) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.bodies...)
}

// Secret values long enough to be redacted, and that the tests look for.
const (
	earlyAccessKey = "AKIAIOSFODNN7EXAMPLE"
	earlySecretKey = "wJalrXUtnFEMI-K7MDENG-bPxRfiCYEXAMPLEKEY"
	earlyToken     = "brc_live_9f8e7d6c5b4a"
)

// earlyApp writes app shop reporting to srv (hosted:) with its secrets and
// a machine id, and returns its app.yaml and backup deps whose store
// cannot be opened unless storeErr is nil.
func earlyApp(t *testing.T, srv *reportServer, storeErr error) (*config.App, string, backupDeps, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	home := testHome(t)
	cfg := testApp(home, "shop")
	cfg.Hosted = &config.Hosted{Server: srv.URL, TokenEnv: "BRACER_TOKEN"}
	p := writeApp(t, cfg)
	writeEarlySecrets(t, cfg, true, true)
	mid := filepath.Join(home, "machine-id")
	os.WriteFile(mid, []byte("0123456789abcdef\n"), 0o644)
	old := backup.MachineIDPath
	backup.MachineIDPath = mid
	t.Cleanup(func() { backup.MachineIDPath = old })
	var out, errb bytes.Buffer
	d := backupDeps{shell: &fakeShell{}, http: srv.Client(), stdout: &out, stderr: &errb,
		newStore: func(storage.S3Config) (storage.Store, error) {
			if storeErr != nil {
				return nil, storeErr
			}
			return storage.NewMemory(0), nil
		}}
	return cfg, p, d, &out, &errb
}

// writeEarlySecrets writes secrets.env with the bucket keys, the secret key
// only if withSecretKey, the token only if withToken.
func writeEarlySecrets(t *testing.T, cfg *config.App, withSecretKey, withToken bool) {
	t.Helper()
	data := cfg.Storage.AccessKeyEnv + "=" + earlyAccessKey + "\n"
	if withSecretKey {
		data += cfg.Storage.SecretKeyEnv + "=" + earlySecretKey + "\n"
	}
	if withToken {
		data += "BRACER_TOKEN=" + earlyToken + "\n"
	}
	if err := os.WriteFile(cfg.SecretsFile, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// oneFailedReport checks there is exactly one report, failed, whose error
// holds want and no secret value.
func oneFailedReport(t *testing.T, srv *reportServer, want string) {
	t.Helper()
	bodies := srv.got()
	if len(bodies) != 1 {
		t.Fatalf("%d reports: %v", len(bodies), bodies)
	}
	for _, secret := range []string{earlyAccessKey, earlySecretKey, earlyToken} {
		if strings.Contains(bodies[0], secret) {
			t.Fatalf("report carries a secret value: %s", bodies[0])
		}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &m); err != nil {
		t.Fatal(err)
	}
	if m["status"] != "failed" || m["ok"] != false || m["kind"] != "backup" || m["app"] != "shop" {
		t.Fatalf("not a failure report: %s", bodies[0])
	}
	if e, _ := m["error"].(string); !strings.Contains(e, want) {
		t.Fatalf("error %q lacks %q", e, want)
	}
	if _, ok := m["schedule"].(map[string]any); !ok || m["timezone"] == nil || m["utc_offset_minutes"] == nil {
		t.Fatalf("no schedule/zone: %s", bodies[0])
	}
	if srv.auth[0] != "Bearer "+earlyToken {
		t.Fatalf("auth header: %q", srv.auth[0])
	}
}

// BR-003: a store that cannot be opened (wrong S3 keys, bad endpoint)
// sends one failed report with the error, secrets redacted; the printed
// error and the exit code are those of before.
func TestBackupStoreErrorReports(t *testing.T) {
	srv := newReportServer(t)
	_, _, d, out, errb := earlyApp(t, srv, errors.New("s3: endpoint refused key "+earlyAccessKey+" with secret "+earlySecretKey))
	code := runBackup(context.Background(), []string{"shop"}, d)
	// The printed error is redacted too: the journal keeps it (BR-014).
	if code != exitError || !strings.Contains(errb.String(), "bracer backup: s3: endpoint refused key *** with secret ***") {
		t.Fatalf("exit %d\n%s", code, errb)
	}
	oneFailedReport(t, srv, "s3: endpoint refused key *** with secret ***")
	if !strings.Contains(out.String(), "✓ reported to "+srv.URL) {
		t.Errorf("no reported line:\n%s", out)
	}
}

// BR-003: a run blocked by the lock of another one sends one failed
// report that names the lock.
func TestBackupLockHeldReports(t *testing.T) {
	srv := newReportServer(t)
	_, p, d, _, errb := earlyApp(t, srv, nil)
	lockPath := filepath.Join(filepath.Dir(p), "."+filepath.Base(p)+".lock")
	held, err := backup.Acquire(p)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	code := runBackup(context.Background(), []string{"shop"}, d)
	if code != exitError || !strings.Contains(errb.String(), "bracer backup: a backup is running for this app.yaml") {
		t.Fatalf("exit %d\n%s", code, errb)
	}
	oneFailedReport(t, srv, "backup lock held: a backup is running for this app.yaml")
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("the other run's lock was removed: %v", err)
	}
}

// BR-003: a variable missing from secrets.env (exit 2) still reports when
// the token is there.
func TestBackupMissingSecretReports(t *testing.T) {
	srv := newReportServer(t)
	cfg, _, d, _, errb := earlyApp(t, srv, nil)
	writeEarlySecrets(t, cfg, false, true)
	code := runBackup(context.Background(), []string{"shop"}, d)
	if code != exitUsage || !strings.Contains(errb.String(), "missing in "+cfg.SecretsFile+": "+cfg.Storage.SecretKeyEnv) {
		t.Fatalf("exit %d\n%s", code, errb)
	}
	oneFailedReport(t, srv, "missing in "+cfg.SecretsFile+": "+cfg.Storage.SecretKeyEnv)
}

// BR-003: an app.yaml that does not validate is read loosely for hosted:.
func TestBackupInvalidConfigReports(t *testing.T) {
	srv := newReportServer(t)
	_, p, d, _, errb := earlyApp(t, srv, nil)
	data, _ := os.ReadFile(p)
	os.WriteFile(p, append(data, []byte("surprise: 1\n")...), 0o644)
	code := runBackup(context.Background(), []string{"shop"}, d)
	if code != exitUsage || !strings.Contains(errb.String(), "surprise") {
		t.Fatalf("exit %d\n%s", code, errb)
	}
	oneFailedReport(t, srv, "surprise")
}

// BR-003: no report without hosted:, in a dry run, in init's run, or when
// the token cannot be read; the exit code is the same in each case.
func TestBackupEarlyFailureNoReport(t *testing.T) {
	storeErr := errors.New("s3: no such host")
	t.Run("no hosted", func(t *testing.T) {
		srv := newReportServer(t)
		cfg, _, d, out, _ := earlyApp(t, srv, storeErr)
		cfg.Hosted = nil
		writeApp(t, cfg)
		if code := runBackup(context.Background(), []string{"shop"}, d); code != exitError || len(srv.got()) != 0 || strings.Contains(out.String(), "reported") {
			t.Fatalf("exit %d, reports %v\n%s", code, srv.got(), out)
		}
	})
	t.Run("dry run", func(t *testing.T) {
		srv := newReportServer(t)
		_, _, d, _, _ := earlyApp(t, srv, storeErr)
		if code := runBackup(context.Background(), []string{"shop", "--dry-run"}, d); code != exitError || len(srv.got()) != 0 {
			t.Fatalf("exit %d, reports %v", code, srv.got())
		}
	})
	t.Run("init draft", func(t *testing.T) {
		srv := newReportServer(t)
		_, p, d, _, _ := earlyApp(t, srv, storeErr)
		if err := os.Rename(p, draftPath(p)); err != nil {
			t.Fatal(err)
		}
		if code := runBackup(context.Background(), []string{"shop", "--init-draft"}, d); code != exitError || len(srv.got()) != 0 {
			t.Fatalf("exit %d, reports %v", code, srv.got())
		}
	})
	t.Run("token missing", func(t *testing.T) {
		srv := newReportServer(t)
		cfg, _, d, out, errb := earlyApp(t, srv, storeErr)
		writeEarlySecrets(t, cfg, true, false)
		code := runBackup(context.Background(), []string{"shop"}, d)
		if code != exitUsage || len(srv.got()) != 0 || !strings.Contains(out.String(), "✗ not reported to "+srv.URL+": token BRACER_TOKEN missing") || !strings.Contains(errb.String(), "hosted: result not reported") {
			t.Fatalf("exit %d, reports %v\n%s\n%s", code, srv.got(), out, errb)
		}
	})
	t.Run("secrets file unreadable", func(t *testing.T) {
		srv := newReportServer(t)
		cfg, _, d, out, errb := earlyApp(t, srv, storeErr)
		os.Remove(cfg.SecretsFile)
		code := runBackup(context.Background(), []string{"shop"}, d)
		if code != exitUsage || len(srv.got()) != 0 || !strings.Contains(out.String(), "✗ not reported to "+srv.URL+": cannot read the token BRACER_TOKEN") || !strings.Contains(errb.String(), "hosted: result not reported") {
			t.Fatalf("exit %d, reports %v\n%s\n%s", code, srv.got(), out, errb)
		}
	})
}

func TestSanitizeError(t *testing.T) {
	secrets := config.NewSecrets(map[string]string{"S3_SECRET": "s3cr3t-value", "PIN": "abc", "TOKEN": "brc_tok", "LONG": "brc_tok_longer"}, "test")
	cases := []struct{ name, in, want string }{
		{"secret value", "bucket refused s3cr3t-value twice: s3cr3t-value", "bucket refused *** twice: ***"},
		{"short values stay", "abc is fine", "abc is fine"},
		{"longest first", "token brc_tok_longer", "token ***"},
		{"url userinfo", "dial postgres://app:hunter2@db:5432/shop failed", "dial postgres://***@db:5432/shop failed"},
		{"other scheme", "GET https://user:pw@example.com/x", "GET https://***@example.com/x"},
		{"url without password", "postgres://app@db/shop", "postgres://app@db/shop"},
		{"aws key", "key AKIAIOSFODNN7EXAMPLE rejected", "key *** rejected"},
		{"age key", "identity AGE-SECRET-KEY-1QYQSZQGPQYQSZQGPQYQSZQGPQYQSZQGPQYQSZQGPQYQSZQGPQYQSZQGP3HKZ0F is wrong", "identity *** is wrong"},
		{"sql password", "ERROR at ALTER ROLE x PASSWORD 'hunter2'", "ERROR at ALTER ROLE x PASSWORD ***"},
		{"plain", "a backup is running for this app.yaml (pid 12)\nFix: wait", "a backup is running for this app.yaml (pid 12)\nFix: wait"},
	}
	for _, c := range cases {
		got := sanitizeError(c.in, secrets)
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
		if hostedfilter.Refuses(got) {
			t.Errorf("%s: the server would refuse %q", c.name, got)
		}
	}
	// No secrets loaded (secrets_file unreadable): patterns still apply.
	if got := sanitizeError("AKIAIOSFODNN7EXAMPLE", nil); got != "***" {
		t.Errorf("nil secrets: %q", got)
	}
	// Long text is cut to 1000 bytes on a rune boundary.
	long := strings.Repeat("é", 700) // 1400 bytes
	got := sanitizeError(long, nil)
	if len(got) > maxReportError || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Errorf("cut: %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
	if short := strings.Repeat("x", maxReportError); sanitizeError(short, nil) != short {
		t.Error("text of exactly the limit was cut")
	}
}

// BR-003: the failure report of backup.Backup's own error is sanitized too.
func TestFailureReportSanitized(t *testing.T) {
	secrets := config.NewSecrets(map[string]string{"K": earlySecretKey}, "test")
	m := failureReport("backup", "shop", time.Time{}, time.Time{}, errors.New("list: "+earlySecretKey+" denied"), secrets)
	if m["error"] != "list: *** denied" {
		t.Fatalf("error: %v", m["error"])
	}
}
