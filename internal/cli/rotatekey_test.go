package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/crypto"
)

// rotateFixture creates an app.yaml with age encryption and a key file in a temp directory.
func rotateFixture(t *testing.T) (cfgPath, keyPath string, kp crypto.Keypair) {
	t.Helper()
	dir := t.TempDir()
	kp, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	keyPath = filepath.Join(dir, "shop.key")
	if err := crypto.WriteIdentityFile(keyPath, kp); err != nil {
		t.Fatal(err)
	}
	cfg := &config.App{App: "shop", Encryption: "age", AgeRecipient: kp.Recipient, AgeIdentityFile: keyPath,
		Instances: []config.Instance{{Name: "main", Container: "db", User: "postgres", Databases: config.Databases{All: true}}}}
	cfgPath = filepath.Join(dir, "app.yaml")
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	return cfgPath, keyPath, kp
}

func TestRotateKey(t *testing.T) {
	cfgPath, keyPath, old := rotateFixture(t)
	var out, errb bytes.Buffer
	code := runRotateKey(context.Background(), []string{"-c", cfgPath}, strings.NewReader("y\n"), &out, &errb)
	if code != exitOK {
		t.Fatalf("code=%d err=%s", code, errb.String())
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgeRecipient == old.Recipient || !strings.HasPrefix(cfg.AgeRecipient, "age1") {
		t.Errorf("app.yaml recipient not rotated: %s", cfg.AgeRecipient)
	}
	ids, recips, err := crypto.ReadIdentities(keyPath)
	if err != nil || len(ids) != 2 || recips[0] != old.Recipient || recips[1] != cfg.AgeRecipient {
		t.Errorf("key file must hold old then new key: %v %v", recips, err)
	}
	if st, _ := os.Stat(keyPath); st.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %v", st.Mode().Perm())
	}
	s := out.String()
	if !strings.Contains(s, cfg.AgeRecipient) || !strings.Contains(s, "1 older key(s) kept") || !strings.Contains(s, "AGE-SECRET-KEY-1") {
		t.Errorf("output:\n%s", s)
	}
	if strings.Contains(s, old.Identity) {
		t.Error("old private key must not be printed")
	}
	// Old backups (old key) and new ones both decrypt with the key file.
	for _, r := range []string{old.Recipient, cfg.AgeRecipient} {
		if err := roundTrip(r, ids); err != nil {
			t.Errorf("round trip %s: %v", r, err)
		}
	}
}

func TestRotateKeyDeclineAndGuards(t *testing.T) {
	cfgPath, keyPath, old := rotateFixture(t)
	var out, errb bytes.Buffer
	// Answer n: nothing is written.
	if code := runRotateKey(context.Background(), []string{"-c", cfgPath}, strings.NewReader("n\n"), &out, &errb); code != exitError {
		t.Errorf("decline: code=%d", code)
	}
	if _, recips, _ := crypto.ReadIdentities(keyPath); len(recips) != 1 {
		t.Error("declined rotation must not touch the key file")
	}
	cfg, _ := config.Load(cfgPath)
	if cfg.AgeRecipient != old.Recipient {
		t.Error("declined rotation must not touch app.yaml")
	}
	// encryption: none → exit 2.
	cfg.Encryption, cfg.AgeRecipient, cfg.AgeIdentityFile = "none", "", ""
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	errb.Reset()
	if code := runRotateKey(context.Background(), []string{"-c", cfgPath, "--yes"}, strings.NewReader(""), &out, &errb); code != exitUsage || !strings.Contains(errb.String(), "not enabled") {
		t.Errorf("none: code=%d err=%s", code, errb.String())
	}
	// stdin ends while asking → exit 2 with a hint about --yes.
	cfg.Encryption, cfg.AgeRecipient, cfg.AgeIdentityFile = "age", old.Recipient, keyPath
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	errb.Reset()
	if code := runRotateKey(context.Background(), []string{"-c", cfgPath}, strings.NewReader(""), &out, &errb); code != exitUsage || !strings.Contains(errb.String(), "--yes") {
		t.Errorf("no input: code=%d err=%s", code, errb.String())
	}
	// Extra argument → exit 2.
	if code := runRotateKey(context.Background(), []string{"-c", cfgPath, "extra"}, strings.NewReader(""), &out, &errb); code != exitUsage {
		t.Errorf("extra arg: code=%d", code)
	}
}

func TestRotateKeyMissingFileAndJSON(t *testing.T) {
	cfgPath, keyPath, old := rotateFixture(t)
	os.Remove(keyPath)
	var out, errb bytes.Buffer
	// Without --yes, answer n to the question about creating a new file.
	if code := runRotateKey(context.Background(), []string{"-c", cfgPath}, strings.NewReader("n\n"), &out, &errb); code != exitError {
		t.Errorf("code=%d", code)
	}
	if _, err := os.Stat(keyPath); err == nil {
		t.Error("declined: key file must not be created")
	}
	errb.Reset()
	out.Reset()
	code := runRotateKey(context.Background(), []string{"-c", cfgPath, "--yes", "--json"}, strings.NewReader(""), &out, &errb)
	if code != exitOK {
		t.Fatalf("code=%d err=%s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "is missing") {
		t.Errorf("missing-file warning expected on stderr: %s", errb.String())
	}
	var res rotateResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("stdout must be JSON only: %v\n%s", err, out.String())
	}
	if res.Kind != "rotate-key" || !res.OK || len(res.OldRecipients) != 0 || res.NewRecipient == old.Recipient {
		t.Errorf("%+v", res)
	}
	if strings.Contains(out.String(), "AGE-SECRET-KEY") {
		t.Error("private key must never be in JSON")
	}
	if !strings.Contains(errb.String(), "AGE-SECRET-KEY-1") {
		t.Error("with --json the private key goes to stderr once")
	}
	if _, recips, err := crypto.ReadIdentities(keyPath); err != nil || len(recips) != 1 || recips[0] != res.NewRecipient {
		t.Errorf("new key file: %v %v", recips, err)
	}
	if st, _ := os.Stat(keyPath); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
}

func TestWriteAtomicKeepsMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.yaml")
	if err := os.WriteFile(p, []byte("app: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.App{App: "shop", Instances: []config.Instance{{Name: "main", Container: "db", User: "postgres", Databases: config.Databases{All: true}}}}
	if err := config.Save(p, cfg); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("Save must keep the existing mode, got %v", st.Mode().Perm())
	}
}
