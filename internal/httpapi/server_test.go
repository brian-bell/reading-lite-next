package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/bbell/reading-lite/internal/blobs"
	"github.com/bbell/reading-lite/internal/clock"
	"github.com/bbell/reading-lite/internal/httpapi"
	"github.com/bbell/reading-lite/internal/reading"
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

func (s *submitter) ForceSubmit(id string) {
	s.forcedIDs = append(s.forcedIDs, id)
}

type harness struct {
	store     *store.Memory
	blobs     *blobs.Memory
	clock     *clock.Fake
	submitter *submitter
	handler   http.Handler
	nextID    int
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	h := &harness{
		store:     store.NewMemory(),
		blobs:     blobs.NewMemory(),
		clock:     clock.NewFake(testNow),
		submitter: &submitter{},
	}
	srv := &httpapi.Server{
		Store:      h.store,
		Blobs:      h.blobs,
		Dispatcher: h.submitter,
		Clock:      h.clock,
		Token:      "secret-token",
		TTLs: reading.TTLs{
			Pending: 5 * time.Minute,
			Running: 5 * time.Minute,
		},
		NewID: func() string {
			h.nextID++
			return "r" + string(rune('0'+h.nextID))
		},
	}
	h.handler = srv.Routes()
	return h
}

func (h *harness) request(t *testing.T, method, path string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()

	var rbody *bytes.Reader
	if body == nil {
		rbody = bytes.NewReader(nil)
	} else {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		rbody = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, rbody)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	return rr
}

func (h *harness) authed(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return h.request(t, method, path, body, "secret-token")
}

func (h *harness) rawRequest(t *testing.T, method, path, body, contentType, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	return rr
}

func decodeJSON[T any](t *testing.T, rr *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %q: %v", rr.Body.String(), err)
	}
	return out
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

