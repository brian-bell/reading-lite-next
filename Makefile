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
	cd "$(WEB_DIR)" && $(NPM) test

web-build:
	cd "$(WEB_DIR)" && env VITE_READER_API_BASE_URL="$(WEB_API_BASE_URL)" $(NPM) run build

web-dev:
	cd "$(WEB_DIR)" && env VITE_READER_API_BASE_URL="$(WEB_API_BASE_URL)" $(NPM) run dev

deploy-web:
	@if [ "$(DEPLOY_WEB_APPLY)" = "1" ]; then \
		if [ -z "$(CLOUDFLARE_PAGES_PROJECT)" ]; then \
			printf '%s\n' "CLOUDFLARE_PAGES_PROJECT is required for DEPLOY_WEB_APPLY=1."; \
			exit 2; \
		fi; \
		env WEB_API_BASE_URL="$(WEB_API_BASE_URL)" node -e 'const net = require("net"); const raw = process.env.WEB_API_BASE_URL || ""; let u; try { u = new URL(raw); } catch (e) { console.error("WEB_API_BASE_URL must be an absolute URL for DEPLOY_WEB_APPLY=1."); process.exit(2); } const host = u.hostname.toLowerCase(); const ip = host.startsWith("[") ? host.slice(1, -1) : host; const fam = net.isIP(ip); const blocked = new net.BlockList(); blocked.addSubnet("127.0.0.0", 8, "ipv4"); blocked.addAddress("::1", "ipv6"); blocked.addSubnet("::ffff:127.0.0.0", 104, "ipv6"); if (host === "localhost" || (fam !== 0 && blocked.check(ip, fam === 4 ? "ipv4" : "ipv6"))) { console.error("WEB_API_BASE_URL must be set to the deployed tunnel origin for DEPLOY_WEB_APPLY=1."); process.exit(2); } if (u.protocol !== "https:") { console.error("WEB_API_BASE_URL must use https for DEPLOY_WEB_APPLY=1."); process.exit(2); } const normalized = raw.trim().replace(/\/+$$/, ""); if (normalized !== u.origin) { console.error("WEB_API_BASE_URL must be an exact https origin (no path, query, fragment, credentials, or default port) for DEPLOY_WEB_APPLY=1."); process.exit(2); }' || exit $$?; \
	fi
	$(MAKE) web-build
	@if [ "$(DEPLOY_WEB_APPLY)" != "1" ]; then \
		project="$(CLOUDFLARE_PAGES_PROJECT)"; \
		if [ -z "$$project" ]; then project="<project-name>"; fi; \
		printf '%s\n' "Dry run only; set DEPLOY_WEB_APPLY=1 to deploy after checking this command."; \
		printf '%s\n' "Set CLOUDFLARE_PAGES_PROJECT to your non-secret Cloudflare Pages project name."; \
		printf '%s\n' "$(WRANGLER) pages deploy \"$(WEB_DIST_DIR)\" --project-name \"$$project\""; \
		exit 0; \
	fi; \
	$(WRANGLER) pages deploy "$(WEB_DIST_DIR)" --project-name "$(CLOUDFLARE_PAGES_PROJECT)"
