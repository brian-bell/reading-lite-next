# reading-lite

`reading-lite` is a Go backend for a personal reading service with an isolated SPA bootstrap.
The codebase contains the reading domain core, persistence ports and adapters, an in-process
dispatcher, the processing pipeline, real adapters for external services, extraction
internals, an HTTP API package, the command service that coordinates multi-resource workflows,
and a separate Vite/React package under `web/`.

The service can ingest URLs, import markdown and bookmark files, fetch and extract source
content, store raw and processed blobs, embed and index readings for similarity, summarize
readings, tag them, and optionally send notifications. The HTTP API is implemented and
tested as a package. The production `cmd/reader-api` binary now validates env config, runs
embedded store migrations, constructs production adapters, starts the dispatcher worker pool,
runs startup recovery, serves HTTP, reports dependency health, handles exact-origin CORS for
configured SPA origins, and shuts down gracefully. The `cmd/readerctl` binary now delegates to
the tested `internal/readerctl` command core, but stateful commands still need injected
store/blob/vector/dispatcher dependencies; the default binary only supports Phase-10-safe smoke
and dry-run deploy/staging planning without production config. The current `web/` tracer
bullet reads `VITE_READER_API_BASE_URL`, stores a bearer token in `localStorage`, and displays
the Go API health document from `/api/healthz`.

## Structure

- `cmd/reader-api/` contains the API process entrypoint. It delegates to
  `internal/readerapi.Main`, which loads env config, runs migrations, wires production
  adapters, starts workers, serves HTTP, and handles graceful shutdown.
- `cmd/readerctl/` contains the operator CLI entrypoint. It delegates to
  `internal/readerctl.Main`; production dependency construction is still deferred to
  future readerctl wiring.
- `internal/clock/` defines the clock port, real system clock, and mutex-protected fake clock
  used by concurrent tests.
- `internal/reading/` defines the pure domain core: reading lifecycle statuses, explicit
  status transitions, terminal-state checks, URL idempotency key normalization, source
  classification, and read-time stale annotation.
- `internal/config/` defines the pure startup environment loader for `reader-api`: required
  fields, safe defaults, strict Postgres TLS URL validation, positive tuning values, exact
  `CORS_ALLOWED_ORIGINS` parsing, and redacted logging fields. CORS origin values are never
  logged; only the configured count is safe to log.
- `internal/store/` defines the `Store` port, shared query/page DTOs, the manual
  `BatchStore` port, `store.Memory`, the pgx-backed `store.Postgres` adapter, embedded
  migrations, SQL query source for sqlc, and `storetest.RunContract`/`RunBatchContract` for
  backend-neutral behavior checks. `UpdateStatus` advances the lifecycle; `UpdateContent`
  overwrites the processed-content columns
  (title/summary/keys and the `summary_json`/`similar_json`/`diagnostics_json` blobs) the
  pipeline produces, leaving status, timestamps, error, attempts, and tags alone.
  `UpdateImport` replaces a failed reading's source metadata with a markdown import under the
  same stable id while clearing derived content. `Reprocess` atomically clears derived content
  and marks a row pending for manual reprocessing. `BatchStore` is independent of reading
  lifecycle state: it persists planned manual Anthropic batches, item `custom_id` lookup,
  remote ids/counts, state transitions, idempotent result writes, applied-item markers, and a
  partial active-item uniqueness guard per reading.
- `internal/dispatch/` defines the in-process dispatcher: the pure retry-decision function
  and error classifier (`decide`/`Classify`, with `RateLimitError`/`ErrPermanent`), an
  injectable delay seam (`Delayer` with a real timer and a fireable fake), a worker pool that
  drains an in-memory channel and persists each run's lifecycle outcome, and a startup
  `Sweep` that re-dispatches readings left non-terminal by a crash, resuming each at its
  stored attempt count. It also exposes a forced-recovery seam
  (`ForceSubmit`/`ForceSubmitAfter`) for operator reprocess of stale in-flight work: it
  cancels and token-replaces the in-process claim, waits for the stale handler to drain,
  bounds that wait with `ForceWaitTTL` through the `Delay` seam, then runs the caller's reset
  and re-enqueues. Correctness against late stale writes rests on the store
  `ExpectedStartedAt` fence and run-scoped blob keys, not on the wait.
