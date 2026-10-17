package pg

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// LocalTools is the Postgres tool set installed on the host (pg_dump, pg_dumpall,
// pg_restore, psql in the same directory). → ADR-0009
type LocalTools struct {
	Dir     string // directory holding the binaries
	Version string // "17.11"
	Major   int
}

// localBinGlobs are the places where Postgres packages tend to put binaries without adding them to PATH.
var localBinGlobs = []string{
	"/usr/lib/postgresql/*/bin",          // Debian, Ubuntu
	"/usr/pgsql-*/bin",                   // RHEL, PGDG
	"/opt/homebrew/opt/postgresql@*/bin", // macOS Apple Silicon
	"/opt/homebrew/opt/libpq/bin",
	"/usr/local/opt/postgresql@*/bin", // macOS Intel
	"/usr/local/opt/libpq/bin",
}

// LocalFinder looks for pg_dump on the host: PATH first, then the usual
// directories; with several installs it takes the highest major. LookPath and
// Glob can be swapped in tests.
type LocalFinder struct {
	Shell    Shell
	LookPath func(string) (string, error)
	Glob     func(string) ([]string, error)
}

// Find returns (nil, nil) when the host has no working pg_dump.
func (f LocalFinder) Find(ctx context.Context) (*LocalTools, error) {
	lookPath, glob := f.LookPath, f.Glob
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if glob == nil {
		glob = filepath.Glob
	}
	var dirs []string
	if p, err := lookPath("pg_dump"); err == nil && p != "" {
		dirs = append(dirs, filepath.Dir(p))
	}
	for _, g := range localBinGlobs {
		matches, _ := glob(g)
		for _, m := range matches {
			if _, err := lookPath(filepath.Join(m, "pg_dump")); err == nil {
				dirs = append(dirs, m)
			}
		}
	}
	var best *LocalTools
	for _, dir := range dirs {
		if best != nil && best.Dir == dir {
			continue
		}
		ver, major, err := f.version(ctx, filepath.Join(dir, "pg_dump"))
		if err != nil {
			continue
		}
		if best == nil || major > best.Major {
			best = &LocalTools{Dir: dir, Version: ver, Major: major}
		}
	}
	return best, nil
}

var reVersion = regexp.MustCompile(`\(PostgreSQL\) (\d+)(?:\.(\d+))?`)

// version runs `<path> --version`: "pg_dump (PostgreSQL) 17.11 (Debian …)".
func (f LocalFinder) version(ctx context.Context, path string) (string, int, error) {
	res, err := f.Shell.Run(ctx, Cmd{Name: path, Args: []string{"--version"}})
	if err != nil {
		return "", 0, err
	}
	ver, major, ok := ParseToolVersion(string(res.Stdout))
	if !ok {
		return "", 0, fmt.Errorf("unexpected --version output: %q", strings.TrimSpace(string(res.Stdout)))
	}
	return ver, major, nil
}

// ParseToolVersion parses the `--version` output of a Postgres tool.
func ParseToolVersion(out string) (version string, major int, ok bool) {
	m := reVersion.FindStringSubmatch(out)
	if m == nil {
		return "", 0, false
	}
	major, _ = strconv.Atoi(m[1])
	version = m[1]
	if m[2] != "" {
		version += "." + m[2]
	}
	return version, major, true
}

// LocalRunner runs the host's Postgres binaries. Connection info (including
// PGPASSWORD) goes into the child's env, not argv. Unix socket connections
// work: Conn.Host is the socket directory.
type LocalRunner struct {
	Shell Shell
	Tools LocalTools
}

func (r LocalRunner) Describe() string {
	return fmt.Sprintf("host pg_dump %s (%s)", r.Tools.Version, r.Tools.Dir)
}

func (r LocalRunner) Run(ctx context.Context, conn Conn, tool string, args []string, stdin io.Reader, stdout io.Writer) error {
	_, err := r.RunStderr(ctx, conn, tool, args, stdin, stdout)
	return err
}

// RunStderr is like Run but also returns stderr even when the command succeeds.
func (r LocalRunner) RunStderr(ctx context.Context, conn Conn, tool string, args []string, stdin io.Reader, stdout io.Writer) ([]byte, error) {
	name := tool
	if r.Tools.Dir != "" {
		name = filepath.Join(r.Tools.Dir, tool)
	}
	res, err := r.Shell.Run(ctx, Cmd{Name: name, Args: args, Env: conn.Env(), Stdin: stdin, Stdout: stdout})
	return res.Stderr, err
}
