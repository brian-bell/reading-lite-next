// Package notify defines the notification-email port and an in-memory fake.
//
// The production adapter is Resend (Phase 6); [Fake] records sent emails for
// tests. A notify failure never fails a reading (the pipeline swallows it), so
// the fake distinguishes attempted calls from successfully sent emails.
package notify

import (
	"context"
	"slices"
	"sync"
)

// Email is one notification message.
type Email struct {
	// From is the sender address.
	From string
	// To is the recipient address.
	To string
	// Subject is the email subject line.
	Subject string
	// HTML is the rendered email body.
	HTML string
}

// Notifier sends a notification email.
type Notifier interface {
	Notify(ctx context.Context, n Email) error
}

// Fake is a concurrency-safe [Notifier] that records sent emails. Set the scripted
// Err before first use (it is read under the lock but not meant to change once
// workers may call concurrently) to script a delivery failure.
type Fake struct {
	// Err, when non-nil, is returned instead of sending.
	Err error

	mu    sync.Mutex
	calls int
	sent  []Email
}

// Notify records the attempt and, on success, the sent email.
func (f *Fake) Notify(ctx context.Context, n Email) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	if f.Err != nil {
		return f.Err
	}
	f.sent = append(f.sent, n)
	return nil
}

// Calls is the number of times Notify was invoked, including failures.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}

// Sent returns every successfully sent email, in send order.
func (f *Fake) Sent() []Email {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.sent)
}
