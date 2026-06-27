package fetch_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bbell/reading-lite/internal/dispatch"
	"github.com/bbell/reading-lite/internal/fetch"
)

func TestHTTP_RequestShapeAndParse(t *testing.T) {
	t.Parallel()

	var gotUA, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotMethod = r.Method
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html>hello</html>"))
	}))
	defer srv.Close()

	f := fetch.NewHTTP(fetch.WithPrivateNetworkBypassForTests(), fetch.WithUserAgent("reading-lite-test/1.0"))
	res, err := f.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotUA != "reading-lite-test/1.0" {
		t.Errorf("User-Agent = %q, want reading-lite-test/1.0", gotUA)
	}
	if res.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", res.Status)
	}
	if string(res.Body) != "<html>hello</html>" {
		t.Errorf("Body = %q, want the html", res.Body)
	}
	if !strings.HasPrefix(res.ContentType, "text/html") {
		t.Errorf("ContentType = %q, want text/html prefix", res.ContentType)
	}
	if res.FinalURL != srv.URL {
		t.Errorf("FinalURL = %q, want %q", res.FinalURL, srv.URL)
	}
}

func TestHTTP_FollowsRedirectsReportsFinalURL(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/end", http.StatusFound)
	})
	mux.HandleFunc("/end", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("arrived"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := fetch.NewHTTP(fetch.WithPrivateNetworkBypassForTests())
	res, err := f.Get(context.Background(), srv.URL+"/start")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if res.Status != http.StatusOK || string(res.Body) != "arrived" {
		t.Fatalf("after redirect = %d/%q, want 200/arrived", res.Status, res.Body)
	}
	if want := srv.URL + "/end"; res.FinalURL != want {
		t.Fatalf("FinalURL = %q, want %q (post-redirect URL)", res.FinalURL, want)
	}
}

func TestHTTP_CapsBodySize(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 100)))
	}))
	defer srv.Close()

	// A cap below the response size rejects rather than truncating or OOMing.
	f := fetch.NewHTTP(fetch.WithPrivateNetworkBypassForTests(), fetch.WithMaxBytes(10))
	_, err := f.Get(context.Background(), srv.URL)
	if !errors.Is(err, fetch.ErrBodyTooLarge) {
		t.Fatalf("Get over cap = %v, want ErrBodyTooLarge", err)
	}
	// An oversized body is permanently oversized: classify it as permanent so the
	// dispatcher fails it instead of burning retries on a page that won't shrink.
	if !errors.Is(err, dispatch.ErrPermanent) {
		t.Fatalf("Get over cap = %v, want it to also be ErrPermanent", err)
	}

	// A body exactly at the cap is accepted.
	g := fetch.NewHTTP(fetch.WithPrivateNetworkBypassForTests(), fetch.WithMaxBytes(100))
	res, err := g.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get at cap: %v", err)
	}
	if len(res.Body) != 100 {
		t.Fatalf("body len = %d, want 100 (exactly at cap)", len(res.Body))
	}
}

func TestHTTP_OversizedNon2xxPreservesStatus(t *testing.T) {
	t.Parallel()

	// A non-2xx response with a large error page must not be turned into a
	// permanent body-size failure: the pipeline classifies non-2xx by status
	// (5xx → retry), so Get returns the status with a capped body, not an error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(strings.Repeat("x", 100)))
	}))
	defer srv.Close()

	res, err := fetch.NewHTTP(fetch.WithPrivateNetworkBypassForTests(), fetch.WithMaxBytes(10)).Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("oversized 5xx = %v, want no error (status drives classification)", err)
	}
	if res.Status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.Status)
	}
	if int64(len(res.Body)) > 10 {
		t.Fatalf("body len = %d, want it capped to 10 (memory-bounded)", len(res.Body))
	}
}

func TestHTTP_RateLimitMapsToRateLimitError(t *testing.T) {
	t.Parallel()

	// A 429 from the origin is a rate limit, not a hard failure: the adapter must
	// surface it as a RateLimitError (honoring Retry-After) so the dispatcher
	// requeues without consuming an attempt — not let the pipeline's 4xx→permanent
	// status policy fail the reading.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("slow down"))
	}))
	defer srv.Close()

	_, err := fetch.NewHTTP(fetch.WithPrivateNetworkBypassForTests()).Get(context.Background(), srv.URL)
	var rl *dispatch.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("429 = %v, want *dispatch.RateLimitError", err)
	}
	if rl.RetryAfter != 30*time.Second {
		t.Fatalf("RetryAfter = %v, want 30s (from Retry-After header)", rl.RetryAfter)
	}
	if got := dispatch.Classify(err); got.Outcome != dispatch.Requeue || got.After != 30*time.Second {
		t.Fatalf("Classify(429) = %v/%v, want Requeue/30s", got.Outcome, got.After)
	}
}

func TestHTTP_RateLimitWithoutRetryAfterUsesBoundedDelay(t *testing.T) {
	t.Parallel()

	// A bare 429 (no Retry-After) must not requeue with a zero delay: that would
	// spin the dispatcher (Requeue consumes no attempt) on an origin that always
	// 429s. The adapter falls back to the bounded default delay.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := fetch.NewHTTP(fetch.WithPrivateNetworkBypassForTests()).Get(context.Background(), srv.URL)
	var rl *dispatch.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("bare 429 = %v, want *dispatch.RateLimitError", err)
	}
	if rl.RetryAfter != dispatch.DefaultRateLimitDelay {
		t.Fatalf("RetryAfter = %v, want DefaultRateLimitDelay %v", rl.RetryAfter, dispatch.DefaultRateLimitDelay)
	}
	if got := dispatch.Classify(err); got.Outcome != dispatch.Requeue || got.After <= 0 {
		t.Fatalf("Classify(bare 429) = %v/%v, want Requeue with a positive delay", got.Outcome, got.After)
	}
}

func TestHTTP_RejectsNonHTTPScheme(t *testing.T) {
	t.Parallel()

	f := fetch.NewHTTP()
	for _, url := range []string{"ftp://example.com/file", "file:///etc/passwd", "javascript:alert(1)"} {
		if _, err := f.Get(context.Background(), url); !errors.Is(err, fetch.ErrUnsupportedScheme) {
			t.Errorf("Get(%q) = %v, want ErrUnsupportedScheme", url, err)
		}
	}
}

func TestHTTP_HonorsTimeout(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Block until the client gives up (its timeout fires and disconnects),
		// so the only way Get returns is via the timeout — deterministic outcome.
		<-r.Context().Done()
	}))
	defer srv.Close()

	f := fetch.NewHTTP(fetch.WithPrivateNetworkBypassForTests(), fetch.WithTimeout(50*time.Millisecond))
	if _, err := f.Get(context.Background(), srv.URL); err == nil {
		t.Fatal("Get against a hung server = nil error, want a timeout error")
	}
}

func TestHTTP_RespectsContextCancellation(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := fetch.NewHTTP(fetch.WithPrivateNetworkBypassForTests())
	if _, err := f.Get(ctx, srv.URL); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get with cancelled ctx = %v, want context.Canceled", err)
	}
}
