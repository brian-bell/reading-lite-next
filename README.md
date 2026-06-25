# reading-lite

`reading-lite` is a Go backend for a personal reading service. The backend is being built
test-first from `docs/PLAN.md`.

Current status: Phase 3 has added the in-process dispatcher. The repository contains the
Go module, Makefile targets, GitHub Actions CI, lint configuration, placeholder `reader-api` and
`readerctl` binaries, the deterministic `internal/clock` package, `internal/reading` for the
pure domain core, `internal/store` for the shared Store interface, concurrency-safe memory
fake, Postgres adapter, embedded migration, SQL query file, and conformance suite, and
`internal/dispatch` for the worker pool, retry/backoff and rate-limit decision logic, and the
crash-recovery sweep.

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
