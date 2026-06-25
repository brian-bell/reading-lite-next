package fetch_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/bbell/reading-lite/internal/fetch"
)

func TestFake_ReturnsScriptedResource(t *testing.T) {
	t.Parallel()

	want := fetch.Resource{
		Body:        []byte("<html><body>hi</body></html>"),
		ContentType: "text/html; charset=utf-8",
		FinalURL:    "https://example.com/article",
		Status:      200,
	}
	f := &fetch.Fake{Resource: want}

	got, err := f.Get(context.Background(), "https://example.com/article")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("resource mismatch (-want +got):\n%s", diff)
	}

	// The returned body is a copy: mutating it must not change the script.
	got.Body[0] = 'X'
	again, _ := f.Get(context.Background(), "https://example.com/article")
	if again.Body[0] != '<' {
		t.Fatalf("scripted body aliased a returned slice: got %q", again.Body[0])
	}
}

func TestFake_ScriptedError(t *testing.T) {
	t.Parallel()

	f := &fetch.Fake{Err: errors.New("dial tcp: timeout")}
	if _, err := f.Get(context.Background(), "https://example.com"); err == nil {
		t.Fatal("Get = nil error, want scripted error")
	}
	if f.Calls() != 1 {
		t.Fatalf("Calls = %d, want 1", f.Calls())
	}
}

func TestFake_RecordsRequestedURLs(t *testing.T) {
	t.Parallel()

	f := &fetch.Fake{}
	for _, u := range []string{"https://a.example", "https://b.example"} {
		if _, err := f.Get(context.Background(), u); err != nil {
			t.Fatalf("Get %q: %v", u, err)
		}
	}
	got := f.URLs()
	if len(got) != 2 || got[0] != "https://a.example" || got[1] != "https://b.example" {
		t.Fatalf("URLs = %v, want [a b]", got)
	}
	if f.Calls() != 2 {
		t.Fatalf("Calls = %d, want 2", f.Calls())
	}
}

func TestFake_ContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fetch.Fake{}
	if _, err := f.Get(ctx, "https://example.com"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get cancelled = %v, want context.Canceled", err)
	}
}

func TestFake_ConcurrentGet(t *testing.T) {
	t.Parallel()

	f := &fetch.Fake{Resource: fetch.Resource{Status: 200}}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = f.Get(context.Background(), "https://example.com")
		}()
	}
	wg.Wait()
	if f.Calls() != 20 {
		t.Fatalf("Calls = %d, want 20", f.Calls())
	}
}
