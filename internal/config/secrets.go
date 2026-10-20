package config

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Secrets is the result of loading three sources in precedence order: process
// environment > --env-file > secrets_file (conventions § Secret). Source records
// where each variable came from so --dry-run can print it.
type Secrets struct {
	values map[string]string
	Source map[string]string
}

// NewSecrets builds Secrets from an existing map (tests, or a source other than the three standard ones).
func NewSecrets(values map[string]string, source string) *Secrets {
	s := &Secrets{values: map[string]string{}, Source: map[string]string{}}
	for k, v := range values {
		s.values[k] = v
		s.Source[k] = source
	}
	return s
}

// With returns a copy that also holds name (a value typed at a prompt).
func (s *Secrets) With(name, value, source string) *Secrets {
	c := &Secrets{values: map[string]string{}, Source: map[string]string{}}
	if s != nil {
		for k, v := range s.values {
			c.values[k] = v
		}
		for k, v := range s.Source {
			c.Source[k] = v
		}
	}
	c.values[name], c.Source[name] = value, source
	return c
}

// Lookup returns the variable's value; ok=false if it is in none of the sources.
func (s *Secrets) Lookup(name string) (string, bool) {
	v, ok := s.values[name]
	return v, ok
}

// Require is like Lookup but returns an error naming the variable and the three ways to provide it.
func (s *Secrets) Require(name string) (string, error) {
	v, ok := s.values[name]
	if !ok {
		return "", fmt.Errorf("%w: variable %s not found in the environment, --env-file or secrets_file", ErrMissingSecret, name)
	}
	return v, nil
}

// LoadSecrets loads secrets_file (if the app declares one), then envFile (if
// given), then the process environment, each level overriding the previous. A
// declared file that cannot be read is an error.
func LoadSecrets(a *App, envFile string) (*Secrets, []string, error) {
	s := &Secrets{values: map[string]string{}, Source: map[string]string{}}
	var warnings []string
	load := func(path, label string) error {
		if path == "" {
			return nil
		}
		path, err := ExpandHome(path)
		if err != nil {
			return err
		}
		if w := permWarning(path); w != "" {
			warnings = append(warnings, w)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		vars, err := ParseEnvFile(data)
		if err != nil {
			return fmt.Errorf("%s %s: %w", label, path, err)
		}
		for k, v := range vars {
			s.values[k] = v
			s.Source[k] = label + " " + path
		}
		return nil
	}
	if a != nil {
		if err := load(a.SecretsFile, "secrets_file"); err != nil {
			return nil, warnings, err
		}
	}
	if err := load(envFile, "--env-file"); err != nil {
		return nil, warnings, err
	}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		s.values[k] = v
		s.Source[k] = "environment"
	}
	return s, warnings, nil
}

func permWarning(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Sprintf("%s has mode %o; should be 600", path, st.Mode().Perm())
	}
	return ""
}

// ParseEnvFile reads a dotenv file of one NAME=value per line, no interpolation.
// A leading `#` is a comment. A value wrapped in a pair of single or double
// quotes has the quotes removed. A malformed line is an error with its line number.
func ParseEnvFile(data []byte) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || !reEnvVar.MatchString(k) {
			return nil, fmt.Errorf("line %d: expected NAME=value", n)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// MergeEnvFile writes vars into the dotenv file at path (created if missing, mode
// 600). An existing variable with the same value is kept; one with a different
// value is overwritten only when overwrite(name) returns true, otherwise the old
// value stays and the name goes into kept. The file's existing lines (comments,
// order) are preserved; new variables are appended at the end.
func MergeEnvFile(path string, vars map[string]string, overwrite func(name string) bool) (kept []string, err error) {
	path, err = ExpandHome(path)
	if err != nil {
		return nil, err
	}
	var lines []string
	existing := map[string]string{}
	if data, rerr := os.ReadFile(path); rerr == nil {
		existing, err = ParseEnvFile(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		lines = strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		if len(lines) == 1 && lines[0] == "" {
			lines = nil
		}
	} else if !os.IsNotExist(rerr) {
		return nil, rerr
	}

	names := make([]string, 0, len(vars))
	for k := range vars {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		v := vars[k]
		old, has := existing[k]
		switch {
		case !has:
			lines = append(lines, k+"="+quoteIfNeeded(v))
		case old == v:
		case overwrite != nil && overwrite(k):
			for i, l := range lines {
				if key, _, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(l), "export "), "="); ok && strings.TrimSpace(key) == k {
					lines[i] = k + "=" + quoteIfNeeded(v)
				}
			}
		default:
			kept = append(kept, k)
		}
	}
	data := strings.Join(lines, "\n") + "\n"
	if err := os.MkdirAll(dirOf(path), 0o700); err != nil {
		return kept, err
	}
	return kept, writeAtomic(path, []byte(data), 0o600)
}

func quoteIfNeeded(v string) string {
	if v == "" || strings.ContainsAny(v, " #\"'\t") || strings.HasPrefix(v, "'") {
		return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
	}
	return v
}

func dirOf(path string) string {
	if i := strings.LastIndex(path, "/"); i > 0 {
		return path[:i]
	}
	return "."
}
