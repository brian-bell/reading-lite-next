# reading-lite — Go Backend TDD Specification

> A test-first implementation specification for the `reading-lite` backend: a personal reading service that
> ingests a URL, extracts + summarizes the article, finds similar past readings, and makes
> everything searchable. This document is the implementation contract — work top to bottom,
> writing the listed tests **before** the code that makes them pass.

---

## 0. Reading guide

The spec is organized as **phases**. Each phase is a vertical slice with:

- **Goal** — the behavior we are adding.
- **Red** — the tests to write first, named concretely, with table cases.
- **Green** — the minimal implementation contract to make them pass.
- **Refactor** — cleanups to do once green, before moving on.
- **Done when** — the checkpoint that lets the next phase start.

Phases are dependency-ordered. The pure domain core comes first (fastest feedback, no I/O),
then the Postgres-backed store (with an in-memory fake), then the in-process dispatcher, then
the external-service ports and their fakes, then the pipeline that wires them, then real adapters, then the HTTP API, then
end-to-end, then operator tooling. Each phase ends green and `go test ./...` stays green
forever after.

### Phase status

Status values are `complete`, `in progress`, or `not started`.

| Phase | Scope | Status | Commit ref(s) |
|---|---|---|---|
| 0 | Tooling, conventions, CI, clock port | complete | `2ebe63b` implementation; `ce09597` PR #1 |
| 1 | Pure reading domain core | complete | `86d05b0` implementation; `ce09597` PR #1 |
| 2 | Store port, memory fake, Postgres adapter, conformance suites | complete | `47078f3` implementation; `ce09597` PR #1 |
| 3 | In-process dispatcher, retry, recovery sweep | complete | `de6d8aa` implementation; `c1a381e` PR #2; `84fdd36` PR #3 acceptance |
| 4 | External-service ports and scriptable fakes | complete | `30fb344` implementation; `83a403e` PR #4 |
| 5 | Processing pipeline over fakes | complete | `8a6d8f9` implementation; `f9100d4` PR #5 |
| 6 | Real service adapters and contract tests | complete | `2bebd4d` implementation; `eb4582f` PR #6 |
| 7 | Extraction internals, YouTube, Reddit guidance | complete | `08b2b07` implementation; `be0bf36` PR #7 |
| 8 | Command service and HTTP API | complete | `058be8c` implementation; `7672b0b` PR #8 |
| 9 | End-to-end HTTP integration stories | complete | `fc2a8d0` implementation; `fcdfb11` PR #10 |
| 10 | Tested `readerctl` command core; production dependency injection remains deferred | complete | `025b7c9` implementation; `f6142fb` PR #11 |
| 11 | Production API runtime, config, health, security, shutdown | complete | `77e4c5a` implementation; `8ace075` PR #23 |
| 12 | Optional alternative backends and multi-instance workers | not started | None |

---

## 1. Architectural decisions

### 1.1 Shape: ports & adapters (hexagonal)

The entire external world is reached through small Go interfaces (**ports**). The domain and
the processing pipeline depend only on ports, never on a concrete SDK or HTTP client. Every
port has (a) a real **adapter** and (b) an in-memory **fake** used by domain/pipeline tests.
This is the single most important decision for TDD: it makes the core testable with zero I/O
and turns each adapter into an independently verifiable unit.

Ports:

| Port | Responsibility | Real adapter | Fake |
|---|---|---|---|
| `Fetcher` | HTTP GET a URL with UA/timeout/size caps | `fetch.HTTP` | `fetch.Fake` |
| `Extractor` | HTML → article (title/author/markdown), salvage tiers | `extract.Readability` | `extract.Fake` |
| `Embedder` | text → `[1536]float32` | `embed.OpenAI` | `embed.Fake` |
| `VectorIndex` | upsert/query/delete 1536-dim vectors | `vector.Postgres` (pgvector) | `vector.Memory` |
| `Summarizer` | article → structured summary via forced tool use | `summarize.Anthropic` | `summarize.Fake` |
| `Notifier` | send notification email | `notify.Resend` | `notify.Fake` |
| `Blobs` | put/get/delete **content blobs** (raw HTML + extracted markdown) | `blobs.R2` | `blobs.Memory` |
| `Store` | readings metadata: list/search (FTS)/sort/tags, url_key idempotency | `store.Postgres` | `store.Memory` |
| `Clock` | `Now()` | `clock.System` | `clock.Fake` |

> `Store` and `VectorIndex` each have **two** implementations: a production Postgres adapter and
> an in-memory fake (`store.Memory`, `vector.Memory`). Both pairs are pinned by a shared
> **conformance suite** — one set of behavioral tests run against the fake on every `go test`
> (fast) and against real Postgres under `//go:build integration` (testcontainers). The fakes
> double as a zero-infra dev/small-deploy backend. The in-process dispatcher (§1.4) is internal,
> not a port — its test seam is a synchronous handler call, not an interface.

### 1.2 Managed services vs. self-hosted

The runtime depends on a **hosted Postgres** (metadata + full-text search + `pgvector`
similarity), **R2** for content blobs, and the OpenAI/Anthropic/Resend HTTP APIs. Postgres is
the one stateful piece we own; everything else is a thin HTTP/S3 adapter behind a port.
Vectors live in Postgres via `pgvector`, so similarity sits transactionally beside the metadata
it ranks — one fewer external service. Hosted-PG options: Neon (scales to zero — good for
personal cost), Supabase, Fly Postgres, or RDS; connect over TLS via `DATABASE_URL` through the
pooled endpoint. Because R2 and the AI APIs sit behind `Blobs`/`Embedder`/`Summarizer`/
`Notifier`, alternatives (e.g. MinIO/local-FS for blobs) remain drop-in (Phase 12).

### 1.3 Metadata store & search

Readings metadata lives in **Postgres**, queried by index — nothing holds the corpus in memory.
The `readings` table carries the row fields, a `tags text[]`, and a generated `tsvector` search
column; `reading_vectors(reading_id, embedding vector(1536))` (FK, `ON DELETE CASCADE`) holds
embeddings for `pgvector` similarity. R2 holds only the large content blobs (raw HTML, extracted
markdown), referenced by key.

Postgres serves each query by index:
- **Full-text `q`** → `search @@ websearch_to_tsquery('english', $q)` ranked by `ts_rank`, GIN
  index. `websearch_to_tsquery` safely parses arbitrary user input (quotes/AND/OR), so the
  adversarial-`q` sanitization problem disappears and we get stemming + ranking for free.
- **Tag filter (AND)** → `tags @> $1`, GIN index.
- **Pagination** → keyset `(created_at, id) < ($cursor)` on a descending composite index — no
  offset scan, bounded result set.
- **Idempotency** → `unique (url_key)` with `insert … on conflict (url_key) do nothing`.
- **Recovery sweep** → `where status='pending' or (status='running' and started_at < $cutoff)`,
  status index — cheap, no full listing.

Data layer: **sqlc** generates type-safe Go from hand-written SQL (`pgx/v5` + `pgxpool`);
`pgvector` columns map via `pgvector-go`. Migrations are embedded (`embed.FS`) and applied by
the store migration runner, which records applied versions in `schema_migrations` and uses a
Postgres advisory lock so repeated or parallel runs are safe. The `Store`/`VectorIndex`
interfaces are unchanged — only the adapter behind them changes, so pipeline/dispatcher/HTTP
code and their tests are untouched.

### 1.4 Async model: in-process dispatcher

Processing is dispatched **in-process**: the ingest handler persists the reading as `pending` to
Postgres, then hands its id to a `Dispatcher` — a buffered channel drained by a small
worker-goroutine pool that runs `Pipeline.Process`. The id is the unit of work; the "pollable
id" works because status lives in Postgres and the client polls `GET /readings/{id}`.

