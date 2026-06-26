# reading-lite

`reading-lite` is a Go backend for a personal reading service. The backend is being built
test-first from `docs/PLAN.md`.

Current status: Phase 9 is complete. The repository contains the Go module, Makefile targets,
GitHub Actions CI, lint configuration, placeholder `reader-api` and `readerctl` binaries,
the deterministic `internal/clock` package, `internal/reading` for the pure domain core,
`internal/store` for the shared Store interface, memory fake, Postgres adapter, embedded
migrations, SQL source/generated code, and conformance suite, `internal/dispatch` for the
worker pool, retry/backoff, rate-limit re-dispatch, and crash-recovery sweep, the external
service ports and fakes, the full processing pipeline, real production adapters behind those
ports, extraction internals, the `internal/readingops` command service, and the
`internal/httpapi` server surface plus Phase 9 end-to-end HTTP tests that wire
`store.Memory`, the real in-process dispatcher, the real pipeline, `blobs.Memory`,
`vector.Memory`, and fake external services.

The API package exposes health, bearer-auth-protected ingest, markdown and bookmark imports,
list/search, detail with read-time stale annotation, content/raw blob reads, reprocess, and the
shared JSON error model. `internal/readingops` owns the ingest/import/reprocess sequencing across
the store, blob backend, and dispatcher; `internal/httpapi` stays focused on transport concerns.
The production `cmd/reader-api` wiring is still pending, so the API is tested as a package but not
yet started by `make run`.

## Requirements

- Go 1.26
- `golangci-lint` for `make lint`
- `sqlc` for `make sqlc`
- Docker or `DATABASE_URL` for `make test-integration`

## Commands

```sh
make test
make test-race
make cover
make lint
make build
make verify   # blackbox verification harness (internal/acceptance, -tags verify)
```

`make verify` automates `docs/ACCEPTANCE.md`: build/vet/gofmt/lint,
sqlc-drift, the conventions audit, and cross-package behavioral acceptance. The
store contract and reading lifecycle run against both `store.Memory` and a
testcontainers Postgres. Steps that need a tool (golangci-lint, sqlc) or Docker
(the Postgres backend) skip when unavailable; set `DATABASE_URL` to use an existing
database instead of testcontainers.

Integration tests are reserved for adapters that need external services and run separately:

```sh
make test-integration
```

The store integration tests use `DATABASE_URL` when it is set. Otherwise they fall back to
testcontainers with a `pgvector/pgvector` Postgres image and skip when Docker is unavailable.

The service and CLI entrypoints exist but are placeholders until later phases:

```sh
make run
make migrate
```
