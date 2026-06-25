package extract_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/bbell/reading-lite/internal/extract"
	"github.com/bbell/reading-lite/internal/fetch"
)

func TestFake_ReturnsScriptedArticle(t *testing.T) {
	t.Parallel()

	want := extract.Article{
		Title:     "Kubernetes for personal services",
		Author:    "Ada",
		Site:      "example.com",
		Lang:      "en",
		Markdown:  "# Kubernetes\n\nbody",
		Mode:      extract.ModeReadability,
		WordCount: 3,
	}
	f := &extract.Fake{Article: want}

	got, err := f.Extract(context.Background(), fetch.Resource{Body: []byte("<html/>")})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("article mismatch (-want +got):\n%s", diff)
	}
}

func TestFake_ScriptedError(t *testing.T) {
	t.Parallel()

	f := &extract.Fake{Err: errors.New("unparseable")}
	if _, err := f.Extract(context.Background(), fetch.Resource{}); err == nil {
		t.Fatal("Extract = nil error, want scripted error")
	}
	if f.Calls() != 1 {
		t.Fatalf("Calls = %d, want 1", f.Calls())
	}
}

func TestFake_RecordsResources(t *testing.T) {
	t.Parallel()

	f := &extract.Fake{}
	in := fetch.Resource{FinalURL: "https://example.com/a", Status: 200}
	if _, err := f.Extract(context.Background(), in); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	got := f.Resources()
	if len(got) != 1 || got[0].FinalURL != "https://example.com/a" {
		t.Fatalf("Resources = %v, want one with FinalURL set", got)
	}
}

// TestFake_RecordsResourceBodyDefensively proves the recorded call history does not
// alias the caller's input Body, nor a previously returned Resources() slice.
func TestFake_RecordsResourceBodyDefensively(t *testing.T) {
	t.Parallel()

	f := &extract.Fake{}
	body := []byte("original")
	if _, err := f.Extract(context.Background(), fetch.Resource{Body: body}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	body[0] = 'X' // mutate the caller's input slice after the call

	got := f.Resources()
	if string(got[0].Body) != "original" {
		t.Fatalf("recorded body aliased the input: got %q", got[0].Body)
	}
	got[0].Body[0] = 'Y' // mutate a returned slice

	again := f.Resources()
	if string(again[0].Body) != "original" {
		t.Fatalf("recorded body aliased a returned slice: got %q", again[0].Body)
	}
}

func TestModeConstants(t *testing.T) {
	t.Parallel()

	// The three extraction tiers map to the reading's extraction_mode values.
	cases := map[extract.Mode]string{
		extract.ModeReadability: "readability",
		extract.ModeRawDOM:      "raw_dom",
		extract.ModeRawOnly:     "raw_only",
	}
	for mode, want := range cases {
		if string(mode) != want {
			t.Fatalf("mode %v = %q, want %q", mode, string(mode), want)
		}
	}
}

func TestFake_ContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &extract.Fake{}
	if _, err := f.Extract(ctx, fetch.Resource{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Extract cancelled = %v, want context.Canceled", err)
	}
}

func TestFake_ConcurrentExtract(t *testing.T) {
	t.Parallel()

	f := &extract.Fake{}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = f.Extract(context.Background(), fetch.Resource{})
		}()
	}
	wg.Wait()
	if f.Calls() != 20 {
		t.Fatalf("Calls = %d, want 20", f.Calls())
	}
}
