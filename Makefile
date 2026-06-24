GO ?= go
GOLANGCI_LINT ?= golangci-lint

.PHONY: test test-integration test-race lint cover sqlc migrate build run

test:
	$(GO) test ./...

test-integration:
	$(GO) test -tags integration ./...

test-race:
	$(GO) test -race ./...

lint:
	files="$$(gofmt -l .)" || exit $$?; if [ -n "$$files" ]; then printf '%s\n' "$$files"; exit 1; fi
	$(GO) vet ./...
	$(GOLANGCI_LINT) run ./...

cover:
	$(GO) test -race -cover ./...

sqlc:
	sqlc generate

migrate:
	$(GO) run ./cmd/readerctl migrate

build:
	$(GO) build ./...

run:
	$(GO) run ./cmd/reader-api
