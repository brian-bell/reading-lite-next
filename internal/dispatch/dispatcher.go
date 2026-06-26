package dispatch

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bbell/reading-lite/internal/clock"
	"github.com/bbell/reading-lite/internal/reading"
	"github.com/bbell/reading-lite/internal/store"
)

const (
	defaultBuffer      = 1024
	defaultMaxAttempts = 5
	// defaultForceWaitTTL bounds how long ForceSubmitAfter waits for a cancelled
	// stale handler to drain before it proceeds with recovery anyway.
	defaultForceWaitTTL = 5 * time.Second
)

// Store is the narrow persistence surface the dispatcher needs: it advances a
// reading's lifecycle status and lists non-terminal readings for recovery.
type Store interface {
	UpdateStatus(ctx context.Context, id string, status reading.Status, fields store.StatusFields) error
	ListNonTerminal(ctx context.Context, runningCutoff time.Time) ([]store.Pending, error)
}

// Dispatcher runs the pipeline asynchronously. The ingest handler hands it a
// reading id; a pool of worker goroutines drains an in-memory channel and runs
// Handler for each id, persisting the lifecycle outcome and re-dispatching on
// retry or rate limit. A startup [Dispatcher.Sweep] re-dispatches readings left
// non-terminal by a crash.
type Dispatcher struct {
	// Handler runs the pipeline for one reading id and reports the outcome.
	Handler func(ctx context.Context, id string) Result
	// Store persists lifecycle status and backs recovery.
	Store Store
	// Clock supplies deterministic timestamps.
	Clock clock.Clock
	// Delay schedules re-dispatch after backoff or a rate-limit delay.
	Delay Delayer
	// Workers is the number of draining goroutines (defaults to 1).
	Workers int
	// Max is the maximum number of attempts before a transient failure is final
	// (defaults to defaultMaxAttempts when unset, so a zero value never fails fast).
	Max int
	// Buffer sizes the dispatch channel (defaults to defaultBuffer).
	Buffer int
	// RunningTTL bounds how long a running reading may stall before Sweep recovers it.
	RunningTTL time.Duration
	// ForceWaitTTL caps how long ForceSubmitAfter waits for a cancelled stale
	// handler to exit before it proceeds with recovery anyway (defaults to
	// defaultForceWaitTTL when unset). The wait is a best-effort clean drain to
	// avoid orphan blobs; the store ExpectedStartedAt fence makes any late stale
	// write a no-op, so proceeding on timeout is safe and keeps a handler stuck
	// in a non-cancelable call from wedging forced recovery. The wait is
	// scheduled through Delay, so Delay must be set whenever a forced submit can
	// contend with an in-flight claim (redispatch already requires it).
	ForceWaitTTL time.Duration
	// Inline runs the initial handle synchronously on Submit/Sweep instead of via
	// the worker channel, for deterministic handler tests and the HTTP layer. Note
	// that re-dispatch after a retry or rate limit still flows through the injected
	// Delay seam, so pair Inline with a Delayer you control (e.g. FakeDelayer) to
	// keep that path deterministic too; the production topology uses Run instead.
	Inline bool

	chOnce sync.Once
	ch     chan item

	inflightMu sync.Mutex
	inflight   map[string]claim
	nextToken  uint64
	forceMu    sync.Mutex
}

type claim struct {
	token  uint64
	cancel context.CancelFunc
	done   <-chan struct{}
}

// claim takes single-process ownership of a reading id. Ownership is held from the
// moment the id is enqueued until its work reaches a terminal outcome — across the
// time it sits queued AND across every retry/requeue in between — so a duplicate
// dispatch (a second Submit, or a sweep re-enqueuing an id that is already queued
// or in flight) is dropped instead of running the pipeline a second time and
// overwriting the winner's terminal status. This single-instance guard matches the
// process topology; a multi-instance deployment would need a store-level claim.
func (d *Dispatcher) claim(it item) (item, bool) {
	d.inflightMu.Lock()
	defer d.inflightMu.Unlock()

	if _, ok := d.inflight[it.id]; ok {
		return item{}, false
	}
	if d.inflight == nil {
		d.inflight = map[string]claim{}
	}
	d.nextToken++
	it.token = d.nextToken
	d.inflight[it.id] = claim{token: it.token}
	return it, true
}

func (d *Dispatcher) release(it item) {
	d.inflightMu.Lock()
	defer d.inflightMu.Unlock()

	current, ok := d.inflight[it.id]
	if !ok || current.token != it.token {
		return
	}
	if current.cancel != nil {
		current.cancel()
	}
	delete(d.inflight, it.id)
}