On a non-success outcome, the worker loop takes one of three actions:
- **Rate-limit → delayed re-dispatch**: a `RateLimitError{RetryAfter}` re-dispatches the same id
  after the delay; the attempt count is **not** consumed.
- **Retry with backoff**: a transient error re-dispatches after exponential backoff, up to
  `MAX_ATTEMPTS`.
- **Retry-exhaustion**: once attempts are spent, the reading is written `failed` and stays
  reprocessable.

Durability across restarts rests on two mechanisms: (1) a **startup recovery sweep** — a single
indexed query for non-terminal readings (`pending`, and `running` past `RUNNING_TTL`) that
re-dispatches them — and (2) **read-time stale annotation** (§1.7) plus on-demand **reprocess**
as the user-visible backstop. A crash loses only the in-memory channel contents, which the next
startup sweep restores.

The attempt counter rides on the in-memory work item for live retries and is mirrored onto the
reading (`process_attempts`) so the recovery sweep can respect `MAX_ATTEMPTS` across restarts.

### 1.5 Process topology

`reader-api` is a **single process** running the HTTP server and the in-process dispatcher +
worker pool together, sharing one `pgxpool`. A personal service needs only one instance, so the
design stays single-instance; Postgres leaves a clean path to multi-instance workers later (§14)
if that ever changes. Operator tasks live in a second binary `readerctl`.

### 1.6 HTTP

Standard library only: Go 1.22 `http.ServeMux` (method + wildcard patterns) and `encoding/json`.
No web framework. Middleware is plain `func(http.Handler) http.Handler`.

### 1.7 Reading lifecycle (state machine)

```
            submit                dispatch              success
   (new) ─────────────▶ pending ─────────▶ running ─────────────▶ ready
     ▲                     │                  │                      │
     │ reprocess           │ requeue(delay)   │ failure              │ reprocess
     │ (idempotent)        ▼ (rate limit)     ▼                      ▼
     └──────────────── pending ◀──────── failed ◀────── (attempts exhausted)
```

- `pending` — accepted, persisted to R2, handed to the dispatcher; not yet started.
- `running` — a worker picked up the id and is executing the pipeline.
- `ready` — pipeline succeeded; carries `extraction_mode ∈ {readability, raw_dom, raw_only}`.
- `failed` — pipeline failed (incl. retry-exhausted), or special-cased unfetchable source
  (Reddit) with operator guidance; reprocessable.

**Stale annotation** is a *read-time* overlay, never a write: a `running` row older than
`RUNNING_TTL`, or a `pending` row older than `PENDING_TTL`, is reported to clients as `failed`
with a synthetic reason. The stored row is untouched (the worker may still be making progress;
the annotation only governs what the API reports).

### 1.8 Package layout

```
reading-lite/
├── cmd/
│   ├── reader-api/        # HTTP server + worker pool (main wires adapters → core)
│   └── readerctl/         # operator CLI
├── internal/
│   ├── reading/           # PURE domain: types, status machine, URL key, stale annotation
│   ├── store/             # Store port: Postgres adapter (sqlc) + Memory fake + conformance suite + migrations/
│   ├── dispatch/          # in-process dispatcher: channel + worker pool + backoff/retry + recovery sweep
│   ├── pipeline/          # orchestration: extract→embed→similar→summarize→notify
│   ├── readingops/        # command workflows: ingest/import/reprocess across store/blob/dispatch
│   ├── fetch/             # Fetcher port + HTTP adapter + fake
│   ├── extract/           # Extractor port + readability adapter + youtube/reddit + fake
│   ├── embed/             # Embedder port + OpenAI adapter + fake
│   ├── vector/            # VectorIndex port + pgvector adapter + memory fake + conformance suite
│   ├── summarize/         # Summarizer port + Anthropic(emit_reading) adapter + fake
│   ├── notify/            # Notifier port + Resend adapter + fake
│   ├── blobs/             # Blobs port + R2/S3 adapter + memory fake
│   ├── httpapi/           # handlers, routing, auth middleware, DTOs, error model
│   ├── config/            # env config load + validation
│   └── clock/             # Clock port + system + fake
├── testdata/              # HTML fixtures, recorded API bodies, bookmark exports
├── go.mod
└── docs/SPEC.md
```

---

## 2. Tooling, conventions, and CI (set up before Phase 1)

- **Go 1.26**. Track the latest supported 1.26 patch release; initialize with
  `go mod init github.com/bbell/reading-lite`, `go 1.26`, and optionally
  `toolchain go1.26.x` when pinning the local/CI toolchain is useful.
- **Test style**: table-driven subtests (`t.Run`), `t.Parallel()` where there's no shared
  state. Black-box test packages (`package reading_test`) by default so tests exercise the
  public surface; switch to white-box only when testing an unexported helper is genuinely
  cheaper. Assertions with `google/go-cmp` (`cmp.Diff`) — no assertion DSL.
- **Determinism**: no wall-clock, no RNG, no network, no Docker in the default `go test` run —
  Postgres-backed tests are isolated behind `//go:build integration`. Time comes from
  `clock.Clock`; IDs come from an injected `func() string` ID generator (a counter in tests).
- **Fakes live next to their port** (`embed.Fake` in `internal/embed`), exported so any package
  can use them. Each fake records calls and exposes scripted responses/errors.
- **Coverage gate**: domain (`reading`, `dispatch`, `pipeline`) and the `store.Memory` /
  `vector.Memory` fakes ≥ 90% (via the conformance suites); HTTP/adapters ≥ 75% (error paths
  included). `go test -race ./...` must pass — the worker pool is concurrent.
- **Lint**: `gofmt -l` clean, `go vet ./...`, `golangci-lint` (errcheck, staticcheck,
  govet, revive).
- **Key deps**: `pgx/v5`+`pgxpool`, `sqlc` (codegen) + `pgvector-go`,
  `testcontainers-go` (ephemeral Postgres for integration tests), `go-cmp`.
- **CI**: `go build ./... && go vet ./... && go test -race -cover ./...` on every push, plus a
  `go test -tags integration ./...` job (Postgres service / testcontainers); matrix on
  linux/amd64 (deploy target). Lint as a separate required job.
- **Makefile** targets: `test` (fast, fakes only), `test-integration` (`-tags integration`,
  spins Postgres), `test-race`, `lint`, `cover`, `sqlc` (generate), `migrate`, `build`, `run`.

**Phase-0 deliverable test** — prove the harness works before any feature:

```go
// internal/clock/clock_test.go
func TestFakeClock_AdvanceMovesNow(t *testing.T) {
    c := clock.NewFake(time.Unix(1000, 0))
    start := c.Now()
    c.Advance(90 * time.Second)
    if got := c.Now().Sub(start); got != 90*time.Second {
        t.Fatalf("Advance: now moved %v, want 90s", got)
    }
}
```

`clock.Clock` = `interface{ Now() time.Time }`; `clock.System{}` real; `clock.Fake` with
`Now/Advance/Set` guarded by a mutex (workers read it concurrently).

---

## 3. Phase 1 — Domain core (pure, no I/O)

**Goal**: the reading type, status machine, URL idempotency key, and stale annotation as pure
functions. This is the spec's correctness heart; it must be airtight and fast.

### 3.1 Status machine

**Red** — `internal/reading/status_test.go`:

