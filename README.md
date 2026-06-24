# reading-lite

`reading-lite` is a Go backend for a personal reading service. The backend is being built
test-first from `docs/backend-tdd-plan.md`.

Current status: Phase 1 has added the pure domain core. The repository contains the Go module,
Makefile targets, GitHub Actions CI, lint configuration, placeholder `reader-api` and
`readerctl` binaries, the deterministic `internal/clock` package, and `internal/reading` for
reading status transitions, URL idempotency keys, source classification, and stale annotation.

## Requirements

- Go 1.26
- `golangci-lint` for `make lint`
- `sqlc` for `make sqlc` once SQL generation is introduced

## Commands

```sh
make test
make test-race
make cover
make lint
make build
```

Integration tests are reserved for adapters that need external services and run separately:

```sh
make test-integration
```

The service and CLI entrypoints exist but are placeholders until later phases:

```sh
make run
make migrate
```
