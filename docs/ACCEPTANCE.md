# reading-lite — Manual Verification Plan (Phases 0–4)

> Purpose: a step-by-step plan a human can follow to independently verify that the
> work completed so far is correct, complete, and consistent with both
> `docs/PLAN.md` (the implementation contract) and `CLAUDE.md` (the
> project conventions). Every step lists **what to do**, **what to expect**, and
> **why it matters**. A consolidated checklist is at the end.
>
> **Automated companion:** most of this plan is now executable as a blackbox
> harness in `internal/acceptance/` (build tag `verify`), run with **`make verify`**.
> It automates Section A (build/vet/gofmt/lint), Section B6 (sqlc drift), the
> Section C behavioral checks, and the Section D conventions audit. The store
> contract and the reading-metadata lifecycle run against **both** backends — the
> in-memory fake and real Postgres via testcontainers — so the harness proves
> fake↔Postgres parity itself (the Postgres backend skips when Docker is
> unavailable, or honors `DATABASE_URL`). The Phase 3 dispatcher lifecycle and its
> error classifier (C9) are automated too — driven inline through the public
> `dispatch.Dispatcher` surface with a fake clock and a fake delayer, so they need
> no goroutines, timers, or Docker. The Phase 4 external-service ports & fakes (C10)
> are automated as well: compile-time port conformance, the VectorIndex contract
> against `vector.Memory` (`TestAcceptance_VectorIndexContract`), and a port-fidelity
> check that each scriptable fake returns scripted results, errors on demand, and
> records its calls (`TestPorts_FakesAreScriptableAndRecordCalls`). Tool- and
> Docker-dependent steps skip when unavailable. What stays manual: the coverage
> judgment call in B3/B4 and reviewing the placeholder binaries. `make test-integration`
> remains a separate, dedicated path for the store↔Postgres integration suite. Each
> automated test names the plan section it covers.

## 0. Scope

### In scope (what exists today)

The checkout has completed **Phase 0** (tooling), **Phase 1** (pure domain core),
**Phase 2** (readings metadata store), **Phase 3** (in-process dispatcher), and
**Phase 4** (external-service ports & in-memory fakes):

| Area | Files | Phase |
|---|---|---|
| Module, Makefile, CI, lint config | `go.mod`, `Makefile`, `.github/workflows/ci.yml`, `.golangci.yml` | 0 |
| Placeholder binaries | `cmd/reader-api/main.go`, `cmd/readerctl/main.go` | 0 |
| Clock port + system + fake | `internal/clock/clock.go` | 0 |
| Status machine, terminal checks | `internal/reading/status.go` | 1 |
| URL idempotency key + source classification | `internal/reading/url.go` | 1 |
| Reading struct + stale annotation | `internal/reading/reading.go` | 1 |
| Store port + DTOs | `internal/store/store.go` | 2 |
| In-memory store | `internal/store/memory.go` | 2 |
| Postgres adapter | `internal/store/postgres.go` | 2 |
| Embedded migration + runner | `internal/store/migrations/0001_init.sql`, `internal/store/migrate.go` | 2 |
| sqlc source + generated code | `internal/store/query.sql`, `sqlc.yaml`, `internal/store/storedb/*` | 2 |
| Shared conformance suite | `internal/store/storetest/contract.go` | 2 |
| Retry decision + error classifier (pure) | `internal/dispatch/dispatch.go` | 3 |
| Injectable delay seam (real + fake) | `internal/dispatch/delayer.go` | 3 |
| Worker pool, claim guard, recovery sweep | `internal/dispatch/dispatcher.go` | 3 |
| Fetcher / Extractor / Embedder ports + fakes | `internal/fetch/fetch.go`, `internal/extract/extract.go`, `internal/embed/embed.go` | 4 |
| Summarizer / Notifier ports + fakes | `internal/summarize/summarize.go`, `internal/notify/notify.go` | 4 |
| Blobs port + in-memory backend | `internal/blobs/blobs.go` | 4 |
| VectorIndex port (`Index`) + cosine `Memory` + conformance suite | `internal/vector/vector.go`, `internal/vector/vectortest/contract.go` | 4 |

### Out of scope (do not expect these to work yet)

The Phase 3 dispatcher lives in `internal/dispatch/` and is fully verified — by its own
race-tested package tests and now by the C9 blackbox checks in this harness — but it is
**not yet wired into the binaries**: nothing calls `Submit`/`Run`/`Sweep` from `main` yet,
because its `Handler` is the Phase 5 pipeline, which does not exist. The Phase 4 ports
(`fetch`/`extract`/`embed`/`vector`/`summarize`/`notify`/`blobs`) now exist with in-memory
fakes (C10), but only the fakes — every `Fake`/`Memory` backend is in scope; **no real
adapter** (HTTP fetch, OpenAI, Anthropic, Resend, R2, pgvector) is implemented yet, and the
ports are not yet wired into a pipeline. The remaining Phases 5–12 are **not** implemented:
the pipeline, the real HTTP/SDK adapters (Phase 6), extraction internals (Phase 7), the HTTP
API, the operator CLI subcommands, config loading, and observability. `reader-api` and
`readerctl` are intentionally empty `main(){}` placeholders. Verifying "the service
runs and ingests a URL" is **premature** and not part of this plan.

---

## 1. Environment prerequisites

