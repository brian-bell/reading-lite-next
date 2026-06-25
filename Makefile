GO ?= go
GOLANGCI_LINT ?= golangci-lint
SQLC ?= $(shell command -v sqlc 2>/dev/null || printf '%s/bin/sqlc' "$$($(GO) env GOPATH)")

.PHONY: test test-integration test-race verify lint cover sqlc migrate build run

test:
	$(GO) test ./...

test-integration:
	$(GO) test -tags integration ./...

verify:
	$(GO) test -tags verify ./internal/acceptance/...

test-race:
	$(GO) test -race ./...

lint:
	files="$$(gofmt -l .)" || exit $$?; if [ -n "$$files" ]; then printf '%s\n' "$$files"; exit 1; fi
	$(GO) vet ./...
	$(GOLANGCI_LINT) run ./...

cover:
	$(GO) test -race -cover ./...

sqlc:
	$(SQLC) generate

migrate:
	$(GO) run ./cmd/readerctl migrate

build:
	$(GO) build ./...

run:
	$(GO) run ./cmd/reader-api
