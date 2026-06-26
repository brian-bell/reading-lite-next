package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bbell/reading-lite/internal/blobs"
	"github.com/bbell/reading-lite/internal/clock"
	"github.com/bbell/reading-lite/internal/dispatch"
	"github.com/bbell/reading-lite/internal/embed"
	"github.com/bbell/reading-lite/internal/extract"
	"github.com/bbell/reading-lite/internal/fetch"
	"github.com/bbell/reading-lite/internal/httpapi"
	"github.com/bbell/reading-lite/internal/notify"
	"github.com/bbell/reading-lite/internal/pipeline"
	"github.com/bbell/reading-lite/internal/reading"
	"github.com/bbell/reading-lite/internal/store"
	"github.com/bbell/reading-lite/internal/summarize"
	"github.com/bbell/reading-lite/internal/vector"
)

var e2eNow = time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)

type e2eHarness struct {
	store      *store.Memory
	blobs      *blobs.Memory
	fetcher    *fetch.Fake
	extractor  *extract.Fake
	embedder   embed.Embedder
	vectors    *vector.Memory
	summarizer *summarize.Fake
	notifier   *notify.Fake
	clock      *clock.Fake
	delay      *dispatch.FakeDelayer
	pipeline   *pipeline.Pipeline
	dispatcher *dispatch.Dispatcher
	handler    http.Handler
	nextID     int
}

type e2eStatusResponse struct {
	ID     string         `json:"id"`
	Status reading.Status `json:"status"`
}

type e2eReadingResponse struct {
	ID              string          `json:"id"`
	URL             string          `json:"url"`
	Status          reading.Status  `json:"status"`
	Title           string          `json:"title"`
	ExtractionMode  string          `json:"extraction_mode"`
	Summary         string          `json:"summary"`
	SummaryJSON     json.RawMessage `json:"summary_json"`
	SimilarJSON     json.RawMessage `json:"similar_json"`
	DiagnosticsJSON json.RawMessage `json:"diagnostics_json"`
	Error           string          `json:"error"`
	Tags            []string        `json:"tags"`
}

func newE2EHarness(t *testing.T) *e2eHarness {
	t.Helper()

	h := &e2eHarness{
		store:      store.NewMemory(),
		blobs:      blobs.NewMemory(),
		fetcher:    &fetch.Fake{Resource: fetch.Resource{Body: []byte("<html><body>raw source</body></html>"), ContentType: "text/html", Status: http.StatusOK}},
		extractor:  &extract.Fake{Article: extract.Article{Title: "Extracted Title", Author: "Ada", Site: "example.com", Lang: "en", Markdown: "# Extracted\n\nBody text.", Mode: extract.ModeReadability, WordCount: 3}},
		embedder:   &embed.Fake{Vec: e2eUnitVec()},
		vectors:    vector.NewMemory(),
		summarizer: &summarize.Fake{Summary: summarize.Summary{Title: "Refined Title", Summary: "A concise summary.", Tags: []string{"go", "reading"}, JSON: json.RawMessage(`{"headline":"Refined Title"}`)}},
		notifier:   &notify.Fake{},
		clock:      clock.NewFake(e2eNow),
		delay:      &dispatch.FakeDelayer{},
	}
	h.rebuildDispatcher(true)
	return h
}

func (h *e2eHarness) rebuildDispatcher(inline bool) {
	h.pipeline = &pipeline.Pipeline{
		Store:      h.store,
		Blobs:      h.blobs,
		Fetcher:    h.fetcher,
		Extractor:  h.extractor,
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
		Max:     3,
		Buffer:  8,
		// Inline is the deterministic E2E drain seam; dispatch worker-pool
		// concurrency is covered in internal/dispatch.
		Inline: inline,
	}
	srv := &httpapi.Server{
		Store:      h.store,
		Blobs:      h.blobs,
		Dispatcher: h.dispatcher,
		Clock:      h.clock,
		Token:      "secret-token",
		TTLs: reading.TTLs{
			Pending: 5 * time.Minute,
			Running: 5 * time.Minute,
		},
		NewID: func() string {
			h.nextID++
			return fmt.Sprintf("e2e-r%d", h.nextID)
		},
	}
	h.handler = srv.Routes()
}

func (h *e2eHarness) authed(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(data))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer secret-token")
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	return rr
}

func (h *e2eHarness) ingest(t *testing.T, rawURL string) e2eStatusResponse {
	t.Helper()

	rr := h.authed(t, http.MethodPost, "/api/readings", map[string]string{"url": rawURL})
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST /api/readings status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	return decodeE2E[e2eStatusResponse](t, rr)
}

