package readingops_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/bbell/reading-lite/internal/blobs"
	"github.com/bbell/reading-lite/internal/clock"
	"github.com/bbell/reading-lite/internal/reading"
	"github.com/bbell/reading-lite/internal/readingops"
	"github.com/bbell/reading-lite/internal/store"
)

var testNow = time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)

type submitter struct {
	ids       []string
	forcedIDs []string
}

func (s *submitter) Submit(id string) {
	s.ids = append(s.ids, id)
}

func (s *submitter) ForceSubmitAfter(_ context.Context, id string, beforeQueue func() error) error {
	if err := beforeQueue(); err != nil {
		return err
	}
	s.forcedIDs = append(s.forcedIDs, id)
	return nil
}

type harness struct {
	store     *store.Memory
	blobs     *blobs.Memory
	clock     *clock.Fake
	submitter *submitter
	nextID    int
	service   *readingops.Service
}

func newHarness() *harness {
	h := &harness{
		store:     store.NewMemory(),
		blobs:     blobs.NewMemory(),
		clock:     clock.NewFake(testNow),
		submitter: &submitter{},
	}
	h.service = &readingops.Service{
		Store:      h.store,
		Blobs:      h.blobs,
		Dispatcher: h.submitter,
		Clock:      h.clock,
		TTLs: reading.TTLs{
			Pending: 5 * time.Minute,
			Running: 5 * time.Minute,
		},
		NewID: func() string {
			h.nextID++
			return "r" + string(rune('0'+h.nextID))
		},
	}
	return h
}

func seedReading(t *testing.T, h *harness, r reading.Reading) reading.Reading {
	t.Helper()
	if r.URLKey == "" {
		key, err := reading.URLKey(r.URL)
		if err != nil {
			t.Fatalf("URLKey(%q): %v", r.URL, err)
		}
		r.URLKey = key
	}
	if r.SourceKind == "" {
		r.SourceKind = reading.ClassifySource(r.URLKey)
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = h.clock.Now()
	}
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = r.CreatedAt
	}
	if err := h.store.SaveReading(context.Background(), r); err != nil {
		t.Fatalf("SaveReading: %v", err)
	}
	return r
}

func TestIngestURL_CreatesPendingWebReadingAndSubmits(t *testing.T) {
	t.Parallel()

	h := newHarness()
	res, err := h.service.IngestURL(context.Background(), "https://example.com/post")
	if err != nil {
		t.Fatalf("IngestURL: %v", err)
	}

	if res.ID != "r1" || res.Status != reading.Pending || !res.Created {
		t.Fatalf("result = %+v, want created r1 pending", res)
	}
	stored, err := h.store.GetByID(context.Background(), "r1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.Status != reading.Pending || stored.SourceKind != reading.SourceWeb {
		t.Fatalf("stored status/source = %q/%q, want pending/web", stored.Status, stored.SourceKind)
	}
	if !slices.Equal(h.submitter.ids, []string{"r1"}) {
		t.Fatalf("submitted ids = %v, want [r1]", h.submitter.ids)
	}
}

func TestIngestURL_MarkdownURLCreatesFetchableWebReading(t *testing.T) {
	t.Parallel()

	h := newHarness()
	if _, err := h.service.IngestURL(context.Background(), "https://example.com/notes.md"); err != nil {
		t.Fatalf("IngestURL: %v", err)
	}

	stored, err := h.store.GetByID(context.Background(), "r1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.SourceKind != reading.SourceWeb || stored.RawKey != "" {
		t.Fatalf("stored source/raw = %q/%q, want web with no raw key", stored.SourceKind, stored.RawKey)
	}
}

func TestImportMarkdown_CreatesRawBlobBeforeReadingAndSubmits(t *testing.T) {
	t.Parallel()

	h := newHarness()
	res, err := h.service.ImportMarkdown(context.Background(), readingops.MarkdownImport{
		URL:      "https://example.com/notes.md",
		Markdown: "# Notes\n\nBody",
		Title:    "Notes",
		Tags:     []string{"personal"},
	})
	if err != nil {
		t.Fatalf("ImportMarkdown: %v", err)
	}

	if res.ID != "r1" || res.Status != reading.Pending || !res.Created {
		t.Fatalf("result = %+v, want created r1 pending", res)
	}
	stored, err := h.store.GetByID(context.Background(), "r1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.SourceKind != reading.SourceMarkdown || stored.RawKey == "" || stored.Title != "Notes" {
		t.Fatalf("stored = %+v, want markdown with raw key and title", stored)
	}
	data, ctype, err := h.blobs.Get(context.Background(), stored.RawKey)
	if err != nil {
		t.Fatalf("raw blob: %v", err)
	}
	if string(data) != "# Notes\n\nBody" || ctype != "text/markdown" {
		t.Fatalf("raw blob = %q/%q, want markdown", data, ctype)
	}
	if !slices.Equal(h.submitter.ids, []string{"r1"}) {
		t.Fatalf("submitted ids = %v, want [r1]", h.submitter.ids)
	}
}

