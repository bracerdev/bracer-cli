package backup

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// BR-013: a lock file left by a run killed without cleanup holds a PID that
// may now be any live process (after a reboot); nobody holds the flock, so
// the next run takes it.
func TestBR013LockIgnoresLivePIDInFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "app.yaml")
	lockPath := filepath.Join(dir, ".app.yaml.lock")
	if err := os.WriteFile(lockPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Acquire(cfgPath)
	if err != nil {
		t.Fatalf("a lock file nobody holds must not block: %v", err)
	}
	l.Release()
}

// BR-013: a lock file this user cannot write (left by a run as root, 0644)
// still locks: flock needs no write permission.
func TestBR013LockOnReadOnlyLeftover(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes any file")
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "app.yaml")
	lockPath := filepath.Join(dir, ".app.yaml.lock")
	if err := os.WriteFile(lockPath, nil, 0o444); err != nil {
		t.Fatal(err)
	}
	l, err := Acquire(cfgPath)
	if err != nil {
		t.Fatalf("a read-only leftover lock file must not block: %v", err)
	}
	if _, err := Acquire(cfgPath); !errors.Is(err, ErrLocked) {
		t.Errorf("second acquire: %v", err)
	}
	l.Release()
}

// BR-013: the holder is the flock, not the file's content: a second starter
// finds the empty file held and is told which file; release leaves the file.
func TestBR013LockHeldWhateverTheFileSays(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "app.yaml")
	lockPath := filepath.Join(dir, ".app.yaml.lock")
	l, err := Acquire(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(lockPath); len(data) != 0 {
		t.Errorf("the lock file must stay empty: %q", data)
	}
	_, err = Acquire(cfgPath)
	if !errors.Is(err, ErrLocked) || !strings.Contains(err.Error(), "a backup is running for this app.yaml ("+lockPath+")") {
		t.Errorf("an empty lock file of a live holder was taken over: %v", err)
	}
	l.Release()
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf("release must leave the file: %v", err)
	}
	l2, err := Acquire(cfgPath)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	l2.Release()
}
