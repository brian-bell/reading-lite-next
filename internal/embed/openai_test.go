package embed_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bbell/reading-lite/internal/dispatch"
	"github.com/bbell/reading-lite/internal/embed"
)

// embeddingResponse builds an OpenAI-shaped embeddings response of the given dim.
func embeddingResponse(t *testing.T, dim int) []byte {
	t.Helper()
	vec := make([]float64, dim)
	for i := range vec {
		vec[i] = float64(i%7) * 0.01
	}
	body, err := json.Marshal(map[string]any{
		"object": "list",
		"model":  "text-embedding-3-small",
		"data":   []map[string]any{{"object": "embedding", "index": 0, "embedding": vec}},
	})
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	return body
}

func TestOpenAI_RequestShapeAndParse(t *testing.T) {
	t.Parallel()

	// Capturing request state in outer vars and asserting it after the client call
	// is race-safe (verified under -race): the handler writes these before writing
	// the response, and the test reads them only after Embed returns — i.e. after the
	// full response transfer, which is the happens-before edge. The same pattern is
	// used in the other adapter tests for the same reason.
	var gotAuth, gotPath, gotContentType string
	var gotBody struct {
		Input string `json:"input"`
		Model string `json:"model"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(embeddingResponse(t, embed.Dim))
	}))
	defer srv.Close()

	e := embed.NewOpenAI("test-key", embed.WithBaseURL(srv.URL))
	v, err := e.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", gotAuth)
	}
	if gotPath != "/v1/embeddings" {
		t.Errorf("path = %q, want /v1/embeddings", gotPath)
	}
	if !strings.HasPrefix(gotContentType, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotBody.Model != "text-embedding-3-small" {
		t.Errorf("model = %q, want text-embedding-3-small", gotBody.Model)
	}
	if gotBody.Input != "hello" {
		t.Errorf("input = %q, want hello", gotBody.Input)
	}
	if len(v) != embed.Dim {
		t.Fatalf("len(vec) = %d, want %d", len(v), embed.Dim)
	}
}

func TestOpenAI_RateLimitMapsToRateLimitError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit reached"}}`))
	}))
	defer srv.Close()

	e := embed.NewOpenAI("k", embed.WithBaseURL(srv.URL))
	_, err := e.Embed(context.Background(), "x")

	var rl *dispatch.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("429 mapped to %v, want *dispatch.RateLimitError", err)
	}
	if rl.RetryAfter != 30*time.Second {
		t.Fatalf("RetryAfter = %v, want 30s (from Retry-After header)", rl.RetryAfter)
	}
	// It must classify as a Requeue carrying the delay, never as Retry/Fail.
	if got := dispatch.Classify(err); got.Outcome != dispatch.Requeue || got.After != 30*time.Second {
		t.Fatalf("Classify(429) = %v/%v, want Requeue/30s", got.Outcome, got.After)
	}
}

func TestOpenAI_ClientErrorIsPermanent(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad input"}}`))
	}))
	defer srv.Close()

	e := embed.NewOpenAI("k", embed.WithBaseURL(srv.URL))
	_, err := e.Embed(context.Background(), "x")
	if !errors.Is(err, dispatch.ErrPermanent) {
		t.Fatalf("4xx = %v, want ErrPermanent", err)
	}
	if got := dispatch.Classify(err); got.Outcome != dispatch.Fail {
		t.Fatalf("Classify(4xx) = %v, want Fail", got.Outcome)
	}
}

func TestOpenAI_WrongDimensionIsPermanent(t *testing.T) {
	t.Parallel()

	// A valid 200 whose vector is not Dim-length violates the Embedder contract
	// (the vector index requires Dim). Treating it as a successful embed only defers
	// the failure to Vectors.Upsert, where it would retry forever; the adapter must
	// reject it up front as permanent (a misconfigured model won't fix itself).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3]}]}`))
	}))
	defer srv.Close()

	e := embed.NewOpenAI("k", embed.WithBaseURL(srv.URL))
	v, err := e.Embed(context.Background(), "x")
	if v != nil {
		t.Fatalf("vector = %v, want nil on a dimension mismatch", v)
	}
	if !errors.Is(err, dispatch.ErrPermanent) {
		t.Fatalf("wrong-dimension embed = %v, want ErrPermanent", err)
	}
	if got := dispatch.Classify(err); got.Outcome != dispatch.Fail {
		t.Fatalf("Classify(wrong dim) = %v, want Fail", got.Outcome)
	}
}

func TestOpenAI_ServerErrorIsTransient(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream boom"))
	}))
	defer srv.Close()

	e := embed.NewOpenAI("k", embed.WithBaseURL(srv.URL))
	_, err := e.Embed(context.Background(), "x")
	if err == nil {
		t.Fatal("5xx = nil error, want a transient error")
	}
	// A 5xx is neither permanent nor rate-limited: it must retry.
	if errors.Is(err, dispatch.ErrPermanent) {
		t.Fatalf("5xx = %v, want NOT permanent", err)
	}
	if got := dispatch.Classify(err); got.Outcome != dispatch.Retry {
		t.Fatalf("Classify(5xx) = %v, want Retry", got.Outcome)
	}
}