```go
func TestStatus_Transitions(t *testing.T) {
    cases := []struct {
        name      string
        from, to  reading.Status
        wantOK    bool
    }{
        {"queue new", reading.Pending, reading.Running, true},
        {"complete", reading.Running, reading.Ready, true},
        {"fail running", reading.Running, reading.Failed, true},
        {"requeue rate-limited", reading.Running, reading.Pending, true},
        {"reprocess failed", reading.Failed, reading.Pending, true},
        {"reprocess ready", reading.Ready, reading.Pending, true},
        {"cannot ready->running", reading.Ready, reading.Running, false},
        {"cannot pending->ready", reading.Pending, reading.Ready, false},
        {"terminal->same noop rejected", reading.Failed, reading.Failed, false},
    }
    // assert CanTransition(from,to) == wantOK
}
```

**Green**: `Status` string type with consts; `CanTransition(from, to Status) bool` as an
explicit allow-table. No "any→any". Add `(Status).IsTerminal()`.

### 3.2 URL idempotency key

The spec requires "idempotent by URL". Two URLs that a human considers identical must collapse
to one reading. Normalization is a pure function and the single source of idempotency.

**Red** — `internal/reading/url_test.go`, table cases:

| input | `URLKey` output | why |
|---|---|---|
| `HTTP://Example.COM/Path` | `https://example.com/Path` | scheme+host lowercased, scheme upgraded to https for known hosts? (decision: **lowercase host & scheme, keep path case**) |
| `https://example.com/a?utm_source=x&id=7` | `https://example.com/a?id=7` | strip tracking params (`utm_*`, `fbclid`, `gclid`, `ref`) |
| `https://example.com/a/` vs `/a` | identical | trailing slash on non-root normalized |
| `https://example.com/a#frag` | `https://example.com/a` | fragment dropped |
| `https://example.com` | `https://example.com/` | root path canonical |
| `https://m.youtube.com/watch?v=ID&t=10` | `https://www.youtube.com/watch?v=ID` | youtube host canonicalized, `t` stripped, `v` kept |
| `https://youtu.be/ID` | `https://www.youtube.com/watch?v=ID` | short-link expanded |
| `not a url` / `ftp://x` / `javascript:…` | error | only http(s) accepted |

**Green**: `func URLKey(raw string) (key string, err error)`; `func ClassifySource(key) SourceKind`
(`web|youtube|reddit|markdown`). Keep the strip-list and host rules as package vars so they're
easy to extend and to read in tests.

### 3.3 Stale annotation (read-time overlay)

**Red** — `internal/reading/stale_test.go`:

```go
func TestAnnotateStale(t *testing.T) {
    now := time.Unix(10_000, 0)
    cfg := reading.TTLs{Pending: 10 * time.Minute, Running: 30 * time.Minute}
    cases := []struct {
        name   string
        in     reading.Reading
        want   reading.Status
        wantReasonContains string
    }{
        {"fresh pending unchanged", mk(reading.Pending, now.Add(-1*time.Minute)), reading.Pending, ""},
        {"expired pending -> failed", mk(reading.Pending, now.Add(-11*time.Minute)), reading.Failed, "timed out before processing"},
        {"fresh running unchanged", mkRun(reading.Running, now.Add(-5*time.Minute)), reading.Running, ""},
        {"stuck running -> failed", mkRun(reading.Running, now.Add(-31*time.Minute)), reading.Failed, "stalled"},
        {"ready never annotated", mk(reading.Ready, now.Add(-99*time.Hour)), reading.Ready, ""},
    }
    // r2 := reading.AnnotateStale(c.in, now, cfg) — assert status + reason, AND that c.in is unmutated
}
```

**Green**: `AnnotateStale(r Reading, now time.Time, ttls TTLs) Reading` returns a **copy** with
overlaid `Status`/`StaleReason`; original untouched (assert no mutation). Uses `started_at` for
running, `created_at` for pending.

**Refactor**: collect TTLs and the strip-list into `reading` package vars; ensure `Reading`
struct fields are documented and JSON-tagged for later DTO reuse.

**Done when**: `reading` package is ~100% covered, zero imports outside stdlib.

---

## 4. Phase 2 — Store: Postgres (sqlc) behind a conformance suite

**Goal**: readings metadata in Postgres — list/search (FTS)/sort/tags/pagination, url_key
idempotency, status sweep, delete — with bounded memory (indexed queries, never the whole
corpus). Two implementations behind one `Store` interface: `store.Memory` (fast fake, every
test run; also a zero-infra dev backend) and `store.Postgres` (sqlc + pgx). One **conformance
suite** proves both behave identically.

### 4.1 Schema (migration `0001_init.sql`, embedded)

```sql
create extension if not exists vector with schema public;

create table readings (
  id text primary key,
  url text not null,
  url_key text not null unique,                  -- idempotency, DB-enforced
  status text not null,                          -- pending|running|ready|failed
  source_kind text not null,
  title text, author text, site text, lang text,
  word_count int, extraction_mode text,
  content_key text, raw_key text,                -- R2 keys
  summary text, summary_json jsonb, similar_json jsonb, diagnostics_json jsonb,
  error text, process_attempts int not null default 0,
  tags text[] not null default '{}',
  search tsvector generated always as (
    to_tsvector('english',
      coalesce(title,'')||' '||coalesce(author,'')||' '||
      coalesce(summary,'')||' '||array_to_string(tags,' '))
  ) stored,
  created_at timestamptz not null, started_at timestamptz,
  finished_at timestamptz, updated_at timestamptz not null
);
create index readings_search_idx on readings using gin (search);
create index readings_tags_idx   on readings using gin (tags);
create index readings_page_idx   on readings (created_at desc, id desc);
create index readings_status_idx on readings (status);

create table reading_vectors (                   -- VectorIndex backing (Phase 4/6)
  reading_id text primary key references readings(id) on delete cascade,
  embedding  vector(1536) not null
);
create index reading_vectors_ann_idx on reading_vectors using hnsw (embedding vector_cosine_ops);
```

### 4.2 Query layer (sqlc)

Hand-write SQL in `query.sql`; sqlc generates typed Go (`pgx/v5`, `pgvector-go`). The
non-obvious ones:
- **search**: `… where ($q='' or search @@ websearch_to_tsquery('english',$q)) and ($tags='{}'
  or tags @> $tags) and ($status='' or status=$status) …` with generated newest/oldest/title
  query variants. When `q` is present, keyset cursors include rank first and then the secondary
  sort key, so paginated ranked results cannot skip or duplicate rows. A companion `count`
  returns `total`.
- **idempotent insert**: `insert … on conflict (url_key) do nothing returning id` (empty result
  ⇒ already exists ⇒ adapter returns `ErrConflict`).
- **sweep**: `select id, process_attempts from readings where status='pending' or
  (status='running' and started_at < $cutoff)`.

### 4.3 The conformance suite (write FIRST — it defines the contract)

`storetest.RunContract(t, func() store.Store)` holds every behavioral test; it is invoked from
two places: `store/memory_test.go` (always) and `store/postgres_test.go` (`//go:build
integration`, testcontainers Postgres with migrations applied). Cases:

- `RoundTrip` — `SaveReading` then `GetByID` deep-equal (cmp.Diff).
- `URLKeyIdempotency` — second save with same `url_key` → `ErrConflict`; `GetByURLKey` returns
  the first; miss → `ErrNotFound`.
- `SearchFTS` — `q="kubernetes"` returns only matches, ranked; adversarial `q` (`'AND OR "'`,
  emoji) never errors (websearch_to_tsquery + the Memory tokenizer both tolerate it).
