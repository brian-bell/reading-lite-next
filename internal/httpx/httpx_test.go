package httpx_test

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bbell/reading-lite/internal/dispatch"
	"github.com/bbell/reading-lite/internal/httpx"
)

func resp(status int, header http.Header, body string) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestClassifyResponse_RateLimit(t *testing.T) {
	t.Parallel()

	h := http.Header{"Retry-After": {"30"}}
	err := httpx.ClassifyResponse("embed", resp(http.StatusTooManyRequests, h, `{"error":"slow down"}`))

	var rl *dispatch.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("429 = %v, want *dispatch.RateLimitError", err)
	}
	if rl.RetryAfter != 30*time.Second {
		t.Fatalf("RetryAfter = %v, want 30s", rl.RetryAfter)
	}
	if got := dispatch.Classify(err); got.Outcome != dispatch.Requeue || got.After != 30*time.Second {
		t.Fatalf("Classify = %v/%v, want Requeue/30s", got.Outcome, got.After)
	}
}

func TestClassifyResponse_ClientErrorPermanent(t *testing.T) {
	t.Parallel()

	err := httpx.ClassifyResponse("embed", resp(http.StatusBadRequest, nil, "bad input"))
	if !errors.Is(err, dispatch.ErrPermanent) {
		t.Fatalf("4xx = %v, want ErrPermanent", err)
	}
	if !strings.Contains(err.Error(), "bad input") {
		t.Fatalf("error = %v, want it to echo the body snippet", err)
	}
}

func TestClassifyResponse_ServerErrorTransient(t *testing.T) {
	t.Parallel()

	err := httpx.ClassifyResponse("summarize", resp(http.StatusBadGateway, nil, "upstream down"))
	if errors.Is(err, dispatch.ErrPermanent) {
		t.Fatalf("5xx = %v, want NOT permanent", err)
	}
	var rl *dispatch.RateLimitError
	if errors.As(err, &rl) {
		t.Fatalf("5xx = %v, want NOT rate-limited", err)
	}
	if got := dispatch.Classify(err); got.Outcome != dispatch.Retry {
		t.Fatalf("Classify(5xx) = %v, want Retry", got.Outcome)
	}
}

func TestRetryAfter(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"seconds", "45", 45 * time.Second},
		{"zero", "0", 0},
		{"empty", "", 0},
		{"negative", "-5", 0},
		{"garbage", "soon", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := httpx.RetryAfter(c.in); got != c.want {
				t.Fatalf("RetryAfter(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}

	// An HTTP-date in the future yields a positive, bounded delay.
	future := time.Now().Add(2 * time.Minute).UTC().Format(http.TimeFormat)
	if got := httpx.RetryAfter(future); got <= 0 || got > 2*time.Minute {
		t.Fatalf("RetryAfter(future date) = %v, want a positive delay ≤ 2m", got)
	}
}
