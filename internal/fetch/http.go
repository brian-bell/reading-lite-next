package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/bbell/reading-lite/internal/dispatch"
	"github.com/bbell/reading-lite/internal/httpx"
)

// Default tuning for the HTTP fetcher. The body cap bounds memory against a
// hostile or runaway response; the timeout bounds a slow upstream.
const (
	defaultUserAgent = "reading-lite/1.0 (+https://github.com/bbell/reading-lite)"
	defaultTimeout   = 30 * time.Second
	defaultMaxBytes  = 10 << 20 // 10 MiB
)

// ErrUnsupportedScheme reports a request URL whose scheme is not http or https.
// Only http(s) is fetched; other schemes (ftp, file, javascript, …) are rejected
// before any network call.
var ErrUnsupportedScheme = errors.New("fetch: unsupported URL scheme")

// ErrBodyTooLarge reports a response whose body exceeds the configured cap. The
// body is rejected rather than truncated so callers never extract a partial
// document, and never buffer an unbounded response into memory.
var ErrBodyTooLarge = errors.New("fetch: response body exceeds limit")

// HTTP is the production [Fetcher]: an http.Client wrapper that sets a User-Agent,
// honors a timeout, caps the response body size, follows redirects (reporting the
// post-redirect URL as FinalURL), and rejects non-http(s) schemes.
type HTTP struct {
	client    *http.Client
	userAgent string
	maxBytes  int64
	timeout   time.Duration
}

// Option configures an [HTTP] fetcher.
type Option func(*HTTP)

// WithUserAgent sets the User-Agent header sent on every request.
func WithUserAgent(ua string) Option {
	return func(h *HTTP) { h.userAgent = ua }
}

// WithTimeout sets the per-request timeout. The fetcher applies it to its client
// after all options run, so it wins regardless of option order (including over a
// client supplied via [WithHTTPClient]).
func WithTimeout(d time.Duration) Option {
	return func(h *HTTP) { h.timeout = d }
}

// WithMaxBytes caps the response body size. A response larger than n bytes is
// rejected with [ErrBodyTooLarge].
func WithMaxBytes(n int64) Option {
	return func(h *HTTP) { h.maxBytes = n }
}

// WithHTTPClient replaces the underlying http.Client (e.g. to inject a custom
// transport in tests). The fetcher's timeout ([WithTimeout] or the default) is
// applied to this client, so set request timeouts via WithTimeout, not on c.
func WithHTTPClient(c *http.Client) Option {
	return func(h *HTTP) { h.client = c }
}

// NewHTTP returns an HTTP fetcher with sensible defaults, overridden by opts.
func NewHTTP(opts ...Option) *HTTP {
	h := &HTTP{
		client:    &http.Client{},
		userAgent: defaultUserAgent,
		maxBytes:  defaultMaxBytes,
		timeout:   defaultTimeout,
	}
	for _, opt := range opts {
		opt(h)
	}
	// Apply the timeout after options so WithTimeout is order-independent and wins
	// over any client passed via WithHTTPClient.
	h.client.Timeout = h.timeout
	return h
}

// Get fetches url, returning the (size-capped) body, content type, final URL after
// redirects, and HTTP status. It rejects non-http(s) URLs before dialing.
func (h *HTTP) Get(ctx context.Context, url string) (Resource, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Resource{}, fmt.Errorf("fetch: build request: %w", err)
	}
	if s := req.URL.Scheme; s != "http" && s != "https" {
		return Resource{}, fmt.Errorf("%w: %q", ErrUnsupportedScheme, s)
	}
	// TODO(phase-11): this validates only the initial URL. When the SSRF guard
	// lands, hook http.Client.CheckRedirect to re-validate each redirect hop's
	// scheme/host (a redirect can otherwise reach an internal address).
	req.Header.Set("User-Agent", h.userAgent)

	resp, err := h.client.Do(req)
	if err != nil {
		return Resource{}, fmt.Errorf("fetch: get %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// A 429 is a rate limit, not a hard failure: surface it as a RateLimitError
	// (honoring Retry-After) so the dispatcher requeues without consuming an
	// attempt, the same way the embed/summarize adapters handle a 429. Returning
	// the raw status instead would let the pipeline's 4xx→permanent policy
	// permanently fail a reading that a throttled origin would serve on retry.
	if resp.StatusCode == http.StatusTooManyRequests {
		return Resource{}, &dispatch.RateLimitError{
			RetryAfter: httpx.RateLimitDelay(resp.Header.Get("Retry-After")),
			Err:        fmt.Errorf("fetch: rate limited by %s", url),
		}
	}

	body, oversize, err := readCapped(resp.Body, h.maxBytes)
	if err != nil {
		return Resource{}, err
	}
	// A body over the cap is only a permanent failure for a 2xx response, whose
	// body is the content we process. For a non-2xx response the pipeline
	// classifies by status and never reads the body, so a large error page must
	// not mask the status (an oversize 5xx must still retry, a 4xx still fails) —
	// keep the (truncated) body and let the status drive classification.
	if oversize && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return Resource{}, fmt.Errorf("%w: %w: > %d bytes", dispatch.ErrPermanent, ErrBodyTooLarge, h.maxBytes)
	}

	final := url
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return Resource{
		Body:        body,
		ContentType: resp.Header.Get("Content-Type"),
		FinalURL:    final,
		Status:      resp.StatusCode,
	}, nil
}

// readCapped reads up to limit bytes from r and reports whether the stream held
// more. It reads one extra byte to distinguish "exactly at the limit" from "over
// the cap" without buffering the whole oversize body; on overflow it returns the
// body truncated to limit so a caller that only needs the status (a non-2xx error
// page) stays memory-bounded.
func readCapped(r io.Reader, limit int64) (body []byte, oversize bool, err error) {
	body, err = io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, fmt.Errorf("fetch: read body: %w", err)
	}
	if int64(len(body)) > limit {
		return body[:limit], true, nil
	}
	return body, false, nil
}