type failingPutBlobs struct {
	inner *blobs.Memory
	err   error
}

func (b *failingPutBlobs) Put(context.Context, string, []byte, string) error {
	return b.err
}

func (b *failingPutBlobs) Get(ctx context.Context, key string) ([]byte, string, error) {
	return b.inner.Get(ctx, key)
}

func (b *failingPutBlobs) Delete(ctx context.Context, key string) error {
	return b.inner.Delete(ctx, key)
}

func TestImportMarkdown_BlobFailureDoesNotLeavePendingReading(t *testing.T) {
	t.Parallel()

	h := newHarness()
	putErr := errors.New("put failed")
	h.service.Blobs = &failingPutBlobs{inner: h.blobs, err: putErr}

	_, err := h.service.ImportMarkdown(context.Background(), readingops.MarkdownImport{
		URL:      "https://example.com/notes.md",
		Markdown: "# Notes",
	})
	if !errors.Is(err, putErr) {
		t.Fatalf("ImportMarkdown error = %v, want put error", err)
	}
	key, err := reading.URLKey("https://example.com/notes.md")
	if err != nil {
		t.Fatalf("URLKey: %v", err)
	}
	if _, err := h.store.GetByURLKey(context.Background(), key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetByURLKey after failed import = %v, want ErrNotFound", err)
	}
	if len(h.submitter.ids) != 0 {
		t.Fatalf("submitted ids = %v, want none", h.submitter.ids)
	}
}

type failingUpdateImportStore struct {
	*store.Memory
	err error
}

func (s failingUpdateImportStore) UpdateImport(context.Context, string, store.ImportFields) error {
	return s.err
}

func TestImportMarkdown_FailedReplacementUpdateFailureKeepsExistingRawBlob(t *testing.T) {
	t.Parallel()

	h := newHarness()
	mem := store.NewMemory()
	h.store = mem
	h.service.Store = failingUpdateImportStore{Memory: mem, err: errors.New("update import failed")}
	seedReading(t, h, reading.Reading{
		ID:         "failed",
		URL:        "https://example.com/old.md",
		Status:     reading.Failed,
		SourceKind: reading.SourceMarkdown,
		RawKey:     "readings/failed/raw.md",
	})
	if err := h.blobs.Put(context.Background(), "readings/failed/raw.md", []byte("old body"), "text/markdown"); err != nil {
		t.Fatalf("seed blob: %v", err)
	}

	_, err := h.service.ImportMarkdown(context.Background(), readingops.MarkdownImport{
		URL:      "https://example.com/old.md",
		Markdown: "new body",
	})
	if err == nil {
		t.Fatal("ImportMarkdown succeeded, want error")
	}
	stored, err := h.store.GetByID(context.Background(), "failed")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.RawKey != "readings/failed/raw.md" {
		t.Fatalf("raw key = %q, want existing key", stored.RawKey)
	}
	data, ctype, err := h.blobs.Get(context.Background(), "readings/failed/raw.md")
	if err != nil {
		t.Fatalf("existing blob get: %v", err)
	}
	if string(data) != "old body" || ctype != "text/markdown" {
		t.Fatalf("existing blob = %q/%q, want old body text/markdown", data, ctype)
	}
	if _, _, err := h.blobs.Get(context.Background(), "readings/failed/raw-r1.md"); !errors.Is(err, blobs.ErrNotFound) {
		t.Fatalf("staged replacement blob get = %v, want ErrNotFound", err)
	}
	if len(h.submitter.ids) != 0 {
		t.Fatalf("submitted ids = %v, want none", h.submitter.ids)
	}
}

