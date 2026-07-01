package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bbell/reading-lite/internal/blobs"
	"github.com/bbell/reading-lite/internal/clock"
	"github.com/bbell/reading-lite/internal/dispatch"
	"github.com/bbell/reading-lite/internal/embed"
	"github.com/bbell/reading-lite/internal/extract"
	"github.com/bbell/reading-lite/internal/fetch"
	"github.com/bbell/reading-lite/internal/notify"
	"github.com/bbell/reading-lite/internal/pipeline"
	"github.com/bbell/reading-lite/internal/reading"
	"github.com/bbell/reading-lite/internal/store"
	"github.com/bbell/reading-lite/internal/summarize"
	"github.com/bbell/reading-lite/internal/vector"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// harness wires the pipeline against fakes and drives it through an inline
// dispatcher — exactly how production runs it, so status transitions are real.
type harness struct {
	store      *store.Memory
	blobs      *blobs.Memory
	fetcher    *fetch.Fake
	extractor  *extract.Fake
	youtube    *fakeYouTube
	embedder   *embed.Fake
	vectors    *vector.Memory
	summarizer *summarize.Fake
	notifier   *notify.Fake
	clock      *clock.Fake
	delay      *dispatch.FakeDelayer
	pipeline   *pipeline.Pipeline
	dispatcher *dispatch.Dispatcher
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	h := &harness{
		store:      store.NewMemory(),
		blobs:      blobs.NewMemory(),
		fetcher:    &fetch.Fake{Resource: fetch.Resource{Body: []byte("<html><body>hi</body></html>"), ContentType: "text/html", Status: 200}},
		extractor:  &extract.Fake{Article: extract.Article{Title: "Extracted Title", Author: "Ada", Site: "example.com", Lang: "en", Markdown: "# Heading\n\nBody text here.", Mode: extract.ModeReadability, WordCount: 42}},
		youtube:    &fakeYouTube{article: extract.Article{Title: "Video Title", Author: "Creator", Site: "YouTube", Markdown: "video description", Mode: extract.ModeRawOnly, WordCount: 2}},
		embedder:   &embed.Fake{Vec: unitVec()},
		vectors:    vector.NewMemory(),
		summarizer: &summarize.Fake{Summary: summarize.Summary{Title: "Refined Title", Summary: "A concise summary.", Tags: []string{"go", "db"}, JSON: json.RawMessage(`{"key":"value"}`)}},
		notifier:   &notify.Fake{},
		clock:      clock.NewFake(epoch),
		delay:      &dispatch.FakeDelayer{},
	}
	h.pipeline = &pipeline.Pipeline{
		Store:      h.store,
		Blobs:      h.blobs,
		Fetcher:    h.fetcher,
		Extractor:  h.extractor,
		YouTube:    h.youtube,
		Embedder:   h.embedder,
		Vectors:    h.vectors,
		Summarizer: h.summarizer,
		Notifier:   h.notifier,
		Clock:      h.clock,
		Config: pipeline.Config{
			TopK:          5,
			NotifyEnabled: true,
			NotifyFrom:    "reader@example.com",
			NotifyTo:      "me@example.com",
		},
	}
	h.dispatcher = &dispatch.Dispatcher{
		Handler: h.pipeline.Process,
		Store:   h.store,
		Clock:   h.clock,
		Delay:   h.delay,
		Max:     5,
		Inline:  true,
	}
	return h
}

func (h *harness) seed(t *testing.T, id, rawURL string) reading.Reading {
	t.Helper()
	key, err := reading.URLKey(rawURL)
	if err != nil {
		t.Fatalf("URLKey(%q): %v", rawURL, err)
	}
	r := reading.Reading{
		ID:         id,
		URL:        rawURL,
		URLKey:     key,
		Status:     reading.Pending,
		SourceKind: reading.ClassifySource(key),
		CreatedAt:  h.clock.Now(),
		UpdatedAt:  h.clock.Now(),
	}
	if err := h.store.SaveReading(context.Background(), r); err != nil {
		t.Fatalf("seed %q: %v", id, err)
	}
	return r
}

func (h *harness) get(t *testing.T, id string) reading.Reading {
	t.Helper()
	r, err := h.store.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetByID(%q): %v", id, err)
	}
	return r
}

func unitVec() []float32 {
	v := make([]float32, embed.Dim)
	v[0] = 1
	return v
}

