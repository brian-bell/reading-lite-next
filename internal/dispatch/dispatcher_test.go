package dispatch_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bbell/reading-lite/internal/clock"
	"github.com/bbell/reading-lite/internal/dispatch"
	"github.com/bbell/reading-lite/internal/reading"
	"github.com/bbell/reading-lite/internal/store"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// recordingHandler scripts a pipeline handler by call index and records the ids
// it ran, so tests can assert call counts and re-dispatch behavior.
type recordingHandler struct {
	mu    sync.Mutex
	calls int
	ids   []string
	fn    func(call int, id string) dispatch.Result
}

func (h *recordingHandler) handle(_ context.Context, id string) dispatch.Result {
	h.mu.Lock()
	call := h.calls
	h.calls++
	h.ids = append(h.ids, id)
	fn := h.fn
	h.mu.Unlock()
	return fn(call, id)
}

func (h *recordingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func (h *recordingHandler) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.ids)
}

func (h *recordingHandler) setFn(fn func(call int, id string) dispatch.Result) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fn = fn
}

func seedPending(t *testing.T, st *store.Memory, clk clock.Clock, id string, attempts int) {
	t.Helper()
	now := clk.Now()
	r := reading.Reading{
		ID:              id,
		URL:             "https://example.com/" + id,
		URLKey:          "key-" + id,
		Status:          reading.Pending,
		SourceKind:      reading.SourceWeb,
		ProcessAttempts: attempts,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := st.SaveReading(context.Background(), r); err != nil {
		t.Fatalf("seed %q: %v", id, err)
	}
}

func seedRunning(t *testing.T, st *store.Memory, id string, startedAt time.Time) {
	t.Helper()
	r := reading.Reading{
		ID:         id,
		URL:        "https://example.com/" + id,
		URLKey:     "key-" + id,
		Status:     reading.Running,
		SourceKind: reading.SourceWeb,
		StartedAt:  &startedAt,
		CreatedAt:  startedAt,
		UpdatedAt:  startedAt,
	}
	if err := st.SaveReading(context.Background(), r); err != nil {
		t.Fatalf("seed running %q: %v", id, err)
	}
}

func seedTerminal(t *testing.T, st *store.Memory, clk clock.Clock, id string, status reading.Status) {
	t.Helper()
	now := clk.Now()
	r := reading.Reading{
		ID:         id,
		URL:        "https://example.com/" + id,
		URLKey:     "key-" + id,
		Status:     status,
		SourceKind: reading.SourceWeb,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := st.SaveReading(context.Background(), r); err != nil {
		t.Fatalf("seed terminal %q: %v", id, err)
	}
}

func getReading(t *testing.T, st *store.Memory, id string) reading.Reading {
	t.Helper()
	r, err := st.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetByID(%q): %v", id, err)
	}
	return r
}

func waitStatus(t *testing.T, st *store.Memory, id string, status reading.Status) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()

	for {
		got := getReading(t, st, id)
		if got.Status == status {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("status for %q = %q, want %q", id, got.Status, status)
		case <-tick.C:
		}
	}
}

func always(o dispatch.Outcome) func(int, string) dispatch.Result {
	return func(int, string) dispatch.Result { return dispatch.Result{Outcome: o} }
}

func TestDispatch_SubmitRunsHandlerOnce(t *testing.T) {
	t.Parallel()

	st := store.NewMemory()
	clk := clock.NewFake(epoch)
	seedPending(t, st, clk, "r1", 0)

	h := &recordingHandler{fn: always(dispatch.Done)}
	d := &dispatch.Dispatcher{Handler: h.handle, Store: st, Clock: clk, Delay: &dispatch.FakeDelayer{}, Max: 3, Inline: true}

	d.Submit("r1")

	if h.count() != 1 {
		t.Fatalf("handler calls = %d, want 1", h.count())
	}
	if got := getReading(t, st, "r1"); got.Status != reading.Ready {
		t.Fatalf("status = %q, want ready", got.Status)
	}
}