Confirm the toolchain before anything else. The Go toolchain
(`/usr/local/go/bin`) and the Go-installed tools (`$HOME/go/bin`) are on `PATH`
via `~/.bashrc`, so a normal interactive shell resolves `go`, `gofmt`,
`golangci-lint`, and `sqlc` directly. If you are in a minimal shell that does not
source `~/.bashrc` (some CI runners, `sh -c`, cron), export them first:

```sh
export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"
```

| Tool | Required version | Verify | Expected |
|---|---|---|---|
| Go | 1.26.x | `go version` | `go version go1.26.4 linux/amd64` (or newer 1.26) |
| golangci-lint | v2.x | `golangci-lint version` | reports v2.12.x |
| sqlc | v1.31.x | `sqlc version` | `v1.31.1` |
| Docker (integration only) | any | `docker info` | Daemon runs v29.6.0; the user is in the `docker` group. If a session predates that membership, `docker info` returns "permission denied" — start a fresh login (so `id -nG` lists `docker`) or prefix commands with `sg docker -c '…'`. `DATABASE_URL` remains an alternative that bypasses Docker entirely. |

> Why it matters: the rest of the plan assumes these are runnable. If `go` resolves
> to nothing, you are not testing what you think you are.

---

## 2. Section A — Build & static analysis

Run from the repo root. These mirror the CI `test` and `lint` jobs.

| # | Step | Command | Expected result |
|---|---|---|---|
| A1 | Compile everything | `make build` (or `go build ./...`) | exits 0, no output |
| A2 | Vet | `go vet ./...` | exits 0, no findings |
| A3 | Format check | `gofmt -l .` | **prints nothing** (any filename printed = unformatted) |
| A4 | Lint | `make lint` (or `golangci-lint run ./...`) | `0 issues.` |
| A5 | Vet integration build tag | `go vet -tags integration ./internal/store/` | exits 0 (proves the integration test file compiles even though it won't run) |

**Known-good baseline (captured while writing this plan):** A1–A5 all pass; lint
reports `0 issues.`

> Why it matters: a green build + clean lint is the floor. `gofmt -l` and the lint
> job are *required* CI gates (`CLAUDE.md` → "Lint: gofmt -l clean, go vet, golangci-lint").

---

## 3. Section B — Automated test verification

The completed phases are TDD'd, so the tests *are* the primary specification. Run
them and read them.

### B1. Fast suite (fakes only, deterministic)

```sh
make test            # go test ./...
```
Expect every package `ok`. There must be **no** network, Docker, or wall-clock
dependence — confirm by running with the network down; it should not matter.

### B2. Race detector

```sh
make test-race       # go test -race ./...
```
Expect `ok` with no `DATA RACE` reports. This specifically exercises
`clock.Fake` concurrent use, `store.Memory` `ConcurrentSaves`, and the Phase 3
dispatcher's real worker pool — `TestDispatch_ConcurrencyBounded` (bounded parallel
handlers), `TestDispatch_GracefulDrain`/`DrainStopsPullingQueuedWork` (ctx-driven
drain), and `TestDispatch_DuplicateIdNotProcessedConcurrently` (the claim guard).

### B3. Coverage

```sh
make cover           # go test -race -cover ./...
```

**Known-good baseline (recaptured for the Phases 0–3 pass):**

| Package | Coverage | Note |
|---|---|---|
| `internal/clock` | 90.9% | meets the ≥90% domain bar |
| `internal/reading` | 97.4% | pure domain; clears the ≥90% bar |
| `internal/dispatch` | 93.0% | Phase 3 domain logic (`decide`/`Classify`/`backoff` + the dispatcher seam); clears the ≥90% bar (TDD plan §2 lists `dispatch` as a domain package) |
| `internal/store` | 46.7% | **expected to look low**: the Postgres adapter's statements are only executed under `-tags integration`, which is excluded from the default run. The `store.Memory` paths are well covered via the conformance suite. |
| `internal/store/storedb` | 0.0% | generated sqlc code, exercised only under integration |
| `internal/store/storetest` | 0.0% | the suite itself; counts as test code |

> Verification judgment call: the three domain packages (`clock`, `reading`,
> `dispatch`) all clear the plan's 90% bar (§2 of the TDD plan). The store's 46.7%
> is a measurement artifact of the build-tag split, **not** a real gap. If any
> domain package later dips below 90%, treat it as a **finding to confirm, not a
> hard failure** — inspect the uncovered lines (`B4`) and decide whether they are
> meaningful branches or unreachable defensive code.

### B4. Per-line coverage inspection (optional)

```sh
go test -coverprofile=/tmp/cover.out ./internal/reading/... ./internal/dispatch/...
go tool cover -func=/tmp/cover.out | sort -k3 -n | head
go tool cover -html=/tmp/cover.out -o /tmp/cover.html   # open in a browser
```
Expect the uncovered lines to be defensive error returns (e.g. `url.Parse` failure
paths, IPv6 edge branches, the dispatcher's best-effort final-write error paths that
fire only when the store rejects a terminal write), not core logic. Flag anything else.

### B5. Integration suite (Store ↔ Postgres parity)

```sh
make test-integration                       # go test -tags integration ./...
# in a session that predates docker-group membership, activate the group:
sg docker -c 'make test-integration'
# or, against an existing DB (bypasses Docker):
DATABASE_URL='postgres://…?sslmode=disable' make test-integration
```
Expected behavior:
- With a working Docker daemon **or** a reachable `DATABASE_URL`: the
  `TestPostgresStoreContract`, `TestPostgresMigrationsAreIdempotent`, and
  `TestPostgresDeleteCascadesVector` tests run and pass — proving `store.Postgres`
  satisfies the **same** `storetest.RunContract` as `store.Memory`.
