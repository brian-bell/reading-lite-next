// Package vector defines the similarity-index port and an in-memory backend.
//
// The production adapter is pgvector (vector.Postgres, Phase 6); [Memory] is a
// real cosine-similarity index over a map, used by tests and zero-infra deploys.
// Both are pinned by the shared vectortest conformance suite so their ranking
// agrees.
package vector

import (
	"cmp"
	"context"
	"errors"
	"math"
	"slices"
	"sync"
)

// Dim is the embedding dimension every indexed vector must carry.
const Dim = 1536

// ErrDimension reports a vector whose length is not [Dim].
var ErrDimension = errors.New("vector: wrong dimension")

// Match is one similarity result: a reading id and its cosine score in [-1, 1].
type Match struct {
	// ID is the matched reading id.
	ID string
	// Score is the cosine similarity to the query vector (higher is closer).
	Score float64
}

// Index stores reading embeddings and answers nearest-neighbor queries. It is
// the VectorIndex port: the production adapter is pgvector, [Memory] the fake.
type Index interface {
	// Upsert stores or replaces the vector for id. The vector must have length Dim.
	Upsert(ctx context.Context, id string, vec []float32) error
	// Query returns up to topK matches ranked by descending cosine similarity,
	// omitting excludeID (matched exactly; the zero value "" excludes nothing).
	// The query vector must have length Dim.
	Query(ctx context.Context, vec []float32, topK int, excludeID string) ([]Match, error)
	// Delete removes id. Deleting an absent id is a no-op.
	Delete(ctx context.Context, id string) error
}

// Memory is a concurrency-safe in-memory [Index] backed by exact cosine
// similarity over a map.
type Memory struct {
	mu   sync.RWMutex
	vecs map[string][]float32
}

// NewMemory returns an empty in-memory vector index.
func NewMemory() *Memory {
	return &Memory{vecs: map[string][]float32{}}
}

// Upsert stores a copy of vec under id, replacing any existing vector.
func (m *Memory) Upsert(ctx context.Context, id string, vec []float32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(vec) != Dim {
		return ErrDimension
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.vecs[id] = slices.Clone(vec)
	return nil
}

// Query ranks stored vectors by cosine similarity to vec and returns the top
// matches, excluding excludeID. Ties break by id for deterministic ordering.
func (m *Memory) Query(ctx context.Context, vec []float32, topK int, excludeID string) ([]Match, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(vec) != Dim {
		return nil, ErrDimension
	}
	if topK < 0 {
		topK = 0
	}

	m.mu.RLock()
	matches := make([]Match, 0, len(m.vecs))
	for id, stored := range m.vecs {
		// The zero value "" excludes nothing (per the Index contract); only a
		// non-empty excludeID drops its matching vector.
		if excludeID != "" && id == excludeID {
			continue
		}
		matches = append(matches, Match{ID: id, Score: cosine(vec, stored)})
	}
	m.mu.RUnlock()

	slices.SortFunc(matches, func(a, b Match) int {
		if c := cmp.Compare(b.Score, a.Score); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	if topK < len(matches) {
		matches = matches[:topK]
	}
	return matches, nil
}

// Delete removes id. A missing id is not an error.
func (m *Memory) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.vecs, id)
	return nil
}

// cosine returns the cosine similarity of two equal-length vectors, or 0 when
// either has zero magnitude (no meaningful direction to compare).
func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
