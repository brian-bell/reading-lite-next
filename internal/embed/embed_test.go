package embed_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/bbell/reading-lite/internal/embed"
)

func TestFake_DefaultReturnsZeroVectorOfDim(t *testing.T) {
	t.Parallel()

	f := &embed.Fake{}
	v, err := f.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(v) != embed.Dim {
		t.Fatalf("len(vec) = %d, want %d", len(v), embed.Dim)
	}
}

func TestFake_ReturnsScriptedVector(t *testing.T) {
	t.Parallel()

	want := make([]float32, embed.Dim)
	want[0], want[1535] = 0.5, -0.25
	f := &embed.Fake{Vec: want}

	v, err := f.Embed(context.Background(), "x")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(v) != embed.Dim || v[0] != 0.5 || v[1535] != -0.25 {
		t.Fatalf("vec = %v…, want scripted", v[:2])
	}

	// The returned vector is a copy: mutating it must not change the script.
	v[0] = 9
	again, _ := f.Embed(context.Background(), "x")
	if again[0] != 0.5 {
		t.Fatalf("scripted vector aliased a returned slice: got %v", again[0])
	}
}

func TestFake_ScriptedError(t *testing.T) {
	t.Parallel()

	f := &embed.Fake{Err: errors.New("boom")}
	if _, err := f.Embed(context.Background(), "x"); err == nil {
		t.Fatal("Embed = nil error, want scripted error")
	}
	if f.Calls() != 1 {
		t.Fatalf("Calls = %d, want 1", f.Calls())
	}
}

func TestFake_RecordsCallsAndTexts(t *testing.T) {
	t.Parallel()

	f := &embed.Fake{}
	if _, err := f.Embed(context.Background(), "first"); err != nil {
		t.Fatalf("Embed first: %v", err)
	}
	if _, err := f.Embed(context.Background(), "second"); err != nil {
		t.Fatalf("Embed second: %v", err)
	}
	if f.Calls() != 2 {
		t.Fatalf("Calls = %d, want 2", f.Calls())
	}
	got := f.Texts()
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("Texts = %v, want [first second]", got)
	}
}

func TestFake_ContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &embed.Fake{}
	if _, err := f.Embed(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Embed cancelled = %v, want context.Canceled", err)
	}
}

func TestFake_ConcurrentEmbed(t *testing.T) {
	t.Parallel()

	f := &embed.Fake{}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = f.Embed(context.Background(), "x")
		}()
	}
	wg.Wait()
	if f.Calls() != 20 {
		t.Fatalf("Calls = %d, want 20", f.Calls())
	}
}
