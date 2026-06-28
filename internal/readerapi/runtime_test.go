package readerapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bbell/reading-lite/internal/blobs"
	"github.com/bbell/reading-lite/internal/clock"
	"github.com/bbell/reading-lite/internal/config"
	"github.com/bbell/reading-lite/internal/dispatch"
	"github.com/bbell/reading-lite/internal/embed"
	"github.com/bbell/reading-lite/internal/extract"
	"github.com/bbell/reading-lite/internal/fetch"
	"github.com/bbell/reading-lite/internal/notify"
	"github.com/bbell/reading-lite/internal/readerapi"
	"github.com/bbell/reading-lite/internal/store"
	"github.com/bbell/reading-lite/internal/summarize"
	"github.com/bbell/reading-lite/internal/vector"
)

var testNow = time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)

func TestMainRejectsInvalidConfigBeforeSideEffects(t *testing.T) {
	t.Parallel()

	var stderr strings.Builder
	var called bool
	code := readerapi.MainWithOptions(nil, nil, &stderr, readerapi.Options{
		OpenPool: func(context.Context, config.Config) (readerapi.Pool, error) {
			called = true
			return nil, nil
		},
	})
	if code == 0 {
		t.Fatal("MainWithOptions code = 0, want config failure")
	}
	if called {
		t.Fatal("OpenPool was called despite invalid config")
	}
	if !strings.Contains(stderr.String(), "READER_API_TOKEN") {
		t.Fatalf("stderr = %q, want config field name", stderr.String())
	}
}

func TestMainDefaultsBuildInfoAndLogger(t *testing.T) {
	t.Parallel()

	var stderr strings.Builder
	code := readerapi.MainWithOptions(nil, validEnv(), &stderr, readerapi.Options{
		OpenPool:        func(context.Context, config.Config) (readerapi.Pool, error) { return &fakePool{}, nil },
		ApplyMigrations: func(context.Context, readerapi.Pool) error { return nil },
		BuildComponents: func(context.Context, config.Config, readerapi.Pool) (readerapi.Components, error) {
			return fakeComponents(), nil
		},
		RunWorkers: func(context.Context, *dispatch.Dispatcher) {},
		Sweep:      func(context.Context, *dispatch.Dispatcher) error { return nil },
		Serve: func(server *http.Server) error {
			req := httptest.NewRequest(http.MethodGet, "/api/healthz", nil)
			rr := httptest.NewRecorder()
			server.Handler.ServeHTTP(rr, req)
			var body struct {
				Build struct {
					Version string `json:"version"`
					Commit  string `json:"commit"`
					Date    string `json:"date"`
				} `json:"build"`
			}
			if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
				t.Fatalf("decode health body: %v", err)
			}
			if body.Build.Version == "" || body.Build.Commit == "" || body.Build.Date == "" {
				t.Fatalf("build = %+v, want non-empty defaults", body.Build)
			}
			return http.ErrServerClosed
		},
	})
	if code != 0 {
		t.Fatalf("MainWithOptions code = %d, stderr %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "reader_api_starting") {
		t.Fatalf("stderr log = %q, want startup log", stderr.String())
	}
}

