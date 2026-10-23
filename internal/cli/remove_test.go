package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordingRoot replaces sudo with a recorder: every system-writing command
// is appended to *ran, nothing is performed.
func recordingRoot(ran *[][]string) func(context.Context, string, ...string) error {
	return func(_ context.Context, name string, args ...string) error {
		*ran = append(*ran, append([]string{name}, args...))
		return nil
	}
}

// spec schedule § remove: terminal only, the app name typed, the units
// through root, then the app's files but age.key (ADR-0030); the bucket is
// never touched.
func TestRemove(t *testing.T) {
	cfgPath := writeAppYAML(t, "shop")
	dir := filepath.Dir(cfgPath)
	var ran [][]string
	deps := func(in string) (scheduleDeps, *strings.Builder, *strings.Builder) {
		// The last run failed: its result is cleared on remove.
		sc := &fakeSystemctl{show: map[string]map[string]string{"bracer-shop.service": {"ActiveState": "failed"}}}
		d, _, _ := testDeps(t, sc, in)
		var out, errb strings.Builder
		d.stdout, d.stderr = &out, &errb
		d.euid, d.tty = 1000, true
		ran = nil
		d.root = recordingRoot(&ran)
		return d, &out, &errb
	}

	d, _, errb := deps("shop\n")
	d.tty = false
	if code := runRemove(context.Background(), []string{"shop"}, d); code != exitUsage || !strings.Contains(errb.String(), "remove needs a terminal") || ran != nil {
		t.Errorf("no terminal: %d %s %v", code, errb, ran)
	}
	d, _, errb = deps("")
	if code := runRemove(context.Background(), nil, d); code != exitUsage || !strings.Contains(errb.String(), "name the app: bracer remove <app> (apps here: shop)") {
		t.Errorf("no name: %d %s", code, errb)
	}

	// Anything but the app's name removes nothing.
	for _, typed := range []string{"y\n", "Shop\n", ""} {
		d, out, _ := deps(typed)
		if code := runRemove(context.Background(), []string{"shop"}, d); code != exitUsage || !strings.Contains(out.String(), "nothing removed") || ran != nil {
			t.Errorf("typed %q: %d %v\n%s", typed, code, ran, out)
		}
		if _, err := os.Stat(cfgPath); err != nil {
			t.Fatalf("typed %q: app.yaml must stay: %v", typed, err)
		}
	}

	// The units cannot go: nothing is deleted.
	d, _, errb = deps("shop\n")
	os.WriteFile(filepath.Join(d.unitDir, "bracer-shop.timer"), []byte("x"), 0o644)
	d.root = func(context.Context, string, ...string) error { return errors.New("sudo: a password is required") }
	if code := runRemove(context.Background(), []string{"shop"}, d); code != exitError || !strings.Contains(errb.String(), "bracer remove: sudo: a password is required\ntimer not removed; nothing deleted") {
		t.Errorf("sudo fails: %d %s", code, errb)
	}
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("app.yaml must stay when the timer stays: %v", err)
	}

	d, out, errb := deps("shop\n")
	for _, n := range []string{"bracer-shop.service", "bracer-shop.timer", "bracer-blog.timer"} {
		os.WriteFile(filepath.Join(d.unitDir, n), []byte("x"), 0o644)
	}
	// Everything of the app goes but age.key, which the next init reuses.
	keyPath := filepath.Join(dir, "age.key")
	for _, n := range []string{"age.key", "secrets.env", ".app.yaml.init", ".app.yaml.lock"} {
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600)
	}
	if code := runRemove(context.Background(), []string{"shop"}, d); code != exitOK {
		t.Fatalf("remove: %d %s\n%s", code, errb, out)
	}
	s := out.String()
	for _, want := range []string{"app shop stores its backups at:\n  endpoint  https://s3.example\n  bucket    b\n  folder    bracer/shop-abc123\n",
		"the backups in shop-abc123 stay in the bucket", "Type the app name to remove it: ",
		"Removed " + filepath.Join(d.unitDir, "bracer-shop.service") + "\nRemoved " + filepath.Join(d.unitDir, "bracer-shop.timer") + "\n",
		"app shop removed from this machine; its backups stay in b/bracer/shop-abc123\n",
		"kept " + keyPath + ": it opens the backups in b/bracer/shop-abc123; delete it yourself when you no longer need them\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "prefix") {
		t.Errorf("no prefix line: bracer always lives at bracer/ in the bucket:\n%s", s)
	}
	var got []string
	for _, c := range ran {
		got = append(got, strings.Join(c, " "))
	}
	want := "systemctl disable --now bracer-shop.timer\nsystemctl stop bracer-shop.service\n" +
		"rm -f " + filepath.Join(d.unitDir, "bracer-shop.service") + "\nrm -f " + filepath.Join(d.unitDir, "bracer-shop.timer") + "\n" +
		"systemctl daemon-reload\nsystemctl reset-failed bracer-shop.service"
	if strings.Join(got, "\n") != want {
		t.Errorf("ran:\n%s\nwant:\n%s", strings.Join(got, "\n"), want)
	}
	left, _ := os.ReadDir(dir)
	if len(left) != 1 || left[0].Name() != "age.key" {
		t.Errorf("only age.key may stay, left: %v", left)
	}
}

// A broken app.yaml is removed too: that is how it is fixed (ADR-0023); the
// units are named after the app, so app.yaml is not needed to find them.
func TestRemoveBrokenApp(t *testing.T) {
	cfgPath := writeAppYAML(t, "shop")
	os.WriteFile(cfgPath, []byte("app: shop\nbogus: 1\n"), 0o644)
	d, _, _ := testDeps(t, &fakeSystemctl{}, "shop\n")
	var out strings.Builder
	d.stdout, d.tty = &out, true
	var ran [][]string
	d.root = recordingRoot(&ran)
	if code := runRemove(context.Background(), []string{"shop"}, d); code != exitOK {
		t.Fatalf("code %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "app shop: where it stores its backups is unknown (app.yaml cannot be read)") || !strings.Contains(out.String(), "No schedule installed for app shop.") || ran != nil {
		t.Errorf("ran %q\n%s", ran, out.String())
	}
	if _, err := os.Stat(filepath.Dir(cfgPath)); !os.IsNotExist(err) {
		t.Errorf("the app directory must be gone: %v", err)
	}
}