func TestDispatch_RetrySchedulesDelayedRedispatch(t *testing.T) {
	t.Parallel()

	st := store.NewMemory()
	clk := clock.NewFake(epoch)
	seedPending(t, st, clk, "r1", 0)

	delay := &dispatch.FakeDelayer{}
	h := &recordingHandler{fn: func(call int, _ string) dispatch.Result {
		if call == 0 {
			return dispatch.Result{Outcome: dispatch.Retry}
		}
		return dispatch.Result{Outcome: dispatch.Done}
	}}
	d := &dispatch.Dispatcher{Handler: h.handle, Store: st, Clock: clk, Delay: delay, Max: 3, Inline: true}

	d.Submit("r1")

	if delay.Total() != 1 {
		t.Fatalf("delay scheduled %d times, want 1", delay.Total())
	}
	// First transient retry backs off one second (the 1s,2s,4s… schedule is
	// pinned by TestBackoff_Schedule).
	if got := delay.Durations()[0]; got != 1*time.Second {
		t.Fatalf("first backoff = %v, want 1s", got)
	}
	if got := getReading(t, st, "r1"); got.Status != reading.Pending {
		t.Fatalf("status before redispatch = %q, want pending", got.Status)
	}

	delay.FireAll()

	if h.count() != 2 {
		t.Fatalf("handler calls = %d, want 2", h.count())
	}
	got := getReading(t, st, "r1")
	if got.Status != reading.Ready {
		t.Fatalf("status after redispatch = %q, want ready", got.Status)
	}
	if got.ProcessAttempts != 1 {
		t.Fatalf("process_attempts = %d, want 1", got.ProcessAttempts)
	}
}

func TestDispatch_RequeueDoesNotConsumeAttempt(t *testing.T) {
	t.Parallel()

	st := store.NewMemory()
	clk := clock.NewFake(epoch)
	seedPending(t, st, clk, "r1", 0)

	delay := &dispatch.FakeDelayer{}
	h := &recordingHandler{fn: func(call int, _ string) dispatch.Result {
		if call == 0 {
			return dispatch.Result{Outcome: dispatch.Requeue, After: 30 * time.Second}
		}
		return dispatch.Result{Outcome: dispatch.Done}
	}}
	d := &dispatch.Dispatcher{Handler: h.handle, Store: st, Clock: clk, Delay: delay, Max: 3, Inline: true}

	d.Submit("r1")

	if got := delay.Durations(); len(got) != 1 || got[0] != 30*time.Second {
		t.Fatalf("delay durations = %v, want [30s]", got)
	}
	got := getReading(t, st, "r1")
	if got.Status != reading.Pending {
		t.Fatalf("status after requeue = %q, want pending", got.Status)
	}
	if got.ProcessAttempts != 0 {
		t.Fatalf("process_attempts after requeue = %d, want 0", got.ProcessAttempts)
	}

	delay.FireAll()

	got = getReading(t, st, "r1")
	if got.Status != reading.Ready {
		t.Fatalf("status after redispatch = %q, want ready", got.Status)
	}
	if got.ProcessAttempts != 0 {
		t.Fatalf("process_attempts after redispatch = %d, want 0 (attempt not consumed)", got.ProcessAttempts)
	}
}

