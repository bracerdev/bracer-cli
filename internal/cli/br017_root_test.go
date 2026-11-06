package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bracer.dev/bracer/internal/config"
)

// asRoot makes the process run with euid 0 for the test.
func asRoot(t *testing.T) {
	t.Helper()
	old := processEUID
	processEUID = func() int { return 0 }
	t.Cleanup(func() { processEUID = old })
}

// BR-017 review 2: as root, a command that finds no app says to run it
// without sudo as the user who created the app, never `bracer init <app>`
// (that would make a second, root-owned app beside the user's).
func TestBR017NoAppAsRoot(t *testing.T) {
	ctx := context.Background()
	fix := func(cmd string) string {
		return "Fix: run bracer " + cmd + " without sudo, as the user who created the app (as root, bracer sees only root's apps)"
	}
	type call func(d scheduleDeps, out, errb *bytes.Buffer) int
	run := func(name string, c call) string {
		d, out, errb := testDeps(t, &fakeSystemctl{}, "")
		d.tty = true
		if code := c(d, out, errb); code != exitUsage {
			t.Errorf("%s: exit %d", name, code)
		}
		return errb.String()
	}
	cases := map[string]struct {
		call call
		want string
	}{
		"status shop": {func(d scheduleDeps, _, _ *bytes.Buffer) int { return runStatus(ctx, []string{"shop"}, d) }, fix("status shop")},
		"status":      {func(d scheduleDeps, _, _ *bytes.Buffer) int { return runStatus(ctx, nil, d) }, fix("status")},
		"remove shop": {func(d scheduleDeps, _, _ *bytes.Buffer) int { return runRemove(ctx, []string{"shop"}, d) }, fix("remove shop")},
		"remove":      {func(d scheduleDeps, _, _ *bytes.Buffer) int { return runRemove(ctx, nil, d) }, fix("remove")},
		"backup shop": {func(_ scheduleDeps, out, errb *bytes.Buffer) int {
			return runBackup(ctx, []string{"shop"}, defaultBackupDeps(nil, out, errb))
		}, fix("backup shop")},
		"rotate-key shop": {func(_ scheduleDeps, out, errb *bytes.Buffer) int {
			return runRotateKey(ctx, []string{"shop"}, strings.NewReader(""), out, errb)
		}, fix("rotate-key shop")},
	}
	asRoot(t)
	for name, tc := range cases {
		got := run(name, tc.call)
		if !strings.Contains(got, tc.want+"\n") || strings.Contains(got, "Fix: bracer init") {
			t.Errorf("%s as root:\n%s", name, got)
		}
	}
	if got := initHint("shop"); got != "to back up this server: bracer init shop, without sudo, as the user the nightly job should run as (as root, bracer sees only root's apps)\n" {
		t.Errorf("restore hint as root: %q", got)
	}
	// init creates the app, so its own Fix stays, root or not.
	if err := noName(nil, "init"); !strings.HasSuffix(err.Error(), "\nFix: bracer init <name>") {
		t.Errorf("init without a name as root: %v", err)
	}

	// Not root: the app does not exist yet, init makes it.
	processEUID = func() int { return 1000 }
	for name, tc := range cases {
		got := run(name, tc.call)
		want := "Fix: bracer init shop\n"
		if !strings.Contains(name, "shop") {
			want = "Fix: bracer init <name>\n"
		}
		if !strings.Contains(got, want) || strings.Contains(got, "without sudo") {
			t.Errorf("%s as a user:\n%s", name, got)
		}
	}
	if got := initHint("shop"); got != "to back up this server: bracer init shop\n" {
		t.Errorf("restore hint as a user: %q", got)
	}
}

// BR-017 review 2: $SUDO_USER is never consulted. The fake user database
// knows deploy (SUDO_USER), who has an app; every command still uses
// root's home and looks nobody up.
func TestBR017SudoUserNeverConsulted(t *testing.T) {
	root, deployHome := t.TempDir(), t.TempDir()
	stubAccount(t, "root", root)
	asRoot(t)
	t.Setenv("HOME", root)
	t.Setenv("SUDO_USER", "deploy")
	var looked []string
	old := lookupAccount
	lookupAccount = func(name string) (account, error) {
		looked = append(looked, name)
		if name == "deploy" {
			return account{home: deployHome}, nil
		}
		return account{}, errors.New("unknown user " + name)
	}
	t.Cleanup(func() { lookupAccount = old })
	p := config.AppPath(deployHome, "shop")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(p, testApp(deployHome, "shop")); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if h, err := appHome(); err != nil || h != root {
		t.Errorf("appHome = %q, %v", h, err)
	}
	if h, err := defaultScheduleDeps(nil, &out, &errb).home(); err != nil || h != root {
		t.Errorf("status/remove home = %q, %v", h, err)
	}
	if h, err := defaultInitDeps(nil, &out, &errb).home(); err != nil || h != root {
		t.Errorf("init home = %q, %v", h, err)
	}
	if code := runStatus(context.Background(), []string{"shop"}, defaultScheduleDeps(nil, &out, &errb)); code != exitUsage || !strings.Contains(errb.String(), "no app shop on this machine") {
		t.Errorf("status found deploy's app: %d\n%s", code, errb.String())
	}
	if initHint("shop") == "" {
		t.Error("restore hint found deploy's app")
	}
	if len(looked) != 0 {
		t.Errorf("looked up %v", looked)
	}
}