func TestRun_PassesCORSAllowedOriginsToHTTPServer(t *testing.T) {
	t.Parallel()

	cfg := validConfig(t)
	cfg.CORSAllowedOrigins = []string{"https://app.example.com"}
	var gotStatus int
	var gotAllowOrigin string
	err := readerapi.Run(context.Background(), cfg, readerapi.Options{
		OpenPool:        func(context.Context, config.Config) (readerapi.Pool, error) { return &fakePool{}, nil },
		ApplyMigrations: func(context.Context, readerapi.Pool) error { return nil },
		BuildComponents: func(context.Context, config.Config, readerapi.Pool) (readerapi.Components, error) {
			return fakeComponents(), nil
		},
		RunWorkers: func(context.Context, *dispatch.Dispatcher) {},
		Sweep:      func(context.Context, *dispatch.Dispatcher) error { return nil },
		Serve: func(server *http.Server) error {
			req := httptest.NewRequest(http.MethodGet, "/api/healthz", nil)
			req.Header.Set("Origin", "https://app.example.com")
			rr := httptest.NewRecorder()
			server.Handler.ServeHTTP(rr, req)
			gotStatus = rr.Code
			gotAllowOrigin = rr.Header().Get("Access-Control-Allow-Origin")
			return http.ErrServerClosed
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotStatus != http.StatusOK {
		t.Fatalf("health status = %d, want 200", gotStatus)
	}
	if gotAllowOrigin != "https://app.example.com" {
		t.Fatalf("allow-origin = %q, want configured origin", gotAllowOrigin)
	}
}

func TestRun_MigrationsAndSweepGateServing(t *testing.T) {
	t.Parallel()

	var events []string
	workersStarted := make(chan struct{})
	pool := &fakePool{events: &events}
	cfg := validConfig(t)
	err := readerapi.Run(context.Background(), cfg, readerapi.Options{
		OpenPool: func(context.Context, config.Config) (readerapi.Pool, error) {
			events = append(events, "open")
			return pool, nil
		},
		ApplyMigrations: func(context.Context, readerapi.Pool) error {
			events = append(events, "migrate")
			return nil
		},
		BuildComponents: func(context.Context, config.Config, readerapi.Pool) (readerapi.Components, error) {
			events = append(events, "build")
			return fakeComponents(), nil
		},
		RunWorkers: func(ctx context.Context, _ *dispatch.Dispatcher) {
			events = append(events, "workers")
			close(workersStarted)
			<-ctx.Done()
		},
		Sweep: func(context.Context, *dispatch.Dispatcher) error {
			<-workersStarted
			events = append(events, "sweep")
			return nil
		},
		Serve: func(server *http.Server) error {
			if server.ReadHeaderTimeout <= 0 {
				t.Fatal("server ReadHeaderTimeout is not configured")
			}
			if server.ReadTimeout <= 0 {
				t.Fatal("server ReadTimeout is not configured")
			}
			events = append(events, "serve")
			return http.ErrServerClosed
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"open", "migrate", "build", "workers", "sweep", "serve", "close"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRun_MigrationFailureCleansUpBeforeServing(t *testing.T) {
	t.Parallel()

	var events []string
	pool := &fakePool{events: &events}
	err := readerapi.Run(context.Background(), validConfig(t), readerapi.Options{
		OpenPool: func(context.Context, config.Config) (readerapi.Pool, error) {
			events = append(events, "open")
			return pool, nil
		},
		ApplyMigrations: func(context.Context, readerapi.Pool) error {
			events = append(events, "migrate")
			return errors.New("db password secret")
		},
		BuildComponents: func(context.Context, config.Config, readerapi.Pool) (readerapi.Components, error) {
			events = append(events, "build")
			return fakeComponents(), nil
		},
		Serve: func(*http.Server) error {
			events = append(events, "serve")
			return nil
		},
	})
	if err == nil {
		t.Fatal("Run = nil error, want migration failure")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("Run error leaked secret: %v", err)
	}
	want := []string{"open", "migrate", "close"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRun_ContextCancelMarksHealthDegradedBeforeShutdown(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	healthSeenDegraded := false
	err := readerapi.Run(ctx, validConfig(t), readerapi.Options{
		OpenPool:        func(context.Context, config.Config) (readerapi.Pool, error) { return &fakePool{}, nil },
		ApplyMigrations: func(context.Context, readerapi.Pool) error { return nil },
		BuildComponents: func(context.Context, config.Config, readerapi.Pool) (readerapi.Components, error) {
			return fakeComponents(), nil
		},
		RunWorkers: func(context.Context, *dispatch.Dispatcher) {},
		Sweep: func(context.Context, *dispatch.Dispatcher) error {
			cancel()
			return nil
		},
		Serve: func(*http.Server) error {
			<-ctx.Done()
			return http.ErrServerClosed
		},
		Shutdown: func(_ context.Context, srv *http.Server) error {
			rr := httptestResponse(srv.Handler)
			healthSeenDegraded = rr.StatusCode == http.StatusServiceUnavailable
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !healthSeenDegraded {
		t.Fatal("health was not degraded before Shutdown")
	}
}

func TestRun_ContextCancelBoundsWorkerDrain(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cfg := validConfig(t)
	cfg.ShutdownTimeout = 10 * time.Millisecond
	err := readerapi.Run(ctx, cfg, readerapi.Options{
		OpenPool:        func(context.Context, config.Config) (readerapi.Pool, error) { return &fakePool{}, nil },
		ApplyMigrations: func(context.Context, readerapi.Pool) error { return nil },
		BuildComponents: func(context.Context, config.Config, readerapi.Pool) (readerapi.Components, error) {
			return fakeComponents(), nil
		},
		RunWorkers: func(context.Context, *dispatch.Dispatcher) {
			select {}
		},
		Sweep: func(context.Context, *dispatch.Dispatcher) error {
			cancel()
			return nil
		},
		Serve: func(*http.Server) error {
			<-ctx.Done()
			return http.ErrServerClosed
		},
		Shutdown: func(context.Context, *http.Server) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "drain workers") {
		t.Fatalf("Run = %v, want bounded worker drain error", err)
	}
}

func TestRun_ContextCancelBoundsServeDrain(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cfg := validConfig(t)
	cfg.ShutdownTimeout = 10 * time.Millisecond
	err := readerapi.Run(ctx, cfg, readerapi.Options{
		OpenPool:        func(context.Context, config.Config) (readerapi.Pool, error) { return &fakePool{}, nil },
		ApplyMigrations: func(context.Context, readerapi.Pool) error { return nil },
		BuildComponents: func(context.Context, config.Config, readerapi.Pool) (readerapi.Components, error) {
			return fakeComponents(), nil
		},
		RunWorkers: func(context.Context, *dispatch.Dispatcher) {},
		Sweep: func(context.Context, *dispatch.Dispatcher) error {
			cancel()
			return nil
		},
		Serve: func(*http.Server) error {
			select {}
		},
		Shutdown: func(context.Context, *http.Server) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "drain http server") {
		t.Fatalf("Run = %v, want bounded serve drain error", err)
	}
}

func TestRun_ShutdownFailureStillBoundsWorkerDrain(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cfg := validConfig(t)
	cfg.ShutdownTimeout = 10 * time.Millisecond
	err := readerapi.Run(ctx, cfg, readerapi.Options{
		OpenPool:        func(context.Context, config.Config) (readerapi.Pool, error) { return &fakePool{}, nil },
		ApplyMigrations: func(context.Context, readerapi.Pool) error { return nil },
		BuildComponents: func(context.Context, config.Config, readerapi.Pool) (readerapi.Components, error) {
			return fakeComponents(), nil
		},
		RunWorkers: func(context.Context, *dispatch.Dispatcher) {
			select {}
		},
		Sweep: func(context.Context, *dispatch.Dispatcher) error {
			cancel()
			return nil
		},
		Serve: func(*http.Server) error {
			<-ctx.Done()
			return http.ErrServerClosed
		},
		Shutdown: func(context.Context, *http.Server) error { return errors.New("shutdown failed with secret") },
	})
	if err == nil || !strings.Contains(err.Error(), "drain workers") {
		t.Fatalf("Run = %v, want worker drain error before shutdown error", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("Run error leaked shutdown detail: %v", err)
	}
}

func validConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.LoadEnv(validEnv())
	if err != nil {
		t.Fatalf("LoadEnv: %v", err)
	}
	return cfg
}

func validEnv() []string {
	return []string{
		"READER_API_TOKEN=api-token",
		"DATABASE_URL=postgres://reader:secret@db.example.com:5432/reading?sslmode=require",
		"OPENAI_API_KEY=openai-key",
		"ANTHROPIC_API_KEY=anthropic-key",
		"R2_ENDPOINT=https://account.r2.cloudflarestorage.com",
		"R2_ACCESS_KEY_ID=r2-access",
		"R2_SECRET_ACCESS_KEY=r2-secret",
		"R2_BUCKET=reading",
		"PENDING_TTL=15m",
		"RUNNING_TTL=20m",
		"MAX_ATTEMPTS=5",
		"WORKER_CONCURRENCY=3",
		"DISPATCH_BUFFER=64",
		"PG_MAX_CONNS=7",
		"LISTEN_ADDR=127.0.0.1:8080",
		"RESEND_API_KEY=resend-key",
		"NOTIFY_FROM=reader@example.com",
		"NOTIFY_TO=me@example.com",
	}
}

func fakeComponents() readerapi.Components {
	return readerapi.Components{
		Store:      store.NewMemory(),
		Blobs:      blobs.NewMemory(),
		Fetcher:    &fetch.Fake{},
		Extractor:  &extract.Fake{},
		YouTube:    extract.NewYouTube(),
		Embedder:   &embed.Fake{},
		Vectors:    vector.NewMemory(),
		Summarizer: &summarize.Fake{},
		Notifier:   &notify.Fake{},
		Clock:      clock.NewFake(testNow),
	}
}

type fakePool struct {
	events *[]string
}

func (p *fakePool) Close() {
	if p.events != nil {
		*p.events = append(*p.events, "close")
	}
}

func (p *fakePool) Ping(context.Context) error { return nil }

func httptestResponse(h http.Handler) *http.Response {
	req, _ := http.NewRequest(http.MethodGet, "/api/healthz", nil)
	rr := &responseRecorder{header: http.Header{}}
	h.ServeHTTP(rr, req)
	return &http.Response{StatusCode: rr.status, Header: rr.header}
}

type responseRecorder struct {
	header http.Header
	status int
}

func (r *responseRecorder) Header() http.Header { return r.header }
func (r *responseRecorder) Write([]byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return 0, nil
}
func (r *responseRecorder) WriteHeader(status int) { r.status = status }
