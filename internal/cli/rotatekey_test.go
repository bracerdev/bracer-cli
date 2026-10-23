package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/crypto"
)

// rotateFixture creates app shop with age encryption and its age.key in the test's home.
func rotateFixture(t *testing.T) (cfgPath, keyPath string, kp crypto.Keypair) {
	t.Helper()
	home := testHome(t)
	kp, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	keyPath = filepath.Join(config.AppDir(home, "shop"), config.KeyFile)
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := crypto.WriteIdentityFile(keyPath, kp); err != nil {
		t.Fatal(err)
	}
	cfg := testApp(home, "shop")
	cfg.Encryption, cfg.AgeRecipient, cfg.AgeIdentityFile = "age", kp.Recipient, keyPath
	return writeApp(t, cfg), keyPath, kp
}

func TestRotateKey(t *testing.T) {
	cfgPath, keyPath, old := rotateFixture(t)
	var out, errb bytes.Buffer
	code := runRotateKey(context.Background(), []string{"shop"}, strings.NewReader("y\nlater\ncopied\n"), &out, &errb)
	if code != exitOK {
		t.Fatalf("code=%d err=%s", code, errb.String())
	}
	s := out.String()
	if strings.Count(s, "Type copied when done") != 2 {
		t.Errorf("the new key must be confirmed copied (ADR-0022):\n%s", s)
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
	if !strings.Contains(s, "New public key:  "+cfg.AgeRecipient) || !strings.Contains(s, "1 older key(s) kept") {
		t.Errorf("output:\n%s", s)
	}
	// The private key is never printed: age.key is the rescue card (ADR-0022).
	if strings.Contains(s+errb.String(), "AGE-SECRET-KEY") {
		t.Errorf("a private key was printed:\n%s\n%s", s, errb.String())
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
	run := func(in string, args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := runRotateKey(context.Background(), args, strings.NewReader(in), &out, &errb)
		return code, errb.String()
	}
	untouched := func(what string) {
		t.Helper()
		if _, recips, _ := crypto.ReadIdentities(keyPath); len(recips) != 1 {
			t.Errorf("%s: the key file must not change", what)
		}
		if cfg, _ := config.Load(cfgPath); cfg == nil || cfg.AgeRecipient != old.Recipient {
			t.Errorf("%s: app.yaml must not change", what)
		}
	}
	// Answer n, or no answer: nothing is written.
	for _, in := range []string{"n\n", ""} {
		if code, err := run(in, "shop"); code != exitError || !strings.Contains(err, "not rotated") {
			t.Errorf("%q: code=%d err=%s", in, code, err)
		}
		untouched("declined")
	}
	// No flags, one app.
	for _, args := range [][]string{{"shop", "--yes"}, {"shop", "--json"}, {"shop", "extra"}, {}} {
		if code, _ := run("y\ncopied\n", args...); code != exitUsage {
			t.Errorf("%v: code=%d", args, code)
		}
		untouched("usage")
	}
	// stdin a file that is not a terminal: nobody can confirm the copy.
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("y\ncopied\n")
	f.Seek(0, 0)
	defer f.Close()
	var out, errb bytes.Buffer
	if code := runRotateKey(context.Background(), []string{"shop"}, f, &out, &errb); code != exitUsage || !strings.Contains(errb.String(), "run it in a terminal") {
		t.Errorf("not a terminal: code=%d err=%s", code, errb.String())
	}
	untouched("not a terminal")
	// encryption: none → exit 2.
	cfg, _ := config.Load(cfgPath)
	cfg.Encryption, cfg.AgeRecipient, cfg.AgeIdentityFile = "none", "", ""
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	if code, err := run("y\n", "shop"); code != exitUsage || !strings.Contains(err, "encryption is not enabled for this app\nFix: bracer remove shop, then bracer init shop") {
		t.Errorf("none: code=%d err=%s", code, err)
	}
}

// Rotating over a lost key would leave the old backups unopenable without
// anyone noticing (ADR-0022): a missing key file, or one without the current
// key, is refused before anything is asked or written.
func TestRotateKeyRefusesMissingKey(t *testing.T) {
	cfgPath, keyPath, old := rotateFixture(t)
	os.Remove(keyPath)
	var out, errb bytes.Buffer
	if code := runRotateKey(context.Background(), []string{"shop"}, strings.NewReader("y\ncopied\n"), &out, &errb); code != exitError || !strings.Contains(errb.String(), "is missing: backups made with "+old.Recipient+" cannot be opened without it") || !strings.Contains(errb.String(), "Fix: copy age.key back") {
		t.Errorf("missing: code=%d err=%s", code, errb.String())
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Error("a missing key file must not be created")
	}
	if strings.Contains(out.String(), "[y/N]") {
		t.Errorf("nothing may be asked:\n%s", out.String())
	}
	other, _ := crypto.Generate()
	if err := crypto.WriteIdentityFile(keyPath, other); err != nil {
		t.Fatal(err)
	}
	errb.Reset()
	if code := runRotateKey(context.Background(), []string{"shop"}, strings.NewReader("y\ncopied\n"), &out, &errb); code != exitError || !strings.Contains(errb.String(), "does not hold the key for "+old.Recipient) {
		t.Errorf("wrong key: code=%d err=%s", code, errb.String())
	}
	if _, recips, _ := crypto.ReadIdentities(keyPath); len(recips) != 1 {
		t.Error("the key file must not change")
	}
	if cfg, _ := config.Load(cfgPath); cfg.AgeRecipient != old.Recipient {
		t.Error("app.yaml must not change")
	}
}

// Input ending at the copy question: the new key is already in use, and the
// message says so.
func TestRotateKeyCopyNotConfirmed(t *testing.T) {
	cfgPath, _, old := rotateFixture(t)
	var out, errb bytes.Buffer
	if code := runRotateKey(context.Background(), []string{"shop"}, strings.NewReader("y\n"), &out, &errb); code != exitUsage || !strings.Contains(errb.String(), "the new key is in use: copy ") {
		t.Errorf("code=%d err=%s", code, errb.String())
	}
	if cfg, _ := config.Load(cfgPath); cfg.AgeRecipient == old.Recipient {
		t.Error("app.yaml must point at the new key once it is written")
	}
}

func TestWriteAtomicKeepsMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.yaml")
	if err := os.WriteFile(p, []byte("app: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(p, testApp(dir, "shop")); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("Save must keep the existing mode, got %v", st.Mode().Perm())
	}
}
