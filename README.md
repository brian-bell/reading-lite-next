# reading-lite

`reading-lite` is a Go backend for a personal reading service with an isolated SPA
bootstrap under `web/`. It provides the core domain model, storage adapters, processing
pipeline, HTTP API package, and real service adapters needed to ingest, process, summarize,
and search saved readings.

The API package supports health checks, bearer-auth-protected URL ingest, markdown and
bookmark imports, list/search, reading detail with stale-state annotation, content and raw
blob reads, reprocess, and a shared JSON error model. `internal/readingops` owns the
ingest/import/reprocess workflows across the store, blob backend, and dispatcher;
`internal/httpapi` stays focused on transport concerns.

The production API process now boots from environment configuration. `cmd/reader-api`
validates startup env, opens Postgres, runs embedded migrations, constructs the production
store/blob/vector/fetch/embed/summarize/notify adapters, starts dispatcher workers, runs the
startup recovery sweep, serves the HTTP API, reports Postgres/R2 health, and shuts down
gracefully on cancellation or SIGTERM. It can also expose browser CORS headers for exact
SPA origins configured through `CORS_ALLOWED_ORIGINS`. `cmd/readerctl` now delegates to the tested
`internal/readerctl` operator command core; commands that need store/blob/vector/dispatcher
dependencies still require injected construction and return a configuration error from the
default binary.

`web/` is a separate Vite/React/TypeScript package. The current SPA tracer bullet reads
`VITE_READER_API_BASE_URL`, stores a bearer token in `localStorage`, and displays the API
health document from `/api/healthz`; reading list and mutation UI are still later slices.

## Requirements

- Go 1.26
- Node.js and npm for the isolated `web/` package
- `golangci-lint` for `make lint`
- `sqlc` for `make sqlc`
- Docker or `DATABASE_URL` for Postgres-backed integration checks

## Commands

```sh
make test
make test-race
make cover
make lint
make build
make verify
```

The web package is intentionally not wired into the Go `Makefile` targets yet. Run its checks
from `web/`:

```sh
cd web && npm ci
cd web && npm test
cd web && npm run build
```

`make verify` runs the blackbox verification harness in `internal/acceptance/` with the
`verify` build tag. It checks build/vet/gofmt/lint, sqlc drift, project conventions, and
cross-package behavior. Steps that need optional tools such as `golangci-lint`, `sqlc`, or
Docker skip when unavailable; set `DATABASE_URL` to use an existing database instead of
testcontainers.

Integration tests are reserved for adapters that need external services and run separately:

```sh
make test-integration
```

The store integration tests use `DATABASE_URL` when it is set. Otherwise they fall back to
testcontainers with a `pgvector/pgvector` Postgres image and skip when Docker is unavailable.

The API entrypoint requires Phase 11 environment configuration:

```sh
make run
```

Required env includes `READER_API_TOKEN`, `DATABASE_URL` with TLS `sslmode=require`,
`verify-ca`, or `verify-full`, `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, R2 endpoint/access/
secret/bucket settings, `RESEND_API_KEY`, `NOTIFY_FROM`, `NOTIFY_TO`, dispatcher TTL/count/
buffer settings, `PG_MAX_CONNS`, and `LISTEN_ADDR`. Optional `FETCH_TIMEOUT`,
`FETCH_MAX_BYTES`, and `SHUTDOWN_TIMEOUT` use safe defaults. Optional
`CORS_ALLOWED_ORIGINS` is a comma-separated exact allowlist such as
`https://app.example.com,http://localhost:5173`; unset leaves browser CORS closed.

`readerctl` supports `smoke` and dry-run `deploy`/`staging` planning from the default binary.
Smoke can authenticate with `--token` or `--token-env`; deploy/staging smoke plans use
`--smoke-token-env` so secrets stay out of rendered step arguments. Stateful commands such as
`import`, `audit`, `recover`, and `drop` are tested in `internal/readerctl` with injected
dependencies; the default binary still refuses them until production dependency construction is
added there.
