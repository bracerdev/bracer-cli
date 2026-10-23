package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/storage"
)

// hcServer records the paths of the pings it gets, like Healthchecks.
type hcServer struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string
}

func newHCServer(t *testing.T) *hcServer {
	h := &hcServer{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.paths = append(h.paths, r.URL.RequestURI())
		h.mu.Unlock()
		w.Write([]byte("OK"))
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *hcServer) got() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.paths, " ")
}

func (h *hcServer) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.paths = nil
}

// heartbeatApp writes app shop with a heartbeat URL and a bucket whose
// folder has one run written from here, and returns the deps of backup.
func heartbeatApp(t *testing.T, hc *hcServer) (*config.App, backupDeps, *bytes.Buffer) {
	t.Helper()
	home := testHome(t)
	cfg := testApp(home, "shop")
	cfg.HeartbeatURLEnv = "BRACER_SHOP_HEARTBEAT_URL"
	p := writeApp(t, cfg)
	writeSecrets(t, cfg)
	f, _ := os.OpenFile(cfg.SecretsFile, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString("BRACER_SHOP_HEARTBEAT_URL=" + hc.URL + "/ping/abc?rid=1\n")
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
	putRun(t, mem, cfg, backup.Run{TS: time.Now().UTC().Add(-24 * time.Hour).Format(backupTSLayout), Writer: here, Backup: backup.Outcome{Result: backup.ResultOK},
		Instances: map[string]*backup.RunInstance{"main": {Databases: map[string]*backup.Dump{"shop": {}}}}})
	var out, errb bytes.Buffer
	d := backupDeps{shell: &fakeShell{}, newStore: func(storage.S3Config) (storage.Store, error) { return mem, nil },
		http: hc.Client(), stdout: &out, stderr: &errb}
	return cfg, d, &errb
}

// ADR-0025, ADR-0026: every backup run sends the one signal, /start then
// /<exit>, the drill included, with drill on or off; a dry run sends
// nothing. A failure to send only warns and never shows the URL.
func TestBackupHeartbeat(t *testing.T) {
	hc := newHCServer(t)
	cfg, d, errb := heartbeatApp(t, hc)
	runBackup(context.Background(), []string{"shop", "--dry-run"}, d)
	if got := hc.got(); got != "" {
		t.Errorf("a dry run must not ping: %s\n%s", got, errb)
	}
	// The fake shell answers nothing, so the instance cannot be dumped: exit 1.
	code := runBackup(context.Background(), []string{"shop"}, d)
	if got := hc.got(); code != exitError || got != "/ping/abc/start?rid=1 /ping/abc/"+strconv.Itoa(code)+"?rid=1" {
		t.Errorf("drill on: exit %d pings %s\n%s", code, got, errb)
	}
	hc.reset()
	cfg.Instances[0].SetDrillOff(true)
	writeApp(t, cfg)
	code = runBackup(context.Background(), []string{"shop"}, d)
	if got := hc.got(); code != exitError || got != "/ping/abc/start?rid=1 /ping/abc/"+strconv.Itoa(code)+"?rid=1" {
		t.Errorf("drill off: exit %d pings %s\n%s", code, got, errb)
	}
	hc.Close()
	errb.Reset()
	heartbeatSleep = func(time.Duration) {}
	t.Cleanup(func() { heartbeatSleep = time.Sleep })
	runBackup(context.Background(), []string{"shop"}, d)
	if !strings.Contains(errb.String(), "heartbeat not sent") || strings.Contains(errb.String(), hc.URL) {
		t.Errorf("send failure:\n%s", errb)
	}
}