- If `docker.sock` returns "permission denied" (a session that predates `docker`
  group membership) and `DATABASE_URL` is unset, testcontainers calls
  `SkipIfProviderIsNotHealthy` and the Postgres tests **skip** rather than fail. A
  *skip* leaves parity unproven — re-login or use `sg docker -c '…'`.

The blackbox harness (`make verify`) also runs the store contract and the
reading-metadata lifecycle against a testcontainers Postgres backend, so parity is
exercised there too — `sg docker -c 'make verify'` proves it in ~4.3s. This B5 step
remains the dedicated, standalone integration path.

**Known-good baseline (captured while writing this plan):** ran via
`sg docker -c 'go test -tags integration -count=1 ./internal/store/'` against a
testcontainers `pgvector/pgvector` Postgres. All three tests **passed** in ~7.3s —
the full `TestPostgresStoreContract` conformance suite (every subtest:
RoundTrip, URLKeyIdempotency, SearchFTS, TagFilterAND, StatusFilterAndSortModes,
KeysetPagination, SortTitlePagination, RankedSearchPagination, UpdateStatus…,
ReplaceTags, ListNonTerminal, Delete, ConcurrentSaves, DefensiveCopies),
`TestPostgresMigrationsAreIdempotent`, and `TestPostgresDeleteCascadesVector`.
Fake↔Postgres parity is therefore **proven locally**.

> Why it matters: the entire store design rests on "the fake and the real DB behave
> identically." That claim is only verified when B5 actually executes (not skips) —
> which it now does. Use `-count=1` to defeat the test cache when you need a fresh
> proof rather than a cached `ok`.

### B6. sqlc reproducibility (no generated drift)

```sh
make sqlc            # sqlc generate
git status --porcelain
```
Expect **no** changes — the committed `internal/store/storedb/*.go` must match what
`query.sql` + the migration regenerate. **Confirmed clean in this checkout.**

> Why it matters: checked-in generated code that drifts from its source is a silent
> correctness hazard; CI doesn't currently regenerate, so this is a manual gate.

---

## 4. Section C — Component-level manual verification

For each completed component: run its tests verbosely, read the implementation
against the TDD plan, and (where useful) poke it with a throwaway program.

### C1. Phase 0 — tooling & CI

- [ ] Read `Makefile`: confirm targets `test`, `test-integration`, `test-race`,
  `lint`, `cover`, `sqlc`, `migrate`, `build`, `run` exist and match `CLAUDE.md`.
- [ ] Read `.github/workflows/ci.yml`: three jobs — `test` (build/vet/race+cover),
  `integration` (pgvector service + `DATABASE_URL`), `lint` (gofmt + golangci-lint).
  Confirm Go `1.26.x` and that integration is a separate job (not in the default run).
- [ ] Confirm `go.mod` declares `go 1.26` and module path
  `github.com/bbell/reading-lite`.
- [ ] Placeholder binaries: `go run ./cmd/reader-api` and `go run ./cmd/readerctl`
  should build and exit 0 immediately (empty `main`). They must **not** be expected
  to do anything else yet.

### C2. Phase 0 — clock port (`internal/clock/clock.go`)

- [ ] `go test -v ./internal/clock/` → `TestFakeClock_AdvanceMovesNow`,
  `TestFakeClock_SetMovesNow`, `TestFakeClock_ConcurrentUse` pass.
- [ ] Read the code: `Clock` is `interface{ Now() time.Time }`; `System` uses
  `time.Now()`; `Fake.Now/Advance/Set` are all mutex-guarded (workers read
  concurrently). This matches the TDD plan §2 deliverable test exactly.
- [ ] Run `go test -race ./internal/clock/` to confirm the mutex actually protects
  concurrent access.

### C3. Phase 1 — status machine (`internal/reading/status.go`)

- [ ] `go test -v -run TestStatus ./internal/reading/` passes.
- [ ] Read `allowedTransitions` and confirm it is an **explicit allow-table** (no
  "any→any"), matching the TDD plan §3.1 table:
  - `pending→running` ✔, `running→ready` ✔, `running→failed` ✔,
    `running→pending` (rate-limit requeue) ✔, `failed→pending` ✔, `ready→pending` ✔.
  - Disallowed: `ready→running` ✘, `pending→ready` ✘, `failed→failed` ✘ (terminal
    self-loop rejected).
- [ ] `IsTerminal()` returns true only for `ready` and `failed`.

### C4. Phase 1 — URL key & source classification (`internal/reading/url.go`)

- [ ] `go test -v -run 'TestURLKey|TestClassifySource' ./internal/reading/` passes
  (note the suite is broader than the plan's table: it adds escaped-path,
  duplicate-slash, and trailing-slash cases).
- [ ] Interactive spot-check — drop this scratch program somewhere outside the module
  or use `go test` scaffolding, and confirm the normalization rules by eye:

  ```go
  // verify the canonical-key contract from the TDD plan §3.2 table
  for _, in := range []string{
      "HTTP://Example.COM/Path",
      "https://example.com/a?utm_source=x&id=7",
      "https://example.com/a/",
      "https://example.com/a#frag",
      "https://example.com",
      "https://m.youtube.com/watch?v=ID&t=10",
      "https://youtu.be/ID",
      "not a url", "ftp://x", "javascript:alert(1)",
  } {
      k, err := reading.URLKey(in)
      fmt.Printf("%-45s -> %q  err=%v\n", in, k, err)
  }
  ```

  Confirm: scheme+host lowercased; `utm_*`/`fbclid`/`gclid`/`ref` stripped; trailing
  slash on non-root normalized; fragment dropped; root path → `/`; YouTube hosts
  canonicalized to `www.youtube.com/watch?v=…` with `t` stripped; `youtu.be`
  expanded; non-http(s) and unparseable inputs → `ErrInvalidURL`.
