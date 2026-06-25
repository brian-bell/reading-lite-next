package blobs_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/bbell/reading-lite/internal/blobs"
)

func TestMemory_PutGetRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := blobs.NewMemory()

	want := []byte("# extracted markdown\n\nbody")
	if err := b.Put(ctx, "readings/r1/content.md", want, "text/markdown"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ct, err := b.Get(ctx, "readings/r1/content.md")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ct != "text/markdown" {
		t.Fatalf("content type = %q, want text/markdown", ct)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Get data mismatch (-want +got):\n%s", diff)
	}
}

func TestMemory_GetMissingIsNotFound(t *testing.T) {
	t.Parallel()

	b := blobs.NewMemory()
	if _, _, err := b.Get(context.Background(), "absent"); !errors.Is(err, blobs.ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
}

func TestMemory_PutOverwrites(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := blobs.NewMemory()
	if err := b.Put(ctx, "k", []byte("v1"), "text/plain"); err != nil {
		t.Fatalf("Put v1: %v", err)
	}
	if err := b.Put(ctx, "k", []byte("v2"), "application/json"); err != nil {
		t.Fatalf("Put v2: %v", err)
	}

	got, ct, err := b.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "v2" || ct != "application/json" {
		t.Fatalf("after overwrite = %q/%q, want v2/application/json", got, ct)
	}
}

func TestMemory_DeleteRemovesAndIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := blobs.NewMemory()
	if err := b.Put(ctx, "k", []byte("v"), "text/plain"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := b.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := b.Get(ctx, "k"); !errors.Is(err, blobs.ErrNotFound) {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
	// Deleting an absent key is a no-op, not an error (S3 delete semantics).
	if err := b.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete absent = %v, want nil", err)
	}
}

// TestMemory_StoresCopies proves the store does not alias the caller's slice:
// mutating the input after Put, or the returned slice after Get, must not change
// stored bytes.
func TestMemory_StoresCopies(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := blobs.NewMemory()
	data := []byte("original")
	if err := b.Put(ctx, "k", data, "text/plain"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	data[0] = 'X' // mutate caller slice after Put

	got, _, err := b.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "original" {
		t.Fatalf("stored bytes aliased the input: got %q", got)
	}
	got[0] = 'Y' // mutate returned slice

	again, _, err := b.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get again: %v", err)
	}
	if string(again) != "original" {
		t.Fatalf("stored bytes aliased a returned slice: got %q", again)
	}
}

func TestMemory_ContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := blobs.NewMemory()

	if err := b.Put(ctx, "k", []byte("v"), "text/plain"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put on cancelled ctx = %v, want context.Canceled", err)
	}
	if _, _, err := b.Get(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get on cancelled ctx = %v, want context.Canceled", err)
	}
	if err := b.Delete(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Delete on cancelled ctx = %v, want context.Canceled", err)
	}
}

// TestMemory_ConcurrentAccess exercises the store under -race with overlapping
// readers and writers; workers may share one Blobs in the pipeline.
func TestMemory_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := blobs.NewMemory()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := "k"
			_ = b.Put(ctx, key, []byte{byte(i)}, "text/plain")
			_, _, _ = b.Get(ctx, key)
			_ = b.Delete(ctx, key)
		}()
	}
	wg.Wait()
}
