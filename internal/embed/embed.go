// Package embed defines the text-embedding port and an in-memory fake.
//
// The production adapter is OpenAI (Phase 6); [Fake] is the scriptable in-memory
// double used by pipeline tests.
package embed

import (
	"context"
	"slices"
	"sync"
)

// Dim is the embedding dimension every vector carries (OpenAI text-embedding-3-small).
const Dim = 1536

// Embedder turns text into a fixed-dimension embedding vector of length [Dim].
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Fake is a concurrency-safe, scriptable [Embedder] for tests. Set the scripted
// fields before first use (they are read under the lock but not meant to change
// once workers may call concurrently); call recording is safe under concurrent workers.
type Fake struct {
	// Vec is the vector returned on success. When nil, Embed returns a fresh
	// zero vector of length Dim so callers get a valid-dimension result by default.
	Vec []float32
	// Err, when non-nil, is returned instead of a vector.
	Err error

	mu    sync.Mutex
	calls int
	texts []string
}

// Embed records the call and returns the scripted vector or error.
func (f *Fake) Embed(ctx context.Context, text string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	f.texts = append(f.texts, text)

	if f.Err != nil {
		return nil, f.Err
	}
	if f.Vec == nil {
		return make([]float32, Dim), nil
	}
	return slices.Clone(f.Vec), nil
}

// Calls is the number of times Embed was invoked.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}

// Texts returns every text passed to Embed, in call order.
func (f *Fake) Texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.texts)
}