- [ ] `ClassifySource` returns `youtube` / `reddit` / `markdown` / `web` for the
  matching hosts/extensions.
- [ ] **Cross-check against the contract:** `storetest.sampleReading` derives both
  `URLKey` and `ClassifySource` from the raw URL, so the store tests transitively
  exercise these too.

### C5. Phase 1 — stale annotation (`internal/reading/reading.go`)

- [ ] `go test -v -run TestAnnotateStale ./internal/reading/` passes.
- [ ] Read `AnnotateStale`: it must operate on a **copy** and never mutate the input
  (the test asserts no mutation). Confirm:
  - `pending` older than `TTLs.Pending` → reported `failed`, reason mentions
    "timed out before processing".
  - `running` started before `now - TTLs.Running` → reported `failed`, reason
    mentions "stalled".
  - `ready`/`failed` never annotated; fresh rows pass through unchanged.
  - A zero TTL disables that check (guarded by `> 0`).
- [ ] Confirm it reads `CreatedAt` for pending and `StartedAt` for running, and that
  a `nil StartedAt` running row is left alone.

### C6. Phase 2 — Store port & Memory fake (`store.go`, `memory.go`)

- [ ] `go test -v -run TestMemoryStoreContract ./internal/store/` → all conformance
  subtests pass.
- [ ] Read `Store` interface in `store.go`: the eight methods match the TDD plan §4.4
  (`SaveReading`, `GetByID`, `GetByURLKey`, `UpdateStatus`, `ReplaceTags`, `Search`,
  `ListNonTerminal`, `Delete`) plus the `Query`/`Page`/`Pending`/`StatusFields`/
  `Cursor` DTOs and the `ErrNotFound`/`ErrConflict` sentinels.
- [ ] Read `memory.go` and confirm the behaviors the plan calls out:
  - Idempotency: duplicate `id` **or** duplicate `url_key` → `ErrConflict`.
  - `Search` returns **defensive copies** (`cloneReading`) — mutating a returned
    slice/pointer must not corrupt stored state (`DefensiveCopies` test).
  - Keyset pagination is implemented in Go with no offset scan; cursors carry
    `(rank, created_at|title, id)`.
  - `ListNonTerminal` returns `pending` + `running` started before the cutoff only.
  - `ctx.Err()` is checked at the top of every method (cancellation respected).
- [ ] Confirm `store.Memory` imports nothing outside stdlib + `internal/reading`
  (`CLAUDE.md`: "keep store.Memory dependency-free").

### C7. Phase 2 — conformance suite (`storetest/contract.go`)

- [ ] Read `RunContract` and map each subtest to the TDD plan §4.3 list:
  `RoundTrip`, `URLKeyIdempotency`, `SearchFTS` (incl. adversarial query
  `'AND OR " 🧪` that must not error), `TagFilterAND`, `StatusFilterAndSortModes`,
  `KeysetPagination` (25 rows, no dup/skip, correct total), `UpdateStatus…`,
  `ReplaceTags`, `ListNonTerminal`, `Delete`, `ConcurrentSaves`. Confirm the extra
  hardening cases too (`SortTitlePagination`, `RankedSearchPagination`,
  `DefensiveCopies`, `SaveReadingAcceptsNilTags`, `UpdateStatusErrorSemantics`).
- [ ] Confirm the suite is **backend-neutral**: it takes a `Factory` and is the single
  source invoked by both `memory_test.go` and `postgres_test.go`. This is the
  mechanism that makes fake↔Postgres divergence impossible to miss — verify nothing
  in the suite reaches into a concrete implementation.

### C8. Phase 2 — Postgres adapter, migration, sqlc

- [ ] Read `migrations/0001_init.sql` against TDD plan §4.1: `readings` table with
  `url_key` unique, generated `tsvector` `search` column, `tags text[]`,
  `reading_vectors(reading_id … on delete cascade, embedding vector(1536))`, and the
  four indexes (`search` GIN, `tags` GIN, `(created_at desc, id desc)` page index,
  `status`) + the HNSW ANN index. Note the deliberate `immutable_array_to_string`
  wrapper (with its explanatory comment) — `array_to_string` is only STABLE and
  cannot be used directly in a generated column.
- [ ] Read `query.sql` and confirm: idempotent insert uses
  `on conflict (url_key) do nothing returning id`; search uses
  `websearch_to_tsquery('english', …)` (safe arbitrary input) ranked by `ts_rank`;
  keyset cursor includes **rank first** then the secondary key so ranked pages can't
  skip/dup; sweep query selects `pending` or stale `running`.
- [ ] Read `postgres.go` and confirm the error mapping: `pgx.ErrNoRows` or unique
  violation (`23505`) on insert → `ErrConflict`; `ErrNoRows` on get → `ErrNotFound`;
  zero affected rows on update/delete → `ErrNotFound`.
