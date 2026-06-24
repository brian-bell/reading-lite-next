package reading_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bbell/reading-lite/internal/reading"
)

func TestAnnotateStale(t *testing.T) {
	t.Parallel()

	now := time.Unix(10_000, 0)
	cfg := reading.TTLs{
		Pending: 10 * time.Minute,
		Running: 30 * time.Minute,
	}
	started := func(t time.Time) *time.Time {
		return &t
	}
	mk := func(status reading.Status, createdAt time.Time) reading.Reading {
		return reading.Reading{
			ID:        "reading-1",
			Status:    status,
			CreatedAt: createdAt,
		}
	}
	mkRun := func(status reading.Status, startedAt time.Time) reading.Reading {
		r := mk(status, now.Add(-99*time.Hour))
		r.StartedAt = started(startedAt)
		return r
	}

	cases := []struct {
		name               string
		in                 reading.Reading
		want               reading.Status
		wantReasonContains string
	}{
		{"fresh pending unchanged", mk(reading.Pending, now.Add(-1*time.Minute)), reading.Pending, ""},
		{"expired pending -> failed", mk(reading.Pending, now.Add(-11*time.Minute)), reading.Failed, "timed out before processing"},
		{"fresh running unchanged", mkRun(reading.Running, now.Add(-5*time.Minute)), reading.Running, ""},
		{"stuck running -> failed", mkRun(reading.Running, now.Add(-31*time.Minute)), reading.Failed, "stalled"},
		{"ready never annotated", mk(reading.Ready, now.Add(-99*time.Hour)), reading.Ready, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			before := tc.in
			got := reading.AnnotateStale(tc.in, now, cfg)
			if got.Status != tc.want {
				t.Fatalf("AnnotateStale status = %q, want %q", got.Status, tc.want)
			}
			if tc.wantReasonContains == "" && got.StaleReason != "" {
				t.Fatalf("AnnotateStale stale reason = %q, want empty", got.StaleReason)
			}
			if tc.wantReasonContains != "" && !strings.Contains(got.StaleReason, tc.wantReasonContains) {
				t.Fatalf("AnnotateStale stale reason = %q, want to contain %q", got.StaleReason, tc.wantReasonContains)
			}
			if !reflect.DeepEqual(tc.in, before) {
				t.Fatalf("AnnotateStale mutated input: got %+v, want %+v", tc.in, before)
			}
		})
	}
}
