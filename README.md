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

The web package has repo-level convenience targets plus the package-local npm scripts:

```sh
npm --prefix web ci
make web-test
make web-build
make web-dev WEB_API_BASE_URL=http://127.0.0.1:8080
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
Smoke supports `--token` for disposable local tests, but operator and production smoke should
use `--token-env`; deploy/staging smoke plans use `--smoke-token-env` so secrets stay out of
rendered step arguments. Stateful commands such as
`import`, `audit`, `recover`, and `drop` are tested in `internal/readerctl` with injected
dependencies; the default binary still refuses them until production dependency construction is
added there.

## Cloudflare Pages + Tunnel Runbook

This operator path runs the Vite SPA on Cloudflare Pages while keeping `reader-api` bound to the
local host and reachable through a stable named Cloudflare Tunnel.

API runtime setup:

- Run `reader-api` with `LISTEN_ADDR=127.0.0.1:8080` so it only accepts local connections.
- Set `CORS_ALLOWED_ORIGINS` to the exact Pages origins that browsers will use. Include the
  production Pages/custom origin and every preview origin you intend to test, for example
  `https://reading-lite.pages.dev,https://<preview>.reading-lite.pages.dev`. Origins must not
  include wildcards, paths, query strings, fragments, or trailing slashes.

Named tunnel routing:

- Create or reuse a stable named tunnel, then route a hostname such as `api.example.com` to it:
  `cloudflared tunnel route dns <tunnel-name> api.example.com`.
- Use tunnel ingress that forwards the hostname to the locally bound API:

```yaml
ingress:
  - hostname: api.example.com
    service: http://127.0.0.1:8080
  - service: http_status:404
```

- Run the named tunnel with that ingress config, for example `cloudflared tunnel run <tunnel-name>`,
  or install it as a service using the same named tunnel configuration. Before Pages smoke, verify
  the tunnel reaches the API without a bearer token: `curl https://api.example.com/api/healthz`.

Pages build settings:

- Project root: `web`
- Build command when the Pages project root is `web`: `npm ci && npm run build`
- Build output directory: `dist`
- Environment variable for production and preview builds:
  `VITE_READER_API_BASE_URL=https://api.example.com`

Local and deploy commands:

```sh
make web-test
make web-build
make web-dev WEB_API_BASE_URL=https://api.example.com
make deploy-web
make deploy-web WEB_API_BASE_URL=https://api.example.com CLOUDFLARE_PAGES_PROJECT=reading-lite
make deploy-web WEB_API_BASE_URL=https://api.example.com CLOUDFLARE_PAGES_PROJECT=reading-lite DEPLOY_WEB_APPLY=1
```

`make deploy-web` runs `web-build` first and is dry-run-only unless `DEPLOY_WEB_APPLY=1` is set.
The dry run prints the non-secret Wrangler command. The apply path requires
`CLOUDFLARE_PAGES_PROJECT`; direct deploys should set `WEB_API_BASE_URL` to the tunnel hostname
so Vite bakes the same API base URL that Pages would receive from
`VITE_READER_API_BASE_URL`. Apply mode refuses localhost API origins because those would make
remote browsers call their own machines instead of the tunnel. It also requires
`WEB_API_BASE_URL` to be an exact `https` origin with no path, query, fragment, credentials, or
default `:443` port, because the SPA concatenates that raw value with `/api/healthz`; anything
else would bake a wrong endpoint into the deployed bundle. Wrangler uses its normal login
state or `CLOUDFLARE_API_TOKEN` from the environment. The target does not print token values.
`WRANGLER` defaults to `npx wrangler`, which may install or use Wrangler outside the repo
lockfile. Treat `WRANGLER` as a command string with optional arguments; if your Wrangler
executable path contains spaces, wrap it in a small script and point `WRANGLER` at that script.

Secret-safe smoke:

- Keep bearer tokens in environment variables or interactive browser input. Do not put tokens in
  command history, screenshots, or logs.
- Current CLI tunnel smoke:

```sh
readerctl smoke --base-url https://api.example.com --ingest-url https://example.com --token-env READER_API_TOKEN
```

This validates health plus authenticated submit/ingest through the tunnel.

- Future browser smoke, once list/detail/submit UI slices exist: open the Pages origin, paste the
  token only in the SPA token control, submit a reading, verify it appears in the list, open the
  detail view, and confirm browser network requests use the tunnel API hostname.