- [ ] Confirm `migrate.go` uses an advisory lock + `schema_migrations` table so
  repeated/parallel runs are safe (the `TestPostgresMigrationsAreIdempotent`
  integration test proves this — see B5).
- [ ] **Parity proof (confirmed via B5):** `TestPostgresStoreContract` passes the
  identical `RunContract` and `TestPostgresDeleteCascadesVector` proves the FK
  cascade removes the vector row — all green against a real testcontainers Postgres
  (see B5 baseline). Re-run with `-count=1` if you want fresh (uncached) proof.

### C9. Phase 3 — in-process dispatcher (`internal/dispatch/`)

The dispatcher's whole point is that its semantics are testable without real time or
goroutines: the decision logic is pure, and every delay flows through an injectable
seam. Read it against TDD plan §5 and confirm each spec bullet maps to a named test.

- [ ] `go test -v ./internal/dispatch/` passes; `go test -race ./internal/dispatch/`
  is clean (the worker pool and `FakeDelayer` are exercised concurrently — see B2).
- [ ] **Pure decision core (`dispatch.go`, §5.1–5.2).** Read `decide` and confirm it
  is the single branch point and matches the table:
  - `Done` → no redispatch, not failed.
  - `Retry` → redispatch with `Delay = backoff(attempt)` and `NextAttempt = attempt+1`,
    **unless** `attempt+1 >= Max`, in which case `MarkFailed` (no redispatch) — the
    reading becomes a *retryable* `failed`, and the in-memory item is dropped.
  - `Requeue` → redispatch with `NextAttempt = attempt` (**unchanged** — a rate limit
    does not consume an attempt) and `Delay = After`.
  - `Fail` → `MarkFailed` regardless of attempt.
  - `backoff` is the `1s,2s,4s,8s,16s,…` schedule capped at 30s.
  - Tests: `TestDecide_Done`, `TestDecide_RetryBackoff`, `TestDecide_RequeueKeepsAttempt`,
    `TestDecide_RetryExhaustion`, `TestDecide_PermanentFailsFast`, `TestBackoff_Schedule`.
  - These are the project's only sanctioned white-box tests (`decide_test.go` is
    `package dispatch`); the conventions audit (D3) allow-lists exactly that file.
- [ ] **Error classifier (`Classify`, §5.1).** `nil → Done`; `*RateLimitError → Requeue`
  (carrying `RetryAfter` as `After`); `errors.Is(err, ErrPermanent) → Fail`; anything
  else → `Retry`. Both wrapped and direct errors classify alike, so the pipeline (Phase 5)
  and the dispatcher will agree. Tests: `TestClassify_*`, `TestRateLimitError_ErrorAndUnwrap`.
- [ ] **Worker pool & lifecycle persistence (`dispatcher.go`, §5.3).** Confirm:
  - `Submit` enqueues at attempt 0 and is non-blocking — a duplicate (already claimed)
    or a full buffer is **dropped**, not blocked; the still-`pending` reading is the
    recovery sweep's job (PLAN §1.4).
  - `handle` is the only method that touches `Store` and `Delay`: it marks the reading
    `running` (mirroring `attempt → process_attempts`, clearing the error), runs
    `Handler`, applies `decide`, then persists `ready` / `pending`+scheduled-redispatch /
    `failed`. The persisted failure reason is always non-empty and distinguishes a spent
    budget from a permanent error.
  - The dedup **claim** is held from enqueue through *every* retry/requeue until a
    terminal outcome, so a second `Submit` or a sweep re-enqueue cannot double-run a
    reading and clobber the winner's status (matches the single-instance topology).
  - Tests: `TestDispatch_SubmitRunsHandlerOnce`, `TestDispatch_RetrySchedulesDelayedRedispatch`,
    `TestDispatch_RequeueDoesNotConsumeAttempt`, `TestDispatch_RetryExhaustionFailsRetryable`
    (incl. reprocess-after-failure), `TestDispatch_DefaultMaxAttempts`,
    `TestDispatch_ExhaustionMessageIncludesCause`, `TestDispatch_PermanentFailRecordsError`,
    `TestDispatch_GracefulDrain`, `TestDispatch_DrainStopsPullingQueuedWork`,
    `TestDispatch_ConcurrencyBounded`, `TestDispatch_DuplicateIdNotProcessedConcurrently`,
    `TestDispatch_BufferedDuplicateRunsOnce`.
- [ ] **Delay seam (`delayer.go`).** `RealDelayer` uses `time.AfterFunc`; `FakeDelayer`
  records scheduled delays and fires them on demand (`Durations`/`PendingLen`/`Total`/
  `FireAll`), so retry/backoff/requeue timing is asserted with **no** sleeps. The
  `Inline` flag runs the initial handle synchronously (the HTTP/test seam) while
  re-dispatch still flows through `Delay`. Tests: `TestRealDelayer_FiresCallback` plus
  every redispatch assertion above.
- [ ] **Recovery sweep (`Sweep`, §5.4).** Reads `Store.ListNonTerminal(cutoff)` — a
  single indexed query returning `pending` + `running` started before
  `now − RunningTTL` — and re-dispatches each at its **stored** `process_attempts`, so
  `Max` is honored across restarts. Terminal readings are left alone. Recovery is
  non-lossy (blocks until a worker accepts each id, or ctx is cancelled) so a backlog
  larger than `Buffer` is not silently dropped. Tests:
  `TestDispatch_RecoverySweepReenqueuesNonTerminal`, `TestDispatch_SweepResumesAtStoredAttempt`,
  `TestDispatch_SweepRecoversBacklogWithoutDropping`, `TestDispatch_SweepStopsOnCanceledContext`,
  `TestDispatch_SweepPropagatesListError`.
