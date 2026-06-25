package summarize_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/bbell/reading-lite/internal/summarize"
)

func TestFake_ReturnsScriptedSummary(t *testing.T) {
	t.Parallel()

	want := summarize.Summary{
		Title:   "Kubernetes for personal services",
		Summary: "A practical guide to a small database-backed service.",
		Tags:    []string{"go", "db"},
		JSON:    json.RawMessage(`{"key_points":["a","b"]}`),
	}
	f := &summarize.Fake{Summary: want}

	got, err := f.Summarize(context.Background(), summarize.SummaryInput{Markdown: "body"})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("summary mismatch (-want +got):\n%s", diff)
	}

	// The returned tags are a copy: mutating them must not change the script.
	got.Tags[0] = "mutated"
	// The returned JSON (a json.RawMessage = []byte) is likewise a copy.
	got.JSON[0] = '['
	again, _ := f.Summarize(context.Background(), summarize.SummaryInput{})
	if again.Tags[0] != "go" {
		t.Fatalf("scripted tags aliased a returned slice: got %q", again.Tags[0])
	}
	if string(again.JSON) != `{"key_points":["a","b"]}` {
		t.Fatalf("scripted JSON aliased a returned slice: got %s", again.JSON)
	}
}

func TestFake_ScriptedError(t *testing.T) {
	t.Parallel()

	f := &summarize.Fake{Err: errors.New("no tool_use block")}
	if _, err := f.Summarize(context.Background(), summarize.SummaryInput{}); err == nil {
		t.Fatal("Summarize = nil error, want scripted error")
	}
	if f.Calls() != 1 {
		t.Fatalf("Calls = %d, want 1", f.Calls())
	}
}

func TestFake_RecordsInputs(t *testing.T) {
	t.Parallel()

	f := &summarize.Fake{}
	in := summarize.SummaryInput{Title: "T", URL: "https://example.com/a", Markdown: "body"}
	if _, err := f.Summarize(context.Background(), in); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	got := f.Inputs()
	if len(got) != 1 {
		t.Fatalf("Inputs len = %d, want 1", len(got))
	}
	if diff := cmp.Diff(in, got[0]); diff != "" {
		t.Fatalf("recorded input mismatch (-want +got):\n%s", diff)
	}
}

func TestFake_ContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &summarize.Fake{}
	if _, err := f.Summarize(ctx, summarize.SummaryInput{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Summarize cancelled = %v, want context.Canceled", err)
	}
}

func TestFake_ConcurrentSummarize(t *testing.T) {
	t.Parallel()

	f := &summarize.Fake{}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = f.Summarize(context.Background(), summarize.SummaryInput{})
		}()
	}
	wg.Wait()
	if f.Calls() != 20 {
		t.Fatalf("Calls = %d, want 20", f.Calls())
	}
}
