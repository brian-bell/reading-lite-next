# reading-lite

`reading-lite` is being rebuilt as a Go backend for a personal reading service. The current
checkout has completed Phase 6 of `docs/PLAN.md`: project tooling, CI conventions,
placeholder binaries, deterministic clock support, the pure reading domain core, the
readings metadata store behind a shared conformance suite, the in-process dispatcher
with retry/backoff, rate-limit re-dispatch, retry-exhaustion, and a crash-recovery sweep,
the external-service ports (`fetch`, `extract`, `embed`, `vector`, `summarize`,
`notify`, `blobs`) with their in-memory fakes, the processing pipeline that wires
them together (fetch→extract→blobs→embed→vector→summarize→notify), and the real
production adapters behind those ports — `fetch.HTTP`, `embed.OpenAI`,
`summarize.Anthropic` (forced `emit_reading` tool use), `notify.Resend`, `vector.Postgres`
(pgvector), and `blobs.R2` (S3-compatible) — each pinned by a contract test (`httptest`
upstreams for the HTTP adapters; testcontainers Postgres/MinIO under `//go:build integration`
for the DB/object-store adapters). The extraction internals (Phase 7), HTTP API, and `main`
wiring are still pending: nothing constructs these adapters from `main` yet.

## Structure

- `cmd/reader-api/` contains the API process entrypoint. It is currently a minimal placeholder
  until the HTTP server and worker pool are implemented.
- `cmd/readerctl/` contains the operator CLI entrypoint. It is currently a minimal placeholder
  until CLI subcommands are implemented.
- `internal/clock/` defines the clock port, real system clock, and mutex-protected fake clock
  used by concurrent tests.
- `internal/reading/` defines the pure domain core: reading lifecycle statuses, explicit status
  transitions, terminal-state checks, URL idempotency key normalization, source classification,
  and read-time stale annotation.
- `internal/store/` defines the `Store` port, shared query/page DTOs, `store.Memory`, the
  pgx-backed `store.Postgres` adapter, embedded migrations, SQL query source for sqlc, and
  `storetest.RunContract` for backend-neutral behavior checks. `UpdateStatus` advances the
  lifecycle; `UpdateContent` overwrites the processed-content columns (title/summary/keys and
  the `summary_json`/`similar_json`/`diagnostics_json` blobs) the pipeline produces, leaving
  status, timestamps, error, attempts, and tags alone.
- `internal/dispatch/` defines the in-process dispatcher: the pure retry-decision function
  and error classifier (`decide`/`Classify`, with `RateLimitError`/`ErrPermanent`), an
  injectable delay seam (`Delayer` with a real timer and a fireable fake), a worker pool that
  drains an in-memory channel and persists each run's lifecycle outcome, and a startup
  `Sweep` that re-dispatches readings left non-terminal by a crash, resuming each at its
  stored attempt count.
- `internal/fetch/`, `internal/extract/`, `internal/embed/`, `internal/summarize/`, and
  `internal/notify/` define the external-service ports (`Fetcher`, `Extractor`, `Embedder`,
  `Summarizer`, `Notifier`) and a concurrency-safe, scriptable in-memory `Fake` for each.
  `extract` consumes a `fetch.Resource`. Each (except `extract`, whose readability adapter is
  Phase 7) now also ships its real adapter: `fetch.HTTP` (UA, timeout, body-size cap,
  redirect→`FinalURL`, non-http(s) scheme rejection, and a 429→`dispatch.RateLimitError`
  requeue — the private-IP SSRF guard is deferred to Phase 11), `embed.OpenAI`
  (`/v1/embeddings`, `text-embedding-3-small`), `summarize.Anthropic`
  (Messages API with forced `emit_reading` tool use), and `notify.Resend` (`/emails`). The HTTP
  adapters take a `WithBaseURL` option so a contract test can point them at an `httptest`
  upstream, and classify upstream failures through `internal/httpx`.
- `internal/httpx/` holds helpers shared by the HTTP service adapters: `ClassifyResponse` maps a
  non-2xx response to a dispatcher-classified error (429 → `dispatch.RateLimitError` honoring
  `Retry-After`, other 4xx → `dispatch.ErrPermanent`, 5xx → transient), and `RetryAfter` parses
  the header. This keeps `embed.OpenAI` and `summarize.Anthropic` error semantics identical to
  `dispatch.Classify`.
- `internal/blobs/` defines the `Blobs` content-blob port, `blobs.Memory` (in-memory), and
  `blobs.R2` — the production S3-compatible adapter (aws-sdk-go-v2, path-style, custom endpoint;
  maps a missing key to `blobs.ErrNotFound`). `R2` is exercised by an `httptest` S3 stub
  (request-shape + round-trip) on every run and a MinIO container under `//go:build integration`.
