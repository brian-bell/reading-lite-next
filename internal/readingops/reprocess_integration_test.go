package readingops_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bbell/reading-lite/internal/blobs"
	"github.com/bbell/reading-lite/internal/clock"
	"github.com/bbell/reading-lite/internal/dispatch"
	"github.com/bbell/reading-lite/internal/reading"
	"github.com/bbell/reading-lite/internal/readingops"
	"github.com/bbell/reading-lite/internal/store"
)

// TestReprocess_StaleRunningRecoversDespiteStuckHandler is the cross-package
// proof of the PR #8 fix: an operator reprocess of a stale-running reading must
// complete even when the in-flight handler is stuck in a non-cancelable call.
// It wires a real readingops.Service to a real *dispatch.Dispatcher (a fake
// submitter is synchronous and cannot reproduce the unbounded wait) and drives
// the dispatcher's ForceWaitTTL budget through the FakeDelayer.
func TestReprocess_StaleRunningRecoversDespiteStuckHandler(t *testing.T) {
	t.Parallel()

	st := store.NewMemory()
	clk := clock.NewFake(testNow)
	delay := &dispatch.FakeDelayer{}

	entered := make(chan int, 2)
	stuck := make(chan struct{}) // call 0 ignores ctx and blocks (non-cancelable)
	var mu sync.Mutex
	calls := 0
	handler := func(_ context.Context, _ string) dispatch.Result {
		mu.Lock()
		call := calls
		calls++
		mu.Unlock()
		entered <- call
		if call == 0 {
			<-stuck
		}
		return dispatch.Result{Outcome: dispatch.Done}
	}
	d := &dispatch.Dispatcher{Handler: handler, Store: st, Clock: clk, Delay: delay, Workers: 2, Max: 3, Buffer: 8}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(runDone)
	}()
	defer func() {
		cancel()
		<-runDone
	}()

	svc := &readingops.Service{
		Store:      st,
		Blobs:      blobs.NewMemory(),
		Dispatcher: d,
		Clock:      clk,
		TTLs:       reading.TTLs{Pending: 5 * time.Minute, Running: 5 * time.Minute},
		NewID:      func() string { return "unused" },
	}

	// Seed a pending web reading and submit it through the real dispatcher so a
	// handler parks (claimed + Running with StartedAt=testNow). A bare store row
	// is not a dispatcher claim, so the force path would have nothing to drain.
	if err := st.SaveReading(context.Background(), reading.Reading{
		ID: "r1", URL: "https://example.com/r1", URLKey: "key-r1",
		Status: reading.Pending, SourceKind: reading.SourceWeb,
		CreatedAt: testNow, UpdatedAt: testNow,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	d.Submit("r1")
	if got := <-entered; got != 0 {
		t.Fatalf("first handler call = %d, want 0", got)
	}

	// Make the running row stale so Reprocess takes the force path (AnnotateStale
	// marks it failed once now-StartedAt exceeds TTLs.Running).
	clk.Advance(6 * time.Minute)

	type result struct {
		res readingops.StatusResult
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		res, err := svc.Reprocess(context.Background(), "r1")
		resCh <- result{res, err}
	}()

	// Reprocess → ForceSubmitAfter blocks in the bounded wait until the budget
	// fires; the stuck call 0 schedules no redispatch, so PendingLen()==1 is the
	// force budget.
	waitForBudget(t, delay)
	delay.FireAll()

	select {
	case got := <-resCh:
		if got.err != nil {
			t.Fatalf("Reprocess hung/failed despite stuck handler: %v", got.err)
		}
		if got.res.Status != reading.Pending {
			t.Fatalf("Reprocess status = %q, want pending", got.res.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Reprocess did not return — bounded wait regressed to unbounded")
	}

	// The replacement run executes off the free worker and reaches ready, proving
	// a fresh run actually ran (not just that Reprocess returned).
	if got := <-entered; got != 1 {
		t.Fatalf("replacement handler call = %d, want 1", got)
	}
	waitReady(t, st, "r1")

	close(stuck) // release the stale handler for clean shutdown
}

// waitForBudget spins until the force-recovery budget is the single pending
// delay on the FakeDelayer (the rendezvous proving ForceSubmitAfter reached the
// bounded-wait select).
func waitForBudget(t *testing.T, delay *dispatch.FakeDelayer) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if delay.PendingLen() == 1 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("force budget was never scheduled")
		case <-tick.C:
		}
	}
}

func waitReady(t *testing.T, st *store.Memory, id string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		r, err := st.GetByID(context.Background(), id)
		if err != nil {
			t.Fatalf("GetByID(%q): %v", id, err)
		}
		if r.Status == reading.Ready {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("status for %q = %q, want ready", id, r.Status)
		case <-tick.C:
		}
	}
}
