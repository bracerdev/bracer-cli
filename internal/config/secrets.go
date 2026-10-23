package config

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Secrets holds the variables of secrets_file, the one source of secrets of
// every command: the process environment does not override it and there is
// no --env-file (ADR-0022). Source records where each variable came from so
// --dry-run can print it.
type Secrets struct {
	values map[string]string
	Source map[string]string
	file   string
}

// NewSecrets builds Secrets from an existing map (tests, or values typed at a prompt).
func NewSecrets(values map[string]string, source string) *Secrets {
	s := &Secrets{values: map[string]string{}, Source: map[string]string{}, file: source}
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
		c.file = s.file
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

// Lookup returns the variable's value; ok=false if secrets_file does not have it.
func (s *Secrets) Lookup(name string) (string, bool) {
	v, ok := s.values[name]
	return v, ok
}

// Require is like Lookup but returns an error naming the variable and the file.
func (s *Secrets) Require(name string) (string, error) {
	v, ok := s.values[name]
	if !ok {
		where := "secrets_file"
		if s.file != "" {
			where = s.file
		}
		return "", fmt.Errorf("%w: variable %s not found in %s", ErrMissingSecret, name, where)
	}
	return v, nil
}

// LoadSecrets loads secrets_file of the app. A file that cannot be read is an
// error; a file with loose permissions is a warning.
func LoadSecrets(a *App) (*Secrets, []string, error) {
	path := a.SecretsFile
	s := &Secrets{values: map[string]string{}, Source: map[string]string{}, file: path}
	var warnings []string
	if w := permWarning(path); w != "" {
		warnings = append(warnings, w)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, warnings, fmt.Errorf("secrets_file: %w", err)
	}
	vars, err := ParseEnvFile(data)
	if err != nil {
		return nil, warnings, fmt.Errorf("secrets_file %s: %w", path, err)
	}
	for k, v := range vars {
		s.values[k] = v
		s.Source[k] = "secrets_file " + path
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
// A leading `#` is a comment. A value wrapped in single quotes is taken as is;
// one wrapped in double quotes has \\ and \" unescaped (what quoteIfNeeded
// writes). A malformed line is an error with its line number.
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
			q := v[0]
			v = v[1 : len(v)-1]
			if q == '"' {
				v = strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(v)
			}
		}
		out[k] = v
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// WriteEnvFile writes exactly vars to the dotenv file at path (mode 600,
// directory 700), replacing whatever was there: the file is init's alone,
// and a leftover of an interrupted init is overwritten without a question
// (ADR-0030).
func WriteEnvFile(path string, vars map[string]string) error {
	path, err := ExpandHome(path)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(vars))
	for k := range vars {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, k := range names {
		b.WriteString(k + "=" + quoteIfNeeded(vars[k]) + "\n")
	}
	if err := os.MkdirAll(dirOf(path), 0o700); err != nil {
		return err
	}
	return writeAtomic(path, []byte(b.String()), 0o600)
}

// quoteIfNeeded writes v so that ParseEnvFile reads it back unchanged: bare
// when it is plain, in single quotes (taken as is) when it has blanks, `#` or
// double quotes, in double quotes with \\ and \" escaped when it holds a
// single quote or a backslash.
func quoteIfNeeded(v string) string {
	switch {
	case strings.ContainsAny(v, `'\`):
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
	case v == "" || strings.ContainsAny(v, " #\"\t"):
		return "'" + v + "'"
	}
	return v
}

func dirOf(path string) string {
	if i := strings.LastIndex(path, "/"); i > 0 {
		return path[:i]
	}
	return "."
}