func TestDispatch_RetryExhaustionFailsRetryable(t *testing.T) {
	t.Parallel()

	st := store.NewMemory()
	clk := clock.NewFake(epoch)
	seedPending(t, st, clk, "r1", 0)

	delay := &dispatch.FakeDelayer{}
	h := &recordingHandler{fn: always(dispatch.Retry)}
	d := &dispatch.Dispatcher{Handler: h.handle, Store: st, Clock: clk, Delay: delay, Max: 3, Inline: true}

	d.Submit("r1")  // attempt 0 -> retry, redispatch at 1
	delay.FireAll() // attempt 1 -> retry, redispatch at 2
	delay.FireAll() // attempt 2 -> retry budget spent -> failed

	if h.count() != 3 {
		t.Fatalf("handler calls = %d, want 3", h.count())
	}
	got := getReading(t, st, "r1")
	if got.Status != reading.Failed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if got.ProcessAttempts != 2 {
		t.Fatalf("process_attempts = %d, want 2", got.ProcessAttempts)
	}
	// Even though the transient Retry carried no error of its own, the persisted
	// reason must explain the failure rather than be blank.
	if !strings.Contains(got.Error, "exhausted") {
		t.Fatalf("error = %q, want it to mention the exhausted retry budget", got.Error)
	}
	if delay.Total() != 2 {
		t.Fatalf("delay scheduled %d times, want 2 (no schedule on exhaustion)", delay.Total())
	}
	if delay.PendingLen() != 0 {
		t.Fatalf("pending delays = %d, want 0", delay.PendingLen())
	}

	// A failed reading stays reprocessable: submitting it again runs the pipeline.
	h.setFn(always(dispatch.Done))
	d.Submit("r1")

	if h.count() != 4 {
		t.Fatalf("handler calls after reprocess = %d, want 4", h.count())
	}
	if got := getReading(t, st, "r1"); got.Status != reading.Ready {
		t.Fatalf("status after reprocess = %q, want ready", got.Status)
	}
}

func TestDispatch_DefaultMaxAttempts(t *testing.T) {
	t.Parallel()

	st := store.NewMemory()
	clk := clock.NewFake(epoch)
	seedPending(t, st, clk, "r1", 0)

	delay := &dispatch.FakeDelayer{}
	h := &recordingHandler{fn: always(dispatch.Retry)}
	// Max left unset: a zero value must default rather than fail fast on first retry.
	d := &dispatch.Dispatcher{Handler: h.handle, Store: st, Clock: clk, Delay: delay, Inline: true}

	d.Submit("r1")
	for range 10 {
		if delay.PendingLen() == 0 {
			break
		}
		delay.FireAll()
	}

	if h.count() != 5 {
		t.Fatalf("handler calls = %d, want 5 (default max attempts)", h.count())
	}
	if got := getReading(t, st, "r1"); got.Status != reading.Failed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
}

func TestDispatch_ExhaustionMessageIncludesCause(t *testing.T) {
	t.Parallel()

	st := store.NewMemory()
	clk := clock.NewFake(epoch)
	seedPending(t, st, clk, "r1", 0)

	delay := &dispatch.FakeDelayer{}
	boom := errors.New("upstream 503")
	h := &recordingHandler{fn: func(int, string) dispatch.Result {
		return dispatch.Result{Outcome: dispatch.Retry, Err: boom}
	}}
	d := &dispatch.Dispatcher{Handler: h.handle, Store: st, Clock: clk, Delay: delay, Max: 3, Inline: true}

	d.Submit("r1")
	for range 5 {
		if delay.PendingLen() == 0 {
			break
		}
		delay.FireAll()
	}

	got := getReading(t, st, "r1")
	if got.Status != reading.Failed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	// The persisted reason names the spent budget (with the true attempt count) and
	// preserves the last underlying error.
	if !strings.Contains(got.Error, "exhausted after 3 attempts") {
		t.Fatalf("error = %q, want it to mention 'exhausted after 3 attempts'", got.Error)
	}
	if !strings.Contains(got.Error, boom.Error()) {
		t.Fatalf("error = %q, want it to include the underlying cause %q", got.Error, boom.Error())
	}
}

func TestDispatch_PermanentFailRecordsError(t *testing.T) {
	t.Parallel()

	st := store.NewMemory()
	clk := clock.NewFake(epoch)
	seedPending(t, st, clk, "r1", 0)

	delay := &dispatch.FakeDelayer{}
	cause := errors.New("reddit is not fetchable")
	h := &recordingHandler{fn: func(int, string) dispatch.Result {
		return dispatch.Result{Outcome: dispatch.Fail, Err: cause}
	}}
	d := &dispatch.Dispatcher{Handler: h.handle, Store: st, Clock: clk, Delay: delay, Max: 3, Inline: true}

	d.Submit("r1")

	got := getReading(t, st, "r1")
	if got.Status != reading.Failed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if got.Error != cause.Error() {
		t.Fatalf("error = %q, want %q", got.Error, cause.Error())
	}
	if delay.Total() != 0 {
		t.Fatalf("delay scheduled %d times, want 0 (permanent failure is immediate)", delay.Total())
	}
}