- [ ] **Automated (B/§make verify):** `TestAcceptance_DispatcherLifecycle` and
  `TestAcceptance_DispatcherClassifiesErrors` re-prove the spec bullets that matter most —
  submit→ready, rate-limit requeue (attempt preserved), retry-exhaustion→retryable
  `failed`, recovery-sweep selection, and sweep-resumes-at-stored-attempt — through the
  **public** `dispatch.Dispatcher` surface against `store.Memory` + `clock.Fake` +
  `FakeDelayer`. They are part of `make verify`.

> Note: the dispatcher's `Store` is a **narrow** port (`UpdateStatus` + `ListNonTerminal`)
> distinct from the full `store.Store`; `store.Memory` satisfies both. The end-to-end
> stories (`TestE2E_RetryExhaustionFailsRetryable`, `TestE2E_RateLimitRequeue`,
> `TestE2E_RecoveryAfterRestart` in PLAN §16) belong to Phase 9 and are **not** in scope
> here — they need the pipeline + HTTP surface. C9 proves the dispatcher in isolation.

### C10. Phase 4 — external-service ports & fakes (`internal/{fetch,extract,embed,summarize,notify,blobs,vector}`)

Phase 4 defines every external port as a small interface and provides a faithful
in-memory fake for each, so the Phase 5 pipeline can be built and tested with zero
I/O. Read each package against TDD plan §6 and confirm the interface shape, the fake
fidelity, and (for the two backends that carry real behavior) the conformance.

- [ ] `go test -race ./internal/fetch/ ./internal/extract/ ./internal/embed/
  ./internal/summarize/ ./internal/notify/ ./internal/blobs/ ./internal/vector/...`
  passes clean (each package has a 20-goroutine concurrency test).
- [ ] **Port shapes match §6.** Every method takes `context.Context`. Confirm:
  `fetch.Fetcher.Get → Resource{Body,ContentType,FinalURL,Status}`;
  `extract.Extractor.Extract(fetch.Resource) → Article{Title,Author,Site,Lang,Markdown,Mode,WordCount}`
  with `Mode ∈ {readability, raw_dom, raw_only}`; `embed.Embedder.Embed → []float32` of
  `embed.Dim == 1536`; `summarize.Summarizer.Summarize(SummaryInput) → Summary{Title,Summary,Tags,JSON}`;
  `notify.Notifier.Notify(Email{From,To,Subject,HTML})`; `blobs.Blobs` Put/Get/Delete;
  `vector.Index` Upsert/Query/Delete with `Match{ID,Score}`.
- [ ] **Fakes are faithful doubles.** Each scriptable fake (`fetch`/`extract`/`embed`/
  `summarize`/`notify`) returns a scripted result, can be scripted to error, records its
  call count + inputs behind a mutex, and returns defensive copies (mutating a returned
  slice must not corrupt the script — every package has a "StoresCopies"/aliasing test).
  `notify.Fake` distinguishes attempted `Calls()` from successfully `Sent()` emails (a
  failed send is not recorded as sent), matching the "notify failure never fails a reading"
  policy. Every method honors `ctx.Err()` first.
- [ ] **`blobs.Memory` round-trips** Put→Get with content type, returns `ErrNotFound` on a
  missing key, overwrites on re-Put, and treats Delete of an absent key as a no-op
  (S3 semantics). Stored bytes are cloned in and out.
- [ ] **`vector.Memory` is a real cosine index.** Read `cosine` (dot / product-of-magnitudes
  with a zero-magnitude guard) and confirm `Query` ranks by descending score with a
  deterministic id tie-break, excludes `excludeID` (the zero value `""` excludes nothing),
  bounds `topK` (0 → empty, negative → empty, `> count` → all), and rejects non-`Dim`
  vectors with `ErrDimension` on both `Upsert` and `Query`.
- [ ] **Shared conformance suite.** `vectortest.RunContract` holds `QueryRanksByCosine`,
  `ExcludesSelf`, `DeleteRemoves` (the §6 cases) plus `UpsertReplaces`, `TopKBounds`,
  `EmptyIndexReturnsNoMatches`, and `RejectsWrongDimension`. It takes a `Factory`, so the
  Phase 6 pgvector adapter will reuse it verbatim under `-tags integration` — the same
  fake↔Postgres parity mechanism `storetest` uses.
- [ ] **No real SDK imports yet (§6 "done when").** `grep -rn "net/http\|openai\|anthropic\|
  resend\|aws-sdk\|pgx" internal/fetch internal/extract internal/embed internal/summarize
  internal/notify internal/blobs internal/vector` prints **nothing** — the real adapters are
  Phase 6.
- [ ] **Naming note / deviation from PLAN.md:** the VectorIndex port interface is named
  `vector.Index`, not `vector.VectorIndex` as written in §6, because `revive`'s exported
  rule flags `vector.VectorIndex` as a type-name stutter and the lint gate (A4) must stay
  clean. The doc comment on `Index` preserves the "VectorIndex port" traceability. This is
  the only Phase 4 deviation from the plan's literal names.
- [ ] **Automated (B/§make verify):** compile-time `var _ Port = (*Fake)(nil)` assertions
  for all seven ports, `TestAcceptance_VectorIndexContract` (the suite against `vector.Memory`),
  and `TestPorts_FakesAreScriptableAndRecordCalls` (port-fidelity through the public surface)
  are part of `make verify`.

