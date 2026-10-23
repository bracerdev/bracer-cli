package cli

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
)

// ADR-0022: the recorded docker is used with exactly PATH and DOCKER_HOST of
// runtime:; docker missing at that path is an error, never a lookup in PATH.
func TestWithRuntime(t *testing.T) {
	d := backupDeps{shell: pg.ExecShell{Verbose: true}}
	cfg := &config.App{App: "shop", Runtime: &config.Runtime{Docker: "/bin/sh", Path: "/opt/x:/usr/bin", DockerHost: "unix:///run/user/1000/docker.sock"}}
	got, err := d.withRuntime(cfg)
	sh := got.shell.(pg.ExecShell)
	if err != nil || sh.Docker != "/bin/sh" || !sh.Verbose || strings.Join(sh.Env, " ") != "PATH=/opt/x:/usr/bin DOCKER_HOST=unix:///run/user/1000/docker.sock" {
		t.Errorf("shell: %+v %v", sh, err)
	}
	cfg.Runtime.DockerHost = ""
	if got, _ = d.withRuntime(cfg); strings.Join(got.shell.(pg.ExecShell).Env, " ") != "PATH=/opt/x:/usr/bin" {
		t.Errorf("no DOCKER_HOST: %+v", got.shell)
	}
	cfg.Runtime.Docker = "/nonexistent/docker"
	if _, err := d.withRuntime(cfg); err == nil || err.Error() != "docker not found at /nonexistent/docker (runtime.docker)\nFix: bracer remove shop, then bracer init shop" {
		t.Errorf("missing docker: %v", err)
	}
	// A fake shell is left alone.
	fake := &fakeShell{}
	if got, err := (backupDeps{shell: fake}).withRuntime(cfg); err != nil || got.shell != fake {
		t.Errorf("fake shell: %+v %v", got.shell, err)
	}
}

// Every job loads its secrets first: a variable missing in secrets.env is a
// usage error that names it and the Fix; Docker missing is a runtime error.
func TestLoadApp(t *testing.T) {
	home := testHome(t)
	cfg := testApp(home, "shop")
	cfg.Instances[0].PasswordEnv = "BRACER_SHOP_MAIN_PASSWORD"
	cfg.Runtime.Docker = filepath.Join(home, "docker")
	p := writeApp(t, cfg)
	writeSecrets(t, cfg)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, _, _, code, err := loadApp(p, backupDeps{shell: pg.ExecShell{}}, logger)
	if code != exitUsage || err == nil || err.Error() != "missing in "+cfg.SecretsFile+": BRACER_SHOP_MAIN_PASSWORD\nFix: bracer remove shop, then bracer init shop" {
		t.Errorf("missing secret: %d %v", code, err)
	}
	os.WriteFile(cfg.SecretsFile, []byte("BRACER_SHOP_S3_ACCESS_KEY=AK\nBRACER_SHOP_S3_SECRET_KEY=SK\nBRACER_SHOP_MAIN_PASSWORD=pw\n"), 0o600)
	if _, _, _, code, err := loadApp(p, backupDeps{shell: pg.ExecShell{}}, logger); code != exitError || err == nil || !strings.Contains(err.Error(), "docker not found at "+cfg.Runtime.Docker) {
		t.Errorf("missing docker: %d %v", code, err)
	}
	os.WriteFile(cfg.Runtime.Docker, nil, 0o755)
	got, secrets, d, code, err := loadApp(p, backupDeps{shell: pg.ExecShell{}}, logger)
	if code != exitOK || err != nil || got.App != "shop" || d.shell.(pg.ExecShell).Docker != cfg.Runtime.Docker {
		t.Fatalf("ok: %d %v", code, err)
	}
	if v, _ := secrets.Require("BRACER_SHOP_MAIN_PASSWORD"); v != "pw" {
		t.Errorf("secret: %q", v)
	}
	var buf bytes.Buffer
	os.WriteFile(p, []byte("app: shop\n"), 0o644)
	if _, _, _, code, err := loadApp(p, backupDeps{shell: pg.ExecShell{}, stderr: &buf}, logger); code != exitUsage || err == nil || !strings.Contains(err.Error(), "Fix: bracer remove shop, then bracer init shop") {
		t.Errorf("broken app.yaml: %d %v", code, err)
	}
}
