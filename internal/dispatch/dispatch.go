// Package dispatch runs the processing pipeline asynchronously with retry,
// backoff, rate-limit re-dispatch, and crash recovery.
//
// The decision logic ([decide]) and the error classifier ([Classify]) are pure
// functions so the dispatcher's retry semantics are tested without real
// goroutines, timers, or a wall clock. The dispatcher's only impure seams are an
// injected [clock.Clock], an injected [Delayer], and a [Store].
package dispatch

import (
	"errors"
	"time"
)

// Outcome is the classified result of one pipeline run.
type Outcome int

const (
	// Done means the run succeeded; the reading becomes ready.
	Done Outcome = iota
	// Retry means a transient error occurred; re-dispatch after backoff and consume an attempt.
	Retry
	// Requeue means the upstream is rate limited; re-dispatch after a delay without consuming an attempt.
	Requeue
	// Fail means a permanent error occurred; the reading fails immediately.
	Fail
)

// Result is what a pipeline run reports back to the dispatcher.
type Result struct {
	// Outcome classifies the run.
	Outcome Outcome
	// After is the requeue delay for a rate-limited [Requeue].
	After time.Duration
	// Err is the underlying error for [Retry], [Requeue], and [Fail].
	Err error
}

// item is the unit of work that rides the in-memory dispatch channel.
type item struct {
	id      string
	attempt int
}

// Action is the pure decision for what to do after a pipeline run.
type Action struct {
	// Redispatch reports whether the item should be re-enqueued after Delay.
	Redispatch bool
	// Delay is how long to wait before re-dispatching.
	Delay time.Duration
	// NextAttempt is the attempt number to carry on the re-dispatched item.
	NextAttempt int
	// MarkFailed reports whether the reading should be written failed now.
	MarkFailed bool
}

// ErrPermanent marks an error as non-retryable: the reading fails immediately
// without consuming the retry budget on transient errors.
var ErrPermanent = errors.New("permanent")

// DefaultRateLimitDelay is the requeue delay used when a rate-limited upstream
// gives no usable Retry-After. A [Requeue] does not consume an attempt, so a
// zero delay would re-dispatch immediately and spin a worker forever on a source
// that always rate-limits without a usable header; this bounds it to a gentle
// retry. It matches the transient-retry backoff ceiling.
const DefaultRateLimitDelay = backoffCap

// RateLimitError signals the upstream is rate limited and the work should be
// re-dispatched after RetryAfter without consuming an attempt.
type RateLimitError struct {
	// RetryAfter is how long to wait before re-dispatching.
	RetryAfter time.Duration
	// Err is the optional underlying error.
	Err error
}

// Error implements error.
func (e *RateLimitError) Error() string {
	if e.Err != nil {
		return "rate limited: " + e.Err.Error()
	}
	return "rate limited"
}

// Unwrap exposes the underlying error.
func (e *RateLimitError) Unwrap() error { return e.Err }

// Classify maps a pipeline error to a [Result]. It is shared by the pipeline so
// the dispatcher and the pipeline agree on what each error means.
func Classify(err error) Result {
	if err == nil {
		return Result{Outcome: Done}
	}

	var rl *RateLimitError
	if errors.As(err, &rl) {
		return Result{Outcome: Requeue, After: rl.RetryAfter, Err: err}
	}
	if errors.Is(err, ErrPermanent) {
		return Result{Outcome: Fail, Err: err}
	}
	return Result{Outcome: Retry, Err: err}
}

const (
	backoffBase = 1 * time.Second
	backoffCap  = 30 * time.Second
)

// backoff returns the exponential re-dispatch delay for a transient retry:
// 1s, 2s, 4s, 8s, 16s, … capped at backoffCap.
func backoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	d := backoffBase << attempt
	if d <= 0 || d > backoffCap {
		return backoffCap
	}
	return d
}

// decide is pure: given a run result and where we are in the retry budget, it
// reports what happens next. It is the single branch point for retry semantics.
func decide(r Result, attempt, maxAttempts int) Action {
	switch r.Outcome {
	case Requeue:
		// Rate limited: re-dispatch after the upstream's delay, attempt unchanged.
		return Action{Redispatch: true, Delay: r.After, NextAttempt: attempt}
	case Retry:
		if attempt+1 >= maxAttempts {
			// Retry budget spent: fail (still reprocessable on demand).
			return Action{MarkFailed: true}
		}
		return Action{Redispatch: true, Delay: backoff(attempt), NextAttempt: attempt + 1}
	case Fail:
		return Action{MarkFailed: true}
	default: // Done
		return Action{}
	}
}