func TestPipeline_HappyPath(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seed(t, "r1", "https://example.com/post")

	// A ready neighbor reading with the same vector so similarity search returns
	// it, proving upsert + query + hydrate end to end. It must be ready: hydrate
	// only snapshots ready readings.
	neighbor := h.seed(t, "neighbor", "https://example.com/neighbor")
	if err := h.vectors.Upsert(context.Background(), neighbor.ID, unitVec(), nil); err != nil {
		t.Fatalf("seed neighbor vector: %v", err)
	}
	if err := h.store.UpdateContent(context.Background(), neighbor.ID, store.ContentFields{Title: "Neighbor Title", Now: epoch}); err != nil {
		t.Fatalf("seed neighbor content: %v", err)
	}
	if err := h.store.UpdateStatus(context.Background(), neighbor.ID, reading.Ready, store.StatusFields{Now: epoch}); err != nil {
		t.Fatalf("seed neighbor status: %v", err)
	}

	h.dispatcher.Submit("r1")

	got := h.get(t, "r1")
	if got.Status != reading.Ready {
		t.Fatalf("status = %q, want ready", got.Status)
	}
	if got.ExtractionMode != string(extract.ModeReadability) {
		t.Fatalf("extraction_mode = %q, want readability", got.ExtractionMode)
	}
	if got.Title != "Refined Title" {
		t.Fatalf("title = %q, want the summarizer's refined title", got.Title)
	}
	if !sliceEqual(got.Tags, []string{"go", "db"}) {
		t.Fatalf("tags = %v, want [go db] from the summary", got.Tags)
	}
	if got.ContentKey == "" || got.RawKey == "" {
		t.Fatalf("content_key/raw_key = %q/%q, want both set", got.ContentKey, got.RawKey)
	}
	if got.Summary != "A concise summary." {
		t.Fatalf("summary = %q, want the summarizer summary", got.Summary)
	}
	if string(got.SummaryJSON) != `{"key":"value"}` {
		t.Fatalf("summary_json = %s, want the emit_reading payload", got.SummaryJSON)
	}

	// The vector was upserted for r1.
	matches, err := h.vectors.Query(context.Background(), unitVec(), 5, "")
	if err != nil {
		t.Fatalf("vector query: %v", err)
	}
	if !containsID(matches, "r1") {
		t.Fatalf("vector index missing r1 after upsert: %+v", matches)
	}

	// similar_json is populated with the hydrated neighbor.
	var similar []pipeline.SimilarItem
	if err := json.Unmarshal(got.SimilarJSON, &similar); err != nil {
		t.Fatalf("similar_json unmarshal: %v (raw=%s)", err, got.SimilarJSON)
	}
	if len(similar) != 1 || similar[0].ID != "neighbor" {
		t.Fatalf("similar = %+v, want one entry for neighbor", similar)
	}
	if similar[0].Title != "Neighbor Title" {
		t.Fatalf("similar[0].Title = %q, want hydrated 'Neighbor Title'", similar[0].Title)
	}

	// "Summarize once" and "one notify".
	if h.summarizer.Calls() != 1 {
		t.Fatalf("summarizer calls = %d, want exactly 1", h.summarizer.Calls())
	}
	if len(h.notifier.Sent()) != 1 {
		t.Fatalf("notify sent = %d, want 1", len(h.notifier.Sent()))
	}

	// Both blobs were written and are retrievable.
	if _, _, err := h.blobs.Get(context.Background(), got.RawKey); err != nil {
		t.Fatalf("raw blob get: %v", err)
	}
	if _, _, err := h.blobs.Get(context.Background(), got.ContentKey); err != nil {
		t.Fatalf("content blob get: %v", err)
	}

	// Diagnostics recorded.
	if len(got.DiagnosticsJSON) == 0 {
		t.Fatalf("diagnostics_json empty, want recorded pipeline diagnostics")
	}
	var diag struct {
		TimingsMS map[string]float64 `json:"timings_ms"`
	}
	if err := json.Unmarshal(got.DiagnosticsJSON, &diag); err != nil {
		t.Fatalf("diagnostics_json unmarshal: %v", err)
	}
	for _, key := range []string{"acquire", "index", "checkpoint", "vector_upsert", "summarize", "notify"} {
		if _, ok := diag.TimingsMS[key]; !ok {
			t.Fatalf("timings_ms = %+v, missing %q", diag.TimingsMS, key)
		}
	}
}

