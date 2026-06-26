# PR #8 — Bound Forced-Recovery Waits on Stale Handlers (TDD Plan)

Addresses review finding
[discussion r3483108349](https://github.com/brian-bell/reading-lite-next/pull/8#discussion_r3483108349)
(P2, `chatgpt-codex-connector`): *"Bound forced recovery waits on stale handlers."*

## 1. The finding

`Dispatcher.ForceSubmitAfter` (`internal/dispatch/dispatcher.go:170`) waits for an
already-running handler to exit before it resets and re-enqueues the reading:

```go
it, wait := d.forceClaim(item{id: id})
if wait != nil {
    <-wait            // dispatcher.go:179 — UNBOUNDED receive
}
```

`wait` is the stale claim's `done` channel; it is closed only by the deferred
`close(done)` in `handle` (`dispatcher.go:333`), which runs **after**
`d.Handler(runCtx, it.id)` returns. `forceClaim` cancels the stale `runCtx`
first, but cancellation only unblocks a *context-aware* handler. If the stale
handler is parked in a non-context-aware adapter call (a socket read with no
deadline, a driver call that ignores ctx, a stuck `io.Copy`), `done` never
closes and `<-wait` blocks forever.

Two things make this worse than a single slow request:

1. **`forceMu` amplifies it into a global stall.** `ForceSubmitAfter` holds
   `d.forceMu` for the whole wait (`dispatcher.go:171-172`). One stuck handler
   wedges forced recovery for *every other reading*, not just its own.
2. **The caller cannot interrupt it.** `readingops.Reprocess`
   (`internal/readingops/service.go:194-202`) deliberately detaches from request
   cancellation with `context.WithoutCancel(ctx)` so a client disconnect cannot
   abort a half-applied recovery. With the wait unbounded, the operator's
   reprocess request hangs indefinitely and the reading is never recovered —
   the exact failure the endpoint exists to fix.

The reviewer's prescription: *"use a bounded/ctx-aware wait or a store-level
generation fence instead of waiting forever."*

## 2. Why bounding the wait is safe — the fence already exists

The wait's doc comment claims it is load-bearing: *"Waiting before beforeQueue
ensures a stale handler cannot write content blobs or vectors after the
replacement reset has started."* That overstates its role. Correctness against
stale writes is **already** provided by a store-level generation fence plus
run-scoped keys, independent of the wait:

- **Store content/tag writes are fenced by `ExpectedStartedAt`.** The pipeline
  passes `r.StartedAt` as `ContentFields.ExpectedStartedAt` /
  `TagFields.ExpectedStartedAt` (`internal/pipeline/pipeline.go:183-189,394-415`).
  Both `store.Memory` (`internal/store/memory.go:148,276`) and `store.Postgres`
  (`internal/store/postgres.go:168,240`) reject the write with `ErrConflict`
  unless the row is still `Running` with that same `started_at`. A forced
  reprocess calls `store.Reprocess`, which sets `Pending` and clears
  `started_at`, so any later write from the stale run fails the fence. This is
  exactly the "store-level generation fence" the reviewer names; it is the
  durable backstop. (Pinned by `storetest` contract cases at
  `internal/store/storetest/contract.go:559,579,751,767`.)

- **Blob keys are run-scoped, so stale blob writes cannot clobber.** `rawKey` /
  `contentKey` embed `run-<startedAt.UnixNano()>` (`pipeline.go:486-499`). A
  stale run (old `started_at`) and the replacement run (new `started_at`) write
  to *different* keys. A stale blob write is at worst an orphan at the old run
  key — never an overwrite of replacement content.

- **The vector upsert is gated behind the guarded checkpoint.** On the fresh
  path, `Pipeline.Process` defers `upsertVector` until after the
  `ExpectedStartedAt`-fenced `UpdateContent` (`pipeline.go:147-152`). Once the
  reset lands, the stale run's checkpoint fails the fence and it returns before
  upserting, so the vector (keyed by bare `r.ID`) is not clobbered.

- **Dispatcher status writes are token-fenced.** After the handler returns,
  `handle` re-checks `d.active(it)` (`dispatcher.go:356`) and `finish` /
  `redispatch` verify the token (`dispatcher.go:383,436`), so a replaced run
  cannot write `ready`/`failed`/`pending` for the new claim.

**Conclusion:** the wait is a best-effort optimization (drain the stale handler
cleanly to minimize orphan blobs), not the correctness gate. Therefore it is
safe to (a) cap it and (b) *proceed* when the cap elapses — the fence makes any
late stale write a no-op.

### Residual gaps (documented, accepted, or optionally hardened)

- **Orphan blobs.** A stale handler that writes its content/raw blob after the
  reset leaves an orphan at the old run key. Harmless (distinct key, never
  served — the row's `content_key`/`raw_key` point at the replacement run), GC'd
  by Phase 11 lifecycle work. The clean-drain value of waiting is *fewer orphans
  for cancelable handlers* (they exit on cancel before the budget, so the budget
  rarely fires for them); a genuinely stuck handler writes the same orphans
  whether we wait forever or proceed, so bounding adds no orphans in the case
  that motivated it.

- **Narrow vector window on the reuse path.** On idempotent re-entry
  (`r.ContentKey != ""`), `upsertVector` runs *without* a preceding guarded
  checkpoint (`pipeline.go:127-135`), keyed by bare `r.ID` (`pipeline.go:350`).
  A stale reuse run whose vector adapter ignores `ctx` could, in a narrow
  interleaving, upsert a stale vector after the replacement's. Note this is
  exactly the threat model of this PR (a non-context-aware call), so we must
  *not* hand-wave it away with "cancellation closes the window" — under that
  model cancellation does not. The reason it is acceptable is **severity, not
  likelihood**: a forced *reprocess* re-embeds the same source body, so the
  stale vector is approximately equal to the replacement vector — a stale upsert
  produces a near-identical embedding, not corrupt data. Closing it fully
  requires fencing the vector `Upsert` itself; tracked as **optional follow-up
  hardening** in §7, out of scope for this P2 fix.

- **Stuck worker goroutine is not reclaimed.** Proceeding on timeout unblocks
  `forceMu` and `readingops`, but a handler genuinely stuck in a non-cancelable
  call keeps occupying its worker goroutine forever — Go cannot kill a
  goroutine, so this is inherent and *pre-existing* (the unbounded wait did not
  reclaim it either; it just also hung the caller). Repeated forced reprocesses
  of such a reading would monotonically consume the `Workers` pool. In practice
  this does not arise because every production adapter is ctx- or
  timeout-bounded (`fetch.HTTP` has a client timeout, pgx honors `ctx`, etc.), so
  a handler stuck forever should not occur; truly bounding adapter calls is
  Phase 11 lifecycle hardening. This fix's contribution is that such a handler no
  longer also wedges the *recovery* path.

## 3. Decision

Implement a **bounded, ctx-aware wait that proceeds on timeout**, backed by the
existing store generation fence. Concretely: honor `ctx.Done()` as a true abort,
and cap the wait with an injected, deterministic budget; when the budget
elapses, stop waiting and continue with `beforeQueue` + enqueue (the fence
guarantees safety).

This satisfies *both* halves of the reviewer's "bounded/ctx-aware wait **or**
store-level generation fence" — we bound the wait *and* lean on the fence as the
correctness backstop.

### Alternatives considered and rejected

- **Remove the wait entirely, rely only on the fence.** Simplest and endorsed by
  the reviewer. Rejected as the primary because it gives up the clean-drain
  benefit unconditionally (more orphan blobs every forced reprocess of a live
  run) for no gain over a bounded wait, which keeps that benefit in the common
  case while still being un-wedgeable. (We effectively get the fence-only
  behavior on the rare timeout path.)
- **Bounded wait that returns an error on timeout.** Unblocks `forceMu` but
  leaves *this* reading unrecovered until the stuck handler dies — only half the
  fix. Proceeding recovers it immediately.
- **Per-id force locking instead of a single `forceMu`.** Removes the
  cross-reading amplification but not the per-reading hang. More surface; the
  bounded wait already prevents the hang. Out of scope.
- **Add a generation column to the vector index and fence `Upsert`.** Closes the
  narrow reuse-path window fully, but touches the `vector.Index` port, both
  backends, and the contract suite. Disproportionate for a P2; tracked as
  optional follow-up (§7).

## 4. Design

### 4.1 Dispatcher changes (`internal/dispatch/dispatcher.go`)

Add a bounded-wait budget field:

```go
type Dispatcher struct {
    // ...existing fields...

    // ForceWaitTTL caps how long ForceSubmitAfter waits for a cancelled stale
    // handler to exit before it proceeds with recovery anyway. The wait is a
    // best-effort clean drain to avoid orphan blobs; the store ExpectedStartedAt
    // fence makes any late stale write a no-op, so proceeding on timeout is safe
    // and keeps a handler stuck in a non-cancelable call from wedging recovery.
    // Defaults to defaultForceWaitTTL when unset.
    ForceWaitTTL time.Duration
}

const defaultForceWaitTTL = 5 * time.Second
```

Replace the unbounded receive with a helper that selects over the handler exit,
a budget timer scheduled through the existing `Delay` seam, and `ctx.Done()`:

```go
func (d *Dispatcher) ForceSubmitAfter(ctx context.Context, id string, beforeQueue func() error) error {
    d.forceMu.Lock()
    defer d.forceMu.Unlock()

    if err := ctx.Err(); err != nil {
        return err
    }
    it, wait := d.forceClaim(item{id: id})
    if wait != nil {
        if err := d.awaitStaleHandler(ctx, wait); err != nil {
            d.release(it) // abort: relinquish the replacement claim so the id is reclaimable
            return err
        }
    }
    if !d.active(it) {
        return nil
    }
    if err := beforeQueue(); err != nil {
        d.release(it)
        return err
    }
    d.queueClaimed(it)
    return nil
}

// awaitStaleHandler blocks until the cancelled stale handler exits, the
// ForceWaitTTL budget elapses, or ctx is cancelled. A budget timeout is NOT an
// error — the caller proceeds and relies on the store ExpectedStartedAt fence —
// so a handler stuck in a non-cancelable call cannot wedge forced recovery. ctx
// cancellation is a real abort and is returned to the caller.
func (d *Dispatcher) awaitStaleHandler(ctx context.Context, wait <-chan struct{}) error {
    budget := d.ForceWaitTTL
    if budget <= 0 {
        budget = defaultForceWaitTTL
    }
    timeout := make(chan struct{})
    d.Delay.After(budget, func() { close(timeout) })

    select {
    case <-wait:    // clean exit: stale handler drained, no orphan
        return nil
    case <-timeout: // budget elapsed: proceed; the fence covers safety
        return nil
    case <-ctx.Done():
        return ctx.Err()
    }
}
```

Notes / invariants:

- **Claim-leak fix on the ctx-abort path.** `forceClaim` has already cancelled
  the stale handler and installed the new token. If `awaitStaleHandler` returns
  a ctx error, we must `d.release(it)` before returning, or the id stays "owned"
  with a token that has no running handler and nothing queued, permanently
  dropping future `Submit`s. The existing `beforeQueue`-error and `!active`
  paths already release/no-op; the new abort path needs its own `release`. This
  is a behavior the plan adds a test for (§5, test C). `release(it)` is safe
  here: `it.token` is the *new* token and `forceClaim` set its `cancel=nil`
  (`dispatcher.go:208`), so `release` just deletes the entry; the still-running
  stale handler's own `finish`/`active` checks fail on the replaced token, so
  there is no double-cancel.
- **The ctx-abort branch is defensive, not on the production hot path.** Both
  current callers pass a non-cancelable context — `ForceSubmit` uses
  `context.Background()` (`dispatcher.go:160`) and `readingops.Reprocess` uses
  `context.WithoutCancel(ctx)` (`service.go:197`) — so in today's codebase the
  only bound that actually trips is the budget timeout; `ctx.Done()` never
  fires. We still honor `ctx` (and test the release-fix) so the method is correct
  for any future caller that passes a cancelable/bounded context, and so the
  "ctx-aware" half of the reviewer's prescription is genuinely satisfied rather
  than nominal.
- **`Delay` must be non-nil when a forced submit can contend.** `awaitStaleHandler`
  calls `d.Delay.After`, so a `Dispatcher` that can take the forced-recovery path
  while an id is in flight must have `Delay` set. This is not a new burden in
  practice: redispatch already requires `Delay`, every test sets
  `Delay: &dispatch.FakeDelayer{}`, and production wiring (Phase 11) will set
  `RealDelayer`. Worth one line in the field doc so a future minimal construction
  does not nil-panic. (Note `Delay.After` is only reached when `wait != nil`,
  i.e. there was an in-flight claim to drain; an uncontended `ForceSubmit` never
  touches `Delay`.)
- **Timer cleanup.** The budget is scheduled through `Delay`. `FakeDelayer`
  records it and never fires unless `FireAll` is called, so existing force tests
  that expect the wait to block until the handler exits keep passing untouched.
  `RealDelayer` (`time.AfterFunc`) fires once even after `<-wait` won; the
  callback closes a per-call channel with no remaining listener, which is
  harmless. We accept a short-lived (≤ `ForceWaitTTL`) uncancelled timer on the
  clean-exit path rather than widen the `Delayer` interface to return a
  canceler; this preserves the deterministic-timing convention. (If profiling
  ever shows it matters, add `Delay.AfterCancelable`; not now.)
- **Determinism.** No `time.Now`/`time.Sleep` is introduced; the only timing is
  through the injected `Delay` seam, matching the repo rule "run delays through
  the injected `Delayer` seam."

### 4.2 Doc-comment correction

Update the `ForceSubmitAfter` doc comment: the wait is a *best-effort, bounded*
clean drain; correctness against stale writes comes from the store
`ExpectedStartedAt` fence + run-scoped blob keys, not from waiting. State that on
timeout it proceeds.

### 4.3 `readingops` — no behavior change required

`Reprocess` keeps `context.WithoutCancel(ctx)` (the recovery must survive client
disconnect). The dispatcher now bounds the wait internally, so this no longer
risks an unbounded hang. Add a one-line comment at
`internal/readingops/service.go:197` noting the dispatcher bounds the wait, so a
future reader does not "fix" it by reintroducing request-scoped cancellation
(which would re-expose the half-applied-recovery race). No production code change
beyond the comment.

## 5. TDD steps (one vertical behavior at a time, black-box `dispatch_test`)

All new tests live in `internal/dispatch/dispatcher_test.go` (package
`dispatch_test`), following existing patterns (`recordingHandler`, `seedPending`,
`store.Memory`, fake clock, `FakeDelayer`). Run `go test -race
./internal/dispatch` after each.

### Synchronization primitive: `FakeDelayer.PendingLen()` is the rendezvous

The budget timer is scheduled by `awaitStaleHandler` via `d.Delay.After`
**after** `forceClaim` runs. So once a `FakeDelayer` reports the budget is
registered, the forcing goroutine is provably past `forceClaim` and at/in the
`select`. Both timing-sensitive tests below use this instead of sleeps:

```go
// budget registered ⇒ goroutine is in awaitStaleHandler's select.
waitForPending := func(want int) {
    for i := 0; i < 2000; i++ {
        if delay.PendingLen() == want { return }
        time.Sleep(time.Millisecond) // bounded spin, deterministic outcome
    }
    t.Fatal("force budget was never scheduled")
}
```

In Tests A/C/D the stuck call-0 schedules no redispatch, so the budget is the
*only* pending delay — `PendingLen() == 1` is an unambiguous rendezvous.

**WARNING for future test authors (I5):** because the budget rides the same
`FakeDelayer` as retry/rate-limit redispatch, `FireAll` fires *both* in any test
that has a pending redispatch *and* a force budget. Keep force-budget tests free
of pending retries (the stuck handler never returns, so it schedules none), or
assert `PendingLen`/`Durations` to disambiguate before firing.

**Test A — stuck stale handler: bounded wait proceeds and recovers (the
finding).** *Red first — this is the bug proof.*
- Seed `r1` pending; start the pool with `Workers: 2`, `delay := &FakeDelayer{}`,
  and an explicit `ForceWaitTTL`.
- Handler call 0 blocks on a channel the test never closes **and ignores its
  ctx** (simulating a non-cancelable adapter); call 1 returns `Done`.
- `Submit("r1")`; read from `entered` to confirm call 0 has started.
- In a goroutine call `ForceSubmitAfter(context.Background(), "r1", beforeQueue)`
  where `beforeQueue` records it ran and returns nil; send the result on a
  channel.
- **`waitForPending(1)`** — proves the goroutine reached `Delay.After` (B1 fix:
  do *not* call `FireAll` blindly; if you fire before the budget is registered it
  is never fired and the test hangs).
- Assert still blocked: `beforeQueue` not run and `entered` has no call 1.
- `delay.FireAll()` to fire the budget.
- Assert `ForceSubmitAfter` returns nil, `beforeQueue` ran, and call 1 (the
  replacement) starts — *without* the stuck call 0 ever unblocking.
- On current code `<-wait` blocks forever and the result channel never delivers →
  the test times out (valid red). The §4 change makes it pass.

**Test B — clean exit still drains before proceeding (no regression).**
- The existing `TestDispatch_ForceSubmitRequeuesInFlightID` already encodes this:
  stale handler exits on `firstProceed`; the budget is registered on the
  `FakeDelayer` but **never fired** (no `FireAll`), so `awaitStaleHandler` blocks
  on `<-wait` until the handler exits, exactly as today. Confirm it still passes
  unchanged; optionally set `ForceWaitTTL` explicitly to document intent. No new
  test needed — this is a regression guard, and I verified the budget-scheduling
  change does not perturb its assertions (it makes no `delay.Total()` claim after
  the force).

**Test C — ctx cancelled mid-wait aborts and releases the claim.** *(B2 fix —
make the during-wait path deterministic.)*
- Stale handler call 0 blocked and ignores ctx; `Submit`; read `entered` for
  call 0.
- In a goroutine call `ForceSubmitAfter(forceCtx, "r1", beforeQueue)` (with a
  cancelable `forceCtx`).
- **`waitForPending(1)`** — guarantees the goroutine is past the top-of-function
  `ctx.Err()` early return (`dispatcher.go:174`) and inside the `select`. Only
  *then* `cancelForce()`. Without this rendezvous the cancel can race ahead of
  `forceClaim`, hit the pre-claim early return, and the test would pass for the
  wrong reason (proving nothing about the new release path — the exact hole the
  existing `…CanceledBeforeClaimLeavesRunAlone` test occupies).
- Since `wait` never closes and the budget is never fired, the only ready
  `select` case after cancel is `ctx.Done()` → deterministic.
- Assert `ForceSubmitAfter` returns `context.Canceled`, `beforeQueue` did **not**
  run, and no replacement was queued.
- Release call 0; then `Submit("r1")` must run a fresh handler — proving the
  abort path released the replacement claim (no leaked ownership).

**Test D — `forceMu` is not wedged by a stuck handler (amplification fixed).**
- Two readings: `r1` (stuck, ignores ctx) and `r2` (normal).
- Force `r1` (proceeds after `waitForPending(1)` + `FireAll`), then force `r2`
  and assert it completes promptly and `r2`'s replacement runs — proving one
  stuck reading no longer blocks recovery of others. May be folded into Test A as
  a second phase if cleaner. (Asserts the *cross-reading* benefit; the
  *per-reading* worker-goroutine leak from §2 is out of scope and untestable.)

**`readingops` coverage (replaces the dropped fake-submitter Test E, I3).** A
fake submitter is synchronous and cannot reproduce the unbounded wait, so a
`service_test.go` test against the fake would only re-assert "`Reprocess`
returns" — already covered by the existing `stale_running` case in
`TestReprocess_ReadyFailedAndStaleReadingsResetAndEnqueue`
(`service_test.go:444`). Do **not** add a redundant fake-submitter test. The
dispatcher unit tests A–D are the primary proof of the bounded wait; the
readingops integration test below is a *recommended* cross-package proof, not a
substitute for them.

Add **one** end-to-end test (black-box `readingops_test`, importing
`internal/dispatch` — no import cycle: `dispatch` does not import `readingops`,
and `readingops` production code only defines its own `Dispatcher` interface at
`service.go:19-23`; `*dispatch.Dispatcher` satisfies it). Wire a real
`readingops.Service` to a real `*dispatch.Dispatcher`. It must be orchestrated so
the bounded wait is actually reached — a literal "seed a Running row + Reprocess"
does **not** work, for four reasons that the test must handle:

1. **A real in-flight claim must exist** or `forceClaim` finds nothing
   (`wait == nil`, `dispatcher.go:186`), schedules no budget, and `waitForPending`
   spins out. So the id must be claimed by a real parked handler — `Submit` it
   through the dispatcher first; a bare store row is not a claim.
2. **Use the async pool, not `Inline: true`.** Run `Workers: 2` + `go d.Run(ctx)`
   so (a) `Submit` returns while call-0 parks in a worker, and (b) the
   replacement (call-1) runs in a worker rather than synchronously inside
   `Reprocess`'s goroutine — otherwise a stuck-or-slow replacement would hang
   `Reprocess`.
3. **The row must be made stale** or `readingops` takes the non-force path:
   `handle` stamps `StartedAt = Clock.Now()` on the Running write
   (`dispatcher.go:336-342`), so the row is fresh; `Reprocess` derives `force`
   from `reading.AnnotateStale(got, s.now(), s.TTLs)` (`service.go:188-192`).
   Advance the shared fake clock past the *service's* running TTL after call-0
   parks so `AnnotateStale` returns `Failed` and the force path is taken.
4. **`Reprocess` itself blocks until the budget fires**, so call it in a
   goroutine; the replacement handler (call-1) must return `Done` (Test-A
   handler script: call 0 ignores ctx and blocks, call 1 returns).

Sketch:
- Seed `r1` pending; `d := &Dispatcher{Handler: h.handle, Store: st, Clock: clk,
  Delay: delay, Workers: 2, ForceWaitTTL: …}`; `go d.Run(ctx)`.
- `svc.Dispatcher = d`; configure `svc.TTLs` so a short clock advance makes a
  running row stale.
- `d.Submit("r1")`; read `entered` for call 0 (now Running@T0, parked).
- Advance `clk` past `svc.TTLs` running threshold.
- `go func(){ resCh <- svc.Reprocess(ctx, "r1") }()`.
- `waitForPending(1)`; `delay.FireAll()`.
- Assert `<-resCh` is `{Status: Pending}, nil` (the deterministic return value),
  and that the replacement reached `Ready` via `waitStatus` (the final state
  itself proves a fresh run executed). Do **not** assert a literal mid-flight
  `GetByID(r1).Status == Pending` right after `<-resCh`: that state is transient
  (worker B may have already driven `Pending→Running→Ready`), so the check would
  be racy. The two assertions above are sufficient.
- Cleanup: unblock call 0, `cancel()` the pool, await `Run` return.

This pins the real cross-package contract — operator recovery completes despite a
stuck handler — which the fake submitter cannot. Not behind the `integration`
build tag (no Docker).

Each test: write it, watch it fail (A is the proof-of-bug), implement the
minimum from §4 to pass, re-run focused tests with `-race`, refactor green.

## 6. Acceptance, docs, and verification

- **Doc strings:** update `ForceSubmitAfter` (and the new `ForceWaitTTL` field
  and `awaitStaleHandler`) per §4.2.
- **Project docs:** the dispatcher description in `CLAUDE.md` and `AGENTS.md`
  mentions the force/stale-recovery seam; add a clause that the forced-recovery
  wait is **bounded** (`ForceWaitTTL`) and proceeds on timeout under the store
  `ExpectedStartedAt` fence. Check `docs/PLAN.md` §1.4/§1.7 wording for the same
  and reconcile. Use the `docs` skill if the edits sprawl.
- **Acceptance harness:** no new public HTTP behavior, so no new
  `internal/acceptance` case is strictly required. Optional: a black-box
  acceptance assertion that a stale-running reprocess returns promptly (bounded)
  rather than a structural check — only if it reads cleanly.
- **Verification (run in order):**
  - `go test ./internal/dispatch` (focused, after each TDD step)
  - `go test ./internal/readingops`
  - `go test -race ./internal/dispatch ./internal/readingops` (this change is
    concurrency-sensitive; race detector is mandatory)
  - `go test ./...`
  - `go test -tags verify ./internal/acceptance`
  - branch autoreview:
    `/home/brian/dev/agent-skills/third-party/autoreview/scripts/autoreview --mode local --parallel-tests "go test ./..."`
  - re-reply to the PR thread (or push a commit) so the finding is resolved.

## 7. Optional follow-up (out of scope, track separately)

- **Fully fence the vector upsert.** Carry an expected generation into
  `vector.Index.Upsert` (or make the reuse-path upsert conditional on the
  guarded checkpoint) to close the narrow non-ctx-aware reuse-path window in §2.
  Requires touching the `vector` port + both backends + `vectortest`; size it as
  its own slice if/when a real non-cancelable vector adapter appears.
- **Orphan-blob GC.** Phase 11 lifecycle hardening already owns reclaiming
  blobs at superseded run keys; the bounded wait does not add a new class of
  orphan, only (rarely) one more per forced reprocess.

## 8. Commit plan

Single focused commit on `codex/phase-8-http-api`:

```
fix(dispatch): bound forced-recovery wait on stale handlers

ForceSubmitAfter waited unbounded on a cancelled stale handler's done
channel; a handler stuck in a non-cancelable call wedged forced recovery
(forceMu-serialized) and hung readingops.Reprocess indefinitely. Cap the
wait with ForceWaitTTL via the Delay seam and proceed on timeout, honoring
ctx as a true abort. Safety rests on the existing store ExpectedStartedAt
generation fence + run-scoped blob keys, so a late stale write is a no-op.
```

Push and reply on the PR thread referencing this finding.

## 9. Definition of done

- `ForceSubmitAfter` cannot block past `ForceWaitTTL` for any handler
  (cancelable or not); ctx cancellation still aborts immediately.
- The ctx-abort path releases the replacement claim (no leaked ownership).
- A stuck handler on one reading no longer blocks the *forced-recovery path*
  (`forceMu`) or the caller for other readings. (Caveat: it still consumes its
  own worker goroutine — inherent, pre-existing, and not in scope; see §2.)
- `readingops.Reprocess` of a stale-running reading always returns.
- Clean-drain ordering preserved when the handler is cancelable.
- All tests green incl. `-race`; `verify` and autoreview clean; PR thread
  answered.