func TestDispatch_PermanentFailWithoutCauseHasReason(t *testing.T) {
	t.Parallel()

	st := store.NewMemory()
	clk := clock.NewFake(epoch)
	seedPending(t, st, clk, "r1", 0)

	h := &recordingHandler{fn: always(dispatch.Fail)} // Fail with no underlying error
	d := &dispatch.Dispatcher{Handler: h.handle, Store: st, Clock: clk, Delay: &dispatch.FakeDelayer{}, Max: 3, Inline: true}

	d.Submit("r1")

	got := getReading(t, st, "r1")
	if got.Status != reading.Failed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if got.Error != "processing failed" {
		t.Fatalf("error = %q, want a non-empty fallback reason", got.Error)
	}
}

func TestDispatch_AsyncDefaultsProcessSubmittedReading(t *testing.T) {
	t.Parallel()

	st := store.NewMemory()
	clk := clock.NewFake(epoch)
	seedPending(t, st, clk, "r1", 0)

	processed := make(chan struct{})
	h := &recordingHandler{fn: func(int, string) dispatch.Result {
		close(processed)
		return dispatch.Result{Outcome: dispatch.Done}
	}}
	// No Workers and no Buffer set: exercise the worker/buffer defaults.
	d := &dispatch.Dispatcher{Handler: h.handle, Store: st, Clock: clk, Delay: &dispatch.FakeDelayer{}, Max: 3}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()

	d.Submit("r1")
	select {
	case <-processed:
	case <-time.After(2 * time.Second):
		t.Fatal("submitted reading was not processed")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestRealDelayer_FiresCallback(t *testing.T) {
	t.Parallel()

	fired := make(chan struct{})
	dispatch.RealDelayer{}.After(time.Millisecond, func() { close(fired) })

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("RealDelayer.After did not fire the callback")
	}
}

func TestDispatch_GracefulDrain(t *testing.T) {
	t.Parallel()

	st := store.NewMemory()
	clk := clock.NewFake(epoch)
	seedPending(t, st, clk, "r1", 0)

	started := make(chan struct{})
	release := make(chan struct{})
	h := &recordingHandler{fn: func(int, string) dispatch.Result {
		started <- struct{}{}
		<-release
		return dispatch.Result{Outcome: dispatch.Done}
	}}
	d := &dispatch.Dispatcher{Handler: h.handle, Store: st, Clock: clk, Delay: &dispatch.FakeDelayer{}, Workers: 1, Max: 3, Buffer: 8}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()

	d.Submit("r1")
	<-started // handler is in-flight
	cancel()  // request graceful drain while in-flight
	close(release)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not drain and return after ctx cancel")
	}

	if got := getReading(t, st, "r1"); got.Status != reading.Ready {
		t.Fatalf("in-flight reading status = %q, want ready (must finish during drain)", got.Status)
	}
}

func TestDispatch_DrainStopsPullingQueuedWork(t *testing.T) {
	t.Parallel()

	st := store.NewMemory()
	clk := clock.NewFake(epoch)
	for _, id := range []string{"r1", "r2", "r3"} {
		seedPending(t, st, clk, id, 0)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	h := &recordingHandler{fn: func(int, string) dispatch.Result {
		started <- struct{}{}
		<-release
		return dispatch.Result{Outcome: dispatch.Done}
	}}
	d := &dispatch.Dispatcher{Handler: h.handle, Store: st, Clock: clk, Delay: &dispatch.FakeDelayer{}, Workers: 1, Max: 3, Buffer: 8}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()

	d.Submit("r1")
	d.Submit("r2")
	d.Submit("r3")

	<-started      // the single worker is in-flight on r1; r2 and r3 are queued
	cancel()       // request a drain
	close(release) // let the in-flight run finish

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not drain and return after ctx cancel")
	}

	// Only the in-flight item ran; queued ids were left pending for the sweep.
	if got := h.count(); got != 1 {
		t.Fatalf("handler calls = %d, want 1 (drain must not pull queued work)", got)
	}
	for _, id := range []string{"r2", "r3"} {
		if got := getReading(t, st, id); got.Status != reading.Pending {
			t.Fatalf("queued %q status = %q, want pending (left for the sweep)", id, got.Status)
		}
	}
}