func (d *Dispatcher) active(it item) bool {
	d.inflightMu.Lock()
	defer d.inflightMu.Unlock()

	current, ok := d.inflight[it.id]
	return ok && current.token == it.token
}

func (d *Dispatcher) beginRun(ctx context.Context, it item) (context.Context, context.CancelFunc, chan struct{}, bool) {
	d.inflightMu.Lock()
	defer d.inflightMu.Unlock()

	current, ok := d.inflight[it.id]
	if !ok || current.token != it.token {
		return nil, nil, nil, false
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	current.cancel = cancel
	current.done = done
	d.inflight[it.id] = current
	return runCtx, cancel, done, true
}

func (d *Dispatcher) ensureCh() {
	d.chOnce.Do(func() {
		n := d.Buffer
		if n <= 0 {
			n = defaultBuffer
		}
		d.ch = make(chan item, n)
	})
}

// Submit enqueues id for processing at attempt 0. It is non-blocking: a duplicate
// (already queued or in flight) or a full channel is dropped rather than blocking
// ingest; the reading stays pending and is recovered by the startup Sweep,
// backstopped meanwhile by read-time stale annotation plus on-demand reprocess
// (PLAN.md §1.4).
func (d *Dispatcher) Submit(id string) {
	it, ok := d.claim(item{id: id})
	if !ok {
		return // already queued or in flight: drop the duplicate
	}
	d.queueClaimed(it)
}

// ForceSubmit enqueues id at attempt 0 even if this process still considers it
// queued or in flight. It is reserved for explicit operator recovery of stale
// running work; ordinary ingest and retry paths should use [Dispatcher.Submit]
// so the duplicate guard remains intact.
func (d *Dispatcher) ForceSubmit(id string) {
	_ = d.ForceSubmitAfter(context.Background(), id, func() error { return nil })
}

// ForceSubmitAfter serializes forced submissions, checks ctx before committing
// to recovery, cancels and replaces any existing in-process claim, waits
// (bounded) for an already-running handler to drain, runs beforeQueue while the
// new claim is reserved, then enqueues id at attempt 0. If beforeQueue fails,
// the replacement claim is released and no work is queued.
//
// The wait is a best-effort clean drain, not a correctness gate: stale writes
// are already neutralized by the store ExpectedStartedAt fence (content/tags),
// run-scoped blob keys, and the vector upsert deferred behind the guarded
// checkpoint on the fresh-acquire path. So the wait is bounded by ForceWaitTTL
// and PROCEEDS on timeout — a stale handler stuck in a non-cancelable call
// cannot wedge forced recovery (which is serialized on forceMu) or hang the
// caller. ctx cancellation is a real abort: it releases the replacement claim
// and returns ctx.Err().
func (d *Dispatcher) ForceSubmitAfter(ctx context.Context, id string, beforeQueue func() error) error {
	d.forceMu.Lock()
	defer d.forceMu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	it, wait := d.forceClaim(item{id: id})
	if wait != nil {
		if err := d.awaitStaleHandler(ctx, wait); err != nil {
			// ctx aborted mid-wait: relinquish the replacement claim so the id is
			// reclaimable, otherwise it stays owned with no running handler and
			// nothing queued, dropping every future Submit. forceClaim already
			// cancelled the stale handler, so the row may be left Running; the
			// startup Sweep + read-time stale annotation recover it. (No production
			// caller reaches this: ForceSubmit and readingops.Reprocess both pass a
			// non-cancelable ctx; it is here for a future cancelable caller.)
			d.release(it)
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
// error: the caller proceeds and relies on the store ExpectedStartedAt fence, so
// a handler stuck in a non-cancelable call cannot wedge forced recovery. ctx
// cancellation is a real abort and is returned to the caller. The budget rides
// the Delay seam so the bound stays deterministic under FakeDelayer.
func (d *Dispatcher) awaitStaleHandler(ctx context.Context, wait <-chan struct{}) error {
	if d.Delay == nil {
		// No delay seam wired: skip the best-effort drain and proceed immediately
		// rather than nil-panic inside a caller's goroutine. The store
		// ExpectedStartedAt fence still makes any late stale write a no-op.
		return nil
	}
	budget := d.ForceWaitTTL
	if budget <= 0 {
		budget = defaultForceWaitTTL
	}
	timeout := make(chan struct{})
	d.Delay.After(budget, func() { close(timeout) })

	select {
	case <-wait: // clean exit: stale handler drained, no orphan
		return nil
	case <-timeout: // budget elapsed: proceed; the store fence covers safety
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *Dispatcher) forceClaim(it item) (item, <-chan struct{}) {
	d.inflightMu.Lock()
	defer d.inflightMu.Unlock()

	if d.inflight == nil {
		d.inflight = map[string]claim{}
	}
	var wait <-chan struct{}
	if current, ok := d.inflight[it.id]; ok {
		if current.cancel != nil {
			current.cancel()
		}
		wait = current.done
	}
	d.nextToken++
	it.token = d.nextToken
	d.inflight[it.id] = claim{token: it.token, done: wait}
	return it, wait
}

// queueClaimed dispatches an item whose id is already claimed — the live path for
// Submit and for re-dispatch. On a full-channel drop it releases the claim, since
// the item will not run; the recovery Sweep re-dispatches the still-pending reading.
func (d *Dispatcher) queueClaimed(it item) {
	if !d.active(it) {
		return
	}
	if d.Inline {
		// Re-dispatch is decoupled from any originating request context; the async
		// worker pool (Run) scopes work to the run context instead, and inline is
		// the test/handler seam.
		d.handle(context.Background(), it)
		return
	}

	d.ensureCh()
	select {
	case d.ch <- it:
	default:
		// Buffer full: drop and relinquish ownership. The default buffer is sized
		// so this is a rare overload backstop, not a normal occurrence.
		d.release(it)
	}
}

// enqueueRecovered is the non-lossy path used by Sweep: recovery must not drop, so
// it claims the id and blocks until a worker accepts it or ctx is cancelled.
// Callers run the worker pool concurrently (or pass a bounded ctx) to avoid blocking.
func (d *Dispatcher) enqueueRecovered(ctx context.Context, it item) error {
	// Honor cancellation deterministically: a select with both a ready send and a
	// ready ctx.Done picks at random, so check the context first — otherwise a
	// cancelled sweep could still enqueue (and report success) when the backlog
	// fits in the buffer. The inline path runs under the sweep context too.
	if err := ctx.Err(); err != nil {
		return err
	}
	claimed, ok := d.claim(it)
	if !ok {
		return nil // already queued or in flight: not an error, just a no-op
	}
	it = claimed
	if d.Inline {
		d.handle(ctx, it)
		return nil
	}

	d.ensureCh()
	select {
	case <-ctx.Done():
		d.release(it)
		return ctx.Err()
	case d.ch <- it:
		return nil
	}
}

// Run drains the dispatch channel with a pool of Workers goroutines until ctx is
// cancelled. On cancellation workers stop pulling new work and in-flight runs
// finish (graceful drain).
func (d *Dispatcher) Run(ctx context.Context) {
	d.ensureCh()

	workers := d.Workers
	if workers <= 0 {
		workers = 1
	}

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				// A select with both ctx.Done and a channel receive ready picks
				// uniformly at random, so prioritize cancellation explicitly: a
				// drain must stop pulling new work even while the channel still
				// holds queued ids. Those ids stay pending and are recovered by
				// the startup sweep.
				select {
				case <-ctx.Done():
					return
				default:
				}

				select {
				case <-ctx.Done():
					return
				case it := <-d.ch:
					// Re-check in case cancellation raced with the receive: leave
					// this id for the sweep rather than starting new work, and
					// relinquish ownership so the sweep can re-dispatch it.
					select {
					case <-ctx.Done():
						d.release(it)
						return
					default:
					}
					// Detach cancellation: a drain stops pulling new work but lets
					// in-flight runs finish and persist their outcome.
					d.handle(context.WithoutCancel(ctx), it)
				}
			}
		}()
	}
	wg.Wait()
}

// handle runs one already-claimed item end to end: mark the reading running
// (mirroring the attempt onto process_attempts), run Handler, then apply decide and
// persist the outcome — ready on success, pending plus a scheduled re-dispatch on
// retry or rate limit, failed when the retry budget is spent or the error is
// permanent. It releases ownership on a terminal outcome but RETAINS it across a
// re-dispatch, so the id stays owned for its whole retry chain. It is the only
// method that touches the Store and the Delayer.
func (d *Dispatcher) handle(ctx context.Context, it item) {
	runCtx, cancel, done, ok := d.beginRun(ctx, it)
	if !ok {
		return
	}
	defer func() {
		cancel()
		close(done)
	}()

	now := d.Clock.Now()
	attempt := it.attempt
	if err := d.Store.UpdateStatus(runCtx, it.id, reading.Running, store.StatusFields{
		Now:             now,
		StartedAt:       &now,
		ProcessAttempts: &attempt,
		ClearError:      true,
	}); err != nil {
		// Could not even start; relinquish ownership (the reading is unchanged) so
		// the recovery sweep can re-dispatch it.
		d.release(it)
		return
	}

	maxAttempts := d.Max
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}

	res := d.Handler(runCtx, it.id)
	if !d.active(it) {
		return
	}
	act := decide(res, attempt, maxAttempts)

	// The terminal/redispatch writes below are best-effort: their error is
	// discarded because there is no logger yet (structured logging lands in a
	// later phase). A dropped final write leaves the reading running or pending,
	// which the stale-running/pending sweep and read-time annotation recover.
	switch {
	case act.MarkFailed:
		d.finish(it, func() {
			d.markFailed(runCtx, it.id, attempt, res)
		})
	case act.Redispatch:
		d.redispatch(runCtx, it, act) // keeps ownership across the delay
	default:
		d.finish(it, func() {
			d.markReady(runCtx, it.id)
		})
	}
}

func (d *Dispatcher) finish(it item, write func()) {
	d.inflightMu.Lock()
	defer d.inflightMu.Unlock()

	current, ok := d.inflight[it.id]
	if !ok || current.token != it.token {
		return
	}
	write()
	if current.cancel != nil {
		current.cancel()
	}
	delete(d.inflight, it.id)
}

func (d *Dispatcher) markReady(ctx context.Context, id string) {
	now := d.Clock.Now()
	_ = d.Store.UpdateStatus(ctx, id, reading.Ready, store.StatusFields{
		Now:        now,
		FinishedAt: &now,
		ClearError: true,
	})
}

func (d *Dispatcher) markFailed(ctx context.Context, id string, attempt int, res Result) {
	now := d.Clock.Now()
	// failureMessage is always non-empty, so the failure write populates the error
	// column itself; no ClearError is needed to undo the running write's clear.
	errText := failureMessage(res, attempt)
	_ = d.Store.UpdateStatus(ctx, id, reading.Failed, store.StatusFields{
		Now:             now,
		FinishedAt:      &now,
		Error:           &errText,
		ProcessAttempts: &attempt,
	})
}

// failureMessage renders the persisted error for a failed reading, distinguishing
// a spent retry budget from a permanent failure so the stored reason is always
// actionable — even when a transient Retry carried no error of its own.
func failureMessage(res Result, attempt int) string {
	if res.Outcome == Retry {
		if res.Err != nil {
			return fmt.Sprintf("retry budget exhausted after %d attempts: %v", attempt+1, res.Err)
		}
		return fmt.Sprintf("retry budget exhausted after %d attempts", attempt+1)
	}
	if res.Err != nil {
		return res.Err.Error()
	}
	return "processing failed"
}

func (d *Dispatcher) redispatch(ctx context.Context, it item, act Action) {
	d.inflightMu.Lock()
	defer d.inflightMu.Unlock()

	current, ok := d.inflight[it.id]
	if !ok || current.token != it.token {
		return
	}

	now := d.Clock.Now()
	next := act.NextAttempt
	_ = d.Store.UpdateStatus(ctx, it.id, reading.Pending, store.StatusFields{
		Now:             now,
		ProcessAttempts: &next,
	})
	d.Delay.After(act.Delay, func() {
		// Ownership is retained from the first enqueue through every retry, so the
		// continuation requeues WITHOUT re-claiming.
		it.attempt = next
		d.queueClaimed(it)
	})
}

// Sweep re-dispatches every reading left non-terminal by a crash: all pending
// readings and any running reading whose start preceded the RunningTTL cutoff.
// Each resumes at its stored attempt count so Max is honored across restarts.
//
// Recovery is non-lossy: Sweep blocks on each id until a worker accepts it (or
// ctx is cancelled), so it never silently drops work even when the backlog
// exceeds Buffer. Callers must run the worker pool concurrently — or pass a
// bounded ctx — so a large backlog cannot block startup indefinitely.
func (d *Dispatcher) Sweep(ctx context.Context) error {
	cutoff := d.Clock.Now().Add(-d.RunningTTL)
	pending, err := d.Store.ListNonTerminal(ctx, cutoff)
	if err != nil {
		return err
	}
	for _, p := range pending {
		if err := d.enqueueRecovered(ctx, item{id: p.ID, attempt: p.ProcessAttempts}); err != nil {
			return err
		}
	}
	return nil
}