func TestPipeline_Reddit_FailsWithGuidance(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seed(t, "r1", "https://www.reddit.com/r/golang/comments/abc/post")

	h.dispatcher.Submit("r1")

	got := h.get(t, "r1")
	if got.Status != reading.Failed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, pipeline.RedditGuidance) {
		t.Fatalf("error = %q, want it to contain the reddit guidance %q", got.Error, pipeline.RedditGuidance)
	}
	if h.fetcher.Calls() != 0 {
		t.Fatalf("fetcher calls = %d, want 0 (reddit must not be fetched)", h.fetcher.Calls())
	}
	// Permanent failure: no retry scheduled.
	if h.delay.Total() != 0 {
		t.Fatalf("delay scheduled %d times, want 0 (reddit fails permanently)", h.delay.Total())
	}
}

func TestPipeline_Markdown_SkipsFetchExtract(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	rawURL := "https://example.com/notes.md"
	key, err := reading.URLKey(rawURL)
	if err != nil {
		t.Fatalf("URLKey: %v", err)
	}
	if reading.ClassifySource(key) != reading.SourceMarkdown {
		t.Fatalf("ClassifySource(%q) = %q, want markdown", key, reading.ClassifySource(key))
	}
	rk := "imports/r-md/raw"
	if err := h.blobs.Put(context.Background(), rk, []byte("# Imported\n\nMarkdown body."), "text/markdown"); err != nil {
		t.Fatalf("seed markdown blob: %v", err)
	}
	r := reading.Reading{
		ID:         "r-md",
		URL:        rawURL,
		URLKey:     key,
		Status:     reading.Pending,
		SourceKind: reading.SourceMarkdown,
		Title:      "Imported Notes",
		RawKey:     rk,
		CreatedAt:  h.clock.Now(),
		UpdatedAt:  h.clock.Now(),
	}
	if err := h.store.SaveReading(context.Background(), r); err != nil {
		t.Fatalf("seed markdown reading: %v", err)
	}

	h.dispatcher.Submit("r-md")

	got := h.get(t, "r-md")
	if got.Status != reading.Ready {
		t.Fatalf("status = %q, want ready", got.Status)
	}
	if h.fetcher.Calls() != 0 {
		t.Fatalf("fetcher calls = %d, want 0 (markdown skips fetch)", h.fetcher.Calls())
	}
	if h.extractor.Calls() != 0 {
		t.Fatalf("extractor calls = %d, want 0 (markdown skips extract)", h.extractor.Calls())
	}
	if h.embedder.Calls() != 1 {
		t.Fatalf("embedder calls = %d, want 1", h.embedder.Calls())
	}
	if h.summarizer.Calls() != 1 {
		t.Fatalf("summarizer calls = %d, want 1", h.summarizer.Calls())
	}
	if got.ContentKey == "" {
		t.Fatalf("content_key empty, want the extracted markdown blob set")
	}
}

func TestPipeline_Markdown_PreservesImportedTitle(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.summarizer.Summary.Title = ""
	rawURL := "https://example.com/notes.md"
	key, err := reading.URLKey(rawURL)
	if err != nil {
		t.Fatalf("URLKey: %v", err)
	}
	rk := "imports/r-md/raw"
	if err := h.blobs.Put(context.Background(), rk, []byte("# Imported\n\nMarkdown body."), "text/markdown"); err != nil {
		t.Fatalf("seed markdown blob: %v", err)
	}
	r := reading.Reading{
		ID:         "r-md",
		URL:        rawURL,
		URLKey:     key,
		Status:     reading.Pending,
		SourceKind: reading.SourceMarkdown,
		Title:      "Imported Notes",
		RawKey:     rk,
		CreatedAt:  h.clock.Now(),
		UpdatedAt:  h.clock.Now(),
	}
	if err := h.store.SaveReading(context.Background(), r); err != nil {
		t.Fatalf("seed markdown reading: %v", err)
	}

	h.dispatcher.Submit("r-md")

	got := h.get(t, "r-md")
	if got.Status != reading.Ready {
		t.Fatalf("status = %q, want ready", got.Status)
	}
	if got.Title != "Imported Notes" {
		t.Fatalf("title = %q, want imported title", got.Title)
	}
}