func TestDispatch_ConcurrencyBounded(t *testing.T) {
	t.Parallel()

	st := store.NewMemory()
	clk := clock.NewFake(epoch)
	ids := []string{"r1", "r2", "r3", "r4"}
	for _, id := range ids {
		seedPending(t, st, clk, id, 0)
	}

	var concurrent, maxConcurrent atomic.Int64
	entered := make(chan struct{}, len(ids))
	release := make(chan struct{})
	h := &recordingHandler{fn: func(int, string) dispatch.Result {
		cur := concurrent.Add(1)
		for {
			m := maxConcurrent.Load()
			if cur <= m || maxConcurrent.CompareAndSwap(m, cur) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		concurrent.Add(-1)
		return dispatch.Result{Outcome: dispatch.Done}
	}}
	d := &dispatch.Dispatcher{Handler: h.handle, Store: st, Clock: clk, Delay: &dispatch.FakeDelayer{}, Workers: 2, Max: 3, Buffer: 8}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()

	for _, id := range ids {
		d.Submit(id)
	}

	// Exactly two workers, so only two handlers can be in-flight at once.
	<-entered
	<-entered
	close(release)
	<-entered
	<-entered

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	if got := maxConcurrent.Load(); got != 2 {
		t.Fatalf("max concurrent handlers = %d, want 2", got)
	}
}

func TestDispatch_DuplicateIdNotProcessedConcurrently(t *testing.T) {
	t.Parallel()

	st := store.NewMemory()
	clk := clock.NewFake(epoch)
	seedPending(t, st, clk, "r1", 0)

	var concurrent, maxConcurrent atomic.Int64
	entered := make(chan struct{}, 4)
	gate := make(chan struct{})
	h := &recordingHandler{fn: func(int, string) dispatch.Result {
		cur := concurrent.Add(1)
		for {
			m := maxConcurrent.Load()
			if cur <= m || maxConcurrent.CompareAndSwap(m, cur) {
				break
			}
		}
		entered <- struct{}{}
		<-gate
		concurrent.Add(-1)
		return dispatch.Result{Outcome: dispatch.Done}
	}}
	d := &dispatch.Dispatcher{Handler: h.handle, Store: st, Clock: clk, Delay: &dispatch.FakeDelayer{}, Workers: 2, Max: 3, Buffer: 8}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()

	// Enqueue the same id twice; with two workers an unguarded handle would run it
	// concurrently and let a losing attempt overwrite the winner's terminal status.
	d.Submit("r1")
	d.Submit("r1")

	<-entered   // one handler holds the id
	close(gate) // release all handlers (closed channel never blocks)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	if got := maxConcurrent.Load(); got != 1 {
		t.Fatalf("max concurrent handlers for one id = %d, want 1 (claim must serialize)", got)
	}
	if got := getReading(t, st, "r1"); got.Status != reading.Ready {
		t.Fatalf("status = %q, want ready", got.Status)
	}
}

