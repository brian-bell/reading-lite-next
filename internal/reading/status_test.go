package reading_test

import (
	"testing"

	"github.com/bbell/reading-lite/internal/reading"
)

func TestStatus_Transitions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		from, to reading.Status
		wantOK   bool
	}{
		{"queue new", reading.Pending, reading.Running, true},
		{"complete", reading.Running, reading.Ready, true},
		{"fail running", reading.Running, reading.Failed, true},
		{"requeue rate-limited", reading.Running, reading.Pending, true},
		{"reprocess failed", reading.Failed, reading.Pending, true},
		{"reprocess ready", reading.Ready, reading.Pending, true},
		{"cannot ready->running", reading.Ready, reading.Running, false},
		{"cannot pending->ready", reading.Pending, reading.Ready, false},
		{"terminal->same noop rejected", reading.Failed, reading.Failed, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := reading.CanTransition(tc.from, tc.to); got != tc.wantOK {
				t.Fatalf("CanTransition(%q, %q) = %v, want %v", tc.from, tc.to, got, tc.wantOK)
			}
		})
	}
}

func TestStatus_IsTerminal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status reading.Status
		want   bool
	}{
		{reading.Pending, false},
		{reading.Running, false},
		{reading.Ready, true},
		{reading.Failed, true},
	}

	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			t.Parallel()

			if got := tc.status.IsTerminal(); got != tc.want {
				t.Fatalf("%q.IsTerminal() = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}
