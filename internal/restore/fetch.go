package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/storage"
)

// ErrChecksum: the downloaded object does not match the sha256 in the manifest.
type ErrChecksum struct{ Want, Got string }

func (e *ErrChecksum) Error() string {
	return fmt.Sprintf("checksum mismatch: manifest %s, downloaded %s", e.Want, e.Got)
}

// Fetch downloads one dump into w: Store.Get → sha256 over the raw object → [age unless raw]
// → w. Returns the number of bytes written; a sha mismatch → *ErrChecksum after writing (the
// caller deletes the file). Touches no disk other than w.
func Fetch(ctx context.Context, st storage.Store, dp *backup.Dump, ids *Identities, raw bool, w io.Writer) (int64, error) {
	rc, err := st.Get(ctx, dp.Key)
	if err != nil {
		return 0, fmt.Errorf("download %s: %w", dp.Key, err)
	}
	defer rc.Close()
	hasher := sha256.New()
	var src io.Reader = io.TeeReader(rc, hasher)
	if dp.Encrypted && !raw {
		recipient := ""
		if dp.Recipient != nil {
			recipient = *dp.Recipient
		}
		if src, err = ids.Decrypt(src, recipient); err != nil {
			return 0, err
		}
	}
	n, err := io.Copy(w, src)
	if err != nil {
		return n, err
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); dp.SHA256 != "" && got != dp.SHA256 {
		return n, &ErrChecksum{Want: dp.SHA256, Got: got}
	}
	return n, nil
}
