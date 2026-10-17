package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config is the storage: section of app.yaml after secrets have been loaded.
type S3Config struct {
	Endpoint  string // full URL, https://… or http://… for an internal MinIO
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	PathStyle bool
}

// S3 is a Store on top of minio-go.
type S3 struct {
	cfg    S3Config
	client *minio.Client
}

// NewS3 builds the client; it makes no network calls.
func NewS3(cfg S3Config) (*S3, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("storage.endpoint must be a full URL, e.g. https://s3.example.com")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("storage.endpoint: only http or https")
	}
	lookup := minio.BucketLookupAuto
	if cfg.PathStyle {
		lookup = minio.BucketLookupPath
	}
	c, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       u.Scheme == "https",
		Region:       cfg.Region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, err
	}
	return &S3{cfg: cfg, client: c}, nil
}

func (s *S3) Describe() string {
	return fmt.Sprintf("s3 %s bucket %s", s.cfg.Endpoint, s.cfg.Bucket)
}

func (s *S3) Check(ctx context.Context) error {
	ok, err := s.client.BucketExists(ctx, s.cfg.Bucket)
	if err != nil {
		return fmt.Errorf("HEAD bucket %s: %w", s.cfg.Bucket, err)
	}
	if !ok {
		return fmt.Errorf("bucket %s does not exist or access is denied", s.cfg.Bucket)
	}
	// list one object to make sure we have ListBucket permission (retention needs it).
	for obj := range s.client.ListObjects(ctx, s.cfg.Bucket, minio.ListObjectsOptions{MaxKeys: 1}) {
		if obj.Err != nil {
			return fmt.Errorf("list bucket %s: %w", s.cfg.Bucket, obj.Err)
		}
		break
	}
	return nil
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	_, err := s.client.PutObject(ctx, s.cfg.Bucket, key, r, size, minio.PutObjectOptions{
		ContentType: "application/octet-stream",
		PartSize:    64 << 20,
	})
	if err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.cfg.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	// GetObject is lazy; Stat so a missing object is reported right away.
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		var resp minio.ErrorResponse
		if errors.As(err, &resp) && resp.Code == "NoSuchKey" {
			return nil, fmt.Errorf("%s: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	return obj, nil
}

func (s *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	for obj := range s.client.ListObjects(ctx, s.cfg.Bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, obj.Err)
		}
		out = append(out, Object{Key: obj.Key, Size: obj.Size, LastModified: obj.LastModified})
	}
	return out, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.cfg.Bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}

// Join concatenates object path parts, dropping extra `/`; empty parts are skipped.
func Join(parts ...string) string {
	var out []string
	for _, p := range parts {
		p = strings.Trim(p, "/")
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "/")
}
