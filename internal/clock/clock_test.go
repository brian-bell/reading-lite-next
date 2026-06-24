package clock_test

import (
	"sync"
	"testing"
	"time"

	"github.com/bbell/reading-lite/internal/clock"
)

func TestFakeClock_AdvanceMovesNow(t *testing.T) {
	t.Parallel()

	c := clock.NewFake(time.Unix(1000, 0))
	start := c.Now()

	c.Advance(90 * time.Second)

	if got := c.Now().Sub(start); got != 90*time.Second {
		t.Fatalf("Advance: now moved %v, want 90s", got)
	}
}

func TestFakeClock_SetMovesNow(t *testing.T) {
	t.Parallel()

	c := clock.NewFake(time.Unix(1000, 0))

	c.Set(time.Unix(2000, 0))

	if got, want := c.Now(), time.Unix(2000, 0); !got.Equal(want) {
		t.Fatalf("Set: now = %v, want %v", got, want)
	}
}

func TestFakeClock_ConcurrentUse(t *testing.T) {
	c := clock.NewFake(time.Unix(1000, 0))

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(3)

		go func() {
			defer wg.Done()
			_ = c.Now()
		}()

		go func() {
			defer wg.Done()
			c.Advance(time.Second)
		}()

		go func(i int) {
			defer wg.Done()
			c.Set(time.Unix(1000+int64(i), 0))
		}(i)
	}

	wg.Wait()
}
