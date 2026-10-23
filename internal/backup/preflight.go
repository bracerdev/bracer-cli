package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"bracer.dev/bracer/internal/config"
	"bracer.dev/bracer/internal/crypto"
	"bracer.dev/bracer/internal/storage"
)

// Preflight checks everything a backup needs except the data (ADR-0019):
// the access key can write, read and delete, the age key round-trips, and a
// schema-only run (SchemaOnly) dumps every database. The drill of that run is
// left to the caller, since drill depends on this package.

// ErrNoDelete: the access key writes but cannot delete, so retention could
// never remove an old backup.
var ErrNoDelete = errors.New("the access key cannot delete objects: retention cannot remove old backups")

// Probe writes, reads back and deletes the probe object.
func Probe(ctx context.Context, st storage.Store, key string, now time.Time) error {
	body := []byte("bracer preflight " + now.UTC().Format(time.RFC3339Nano) + "\n")
	if err := st.Put(ctx, key, bytes.NewReader(body), int64(len(body))); err != nil {
		return fmt.Errorf("write %s: %w", key, err)
	}
	rc, err := st.Get(ctx, key)
	if err != nil {
		_ = st.Delete(ctx, key)
		return fmt.Errorf("read back %s: %w", key, err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, body) {
		_ = st.Delete(ctx, key)
		if err == nil {
			err = errors.New("the content differs from what was written")
		}
		return fmt.Errorf("read back %s: %w", key, err)
	}
	if err := st.Delete(ctx, key); err != nil {
		return fmt.Errorf("%w (%s: %v)", ErrNoDelete, key, firstLine(err.Error()))
	}
	return nil
}

// AgeRoundTrip encrypts a sample to age_recipient and decrypts it back with
// age_identity_file.
func AgeRoundTrip(cfg *config.App) error {
	sample := []byte("bracer preflight")
	var buf bytes.Buffer
	enc, err := crypto.Encrypt(&buf, cfg.AgeRecipient)
	if err != nil {
		return fmt.Errorf("encrypt to %s: %w", cfg.AgeRecipient, err)
	}
	if _, err := enc.Write(sample); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	if msg := keyMismatch(cfg); msg != "" {
		return errors.New(msg)
	}
	ids, _, err := crypto.ReadIdentities(cfg.AgeIdentityFile)
	if err != nil {
		return err
	}
	r, err := crypto.Decrypt(&buf, ids)
	if err != nil {
		return fmt.Errorf("decrypt with %s: %w", cfg.AgeIdentityFile, err)
	}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, sample) {
		return fmt.Errorf("decrypt with %s: the result differs from the sample", cfg.AgeIdentityFile)
	}
	return nil
}

// PreflightMaxBytes caps each object of the schema-only run.
const PreflightMaxBytes = 64 << 20