- `internal/fetch/`, `internal/extract/`, `internal/embed/`, `internal/summarize/`, and
  `internal/notify/` define the external-service ports (`Fetcher`, `Extractor`, `Embedder`,
  `Summarizer`, `Notifier`) and a concurrency-safe, scriptable in-memory `Fake` for each.
  `extract` consumes a `fetch.Resource`. Each package also ships its real adapter:
  `fetch.HTTP` (user agent, timeout, body-size cap, redirect tracking to `FinalURL`,
  non-http(s) scheme rejection, private/special-use address SSRF blocking for literal,
  DNS-resolved, and redirect targets, environment proxy disabling, and a 429 to
  `dispatch.RateLimitError` requeue),
  `embed.OpenAI` (`/v1/embeddings`, `text-embedding-3-small`),
  `summarize.Anthropic` (Messages API with forced `emit_reading` tool use, plus a Message
  Batches client for create/get/results and JSONL result parsing), and `notify.Resend`
  (`/emails`). The HTTP adapters take `WithBaseURL` and `WithHTTPClient` options so contract
  tests can point them at `httptest` upstreams, and classify upstream failures through
  `internal/httpx`. `fetch.HTTP` exposes an explicit private-network bypass only for
  non-production tests.
- `internal/extract/` also holds the extraction internals. `extract.Readability` is the
  production `Extractor`: a three-tier salvage ladder over fetched HTML: `readability`
  (go-readability when the page is readerable, then html-to-markdown), `raw_dom` (whole-DOM
  markdown salvage when it is not), and the `raw_only` floor (every text node, including
  script/style text, collected from the parsed body; a contentless body fails extraction
  permanently). Each tier carries the matching `extract.Mode`. The ladder ordering and the
  sufficiency gate live in the pure, white-box-tested `selectTier`/`sufficient` helpers, kept
  separate from the HTML libraries; the tiers are pinned by HTML fixtures and golden markdown
  in `internal/extract/testdata/` (regenerate with
  `go test ./internal/extract -run TestReadability -update`). `extract.YouTube` is a separate
  oEmbed client, not an `Extractor`, since it takes a video URL and makes its own requests. It
  fetches the title/author floor from `/oembed`, folds in a best-effort `/api/timedtext`
  transcript, reports `ModeRawOnly`, and classifies oEmbed failures through `internal/httpx`.
  `extract.RedditGuidance` is the canonical operator-facing message for the unfetchable Reddit
  source; the pipeline reuses it.
- `internal/httpx/` holds helpers shared by the HTTP service adapters: `ClassifyResponse` maps
  a non-2xx response to a dispatcher-classified error (429 to `dispatch.RateLimitError`
  honoring `Retry-After`, other 4xx to `dispatch.ErrPermanent`, 5xx to transient), and
  `RetryAfter` parses the header. This keeps `embed.OpenAI`, `summarize.Anthropic`, and
  `extract.YouTube` oEmbed error semantics identical to `dispatch.Classify`.
- `internal/blobs/` defines the `Blobs` content-blob port, `blobs.Memory` (in-memory), and
  `blobs.R2`, the production S3-compatible adapter (aws-sdk-go-v2, path-style, custom
  endpoint; maps a missing key to `blobs.ErrNotFound`; health probes distinguish a typed
  missing key from a missing bucket). `R2` is exercised by an `httptest` S3 stub on every run
  and a MinIO container under `//go:build integration`.
- `internal/vector/` defines the `Index` similarity port, `vector.Memory` (a real
  cosine-similarity index), `vector.Postgres` (the pgvector adapter over `reading_vectors`,
  ranking by cosine distance via `<=>`), and `vectortest.RunContract`, the backend-neutral
  suite both backends satisfy (`vector.Memory` on every run, `vector.Postgres` under
  `//go:build integration`).
- `internal/pipeline/` defines the processing pipeline. `Pipeline.Process` is the dispatcher's
  `Handler`: it loads a reading, acquires content (markdown imports read the stored body;
  Reddit fails permanently with `extract.RedditGuidance`; YouTube uses the oEmbed/timed-text
  adapter; everything else fetches and extracts), embeds, snapshots similar readings, persists a guarded content checkpoint,
  upserts a vector, summarizes once, optionally notifies, and persists the content via
  `store.UpdateContent` plus `ReplaceTags`. It returns a `dispatch.Result`; the dispatcher owns
  lifecycle status, and the pipeline owns content. Re-entry is idempotent: a persisted
  `content_key` checkpoint lets a retried run skip fetch/extract and resume near summarize; it
  may re-embed to recover a vector upsert after the guarded checkpoint. Blob keys are derived
  from the server-side reading id and dispatcher run timestamp. Diagnostics include
  `timings_ms` for durable pipeline stages such as acquire, index, summarize, and notify.