func TestDispatch_BufferedDuplicateRunsOnce(t *testing.T) {
	t.Parallel()

	st := store.NewMemory()
	clk := clock.NewFake(epoch)
	seedPending(t, st, clk, "r1", 0)

	started := make(chan struct{}, 4)
	proceed := make(chan struct{})
	h := &recordingHandler{fn: func(int, string) dispatch.Result {
		started <- struct{}{}
		<-proceed
		return dispatch.Result{Outcome: dispatch.Done}
	}}
	d := &dispatch.Dispatcher{Handler: h.handle, Store: st, Clock: clk, Delay: &dispatch.FakeDelayer{}, Workers: 1, Max: 3, Buffer: 8}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()

	// The same id is submitted twice before it is drained. Ownership is held from
	// the first enqueue, so the second submit is dropped — otherwise the buffered
	// duplicate would reprocess after the first run released the id and could
	// overwrite its terminal status.
	d.Submit("r1")
	d.Submit("r1")

	<-started      // the single run is in flight, holding the id
	close(proceed) // let it finish -> ready -> release

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	if got := h.count(); got != 1 {
		t.Fatalf("handler ran %d times for a buffered duplicate submit, want 1", got)
	}
	if got := getReading(t, st, "r1"); got.Status != reading.Ready {
		t.Fatalf("status = %q, want ready", got.Status)
	}
}

func TestDispatch_ForceSubmitRequeuesInFlightID(t *testing.T) {
	t.Parallel()

	st := store.NewMemory()
	clk := clock.NewFake(epoch)
	seedPending(t, st, clk, "r1", 0)

	entered := make(chan int, 2)
	firstProceed := make(chan struct{})
	h := &recordingHandler{fn: func(call int, _ string) dispatch.Result {
		entered <- call
		if call == 0 {
			<-firstProceed
			return dispatch.Result{Outcome: dispatch.Fail}
		}
		return dispatch.Result{Outcome: dispatch.Done}
	}}
	d := &dispatch.Dispatcher{Handler: h.handle, Store: st, Clock: clk, Delay: &dispatch.FakeDelayer{}, Workers: 2, Max: 3, Buffer: 8}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()

	d.Submit("r1")
	if got := <-entered; got != 0 {
		t.Fatalf("first handler call = %d, want 0", got)
	}

	d.ForceSubmit("r1")
	if got := <-entered; got != 1 {
		t.Fatalf("forced handler call = %d, want 1", got)
	}
	waitStatus(t, st, "r1", reading.Ready)
	close(firstProceed)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	if got := h.count(); got != 2 {
		t.Fatalf("handler ran %d times, want forced replacement run", got)
	}
	if got := getReading(t, st, "r1"); got.Status != reading.Ready {
		t.Fatalf("status = %q, want replacement ready to survive stale failure", got.Status)
	}
}

func TestDispatch_ForceSubmitAfterFailureDoesNotQueueAndReleasesClaim(t *testing.T) {
	t.Parallel()

	st := store.NewMemory()
	clk := clock.NewFake(epoch)
	seedPending(t, st, clk, "r1", 0)

	h := &recordingHandler{fn: always(dispatch.Done)}
	d := &dispatch.Dispatcher{Handler: h.handle, Store: st, Clock: clk, Delay: &dispatch.FakeDelayer{}, Max: 3, Inline: true}
	beforeErr := errors.New("store reset failed")

	if err := d.ForceSubmitAfter("r1", func() error { return beforeErr }); !errors.Is(err, beforeErr) {
		t.Fatalf("ForceSubmitAfter error = %v, want %v", err, beforeErr)
	}
	if got := h.count(); got != 0 {
		t.Fatalf("handler calls after failed force = %d, want 0", got)
	}

	d.Submit("r1")

	if got := h.count(); got != 1 {
		t.Fatalf("handler calls after retry submit = %d, want 1 (failed force must release claim)", got)
	}
	if got := getReading(t, st, "r1"); got.Status != reading.Ready {
		t.Fatalf("status = %q, want ready after later submit", got.Status)
	}
}