func TestPipeline_SummarizerError_RetriesNotDoubleSummarize(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seed(t, "r1", "https://example.com/post")

	// First run: everything up to and including the content checkpoint succeeds,
	// then summarize fails -> transient Retry, reading goes back to pending.
	h.summarizer.Err = errors.New("summarizer 503")
	h.dispatcher.Submit("r1")

	got := h.get(t, "r1")
	if got.Status != reading.Pending {
		t.Fatalf("status after summarize failure = %q, want pending (retry)", got.Status)
	}
	if got.ContentKey == "" {
		t.Fatalf("content_key empty after first run, want the checkpoint persisted for re-entry")
	}
	if h.summarizer.Calls() != 1 {
		t.Fatalf("summarizer calls after run 1 = %d, want exactly 1 per run", h.summarizer.Calls())
	}
	if h.delay.PendingLen() != 1 {
		t.Fatalf("scheduled retries = %d, want 1", h.delay.PendingLen())
	}

	// Second run (re-entry): summarize now succeeds. The already-done fetch/extract
	// work must be skipped. Embedding may run again to recover a vector upsert after
	// a guarded content checkpoint, and summarize runs exactly once per run.
	h.summarizer.Err = nil
	h.delay.FireAll()

	got = h.get(t, "r1")
	if got.Status != reading.Ready {
		t.Fatalf("status after retry = %q, want ready", got.Status)
	}
	if h.summarizer.Calls() != 2 {
		t.Fatalf("summarizer calls total = %d, want 2 (once per run, no double-summary)", h.summarizer.Calls())
	}
	if h.fetcher.Calls() != 1 {
		t.Fatalf("fetcher calls = %d, want 1 (re-entry must not re-fetch)", h.fetcher.Calls())
	}
	if h.extractor.Calls() != 1 {
		t.Fatalf("extractor calls = %d, want 1 (re-entry must not re-extract)", h.extractor.Calls())
	}
	if h.embedder.Calls() != 2 {
		t.Fatalf("embedder calls = %d, want 2 (re-entry re-upserts vector)", h.embedder.Calls())
	}
	if got.Summary != "A concise summary." {
		t.Fatalf("summary = %q, want it filled on the successful retry", got.Summary)
	}
}

func TestPipeline_ExtractionFallback(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mode extract.Mode
	}{
		{"raw_dom salvage", extract.ModeRawDOM},
		{"raw_only floor", extract.ModeRawOnly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			h.extractor.Article = extract.Article{Markdown: "salvaged body text", Mode: tc.mode, WordCount: 3}
			h.seed(t, "r1", "https://example.com/post")

			h.dispatcher.Submit("r1")

			got := h.get(t, "r1")
			if got.Status != reading.Ready {
				t.Fatalf("status = %q, want ready", got.Status)
			}
			if got.ExtractionMode != string(tc.mode) {
				t.Fatalf("extraction_mode = %q, want %q", got.ExtractionMode, tc.mode)
			}
			// The lower tiers still embed and summarize their salvaged text.
			if h.embedder.Calls() != 1 || h.summarizer.Calls() != 1 {
				t.Fatalf("embed/summarize calls = %d/%d, want 1/1", h.embedder.Calls(), h.summarizer.Calls())
			}
		})
	}
}

func TestPipeline_YouTube_OEmbedFloor(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	r := h.seed(t, "r1", "https://youtu.be/abcdEFGHijk")
	if r.SourceKind != reading.SourceYouTube {
		t.Fatalf("source kind = %q, want youtube", r.SourceKind)
	}

	h.dispatcher.Submit("r1")

	got := h.get(t, "r1")
	if got.Status != reading.Ready {
		t.Fatalf("status = %q, want ready (youtube is fetchable, unlike reddit)", got.Status)
	}
	if h.fetcher.Calls() != 0 {
		t.Fatalf("fetcher calls = %d, want youtube to use oEmbed adapter", h.fetcher.Calls())
	}
	if h.extractor.Calls() != 0 {
		t.Fatalf("extractor calls = %d, want youtube to skip HTML extractor", h.extractor.Calls())
	}
	if h.youtube.calls != 1 {
		t.Fatalf("youtube calls = %d, want 1", h.youtube.calls)
	}
	if h.youtube.urls[0] != r.URLKey {
		t.Fatalf("youtube URL = %q, want canonical key %q", h.youtube.urls[0], r.URLKey)
	}
	if got.Author != "Creator" {
		t.Fatalf("author = %q, want the floor author 'Creator'", got.Author)
	}
	if got.ExtractionMode != string(extract.ModeRawOnly) {
		t.Fatalf("extraction_mode = %q, want raw_only floor", got.ExtractionMode)
	}
}

