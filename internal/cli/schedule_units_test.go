package cli

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serviceRunning is a service whose ExecStart= runs args.
func serviceRunning(args string) string {
	return "[Service]\nType=oneshot\nExecStart=/usr/local/bin/bracer " + args + "\n"
}

// pairOf is the current pair of app, its service running app.
func pairOf(app string) map[string]string {
	return map[string]string{
		unitBase(app) + ".service": serviceRunning("backup " + app),
		unitBase(app) + ".timer":   "[Timer]\nOnCalendar=*-*-* 03:00:00\n",
	}
}

func writeUnits(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func merge(ms ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// hashUnits is the sha256 of every named file in dir.
func hashUnits(t *testing.T, dir string, names []string) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		out[n] = sha256.Sum256(b)
	}
	return out
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// recordRoot makes d.root record every command it runs (install, rm,
// systemctl), then run it as before.
func recordRoot(d *scheduleDeps) *[]string {
	var calls []string
	root := d.root
	d.root = func(ctx context.Context, name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return root(ctx, name, args...)
	}
	return &calls
}

// assertUntouched: every neighbour file still has its content, and no
// command run through root or systemctl names it.
func assertUntouched(t *testing.T, dir string, before map[string][32]byte, calls ...[]string) {
	t.Helper()
	for n, h := range before {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Errorf("%s must stay: %v", n, err)
			continue
		}
		if sha256.Sum256(b) != h {
			t.Errorf("%s changed:\n%s", n, b)
		}
		for _, cs := range calls {
			for _, c := range cs {
				if strings.Contains(c, n) {
					t.Errorf("%s was touched: %s", n, c)
				}
			}
		}
	}
}

// App names may hold '-': shop, shop-2, shop-2-x, shop-old and the real app
// shop-backup are each matched by exact name only.
func TestAppUnits(t *testing.T) {
	neighbours := merge(pairOf("shop"), pairOf("shop-2"), pairOf("shop-2-x"), pairOf("shop-old"))
	for _, tc := range []struct {
		name  string
		files map[string]string
		app   string
		want  string
	}{
		{"own pair only", neighbours, "shop", "bracer-shop.service bracer-shop.timer"},
		{"shop-2", neighbours, "shop-2", "bracer-shop-2.service bracer-shop-2.timer"},
		{"shop-2-x", neighbours, "shop-2-x", "bracer-shop-2-x.service bracer-shop-2-x.timer"},
		{"shop-old", neighbours, "shop-old", "bracer-shop-old.service bracer-shop-old.timer"},
		{"no unit", neighbours, "blog", ""},
		{"real app shop-backup is not shop's", merge(pairOf("shop"), pairOf("shop-backup")), "shop", "bracer-shop.service bracer-shop.timer"},
		{"real app shop-backup", merge(pairOf("shop"), pairOf("shop-backup")), "shop-backup", "bracer-shop-backup.service bracer-shop-backup.timer"},
		{"other suffix", map[string]string{"bracer-shop-old.service": serviceRunning("backup shop")}, "shop", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeUnits(t, dir, tc.files)
			got, err := appUnits(dir, tc.app)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, " ") != tc.want {
				t.Errorf("appUnits(%s) = %v, want %s", tc.app, got, tc.want)
			}
		})
	}
	// A directory with a unit name is no unit file.
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "bracer-shop.service"), 0o755)
	if got, err := appUnits(dir, "shop"); err != nil || len(got) != 0 {
		t.Errorf("directory: %v %v", got, err)
	}
}

// bracer remove <app> deletes the app's own units and nothing of an app
// whose name only starts the same:
// those files keep their bytes and no command names them.
func TestUninstallUnitsLeavesNeighbours(t *testing.T) {
	for _, tc := range []struct {
		name       string
		app        string
		own        map[string]string
		neighbours map[string]string
	}{
		{"shop", "shop", pairOf("shop"), merge(pairOf("shop-2"), pairOf("shop-2-x"), pairOf("shop-old"))},
		{"shop-2", "shop-2", pairOf("shop-2"), merge(pairOf("shop"), pairOf("shop-2-x"))},
		{"shop next to real app shop-backup", "shop", pairOf("shop"), merge(pairOf("shop-backup"), pairOf("shop-2"))},
		{"real app shop-backup", "shop-backup", pairOf("shop-backup"), pairOf("shop")},
		{"files that only start like shop's", "shop", pairOf("shop"), map[string]string{
			"bracer-shop-drill.timer": "t", "bracer-shop-backup.service": serviceRunning("backup shop"), "bracer-shop-backup.timer": "t",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := &fakeSystemctl{show: map[string]map[string]string{}}
			d, out, errb := testDeps(t, sc, "")
			rootCalls := recordRoot(&d)
			writeUnits(t, d.unitDir, merge(tc.own, tc.neighbours))
			before := hashUnits(t, d.unitDir, keys(tc.neighbours))
			if err := uninstallUnits(context.Background(), tc.app, d); err != nil {
				t.Fatalf("uninstallUnits: %v\n%s", err, errb)
			}
			for n := range tc.own {
				if _, err := os.Stat(filepath.Join(d.unitDir, n)); err == nil {
					t.Errorf("%s must be removed", n)
				}
				if !strings.Contains(out.String(), "Removed "+filepath.Join(d.unitDir, n)+"\n") {
					t.Errorf("stdout missing Removed %s:\n%s", n, out)
				}
				if strings.HasSuffix(n, ".timer") && !containsStr(sc.calls, "disable --now "+n) {
					t.Errorf("%s not disabled: %v", n, sc.calls)
				}
			}
			assertUntouched(t, d.unitDir, before, sc.calls, *rootCalls)
			if entries, _ := os.ReadDir(d.unitDir); len(entries) != len(tc.neighbours) {
				t.Errorf("left %d files, want %d", len(entries), len(tc.neighbours))
			}
			// Again: only neighbours are left, so nothing is installed for the app.
			sc.calls, *rootCalls = nil, nil
			out.Reset()
			if err := uninstallUnits(context.Background(), tc.app, d); err != nil || out.String() != "No schedule installed for app "+tc.app+".\n" || len(sc.calls) != 0 || len(*rootCalls) != 0 {
				t.Errorf("second remove: %v %q %v %v", err, out, sc.calls, *rootCalls)
			}
			assertUntouched(t, d.unitDir, before)
		})
	}
}

