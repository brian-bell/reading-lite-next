# reading-lite — Manual Verification Plan (Phases 0–2)

> Purpose: a step-by-step plan a human can follow to independently verify that the
> work completed so far is correct, complete, and consistent with both
> `docs/backend-tdd-plan.md` (the implementation contract) and `CLAUDE.md` (the
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
> unavailable, or honors `DATABASE_URL`). Tool- and Docker-dependent steps skip
> when unavailable. What stays manual: the coverage judgment call in B3/B4 and
> reviewing the placeholder binaries. `make test-integration` remains a separate,
> dedicated path for the store↔Postgres integration suite. Each automated test
> names the plan section it covers.

## 0. Scope

### In scope (what exists today)

The checkout has completed **Phase 0** (tooling), **Phase 1** (pure domain core), and
**Phase 2** (readings metadata store):

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

### Out of scope (do not expect these to work yet)

Phases 3–12 are **not** implemented: the dispatcher, pipeline, external-service ports
(`fetch`/`extract`/`embed`/`vector`/`summarize`/`notify`/`blobs`), the HTTP API, the
operator CLI subcommands, config loading, and observability. `reader-api` and
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
`clock.Fake` concurrent use and `store.Memory` `ConcurrentSaves`.

### B3. Coverage

```sh
make cover           # go test -race -cover ./...
```

**Known-good baseline (captured while writing this plan):**

| Package | Coverage | Note |
|---|---|---|
| `internal/clock` | 90.9% | meets the ≥90% domain bar |
| `internal/reading` | 88.6% | pure domain; just under the 90% target — see note below |
| `internal/store` | 46.7% | **expected to look low**: the Postgres adapter's statements are only executed under `-tags integration`, which is excluded from the default run. The `store.Memory` paths are well covered via the conformance suite. |
| `internal/store/storedb` | 0.0% | generated sqlc code, exercised only under integration |
| `internal/store/storetest` | 0.0% | the suite itself; counts as test code |

> Verification judgment call: `internal/reading` at 88.6% is *slightly* below the
> plan's 90% domain bar (§2 of the TDD plan). Treat this as a **finding to confirm,
> not a hard failure** — inspect which lines are uncovered (`B4`) and decide whether
> they are meaningful branches or unreachable defensive code. The store's 46.7% is a
> measurement artifact of the build-tag split, **not** a real gap.

### B4. Per-line coverage inspection (optional, for the 88.6% question)

```sh
go test -coverprofile=/tmp/cover.out ./internal/reading/...
go tool cover -func=/tmp/cover.out | sort -k3 -n | head
go tool cover -html=/tmp/cover.out -o /tmp/cover.html   # open in a browser
```
Expect the uncovered lines to be defensive error returns (e.g. `url.Parse` failure
paths, IPv6 edge branches), not core logic. Flag anything else.

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
- [ ] **Black-box test packages:** confirm `_test.go` files use `package *_test`
  (e.g. `reading_test`, `clock_test`, `store_test`, `storetest`).
- [ ] **Integration behind build tag:** only `postgres_test.go` carries
  `//go:build integration`; nothing else pulls Docker into the default run.
- [ ] **Fakes next to ports, concurrency-safe:** `clock.Fake` and `store.Memory` are
  in their port packages and pass `-race`.
- [ ] **Table-driven + `t.Parallel()`:** confirm subtests use `t.Run` and parallelism
  where there's no shared state (the contract suite does; `ConcurrentSaves`
  deliberately does not call `t.Parallel`).

---

## 6. Section E — Known limitations & things to explicitly NOT verify

Record these so a reviewer doesn't waste time or raise false bugs:

1. **Integration parity is proven locally** — the Postgres conformance suite ran
   green against testcontainers (see B5). In a session that predates `docker` group
   membership, run it via `sg docker -c '…'` or after a fresh login; `DATABASE_URL`
   bypasses Docker entirely.
2. **`internal/reading` coverage is 88.6%**, just under the 90% domain target. Decide
   (via B4) whether the uncovered lines are meaningful.
3. **Binaries are placeholders** — `reader-api`/`readerctl` do nothing. No HTTP,
   dispatcher, pipeline, adapters, config, or CLI subcommands exist yet (Phases 3–12).
4. **Toolchain `PATH` is configured in `~/.bashrc`** — interactive shells resolve
   the tools directly; only minimal non-interactive shells need the manual export
   in §1.
5. **`store.Postgres.UpdateStatus` does a read-then-write** (GetByID then update)
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
- [ ] B2 `make test-race` no data races
- [ ] B3 `make cover` — clock ≥90%, reading ~88.6%, store ~46.7% (artifact, see note)
- [ ] B4 reading uncovered lines inspected and judged benign
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

### D — Conventions
- [ ] D1 no wall-clock/RNG/network in domain core + memory fake (fallback noted)
- [ ] D2 `internal/reading` stdlib-only (`go list -deps`)
- [ ] D3 black-box `_test` packages
- [ ] D4 integration behind `//go:build integration` only
- [ ] D5 fakes co-located with ports and race-safe
- [ ] D6 table-driven subtests + `t.Parallel()` where safe

### E — Sign-off
- [ ] Limitations in §6 acknowledged
- [ ] Any deviation from `docs/backend-tdd-plan.md` recorded with rationale
- [ ] Overall verdict: Phases 0–2 **accept / reject** (record in §8 sign-off log)

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
- Scope is **Phases 0–2 only**; Phases 3–12 (dispatcher, pipeline, external-service
  ports, HTTP API, CLI, hardening) are not yet built and are out of scope for this
  sign-off (§0).
- No deviations from `docs/backend-tdd-plan.md` were found.