func TestPipeline_YouTubeNonVideoFallsBackToFetchExtract(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	key, err := reading.URLKey("https://www.youtube.com/@channel")
	if err != nil {
		t.Fatalf("URLKey: %v", err)
	}
	r := reading.Reading{
		ID:         "r1",
		URL:        "https://www.youtube.com/@channel",
		URLKey:     key,
		Status:     reading.Pending,
		SourceKind: reading.SourceYouTube,
		CreatedAt:  h.clock.Now(),
		UpdatedAt:  h.clock.Now(),
	}
	if err := h.store.SaveReading(context.Background(), r); err != nil {
		t.Fatalf("SaveReading: %v", err)
	}

	h.dispatcher.Submit("r1")

	got := h.get(t, "r1")
	if got.Status != reading.Ready {
		t.Fatalf("status = %q, want ready", got.Status)
	}
	if h.youtube.calls != 0 {
		t.Fatalf("youtube calls = %d, want non-video URL to skip oEmbed adapter", h.youtube.calls)
	}
	if h.fetcher.Calls() != 1 || h.extractor.Calls() != 1 {
		t.Fatalf("fetch/extract calls = %d/%d, want 1/1", h.fetcher.Calls(), h.extractor.Calls())
	}
}

type fakeYouTube struct {
	article extract.Article
	err     error
	calls   int
	urls    []string
}

func (f *fakeYouTube) Extract(_ context.Context, videoURL string) (extract.Article, error) {
	f.calls++
	f.urls = append(f.urls, videoURL)
	if f.err != nil {
		return extract.Article{}, f.err
	}
	return f.article, nil
}

func TestPipeline_RateLimited_Requeues(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.embedder.Err = &dispatch.RateLimitError{RetryAfter: 30 * time.Second}
	h.seed(t, "r1", "https://example.com/post")

	h.dispatcher.Submit("r1")

	got := h.get(t, "r1")
	if got.Status != reading.Pending {
		t.Fatalf("status = %q, want pending (rate limit requeues, not fails)", got.Status)
	}
	if got.ProcessAttempts != 0 {
		t.Fatalf("process_attempts = %d, want 0 (rate limit does not consume an attempt)", got.ProcessAttempts)
	}
	if d := h.delay.Durations(); len(d) != 1 || d[0] != 30*time.Second {
		t.Fatalf("delay durations = %v, want [30s] from RetryAfter", d)
	}
	if h.summarizer.Calls() != 0 {
		t.Fatalf("summarizer calls = %d, want 0 (failed before summarize)", h.summarizer.Calls())
	}
}

func TestPipeline_NotifyFailureDoesNotFailReading(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.notifier.Err = errors.New("resend 500")
	h.seed(t, "r1", "https://example.com/post")

	h.dispatcher.Submit("r1")

	got := h.get(t, "r1")
	if got.Status != reading.Ready {
		t.Fatalf("status = %q, want ready (a notify failure must not fail the reading)", got.Status)
	}
	if h.notifier.Calls() != 1 {
		t.Fatalf("notifier calls = %d, want 1 (attempted)", h.notifier.Calls())
	}
	if len(h.notifier.Sent()) != 0 {
		t.Fatalf("notifier sent = %d, want 0 (delivery failed)", len(h.notifier.Sent()))
	}
	if !strings.Contains(string(got.DiagnosticsJSON), "notify_error") {
		t.Fatalf("diagnostics_json = %s, want it to record the notify error", got.DiagnosticsJSON)
	}
}

func TestPipeline_FetchHardError_Fails(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		status     int
		wantStatus reading.Status
		wantDelay  int
	}{
		{"4xx is permanent", 404, reading.Failed, 0},
		{"5xx is transient", 503, reading.Pending, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			h.fetcher.Resource = fetch.Resource{Status: tc.status}
			h.seed(t, "r1", "https://example.com/post")

			h.dispatcher.Submit("r1")

			got := h.get(t, "r1")
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if h.delay.Total() != tc.wantDelay {
				t.Fatalf("delay scheduled %d times, want %d", h.delay.Total(), tc.wantDelay)
			}
			if h.extractor.Calls() != 0 {
				t.Fatalf("extractor calls = %d, want 0 (a bad fetch never extracts)", h.extractor.Calls())
			}
		})
	}
}

func TestPipeline_FetchRateLimited_Requeues(t *testing.T) {
	t.Parallel()

	// A fetched 429 is a rate limit: the reading stays pending, no attempt is
	// consumed, and a re-dispatch is scheduled — never a permanent failure.
	h := newHarness(t)
	h.fetcher.Resource = fetch.Resource{Status: 429}
	h.seed(t, "r1", "https://example.com/post")

	h.dispatcher.Submit("r1")

	got := h.get(t, "r1")
	if got.Status != reading.Pending {
		t.Fatalf("status = %q, want pending (a rate limit requeues, not fails)", got.Status)
	}
	if got.ProcessAttempts != 0 {
		t.Fatalf("process_attempts = %d, want 0 (a rate limit must not consume an attempt)", got.ProcessAttempts)
	}
	if h.delay.Total() != 1 {
		t.Fatalf("delays scheduled = %d, want 1 (the requeue)", h.delay.Total())
	}
	if h.extractor.Calls() != 0 {
		t.Fatalf("extractor calls = %d, want 0 (a rate-limited fetch never extracts)", h.extractor.Calls())
	}
}

