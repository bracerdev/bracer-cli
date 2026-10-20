package cli

import (
	"bytes"
	"strings"
	"testing"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/pg"
)

// spec init § Data model runtime: the recorded docker is used with PATH and
// DOCKER_HOST as defaults; a missing binary (another machine) falls back to PATH.
func TestWithRuntime(t *testing.T) {
	var errb bytes.Buffer
	d := backupDeps{shell: pg.ExecShell{Verbose: true}, stderr: &errb}
	cfg := &config.App{Runtime: &config.Runtime{Docker: "/bin/sh", Path: "/opt/x:/usr/bin", DockerHost: "unix:///run/user/1000/docker.sock"}}
	sh := d.withRuntime(cfg).shell.(pg.ExecShell)
	if sh.Docker != "/bin/sh" || !sh.Verbose || strings.Join(sh.Env, " ") != "PATH=/opt/x:/usr/bin DOCKER_HOST=unix:///run/user/1000/docker.sock" || errb.Len() != 0 {
		t.Errorf("shell: %+v %s", sh, errb.String())
	}
	cfg.Runtime.Docker = "/nonexistent/docker"
	if sh = d.withRuntime(cfg).shell.(pg.ExecShell); sh.Docker != "" || !strings.Contains(errb.String(), "using docker from PATH") {
		t.Errorf("missing docker: %+v %s", sh, errb.String())
	}
}