func TestImportMarkdown_FailedReplacementCollisionDoesNotOverwriteExistingRawBlob(t *testing.T) {
	t.Parallel()

	h := newHarness()
	mem := store.NewMemory()
	h.store = mem
	h.service.Store = failingUpdateImportStore{Memory: mem, err: errors.New("update import failed")}
	seedReading(t, h, reading.Reading{
		ID:         "failed",
		URL:        "https://example.com/old.md",
		Status:     reading.Failed,
		SourceKind: reading.SourceMarkdown,
		RawKey:     "readings/failed/raw-r1.md",
	})
	if err := h.blobs.Put(context.Background(), "readings/failed/raw-r1.md", []byte("old body"), "text/markdown"); err != nil {
		t.Fatalf("seed blob: %v", err)
	}

	_, err := h.service.ImportMarkdown(context.Background(), readingops.MarkdownImport{
		URL:      "https://example.com/old.md",
		Markdown: "new body",
	})
	if err == nil {
		t.Fatal("ImportMarkdown succeeded, want error")
	}
	stored, err := h.store.GetByID(context.Background(), "failed")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.RawKey != "readings/failed/raw-r1.md" {
		t.Fatalf("raw key = %q, want existing key", stored.RawKey)
	}
	data, ctype, err := h.blobs.Get(context.Background(), "readings/failed/raw-r1.md")
	if err != nil {
		t.Fatalf("existing blob get: %v", err)
	}
	if string(data) != "old body" || ctype != "text/markdown" {
		t.Fatalf("existing blob = %q/%q, want old body text/markdown", data, ctype)
	}
	if _, _, err := h.blobs.Get(context.Background(), "readings/failed/raw-r1-1.md"); !errors.Is(err, blobs.ErrNotFound) {
		t.Fatalf("staged replacement blob get = %v, want ErrNotFound", err)
	}
	if len(h.submitter.ids) != 0 {
		t.Fatalf("submitted ids = %v, want none", h.submitter.ids)
	}
}

func TestImportMarkdown_SuccessfulReplacementDeletesOldBlobsAfterUpdate(t *testing.T) {
	t.Parallel()

	h := newHarness()
	seedReading(t, h, reading.Reading{
		ID:         "failed",
		URL:        "https://example.com/old.md",
		Status:     reading.Failed,
		SourceKind: reading.SourceMarkdown,
		RawKey:     "readings/failed/raw.md",
		ContentKey: "readings/failed/content.md",
	})
	if err := h.blobs.Put(context.Background(), "readings/failed/raw.md", []byte("old raw"), "text/markdown"); err != nil {
		t.Fatalf("seed raw blob: %v", err)
	}
	if err := h.blobs.Put(context.Background(), "readings/failed/content.md", []byte("old content"), "text/markdown"); err != nil {
		t.Fatalf("seed content blob: %v", err)
	}

	res, err := h.service.ImportMarkdown(context.Background(), readingops.MarkdownImport{
		URL:      "https://example.com/old.md",
		Markdown: "new body",
		Title:    "Replacement",
	})
	if err != nil {
		t.Fatalf("ImportMarkdown: %v", err)
	}

	if res.ID != "failed" || res.Status != reading.Pending || res.Created {
		t.Fatalf("result = %+v, want replacement pending without Created", res)
	}
	stored, err := h.store.GetByID(context.Background(), "failed")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.RawKey != "readings/failed/raw-r1.md" {
		t.Fatalf("raw key = %q, want staged replacement key", stored.RawKey)
	}
	data, ctype, err := h.blobs.Get(context.Background(), stored.RawKey)
	if err != nil {
		t.Fatalf("replacement blob get: %v", err)
	}
	if string(data) != "new body" || ctype != "text/markdown" {
		t.Fatalf("replacement blob = %q/%q, want new markdown", data, ctype)
	}
	for _, key := range []string{"readings/failed/raw.md", "readings/failed/content.md"} {
		if _, _, err := h.blobs.Get(context.Background(), key); !errors.Is(err, blobs.ErrNotFound) {
			t.Fatalf("old blob %q get = %v, want ErrNotFound", key, err)
		}
	}
	if !slices.Equal(h.submitter.ids, []string{"failed"}) {
		t.Fatalf("submitted ids = %v, want [failed]", h.submitter.ids)
	}
}

