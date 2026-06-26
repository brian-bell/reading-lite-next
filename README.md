# reading-lite

`reading-lite` is a Go backend for a personal reading service. It provides the core
domain model, storage adapters, processing pipeline, HTTP API package, and real service
adapters needed to ingest, process, summarize, and search saved readings.

The API package supports health checks, bearer-auth-protected URL ingest, markdown and
bookmark imports, list/search, reading detail with stale-state annotation, content and raw
blob reads, reprocess, and a shared JSON error model. `internal/readingops` owns the
ingest/import/reprocess workflows across the store, blob backend, and dispatcher;
`internal/httpapi` stays focused on transport concerns.

Production API process wiring is not in place yet. `cmd/reader-api` exists so the project
builds, but it still exits immediately instead of starting the API server. `cmd/readerctl`
now delegates to the tested `internal/readerctl` operator command core; commands that need
store/blob/vector/dispatcher dependencies still require injected construction and return a
configuration error from the default binary until Phase 11 wiring exists.

## Requirements

- Go 1.26
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

The API entrypoint currently builds but does not perform useful work:

```sh
make run
```

`readerctl` supports `smoke` and dry-run `deploy`/`staging` planning from the default binary.
Smoke can authenticate with `--token` or `--token-env`; deploy/staging smoke plans use
`--smoke-token-env` so secrets stay out of rendered step arguments. Stateful commands such as
`import`, `audit`, `recover`, and `drop` are tested in `internal/readerctl` with injected
dependencies; the default binary refuses them until production configuration and adapter
construction land.
