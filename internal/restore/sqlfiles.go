package restore

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// SQL files are written for the customer to read and apply (ADR-0015):
// numbered, never overwritten, chmod 600 in a 700 directory because env-up
// may hold password hashes.

var reNumbered = regexp.MustCompile(`^(\d{2,})-`)

// nextNumber is one more than the highest NN- prefix in dir.
func nextNumber(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if m := reNumbered.FindStringSubmatch(e.Name()); m != nil {
			if v, _ := strconv.Atoi(m[1]); v > n {
				n = v
			}
		}
	}
	return n + 1, nil
}

// SQLFile is one file to write: its suffix (env-up, env-down, data-down), a
// header explaining it, and the statements.
type SQLFile struct {
	Suffix     string
	Header     []string
	Statements []string
}

// WriteSQLFiles writes the files under one new number in dir and returns their paths.
func WriteSQLFiles(dir string, files ...SQLFile) ([]string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	n, err := nextNumber(dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, f := range files {
		path := filepath.Join(dir, fmt.Sprintf("%02d-%s.sql", n, f.Suffix))
		var b strings.Builder
		for _, h := range f.Header {
			b.WriteString("-- " + h + "\n")
		}
		b.WriteString("\\set ON_ERROR_STOP on\n\n")
		for _, s := range f.Statements {
			b.WriteString(s + "\n")
		}
		fh, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return paths, err
		}
		if _, err := fh.WriteString(b.String()); err != nil {
			fh.Close()
			return paths, err
		}
		if err := fh.Close(); err != nil {
			return paths, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// SQLText is the file body without the header, for applying it directly (drill).
func SQLText(stmts []string) string {
	return "\\set ON_ERROR_STOP on\n" + strings.Join(stmts, "\n") + "\n"
}