- `internal/readingops/` defines the application command service for multi-resource HTTP
  workflows: URL ingest, markdown import and failed-reading replacement, bookmark import, and
  manual reprocess. It owns sequencing across `store.Store`, `blobs.Blobs`, and the dispatcher,
  including stale pending/running force requeue, markdown raw-blob staging/cleanup, and
  `store.Reprocess` checkpoint clearing.
- `internal/bookmarks/` defines the shared bookmark import parser used by HTTP and
  `readerctl`: Netscape/HTML links, JSON arrays of `{ "url": ... }`, and JSON objects with
  `bookmarks` plus optional `html`, preserving order and duplicate URLs.
- `internal/readerctl/` defines the tested operator command core. It supports URL/markdown/
  bookmark imports through `readingops.Service`, audit reports with stale/missing-blob and
  optional orphan inventory checks, recover dry-runs/apply through `readingops.Reprocess`,
  destructive drop dry-runs/apply over store/blob/vector dependencies, smoke checks against a
  running API (`--token` or `--token-env`), and deploy/staging structured plans behind a
  `Runner` seam. Deploy and staging up/promote require `--smoke-token-env` so generated smoke
  steps can authenticate without printing a secret. Default `readerctl.Main` intentionally
  constructs no store/blob/vector/dispatcher dependencies.
- `internal/httpapi/` defines the JSON API as
  `httpapi.Server{Store, Dispatcher, Blobs, Clock, Token, TTLs, NewID}` with one
  `Routes() http.Handler`. It uses the stdlib `http.ServeMux`, constant-time bearer-token
  auth (health skips auth), exact-origin CORS middleware for configured `/api/` browser
  origins, bounded JSON decoding, opaque cursor encoding over `store.Cursor`, dependency health
  JSON for Postgres/R2, request-id structured logging, and DTO mapping that avoids exposing
  internal blob/url-key columns. Ingest, import, and reprocess handlers delegate to
  `internal/readingops`. Tests drive it through `httptest`
  against `store.Memory`, `blobs.Memory`, a fake clock, and a submitter seam;
  `*dispatch.Dispatcher` satisfies that seam. `internal/httpapi/e2e_test.go` adds end-to-end
  HTTP coverage for URL ingest/read/content, similarity across two readings, failed-to-reprocess
  flows, retry exhaustion, rate-limit requeue, and restart recovery, all with the real
  dispatcher and pipeline over in-memory backends and fake external ports.
- `internal/readerapi/` defines the production API runtime: env-driven startup, pgx pool
  construction, embedded migrations, production adapter construction, worker lifecycle,
  startup sweep, health checks, CORS config propagation, redacted startup logging, and graceful
  shutdown ordering.
- `web/` is an isolated Vite/React/TypeScript package with its own `package.json` and
  `package-lock.json`. It exposes scripts for `npm test`, `npm run build`, `npm run dev`, and
  `npm run preview`. It currently implements only the SPA bootstrap utility: configured API
  base URL resolution, unauthenticated health fetches, typed API errors, local bearer-token
  persistence, and a focused health/status screen.
- `.github/workflows/ci.yml`, `Makefile`, and `.golangci.yml` define the project tooling and CI
  conventions.

## Commands

The project targets Go 1.26.

- `make test` runs the default fast test suite with fakes only.
- `make verify` runs the blackbox verification harness in `internal/acceptance/`
  (build tag `verify`): build/vet/gofmt/lint, sqlc-drift, conventions, and cross-package
  behavioral acceptance. The store contract and reading lifecycle run against both
  `store.Memory` and a testcontainers Postgres. Postgres checks skip without Docker, or use
  `DATABASE_URL` when it is set. It automates `docs/ACCEPTANCE.md`.
