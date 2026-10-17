package pg

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
)

// DiskFreer is a Runner that can measure free space on the Postgres server's
// own filesystem (spec fetch-restore: disk check before restore). Only runners
// that run on the same machine/container as the server have it: docker exec,
// kubectl exec, host binaries. An ImageRunner to another host/cloud cannot measure it.
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

// DiskFree: `df` inside the pod.
func (r KubeExecRunner) DiskFree(ctx context.Context, dir string) (int64, error) {
	var out bytes.Buffer
	if err := r.Run(ctx, Conn{}, "df", dfArgs(dir), nil, &out); err != nil {
		return 0, err
	}
	return ParseDF(out.Bytes())
}

// DiskFree: `df` on the host (not via the Postgres binary directory).
func (r LocalRunner) DiskFree(ctx context.Context, dir string) (int64, error) {
	var out bytes.Buffer
	if _, err := r.Shell.Run(ctx, Cmd{Name: "df", Args: dfArgs(dir), Stdout: &out}); err != nil {
		return 0, err
	}
	return ParseDF(out.Bytes())
}
