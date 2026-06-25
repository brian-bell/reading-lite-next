package dispatch

import (
	"testing"
	"time"
)

func TestBackoff_Schedule(t *testing.T) {
	t.Parallel()

	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 1 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 30 * time.Second},
		{6, 30 * time.Second},
		{100, 30 * time.Second},
		{-1, 1 * time.Second},
	}

	for _, tc := range cases {
		if got := backoff(tc.attempt); got != tc.want {
			t.Errorf("backoff(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

func TestDecide_Done(t *testing.T) {
	t.Parallel()

	got := decide(Result{Outcome: Done}, 0, 3)
	want := Action{}
	if got != want {
		t.Fatalf("decide(Done) = %+v, want %+v", got, want)
	}
}

func TestDecide_RetryBackoff(t *testing.T) {
	t.Parallel()

	// max is large so every Retry redispatches rather than exhausting.
	for _, attempt := range []int{0, 1, 2, 3, 4, 5} {
		got := decide(Result{Outcome: Retry}, attempt, 100)
		want := Action{
			Redispatch:  true,
			Delay:       backoff(attempt),
			NextAttempt: attempt + 1,
		}
		if got != want {
			t.Errorf("decide(Retry, attempt=%d) = %+v, want %+v", attempt, got, want)
		}
	}
}

func TestDecide_RequeueKeepsAttempt(t *testing.T) {
	t.Parallel()

	got := decide(Result{Outcome: Requeue, After: 30 * time.Second}, 2, 5)
	want := Action{
		Redispatch:  true,
		Delay:       30 * time.Second,
		NextAttempt: 2, // unchanged: a rate limit does not consume an attempt
	}
	if got != want {
		t.Fatalf("decide(Requeue) = %+v, want %+v", got, want)
	}
}

func TestDecide_RetryExhaustion(t *testing.T) {
	t.Parallel()

	// attempt+1 >= max: the retry budget is spent, so the reading fails (retryable).
	got := decide(Result{Outcome: Retry}, 2, 3)
	want := Action{MarkFailed: true}
	if got != want {
		t.Fatalf("decide(Retry exhausted) = %+v, want %+v", got, want)
	}
}

func TestDecide_PermanentFailsFast(t *testing.T) {
	t.Parallel()

	got := decide(Result{Outcome: Fail}, 0, 5)
	want := Action{MarkFailed: true}
	if got != want {
		t.Fatalf("decide(Fail) = %+v, want %+v", got, want)
	}
}
