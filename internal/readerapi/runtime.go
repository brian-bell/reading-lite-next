// Package readerapi wires the production reader-api process.
package readerapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bbell/reading-lite/internal/blobs"
	"github.com/bbell/reading-lite/internal/clock"
	"github.com/bbell/reading-lite/internal/config"
	"github.com/bbell/reading-lite/internal/dispatch"
	"github.com/bbell/reading-lite/internal/embed"
	"github.com/bbell/reading-lite/internal/extract"
	"github.com/bbell/reading-lite/internal/fetch"
	"github.com/bbell/reading-lite/internal/httpapi"
	"github.com/bbell/reading-lite/internal/notify"
	"github.com/bbell/reading-lite/internal/pipeline"
	"github.com/bbell/reading-lite/internal/reading"
	"github.com/bbell/reading-lite/internal/store"
	"github.com/bbell/reading-lite/internal/summarize"
	"github.com/bbell/reading-lite/internal/vector"
)

const (
	serverReadHeaderTimeout = 5 * time.Second
	serverReadTimeout       = 30 * time.Second
)

// Build metadata is set by release builds with -ldflags -X.
var (
	BuildVersion = "dev"
	BuildCommit  = "unknown"
	BuildDate    = "unknown"
)

// Pool is the database pool surface the runtime owns.
type Pool interface {
	Close()
	Ping(context.Context) error
}

// Components are the production adapters wired into the HTTP server and worker.
type Components struct {
	Store      store.Store
	Blobs      blobs.Blobs
	Fetcher    fetch.Fetcher
	Extractor  extract.Extractor
	YouTube    pipeline.YouTubeExtractor
	Embedder   embed.Embedder
	Vectors    vector.Index
	Summarizer summarize.Summarizer
	Notifier   notify.Notifier
	Clock      clock.Clock
	Close      func()
}

// Options overrides runtime side effects for tests.
type Options struct {
	Build           httpapi.BuildInfo
	Logger          *slog.Logger
	OpenPool        func(context.Context, config.Config) (Pool, error)
	ApplyMigrations func(context.Context, Pool) error
	BuildComponents func(context.Context, config.Config, Pool) (Components, error)
	RunWorkers      func(context.Context, *dispatch.Dispatcher)
	Sweep           func(context.Context, *dispatch.Dispatcher) error
	Serve           func(*http.Server) error
	Shutdown        func(context.Context, *http.Server) error
}

// Main loads environment configuration, runs the process, and returns a process
// exit code. args is reserved for future flags.
func Main(args []string, environ []string, _ io.Writer, stderr io.Writer) int {
	return MainWithOptions(args, environ, stderr, Options{})
}

// MainWithOptions is Main with injectable side effects for tests.
func MainWithOptions(_ []string, environ []string, stderr io.Writer, opts Options) int {
	cfg, err := config.LoadEnv(environ)
	if err != nil {
		writeLine(stderr, err.Error())
		return 2
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewJSONHandler(defaultLogWriter(stderr), nil))
	}
	if opts.Build == (httpapi.BuildInfo{}) {
		opts.Build = defaultBuildInfo()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := Run(ctx, cfg, opts); err != nil {
		writeLine(stderr, err.Error())
		return 1
	}
	return 0
}

