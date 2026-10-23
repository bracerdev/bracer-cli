package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"bracer.dev/bracer/internal/restore"
)

// spec restore: no app name, no flag but --verbose, a terminal only; it
// starts from the bucket (ADR-0023).
func TestRestoreRefusals(t *testing.T) {
	for args, want := range map[string]string{
		"shop":                    "restore takes no app name: it starts from the bucket",
		"--into-container pg":     "flag provided but not defined: -into-container",
		"--database shop":         "flag provided but not defined: -database",
		"":                        "restore needs a terminal",
		"--verbose":               "restore needs a terminal",
		"--verbose --dry-run x y": "flag provided but not defined: -dry-run",
	} {
		var out, errOut bytes.Buffer
		code := runRestore(context.Background(), strings.Fields(args), backupDeps{stdin: strings.NewReader(""), stdout: &out, stderr: &errOut})
		if code != exitUsage || !strings.Contains(errOut.String(), want) {
			t.Errorf("%q: code %d, stderr %q", args, code, errOut.String())
		}
	}
}

// spec restore § compare: one line per check, the fix under a ✗.
func TestPrintChecks(t *testing.T) {
	var w bytes.Buffer
	printChecks(&w, []restore.Check{
		{Area: "role", Name: "app", Detail: "missing", Fix: restore.FixSQL},
		{Area: "locks", Name: "max_locks_per_transaction", Detail: "64, needs 128", Fix: restore.FixSQL, Manual: "after env-up: docker restart pg"},
		{Area: "extension", Name: "shop: vector 0.8.1", Detail: "the target server does not have it", Fix: restore.FixManual, Manual: "use an image with vector"},
		{Area: "version", Name: "PostgreSQL 17", OK: true},
		{Area: "locale", Name: "shop", OK: true, Warn: true, Detail: "C.UTF-8 instead of en_US.UTF-8"},
	})
	want := "✗ role app: missing\n    fix: in env-up\n" +
		"✗ locks max_locks_per_transaction: 64, needs 128\n    fix: in env-up; after env-up: docker restart pg\n" +
		"✗ extension shop: vector 0.8.1: the target server does not have it\n    fix: use an image with vector\n" +
		"✓ version PostgreSQL 17\n" +
		"! locale shop: C.UTF-8 instead of en_US.UTF-8\n"
	if w.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", w.String(), want)
	}
}
