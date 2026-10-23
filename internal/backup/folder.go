package backup

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/storage"
)

// The folder of an app in the bucket belongs to one place: one machine and
// one app directory on it (ADR-0023). Only init's first real run starts it;
// backup writes to it only from that place, and every run.json
// records the writer.

// Writer is the place that writes a folder: an id derived from the machine
// and the app directory, with the hostname and path it was seen at, for
// display only.
type Writer struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	Path     string `json:"path"`
}

// MachineIDPath is read for the machine half of Writer.ID; tests point it elsewhere.
var MachineIDPath = "/etc/machine-id"

// Here is the Writer of the app directory of appPath (its app.yaml) for
// folder on this machine. The id is the first 16 hex characters of
// HMAC-SHA256(machine-id, "bracer:<folder>:<dir>"), so the machine id itself
// never reaches the bucket.
func Here(folder, appPath string) (Writer, error) {
	raw, err := os.ReadFile(MachineIDPath)
	if err != nil {
		return Writer{}, fmt.Errorf("read %s to tell this machine apart: %w", MachineIDPath, err)
	}
	mid := strings.TrimSpace(string(raw))
	if mid == "" {
		return Writer{}, fmt.Errorf("%s is empty: bracer cannot tell this machine apart", MachineIDPath)
	}
	dir, err := filepath.Abs(filepath.Dir(appPath))
	if err != nil {
		return Writer{}, err
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	mac := hmac.New(sha256.New, []byte("bracer:"+folder+":"+dir))
	mac.Write([]byte(mid))
	host, _ := os.Hostname()
	return Writer{ID: hex.EncodeToString(mac.Sum(nil))[:16], Hostname: host, Path: dir}, nil
}

// Root is bracer/<folder>/, the only place an app reads and writes (ADR-0027).
func Root(cfg *config.App) string {
	return FolderRoot(cfg.Storage.Folder)
}

// FolderRoot is bracer/<folder>/ for a folder name.
func FolderRoot(folder string) string {
	return storage.Join(config.BucketPrefix, folder) + "/"
}

// ProbeKey is the object preflight writes, reads and deletes to prove the
// access key can do all three (ADR-0019). It sits at the folder root, outside
// any run.
func ProbeKey(folder string) string {
	return FolderRoot(folder) + ".bracer-probe"
}

// ErrNoFolder: the app's folder in the bucket has no run that ended.
var ErrNoFolder = errors.New("no folder")

// fixRemove is the Fix of a folder this place must not write to.
func fixRemove(app string) string {
	return "Fix: bracer remove " + app + ", then bracer init " + app
}

// OpenFolder reads the newest run.json of the app's folder for a job that
// writes to it, before anything is written (ADR-0023). A folder with no run
// is an error unless create (init's first real run), which returns a nil
// run. A corrupt run.json, or one another place wrote, is an error: nothing
// may be written then. The listing of the folder comes back with it.
func OpenFolder(ctx context.Context, st storage.Store, cfg *config.App, here Writer, create bool) (*Run, []RunRef, error) {
	prev, refs, err := Newest(ctx, st, Root(cfg))
	switch {
	case errors.Is(err, ErrNoFolder) && create:
		return nil, refs, nil
	case errors.Is(err, ErrNoFolder):
		return nil, nil, fmt.Errorf("the bucket has no folder %s: its backups are gone or the bucket settings point elsewhere\nFix: bracer remove %s, then bracer init", cfg.Storage.Folder, cfg.App)
	case err != nil:
		return nil, nil, fmt.Errorf("%w; nothing is written until it is readable\n%s", err, fixRemove(cfg.App))
	}
	if err := CheckWriter(prev, cfg, here); err != nil {
		return nil, nil, err
	}
	return prev, refs, nil
}

// CheckWriter fails when the run belongs to another place than here.
func CheckWriter(r *Run, cfg *config.App, here Writer) error {
	if r.Writer.ID == "" {
		return fmt.Errorf("folder %s has no writer in its run.json; bracer does not write to it\n%s", cfg.Storage.Folder, fixRemove(cfg.App))
	}
	if r.Writer.ID != here.ID {
		return fmt.Errorf("folder %s is written by %s:%s; bracer does not write to it from here\n%s", cfg.Storage.Folder, r.Writer.Hostname, r.Writer.Path, fixRemove(cfg.App))
	}
	return nil
}