func TestReprocess_FreshPendingAndRunningAreIdempotent(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		status reading.Status
	}{
		{name: "pending", status: reading.Pending},
		{name: "running", status: reading.Running},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness()
			started := h.clock.Now().Add(-time.Minute)
			r := reading.Reading{
				ID:              "r1",
				URL:             "https://example.com/post",
				Status:          tc.status,
				ProcessAttempts: 2,
			}
			if tc.status == reading.Running {
				r.StartedAt = &started
			}
			seedReading(t, h, r)

			res, err := h.service.Reprocess(context.Background(), "r1")
			if err != nil {
				t.Fatalf("Reprocess: %v", err)
			}

			if res.ID != "r1" || res.Status != tc.status {
				t.Fatalf("result = %+v, want existing %s", res, tc.status)
			}
			stored, err := h.store.GetByID(context.Background(), "r1")
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			if stored.ProcessAttempts != 2 {
				t.Fatalf("ProcessAttempts = %d, want unchanged 2", stored.ProcessAttempts)
			}
			if len(h.submitter.ids) != 0 || len(h.submitter.forcedIDs) != 0 {
				t.Fatalf("submitted ids = %v forced = %v, want none", h.submitter.ids, h.submitter.forcedIDs)
			}
		})
	}
}

func TestReprocess_ReadyFailedAndStaleReadingsResetAndEnqueue(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		reading    reading.Reading
		wantSubmit []string
		wantForce  []string
	}{
		{
			name:       "ready",
			reading:    reading.Reading{ID: "r1", URL: "https://example.com/ready", Status: reading.Ready, ContentKey: "content", RawKey: "raw", Summary: "old"},
			wantSubmit: []string{"r1"},
		},
		{
			name:       "failed",
			reading:    reading.Reading{ID: "r1", URL: "https://example.com/failed", Status: reading.Failed, Error: "boom", ProcessAttempts: 3},
			wantSubmit: []string{"r1"},
		},
		{
			name:      "stale_pending",
			reading:   reading.Reading{ID: "r1", URL: "https://example.com/pending", Status: reading.Pending, CreatedAt: testNow.Add(-10 * time.Minute), UpdatedAt: testNow.Add(-10 * time.Minute), ProcessAttempts: 2},
			wantForce: []string{"r1"},
		},
		{
			name:      "stale_running",
			reading:   reading.Reading{ID: "r1", URL: "https://example.com/running", Status: reading.Running, StartedAt: ptr(testNow.Add(-10 * time.Minute)), ProcessAttempts: 2},
			wantForce: []string{"r1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness()
			seedReading(t, h, tc.reading)

			res, err := h.service.Reprocess(context.Background(), "r1")
			if err != nil {
				t.Fatalf("Reprocess: %v", err)
			}

			if res.ID != "r1" || res.Status != reading.Pending {
				t.Fatalf("result = %+v, want pending r1", res)
			}
			stored, err := h.store.GetByID(context.Background(), "r1")
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			if stored.Status != reading.Pending || stored.ProcessAttempts != 0 || stored.ContentKey != "" || stored.Summary != "" {
				t.Fatalf("stored = %+v, want reset pending with cleared derived content", stored)
			}
			if !slices.Equal(h.submitter.ids, tc.wantSubmit) {
				t.Fatalf("submitted ids = %v, want %v", h.submitter.ids, tc.wantSubmit)
			}
			if !slices.Equal(h.submitter.forcedIDs, tc.wantForce) {
				t.Fatalf("forced ids = %v, want %v", h.submitter.forcedIDs, tc.wantForce)
			}
		})
	}
}

func TestReprocess_MarkdownImportPreservesSourceTitleAndRawKey(t *testing.T) {
	t.Parallel()

	h := newHarness()
	seedReading(t, h, reading.Reading{
		ID:         "r1",
		URL:        "https://example.com/notes.md",
		Status:     reading.Ready,
		SourceKind: reading.SourceMarkdown,
		Title:      "Imported Notes",
		RawKey:     "readings/r1/raw.md",
		ContentKey: "readings/r1/content.md",
		Summary:    "Old summary",
	})

	res, err := h.service.Reprocess(context.Background(), "r1")
	if err != nil {
		t.Fatalf("Reprocess: %v", err)
	}

	if res.ID != "r1" || res.Status != reading.Pending {
		t.Fatalf("result = %+v, want pending r1", res)
	}
	stored, err := h.store.GetByID(context.Background(), "r1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.Title != "Imported Notes" || stored.RawKey != "readings/r1/raw.md" {
		t.Fatalf("stored title/raw = %q/%q, want imported title and raw key", stored.Title, stored.RawKey)
	}
	if stored.ContentKey != "" || stored.Summary != "" {
		t.Fatalf("stored derived content = %q/%q, want cleared", stored.ContentKey, stored.Summary)
	}
	if !slices.Equal(h.submitter.ids, []string{"r1"}) {
		t.Fatalf("submitted ids = %v, want [r1]", h.submitter.ids)
	}
}

func ptr[T any](v T) *T {
	return &v
}