// Run starts the configured API server and worker pool until ctx is cancelled.
func Run(ctx context.Context, cfg config.Config, opts Options) error {
	opts = withDefaults(opts)
	if opts.Logger != nil {
		opts.Logger.Info("reader_api_starting", "config", config.LogValue(cfg))
	}

	pool, err := opts.OpenPool(ctx, cfg)
	if err != nil {
		return redactedError("open postgres pool")
	}
	defer pool.Close()

	if err := opts.ApplyMigrations(ctx, pool); err != nil {
		return redactedError("apply migrations")
	}

	components, err := opts.BuildComponents(ctx, cfg, pool)
	if err != nil {
		return redactedError("construct adapters")
	}
	if components.Close != nil {
		defer components.Close()
	}
	if components.Clock == nil {
		components.Clock = clock.System{}
	}

	pipe := &pipeline.Pipeline{
		Store:      components.Store,
		Blobs:      components.Blobs,
		Fetcher:    components.Fetcher,
		Extractor:  components.Extractor,
		YouTube:    components.YouTube,
		Embedder:   components.Embedder,
		Vectors:    components.Vectors,
		Summarizer: components.Summarizer,
		Notifier:   components.Notifier,
		Clock:      components.Clock,
		Config: pipeline.Config{
			TopK:          5,
			NotifyEnabled: true,
			NotifyFrom:    cfg.Notify.From,
			NotifyTo:      cfg.Notify.To,
		},
	}
	dispatcher := &dispatch.Dispatcher{
		Handler:    pipe.Process,
		Store:      components.Store,
		Clock:      components.Clock,
		Delay:      dispatch.RealDelayer{},
		Workers:    cfg.WorkerConcurrency,
		Max:        cfg.MaxAttempts,
		Buffer:     cfg.DispatchBuffer,
		RunningTTL: cfg.RunningTTL,
	}
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	workersDone := make(chan struct{})
	go func() {
		defer close(workersDone)
		opts.RunWorkers(workerCtx, dispatcher)
	}()

	workersCleaned := false
	cleanupWorkers := func() error {
		if workersCleaned {
			return nil
		}
		workersCleaned = true
		cancelWorkers()
		timer := time.NewTimer(cfg.ShutdownTimeout)
		defer timer.Stop()
		select {
		case <-workersDone:
			return nil
		case <-timer.C:
			return redactedError("drain workers")
		}
	}
	defer func() { _ = cleanupWorkers() }()

	if err := opts.Sweep(ctx, dispatcher); err != nil {
		if cleanupErr := cleanupWorkers(); cleanupErr != nil {
			return cleanupErr
		}
		return redactedError("startup sweep")
	}

	health := &httpapi.HealthState{}
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		ReadTimeout:       serverReadTimeout,
		Handler: (&httpapi.Server{
			Store:      components.Store,
			Dispatcher: dispatcher,
			Blobs:      components.Blobs,
			Clock:      components.Clock,
			Token:      cfg.APIToken,
			TTLs:       readingTTLs(cfg),
			NewID:      nextID,
			Build:      opts.Build,
			Health:     health,
			HealthChecks: httpapi.HealthChecks{
				Postgres: pool.Ping,
				R2:       r2HealthCheck(components.Blobs),
			},
			CORSAllowedOrigins: cfg.CORSAllowedOrigins,
			Logger:             opts.Logger,
		}).Routes(),
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- opts.Serve(server) }()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			if ctx.Err() != nil {
				health.MarkDegraded()
				shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
				_ = opts.Shutdown(shutdownCtx, server)
				cancel()
			}
			if err := cleanupWorkers(); err != nil {
				return err
			}
			return nil
		}
		if cleanupErr := cleanupWorkers(); cleanupErr != nil {
			return cleanupErr
		}
		return redactedError("serve http")
	case <-ctx.Done():
		health.MarkDegraded()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		shutdownErr := opts.Shutdown(shutdownCtx, server)
		cancel()
		if shutdownErr != nil {
			if cleanupErr := cleanupWorkers(); cleanupErr != nil {
				return cleanupErr
			}
			return redactedError("shutdown http")
		}
		if err := cleanupWorkers(); err != nil {
			return err
		}
		ok, err := waitServeErr(serveErr, cfg.ShutdownTimeout)
		if !ok {
			return redactedError("drain http server")
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return redactedError("serve http")
		}
		return nil
	}
}

func waitServeErr(serveErr <-chan error, timeout time.Duration) (bool, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-serveErr:
		return true, err
	case <-timer.C:
		return false, nil
	}
}

