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
	// Inline runs the initial handle synchronously on Submit/Sweep instead of via
	// the worker channel, for deterministic handler tests and the HTTP layer. Note
	// that re-dispatch after a retry or rate limit still flows through the injected
	// Delay seam, so pair Inline with a Delayer you control (e.g. FakeDelayer) to
	// keep that path deterministic too; the production topology uses Run instead.
	Inline bool

	chOnce sync.Once
	ch     chan item

	inflightMu sync.Mutex
	inflight   map[string]bool
}

// claim takes single-process ownership of a reading id for the duration of one
// handle. It returns false when another worker is already processing the id, so a
// duplicate dispatch (a live Submit racing a re-dispatch or a recovery sweep)
// cannot run the pipeline twice and overwrite the winner's terminal status with a
// losing attempt's. This single-instance guard matches the process topology; a
// multi-instance deployment would need a store-level claim instead.
func (d *Dispatcher) claim(id string) bool {
	d.inflightMu.Lock()
	defer d.inflightMu.Unlock()

	if d.inflight[id] {
		return false
	}
	if d.inflight == nil {
		d.inflight = map[string]bool{}
	}
	d.inflight[id] = true
	return true
}

func (d *Dispatcher) release(id string) {
	d.inflightMu.Lock()
	defer d.inflightMu.Unlock()

	delete(d.inflight, id)
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

// Submit enqueues id for processing at attempt 0. It is non-blocking: if the
// channel is full the item is dropped (see enqueue) rather than blocking ingest.
func (d *Dispatcher) Submit(id string) {
	d.enqueue(item{id: id})
}

// enqueue is the lossy path used by live ingest (Submit) and re-dispatch: a full
// channel drops the item rather than blocking. A dropped reading stays pending and
// is recovered by the startup Sweep (see enqueueRecovered), backstopped meanwhile
// by read-time stale annotation plus on-demand reprocess (PLAN.md §1.4).
func (d *Dispatcher) enqueue(it item) {
	if d.Inline {
		// A re-dispatch is a fresh unit of work, decoupled from any originating
		// request context; the async worker pool (Run) scopes work to the run
		// context instead, and inline is the test/handler seam.
		d.handle(context.Background(), it)
		return
	}

	d.ensureCh()
	select {
	case d.ch <- it:
	default:
		// Buffer full: drop. The default buffer is sized so drops are a rare
		// overload backstop on the live path, not a normal occurrence.
	}
}

// enqueueRecovered is the non-lossy path used by Sweep: recovery must not drop, so
// it blocks until a worker accepts the item or ctx is cancelled. Callers therefore
// run the worker pool concurrently (or pass a bounded ctx) to avoid blocking.
func (d *Dispatcher) enqueueRecovered(ctx context.Context, it item) error {
	// Honor cancellation deterministically: a select with both a ready send and a
	// ready ctx.Done picks at random, so check the context first — otherwise a
	// cancelled sweep could still enqueue (and report success) when the backlog
	// fits in the buffer. The inline path runs under the sweep context too.
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.Inline {
		d.handle(ctx, it)
		return nil
	}

	d.ensureCh()
	select {
	case <-ctx.Done():
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
					// this id for the sweep rather than starting new work.
					select {
					case <-ctx.Done():
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

// handle runs one item end to end: mark the reading running (mirroring the
// attempt onto process_attempts), run Handler, then apply decide and persist the
// outcome — ready on success, pending plus a scheduled re-dispatch on retry or
// rate limit, failed when the retry budget is spent or the error is permanent.
// It is the only method that touches the Store and the Delayer.
func (d *Dispatcher) handle(ctx context.Context, it item) {
	if !d.claim(it.id) {
		// Another worker already owns this id; drop the duplicate rather than
		// processing it concurrently. The owner persists the terminal outcome.
		return
	}
	defer d.release(it.id)

	now := d.Clock.Now()
	attempt := it.attempt
	if err := d.Store.UpdateStatus(ctx, it.id, reading.Running, store.StatusFields{
		Now:             now,
		StartedAt:       &now,
		ProcessAttempts: &attempt,
		ClearError:      true,
	}); err != nil {
		return
	}

	maxAttempts := d.Max
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}

	res := d.Handler(ctx, it.id)
	act := decide(res, attempt, maxAttempts)

	// The terminal/redispatch writes below are best-effort: their error is
	// discarded because there is no logger yet (structured logging lands in a
	// later phase). A dropped final write leaves the reading running or pending,
	// which the stale-running/pending sweep and read-time annotation recover.
	switch {
	case act.MarkFailed:
		d.markFailed(ctx, it.id, attempt, res)
	case act.Redispatch:
		d.redispatch(ctx, it.id, act)
	default:
		d.markReady(ctx, it.id)
	}
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

func (d *Dispatcher) redispatch(ctx context.Context, id string, act Action) {
	now := d.Clock.Now()
	next := act.NextAttempt
	_ = d.Store.UpdateStatus(ctx, id, reading.Pending, store.StatusFields{
		Now:             now,
		ProcessAttempts: &next,
	})
	d.Delay.After(act.Delay, func() {
		d.enqueue(item{id: id, attempt: act.NextAttempt})
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