func TestPipeline_MarkdownWithoutBody_FailsPermanently(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	r := reading.Reading{
		ID:         "r-md",
		URL:        "https://example.com/notes.md",
		URLKey:     "https://example.com/notes.md",
		Status:     reading.Pending,
		SourceKind: reading.SourceMarkdown,
		CreatedAt:  h.clock.Now(),
		UpdatedAt:  h.clock.Now(),
	}
	if err := h.store.SaveReading(context.Background(), r); err != nil {
		t.Fatalf("seed: %v", err)
	}

	h.dispatcher.Submit("r-md")

	got := h.get(t, "r-md")
	if got.Status != reading.Failed {
		t.Fatalf("status = %q, want failed (markdown with no stored body)", got.Status)
	}
	if h.delay.Total() != 0 {
		t.Fatalf("delay scheduled %d times, want 0 (permanent failure)", h.delay.Total())
	}
}

// failUpdateStore delegates to a real store but fails UpdateContent, to exercise
// the pipeline's persistence-error handling.
type failUpdateStore struct {
	*store.Memory
	err error
}

func (s failUpdateStore) UpdateContent(context.Context, string, store.ContentFields) error {
	return s.err
}

func TestPipeline_PersistError_Retries(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.pipeline.Store = failUpdateStore{Memory: h.store, err: errors.New("db write failed")}
	h.seed(t, "r1", "https://example.com/post")

	h.dispatcher.Submit("r1")

	got := h.get(t, "r1")
	if got.Status != reading.Pending {
		t.Fatalf("status = %q, want pending (a persistence error is transient)", got.Status)
	}
	if h.delay.PendingLen() != 1 {
		t.Fatalf("scheduled retries = %d, want 1", h.delay.PendingLen())
	}
	// Failed before summarize (the checkpoint write is what failed).
	if h.summarizer.Calls() != 0 {
		t.Fatalf("summarizer calls = %d, want 0", h.summarizer.Calls())
	}
}

func TestPipeline_ReuseWithMissingBlob_Retries(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seed(t, "r1", "https://example.com/post")
	// Simulate a prior run's checkpoint that points at a content blob which is no
	// longer present: re-entry must retry, not crash.
	if err := h.store.UpdateContent(context.Background(), "r1", store.ContentFields{ContentKey: "readings/r1/gone.md", Now: epoch}); err != nil {
		t.Fatalf("seed checkpoint: %v", err)
	}

	h.dispatcher.Submit("r1")

	got := h.get(t, "r1")
	if got.Status != reading.Pending {
		t.Fatalf("status = %q, want pending (missing blob on re-entry retries)", got.Status)
	}
	if h.fetcher.Calls() != 0 {
		t.Fatalf("fetcher calls = %d, want 0 (re-entry path does not fetch)", h.fetcher.Calls())
	}
}

func TestPipeline_DefaultTopK(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.pipeline.Config.TopK = 0 // exercise the default
	h.seed(t, "r1", "https://example.com/post")

	h.dispatcher.Submit("r1")

	if got := h.get(t, "r1"); got.Status != reading.Ready {
		t.Fatalf("status = %q, want ready", got.Status)
	}
}

func TestPipeline_TransientStepErrorsRetry(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		setup func(*harness)
	}{
		{"extractor error", func(h *harness) { h.extractor.Err = errors.New("extract boom") }},
		{"embed wrong dimension", func(h *harness) { h.embedder.Vec = make([]float32, 8) }},
		{"summarizer error", func(h *harness) { h.summarizer.Err = errors.New("summarize boom") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			tc.setup(h)
			h.seed(t, "r1", "https://example.com/post")

			h.dispatcher.Submit("r1")

			got := h.get(t, "r1")
			if got.Status != reading.Pending {
				t.Fatalf("status = %q, want pending (transient error retries)", got.Status)
			}
			if h.delay.PendingLen() != 1 {
				t.Fatalf("scheduled retries = %d, want 1", h.delay.PendingLen())
			}
		})
	}
}

func TestPipeline_GetByIDError_Retries(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	// Process a missing id directly (the dispatcher would not even reach the
	// handler, so call Process to exercise its load-error path).
	res := h.pipeline.Process(context.Background(), "ghost")
	if res.Outcome != dispatch.Retry {
		t.Fatalf("outcome = %v, want Retry for a missing reading", res.Outcome)
	}
}