- `internal/vector/` defines the `Index` similarity port (the VectorIndex port; renamed from
  `VectorIndex` to avoid a revive stutter), `vector.Memory` (a real cosine-similarity index),
  `vector.Postgres` (the pgvector adapter over `reading_vectors`, ranking by cosine distance via
  `<=>`), and `vectortest.RunContract` — the backend-neutral suite both backends satisfy
  (`vector.Memory` on every run, `vector.Postgres` under `//go:build integration`).
- `internal/pipeline/` defines the processing pipeline. `Pipeline.Process` is the dispatcher's
  `Handler`: it loads a reading, acquires content (markdown imports read the stored body;
  Reddit fails permanently with guidance; everything else fetches + extracts), embeds and
  upserts a vector, snapshots similar readings, summarizes once, optionally notifies
  (a notify failure never fails the reading), and persists the content via `store.UpdateContent`
  + `ReplaceTags`. It returns a `dispatch.Result` (the dispatcher owns lifecycle status; the
  pipeline owns content). Re-entry is idempotent: a persisted `content_key` checkpoint lets a
  retried run skip fetch/extract/embed and resume at summarize. Blob keys are derived from the
  server-side reading id.
- `docs/PLAN.md` is the implementation contract for the backend phases.
- `.github/workflows/ci.yml`, `Makefile`, and `.golangci.yml` define the Phase 0 toolchain
  conventions.

## Commands

The project targets Go 1.26.

- `make test` runs the default fast test suite with fakes only.
- `make verify` runs the blackbox verification harness in `internal/acceptance/`
  (build tag `verify`): build/vet/gofmt/lint, sqlc-drift, conventions, and
  cross-package behavioral acceptance. The store contract and reading lifecycle run
  against both `store.Memory` and a testcontainers Postgres (the Postgres backend
  skips without Docker, or uses `DATABASE_URL`). It automates
  `docs/ACCEPTANCE.md`.
- `make test-race` runs `go test -race ./...`.
- `make cover` runs `go test -race -cover ./...`.
- `make test-integration` runs tests behind the `integration` build tag. Store integration
  tests use `DATABASE_URL` when set; otherwise they use testcontainers with `pgvector/pgvector`
  and skip when Docker is unavailable.
- `make lint` checks `gofmt`, `go vet`, and `golangci-lint`.
- `make build` runs `go build ./...`.
- `make run` runs `cmd/reader-api`.
- `make migrate` runs `cmd/readerctl migrate`.
- `make sqlc` runs `sqlc generate`.

## Conventions

- Write tests before implementation, one vertical behavior at a time.
- Prefer black-box test packages such as `clock_test` unless an unexported helper is the right
  boundary.
- Keep default tests deterministic: no wall-clock, RNG, network, Docker, or live services.
- Keep `internal/reading` dependency-free outside the Go standard library.
- Use injected ports for time, IDs, stores, vectors, fetchers, extractors, embedders,
  summarizers, notifiers, and blobs.
- Put fakes next to their ports and make them safe for concurrent test use when workers may
  read them.
- Keep integration tests behind `//go:build integration`.
- Add store behavior to `internal/store/storetest` first, then make `store.Memory` and
  `store.Postgres` satisfy the same contract. Likewise, add vector-index behavior to
  `internal/vector/vectortest` first, then make `vector.Memory` and the pgvector adapter
  satisfy it.
- Scriptable port fakes expose their configured response/error as fields set before use and
  guard call recording behind a mutex; return defensive copies so callers cannot corrupt the
  script.
- Give each real HTTP adapter a `NewX(key, ...Option)` constructor with a `WithBaseURL` (and
  `WithHTTPClient`) seam, and contract-test it against an `httptest` upstream — assert the
  request shape (auth, path, body) and the happy parse, plus at least one error-mapping case.
  Route every adapter's upstream-error classification through `internal/httpx` so it agrees
  with `dispatch.Classify`. Keep DB/object-store adapters (`vector.Postgres`, `blobs.R2`) out
  of the default run: prove them with their conformance/round-trip suites under
  `//go:build integration` (testcontainers), and rely on compile-time `var _ Port = (*Adapter)(nil)`
  conformance assertions in the acceptance harness.
- Keep retry/backoff logic in pure functions (`dispatch.decide`, `dispatch.Classify`) and run
  delays through the injected `dispatch.Delayer` seam so retry, backoff, rate-limit, and
  recovery semantics test deterministically without real goroutines, timers, or sleeps.
- Keep the pipeline's status/content split: the dispatcher owns lifecycle status (it marks
  running/ready/failed/pending), the pipeline owns content. `Pipeline.Process` maps step errors
  to outcomes through the shared `dispatch.Classify`, and persists a content checkpoint before
  the summarize step so a retried run resumes idempotently from the stored `content_key`.
- Drive pipeline tests through an inline `dispatch.Dispatcher` (`Inline: true`) with a
  `dispatch.FakeDelayer`, so a test exercises real status transitions and asserts the pipeline's
  effects on the store/blobs/vector index and its scripted port fakes.
- Use table-driven subtests and `t.Parallel()` when there is no shared mutable state.
