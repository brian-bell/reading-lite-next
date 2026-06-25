// Package blobs defines the content-blob storage port and an in-memory backend.
//
// Blobs holds the large content payloads referenced by a reading's keys: the raw
// fetched body and the extracted markdown. Metadata stays in the Store; only the
// bulky bytes live here. The production adapter is R2/S3 (Phase 6); [Memory] is
// the in-memory backend used by tests and zero-infra deployments.
package blobs

import (
	"bytes"
	"context"
	"errors"
	"sync"
)

// ErrNotFound reports that no blob exists for the requested key.
var ErrNotFound = errors.New("blob not found")

// Blobs stores opaque content blobs keyed by a server-derived key.
type Blobs interface {
	// Put stores data under key with contentType, overwriting any prior value.
	Put(ctx context.Context, key string, data []byte, contentType string) error
	// Get returns the stored bytes and content type, or ErrNotFound.
	Get(ctx context.Context, key string) ([]byte, string, error)
	// Delete removes key. Deleting an absent key is a no-op.
	Delete(ctx context.Context, key string) error
}

// blob is one stored payload.
type blob struct {
	data        []byte
	contentType string
}

// Memory is a concurrency-safe in-memory [Blobs] for tests and zero-infra deploys.
type Memory struct {
	mu      sync.RWMutex
	objects map[string]blob
}

// NewMemory returns an empty in-memory blob store.
func NewMemory() *Memory {
	return &Memory{objects: map[string]blob{}}
}

// Put stores a copy of data under key, overwriting any existing blob.
func (m *Memory) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.objects[key] = blob{data: bytes.Clone(data), contentType: contentType}
	return nil
}

// Get returns a copy of the stored bytes and content type, or ErrNotFound.
func (m *Memory) Get(ctx context.Context, key string) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	b, ok := m.objects[key]
	if !ok {
		return nil, "", ErrNotFound
	}
	return bytes.Clone(b.data), b.contentType, nil
}

// Delete removes key. A missing key is not an error.
func (m *Memory) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.objects, key)
	return nil
}