// bracer init <app> installs and enables the app's pair; the units of
// shop-2 or of a real app shop-backup stay as they were.
func TestInstallUnitsLeavesNeighbours(t *testing.T) {
	for _, tc := range []struct {
		name       string
		neighbours map[string]string
	}{
		{"shop-2 installed", merge(pairOf("shop-2"), pairOf("shop-2-x"), pairOf("shop-old"))},
		{"real app shop-backup installed", merge(pairOf("shop-backup"), pairOf("shop-2"))},
		{"orphan timer", map[string]string{"bracer-shop-drill.timer": "t"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := &fakeSystemctl{show: map[string]map[string]string{}}
			d, out, errb := testDeps(t, sc, "")
			rootCalls := recordRoot(&d)
			cfgPath := writeAppYAML(t, "shop")
			writeUnits(t, d.unitDir, tc.neighbours)
			before := hashUnits(t, d.unitDir, keys(tc.neighbours))
			if err := installUnits(context.Background(), loadAppYAML(t, cfgPath), cfgPath, d); err != nil {
				t.Fatalf("installUnits: %v\n%s", err, errb)
			}
			if !strings.Contains(out.String(), "Installed 2 units in "+d.unitDir+".\n") || !containsStr(sc.calls, "enable --now bracer-shop.timer") {
				t.Errorf("stdout:\n%s\ncalls: %v", out, sc.calls)
			}
			for _, n := range []string{"bracer-shop.service", "bracer-shop.timer"} {
				if _, err := os.Stat(filepath.Join(d.unitDir, n)); err != nil {
					t.Errorf("%s not installed: %v", n, err)
				}
			}
			assertUntouched(t, d.unitDir, before, sc.calls, *rootCalls)
			if entries, _ := os.ReadDir(d.unitDir); len(entries) != 2+len(tc.neighbours) {
				t.Errorf("%d files, want the pair and %d neighbours", len(entries), len(tc.neighbours))
			}
		})
	}
}

// TestBR022OnlyCurrentUnitNames: bracer-<app>.service and .timer are the
// only units of an app. Files named like an older layout
// (bracer-<app>-backup.*, bracer-<app>-drill.*), even running the app, are
// not the app's: appUnits does not list them, so remove and install never
// touch them.
func TestBR022OnlyCurrentUnitNames(t *testing.T) {
	old := map[string]string{
		"bracer-shop-backup.service": serviceRunning("backup shop"), "bracer-shop-backup.timer": "t",
		"bracer-shop-drill.service": serviceRunning("drill -c /home/deploy/.config/bracer/shop/app.yaml"), "bracer-shop-drill.timer": "t",
	}
	dir := t.TempDir()
	writeUnits(t, dir, merge(pairOf("shop"), old))
	if got, err := appUnits(dir, "shop"); err != nil || strings.Join(got, " ") != "bracer-shop.service bracer-shop.timer" {
		t.Fatalf("appUnits = %v %v", got, err)
	}

	sc := &fakeSystemctl{show: map[string]map[string]string{}}
	d, _, errb := testDeps(t, sc, "")
	rootCalls := recordRoot(&d)
	writeUnits(t, d.unitDir, merge(pairOf("shop"), old))
	before := hashUnits(t, d.unitDir, keys(old))
	cfgPath := writeAppYAML(t, "shop")
	if err := installUnits(context.Background(), loadAppYAML(t, cfgPath), cfgPath, d); err != nil {
		t.Fatalf("installUnits: %v\n%s", err, errb)
	}
	if err := uninstallUnits(context.Background(), "shop", d); err != nil {
		t.Fatalf("uninstallUnits: %v\n%s", err, errb)
	}
	assertUntouched(t, d.unitDir, before, sc.calls, *rootCalls)
	for _, n := range []string{"bracer-shop.service", "bracer-shop.timer"} {
		if exists(filepath.Join(d.unitDir, n)) {
			t.Errorf("%s must be removed", n)
		}
	}
}
