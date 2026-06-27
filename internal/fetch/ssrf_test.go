package fetch_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bbell/reading-lite/internal/dispatch"
	"github.com/bbell/reading-lite/internal/fetch"
)

func TestIsPrivateAddress_BlocksNonPublicCorpus(t *testing.T) {
	t.Parallel()

	blocked := []string{
		"127.0.0.1",
		"10.0.0.1",
		"172.16.0.1",
		"192.168.1.1",
		"169.254.1.1",
		"169.254.169.254",
		"0.1.2.3",
		"100.64.0.1",
		"198.18.0.1",
		"192.0.2.1",
		"192.31.196.1",
		"192.52.193.1",
		"192.88.99.1",
		"192.175.48.1",
		"198.51.100.1",
		"203.0.113.1",
		"240.0.0.1",
		"0.0.0.0",
		"255.255.255.255",
		"224.0.0.1",
		"::1",
		"fc00::1",
		"fe80::1",
		"ff02::1",
		"100::1",
		"2001:1::1",
		"2001:1::2",
		"2001:3::1",
		"2001:4:112::1",
		"2001:db8::1",
		"::ffff:192.168.1.1",
	}
	for _, raw := range blocked {
		addr := netip.MustParseAddr(raw)
		if !fetch.IsPrivateAddress(addr) {
			t.Fatalf("IsPrivateAddress(%s) = false, want true", raw)
		}
	}

	for _, raw := range []string{"93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946", "::ffff:93.184.216.34"} {
		addr := netip.MustParseAddr(raw)
		if fetch.IsPrivateAddress(addr) {
			t.Fatalf("IsPrivateAddress(%s) = true, want false", raw)
		}
	}
}

func TestHTTP_BlocksPrivateLiteralIP(t *testing.T) {
	t.Parallel()

	_, err := fetch.NewHTTP().Get(context.Background(), "http://127.0.0.1/")
	if !errors.Is(err, fetch.ErrBlockedPrivateAddress) {
		t.Fatalf("Get private literal = %v, want ErrBlockedPrivateAddress", err)
	}
	if !errors.Is(err, dispatch.ErrPermanent) {
		t.Fatalf("Get private literal = %v, want dispatch.ErrPermanent", err)
	}
}

func TestHTTP_BlocksDNSPrivateAddressAndIgnoresProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")

	resolver := &staticResolver{addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
	_, err := fetch.NewHTTP(fetch.WithResolver(resolver)).Get(context.Background(), "http://public.example/")
	if !errors.Is(err, fetch.ErrBlockedPrivateAddress) {
		t.Fatalf("Get DNS private = %v, want ErrBlockedPrivateAddress", err)
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolver.calls)
	}
}

func TestHTTP_BlocksPrivateRedirect(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1/private", http.StatusFound)
	}))
	defer srv.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split test server URL: %v", err)
	}

	resolver := &staticResolver{addrs: []netip.Addr{netip.MustParseAddr("93.184.216.34")}}
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
	}
	_, err = fetch.NewHTTP(fetch.WithResolver(resolver), fetch.WithDialContextForTests(dial)).Get(context.Background(), "http://public.example/start")
	if !errors.Is(err, fetch.ErrBlockedPrivateAddress) {
		t.Fatalf("private redirect = %v, want ErrBlockedPrivateAddress", err)
	}
}

func TestHTTP_CustomClientDoesNotBypassPrivateAddressValidation(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("custom transport was called instead of guarded transport")
		return nil, nil
	})}
	resolver := &staticResolver{addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
	_, err := fetch.NewHTTP(fetch.WithHTTPClient(client), fetch.WithResolver(resolver)).Get(context.Background(), "http://public.example/")
	if !errors.Is(err, fetch.ErrBlockedPrivateAddress) {
		t.Fatalf("custom client DNS private = %v, want ErrBlockedPrivateAddress", err)
	}
}

func TestHTTP_CustomTransportTLSHooksDoNotBypassPrivateAddressValidation(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: &http.Transport{
		DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("DialTLSContext bypassed guarded dialing")
			return nil, nil
		},
	}}
	resolver := &staticResolver{addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
	_, err := fetch.NewHTTP(fetch.WithHTTPClient(client), fetch.WithResolver(resolver)).Get(context.Background(), "https://public.example/")
	if !errors.Is(err, fetch.ErrBlockedPrivateAddress) {
		t.Fatalf("custom TLS transport private = %v, want ErrBlockedPrivateAddress", err)
	}
}

func TestHTTP_DialsReachableAllowedAddressWhenAnotherAddressHangs(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split test server URL: %v", err)
	}

	resolver := &staticResolver{addrs: []netip.Addr{
		netip.MustParseAddr("93.184.216.34"),
		netip.MustParseAddr("93.184.216.35"),
	}}
	var mu sync.Mutex
	var dialed []string
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, address)
		mu.Unlock()
		if strings.HasPrefix(address, "93.184.216.34:") {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		var d net.Dialer
		return d.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
	}

	resource, err := fetch.NewHTTP(fetch.WithResolver(resolver), fetch.WithDialContextForTests(dial)).Get(context.Background(), "http://public.example/")
	if err != nil {
		t.Fatalf("Get public host with one hanging address: %v", err)
	}
	if string(resource.Body) != "ok" {
		t.Fatalf("body = %q, want ok", string(resource.Body))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dialed) != 2 {
		t.Fatalf("dialed = %v, want two attempts", dialed)
	}
}

func TestHTTP_ClosesSuccessfulLosingDial(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split test server URL: %v", err)
	}

	resolver := &staticResolver{addrs: []netip.Addr{
		netip.MustParseAddr("93.184.216.34"),
		netip.MustParseAddr("93.184.216.35"),
	}}
	loserClosed := make(chan struct{})
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if strings.HasPrefix(address, "93.184.216.35:") {
			<-ctx.Done()
			client, peer := net.Pipe()
			return &trackingConn{Conn: client, peer: peer, closed: loserClosed}, nil
		}
		var d net.Dialer
		return d.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
	}

	resource, err := fetch.NewHTTP(fetch.WithResolver(resolver), fetch.WithDialContextForTests(dial)).Get(context.Background(), "http://public.example/")
	if err != nil {
		t.Fatalf("Get public host: %v", err)
	}
	if string(resource.Body) != "ok" {
		t.Fatalf("body = %q, want ok", string(resource.Body))
	}
	select {
	case <-loserClosed:
	case <-time.After(time.Second):
		t.Fatal("successful losing dial was not closed")
	}
}

type staticResolver struct {
	addrs []netip.Addr
	calls int
}

func (r *staticResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	r.calls++
	return append([]netip.Addr(nil), r.addrs...), nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type trackingConn struct {
	net.Conn
	peer   net.Conn
	closed chan struct{}
	once   sync.Once
}

func (c *trackingConn) Close() error {
	c.once.Do(func() {
		close(c.closed)
		_ = c.peer.Close()
	})
	return c.Conn.Close()
}

var _ fetch.Resolver = (*staticResolver)(nil)