func TestAuth_MissingTokenRejected(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	rr := h.request(t, http.MethodGet, "/api/readings", nil, "")

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestAuth_WrongTokenRejected(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	rr := h.request(t, http.MethodGet, "/api/readings", nil, "wrong")

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestAuth_RawTokenRejected(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	rr := h.rawRequest(t, http.MethodGet, "/api/readings", "", "", "secret-token")

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestAuth_EmptyConfiguredTokenRejectsProtectedRoutes(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	srv := &httpapi.Server{
		Store:      h.store,
		Blobs:      h.blobs,
		Dispatcher: h.submitter,
		Clock:      h.clock,
		Token:      "",
	}
	h.handler = srv.Routes()

	rr := h.request(t, http.MethodGet, "/api/readings", nil, "")

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestAuth_HealthzSkipsAuth(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	rr := h.request(t, http.MethodGet, "/api/healthz", nil, "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestAuth_ValidTokenPasses(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	rr := h.authed(t, http.MethodGet, "/api/readings", nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

func TestErrors_UnmatchedRouteUsesJSONEnvelope(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	rr := h.authed(t, http.MethodGet, "/api/does-not-exist", nil)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	got := decodeJSON[struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}](t, rr)
	if got.Error.Code != "not_found" {
		t.Fatalf("error code = %q, want not_found", got.Error.Code)
	}
}

func TestErrors_MethodMismatchUsesJSONEnvelope(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	rr := h.authed(t, http.MethodPut, "/api/readings/r1", nil)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
	got := decodeJSON[struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}](t, rr)
	if got.Error.Code != "method_not_allowed" {
		t.Fatalf("error code = %q, want method_not_allowed", got.Error.Code)
	}
}

func TestIngest_NewURLCreatesPending(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	rr := h.authed(t, http.MethodPost, "/api/readings", map[string]string{"url": "https://example.com/post"})

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	got := decodeJSON[struct {
		ID     string         `json:"id"`
		Status reading.Status `json:"status"`
	}](t, rr)
	if got.ID != "r1" || got.Status != reading.Pending {
		t.Fatalf("response = %+v, want id r1 pending", got)
	}
	stored, err := h.store.GetByID(context.Background(), "r1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.Status != reading.Pending || stored.SourceKind != reading.SourceWeb {
		t.Fatalf("stored status/source = %q/%q, want pending/web", stored.Status, stored.SourceKind)
	}
	if diff := cmp.Diff([]string{"r1"}, h.submitter.ids); diff != "" {
		t.Fatalf("submitted ids mismatch (-want +got):\n%s", diff)
	}
}

func TestIngest_MarkdownURLCreatesFetchableWebReading(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	rr := h.authed(t, http.MethodPost, "/api/readings", map[string]string{"url": "https://example.com/notes.md"})

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	stored, err := h.store.GetByID(context.Background(), "r1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.SourceKind != reading.SourceWeb || stored.RawKey != "" {
		t.Fatalf("stored source/raw = %q/%q, want web with no raw key", stored.SourceKind, stored.RawKey)
	}
}

func TestIngest_ExistingReadyReturnsSame(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	seedReading(t, h, reading.Reading{ID: "ready", URL: "https://example.com/post", Status: reading.Ready})

	rr := h.authed(t, http.MethodPost, "/api/readings", map[string]string{"url": "https://example.com/post"})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	got := decodeJSON[struct {
		ID     string         `json:"id"`
		Status reading.Status `json:"status"`
	}](t, rr)
	if got.ID != "ready" || got.Status != reading.Ready {
		t.Fatalf("response = %+v, want existing ready", got)
	}
	if len(h.submitter.ids) != 0 {
		t.Fatalf("submitted ids = %v, want none", h.submitter.ids)
	}
}

func TestIngest_ExistingPendingReturnsSame(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	seedReading(t, h, reading.Reading{ID: "pending", URL: "https://example.com/post", Status: reading.Pending})

	rr := h.authed(t, http.MethodPost, "/api/readings", map[string]string{"url": "https://example.com/post"})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	got := decodeJSON[struct {
		ID string `json:"id"`
	}](t, rr)
	if got.ID != "pending" {
		t.Fatalf("id = %q, want pending", got.ID)
	}
	if len(h.submitter.ids) != 0 {
		t.Fatalf("submitted ids = %v, want none", h.submitter.ids)
	}
}

func TestIngest_FailedReprocessesInPlace(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	seedReading(t, h, reading.Reading{ID: "failed", URL: "https://example.com/post", Status: reading.Failed, Error: "boom"})

	rr := h.authed(t, http.MethodPost, "/api/readings", map[string]string{"url": "https://example.com/post"})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	stored, err := h.store.GetByID(context.Background(), "failed")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.Status != reading.Pending || stored.Error != "" {
		t.Fatalf("stored status/error = %q/%q, want pending with cleared error", stored.Status, stored.Error)
	}
	if diff := cmp.Diff([]string{"failed"}, h.submitter.ids); diff != "" {
		t.Fatalf("submitted ids mismatch (-want +got):\n%s", diff)
	}
}

func TestIngest_NormalizesBeforeLookup(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	first := h.authed(t, http.MethodPost, "/api/readings", map[string]string{"url": "https://example.com/post?utm_source=a"})
	second := h.authed(t, http.MethodPost, "/api/readings", map[string]string{"url": "https://example.com/post?utm_campaign=b"})

	if first.Code != http.StatusCreated || second.Code != http.StatusOK {
		t.Fatalf("statuses = %d/%d, want 201/200; second body=%s", first.Code, second.Code, second.Body.String())
	}
	got := decodeJSON[struct {
		ID string `json:"id"`
	}](t, second)
	if got.ID != "r1" {
		t.Fatalf("second id = %q, want first id r1", got.ID)
	}
	if diff := cmp.Diff([]string{"r1"}, h.submitter.ids); diff != "" {
		t.Fatalf("submitted ids mismatch (-want +got):\n%s", diff)
	}
}

func TestIngest_InvalidURL(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	rr := h.authed(t, http.MethodPost, "/api/readings", map[string]string{"url": "notaurl"})

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	got := decodeJSON[struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}](t, rr)
	if got.Error.Code != "invalid_url" {
		t.Fatalf("error code = %q, want invalid_url", got.Error.Code)
	}
}

func TestGetReading_AnnotatesStaleAtReadTime(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	started := testNow.Add(-10 * time.Minute)
	seedReading(t, h, reading.Reading{
		ID:        "stale",
		URL:       "https://example.com/stale",
		Status:    reading.Running,
		StartedAt: &started,
		CreatedAt: started,
		UpdatedAt: started,
	})

	rr := h.authed(t, http.MethodGet, "/api/readings/stale", nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	got := decodeJSON[struct {
		ID          string         `json:"id"`
		Status      reading.Status `json:"status"`
		StaleReason string         `json:"stale_reason"`
	}](t, rr)
	if got.ID != "stale" || got.Status != reading.Failed || got.StaleReason == "" {
		t.Fatalf("response = %+v, want stale failed annotation", got)
	}
	stored, err := h.store.GetByID(context.Background(), "stale")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.Status != reading.Running {
		t.Fatalf("stored status = %q, want running (read-time overlay only)", stored.Status)
	}
}

func TestGetReading_DoesNotExposeInternalColumns(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	seedReading(t, h, reading.Reading{
		ID:              "r1",
		URL:             "https://example.com/post",
		Status:          reading.Ready,
		ContentKey:      "content/r1",
		RawKey:          "raw/r1",
		ProcessAttempts: 3,
	})

	rr := h.authed(t, http.MethodGet, "/api/readings/r1", nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	for _, field := range []string{"url_key", "content_key", "raw_key", "process_attempts"} {
		if _, ok := got[field]; ok {
			t.Fatalf("response exposed internal field %q: %s", field, rr.Body.String())
		}
	}
}

func TestListReadings_QTagsSortPaginate(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	seedReading(t, h, reading.Reading{ID: "old", URL: "https://example.com/old", Status: reading.Ready, Title: "Go old", Summary: "database", Tags: []string{"go", "db"}, CreatedAt: testNow.Add(-2 * time.Hour), UpdatedAt: testNow})
	seedReading(t, h, reading.Reading{ID: "new", URL: "https://example.com/new", Status: reading.Ready, Title: "Go new", Summary: "database", Tags: []string{"go", "db"}, CreatedAt: testNow.Add(-1 * time.Hour), UpdatedAt: testNow})
	seedReading(t, h, reading.Reading{ID: "miss", URL: "https://example.com/miss", Status: reading.Ready, Title: "Other", Summary: "database", Tags: []string{"db"}, CreatedAt: testNow, UpdatedAt: testNow})

	rr := h.authed(t, http.MethodGet, "/api/readings?q=go&tags=go,db&status=ready&sort=newest&limit=1", nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	first := decodeJSON[struct {
		Total      int    `json:"total"`
		NextCursor string `json:"next_cursor"`
		Readings   []struct {
			ID string `json:"id"`
		} `json:"readings"`
	}](t, rr)
	if first.Total != 2 || len(first.Readings) != 1 || first.Readings[0].ID != "new" || first.NextCursor == "" {
		t.Fatalf("first page = %+v, want new with cursor and total 2", first)
	}

	next := h.authed(t, http.MethodGet, "/api/readings?q=go&tags=go,db&status=ready&sort=newest&limit=1&cursor="+first.NextCursor, nil)
	if next.Code != http.StatusOK {
		t.Fatalf("next status = %d, want 200; body=%s", next.Code, next.Body.String())
	}
	second := decodeJSON[struct {
		Readings []struct {
			ID string `json:"id"`
		} `json:"readings"`
	}](t, next)
	if len(second.Readings) != 1 || second.Readings[0].ID != "old" {
		t.Fatalf("second page = %+v, want old", second)
	}
}

func TestGetContent_AuthGatedAndStreamsBlob(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	seedReading(t, h, reading.Reading{ID: "r1", URL: "https://example.com/post", Status: reading.Ready, ContentKey: "content/r1"})
	if err := h.blobs.Put(context.Background(), "content/r1", []byte("# Article"), "text/markdown"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	missingAuth := h.request(t, http.MethodGet, "/api/readings/r1/content", nil, "")
	if missingAuth.Code != http.StatusUnauthorized {
		t.Fatalf("missing auth status = %d, want 401", missingAuth.Code)
	}

	rr := h.authed(t, http.MethodGet, "/api/readings/r1/content", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "# Article" {
		t.Fatalf("body = %q, want markdown", rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "text/markdown" {
		t.Fatalf("content-type = %q, want text/markdown", got)
	}
}

func TestGetRaw_Returns404WhenBlobMissing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	seedReading(t, h, reading.Reading{ID: "r1", URL: "https://example.com/post", Status: reading.Ready, RawKey: "raw/r1"})

	rr := h.authed(t, http.MethodGet, "/api/readings/r1/raw", nil)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestGetRaw_DoesNotServeExecutableHTML(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	seedReading(t, h, reading.Reading{ID: "r1", URL: "https://example.com/post", Status: reading.Ready, RawKey: "raw/r1"})
	if err := h.blobs.Put(context.Background(), "raw/r1", []byte("<script>alert(1)</script>"), "text/html"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rr := h.authed(t, http.MethodGet, "/api/readings/r1/raw", nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("content-type = %q, want application/octet-stream", got)
	}
	if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("nosniff = %q, want nosniff", got)
	}
	if got := rr.Header().Get("Content-Disposition"); !strings.Contains(got, "attachment") {
		t.Fatalf("content-disposition = %q, want attachment", got)
	}
}

func TestReprocess_ReenqueuesAndReturns202(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	started := testNow.Add(-time.Minute)
	finished := testNow.Add(-30 * time.Second)
	seedReading(t, h, reading.Reading{
		ID:              "r1",
		URL:             "https://example.com/post",
		Status:          reading.Ready,
		Error:           "previous failure",
		ProcessAttempts: 4,
		StartedAt:       &started,
		FinishedAt:      &finished,
		Title:           "Old title",
		ContentKey:      "readings/r1/content",
		RawKey:          "readings/r1/raw",
		Summary:         "Old summary",
	})

	rr := h.authed(t, http.MethodPost, "/api/readings/r1/reprocess", nil)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	stored, err := h.store.GetByID(context.Background(), "r1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.Status != reading.Pending {
		t.Fatalf("stored status = %q, want pending", stored.Status)
	}
	if stored.Error != "" || stored.ProcessAttempts != 0 || stored.StartedAt != nil || stored.FinishedAt != nil {
		t.Fatalf("stored lifecycle after reprocess = error %q attempts %d started %v finished %v, want cleared",
			stored.Error, stored.ProcessAttempts, stored.StartedAt, stored.FinishedAt)
	}
	if stored.Title != "" || stored.ContentKey != "" || stored.RawKey != "" || stored.Summary != "" {
		t.Fatalf("stored content after reprocess = title %q content %q raw %q summary %q, want cleared",
			stored.Title, stored.ContentKey, stored.RawKey, stored.Summary)
	}
	if diff := cmp.Diff([]string{"r1"}, h.submitter.ids); diff != "" {
		t.Fatalf("submitted ids mismatch (-want +got):\n%s", diff)
	}
}

func TestReprocess_MarkdownImportPreservesTitle(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
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

	rr := h.authed(t, http.MethodPost, "/api/readings/r1/reprocess", nil)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
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
}

func TestReprocess_OldReadingDoesNotStayStale(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	seedReading(t, h, reading.Reading{
		ID:        "r1",
		URL:       "https://example.com/post",
		Status:    reading.Ready,
		CreatedAt: testNow.Add(-24 * time.Hour),
		UpdatedAt: testNow.Add(-23 * time.Hour),
	})

	rr := h.authed(t, http.MethodPost, "/api/readings/r1/reprocess", nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("reprocess status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	detail := h.authed(t, http.MethodGet, "/api/readings/r1", nil)
	if detail.Code != http.StatusOK {
		t.Fatalf("detail status = %d, want 200; body=%s", detail.Code, detail.Body.String())
	}
	got := decodeJSON[struct {
		Status      reading.Status `json:"status"`
		StaleReason string         `json:"stale_reason"`
	}](t, detail)
	if got.Status != reading.Pending || got.StaleReason != "" {
		t.Fatalf("detail after reprocess = %q/%q, want fresh pending", got.Status, got.StaleReason)
	}
}

func TestReprocess_PendingIsIdempotent(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	seedReading(t, h, reading.Reading{
		ID:              "r1",
		URL:             "https://example.com/post",
		Status:          reading.Pending,
		ProcessAttempts: 2,
	})

	rr := h.authed(t, http.MethodPost, "/api/readings/r1/reprocess", nil)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	got := decodeJSON[struct {
		ID     string         `json:"id"`
		Status reading.Status `json:"status"`
	}](t, rr)
	if got.ID != "r1" || got.Status != reading.Pending {
		t.Fatalf("response = %+v, want existing pending", got)
	}
	stored, err := h.store.GetByID(context.Background(), "r1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.ProcessAttempts != 2 {
		t.Fatalf("ProcessAttempts = %d, want unchanged 2", stored.ProcessAttempts)
	}
	if len(h.submitter.ids) != 0 {
		t.Fatalf("submitted ids = %v, want none", h.submitter.ids)
	}
}

func TestReprocess_RunningIsIdempotent(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	started := testNow.Add(-time.Minute)
	seedReading(t, h, reading.Reading{
		ID:              "r1",
		URL:             "https://example.com/post",
		Status:          reading.Running,
		ProcessAttempts: 1,
		StartedAt:       &started,
	})

	rr := h.authed(t, http.MethodPost, "/api/readings/r1/reprocess", nil)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	got := decodeJSON[struct {
		Status reading.Status `json:"status"`
	}](t, rr)
	if got.Status != reading.Running {
		t.Fatalf("response status = %q, want running", got.Status)
	}
	stored, err := h.store.GetByID(context.Background(), "r1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.Status != reading.Running || stored.ProcessAttempts != 1 || stored.StartedAt == nil {
		t.Fatalf("stored lifecycle = status %q attempts %d started %v, want unchanged running",
			stored.Status, stored.ProcessAttempts, stored.StartedAt)
	}
	if len(h.submitter.ids) != 0 || len(h.submitter.forcedIDs) != 0 {
		t.Fatalf("submitted ids = %v forced = %v, want none", h.submitter.ids, h.submitter.forcedIDs)
	}
}

func TestReprocess_StaleRunningReenqueues(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	started := testNow.Add(-10 * time.Minute)
	seedReading(t, h, reading.Reading{
		ID:              "r1",
		URL:             "https://example.com/post",
		Status:          reading.Running,
		ProcessAttempts: 2,
		StartedAt:       &started,
	})

	rr := h.authed(t, http.MethodPost, "/api/readings/r1/reprocess", nil)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	got := decodeJSON[struct {
		Status reading.Status `json:"status"`
	}](t, rr)
	if got.Status != reading.Pending {
		t.Fatalf("response status = %q, want pending", got.Status)
	}
	stored, err := h.store.GetByID(context.Background(), "r1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.Status != reading.Pending || stored.ProcessAttempts != 0 || stored.StartedAt != nil {
		t.Fatalf("stored lifecycle = status %q attempts %d started %v, want reset pending",
			stored.Status, stored.ProcessAttempts, stored.StartedAt)
	}
	if len(h.submitter.ids) != 0 {
		t.Fatalf("submitted ids = %v, want none", h.submitter.ids)
	}
	if diff := cmp.Diff([]string{"r1"}, h.submitter.forcedIDs); diff != "" {
		t.Fatalf("force-submitted ids mismatch (-want +got):\n%s", diff)
	}
}

func TestImportMarkdown_CreatesReadingAndEnqueues(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	rr := h.authed(t, http.MethodPost, "/api/readings/import/markdown", map[string]any{
		"url":      "https://example.com/notes.md",
		"markdown": "# Notes\n\nBody",
		"title":    "Notes",
		"tags":     []string{"personal"},
	})

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
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
	if diff := cmp.Diff([]string{"r1"}, h.submitter.ids); diff != "" {
		t.Fatalf("submitted ids mismatch (-want +got):\n%s", diff)
	}
}

func TestImportMarkdown_ReprocessesExistingFailedReading(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	seedReading(t, h, reading.Reading{
		ID:         "failed",
		URL:        "https://www.reddit.com/r/golang/comments/abc/post",
		Status:     reading.Failed,
		SourceKind: reading.SourceReddit,
		Error:      "reddit blocked",
	})

	rr := h.authed(t, http.MethodPost, "/api/readings/import/markdown", map[string]any{
		"url":      "https://www.reddit.com/r/golang/comments/abc/post",
		"markdown": "# Imported\n\nReddit body.",
		"title":    "Imported Reddit",
		"tags":     []string{"reddit"},
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	got := decodeJSON[struct {
		ID     string         `json:"id"`
		Status reading.Status `json:"status"`
	}](t, rr)
	if got.ID != "failed" || got.Status != reading.Pending {
		t.Fatalf("response = %+v, want same failed id pending", got)
	}
	stored, err := h.store.GetByID(context.Background(), "failed")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.Status != reading.Pending || stored.SourceKind != reading.SourceMarkdown || stored.Error != "" {
		t.Fatalf("stored status/source/error = %q/%q/%q, want pending/markdown/empty",
			stored.Status, stored.SourceKind, stored.Error)
	}
	if stored.RawKey == "" || stored.Title != "Imported Reddit" {
		t.Fatalf("stored raw/title = %q/%q, want raw key and imported title", stored.RawKey, stored.Title)
	}
	data, _, err := h.blobs.Get(context.Background(), stored.RawKey)
	if err != nil {
		t.Fatalf("raw blob: %v", err)
	}
	if string(data) != "# Imported\n\nReddit body." {
		t.Fatalf("raw blob = %q, want imported markdown", data)
	}
	if diff := cmp.Diff([]string{"failed"}, h.submitter.ids); diff != "" {
		t.Fatalf("submitted ids mismatch (-want +got):\n%s", diff)
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

	h := newHarness(t)
	mem := store.NewMemory()
	h.store = mem
	h.blobs = blobs.NewMemory()
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
	srv := &httpapi.Server{
		Store:      failingUpdateImportStore{Memory: mem, err: errors.New("update import failed")},
		Blobs:      h.blobs,
		Dispatcher: h.submitter,
		Clock:      h.clock,
		Token:      "secret-token",
	}
	h.handler = srv.Routes()

	rr := h.authed(t, http.MethodPost, "/api/readings/import/markdown", map[string]any{
		"url":      "https://example.com/old.md",
		"markdown": "new body",
	})

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rr.Code, rr.Body.String())
	}
	data, ctype, err := h.blobs.Get(context.Background(), "readings/failed/raw.md")
	if err != nil {
		t.Fatalf("existing blob get: %v", err)
	}
	if string(data) != "old body" || ctype != "text/markdown" {
		t.Fatalf("existing blob = %q/%q, want old body text/markdown", data, ctype)
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

	h := newHarness(t)
	h.blobs = blobs.NewMemory()
	srv := &httpapi.Server{
		Store:      h.store,
		Blobs:      &failingPutBlobs{inner: h.blobs, err: errors.New("put failed")},
		Dispatcher: h.submitter,
		Clock:      h.clock,
		Token:      "secret-token",
		NewID:      func() string { return "r1" },
	}
	h.handler = srv.Routes()

	rr := h.authed(t, http.MethodPost, "/api/readings/import/markdown", map[string]any{
		"url":      "https://example.com/notes.md",
		"markdown": "# Notes",
	})

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rr.Code, rr.Body.String())
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

func TestImportMarkdown_IDConflictDoesNotOverwriteExistingBlob(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	seedReading(t, h, reading.Reading{
		ID:     "existing",
		URL:    "https://example.com/existing.md",
		Status: reading.Ready,
		RawKey: "readings/existing/raw.md",
	})
	if err := h.blobs.Put(context.Background(), "readings/existing/raw.md", []byte("original"), "text/markdown"); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	srv := &httpapi.Server{
		Store:      h.store,
		Blobs:      h.blobs,
		Dispatcher: h.submitter,
		Clock:      h.clock,
		Token:      "secret-token",
		NewID:      func() string { return "existing" },
	}
	h.handler = srv.Routes()

	rr := h.authed(t, http.MethodPost, "/api/readings/import/markdown", map[string]any{
		"url":      "https://example.com/new.md",
		"markdown": "replacement",
	})

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rr.Code, rr.Body.String())
	}
	data, ctype, err := h.blobs.Get(context.Background(), "readings/existing/raw.md")
	if err != nil {
		t.Fatalf("existing blob get: %v", err)
	}
	if string(data) != "original" || ctype != "text/markdown" {
		t.Fatalf("existing blob = %q/%q, want original text/markdown", data, ctype)
	}
}

func TestImportBookmarks_BulkResult(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	seedReading(t, h, reading.Reading{ID: "existing", URL: "https://example.com/already", Status: reading.Ready})

	rr := h.authed(t, http.MethodPost, "/api/readings/import/bookmarks", map[string]any{
		"bookmarks": []map[string]string{
			{"url": "https://example.com/new?utm_source=feed"},
			{"url": "https://example.com/already"},
			{"url": "notaurl"},
			{"url": "https://example.com/new?utm_campaign=dup"},
		},
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	got := decodeJSON[struct {
		Results []struct {
			URL    string `json:"url"`
			ID     string `json:"id,omitempty"`
			Result string `json:"result"`
		} `json:"results"`
	}](t, rr)
	wantResults := []string{"created", "existing", "invalid", "existing"}
	if len(got.Results) != len(wantResults) {
		t.Fatalf("results len = %d, want %d: %+v", len(got.Results), len(wantResults), got.Results)
	}
	for i, want := range wantResults {
		if got.Results[i].Result != want {
			t.Fatalf("result[%d] = %q, want %q: %+v", i, got.Results[i].Result, want, got.Results)
		}
	}
	if !slices.Equal(h.submitter.ids, []string{"r1"}) {
		t.Fatalf("submitted ids = %v, want only new reading r1", h.submitter.ids)
	}
}

func TestImportBookmarks_MarkdownURLCreatesFetchableWebReading(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	rr := h.authed(t, http.MethodPost, "/api/readings/import/bookmarks", map[string]any{
		"bookmarks": []map[string]string{
			{"url": "https://example.com/export.markdown"},
		},
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	stored, err := h.store.GetByID(context.Background(), "r1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.SourceKind != reading.SourceWeb || stored.RawKey != "" {
		t.Fatalf("stored source/raw = %q/%q, want web with no raw key", stored.SourceKind, stored.RawKey)
	}
}

func TestImportBookmarks_AcceptsRawNetscapeHTML(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	body := `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<DL><p>
<DT><A HREF="https://example.com/one?utm_source=export">One</A>
<DT><A HREF="notaurl">Bad</A>
</DL>`

	rr := h.rawRequest(t, http.MethodPost, "/api/readings/import/bookmarks", body, "text/html", "Bearer secret-token")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	got := decodeJSON[struct {
		Results []struct {
			ID     string `json:"id,omitempty"`
			Result string `json:"result"`
		} `json:"results"`
	}](t, rr)
	if len(got.Results) != 2 || got.Results[0].Result != "created" || got.Results[1].Result != "invalid" {
		t.Fatalf("results = %+v, want created then invalid", got.Results)
	}
	if !slices.Equal(h.submitter.ids, []string{"r1"}) {
		t.Fatalf("submitted ids = %v, want only new reading r1", h.submitter.ids)
	}
}

func TestImportBookmarks_AcceptsTopLevelJSONArray(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	rr := h.rawRequest(t, http.MethodPost, "/api/readings/import/bookmarks", `[{"url":"https://example.com/array"}]`, "application/json", "Bearer secret-token")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	got := decodeJSON[struct {
		Results []struct {
			ID     string `json:"id,omitempty"`
			Result string `json:"result"`
		} `json:"results"`
	}](t, rr)
	if len(got.Results) != 1 || got.Results[0].Result != "created" || got.Results[0].ID != "r1" {
		t.Fatalf("results = %+v, want one created r1", got.Results)
	}
}
