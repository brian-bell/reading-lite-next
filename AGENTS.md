# reading-lite

`reading-lite` is being rebuilt as a Go backend for a personal reading service. The current
checkout has completed Phase 5 of `docs/PLAN.md`: project tooling, CI conventions,
placeholder binaries, deterministic clock support, the pure reading domain core, the
readings metadata store behind a shared conformance suite, the in-process dispatcher
with retry/backoff, rate-limit re-dispatch, retry-exhaustion, and a crash-recovery sweep,
the external-service ports (`fetch`, `extract`, `embed`, `vector`, `summarize`,
`notify`, `blobs`) with their in-memory fakes, and the processing pipeline that wires
them together (fetch→extract→blobs→embed→vector→summarize→notify) — no real
network/SDK code yet.

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
  `extract` consumes a `fetch.Resource`; the production HTTP/SDK adapters land in later phases.
- `internal/blobs/` defines the `Blobs` content-blob port and `blobs.Memory`, an in-memory
  store of raw and extracted payloads keyed by server-derived key.
- `internal/vector/` defines the `Index` similarity port (the VectorIndex port; renamed from
  `VectorIndex` to avoid a revive stutter), `vector.Memory` (a real cosine-similarity index),
  and `vectortest.RunContract` — the backend-neutral suite both `vector.Memory` and the future
  pgvector adapter must satisfy.
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