func (h *e2eHarness) detail(t *testing.T, id string) e2eReadingResponse {
	t.Helper()

	rr := h.authed(t, http.MethodGet, "/api/readings/"+id, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET detail status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	return decodeE2E[e2eReadingResponse](t, rr)
}

func (h *e2eHarness) content(t *testing.T, id string) string {
	t.Helper()

	rr := h.authed(t, http.MethodGet, "/api/readings/"+id+"/content", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET content status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

func (h *e2eHarness) reprocess(t *testing.T, id string) e2eStatusResponse {
	t.Helper()

	rr := h.authed(t, http.MethodPost, "/api/readings/"+id+"/reprocess", nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("POST reprocess status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	return decodeE2E[e2eStatusResponse](t, rr)
}

func decodeE2E[T any](t *testing.T, rr *httptest.ResponseRecorder) T {
	t.Helper()

	var out T
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %q: %v", rr.Body.String(), err)
	}
	return out
}

func e2eUnitVec() []float32 {
	v := make([]float32, embed.Dim)
	v[0] = 1
	return v
}

func TestE2E_IngestProcessRead(t *testing.T) {
	t.Parallel()

	h := newE2EHarness(t)
	created := h.ingest(t, "https://example.com/a")
	if created.ID == "" {
		t.Fatal("created id empty")
	}

	got := h.detail(t, created.ID)
	if got.Status != reading.Ready {
		t.Fatalf("status = %q, want ready", got.Status)
	}
	if got.Title != "Refined Title" {
		t.Fatalf("title = %q, want summarizer-refined title", got.Title)
	}
	if got.Summary != "A concise summary." {
		t.Fatalf("summary = %q, want summarizer summary", got.Summary)
	}
	if got.ExtractionMode != string(extract.ModeReadability) {
		t.Fatalf("extraction_mode = %q, want readability", got.ExtractionMode)
	}
	if !sameStrings(got.Tags, []string{"go", "reading"}) {
		t.Fatalf("tags = %v, want [go reading]", got.Tags)
	}

	var summary map[string]string
	if err := json.Unmarshal(got.SummaryJSON, &summary); err != nil {
		t.Fatalf("summary_json = %s, want valid JSON: %v", got.SummaryJSON, err)
	}
	if summary["headline"] != "Refined Title" {
		t.Fatalf("summary_json headline = %q, want Refined Title", summary["headline"])
	}
	var diagnostics map[string]any
	if err := json.Unmarshal(got.DiagnosticsJSON, &diagnostics); err != nil {
		t.Fatalf("diagnostics_json = %s, want valid JSON: %v", got.DiagnosticsJSON, err)
	}
	if diagnostics["source"] != string(reading.SourceWeb) {
		t.Fatalf("diagnostics source = %v, want web", diagnostics["source"])
	}
	if raw := strings.TrimSpace(string(got.SimilarJSON)); raw != "" && raw != "[]" {
		t.Fatalf("similar_json = %s, want empty list for first reading", got.SimilarJSON)
	}

	if body := h.content(t, created.ID); body != "# Extracted\n\nBody text." {
		t.Fatalf("content = %q, want extracted markdown", body)
	}
	if h.notifier.Calls() != 1 {
		t.Fatalf("notifier calls = %d, want 1", h.notifier.Calls())
	}
	sent := h.notifier.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent notifications = %d, want 1", len(sent))
	}
	if sent[0].From != "reader@example.com" || sent[0].To != "me@example.com" {
		t.Fatalf("notification route = %q -> %q, want reader@example.com -> me@example.com", sent[0].From, sent[0].To)
	}
	if !strings.Contains(sent[0].Subject, "Refined Title") || !strings.Contains(sent[0].HTML, "A concise summary.") {
		t.Fatalf("notification subject/html = %q/%q, want ready reading summary", sent[0].Subject, sent[0].HTML)
	}
}

func TestE2E_SimilarAcrossTwoReadings(t *testing.T) {
	t.Parallel()

	h := newE2EHarness(t)
	first := h.ingest(t, "https://example.com/a")
	firstDetail := h.detail(t, first.ID)
	if firstDetail.Status != reading.Ready {
		t.Fatalf("first status = %q, want ready before ingesting second reading", firstDetail.Status)
	}

	second := h.ingest(t, "https://example.com/b")
	secondDetail := h.detail(t, second.ID)
	if secondDetail.Status != reading.Ready {
		t.Fatalf("second status = %q, want ready", secondDetail.Status)
	}
	matches := similarItems(t, secondDetail.SimilarJSON)
	if len(matches) != 1 {
		t.Fatalf("similar items = %+v, want one ready prior reading", matches)
	}
	got := matches[0]
	if got.ID != first.ID {
		t.Fatalf("similar id = %q, want %q", got.ID, first.ID)
	}
	if got.Title != firstDetail.Title {
		t.Fatalf("similar title = %q, want %q", got.Title, firstDetail.Title)
	}
	if got.URL != firstDetail.URL {
		t.Fatalf("similar url = %q, want %q", got.URL, firstDetail.URL)
	}
	if got.Score <= 0 {
		t.Fatalf("similar score = %v, want positive score", got.Score)
	}
}

func TestE2E_FailedThenReprocessSucceeds(t *testing.T) {
	t.Parallel()

	h := newE2EHarness(t)
	h.fetcher.Err = dispatch.ErrPermanent
	created := h.ingest(t, "https://example.com/fails-once")

	failed := h.detail(t, created.ID)
	if failed.Status != reading.Failed {
		t.Fatalf("status after first ingest = %q, want failed", failed.Status)
	}
	if failed.Error == "" {
		t.Fatal("error after first ingest empty, want failure reason")
	}
	if h.fetcher.Calls() != 1 {
		t.Fatalf("fetcher calls after failure = %d, want 1", h.fetcher.Calls())
	}
	if h.extractor.Calls() != 0 {
		t.Fatalf("extractor calls after fetch failure = %d, want 0", h.extractor.Calls())
	}
	if h.notifier.Calls() != 0 {
		t.Fatalf("notifier calls after failure = %d, want 0", h.notifier.Calls())
	}

	h.fetcher.Err = nil
	res := h.reprocess(t, created.ID)
	if res.ID != created.ID {
		t.Fatalf("reprocess id = %q, want %q", res.ID, created.ID)
	}
	if res.Status != reading.Pending {
		t.Fatalf("reprocess response status = %q, want pending", res.Status)
	}

	got := h.detail(t, created.ID)
	if got.Status != reading.Ready {
		t.Fatalf("status after reprocess = %q, want ready", got.Status)
	}
	if got.Error != "" {
		t.Fatalf("error after reprocess = %q, want cleared", got.Error)
	}
	if got.Summary != "A concise summary." {
		t.Fatalf("summary after reprocess = %q, want summarizer summary", got.Summary)
	}
	if h.fetcher.Calls() != 2 {
		t.Fatalf("fetcher calls after reprocess = %d, want 2", h.fetcher.Calls())
	}
	if h.extractor.Calls() != 1 {
		t.Fatalf("extractor calls after reprocess = %d, want 1", h.extractor.Calls())
	}
	if body := h.content(t, created.ID); body != "# Extracted\n\nBody text." {
		t.Fatalf("content after reprocess = %q, want extracted markdown", body)
	}
}

func TestE2E_RetryExhaustionFailsRetryable(t *testing.T) {
	t.Parallel()

	h := newE2EHarness(t)
	h.extractor.Err = errors.New("extract 503")
	created := h.ingest(t, "https://example.com/retry-until-exhausted")

	h.drainDelays(t, 5)

	got := h.detail(t, created.ID)
	if got.Status != reading.Failed {
		t.Fatalf("status = %q, want failed after retry exhaustion", got.Status)
	}
	if !strings.Contains(got.Error, "retry budget exhausted") {
		t.Fatalf("error = %q, want retry budget exhaustion", got.Error)
	}
	stored, err := h.store.GetByID(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetByID(%q): %v", created.ID, err)
	}
	if stored.ProcessAttempts != 2 {
		t.Fatalf("process_attempts = %d, want 2 for Max=3 terminal attempt", stored.ProcessAttempts)
	}
	if h.delay.PendingLen() != 0 {
		t.Fatalf("pending delays = %d, want dispatcher idle", h.delay.PendingLen())
	}
	if h.extractor.Calls() != 3 {
		t.Fatalf("extractor calls = %d, want 3 attempts", h.extractor.Calls())
	}

	h.extractor.Err = nil
	h.reprocess(t, created.ID)
	if got := h.detail(t, created.ID); got.Status != reading.Ready {
		t.Fatalf("status after reprocess = %q, want ready", got.Status)
	}
}

func TestE2E_RateLimitRequeue(t *testing.T) {
	t.Parallel()

	h := newE2EHarness(t)
	emb := &onceRateLimitedEmbedder{vec: e2eUnitVec()}
	h.embedder = emb
	h.rebuildDispatcher(true)

	created := h.ingest(t, "https://example.com/rate-limited")
	got := h.detail(t, created.ID)
	if got.Status != reading.Pending {
		t.Fatalf("status before delayed requeue = %q, want pending", got.Status)
	}
	if h.delay.PendingLen() != 1 {
		t.Fatalf("pending delays = %d, want 1", h.delay.PendingLen())
	}
	if durations := h.delay.Durations(); len(durations) != 1 || durations[0] != 30*time.Second {
		t.Fatalf("delay durations = %v, want [30s]", durations)
	}
	stored, err := h.store.GetByID(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetByID(%q): %v", created.ID, err)
	}
	if stored.ProcessAttempts != 0 {
		t.Fatalf("process_attempts before requeue = %d, want 0", stored.ProcessAttempts)
	}
	if h.summarizer.Calls() != 0 {
		t.Fatalf("summarizer calls before requeue = %d, want 0", h.summarizer.Calls())
	}
	if h.notifier.Calls() != 0 {
		t.Fatalf("notifier calls before requeue = %d, want 0", h.notifier.Calls())
	}

	h.delay.FireAll()

	got = h.detail(t, created.ID)
	if got.Status != reading.Ready {
		t.Fatalf("status after delayed requeue = %q, want ready", got.Status)
	}
	if h.delay.PendingLen() != 0 {
		t.Fatalf("pending delays after requeue = %d, want 0", h.delay.PendingLen())
	}
	if got.Summary != "A concise summary." {
		t.Fatalf("summary after requeue = %q, want summarizer summary", got.Summary)
	}
	if emb.Calls() != 2 {
		t.Fatalf("embedder calls = %d, want 2", emb.Calls())
	}
	if h.notifier.Calls() != 1 {
		t.Fatalf("notifier calls after requeue = %d, want 1", h.notifier.Calls())
	}
	if body := h.content(t, created.ID); body != "# Extracted\n\nBody text." {
		t.Fatalf("content after requeue = %q, want extracted markdown", body)
	}
}

func TestE2E_RecoveryAfterRestart(t *testing.T) {
	t.Parallel()

	h := newE2EHarness(t)
	h.rebuildDispatcher(false)
	created := h.ingest(t, "https://example.com/recovered-after-restart")

	got := h.detail(t, created.ID)
	if got.Status != reading.Pending {
		t.Fatalf("status before sweep = %q, want pending", got.Status)
	}
	if h.fetcher.Calls() != 0 {
		t.Fatalf("fetcher calls before sweep = %d, want 0", h.fetcher.Calls())
	}
	if h.extractor.Calls() != 0 {
		t.Fatalf("extractor calls before sweep = %d, want 0", h.extractor.Calls())
	}
	if h.summarizer.Calls() != 0 {
		t.Fatalf("summarizer calls before sweep = %d, want 0", h.summarizer.Calls())
	}
	if h.delay.PendingLen() != 0 {
		t.Fatalf("pending delays before sweep = %d, want 0", h.delay.PendingLen())
	}

	h.rebuildDispatcher(true)
	if err := h.dispatcher.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	got = h.detail(t, created.ID)
	if got.Status != reading.Ready {
		t.Fatalf("status after sweep = %q, want ready", got.Status)
	}
	if body := h.content(t, created.ID); body != "# Extracted\n\nBody text." {
		t.Fatalf("content after sweep = %q, want extracted markdown", body)
	}
}

type e2eSimilarItem struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
	Title string  `json:"title"`
	URL   string  `json:"url"`
}

func similarItems(t *testing.T, raw json.RawMessage) []e2eSimilarItem {
	t.Helper()

	var out []e2eSimilarItem
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("similar_json = %s, want valid similar list: %v", raw, err)
	}
	return out
}

type onceRateLimitedEmbedder struct {
	mu    sync.Mutex
	calls int
	vec   []float32
}

func (e *onceRateLimitedEmbedder) Embed(ctx context.Context, _ string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	e.calls++
	if e.calls == 1 {
		return nil, &dispatch.RateLimitError{RetryAfter: 30 * time.Second}
	}
	return append([]float32(nil), e.vec...), nil
}

func (e *onceRateLimitedEmbedder) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.calls
}

func (h *e2eHarness) drainDelays(t *testing.T, limit int) {
	t.Helper()

	for i := range limit {
		if h.delay.PendingLen() == 0 {
			return
		}
		h.delay.FireAll()
		if i == limit-1 && h.delay.PendingLen() != 0 {
			t.Fatalf("pending delays still rescheduling after %d drains; durations=%v", limit, h.delay.Durations())
		}
	}
}

func sameStrings(a, b []string) bool {
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
