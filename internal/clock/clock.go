// Package clock defines time ports and deterministic test clocks.
package clock

import (
	"sync"
	"time"
)

// Clock reports the current time.
type Clock interface {
	Now() time.Time
}

// System reads time from the process clock.
type System struct{}

// Now returns the current wall-clock time.
func (System) Now() time.Time {
	return time.Now()
}

// Fake is a deterministic, concurrency-safe clock for tests.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// NewFake returns a fake clock initialized to now.
func NewFake(now time.Time) *Fake {
	return &Fake{now: now}
}

// Now returns the fake clock's current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.now
}

// Advance moves the fake clock forward by d.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.now = f.now.Add(d)
}

// Set moves the fake clock to now.
func (f *Fake) Set(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.now = now
}
