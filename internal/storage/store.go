// Package storage is the customer's S3-compatible bucket behind a small interface,
// so tests need no network and swapping the library does not leak outward. → ADR-0005
package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

// Object is the metadata of one object in the bucket.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// Store is what backup/drill need from the bucket. Keys already include the prefix.
type Store interface {
	// Check verifies the bucket exists and is readable (HEAD bucket + list one object).
	Check(ctx context.Context) error
	// Put writes a stream; size = -1 if unknown (multipart).
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	List(ctx context.Context, prefix string) ([]Object, error)
	Delete(ctx context.Context, key string) error
	// Describe is a one-liner for --dry-run.
	Describe() string
}

// ErrNotFound: the object does not exist.
var ErrNotFound = errors.New("object not found")
