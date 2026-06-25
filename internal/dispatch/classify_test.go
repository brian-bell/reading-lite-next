package dispatch_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/bbell/reading-lite/internal/dispatch"
)

func TestClassify_NilIsDone(t *testing.T) {
	t.Parallel()

	if got := dispatch.Classify(nil); got.Outcome != dispatch.Done {
		t.Fatalf("Classify(nil).Outcome = %v, want Done", got.Outcome)
	}
}

func TestClassify_RateLimitRequeues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
	}{
		{"direct", &dispatch.RateLimitError{RetryAfter: 30 * time.Second}},
		{"wrapped", fmt.Errorf("embed: %w", &dispatch.RateLimitError{RetryAfter: 30 * time.Second})},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := dispatch.Classify(tc.err)
			if got.Outcome != dispatch.Requeue {
				t.Fatalf("Classify(%v).Outcome = %v, want Requeue", tc.err, got.Outcome)
			}
			if got.After != 30*time.Second {
				t.Fatalf("Classify(%v).After = %v, want 30s", tc.err, got.After)
			}
			if got.Err == nil {
				t.Fatalf("Classify(%v).Err = nil, want the original error", tc.err)
			}
		})
	}
}

func TestClassify_PermanentFails(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
	}{
		{"direct", dispatch.ErrPermanent},
		{"wrapped", fmt.Errorf("reddit source: %w", dispatch.ErrPermanent)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := dispatch.Classify(tc.err); got.Outcome != dispatch.Fail {
				t.Fatalf("Classify(%v).Outcome = %v, want Fail", tc.err, got.Outcome)
			}
		})
	}
}

func TestRateLimitError_ErrorAndUnwrap(t *testing.T) {
	t.Parallel()

	bare := &dispatch.RateLimitError{RetryAfter: time.Second}
	if got := bare.Error(); got != "rate limited" {
		t.Fatalf("bare.Error() = %q, want %q", got, "rate limited")
	}
	if bare.Unwrap() != nil {
		t.Fatalf("bare.Unwrap() = %v, want nil", bare.Unwrap())
	}

	cause := errors.New("429")
	wrapped := &dispatch.RateLimitError{RetryAfter: time.Second, Err: cause}
	if got := wrapped.Error(); got != "rate limited: 429" {
		t.Fatalf("wrapped.Error() = %q, want %q", got, "rate limited: 429")
	}
	if !errors.Is(wrapped, cause) {
		t.Fatalf("errors.Is(wrapped, cause) = false, want true")
	}
}

func TestClassify_OtherRetries(t *testing.T) {
	t.Parallel()

	err := errors.New("connection reset")
	got := dispatch.Classify(err)
	if got.Outcome != dispatch.Retry {
		t.Fatalf("Classify(%v).Outcome = %v, want Retry", err, got.Outcome)
	}
	if got.Err == nil {
		t.Fatalf("Classify(%v).Err = nil, want the original error", err)
	}
}