- `TagFilterAND` — `tags=[go,db]` returns only readings having **all**; combined `q`+tags AND.
- `StatusFilter`, `SortModes` (`newest|oldest|title`).
- `KeysetPagination` — 25 rows, limit 10, walk the cursor: no dup/skip, correct `total`.
- `UpdateStatusAdvancesTimestamps` — via injected clock.
- `ReplaceTags` — idempotent; reflected in tag filter + FTS.
- `ListNonTerminal` — returns only `pending` + stale-`running` ids (sweep input).
- `Delete` — removes the reading (and, via FK cascade, its vector row).
- `ConcurrentSaves` — N goroutines, distinct ids, under `-race`; all present.

> Writing the contract first, then making `store.Memory` pass, gives a fast red/green loop; the
> Postgres adapter must then satisfy the *same* asserted behavior — including `ErrConflict` and
> FTS ranking — so any divergence between fake and prod is impossible to miss.

### 4.4 Green — `Store` interface

```go
type Store interface {
    SaveReading(ctx context.Context, r reading.Reading) error          // insert; ErrConflict on url_key clash
    GetByID(ctx context.Context, id string) (reading.Reading, error)
    GetByURLKey(ctx context.Context, key string) (reading.Reading, error)
    UpdateStatus(ctx context.Context, id string, s reading.Status, f StatusFields) error
    ReplaceTags(ctx context.Context, id string, tags []string, f TagFields) error
    Search(ctx context.Context, q Query) (Page, error)                 // q, tags, status, sort, cursor, limit
    ListNonTerminal(ctx context.Context, runningCutoff time.Time) ([]Pending, error) // {id, attempts}
    Delete(ctx context.Context, id string) error
}
var ErrNotFound = errors.New("not found"); var ErrConflict = errors.New("conflict")
```

`store.Memory` implements this with maps + a slice (sort/filter/keyset in Go); `store.Postgres`
delegates to sqlc. Idempotency now lives in the DB (`url_key` unique), so the ingest handler
maps `ErrConflict` → "return existing reading".

**Refactor**: keep all SQL in `query.sql`; keep `store.Memory` dependency-free; share the
`Query`/`Page`/`Pending` DTOs across both adapters so the conformance suite is backend-agnostic.

**Done when**: `RunContract` is green against `store.Memory` on every run and against
`store.Postgres` under `-tags integration`; `-race` clean.

---

## 5. Phase 3 — In-process dispatcher + recovery sweep

**Goal**: run the pipeline asynchronously with retry/backoff, **rate-limit → delayed re-dispatch
(no attempt consumed)**, **retry-exhaustion → retryable `failed`**, and **crash recovery** via a
startup sweep. The decision logic is a pure function (fake-clock tested); the dispatcher's seam
is a synchronous handle call, so no real goroutines or timers are needed for the semantics tests.

### 5.1 Work item & outcomes

```go
type item struct { id string; attempt int }                 // rides the in-memory channel

type Outcome int
const (
    Done    Outcome = iota // success
    Retry                  // transient → backoff, attempt++
    Requeue                // rate-limited → re-dispatch after delay, attempt UNCHANGED
    Fail                   // permanent → reading=failed now
)
type Result struct { Outcome Outcome; After time.Duration; Err error }
```

Pipeline error → outcome: `RateLimitError{RetryAfter}` → `Requeue`; `errors.Is(err, ErrPermanent)`
→ `Fail`; else → `Retry`. (Same `classify(err)` the pipeline uses — Phase 5.)

### 5.2 Pure decision function

```go
// decide is pure: given the result and where we are, what happens next?
func decide(r Result, attempt, max int) Action
// Action{ Redispatch bool; Delay time.Duration; NextAttempt int; MarkFailed bool }
```

**Red** — `internal/dispatch/decide_test.go` (no clock, no I/O):
- `TestDecide_Done` → no redispatch, not failed.
- `TestDecide_RetryBackoff` → redispatch, `NextAttempt=attempt+1`, `Delay=backoff(attempt)`
  (assert the 1s,2s,4s… capped schedule across a table of attempts).
- `TestDecide_RequeueKeepsAttempt` → redispatch, `NextAttempt=attempt` (unchanged), `Delay=After`.
- `TestDecide_RetryExhaustion` → on a `Retry` where `attempt+1 >= max`: `MarkFailed=true`,
  `Redispatch=false` (reading becomes retryable `failed`; the in-memory item is dropped).
- `TestDecide_PermanentFailsFast` → `Fail` → `MarkFailed=true` regardless of attempt.

### 5.3 Dispatcher (channel + worker pool + delayer)

Delays go through a tiny injectable seam so tests never sleep:

```go
type Delayer interface { After(d time.Duration, fn func()) } // real: time.AfterFunc; fake: records & fires on demand
type Dispatcher struct {
    ch      chan item
    Handler func(ctx context.Context, id string) Result
    Store   Store
    Clock   clock.Clock
    Delay   Delayer
    Workers, Max int
    Backoff func(int) time.Duration
}
func (d *Dispatcher) Submit(id string)                 // enqueue at attempt 0 (non-blocking; sweep is the overflow backstop)
func (d *Dispatcher) handle(ctx context.Context, it item) // run Handler, apply decide(), persist, maybe Delay→re-dispatch
func (d *Dispatcher) Run(ctx context.Context)          // worker pool draining ch; honors ctx for graceful drain
func (d *Dispatcher) Sweep(ctx context.Context) error  // startup recovery
```

**Red** — `internal/dispatch/dispatcher_test.go` (fake Store, fake Delayer, scripted Handler):
- `TestDispatch_SubmitRunsHandlerOnce` — `Submit` → handler invoked once → reading `ready`.
- `TestDispatch_RetrySchedulesDelayedRedispatch` — handler `Retry` → asserts `Delay.After` called
  with `backoff(0)`; firing the fake delayer re-invokes the handler at attempt 1.
- `TestDispatch_RequeueDoesNotConsumeAttempt` — handler `Requeue{After:30s}` → `Delay.After(30s,…)`;
  on fire the re-dispatched item still has `attempt==0`; reading stays `pending`.
- `TestDispatch_RetryExhaustionFailsRetryable` — handler always `Retry`; after `Max` attempts the
  reading is `failed` (and a later `Submit` of the same id can run again — reprocessable); no
  further `Delay.After` scheduled.
- `TestDispatch_GracefulDrain` — `Run` honors `ctx.Done()`: stops pulling, lets in-flight finish.
- `TestDispatch_ConcurrencyBounded` — with `Workers=2`, never more than 2 handlers run at once
  (scripted handler blocks on a barrier; assert max concurrency) under `-race`.

### 5.4 Recovery sweep

**Red**:
- `TestDispatch_RecoverySweepReenqueuesNonTerminal` — seed the store with `pending`, stale
  `running`, `ready`, `failed`; `Sweep` re-`Submit`s only the `pending` + stale-`running` ids
  (via `Store.ListNonTerminal`, a single indexed query), leaving terminal readings alone.
- `TestDispatch_SweepResumesAtStoredAttempt` — a `pending` reading carrying `process_attempts=2`
  re-dispatches at attempt 2 so `MAX_ATTEMPTS` is honored across restarts.

**Green**: implement `decide` first (pure), then `handle` (decide + persist via `Store.UpdateStatus`,
mirroring `attempt`→`process_attempts`), then `Run`/`Submit`/`Sweep`. `main` calls `Sweep` once at
startup before serving. The HTTP layer (Phase 8) drives an **inline mode** (call `handle`
synchronously) so handler tests are deterministic without goroutines.

**Refactor**: `decide` stays the single branch point (legible, fuzzable); `handle` is the only
place that touches `Store` + `Delay`.

**Done when**: every behavior above is proven by name; `-race` clean; the spec's retry /
rate-limit / retry-exhaustion / crash-recovery bullets each map to a passing test.

---