> Note: only the fakes/in-memory backends are in scope here. The real adapters — `fetch.HTTP`,
> `embed.OpenAI`, `summarize.Anthropic`, `notify.Resend`, `blobs.R2`, `vector.Postgres` — and
> the integration arm of `vectortest.RunContract` belong to Phase 6 and are **not** in scope.
> Wiring these ports into a pipeline is Phase 5. Do not flag their absence as a gap.

---

## 5. Section D — Conventions & constraints audit (`CLAUDE.md`)

Spot-check the project rules that aren't covered by a passing test:

- [ ] **Determinism:** `grep -rn "time.Now\|math/rand\|net/http" internal/reading
  internal/store/memory.go internal/store/store.go` — the pure domain core and the
  memory fake should not reach for wall-clock/RNG/network. (Note: `memory.go` does
  fall back to `time.Now().UTC()` inside `UpdateStatus`/`ReplaceTags` when no clock
  time is supplied — confirm that's only a fallback and tests always inject `Now`.)
- [ ] **`internal/reading` is stdlib-only:** `go list -deps ./internal/reading` shows
  no third-party imports.
- [ ] **Pure retry logic + injected seams (`dispatch`):** confirm `decide`/`Classify`/
  `backoff` are pure functions and that the dispatcher's only impure dependencies are
  the injected `clock.Clock`, `Delayer`, and `Store` — no `time.Sleep`, no bare
  `time.Now()`, no goroutine-timed waits in the semantics (`CLAUDE.md` → keep
  retry/backoff logic pure, run delays through the `Delayer` seam).
  `grep -rn "time.Sleep\|time.Now" internal/dispatch` should print **nothing** — the
  dispatcher never sleeps and reads time only through the injected `clock.Clock`. The
  one production timer is `time.AfterFunc`, confined to `RealDelayer.After` in
  `delayer.go` (the seam); `grep -rn "time.AfterFunc" internal/dispatch` shows it lives
  there and nowhere else.
- [ ] **Black-box test packages:** confirm `_test.go` files use `package *_test`
  (e.g. `reading_test`, `clock_test`, `store_test`, `storetest`, `dispatch_test`,
  `acceptance_test`). The **one** sanctioned white-box file is
  `internal/dispatch/decide_test.go` (`package dispatch`) — testing the unexported
  `decide`/`backoff` is the right boundary, and the harness's
  `TestConventions_TestPackagesAreBlackbox` allow-lists exactly that path.
- [ ] **Integration behind build tag:** only `postgres_test.go` carries
  `//go:build integration`; nothing else pulls Docker into the default run.
- [ ] **Fakes next to ports, concurrency-safe:** `clock.Fake`, `store.Memory`, and
  `dispatch.FakeDelayer` live in their port packages and pass `-race`.
- [ ] **Table-driven + `t.Parallel()`:** confirm subtests use `t.Run` and parallelism
  where there's no shared state (the contract suite and the `dispatch` tests do;
  `ConcurrentSaves` deliberately does not call `t.Parallel`).

---

## 6. Section E — Known limitations & things to explicitly NOT verify

Record these so a reviewer doesn't waste time or raise false bugs:

1. **Integration parity is proven locally** — the Postgres conformance suite ran
   green against testcontainers (see B5). In a session that predates `docker` group
   membership, run it via `sg docker -c '…'` or after a fresh login; `DATABASE_URL`
   bypasses Docker entirely.
2. **Domain coverage clears the 90% bar** — `clock` 90.9%, `reading` 97.4%,
   `dispatch` 93.0% (B3). The store's 46.7% is a build-tag artifact, not a gap. No
   domain coverage finding is currently open; B4 is the method to use if one reopens.
3. **Binaries are placeholders** — `reader-api`/`readerctl` do nothing. The Phase 3
   dispatcher (`internal/dispatch`) is complete and verified (C9) but is **not yet
   wired into them**: nothing calls `Submit`/`Run`/`Sweep` from `main`, because its
   `Handler` is the Phase 5 pipeline, which does not exist. The Phase 4 ports + fakes
   (C10) exist but are not yet wired into anything either. No pipeline, real HTTP/SDK
   adapters, HTTP API, config, or CLI subcommands exist yet (Phases 5–12).
4. **The dispatcher's dedup claim is in-process** — the `claim`/`release` map gives
   single-process exactly-once dispatch, matching the single-instance topology
   (PLAN §1.5). It is **not** a cross-instance guard; a multi-instance deployment
   would need a store-level claim (e.g. `SELECT … FOR UPDATE SKIP LOCKED`, PLAN §14).
   Do not flag the in-memory map as a distributed-correctness bug — it is by design.
5. **Dropped final writes are best-effort** — `handle` discards the error from its
   terminal `ready`/`failed`/`pending` write (no logger until a later phase). A
   dropped write leaves the reading non-terminal, which the recovery sweep plus
   read-time stale annotation recover. This is intentional, not a missing error check.
6. **Toolchain `PATH` is configured in `~/.bashrc`** — interactive shells resolve
   the tools directly; only minimal non-interactive shells need the manual export
   in §1.
7. **`store.Postgres.UpdateStatus` does a read-then-write** (GetByID then update)
   rather than a single statement — correct under the current single-instance design,
   but note it is not transactionally atomic across the two calls (revisit if/when
   multi-instance workers land, TDD plan §14).

