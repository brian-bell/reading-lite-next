# reading-lite

`reading-lite` is a Go backend for a personal reading service with an isolated SPA bootstrap
under `web/`. It ingests URLs and imports markdown/bookmark files, fetches and extracts source
content, stores raw and processed blobs, embeds and indexes readings for similarity, summarizes
readings, tags them, and can send notifications.

The production `cmd/reader-api` binary validates env config, runs embedded store migrations,
wires production adapters, starts dispatcher workers, runs startup recovery, serves the HTTP
API, reports dependency health, handles exact-origin CORS for configured SPA origins, and shuts
down gracefully. `cmd/readerctl` delegates to the tested `internal/readerctl` command core, but
stateful default-binary dependency wiring is still deferred. The `web/` SPA reads
`VITE_READER_API_BASE_URL`, stores a bearer token in `localStorage`, shows `/api/healthz`, and
drives the authenticated reading workflow: list with live processing polls, URL submission,
a reading detail view with content/raw fetch, and a reprocess action.

## Structure

- `cmd/reader-api/` and `internal/readerapi/` are the production API runtime: env loading,
  migrations, adapter construction, dispatcher workers, startup sweep, health checks, HTTP
  serving, and graceful shutdown.
- `cmd/readerctl/` and `internal/readerctl/` are the operator CLI entrypoint and tested command
  core. The default binary still does not construct production store/blob/vector/dispatcher
  dependencies for stateful commands.
- `internal/reading/` and `internal/clock/` are the dependency-light domain core and time seam:
  reading lifecycle, URL keys, source classification, stale overlays, and deterministic clocks.
- `internal/store/`, `internal/blobs/`, and `internal/vector/` define persistence ports,
  in-memory implementations, production adapters, migrations/sqlc output, and backend-neutral
  contract suites. Postgres/pgvector/R2 integration stays behind `//go:build integration`.
- `internal/dispatch/` owns retry classification, pure retry/backoff decisions, delayed
  requeueing, worker lifecycle, crash sweep, and forced stale-work recovery.
- `internal/fetch/`, `internal/extract/`, `internal/embed/`, `internal/summarize/`, and
  `internal/notify/` define external-service ports, race-safe scriptable fakes, and real HTTP
  adapters. Shared non-2xx classification lives in `internal/httpx/`.
- `internal/extract/` also contains extraction internals: readability/raw DOM/raw-only HTML
  salvage, YouTube oEmbed/timed-text support, and the canonical Reddit guidance string.
- `internal/pipeline/` is the dispatcher handler. It acquires source content, embeds, snapshots
  similar readings, checkpoints processed content, summarizes, optionally notifies, and persists
  content/tags while leaving lifecycle status to the dispatcher.
- `internal/readingops/` coordinates multi-resource workflows for HTTP and CLI: URL ingest,
  markdown import/replacement, bookmark import, and manual reprocess.
- `internal/httpapi/` is the JSON API transport: auth, exact-origin CORS, bounded JSON,
  health/dependency checks, DTO mapping, cursor encoding, blob streaming, and delegation to
  `readingops`.
- `internal/bookmarks/` is the shared bookmark parser for HTTP and `readerctl`.
- `web/` is the isolated Vite/React/TypeScript SPA package: configured API base URL, typed API
  client with the Go error envelope, local bearer-token persistence, health/status screen, an
  authenticated reading list with cursor paging and live processing polls, URL submission, and a
  reading detail view (summary/tags/similar/diagnostics, markdown content, raw download, and a
  reprocess action) with race-safe request-id guards.
- `.github/workflows/ci.yml`, `Makefile`, `.golangci.yml`, `sqlc.yaml`, and `go.mod` define the
  project tooling and generated-code conventions.

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