## 6. Phase 4 — Ports & fakes for external services

**Goal**: define every external port and a faithful in-memory fake. No real network code yet.
This unblocks the pipeline (Phase 5) entirely against fakes.

Define interfaces (all methods take `context.Context`):

```go
// fetch
type Fetcher interface { Get(ctx, url string) (Resource, error) } // Resource{Body []byte, ContentType, FinalURL, Status}
// extract
type Extractor interface { Extract(ctx, r Resource) (Article, error) } // Article{Title,Author,Site,Lang,Markdown,Mode,WordCount}
// embed
type Embedder interface { Embed(ctx, text string) ([]float32, error) } // len==1536
// vector — backed by pgvector in prod (vector.Postgres), vector.Memory fake
type VectorIndex interface {
    Upsert(ctx, id string, vec []float32) error
    Query(ctx, vec []float32, topK int, excludeID string) ([]Match, error) // Match{ID,Score} — hydrate via Store
    Delete(ctx, id string) error
}
// summarize
type Summarizer interface { Summarize(ctx, in SummaryInput) (Summary, error) } // forced emit_reading
// notify
type Notifier interface { Notify(ctx, n Email) error }
// blobs
type Blobs interface {
    Put(ctx, key string, data []byte, contentType string) error
    Get(ctx, key string) ([]byte, string, error)
    Delete(ctx, key string) error
}
```

**Red/Green for fakes**: each fake gets a tiny test proving it honors its contract and can be
scripted to error, e.g.:

```go
func TestEmbedFake_ScriptedVectorAndError(t *testing.T) {
    f := &embed.Fake{Vec: make([]float32, 1536)}
    v, _ := f.Embed(ctx, "hi"); if len(v) != 1536 { t.Fatal("dim") }
    f.Err = errors.New("boom")
    if _, err := f.Embed(ctx, "x"); err == nil { t.Fatal("want error") }
    if f.Calls != 2 { t.Fatalf("calls=%d", f.Calls) }
}
```

`vector.Memory` is a real cosine-similarity index over a map; the production adapter is
`vector.Postgres` (pgvector). Both satisfy a `vectortest.RunContract` suite —
`QueryRanksByCosine`, `ExcludesSelf`, `DeleteRemoves` — run against the fake on every test and
against pgvector under `-tags integration` (the cosine math + ANN ordering must agree).

**Done when**: every port has an interface + fake + fake-contract test; nothing imports a real
SDK yet.

---

## 7. Phase 5 — Processing pipeline (the heart, fakes only)

**Goal**: orchestrate the full process pipeline against fakes, covering the happy path, the
extraction fallback tiers, source special-casing, and partial-failure policy. This is where the
spec's "Process" section lives and where most behavior tests concentrate.