---

## 7. Consolidated checklist (template)

**This is a blank template — leave the boxes unticked here.** Copy this block into
a review note (or a PR) and tick it there as you work through one verification pass;
record the outcome of a completed pass as a row in the §8 sign-off log rather than
by ticking boxes in this document. `[ ]` = todo, `[x]` = verified, `[~]` =
skipped/blocked (write why).

### Environment
- [ ] `command -v go gofmt golangci-lint sqlc` all resolve (PATH set in `~/.bashrc`)
- [ ] `go version` is 1.26.x
- [ ] `golangci-lint version` is v2.12.x
- [ ] `sqlc version` is v1.31.x
- [ ] Docker daemon reachable **or** `DATABASE_URL` set (else integration is `[~]`)

### A — Build & static analysis
- [ ] A1 `make build` clean
- [ ] A2 `go vet ./...` clean
- [ ] A3 `gofmt -l .` prints nothing
- [ ] A4 `make lint` → `0 issues.`
- [ ] A5 `go vet -tags integration ./internal/store/` clean

### B — Automated tests
- [ ] B1 `make test` all `ok`
- [ ] B2 `make test-race` no data races (incl. dispatcher worker pool + claim guard)
- [ ] B3 `make cover` — clock ≥90%, reading ~97.4%, dispatch ~93.0%, store ~46.7% (artifact, see note)
- [ ] B4 domain uncovered lines inspected and judged benign (if any finding reopens)
- [ ] B5 `make test-integration` actually **ran** (not skipped) and passed
- [ ] B6 `make sqlc` produces no `git` drift

### C — Components
- [ ] C1 Makefile / CI / go.mod / placeholder binaries reviewed
- [ ] C2 clock: tests pass, mutex-guarded, race-clean
- [ ] C3 status machine: explicit allow-table matches §3.1, `IsTerminal` correct
- [ ] C4 URLKey/ClassifySource: normalization table confirmed, invalid inputs error
- [ ] C5 AnnotateStale: copy-not-mutate, pending/running TTL reasons, zero-TTL guard
- [ ] C6 Store port + Memory: interface matches §4.4, defensive copies, ctx checks,
  stdlib-only
- [ ] C7 conformance suite: every §4.3 case present, backend-neutral, adversarial-q
  tolerated
- [ ] C8 Postgres/migration/sqlc: schema+indexes match §4.1, query semantics match
  §4.2, error mapping correct, migrations idempotent, FK cascade (parity proven, B5)
- [ ] C9 dispatcher: `decide`/`Classify`/`backoff` match §5.1–5.2; worker pool persists
  lifecycle via narrow `Store`; rate-limit keeps attempt; retry-exhaustion → retryable
  `failed` (reprocessable); claim held across retries; `Sweep` recovers non-terminal at
  stored attempt, non-lossy; `FakeDelayer` seam; harness lifecycle+classifier green
- [ ] C10 ports & fakes: interfaces match §6; fakes scriptable + record calls + defensive
  copies + ctx-checked, race-clean; `blobs.Memory` round-trip + `ErrNotFound`; `vector.Memory`
  cosine ranking/exclude/topK/dim via `vectortest.RunContract`; no real SDK imports;
  `vector.Index` rename noted; harness conformance + `TestPorts_*` green

### D — Conventions
- [ ] D1 no wall-clock/RNG/network in domain core + memory fake (fallback noted);
  `dispatch` retry logic pure + delays through the `Delayer` seam
- [ ] D2 `internal/reading` stdlib-only (`go list -deps`)
- [ ] D3 black-box `_test` packages (only `dispatch/decide_test.go` white-box, allow-listed)
- [ ] D4 integration behind `//go:build integration` only
- [ ] D5 fakes co-located with ports and race-safe (`clock.Fake`, `store.Memory`, `dispatch.FakeDelayer`)
- [ ] D6 table-driven subtests + `t.Parallel()` where safe

### E — Sign-off
- [ ] Limitations in §6 acknowledged
- [ ] Any deviation from `docs/PLAN.md` recorded with rationale
- [ ] C9 dispatcher behavior confirmed and harness `TestAcceptance_Dispatcher*` green
- [ ] Overall verdict: Phases 0–3 **accept / reject** (record in §8 sign-off log)

---

## 8. Sign-off log

A running, append-only record of formal acceptance sign-offs. Add one row per
milestone or phase that has been accepted; never edit or remove past rows — to
revise a verdict, append a new row that supersedes it. Each entry pins the exact
state that was verified (PR/commit + CI) so the sign-off is reproducible later.

| Date | Scope | Signed off by | State verified | Verdict |
|---|---|---|---|---|
| 2026-06-24 | Phases 0–2 — tooling, domain core (`reading`, `clock`), store (`Memory` + `Postgres`) | Brian Bell | PR #1 (`verification-harness`); CI build/integration/lint green; `make verify` green; `go test -race ./...` clean; `internal/reading` 97.4%; store contract proven against Memory + testcontainers Postgres | ✅ Accepted |

Notes:
- Scope is **Phases 0–2 only**; the Phase 3 dispatcher and Phases 4–12 (pipeline,
  external-service ports, HTTP API, CLI, hardening) are out of scope for this sign-off
  (§0). The Phase 3 dispatcher has since landed in `internal/dispatch` and is verified
  by its own package tests, pending its own acceptance sign-off.
- No deviations from `docs/PLAN.md` were found.
