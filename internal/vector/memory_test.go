package vector_test

import (
	"context"
	"sync"
	"testing"

	"github.com/bbell/reading-lite/internal/vector"
	"github.com/bbell/reading-lite/internal/vector/vectortest"
)

func TestMemoryContract(t *testing.T) {
	vectortest.RunContract(t, func(t *testing.T) vector.Index {
		t.Helper()
		return vector.NewMemory()
	})
}

// TestMemory_StoresCopies proves Upsert does not alias the caller's slice.
func TestMemory_StoresCopies(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	idx := vector.NewMemory()
	v := make([]float32, vector.Dim)
	v[0] = 1
	if err := idx.Upsert(ctx, "a", v); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	v[0] = 0
	v[1] = 1 // mutate the caller slice into a different direction after Upsert

	got, err := idx.Query(ctx, queryVec(1, 0), 1, "")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 || got[0].Score < 0.99 {
		t.Fatalf("stored vector aliased the input: score=%v, want ~1.0", got[0].Score)
	}
}

func TestMemory_ContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	idx := vector.NewMemory()
	if err := idx.Upsert(ctx, "a", queryVec(1, 0)); err == nil {
		t.Fatal("Upsert on cancelled ctx = nil, want error")
	}
	if _, err := idx.Query(ctx, queryVec(1, 0), 1, ""); err == nil {
		t.Fatal("Query on cancelled ctx = nil, want error")
	}
	if err := idx.Delete(ctx, "a"); err == nil {
		t.Fatal("Delete on cancelled ctx = nil, want error")
	}
}

func TestMemory_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	idx := vector.NewMemory()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v := queryVec(float32(i), 1)
			_ = idx.Upsert(ctx, "shared", v)
			_, _ = idx.Query(ctx, v, 5, "")
			_ = idx.Delete(ctx, "shared")
		}()
	}
	wg.Wait()
}

func queryVec(components ...float32) []float32 {
	v := make([]float32, vector.Dim)
	copy(v, components)
	return v
}
