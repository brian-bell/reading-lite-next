package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
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

// ErrBlockedPrivateAddress reports an SSRF guard rejection. It wraps
// dispatch.ErrPermanent at call sites so the dispatcher does not retry a URL
// whose resolved destination is forbidden.
var ErrBlockedPrivateAddress = errors.New("fetch: blocked private address")

// Resolver resolves hostnames for the guarded production transport.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

type dialResult struct {
	conn net.Conn
	err  error
}

// HTTP is the production [Fetcher]: an http.Client wrapper that sets a User-Agent,
// honors a timeout, caps the response body size, follows redirects (reporting the
// post-redirect URL as FinalURL), and rejects non-http(s) schemes.
type HTTP struct {
	client    *http.Client
	userAgent string
	maxBytes  int64
	timeout   time.Duration
	resolver  Resolver
	dial      func(context.Context, string, string) (net.Conn, error)
	bypass    bool
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

// WithResolver replaces DNS resolution for the guarded transport. It is intended
// for tests; production uses net.DefaultResolver through the same guard.
func WithResolver(r Resolver) Option {
	return func(h *HTTP) { h.resolver = r }
}

// WithPrivateNetworkBypassForTests permits private loopback httptest servers.
// Production construction must not expose or enable this option from config.
func WithPrivateNetworkBypassForTests() Option {
	return func(h *HTTP) { h.bypass = true }
}

// WithDialContextForTests replaces the final network dial while keeping
// production URL, DNS, redirect, and private-address validation in place.
func WithDialContextForTests(dial func(context.Context, string, string) (net.Conn, error)) Option {
	return func(h *HTTP) { h.dial = dial }
}

// NewHTTP returns an HTTP fetcher with sensible defaults, overridden by opts.
func NewHTTP(opts ...Option) *HTTP {
	h := &HTTP{
		client:    &http.Client{},
		userAgent: defaultUserAgent,
		maxBytes:  defaultMaxBytes,
		timeout:   defaultTimeout,
		resolver:  netResolver{},
	}
	for _, opt := range opts {
		opt(h)
	}
	h.guardClient()
	// Apply the timeout after options so WithTimeout is order-independent.
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
	if err := h.validateURL(req.URL.Scheme, req.URL.Hostname()); err != nil {
		return Resource{}, err
	}
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

func (h *HTTP) guardClient() {
	if h.client == nil {
		h.client = &http.Client{}
	}
	if h.resolver == nil {
		h.resolver = netResolver{}
	}
	if h.dial == nil {
		dialer := net.Dialer{}
		h.dial = dialer.DialContext
	}
	priorRedirect := h.client.CheckRedirect
	h.client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !h.bypass {
			if req.URL == nil {
				return nil
			}
			if err := h.validateURL(req.URL.Scheme, req.URL.Hostname()); err != nil {
				return err
			}
		}
		if priorRedirect != nil {
			return priorRedirect(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	if h.bypass {
		return
	}
	if h.client.Transport == nil {
		h.client.Transport = &http.Transport{
			Proxy:       nil,
			DialContext: h.guardedDialContext,
		}
		return
	}
	if transport, ok := h.client.Transport.(*http.Transport); ok {
		clone := transport.Clone()
		clone.Proxy = nil
		clone.DialContext = h.guardedDialContext
		clone.DialTLSContext = nil
		clone.DialTLS = nil //nolint:staticcheck // Clear deprecated hook too; otherwise custom TLS dials bypass the SSRF guard.
		h.client.Transport = clone
		return
	}
	h.client.Transport = &http.Transport{
		Proxy:       nil,
		DialContext: h.guardedDialContext,
	}
}

func (h *HTTP) guardedDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("fetch: split dial address: %w", err)
	}
	if err := h.validateURL("", host); err != nil {
		return nil, err
	}
	addrs, err := h.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("fetch: resolve %s: %w", host, err)
	}
	for _, addr := range addrs {
		if IsPrivateAddress(addr) {
			return nil, blockedAddressError()
		}
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("fetch: resolve %s: no addresses", host)
	}
	dialCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan dialResult, len(addrs))
	for _, addr := range addrs {
		address := net.JoinHostPort(addr.String(), port)
		go func() {
			conn, err := h.dial(dialCtx, network, address)
			results <- dialResult{conn: conn, err: err}
		}()
	}
	var lastErr error
	for range addrs {
		select {
		case result := <-results:
			if result.err == nil {
				cancel()
				go drainDialResults(results, len(addrs)-1)
				return result.conn, nil
			}
			lastErr = result.err
		case <-ctx.Done():
			go drainDialResults(results, len(addrs))
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("fetch: dial %s: %w", host, lastErr)
}

func drainDialResults(results <-chan dialResult, n int) {
	for range n {
		result := <-results
		if result.conn != nil {
			_ = result.conn.Close()
		}
	}
}

func (h *HTTP) validateURL(scheme, host string) error {
	if scheme != "" && scheme != "http" && scheme != "https" {
		return fmt.Errorf("%w: %q", ErrUnsupportedScheme, scheme)
	}
	if h.bypass {
		return nil
	}
	addr, ok := parseHostAddr(host)
	if ok && IsPrivateAddress(addr) {
		return blockedAddressError()
	}
	return nil
}

func parseHostAddr(host string) (netip.Addr, bool) {
	host = strings.Trim(host, "[]")
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr, true
}

func blockedAddressError() error {
	return fmt.Errorf("%w: %w", dispatch.ErrPermanent, ErrBlockedPrivateAddress)
}

// IsPrivateAddress reports whether addr is unsuitable for production fetching.
func IsPrivateAddress(addr netip.Addr) bool {
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	for _, prefix := range blockedSpecialPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return addr.IsLoopback() ||
		addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsUnspecified() ||
		addr.IsMulticast() ||
		addr == netip.MustParseAddr("255.255.255.255")
}

var blockedSpecialPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT
	netip.MustParsePrefix("0.0.0.0/8"),       // this network
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.31.196.0/24"), // AS112
	netip.MustParsePrefix("192.52.193.0/24"), // AMT
	netip.MustParsePrefix("192.88.99.0/24"),  // deprecated 6to4 relay anycast
	netip.MustParsePrefix("192.175.48.0/24"), // direct delegation AS112
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved
	netip.MustParsePrefix("100::/64"),        // discard-only
	netip.MustParsePrefix("2001:1::1/128"),   // Port Control Protocol anycast
	netip.MustParsePrefix("2001:1::2/128"),   // Traversal Using Relays around NAT anycast
	netip.MustParsePrefix("2001:3::/32"),     // AMT
	netip.MustParsePrefix("2001:4:112::/48"), // AS112v6
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2001:2::/48"),     // benchmarking
	netip.MustParsePrefix("2001:10::/28"),    // deprecated ORCHID
	netip.MustParsePrefix("2002::/16"),       // 6to4
	netip.MustParsePrefix("3fff::/20"),       // documentation
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local-use NAT64
	netip.MustParsePrefix("64:ff9b::/96"),    // well-known NAT64
	netip.MustParsePrefix("2001:0000::/32"),  // Teredo
	netip.MustParsePrefix("2001:20::/28"),    // ORCHIDv2
	netip.MustParsePrefix("2001:30::/28"),    // Drone Remote ID
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("fc00::/7"),        // unique local
	netip.MustParsePrefix("fe80::/10"),       // link local
}

type netResolver struct{}

func (netResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, network, host)
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
