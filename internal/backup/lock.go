package backup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLocked: another bracer process is running with the same app.yaml.
var ErrLocked = errors.New("another bracer process is running for this app.yaml")

// Lock is the one lock next to app.yaml: backup holds it for the whole run
// (drill included), init's real run being one (ADR-0026). It is an flock(2)
// on `.<app.yaml>.lock`, which the kernel releases when the process dies, so
// a run killed without cleanup leaves nothing to take over. The file stays
// empty: it is opened read-only, since flock needs no write permission, so a
// file left by a run as another user (root) still locks.
type Lock struct{ f *os.File }

// Acquire takes the lock, or returns an error wrapping ErrLocked when another
// process holds it.
func Acquire(configPath string) (*Lock, error) {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(filepath.Dir(abs), "."+filepath.Base(abs)+".lock")
	// os.OpenFile sets O_CLOEXEC: docker and the other children never hold the lock.
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		return nil, fmt.Errorf("a backup is running for this app.yaml (%s): %w", path, ErrLocked)
	}
	return &Lock{f: f}, nil
}

// Release closes the file, which releases the lock. The file stays: removing
// it would let a starter that opened it before lock a file nobody else sees.
func (l *Lock) Release() {
	if l != nil {
		l.f.Close()
	}
}
