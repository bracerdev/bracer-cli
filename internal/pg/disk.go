package pg

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
)

// DiskFreer is a Runner that can measure free space on the Postgres server's
// own filesystem (spec restore § disk): docker exec into the server's
// container, or df on this machine for a local cluster. An ImageRunner (URL)
// cannot measure it.
type DiskFreer interface {
	DiskFree(ctx context.Context, dir string) (int64, error)
}

// ParseDF parses the output of `df -Pk <dir>` (POSIX): line 2, column 4 = free KB.
func ParseDF(out []byte) (int64, error) {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("unexpected df output")
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 4 {
		return 0, fmt.Errorf("unexpected df output")
	}
	kb, err := strconv.ParseInt(f[3], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unexpected df output: %v", err)
	}
	return kb * 1024, nil
}

func dfArgs(dir string) []string { return []string{"-Pk", dir} }

// DiskFree: `df` inside the Postgres container.
func (r ExecRunner) DiskFree(ctx context.Context, dir string) (int64, error) {
	var out bytes.Buffer
	if err := r.Run(ctx, Conn{}, "df", dfArgs(dir), nil, &out); err != nil {
		return 0, err
	}
	return ParseDF(out.Bytes())
}

// DatabasesSize is the sum of pg_database_size of the databases names on
// the connected server: about what a restore of them writes.
func (c *Client) DatabasesSize(ctx context.Context, names []string) (int64, error) {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, QuoteLiteral(n))
	}
	rows, err := c.Query(ctx, "select coalesce(sum(pg_database_size(datname)), 0) from pg_database where datname in ("+strings.Join(quoted, ",")+")")
	if err != nil {
		return 0, err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return 0, fmt.Errorf("unexpected answer to pg_database_size: %q", rows)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(rows[0][0]), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unexpected answer to pg_database_size: %v", err)
	}
	return n, nil
}

// DiskFree: `df` on this machine, where the local cluster's data directory is.
func (r SocketRunner) DiskFree(ctx context.Context, dir string) (int64, error) {
	res, err := r.Shell.Run(ctx, Cmd{Name: "df", Args: dfArgs(dir)})
	if err != nil {
		return 0, err
	}
	return ParseDF(res.Stdout)
}
