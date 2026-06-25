// Package httpx holds helpers shared by the HTTP service adapters
// (embed.OpenAI, summarize.Anthropic): mapping a non-2xx response to a
// dispatcher-classified error and parsing the Retry-After header. Centralizing
// this keeps every adapter's error semantics identical to dispatch.Classify, so
// the dispatcher and the pipeline route upstream failures the same way.
package httpx

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/bbell/reading-lite/internal/dispatch"
)

// errSnippetMax bounds how much of an error response body is echoed into the
// returned error message.
const errSnippetMax = 512

// ClassifyResponse maps a non-2xx response to a dispatcher-classified error:
//   - 429 → *[dispatch.RateLimitError] honoring Retry-After (a Requeue),
//   - other 4xx → wraps [dispatch.ErrPermanent] (a Fail),
//   - everything else (5xx and unexpected codes) → a plain transient error (a Retry).
//
// svc names the calling adapter; a snippet of the response body is included for
// diagnostics. The caller still owns closing resp.Body.
func ClassifyResponse(svc string, resp *http.Response) error {
	snippet := readSnippet(resp.Body)
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return &dispatch.RateLimitError{
			RetryAfter: RateLimitDelay(resp.Header.Get("Retry-After")),
			Err:        fmt.Errorf("%s: rate limited: %s", svc, snippet),
		}
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return fmt.Errorf("%w: %s: status %d: %s", dispatch.ErrPermanent, svc, resp.StatusCode, snippet)
	default:
		return fmt.Errorf("%s: status %d: %s", svc, resp.StatusCode, snippet)
	}
}

// RateLimitDelay returns the re-dispatch delay for a rate-limited (429) response.
// A meaningful explicit Retry-After (>= 1s) is honored; a missing, zero, or
// sub-second value falls back to [dispatch.DefaultRateLimitDelay]. The fallback is
// essential: a Requeue does not consume an attempt, so a zero delay would
// re-dispatch immediately and spin the dispatcher on an origin that always 429s
// without a usable Retry-After.
func RateLimitDelay(header string) time.Duration {
	if d := RetryAfter(header); d >= time.Second {
		return d
	}
	return dispatch.DefaultRateLimitDelay
}

// RetryAfter parses a Retry-After header value, accepting either a delay in
// seconds or an HTTP date. An absent, past, or unparseable value yields 0
// (re-dispatch without an extra wait).
func RetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

func readSnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, errSnippetMax))
	return string(bytes.TrimSpace(b))
}
