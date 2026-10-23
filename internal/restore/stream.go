// Package restore is the part shared by drill and restore: opening a dump
// stream (bucket or file → [age]), restoring globals/database through a Runner,
// comparing tables against the manifest (spec backup/drill, backup/restore).
package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"strings"

	"filippo.io/age"

	"bracer.dev/bracer/internal/backup"
	"bracer.dev/bracer/internal/crypto"
	"bracer.dev/bracer/internal/storage"
)

// Identities holds the age keys that were read; Err records why they could not be read.
type Identities struct {
	IDs        []age.Identity
	Recipients []string
	Err        error
}

// ReadIdentities reads the age key file through read; empty path → Err.
func ReadIdentities(path string, read func(string) ([]age.Identity, []string, error)) *Identities {
	ids := &Identities{}
	if path == "" {
		ids.Err = fmt.Errorf("no age key file given")
		return ids
	}
	ids.IDs, ids.Recipients, ids.Err = read(path)
	return ids
}

// Has reports whether there is a key for recipient (empty recipient: any key will do).
func (ids *Identities) Has(recipient string) bool {
	if recipient == "" {
		return len(ids.IDs) > 0
	}
	for _, r := range ids.Recipients {
		if r == recipient {
			return true
		}
	}
	return false
}

// Decrypt wraps r with age using every key; the error names the recipient that is needed.
func (ids *Identities) Decrypt(r io.Reader, recipient string) (io.Reader, error) {
	if ids == nil || ids.Err != nil {
		var err error = fmt.Errorf("no age key")
		if ids != nil {
			err = ids.Err
		}
		return nil, fmt.Errorf("dump is encrypted for %s but the age key cannot be read: %v", orUnknown(recipient), err)
	}
	if recipient != "" && !ids.Has(recipient) {
		return nil, fmt.Errorf("dump is encrypted for %s but age_identity_file has no matching key (keys for: %s)", recipient, strings.Join(ids.Recipients, ", "))
	}
	plain, err := crypto.Decrypt(r, ids.IDs)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plain, nil
}

func orUnknown(s string) string {
	if s == "" {
		return "an unknown recipient"
	}
	return s
}

// readCloser pairs the decrypted reader with the source's closer.
type readCloser struct {
	io.Reader
	io.Closer
}

// ChecksumErr passes the source's SHA-256 error (shaReader) through the decryption layer.
func (rc readCloser) ChecksumErr() error {
	if c, ok := rc.Closer.(interface{ ChecksumErr() error }); ok {
		return c.ChecksumErr()
	}
	return nil
}

// Finish passes shaReader.Finish through the decryption layer.
func (rc readCloser) Finish() error { return finish(rc.Closer) }

// finish reads what is left of the raw object and returns the SHA-256
// result; nil when the stream has no checksum to check.
func finish(r interface{}) error {
	if f, ok := r.(interface{ Finish() error }); ok {
		return f.Finish()
	}
	return nil
}

// OpenDump opens a stream of one dump in the bucket: Store.Get → [age]. Touches no disk.
func OpenDump(ctx context.Context, st storage.Store, dp *backup.Dump, ids *Identities) (io.ReadCloser, error) {
	raw, err := st.Get(ctx, dp.Key)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", dp.Key, err)
	}
	// SHA-256 is computed over the raw object while streaming; a manifest mismatch → Read
	// returns an error instead of EOF, so the downstream pg_restore/psql sees a failed stdin and stops.
	var rc io.ReadCloser = raw
	if dp.SHA256 != "" {
		rc = &shaReader{r: raw, h: sha256.New(), want: dp.SHA256}
	}
	if !dp.Encrypted {
		return rc, nil
	}
	recipient := ""
	if dp.Recipient != nil {
		recipient = *dp.Recipient
	}
	plain, err := ids.Decrypt(rc, recipient)
	if err != nil {
		rc.Close()
		return nil, err
	}
	return readCloser{plain, rc}, nil
}

// shaReader hashes the data passing through and compares it with the manifest at end of stream.
type shaReader struct {
	r    io.ReadCloser
	h    hash.Hash
	want string
	done bool
	err  error // *ErrChecksum if a mismatch was found
}

func (s *shaReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.h.Write(p[:n])
	if err == io.EOF && !s.done {
		s.done = true
		if got := hex.EncodeToString(s.h.Sum(nil)); got != s.want {
			s.err = &ErrChecksum{Want: s.want, Got: got}
			return n, s.err
		}
	}
	return n, err
}

func (s *shaReader) Close() error { return s.r.Close() }

// ChecksumErr returns the SHA-256 mismatch error if the stream was read to the end and did not match.
func (s *shaReader) ChecksumErr() error { return s.err }

// Finish reads the raw object to its end (age stops at its last chunk, which
// may leave bytes unread) and returns the mismatch, or a read error.
func (s *shaReader) Finish() error {
	if _, err := io.Copy(io.Discard, s); err != nil && s.err == nil {
		return err
	}
	return s.err
}

const ageHeader = "age-encryption.org/v1"

// ErrChecksum: the downloaded object does not match the sha256 in the manifest.
type ErrChecksum struct{ Want, Got string }

func (e *ErrChecksum) Error() string {
	return fmt.Sprintf("checksum mismatch: manifest %s, downloaded %s", e.Want, e.Got)
}