Pipeline signature (this *is* the dispatcher's handler):

```go
func (p *Pipeline) Process(ctx context.Context, readingID string) dispatch.Result
```

Ordered steps, each guarded and recorded into `diagnostics_json` (timings, tier, model ids):

1. Load reading; mark `running` (set by the dispatcher before invoking; asserted here).
2. **Acquire content**:
   - `source_kind == markdown` → use stored markdown, skip fetch/extract.
   - `youtube` → oEmbed floor (title/author) + best-effort description/transcript.
   - `reddit` → **do not fetch**; set `failed` with guidance, `Fail` outcome.
   - else → `Fetcher.Get`.
3. **Extract** with fallback tiers: Readability → raw-DOM salvage → `raw_only`. Record
   `extraction_mode`.
4. Persist blobs: raw body → `raw_key`, extracted markdown → `content_key` (via `Blobs`).
5. **Embed** extracted text → 1536-vec.
6. `VectorIndex.Upsert(readingID, vec)` (writes `reading_vectors` in Postgres).
7. `VectorIndex.Query(vec, topK, excludeSelf)` → hydrate match ids via `Store` → snapshot `similar_json`.
8. **Summarize once** via `Summarizer` (forced `emit_reading`) → title/summary/tags/structured.
9. Persist reading: `ready`, fields, tags (`ReplaceTags`), `finished_at`.
10. **Optionally notify** (Resend) if configured; notify failure must **not** fail the reading.

### 7.1 Red — pipeline tests (fakes wired, `store.Memory` + `vector.Memory`)

- `TestPipeline_HappyPath` — web URL → fetch→extract(readability)→blobs(2 puts)→embed→upsert→
  query→summarize→ready. Assert: status `ready`, `extraction_mode=readability`, tags from
  summary persisted, `content_key`/`raw_key` set, vector upserted, `similar_json` populated,
  one summarizer call (the "summarize once" guarantee), one notify call.
- `TestPipeline_ExtractionFallback_RawDOM` — extractor returns readability-failure;
  salvage succeeds → `extraction_mode=raw_dom`, still `ready`.
- `TestPipeline_ExtractionFallback_RawOnly` — both fail; `raw_only` path still embeds+summarizes
  raw text → `ready` with `extraction_mode=raw_only`.
- `TestPipeline_Reddit_FailsWithGuidance` — `reddit` source → no fetch call (assert fake Fetcher
  uncalled), status `failed`, `error` contains the reddit-import guidance, outcome `Fail`.
- `TestPipeline_YouTube_OEmbedFloor` — fetch unavailable transcript → still `ready` with
  title/author from oEmbed floor; `extraction_mode` reflects floor.
- `TestPipeline_Markdown_SkipsFetchExtract` — markdown import → Fetcher & Extractor uncalled;
  embed/summarize still run; `ready`.
- `TestPipeline_RateLimited_Requeues` — Embedder returns `RateLimitError{RetryAfter:30s}` →
  outcome `Requeue{After:30s}`, reading stays `pending` (not failed). (Rate-limit awareness.)
- `TestPipeline_SummarizerError_RetriesNotDoubleSummarize` — summarizer fails once → outcome
  `Retry`; on the retried run, assert fetch/extract are skipped, vector embed/upsert may be
  retried after the content checkpoint, and summarize is attempted again exactly once per run
  (no double-summary within a run).
- `TestPipeline_NotifyFailureDoesNotFailReading` — Notifier errors → reading still `ready`,
  error logged in diagnostics, outcome `Done`.
- `TestPipeline_FetchHardError_Fails` — 404/blocked → `failed` with reason; outcome by policy
  (`Retry` for 5xx/timeout, `Fail` for 4xx).

### 7.2 Green

Build `Pipeline` with injected ports + `Store` + `Clock` + `IDGen` + `Config`. Keep each step a
small private method returning `(stepResult, error)`; translate errors→`dispatch.Result` in one
`classify(err)` switch (shared with Phase 3's mapping). Idempotent re-entry: guard content/tag
writes with the store `ExpectedStartedAt` fence (the run lease) and use run-scoped blob keys, so
stale forced runs cannot overwrite replacement content. That fence — not the dispatcher's
bounded forced-recovery drain (`ForceWaitTTL`, which proceeds on timeout) — is the correctness
backstop for a handler stuck in a non-cancelable call. Retried runs skip already-completed
fetch/extract work and may repeat vector upsert after the content checkpoint — this is what
makes `Retry` safe.

**Refactor**: extract `acquireContent` (the source-kind switch) and `extractTiers` (the
fallback ladder) as named, separately tested units.

**Done when**: every Process branch in the spec has a passing named test; pipeline depends only
on ports + store + clock.

---

## 8. Phase 6 — Real adapters (contract tests via httptest)

**Goal**: implement each real adapter and verify it (a) builds the correct outbound request and
(b) parses real-shaped responses, using `httptest.NewServer` as the upstream. No live network.
Pattern for every adapter:

```go
func TestOpenAIEmbed_RequestAndParse(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if got := r.Header.Get("Authorization"); got != "Bearer test-key" { t.Errorf("auth=%q", got) }
        var body struct{ Input string `json:"input"`; Model string `json:"model"` }
        json.NewDecoder(r.Body).Decode(&body)
        if body.Model != "text-embedding-3-small" { t.Errorf("model=%q", body.Model) }
        w.Write(loadFixture(t, "openai_embed_1536.json"))
    }))
    defer srv.Close()
    e := embed.NewOpenAI("test-key", embed.WithBaseURL(srv.URL))
    v, err := e.Embed(ctx, "hello"); /* assert err nil, len(v)==1536 */
}
```

Adapters + the contract specifics each must prove:

- **`embed.OpenAI`** — endpoint `/v1/embeddings`, `model=text-embedding-3-small`, dim 1536;
  parse `data[0].embedding`; map HTTP 429 (+`Retry-After`) → `RateLimitError`; 5xx → retRetry; 4xx → permanent.
- **`summarize.Anthropic`** — Messages API with `tools=[emit_reading]` and
  `tool_choice={type:"tool",name:"emit_reading"}` (**forced tool use**); parse the
  `tool_use` block's `input` into `Summary`; test the **forced-tool path** and the
  malformed/absent tool_use → error. 429 → `RateLimitError`.
- **`vector.Postgres`** (pgvector) — `Upsert/Query/Delete` against `reading_vectors`; verified
  by `vectortest.RunContract` under `-tags integration` (testcontainers), **not** httptest —
  it's a DB adapter, not an HTTP one. Assert 1536-dim enforcement, cosine ordering, self-exclusion.
- **`blobs.R2`** — S3-compatible (`aws-sdk-go-v2` with custom endpoint + path-style);
  Put/Get/Delete round-trip against an httptest S3 stub or a MinIO container (build-tagged
  `//go:build integration`); unit-level: assert correct bucket/key/content-type composition.
- **`notify.Resend`** — `/emails` POST shape (from/to/subject/html); 2xx ok; non-2xx → error
  that the pipeline swallows (proven in 7.1).
- **`fetch.HTTP`** — sets UA, obeys `WithTimeout`, **caps body size** (assert a >maxBytes
  response is truncated/rejected, not OOM), follows redirects to `FinalURL`, returns status;
  blocks non-http(s) and (optionally) private IPs (SSRF guard — see Phase 11).

**Done when**: each adapter has request-shape + happy-parse + at least one error-mapping test;
integration-tagged tests (R2/MinIO) documented and runnable but excluded from default `go test`.

---

## 9. Phase 7 — Extraction details (fixture-driven)

**Goal**: lock down `extract.Readability` and source special-casing against real HTML fixtures
in `testdata/`.

**Red**:
- `TestReadability_ExtractsArticle` — fixture blog post → title/author/markdown, sensible
  `word_count`, `mode=readability`. Use `cmp.Diff` against a golden markdown file (update via a
  `-update` flag).
- `TestReadability_RawDOMSalvage` — fixture with no semantic article wrapper → salvage yields
  `mode=raw_dom` non-empty text.
- `TestReadability_RawOnly` — binary/garbage/JS-only fixture → `mode=raw_only`, text is the
  stripped raw.
- `TestYouTube_OEmbed` — httptest oEmbed endpoint → title/author floor; transcript-present and
  transcript-absent variants.
- `TestReddit_Guidance` — classifier routes reddit URL; extractor/pipeline yields the canonical
  guidance string (assert exact operator-facing message).

**Green**: wrap `go-readability` + an HTML→markdown converter (e.g. `html-to-markdown`) behind
`Extractor`; implement the tier ladder; implement YouTube oEmbed client and Reddit guidance
constant. Keep tier selection logic pure and separately tested from the HTML libraries.

**Done when**: golden files committed; tiers deterministic across runs.

---

## 10. Phase 8 — HTTP API

**Goal**: the read/browse/ingest surface, auth, idempotency, imports, reprocess, error model,
and read-time stale annotation. Handlers tested via `httptest` against a server wired to the
real in-mem (blob-backed) store + fakes + the dispatcher in **inline mode** (so ingest→read can
be driven within a test by calling `Pipeline.Process` directly).

### 10.1 Endpoints

| Method | Path | Auth | Behavior |
|---|---|---|---|
| `GET` | `/api/healthz` | none | liveness |
| `POST` | `/api/readings` | bearer | ingest URL; idempotent; returns `{id,status}` (201 new / 200 existing) |
| `POST` | `/api/readings/import/markdown` | bearer | create reading from supplied markdown |
| `POST` | `/api/readings/import/bookmarks` | bearer | bulk import (Netscape HTML or JSON); per-URL result |
| `GET` | `/api/readings` | bearer | list/search: `q`, `tags`, `status`, `sort`, `cursor`, `limit` |
| `GET` | `/api/readings/{id}` | bearer | detail: summary, similar, diagnostics, stale-annotated status |
| `GET` | `/api/readings/{id}/content` | bearer | extracted markdown (from Blobs) |
| `GET` | `/api/readings/{id}/raw` | bearer | raw content (from Blobs) |
| `POST` | `/api/readings/{id}/reprocess` | bearer | re-enqueue (idempotent) |

### 10.2 Red — middleware

- `TestAuth_MissingTokenRejected` → 401.
- `TestAuth_WrongTokenRejected` → 401; uses constant-time compare (assert no early-exit timing
  leak by code review; test just asserts 401).
- `TestAuth_HealthzSkipsAuth` → 200 without token.
- `TestAuth_ValidTokenPasses` → handler reached.

### 10.3 Red — ingest idempotency (spec-critical)

- `TestIngest_NewURLCreatesPending` → 201, status `pending`, job enqueued.
- `TestIngest_ExistingReadyReturnsSame` → POST same URL again → 200, **same id**, no new job.
- `TestIngest_ExistingPendingReturnsSame` → 200, same id, no duplicate job.
- `TestIngest_FailedReprocessesInPlace` → existing `failed` URL → 200, **same id**, status flips to
  `pending`, new job enqueued (reprocess-in-place, not a new reading).
- `TestIngest_NormalizesBeforeLookup` → `?utm_*` variants collapse to one reading (uses `URLKey`).
- `TestIngest_InvalidURL` → 400 with error body.

### 10.4 Red — read & browse

- `TestGetReading_AnnotatesStaleAtReadTime` — store a `running` row older than `RUNNING_TTL`;
  GET reports `failed` (annotated) while a direct store fetch still shows `running` (no write).
- `TestListReadings_QTagsSortPaginate` — exercises store search through the handler; assert DTO
  shape, `total`, `next_cursor`.
- `TestGetContent_AuthGatedAndStreamsBlob` / `TestGetRaw_…` — returns blob bytes + content-type;
  404 when key absent.
- `TestReprocess_ReenqueuesAndReturns202`.
- `TestImportMarkdown_CreatesReadingAndEnqueues` — source_kind `markdown`.
- `TestImportBookmarks_BulkResult` — mixed valid/dupe/invalid URLs → per-URL `{url,id,result}`
  array (`created|existing|invalid`); dupes collapse via `URLKey`.

### 10.5 Green

`readingops.Service{ Store, Dispatcher, Blobs, Clock, TTLs, NewID }` owns command sequencing for
URL ingest, markdown/bookmark import, failed-reading markdown replacement, and reprocess. It is
the only boundary that coordinates `store.Store`, `blobs.Blobs`, and dispatcher enqueue/force
enqueue for those workflows.

`httpapi.Server{ Store, Dispatcher, Blobs, Clock, Token, TTLs, NewID }` with one
`Routes() http.Handler` delegates command workflows to `readingops` and keeps request decoding,
auth, route selection, DTO mapping, response status mapping, and error serialization in the HTTP
package. DTO structs map domain→JSON (never expose internal columns directly). Single error model:

```json
{ "error": { "code": "invalid_url", "message": "…" } }
```

Helpers `writeJSON`, `writeErr(code, status, msg)`. Auth middleware uses
`subtle.ConstantTimeCompare`. Stale annotation applied in the read handlers via
`reading.AnnotateStale` before DTO mapping.

**Refactor**: a `decode[T]` generic for request bodies with size limit (`http.MaxBytesReader`);
shared pagination cursor codec; keep multi-resource workflow ownership in `readingops` so the
HTTP layer stays thin.

**Done when**: every endpoint + the idempotency matrix + read-time stale annotation are green.

---

## 11. Phase 9 — End-to-end integration

**Goal**: prove the whole machine fits together with the real Store + the real in-process
dispatcher + fakes for external services, exercised through the HTTP surface.

`internal/httpapi/e2e_test.go` (or `cmd/reader-api` blackbox):

- `TestE2E_IngestProcessRead` — `POST /api/readings` → drain the dispatcher → `GET /api/readings/{id}`
  shows `ready` with summary, similar (empty on first, populated on second related ingest),
  diagnostics; `GET …/content` returns extracted markdown.
- `TestE2E_SimilarAcrossTwoReadings` — ingest A then B (embeddings scripted close) → B's detail
  lists A as similar with a score.
- `TestE2E_FailedThenReprocessSucceeds` — first run fails (scripted fetch error) → `failed` →
  `POST …/reprocess` with fetch now succeeding → `ready`.
- `TestE2E_RetryExhaustionFailsRetryable` — handler scripted to always `Retry` → after
  `MAX_ATTEMPTS` (driven by firing the fake delayer) the reading is `failed` and reprocessable;
  the dispatcher is idle.
- `TestE2E_RateLimitRequeue` — embedder rate-limited once → reading stays `pending`; firing the
  fake delayer after `RetryAfter` completes → `ready`.
- `TestE2E_RecoveryAfterRestart` — submit, then drop the dispatcher before it processes; re-`Open`
  the store, build a new dispatcher, run `Sweep` → the reading reaches `ready` (crash recovery
  via the startup sweep).

**Done when**: these end-to-end stories pass under `-race`, wired exactly like `main` but with
fake external adapters.

---

## 12. Phase 10 — Operator CLIs (`readerctl`)

**Goal**: the spec's operator surface, each subcommand a thin shell over the same core packages,
tested by invoking the command with fakes/in-mem store and asserting effects + output.

Implemented subcommands (with representative tests that define each):

- `import url <u>` / `import markdown <file>` / `import bookmarks <file>` —
  `TestRun_ImportURLPreflightsFailedAsReprocessedAndPrintsStatusLine`,
  `TestRun_ImportMarkdownCreatesRawBlobAndPrintsStatusLine`, and
  `TestRun_ImportBookmarksPrintsCreatedExistingInvalidLines` assert command output and effects.
  Shared bookmark parsing lives in `internal/bookmarks` and is tested for Netscape/HTML,
  JSON array, and JSON object inputs.
- `audit` — scan corpus, report counts by status, orphaned blobs/vectors, stuck `running`;
  `TestRun_AuditTextReportsStatusStaleMissingBlobAndInventoryLines` and
  `TestRun_AuditJSONReportsCompleteSchema` cover text and JSON output against a seeded store.
- `recover` — re-dispatch stuck/failed readings; `TestCmdRecover_ReenqueuesStuck` (dry-run default,
  `--apply` to mutate). `TestRun_RecoverDryRunTargetsFailedAndStaleOnlyInUnifiedScanOrder` and
  `TestRun_RecoverApplyContinuesAfterPerIDFailure` pin target selection and per-ID failure behavior.
- `drop <id|--all>` — delete reading + its blobs + its vector; `TestCmdDrop_RemovesAllArtifacts`
  asserts Store + Blobs + VectorIndex all cleaned; **requires `--yes` confirmation**.
  `TestRun_DropWithoutYesIsDryRunAndDoesNotMutate`,
  `TestRun_DropExplicitMissingIDAbortsBeforeMutation`, and
  `TestRun_DropYesDeletesRawContentVectorThenMetadata` pin the destructive-command contract.
- `smoke` — hit a running API's healthz + a throwaway ingest; `TestCmdSmoke_PassFail`.
  `TestRun_SmokePostsIngestJSONAndValidatesResponse` pins request shape and success parsing,
  while `--token-env` lets deploy/staging plans authenticate without printing a secret.
- `deploy` / `staging {up,down,promote}` — these orchestrate environment/process; keep the
  Go-testable core (config rendering, target selection) pure and unit-tested, shell-out parts
  behind a `Runner` port with a fake
  (`TestRun_DeployApplyRunsExactStepsAndStopsOnFailure`,
  `TestRun_StagingDownDoesNotRequireSmokeInputs`). Deploy and staging up/promote require
  `--smoke-token-env` so the generated smoke step can authenticate the ingest request.

**Green**: `internal/readerctl.Command` takes dependencies as parameters so tests inject fakes.
`cmd/readerctl` delegates to `readerctl.Main`, which constructs only Phase-10-safe defaults:
smoke and dry-run deploy/staging planning can run, while stateful commands return deterministic
configuration errors until Phase 11 production construction exists. **Destructive commands
default to dry-run and demand explicit confirmation**.

**Done when**: each subcommand has a behavior test; destructive ones have a refuse-to-confirm
test; `go test ./internal/bookmarks ./internal/httpapi ./internal/readerctl ./cmd/readerctl`
and `make test` pass.

---

## 13. Phase 11 — Config, observability, security, lifecycle hardening

**Config** (`internal/config`): load + **validate** all env at startup; missing required →
fail fast with a clear message. `TestConfig_RequiresToken`, `TestConfig_Defaults`.

Required env: `READER_API_TOKEN`, `DATABASE_URL` (TLS, pooled endpoint), `OPENAI_API_KEY`,
`ANTHROPIC_API_KEY`, `R2_*` (endpoint/key/secret/bucket — content blobs only),
`RESEND_API_KEY`+`NOTIFY_FROM`/`NOTIFY_TO`, `PENDING_TTL`, `RUNNING_TTL`, `MAX_ATTEMPTS`,
`WORKER_CONCURRENCY`, `DISPATCH_BUFFER`, `PG_MAX_CONNS`, `LISTEN_ADDR`.

**Security** (each a test or documented review item):
- Bearer auth constant-time compare; token never logged. `TestAuth_*` already cover rejection.
- **SSRF guard** in `fetch.HTTP`: reject requests resolving to private/loopback/link-local IPs;
  `TestFetch_BlocksPrivateIP`. (Personal service, but ingest takes arbitrary URLs.)
- Body-size caps on both ingest request bodies and outbound fetch (proven in Phases 6/10).
- Blob keys derived from server-side ids only (never client input) → no path traversal;
  `TestBlobKey_NoTraversal`.
- Secrets sourced only from env/config; a test greps that no secret is interpolated into logs
  (review-level).

**Observability**: structured logging (`log/slog`) with a request id; per-pipeline-step timing
into `diagnostics_json` (already populated in Phase 5); `/api/healthz` returns build info + a
Postgres ping + an R2 reachability check. Optional `/metrics` (Prometheus) counters: ingests,
processed, failures by reason, requeues, retry-exhaustions, dispatch queue depth.

**Lifecycle**: `cmd/reader-api/main.go` runs embedded store migrations, wires
config→pgxpool→adapters→store→dispatcher→server, runs the startup recovery sweep (§1.4), starts
the worker pool and `http.Server`, and on `SIGTERM` does graceful shutdown — stop accepting
HTTP, drain in-flight pipelines, close the pool. `TestMain_GracefulShutdown` (or a focused
`Server.Shutdown` test) asserts in-flight requests drain.

**Done when**: config validation, SSRF/body caps, graceful shutdown all covered; no secret
appears in any log path.

---

## 14. Phase 12 — (Optional) alternative backends & multi-instance

The ports make several swaps drop-in, each re-running the relevant conformance suite:
- `blobs.FS` / `blobs.MinIO` for `Blobs` (local/dev without R2).
- `store.Memory` + `vector.Memory` as a **zero-infra deploy** (small corpus, no Postgres) — the
  same fakes the tests use, now in production.
- **Multi-instance**: promote the in-process dispatcher to a Postgres-backed durable queue
  (`SELECT … FOR UPDATE SKIP LOCKED`) — a contained add-on, since the dispatcher seam and the
  `Store` already exist. Only do this if a single instance stops sufficing.
Because the ports are unchanged, the whole domain/pipeline/HTTP suite runs unmodified against
any backend — the final proof the abstractions hold.

---

## 15. Test pyramid & how the fakes compose

```
   integration (-tags integration)   store.Postgres + vector.Postgres via testcontainers; R2/MinIO
  ──────────────────────────────────
        e2e (Phase 9, 6 stories)      real store.Memory + dispatcher, fake externals, via HTTP
     ────────────────────────────
   adapter contract tests (Phase 6/7) httptest upstreams, golden fixtures
  ──────────────────────────────────
 domain · store.Memory · dispatch · pipeline · http   pure + fakes ← the bulk
────────────────────────────────────────
```

- **Fast unit layer** (default `go test`, sub-second): pure domain, dispatch semantics (fake
  clock + fake delayer), pipeline (fakes), `store.Memory`/`vector.Memory`, handlers (httptest).
- **Contract layer** pins each HTTP adapter to its real API shape with `httptest` + fixtures.
- **E2E layer** wires the whole app on `store.Memory` + fakes, driven through HTTP.
- **Integration layer** (`//go:build integration`, on demand / CI) runs the **same** Store and
  VectorIndex conformance suites against real Postgres (testcontainers), plus R2/MinIO — proving
  the production adapters match the fakes the rest of the suite trusts.

---

## 16. Spec → test traceability (acceptance checklist)

Each spec bullet must map to at least one named test before it's "done":

| Spec capability | Proving test(s) |
|---|---|
| Submit URL → pollable id, async | `TestIngest_NewURLCreatesPending`, `TestE2E_IngestProcessRead` |
| Idempotent by URL (ready/pending return existing) | `TestIngest_ExistingReady/PendingReturnsSame` |
| Failed reprocess in place | `TestIngest_FailedReprocessesInPlace`, `TestE2E_FailedThenReprocessSucceeds` |
| Markdown import | `TestImportMarkdown_*`, `TestPipeline_Markdown_SkipsFetchExtract` |
| Bulk bookmark import | `TestImportBookmarks_BulkResult`, `TestCmdImport_*` |
| Extract w/ raw-DOM salvage + raw_only | `TestReadability_RawDOMSalvage/RawOnly`, `TestPipeline_ExtractionFallback_*` |
| Embed 1536 → pgvector | `TestOpenAIEmbed_*`, `vectortest.RunContract` (Upsert), `TestPipeline_HappyPath` |
| Similar via vector search | `vectortest.RunContract` (QueryRanksByCosine), `TestE2E_SimilarAcrossTwoReadings` |
| Summarize once (forced emit_reading) | `TestAnthropic_ForcedTool*`, `TestPipeline_HappyPath` (one call) |
| Notification email (Resend) | `TestPipeline_HappyPath` (one notify), `TestPipeline_NotifyFailureDoesNotFailReading` |
| List/search (q + tags)/sort/paginate | `storetest.RunContract` (SearchFTS/TagFilterAND/KeysetPagination), `TestListReadings_QTagsSortPaginate` |
| View summary/similar/diagnostics | `TestGetReading_*`, `TestE2E_IngestProcessRead` |
| Open extracted/raw (auth-gated) | `TestGetContent/Raw_AuthGatedAndStreamsBlob` |
| Reprocess on demand | `TestReprocess_*` |
| YouTube oEmbed floor + transcript | `TestYouTube_OEmbed`, `TestPipeline_YouTube_OEmbedFloor` |
| Reddit → failed w/ guidance | `TestReddit_Guidance`, `TestPipeline_Reddit_FailsWithGuidance` |
| Single bearer token auth | `TestAuth_*` |
| Stale annotation at read (no write) | `TestGetReading_AnnotatesStaleAtReadTime`, `TestAnnotateStale` |
| Retry-exhaustion → retryable failed | `TestDispatch_RetryExhaustionFailsRetryable`, `TestE2E_RetryExhaustionFailsRetryable` |
| Rate-limit → delayed re-dispatch (no attempt consumed) | `TestDispatch_RequeueDoesNotConsumeAttempt`, `TestE2E_RateLimitRequeue` |
| Restart recovery (startup sweep) | `TestDispatch_RecoverySweepReenqueuesNonTerminal`, `TestE2E_RecoveryAfterRestart` |
| Operator CLIs | `TestCmd*` (audit/recover/drop/import/smoke/staging) |
| Store/Vector parity (fake ↔ Postgres) | `storetest.RunContract`, `vectortest.RunContract` (`-tags integration`) |
| Bounded memory (no full-corpus load) | indexed `Search`/keyset pagination in `store.Postgres` (conformance + integration) |

---

## 17. Milestones / suggested ordering

1. **M1 — Core (Phases 0–3)**: harness, domain, `Store` conformance suite + `store.Memory`,
   dispatcher. Deliverable: in-process async plumbing + recovery proven on `store.Memory`, zero
   external deps. *Highest-risk logic landed first.*
2. **M2 — Pipeline (Phases 4–5)**: ports + fakes + full process orchestration. Deliverable:
   every Process branch green against fakes.
3. **M3 — Reality (Phases 6–7)**: HTTP adapters + extraction fixtures + the Postgres `Store`/
   `VectorIndex` adapters passing their conformance suites under `-tags integration`.
4. **M4 — API (Phases 8–9)**: HTTP surface + end-to-end. Deliverable: the app works through
   its real interface with fake externals.
5. **M5 — Ops & hardening (Phases 10–11)**: CLIs, config, security, lifecycle. Deliverable:
   deployable.
6. **M6 — Optional (Phase 12)**: alternative backends / multi-instance workers.

Each milestone ends with `go test -race ./...` green, lint clean, and the relevant rows of the
§16 traceability table checked off.

---

## 18. Definition of Done (whole backend)

- All §16 capabilities have ≥1 passing named test.
- `go build ./... && go vet ./... && golangci-lint run` clean.
- `go test -race -cover ./...` green; domain/dispatch/pipeline + `*.Memory` fakes ≥90%, HTTP/adapters ≥75%.
- `go test -tags integration ./...` green: `store.Postgres` + `vector.Postgres` pass the same conformance suites (testcontainers).
- No wall-clock, RNG, network, or Docker in the default test run (Postgres/R2 tests build-tagged).
- `cmd/reader-api` runs migrations, boots from env, serves the API, runs the worker, shuts down gracefully.
- Secrets never logged; ingest fetch is SSRF- and size-guarded; destructive CLIs gated.
```