func TestPipeline_MarkdownBlobMissing_Retries(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	r := reading.Reading{
		ID:         "r-md",
		URL:        "https://example.com/notes.md",
		URLKey:     "https://example.com/notes.md",
		Status:     reading.Pending,
		SourceKind: reading.SourceMarkdown,
		RawKey:     "imports/r-md/raw", // points at a blob that was never stored
		CreatedAt:  h.clock.Now(),
		UpdatedAt:  h.clock.Now(),
	}
	if err := h.store.SaveReading(context.Background(), r); err != nil {
		t.Fatalf("seed: %v", err)
	}

	h.dispatcher.Submit("r-md")

	if got := h.get(t, "r-md"); got.Status != reading.Pending {
		t.Fatalf("status = %q, want pending (missing import blob retries)", got.Status)
	}
}

type staleSnapshotStore struct {
	*store.Memory
	snapshot reading.Reading
}

func (s staleSnapshotStore) GetByID(ctx context.Context, id string) (reading.Reading, error) {
	if id == s.snapshot.ID {
		return s.snapshot, nil
	}
	return s.Memory.GetByID(ctx, id)
}

func TestPipeline_StaleRunCannotWriteContentAfterReprocess(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	r := h.seed(t, "r1", "https://example.com/post")
	startedAt := h.clock.Now().Add(-10 * time.Minute)
	stale := r
	stale.Status = reading.Running
	stale.StartedAt = &startedAt
	if err := h.store.Reprocess(context.Background(), "r1", store.ReprocessFields{Now: h.clock.Now()}); err != nil {
		t.Fatalf("Reprocess: %v", err)
	}
	h.pipeline.Store = staleSnapshotStore{Memory: h.store, snapshot: stale}

	res := h.pipeline.Process(context.Background(), "r1")

	if res.Outcome != dispatch.Retry || !errors.Is(res.Err, store.ErrConflict) {
		t.Fatalf("Process result = %+v, want retry with ErrConflict", res)
	}
	got := h.get(t, "r1")
	if got.ContentKey != "" || got.Title != "" || got.Summary != "" {
		t.Fatalf("stale content was written after reprocess: %+v", got)
	}
}

// TestPipeline_StaleReuseUpsertCannotOverwriteReplacementVector proves the
// generation fence (issue #9): the reuse path's vector upsert is fenced on
// r.StartedAt, so a run holding an older lease cannot clobber a vector a
// replacement run already wrote at a later generation — independent of ctx
// cancellation, since ctx is never cancelled in this test.
func TestPipeline_StaleReuseUpsertCannotOverwriteReplacementVector(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seed(t, "r1", "https://example.com/post")

	earlier := epoch.Add(-10 * time.Minute)
	later := epoch.Add(-time.Minute)

	// Drive the real store into a Running state with an older lease and a
	// checkpointed content_key, so Process takes the reuse path.
	if err := h.store.UpdateStatus(context.Background(), "r1", reading.Running, store.StatusFields{
		Now:       epoch,
		StartedAt: &earlier,
	}); err != nil {
		t.Fatalf("seed running status: %v", err)
	}
	if err := h.blobs.Put(context.Background(), "readings/r1/content.md", []byte("# Old\n\nOld body."), "text/markdown"); err != nil {
		t.Fatalf("seed content blob: %v", err)
	}
	if err := h.store.UpdateContent(context.Background(), "r1", store.ContentFields{
		Now:        epoch,
		Title:      "Old Title",
		ContentKey: "readings/r1/content.md",
	}); err != nil {
		t.Fatalf("seed checkpoint: %v", err)
	}

	// The replacement run already wrote its fresh vector at the later
	// generation. Its direction must be orthogonal to the harness's default
	// embedder output (unitVec, direction (1,0,0,...)) so a stale unfenced
	// overwrite is unambiguously detectable as a score collapse, not a
	// same-direction coincidence.
	replacementVec := make([]float32, vector.Dim)
	replacementVec[1] = 1
	if err := h.vectors.Upsert(context.Background(), "r1", replacementVec, &later); err != nil {
		t.Fatalf("seed replacement vector: %v", err)
	}

	res := h.pipeline.Process(context.Background(), "r1")
	if res.Outcome != dispatch.Done {
		t.Fatalf("Process result = %+v, want Done", res)
	}

	if h.embedder.Calls() != 1 {
		t.Fatalf("embedder calls = %d, want 1 (reuse path must re-embed to upsert)", h.embedder.Calls())
	}
	got, err := h.vectors.Query(context.Background(), replacementVec, 1, "")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 || got[0].ID != "r1" || got[0].Score < 0.99 {
		t.Fatalf("replacement vector was overwritten by stale reuse upsert: %+v", got)
	}
}

