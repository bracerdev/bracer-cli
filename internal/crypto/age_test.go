package crypto

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateWriteReadRoundTrip(t *testing.T) {
	kp, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(kp.Recipient, "age1") || !strings.HasPrefix(kp.Identity, "AGE-SECRET-KEY-1") {
		t.Fatalf("key format: %+v", kp)
	}
	if r, err := ParseRecipient(" " + kp.Recipient + "\n"); err != nil || r != kp.Recipient {
		t.Errorf("ParseRecipient: %s %v", r, err)
	}
	if _, err := ParseRecipient("age1xyz"); err == nil {
		t.Error("a broken public key must be rejected")
	}

	path := filepath.Join(t.TempDir(), "sub", "shop.key")
	if err := WriteIdentityFile(path, kp); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %o", st.Mode().Perm())
	}
	if err := WriteIdentityFile(path, kp); !errors.Is(err, ErrExists) {
		t.Errorf("overwrite must be refused: %v", err)
	}
	kp2, _ := Generate()
	if err := AppendIdentity(path, kp2); err != nil {
		t.Fatal(err)
	}
	ids, recips, err := ReadIdentities(path)
	if err != nil || len(ids) != 2 || recips[0] != kp.Recipient || recips[1] != kp2.Recipient {
		t.Fatalf("ReadIdentities: %v %v %v", len(ids), recips, err)
	}

	// Encrypt with the old key, decrypt with the rotated file (which keeps both keys).
	var buf bytes.Buffer
	w, err := Encrypt(&buf, kp.Recipient)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "pg_dump -Fc payload")
	w.Close()
	r, err := Decrypt(&buf, ids)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	if string(got) != "pg_dump -Fc payload" {
		t.Errorf("decrypted = %q", got)
	}
}