func TestDispatch_OldRetryContinuationCannotRequeueAfterForce(t *testing.T) {
	t.Parallel()

	st := store.NewMemory()
	clk := clock.NewFake(epoch)
	seedPending(t, st, clk, "r1", 0)

	delay := &dispatch.FakeDelayer{}
	h := &recordingHandler{fn: func(call int, _ string) dispatch.Result {
		if call == 0 {
			return dispatch.Result{Outcome: dispatch.Retry}
		}
		return dispatch.Result{Outcome: dispatch.Done}
	}}
	d := &dispatch.Dispatcher{Handler: h.handle, Store: st, Clock: clk, Delay: delay, Max: 3, Inline: true}

	d.Submit("r1")
	if got := h.count(); got != 1 {
		t.Fatalf("handler calls after retrying submit = %d, want 1", got)
	}
	if delay.PendingLen() != 1 {
		t.Fatalf("pending delays = %d, want 1", delay.PendingLen())
	}

	d.ForceSubmit("r1")
	if got := h.count(); got != 2 {
		t.Fatalf("handler calls after force = %d, want replacement run", got)
	}
	waitStatus(t, st, "r1", reading.Ready)

	delay.FireAll()

	if got := h.count(); got != 2 {
		t.Fatalf("handler calls after firing old retry = %d, want old continuation ignored", got)
	}
	if got := getReading(t, st, "r1"); got.Status != reading.Ready {
		t.Fatalf("status = %q, want replacement ready to survive old retry", got.Status)
	}
}

func TestDispatch_OldRateLimitContinuationCannotRequeueAfterForce(t *testing.T) {
	t.Parallel()

	st := store.NewMemory()
	clk := clock.NewFake(epoch)
	seedPending(t, st, clk, "r1", 0)

	delay := &dispatch.FakeDelayer{}
	h := &recordingHandler{fn: func(call int, _ string) dispatch.Result {
		if call == 0 {
			return dispatch.Result{Outcome: dispatch.Retry, Err: &dispatch.RateLimitError{RetryAfter: time.Minute}}
		}
		return dispatch.Result{Outcome: dispatch.Done}
	}}
	d := &dispatch.Dispatcher{Handler: h.handle, Store: st, Clock: clk, Delay: delay, Max: 3, Inline: true}

	d.Submit("r1")
	if delay.PendingLen() != 1 {
		t.Fatalf("pending delays = %d, want 1", delay.PendingLen())
	}

	d.ForceSubmit("r1")
	if got := h.count(); got != 2 {
		t.Fatalf("handler calls after force = %d, want replacement run", got)
	}
	waitStatus(t, st, "r1", reading.Ready)

	delay.FireAll()

	if got := h.count(); got != 2 {
		t.Fatalf("handler calls after firing old rate-limit delay = %d, want old continuation ignored", got)
	}
	if got := getReading(t, st, "r1"); got.Status != reading.Ready {
		t.Fatalf("status = %q, want replacement ready to survive old rate-limit delay", got.Status)
	}
}

func TestDispatch_RecoverySweepReenqueuesNonTerminal(t *testing.T) {
	t.Parallel()

	st := store.NewMemory()
	clk := clock.NewFake(epoch)
	runningTTL := 30 * time.Minute

	seedPending(t, st, clk, "r-pending", 0)
	seedRunning(t, st, "r-running-stale", clk.Now().Add(-31*time.Minute))
	seedRunning(t, st, "r-running-fresh", clk.Now().Add(-5*time.Minute))
	seedTerminal(t, st, clk, "r-ready", reading.Ready)
	seedTerminal(t, st, clk, "r-failed", reading.Failed)

	h := &recordingHandler{fn: always(dispatch.Done)}
	d := &dispatch.Dispatcher{Handler: h.handle, Store: st, Clock: clk, Delay: &dispatch.FakeDelayer{}, Max: 3, RunningTTL: runningTTL, Inline: true}

	if err := d.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	got := h.seen()
	slices.Sort(got)
	want := []string{"r-pending", "r-running-stale"}
	if !slices.Equal(got, want) {
		t.Fatalf("swept ids = %v, want %v", got, want)
	}
}

// listErrStore is a dispatch.Store whose ListNonTerminal always fails, for the
// Sweep error path.
type listErrStore struct {
	err error
}

func (s listErrStore) UpdateStatus(context.Context, string, reading.Status, store.StatusFields) error {
	return nil
}

func (s listErrStore) ListNonTerminal(context.Context, time.Time) ([]store.Pending, error) {
	return nil, s.err
}

