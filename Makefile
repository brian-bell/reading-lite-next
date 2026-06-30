GO ?= go
GOLANGCI_LINT ?= golangci-lint
SQLC ?= $(shell command -v sqlc 2>/dev/null || printf '%s/bin/sqlc' "$$($(GO) env GOPATH)")
NPM ?= npm
WEB_DIR ?= web
WEB_DIST_DIR ?= $(WEB_DIR)/dist
WEB_API_BASE_URL ?= http://127.0.0.1:8080
WRANGLER ?= npx wrangler
CLOUDFLARE_PAGES_PROJECT ?=
DEPLOY_WEB_APPLY ?= 0

.PHONY: test test-integration test-race verify lint cover sqlc migrate build run web-test web-build web-dev deploy-web

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

web-test:
	cd $(WEB_DIR) && $(NPM) test

web-build:
	cd $(WEB_DIR) && $(NPM) run build

web-dev:
	cd $(WEB_DIR) && VITE_READER_API_BASE_URL=$(WEB_API_BASE_URL) $(NPM) run dev

deploy-web: web-build
	@if [ "$(DEPLOY_WEB_APPLY)" != "1" ]; then \
		project="$(CLOUDFLARE_PAGES_PROJECT)"; \
		if [ -z "$$project" ]; then project="<project-name>"; fi; \
		printf '%s\n' "Dry run only; set DEPLOY_WEB_APPLY=1 to deploy after checking this command."; \
		printf '%s\n' "Set CLOUDFLARE_PAGES_PROJECT to your non-secret Cloudflare Pages project name."; \
		printf '%s\n' "$(WRANGLER) pages deploy $(WEB_DIST_DIR) --project-name \"$$project\""; \
		exit 0; \
	fi; \
	if [ -z "$(CLOUDFLARE_PAGES_PROJECT)" ]; then \
		printf '%s\n' "CLOUDFLARE_PAGES_PROJECT is required for DEPLOY_WEB_APPLY=1."; \
		exit 2; \
	fi; \
	$(WRANGLER) pages deploy $(WEB_DIST_DIR) --project-name "$(CLOUDFLARE_PAGES_PROJECT)"