- `make test-race` runs `go test -race ./...`.
- `make cover` runs `go test -race -cover ./...`.
- `make test-integration` runs tests behind the `integration` build tag. Store integration
  tests use `DATABASE_URL` when set; otherwise they use testcontainers with `pgvector/pgvector`
  and skip when Docker is unavailable. In this environment, a shell may not inherit the
  `docker` group even when the user is a member; if `docker info` fails with socket permission
  errors, run Docker-backed checks through `sg docker -c 'GOFLAGS=-count=1 make test-integration'`
  or use the same `sg docker -c '...'` wrapper for a focused `go test -tags integration`
  command.
- `make lint` checks `gofmt`, `go vet`, and `golangci-lint`.
- `make build` runs `go build ./...`.
- `make run` runs `cmd/reader-api`; the binary now requires production env and exits with a
  redacted configuration error when required fields are missing or invalid.
- `make migrate` is a reserved target that currently invokes unsupported `readerctl migrate`;
  production CLI migration wiring remains deferred to future readerctl dependency injection.
- `make sqlc` runs `sqlc generate`.
- `cd web && npm ci` installs the isolated SPA package from the committed lockfile.
- `cd web && npm test` runs the Vitest suite for the SPA bootstrap.
- `cd web && npm run build` runs TypeScript project checks and a Vite production build.

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
  `store.Postgres` satisfy the same contract (`RunContract` for readings, `RunBatchContract`
  for manual batches). Likewise, add vector-index behavior to
  `internal/vector/vectortest` first, then make `vector.Memory` and the pgvector adapter
  satisfy it.
- Scriptable port fakes expose their configured response/error as fields set before use and
  guard call recording behind a mutex; return defensive copies so callers cannot corrupt the
  script.
- Give each real HTTP adapter a `NewX(key, ...Option)` constructor with a `WithBaseURL` and
  `WithHTTPClient` seam, and contract-test it against an `httptest` upstream. Assert request
  shape, happy parsing, and at least one error-mapping case. Route every adapter's
  upstream-error classification through `internal/httpx` so it agrees with
  `dispatch.Classify`. Keep DB/object-store adapters (`vector.Postgres`, `blobs.R2`) out of
  the default run: prove them with their conformance/round-trip suites under
  `//go:build integration` and rely on compile-time `var _ Port = (*Adapter)(nil)` conformance
  assertions in the acceptance harness.
- Keep retry/backoff logic in pure functions (`dispatch.decide`, `dispatch.Classify`) and run
  delays through the injected `dispatch.Delayer` seam so retry, backoff, rate-limit, and
  recovery semantics test deterministically without real goroutines, timers, or sleeps.
- Keep the extraction tier selection (`extract.selectTier`/`sufficient`) pure and white-box
  tested, separate from the HTML libraries, and pin each tier with an HTML fixture plus golden
  markdown under `internal/extract/testdata/` (regenerate goldens with `-update`). Justified
  white-box test files (`decide_test.go`, `ladder_test.go`) are listed in the acceptance
  harness's `whiteboxAllowed` allowlist; every other `_test.go` stays a black-box `_test`
  package.
- Keep the pipeline's status/content split: the dispatcher owns lifecycle status (it marks
  running/ready/failed/pending), and the pipeline owns content. `Pipeline.Process` maps step
  errors to outcomes through the shared `dispatch.Classify`, and persists a guarded content
  checkpoint before the summarize step so a retried run resumes idempotently from the stored
  `content_key`.
- Keep the HTTP API thin: handlers should delegate command workflows to `internal/readingops`,
  stale detail overlays to `reading.AnnotateStale`, queries to `store.Store`, and blob payloads
  to `blobs.Blobs`. Preserve the single JSON error envelope
  `{ "error": { "code": "...", "message": "..." } }`. `readingops.Service.Reprocess` must use
  `store.Reprocess` to atomically clear content checkpoints before re-enqueueing so the
  pipeline does not reuse stale `content_key` data. Raw blob responses must not reflect upstream
  executable content types; serve them as downloads with `nosniff`. CORS stays closed by
  default, exact-origin only when configured, and preflight handling must run before bearer auth.
- Drive pipeline tests through an inline `dispatch.Dispatcher` (`Inline: true`) with a
  `dispatch.FakeDelayer`, so a test exercises real status transitions and asserts the
  pipeline's effects on the store/blobs/vector index and its scripted port fakes.
- Use table-driven subtests and `t.Parallel()` when there is no shared mutable state.
