// Package fetch defines the URL-fetching port and an in-memory fake.
//
// The production adapter is an HTTP client with UA, timeout, body-size caps, and
// an SSRF guard (Phase 6); [Fake] is the scriptable double used by pipeline tests.
package fetch

import (
	"bytes"
	"context"
	"slices"
	"sync"
)

// Resource is the result of fetching a URL.
type Resource struct {
	// Body is the fetched response body.
	Body []byte
	// ContentType is the response Content-Type header.
	ContentType string
	// FinalURL is the URL after following redirects.
	FinalURL string
	// Status is the HTTP status code.
	Status int
}

// Fetcher performs an HTTP GET, returning the fetched resource.
type Fetcher interface {
	Get(ctx context.Context, url string) (Resource, error)
}

// Fake is a concurrency-safe, scriptable [Fetcher] for tests. Set the scripted
// fields before first use (they are read under the lock but not meant to change
// once workers may call concurrently); requested URLs are recorded for assertions.
type Fake struct {
	// Resource is returned on success.
	Resource Resource
	// Err, when non-nil, is returned instead of Resource.
	Err error

	mu    sync.Mutex
	calls int
	urls  []string
}

// Get records the requested URL and returns the scripted resource or error.
func (f *Fake) Get(ctx context.Context, url string) (Resource, error) {
	if err := ctx.Err(); err != nil {
		return Resource{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	f.urls = append(f.urls, url)

	if f.Err != nil {
		return Resource{}, f.Err
	}
	// Copy the body so a caller mutating it cannot corrupt the script.
	r := f.Resource
	r.Body = bytes.Clone(f.Resource.Body)
	return r, nil
}

// Calls is the number of times Get was invoked.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}

// URLs returns every URL passed to Get, in call order.
func (f *Fake) URLs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.urls)
}