func TestDispatch_SweepPropagatesListError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("database unavailable")
	d := &dispatch.Dispatcher{
		Store:      listErrStore{err: wantErr},
		Clock:      clock.NewFake(epoch),
		Delay:      &dispatch.FakeDelayer{},
		RunningTTL: time.Minute,
	}

	if err := d.Sweep(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Sweep error = %v, want %v", err, wantErr)
	}
}

func TestDispatch_SweepRecoversBacklogWithoutDropping(t *testing.T) {
	t.Parallel()

	st := store.NewMemory()
	clk := clock.NewFake(epoch)
	const n = 12
	for i := range n {
		seedPending(t, st, clk, fmt.Sprintf("r%02d", i), 0)
	}

	var mu sync.Mutex
	seen := map[string]bool{}
	processed := make(chan struct{}, n)
	h := &recordingHandler{fn: func(_ int, id string) dispatch.Result {
		mu.Lock()
		seen[id] = true
		mu.Unlock()
		processed <- struct{}{}
		return dispatch.Result{Outcome: dispatch.Done}
	}}
	// Buffer is smaller than the backlog: a lossy enqueue would drop the overflow,
	// but recovery must re-dispatch every non-terminal reading.
	d := &dispatch.Dispatcher{Handler: h.handle, Store: st, Clock: clk, Delay: &dispatch.FakeDelayer{}, Workers: 2, Max: 3, Buffer: 4, RunningTTL: time.Minute}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	if err := d.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	for range n {
		select {
		case <-processed:
		case <-time.After(2 * time.Second):
			t.Fatal("not all backlog readings were processed; recovery dropped work")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != n {
		t.Fatalf("processed %d distinct readings, want %d", len(seen), n)
	}
}

func TestDispatch_SweepStopsOnCanceledContext(t *testing.T) {
	t.Parallel()

	// In both modes a cancelled sweep must abort and process nothing, even when
	// the whole backlog would fit in the buffer (so only the context check, not a
	// full channel, can stop it).
	for _, inline := range []bool{true, false} {
		name := "async"
		if inline {
			name = "inline"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			st := store.NewMemory()
			clk := clock.NewFake(epoch)
			for i := range 3 {
				seedPending(t, st, clk, fmt.Sprintf("r%02d", i), 0)
			}
			h := &recordingHandler{fn: always(dispatch.Done)}
			d := &dispatch.Dispatcher{
				Handler:    h.handle,
				Store:      st,
				Clock:      clk,
				Delay:      &dispatch.FakeDelayer{},
				Max:        3,
				Buffer:     16, // comfortably fits the backlog
				RunningTTL: time.Minute,
				Inline:     inline,
			}

			ctx, cancel := context.WithCancel(context.Background())
			cancel() // already cancelled

			if err := d.Sweep(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("Sweep error = %v, want context.Canceled", err)
			}
			if got := h.count(); got != 0 {
				t.Fatalf("handler calls = %d, want 0 (cancelled sweep must not process)", got)
			}
		})
	}
}

func TestDispatch_SweepResumesAtStoredAttempt(t *testing.T) {
	t.Parallel()

	st := store.NewMemory()
	clk := clock.NewFake(epoch)
	seedPending(t, st, clk, "r1", 2) // process_attempts already at 2

	delay := &dispatch.FakeDelayer{}
	h := &recordingHandler{fn: always(dispatch.Retry)}
	d := &dispatch.Dispatcher{Handler: h.handle, Store: st, Clock: clk, Delay: delay, Max: 3, Inline: true}

	if err := d.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	// Resumed at attempt 2, so one more Retry exhausts the budget immediately:
	// failed with no further re-dispatch. Resuming at 0 would instead re-dispatch.
	got := getReading(t, st, "r1")
	if got.Status != reading.Failed {
		t.Fatalf("status = %q, want failed (must resume at stored attempt 2)", got.Status)
	}
	if got.ProcessAttempts != 2 {
		t.Fatalf("process_attempts = %d, want 2", got.ProcessAttempts)
	}
	if delay.Total() != 0 {
		t.Fatalf("delay scheduled %d times, want 0", delay.Total())
	}
}
