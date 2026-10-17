package backup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ErrLocked: another bracer process of the same kind is running with the same app.yaml.
var ErrLocked = errors.New("another bracer process is running for this app.yaml")

// Lock is the lock file next to app.yaml, holding the PID. A stale lock of a dead
// process is taken over.
type Lock struct{ path string }

// Acquire creates `.<app.yaml>.lock` for backup.
func Acquire(configPath string) (*Lock, error) {
	return AcquireKind(configPath, "backup")
}

// AcquireKind creates `.<app.yaml>.lock` (kind backup) or `.<app.yaml>.<kind>.lock`.
// backup and drill do not lock each other out (spec drill § Edge cases).
func AcquireKind(configPath, kind string) (*Lock, error) {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return nil, err
	}
	suffix := ".lock"
	if kind != "backup" {
		suffix = "." + kind + ".lock"
	}
	l := &Lock{path: filepath.Join(filepath.Dir(abs), "."+filepath.Base(abs)+suffix)}
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return l, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		data, rerr := os.ReadFile(l.path)
		if rerr != nil {
			return nil, ErrLocked
		}
		pid, perr := strconv.Atoi(strings.TrimSpace(string(data)))
		if perr == nil && processAlive(pid) {
			return nil, fmt.Errorf("another bracer %s is running for this app.yaml (pid %d, %s): %w", kind, pid, l.path, ErrLocked)
		}
		// Orphan lock: remove it and retry once.
		_ = os.Remove(l.path)
	}
	return nil, ErrLocked
}

// Release removes the lock file.
func (l *Lock) Release() {
	if l != nil {
		_ = os.Remove(l.path)
	}
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
