package notify_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/bbell/reading-lite/internal/notify"
)

func TestFake_RecordsSentEmail(t *testing.T) {
	t.Parallel()

	f := &notify.Fake{}
	email := notify.Email{
		From:    "reader@example.com",
		To:      "me@example.com",
		Subject: "Ready: Kubernetes for personal services",
		HTML:    "<p>Your reading is ready.</p>",
	}
	if err := f.Notify(context.Background(), email); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	sent := f.Sent()
	if len(sent) != 1 {
		t.Fatalf("Sent len = %d, want 1", len(sent))
	}
	if diff := cmp.Diff(email, sent[0]); diff != "" {
		t.Fatalf("sent email mismatch (-want +got):\n%s", diff)
	}
	if f.Calls() != 1 {
		t.Fatalf("Calls = %d, want 1", f.Calls())
	}
}

func TestFake_ScriptedErrorIsCountedButNotSent(t *testing.T) {
	t.Parallel()

	f := &notify.Fake{Err: errors.New("resend down")}
	if err := f.Notify(context.Background(), notify.Email{To: "me@example.com"}); err == nil {
		t.Fatal("Notify = nil, want scripted error")
	}
	if f.Calls() != 1 {
		t.Fatalf("Calls = %d, want 1", f.Calls())
	}
	if got := f.Sent(); len(got) != 0 {
		t.Fatalf("Sent = %v, want empty (a failed send is not recorded as sent)", got)
	}
}

func TestFake_ContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &notify.Fake{}
	if err := f.Notify(ctx, notify.Email{To: "me@example.com"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Notify cancelled = %v, want context.Canceled", err)
	}
}

func TestFake_ConcurrentNotify(t *testing.T) {
	t.Parallel()

	f := &notify.Fake{}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = f.Notify(context.Background(), notify.Email{To: "me@example.com"})
		}()
	}
	wg.Wait()
	if f.Calls() != 20 || len(f.Sent()) != 20 {
		t.Fatalf("Calls/Sent = %d/%d, want 20/20", f.Calls(), len(f.Sent()))
	}
}
