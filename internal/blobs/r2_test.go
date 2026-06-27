package blobs_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bbell/reading-lite/internal/blobs"
)

// s3Stub is a minimal in-memory, path-style S3-compatible server: enough for a
// Put/Get/Delete round-trip and to capture the bucket/key/content-type the adapter
// composed. It ignores SigV4 signatures (it only verifies request shape).
type s3Stub struct {
	mu      sync.Mutex
	objects map[string]s3Object
	lastPut s3Request
}

type s3Object struct {
	data        []byte
	contentType string
}

type s3Request struct {
	method      string
	bucket      string
	key         string
	contentType string
}

func newS3Stub(t *testing.T) (*httptest.Server, *s3Stub) {
	t.Helper()
	stub := &s3Stub{objects: map[string]s3Object{}}
	srv := httptest.NewServer(http.HandlerFunc(stub.serve))
	t.Cleanup(srv.Close)
	return srv, stub
}

// lastPutRequest returns the most recent PUT the stub recorded, read under the
// same lock the handler writes it with so the stub's shared state has a single,
// consistent locking discipline.
func (s *s3Stub) lastPutRequest() s3Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastPut
}

// splitPath parses a path-style "/{bucket}/{key...}" request path.
func splitPath(p string) (bucket, key string) {
	p = strings.TrimPrefix(p, "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return p, ""
}

func (s *s3Stub) serve(w http.ResponseWriter, r *http.Request) {
	bucket, key := splitPath(r.URL.Path)
	id := bucket + "/" + key

	s.mu.Lock()
	defer s.mu.Unlock()

	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		ct := r.Header.Get("Content-Type")
		s.objects[id] = s3Object{data: body, contentType: ct}
		s.lastPut = s3Request{method: r.Method, bucket: bucket, key: key, contentType: ct}
		w.Header().Set("ETag", `"stub-etag"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		obj, ok := s.objects[id]
		if !ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<?xml version="1.0"?><Error><Code>NoSuchKey</Code><Message>not found</Message></Error>`))
			return
		}
		if obj.contentType != "" {
			w.Header().Set("Content-Type", obj.contentType)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(obj.data)
	case http.MethodDelete:
		delete(s.objects, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func newR2(t *testing.T, endpoint string) *blobs.R2 {
	t.Helper()
	return blobs.NewR2(blobs.R2Config{
		Endpoint:        endpoint,
		Region:          "auto",
		AccessKeyID:     "test-key",
		SecretAccessKey: "test-secret",
		Bucket:          "content",
	})
}

func TestR2_RoundTripAndComposition(t *testing.T) {
	t.Parallel()

	srv, stub := newS3Stub(t)
	r := newR2(t, srv.URL)
	ctx := context.Background()

	const key = "readings/r1/content.md"
	if err := r.Put(ctx, key, []byte("# hello"), "text/markdown"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Composition: the adapter targets the configured bucket, the exact key, and
	// the supplied content type (path-style, so the request path is /bucket/key).
	put := stub.lastPutRequest()
	if put.bucket != "content" {
		t.Errorf("PUT bucket = %q, want content", put.bucket)
	}
	if put.key != key {
		t.Errorf("PUT key = %q, want %q", put.key, key)
	}
	if put.contentType != "text/markdown" {
		t.Errorf("PUT content-type = %q, want text/markdown", put.contentType)
	}

	// Round-trip: Get returns the stored bytes and content type.
	data, ct, err := r.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(data) != "# hello" {
		t.Errorf("Get data = %q, want # hello", data)
	}
	if !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("Get content-type = %q, want text/markdown", ct)
	}

	// Delete removes it; a subsequent Get reports ErrNotFound.
	if err := r.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := r.Get(ctx, key); !errors.Is(err, blobs.ErrNotFound) {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
}

func TestR2_GetMissingKeyIsErrNotFound(t *testing.T) {
	t.Parallel()

	srv, _ := newS3Stub(t)
	r := newR2(t, srv.URL)

	if _, _, err := r.Get(context.Background(), "readings/missing/raw"); !errors.Is(err, blobs.ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
}

func TestR2_HealthMissingKeyIsReachable(t *testing.T) {
	t.Parallel()

	srv, _ := newS3Stub(t)
	r := newR2(t, srv.URL)

	if err := r.Health(context.Background()); err != nil {
		t.Fatalf("Health missing key = %v, want nil", err)
	}
}

func TestR2_HealthMissingBucketIsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<?xml version="1.0"?><Error><Code>NoSuchBucket</Code><Message>bucket not found</Message></Error>`))
	}))
	defer srv.Close()
	r := newR2(t, srv.URL)

	if err := r.Health(context.Background()); err == nil {
		t.Fatal("Health missing bucket = nil, want error")
	}
}
