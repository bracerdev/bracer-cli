package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseEnvFile(t *testing.T) {
	in := "# comment\n\nA=1\nexport B = two words\nC='quoted # not comment'\nD=\"dq\"\nE=\n"
	got, err := ParseEnvFile([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "1", "B": "two words", "C": "quoted # not comment", "D": "dq", "E": ""}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if _, err := ParseEnvFile([]byte("A=1\nno equals sign here\n")); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("want an error for line 2, got %v", err)
	}
}

// secrets_file is the one source: the process environment does not override it (ADR-0022).
func TestLoadSecretsOneSource(t *testing.T) {
	dir := t.TempDir()
	sf := filepath.Join(dir, "s.env")
	os.WriteFile(sf, []byte("A=file\nB=file\n"), 0o600)
	t.Setenv("A", "env")
	t.Setenv("C", "env")
	s, warns, err := LoadSecrets(&App{SecretsFile: sf})
	if err != nil || len(warns) != 0 {
		t.Fatal(err, warns)
	}
	if v, _ := s.Lookup("A"); v != "file" {
		t.Errorf("A = %q, want the value of secrets_file", v)
	}
	if _, ok := s.Lookup("C"); ok {
		t.Error("C comes from the environment only and must not be read")
	}
	if _, err := s.Require("NOPE"); err == nil || !strings.Contains(err.Error(), "NOPE not found in "+sf) {
		t.Errorf("Require must name the variable and the file: %v", err)
	}
	if _, _, err := LoadSecrets(&App{SecretsFile: filepath.Join(dir, "gone.env")}); err == nil {
		t.Error("a missing secrets_file must be an error")
	}
}

func TestLoadSecretsWarnsOnLoosePerm(t *testing.T) {
	dir := t.TempDir()
	sf := filepath.Join(dir, "s.env")
	os.WriteFile(sf, []byte("A=1\n"), 0o644)
	_, warns, err := LoadSecrets(&App{SecretsFile: sf})
	if err != nil || len(warns) != 1 || !strings.Contains(warns[0], "600") {
		t.Errorf("want one permission warning, got %v %v", warns, err)
	}
}

func TestWriteEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "x.env")
	if err := WriteEnvFile(path, map[string]string{"B": "2", "A": "with space"}); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o", st.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if string(data) != "A='with space'\nB=2\n" {
		t.Errorf("content:\n%s", data)
	}
	// A leftover file is replaced whole, no question asked (ADR-0030).
	os.WriteFile(path, append([]byte("# old\nOLD=1\n"), data...), 0o600)
	if err := WriteEnvFile(path, map[string]string{"A": "new", "C": "3"}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != "A=new\nC=3\n" {
		t.Errorf("content after rewrite:\n%s", data)
	}
}

// Every value comes back as written: a password may hold quotes, blanks,
// `#` or backslashes.
func TestEnvFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.env")
	vars := map[string]string{
		"PLAIN": "abc123",
		"EMPTY": "",
		"SPACE": "two words",
		"HASH":  "a#b",
		"DQ":    `say "hi"`,
		"SQ":    "it's",
		"BOTH":  `it's "x"`,
		"BS":    `back\slash'`,
		"ALL":   `a 'b' "c" \ #d`,
	}
	if err := WriteEnvFile(path, vars); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	got, err := ParseEnvFile(data)
	if err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	for k, v := range vars {
		if got[k] != v {
			t.Errorf("%s: wrote %q, read %q (file line %q)", k, v, got[k], lineOf(data, k))
		}
	}
}

func lineOf(data []byte, key string) string {
	for _, l := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(l, key+"=") {
			return l
		}
	}
	return ""
}