func withDefaults(opts Options) Options {
	if opts.OpenPool == nil {
		opts.OpenPool = openPostgresPool
	}
	if opts.ApplyMigrations == nil {
		opts.ApplyMigrations = applyStoreMigrations
	}
	if opts.BuildComponents == nil {
		opts.BuildComponents = BuildProductionComponents
	}
	if opts.RunWorkers == nil {
		opts.RunWorkers = func(ctx context.Context, d *dispatch.Dispatcher) { d.Run(ctx) }
	}
	if opts.Sweep == nil {
		opts.Sweep = func(ctx context.Context, d *dispatch.Dispatcher) error { return d.Sweep(ctx) }
	}
	if opts.Serve == nil {
		opts.Serve = serve
	}
	if opts.Shutdown == nil {
		opts.Shutdown = func(ctx context.Context, s *http.Server) error { return s.Shutdown(ctx) }
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	if opts.Build == (httpapi.BuildInfo{}) {
		opts.Build = defaultBuildInfo()
	}
	return opts
}

func defaultBuildInfo() httpapi.BuildInfo {
	return httpapi.BuildInfo{Version: BuildVersion, Commit: BuildCommit, Date: BuildDate}
}

func defaultLogWriter(w io.Writer) io.Writer {
	if w != nil {
		return w
	}
	return os.Stderr
}

func openPostgresPool(ctx context.Context, cfg config.Config) (Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	poolCfg.MaxConns = cfg.PGMaxConns
	return pgxpool.NewWithConfig(ctx, poolCfg)
}

func applyStoreMigrations(ctx context.Context, pool Pool) error {
	pgxPool, ok := pool.(*pgxpool.Pool)
	if !ok {
		return errors.New("postgres pool type mismatch")
	}
	return store.ApplyMigrations(ctx, pgxPool)
}

// BuildProductionComponents constructs the production store, blob, vector, and service adapters.
func BuildProductionComponents(_ context.Context, cfg config.Config, pool Pool) (Components, error) {
	pgxPool, ok := pool.(*pgxpool.Pool)
	if !ok {
		return Components{}, errors.New("postgres pool type mismatch")
	}
	b := blobs.NewR2(blobs.R2Config{
		Endpoint:        cfg.R2.Endpoint,
		Region:          cfg.R2.Region,
		AccessKeyID:     cfg.R2.AccessKeyID,
		SecretAccessKey: cfg.R2.SecretAccessKey,
		Bucket:          cfg.R2.Bucket,
	})
	return Components{
		Store:      store.NewPostgres(pgxPool),
		Blobs:      b,
		Fetcher:    fetch.NewHTTP(fetch.WithTimeout(cfg.FetchTimeout), fetch.WithMaxBytes(cfg.FetchMaxBytes)),
		Extractor:  extract.NewReadability(),
		YouTube:    extract.NewYouTube(),
		Embedder:   embed.NewOpenAI(cfg.OpenAIAPIKey),
		Vectors:    vector.NewPostgres(pgxPool),
		Summarizer: summarize.NewAnthropic(cfg.AnthropicAPIKey),
		Notifier:   notify.NewResend(cfg.ResendAPIKey),
		Clock:      clock.System{},
	}, nil
}

func serve(s *http.Server) error {
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

func r2HealthCheck(b blobs.Blobs) func(context.Context) error {
	return func(ctx context.Context) error {
		if checker, ok := b.(interface{ Health(context.Context) error }); ok {
			return checker.Health(ctx)
		}
		_, _, err := b.Get(ctx, "__reading-lite-healthcheck-missing__")
		if errors.Is(err, blobs.ErrNotFound) {
			return nil
		}
		return err
	}
}

func readingTTLs(cfg config.Config) reading.TTLs {
	return reading.TTLs{Pending: cfg.PendingTTL, Running: cfg.RunningTTL}
}

type redactedError string

func (e redactedError) Error() string { return "reader-api: " + string(e) + " failed" }

func writeLine(w io.Writer, s string) {
	if w == nil {
		return
	}
	_, _ = fmt.Fprintln(w, s)
}

var idSeq atomic.Uint64

func nextID() string {
	return fmt.Sprintf("r-%d-%d", time.Now().UTC().UnixNano(), idSeq.Add(1))
}
