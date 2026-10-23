package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Memory is a Store held in memory: the run preflight builds and drills
// without writing the customer's bucket (ADR-0019).
type Memory struct {
	// MaxBytes caps each object; 0 means no cap.
	MaxBytes int64

	mu   sync.Mutex
	objs map[string][]byte
}

// NewMemory returns an empty Memory store whose objects hold at most maxBytes.
func NewMemory(maxBytes int64) *Memory {
	return &Memory{MaxBytes: maxBytes, objs: map[string][]byte{}}
}

func (m *Memory) Check(ctx context.Context) error { return nil }

func (m *Memory) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	src := r
	if m.MaxBytes > 0 {
		src = io.LimitReader(r, m.MaxBytes+1)
	}
	data, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	if m.MaxBytes > 0 && int64(len(data)) > m.MaxBytes {
		// Drain the writer so it does not block on a full pipe.
		_, _ = io.Copy(io.Discard, r)
		return fmt.Errorf("%s is larger than %d MB", key, m.MaxBytes>>20)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = data
	return nil
}

func (m *Memory) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objs[key]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *Memory) List(ctx context.Context, prefix string) ([]Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Object
	for k, v := range m.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, Object{Key: k, Size: int64(len(v)), LastModified: time.Time{}})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (m *Memory) Delete(ctx context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	return nil
}

func (m *Memory) Describe() string { return "memory" }
