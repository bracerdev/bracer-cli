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

func TestLoadSecretsPrecedence(t *testing.T) {
	dir := t.TempDir()
	sf := filepath.Join(dir, "s.env")
	ef := filepath.Join(dir, "e.env")
	os.WriteFile(sf, []byte("ONLY_S=s\nBOTH=s\nALL3=s\n"), 0o600)
	os.WriteFile(ef, []byte("BOTH=e\nALL3=e\n"), 0o600)
	t.Setenv("ALL3", "env")
	a := &App{SecretsFile: sf}
	s, warns, err := LoadSecrets(a, ef)
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}
	for k, want := range map[string]string{"ONLY_S": "s", "BOTH": "e", "ALL3": "env"} {
		if v, _ := s.Lookup(k); v != want {
			t.Errorf("%s = %q, want %q (from %s)", k, v, want, s.Source[k])
		}
	}
	if _, err := s.Require("NOPE"); err == nil || !strings.Contains(err.Error(), "NOPE") {
		t.Errorf("Require must name the missing variable: %v", err)
	}
}

func TestLoadSecretsWarnsOnLoosePerm(t *testing.T) {
	dir := t.TempDir()
	sf := filepath.Join(dir, "s.env")
	os.WriteFile(sf, []byte("A=1\n"), 0o644)
	_, warns, err := LoadSecrets(&App{SecretsFile: sf}, "")
	if err != nil || len(warns) != 1 || !strings.Contains(warns[0], "600") {
		t.Errorf("want one permission warning, got %v %v", warns, err)
	}
}

func TestMergeEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "x.env")
	kept, err := MergeEnvFile(path, map[string]string{"B": "2", "A": "with space"}, nil)
	if err != nil || len(kept) != 0 {
		t.Fatal(err, kept)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o", st.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if string(data) != "A='with space'\nB=2\n" {
		t.Errorf("content:\n%s", data)
	}
	os.WriteFile(path, append([]byte("# keep this comment\n"), data...), 0o600)
	kept, err = MergeEnvFile(path, map[string]string{"A": "new", "B": "new", "C": "3"}, func(n string) bool { return n == "B" })
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0] != "A" {
		t.Errorf("kept = %v", kept)
	}
	data, _ = os.ReadFile(path)
	if string(data) != "# keep this comment\nA='with space'\nB=new\nC=3\n" {
		t.Errorf("content after merge:\n%s", data)
	}
}