// failReplaceTagsStore delegates everything to a real store but fails ReplaceTags
// until failUntil successful runs have been attempted, for the post-summarize
// retry path.
type failReplaceTagsStore struct {
	*store.Memory
	failCalls int
	calls     int
}

func (s *failReplaceTagsStore) ReplaceTags(ctx context.Context, id string, tags []string, fields store.TagFields) error {
	s.calls++
	if s.calls <= s.failCalls {
		return errors.New("tags write failed")
	}
	return s.Memory.ReplaceTags(ctx, id, tags, fields)
}

func TestPipeline_ReplaceTagsError_Retries(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	stub := &failReplaceTagsStore{Memory: h.store, failCalls: 1} // fail the first ReplaceTags only
	h.pipeline.Store = stub
	h.seed(t, "r1", "https://example.com/post")

	// Run 1: a post-summarize failure (the tag write) is transient -> pending.
	h.dispatcher.Submit("r1")

	got := h.get(t, "r1")
	if got.Status != reading.Pending {
		t.Fatalf("status = %q, want pending (a tag-persist error is transient)", got.Status)
	}
	if h.summarizer.Calls() != 1 {
		t.Fatalf("summarizer calls after run 1 = %d, want 1", h.summarizer.Calls())
	}

	// Run 2 (re-entry from the content checkpoint): fetch/extract are skipped,
	// vector embedding/upsert may run again, and summarize deliberately runs again
	// (per-run, PLAN §7) before the now-
	// succeeding tag write -> ready.
	h.delay.FireAll()

	got = h.get(t, "r1")
	if got.Status != reading.Ready {
		t.Fatalf("status after retry = %q, want ready", got.Status)
	}
	if h.fetcher.Calls() != 1 || h.embedder.Calls() != 2 {
		t.Fatalf("fetch/embed calls = %d/%d, want 1/2 (re-entry skips fetch and re-upserts vector)", h.fetcher.Calls(), h.embedder.Calls())
	}
	if h.summarizer.Calls() != 2 {
		t.Fatalf("summarizer calls total = %d, want 2 (re-summarized once on the retry, by design)", h.summarizer.Calls())
	}
	if !sliceEqual(got.Tags, []string{"go", "db"}) {
		t.Fatalf("tags = %v, want [go db] persisted on the successful retry", got.Tags)
	}
	// The failed first run wrote tags before notifying in the old order, which would
	// have emailed "ready" for a still-pending reading and emailed again on retry.
	// Notify now runs only after the durable writes, so exactly one email is sent.
	if len(h.notifier.Sent()) != 1 {
		t.Fatalf("emails sent = %d, want exactly 1 (no duplicate/early ready email across the retry)", len(h.notifier.Sent()))
	}
}

func TestPipeline_NotifyDisabled_NoEmail(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.pipeline.Config.NotifyEnabled = false
	h.seed(t, "r1", "https://example.com/post")

	h.dispatcher.Submit("r1")

	if got := h.get(t, "r1"); got.Status != reading.Ready {
		t.Fatalf("status = %q, want ready", got.Status)
	}
	if h.notifier.Calls() != 0 {
		t.Fatalf("notifier calls = %d, want 0 when notification is disabled", h.notifier.Calls())
	}
}

func TestPipeline_NotifySubjectFallsBackToURL(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	// No refined title from the summarizer and no extracted title: the notify
	// subject falls back to the URL.
	h.extractor.Article = extract.Article{Markdown: "body", Mode: extract.ModeReadability}
	h.summarizer.Summary = summarize.Summary{Summary: "s", Tags: []string{"t"}}
	h.seed(t, "r1", "https://example.com/post")

	h.dispatcher.Submit("r1")

	if got := h.get(t, "r1"); got.Status != reading.Ready {
		t.Fatalf("status = %q, want ready", got.Status)
	}
	sent := h.notifier.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent = %d, want 1", len(sent))
	}
	if !strings.Contains(sent[0].Subject, "https://example.com/post") {
		t.Fatalf("subject = %q, want it to fall back to the URL", sent[0].Subject)
	}
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsID(matches []vector.Match, id string) bool {
	for _, m := range matches {
		if m.ID == id {
			return true
		}
	}
	return false
}
