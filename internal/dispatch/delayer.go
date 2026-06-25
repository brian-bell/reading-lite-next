package dispatch

import (
	"slices"
	"sync"
	"time"
)

// Delayer schedules fn to run after d. Production uses [RealDelayer]
// (time.AfterFunc); tests use [FakeDelayer] to fire pending delays on demand so
// retry/backoff semantics are exercised without sleeping.
type Delayer interface {
	After(d time.Duration, fn func())
}

// RealDelayer schedules callbacks on the process timer.
type RealDelayer struct{}

// After runs fn after d on a background timer.
func (RealDelayer) After(d time.Duration, fn func()) {
	time.AfterFunc(d, fn)
}

// FakeDelayer records scheduled delays instead of running them, so tests can
// assert what was scheduled and fire callbacks deterministically. It is safe for
// concurrent use.
type FakeDelayer struct {
	mu        sync.Mutex
	pending   []func()
	durations []time.Duration
	total     int
}

// After records d and fn without running fn.
func (f *FakeDelayer) After(d time.Duration, fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.pending = append(f.pending, fn)
	f.durations = append(f.durations, d)
	f.total++
}

// Total is the cumulative number of delays scheduled over the delayer's lifetime.
func (f *FakeDelayer) Total() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.total
}

// Durations returns every scheduled delay in schedule order, including ones
// already fired.
func (f *FakeDelayer) Durations() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.durations)
}

// PendingLen is the number of scheduled-but-not-yet-fired delays.
func (f *FakeDelayer) PendingLen() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.pending)
}

// FireAll runs every pending callback in schedule order and clears the pending
// queue. Callbacks scheduled while firing remain pending for the next FireAll.
func (f *FakeDelayer) FireAll() {
	f.mu.Lock()
	pending := f.pending
	f.pending = nil
	f.mu.Unlock()

	for _, fn := range pending {
		fn()
	}
}
