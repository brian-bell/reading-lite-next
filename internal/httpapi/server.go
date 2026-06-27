// Package httpapi exposes the JSON HTTP surface for reading-lite.
package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bbell/reading-lite/internal/blobs"
	"github.com/bbell/reading-lite/internal/bookmarks"
	"github.com/bbell/reading-lite/internal/clock"
	"github.com/bbell/reading-lite/internal/reading"
	"github.com/bbell/reading-lite/internal/readingops"
	"github.com/bbell/reading-lite/internal/store"
)

const (
	maxBodyBytes              = 1 << 20
	defaultHealthCheckTimeout = 2 * time.Second
)

// Dispatcher is the queue surface the HTTP layer needs.
type Dispatcher interface {
	Submit(id string)
	ForceSubmitAfter(ctx context.Context, id string, beforeQueue func() error) error
}

// Server wires the HTTP API to the core ports.
type Server struct {
	Store        store.Store
	Dispatcher   Dispatcher
	Blobs        blobs.Blobs
	Clock        clock.Clock
	Token        string
	TTLs         reading.TTLs
	NewID        func() string
	Build        BuildInfo
	Health       *HealthState
	HealthChecks HealthChecks
	// HealthCheckTimeout bounds each dependency probe. A zero value uses the default.
	HealthCheckTimeout time.Duration
	Logger             *slog.Logger
}

// BuildInfo is the build metadata exposed by /api/healthz.
type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

// HealthChecks are dependency probes exposed by /api/healthz.
type HealthChecks struct {
	Postgres func(context.Context) error
	R2       func(context.Context) error
}

// HealthState carries process-level readiness for graceful shutdown.
type HealthState struct {
	degraded atomic.Bool
}

// MarkDegraded makes health checks report degraded regardless of dependency state.
func (h *HealthState) MarkDegraded() {
	h.degraded.Store(true)
}

// Degraded reports whether the process has been marked unhealthy.
func (h *HealthState) Degraded() bool {
	return h != nil && h.degraded.Load()
}

// Routes returns the API handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/healthz", s.healthz)
	mux.HandleFunc("POST /api/readings", s.ingest)
	mux.HandleFunc("POST /api/readings/import/markdown", s.importMarkdown)
	mux.HandleFunc("POST /api/readings/import/bookmarks", s.importBookmarks)
	mux.HandleFunc("GET /api/readings", s.listReadings)
	mux.HandleFunc("GET /api/readings/{id}", s.getReading)
	mux.HandleFunc("GET /api/readings/{id}/content", s.getContent)
	mux.HandleFunc("GET /api/readings/{id}/raw", s.getRaw)
	mux.HandleFunc("POST /api/readings/{id}/reprocess", s.reprocess)
	return s.logRequests(s.auth(jsonMuxErrors(mux)))
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if s.Health.Degraded() {
		writeJSON(w, http.StatusServiceUnavailable, healthDocument{
			Status: "degraded",
			Build:  s.Build,
			Checks: skippedHealthChecks(),
		})
		return
	}

	doc := healthDocument{
		Status: "ok",
		Build:  s.Build,
		Checks: map[string]healthCheck{
			"postgres": s.runHealthCheck(r.Context(), s.HealthChecks.Postgres),
			"r2":       s.runHealthCheck(r.Context(), s.HealthChecks.R2),
		},
	}
	status := http.StatusOK
	if s.Health.Degraded() || doc.Checks["postgres"].Status != "ok" || doc.Checks["r2"].Status != "ok" {
		doc.Status = "degraded"
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, doc)
}

func skippedHealthChecks() map[string]healthCheck {
	return map[string]healthCheck{
		"postgres": {Status: "skipped"},
		"r2":       {Status: "skipped"},
	}
}

type healthDocument struct {
	Status string                 `json:"status"`
	Build  BuildInfo              `json:"build"`
	Checks map[string]healthCheck `json:"checks"`
}

type healthCheck struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

func (s *Server) runHealthCheck(ctx context.Context, check func(context.Context) error) healthCheck {
	if check == nil {
		return healthCheck{Status: "ok"}
	}
	ctx, cancel := context.WithTimeout(ctx, s.healthCheckTimeout())
	defer cancel()
	if err := check(ctx); err != nil {
		return healthCheck{Status: "error", Error: safeHealthError(err)}
	}
	return healthCheck{Status: "ok"}
}

func (s *Server) healthCheckTimeout() time.Duration {
	if s.HealthCheckTimeout > 0 {
		return s.HealthCheckTimeout
	}
	return defaultHealthCheckTimeout
}

func safeHealthError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "unavailable"
	}
}

func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		URL string `json:"url"`
	}](w, r)
	if !ok {
		return
	}
	res, err := s.ops().IngestURL(r.Context(), req.URL)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, createdStatus(res.Created), statusResponse{ID: res.ID, Status: res.Status})
}

func (s *Server) importMarkdown(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		URL      string   `json:"url"`
		Markdown string   `json:"markdown"`
		Title    string   `json:"title"`
		Tags     []string `json:"tags"`
	}](w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Markdown) == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "markdown is required")
		return
	}
	res, err := s.ops().ImportMarkdown(r.Context(), readingops.MarkdownImport{
		URL:      req.URL,
		Markdown: req.Markdown,
		Title:    req.Title,
		Tags:     req.Tags,
	})
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, createdStatus(res.Created), statusResponse{ID: res.ID, Status: res.Status})
}

func (s *Server) importBookmarks(w http.ResponseWriter, r *http.Request) {
	urls, ok := bookmarkURLsFromRequest(w, r)
	if !ok {
		return
	}
	if len(urls) == 0 {
		writeErr(w, http.StatusBadRequest, "invalid_request", "bookmarks are required")
		return
	}

	results, err := s.ops().ImportBookmarks(r.Context(), urls)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]bookmarkResult{"results": bookmarkDTOs(results)})
}

func (s *Server) listReadings(w http.ResponseWriter, r *http.Request) {
	q, err := queryFromRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	page, err := s.Store.Search(r.Context(), q)
	if err != nil {
		s.writeError(w, err)
		return
	}
	out := listResponse{
		Readings: readingDTOs(page.Readings),
		Total:    page.Total,
	}
	if page.NextCursor.Valid {
		out.NextCursor = encodeCursor(page.NextCursor)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getReading(w http.ResponseWriter, r *http.Request) {
	got, err := s.Store.GetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	got = reading.AnnotateStale(got, s.now(), s.TTLs)
	writeJSON(w, http.StatusOK, readingDTOFrom(got))
}

func (s *Server) getContent(w http.ResponseWriter, r *http.Request) {
	s.streamBlob(w, r, true)
}

func (s *Server) getRaw(w http.ResponseWriter, r *http.Request) {
	s.streamBlob(w, r, false)
}

func (s *Server) reprocess(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	res, err := s.ops().Reprocess(r.Context(), id)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, statusResponse{ID: res.ID, Status: res.Status})
}

func (s *Server) streamBlob(w http.ResponseWriter, r *http.Request, content bool) {
	got, err := s.Store.GetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	key := got.RawKey
	if content {
		key = got.ContentKey
	}
	if key == "" {
		writeErr(w, http.StatusNotFound, "not_found", "blob not found")
		return
	}
	data, ctype, err := s.Blobs.Get(r.Context(), key)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !content {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="raw-content"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}
	if ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		header := r.Header.Get("Authorization")
		if s.Token == "" || !strings.HasPrefix(header, "Bearer ") {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		token := strings.TrimPrefix(header, "Bearer ")
		if !tokenEqual(token, s.Token) {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func tokenEqual(got, want string) bool {
	gotHash := sha256.Sum256([]byte(got))
	wantHash := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(gotHash[:], wantHash[:]) == 1
}

func (s *Server) now() time.Time {
	if s.Clock == nil {
		return time.Now().UTC()
	}
	return s.Clock.Now()
}

func (s *Server) writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, reading.ErrInvalidURL):
		writeErr(w, http.StatusBadRequest, "invalid_url", "invalid reading url")
	case errors.Is(err, store.ErrNotFound), errors.Is(err, blobs.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "not found")
	default:
		writeErr(w, http.StatusInternalServerError, "internal", "internal server error")
	}
}

type statusResponse struct {
	ID     string         `json:"id"`
	Status reading.Status `json:"status"`
}

type bookmarkResult struct {
	URL    string `json:"url"`
	ID     string `json:"id,omitempty"`
	Result string `json:"result"`
}

func bookmarkDTOs(in []readingops.BookmarkResult) []bookmarkResult {
	out := make([]bookmarkResult, len(in))
	for i, res := range in {
		out[i] = bookmarkResult{URL: res.URL, ID: res.ID, Result: res.Result}
	}
	return out
}

func (s *Server) ops() *readingops.Service {
	return &readingops.Service{
		Store:      s.Store,
		Blobs:      s.Blobs,
		Dispatcher: s.Dispatcher,
		Clock:      s.Clock,
		TTLs:       s.TTLs,
		NewID:      s.NewID,
	}
}

func createdStatus(created bool) int {
	if created {
		return http.StatusCreated
	}
	return http.StatusOK
}

type listResponse struct {
	Readings   []readingDTO `json:"readings"`
	Total      int          `json:"total"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

type readingDTO struct {
	ID              string             `json:"id"`
	URL             string             `json:"url"`
	Status          reading.Status     `json:"status"`
	SourceKind      reading.SourceKind `json:"source_kind"`
	Title           string             `json:"title,omitempty"`
	Author          string             `json:"author,omitempty"`
	Site            string             `json:"site,omitempty"`
	Lang            string             `json:"lang,omitempty"`
	WordCount       int                `json:"word_count,omitempty"`
	ExtractionMode  string             `json:"extraction_mode,omitempty"`
	Summary         string             `json:"summary,omitempty"`
	SummaryJSON     json.RawMessage    `json:"summary_json,omitempty"`
	SimilarJSON     json.RawMessage    `json:"similar_json,omitempty"`
	DiagnosticsJSON json.RawMessage    `json:"diagnostics_json,omitempty"`
	Error           string             `json:"error,omitempty"`
	StaleReason     string             `json:"stale_reason,omitempty"`
	Tags            []string           `json:"tags"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
}

func readingDTOs(in []reading.Reading) []readingDTO {
	out := make([]readingDTO, len(in))
	for i, r := range in {
		out[i] = readingDTOFrom(r)
	}
	return out
}

func readingDTOFrom(r reading.Reading) readingDTO {
	return readingDTO{
		ID:              r.ID,
		URL:             r.URL,
		Status:          r.Status,
		SourceKind:      r.SourceKind,
		Title:           r.Title,
		Author:          r.Author,
		Site:            r.Site,
		Lang:            r.Lang,
		WordCount:       r.WordCount,
		ExtractionMode:  r.ExtractionMode,
		Summary:         r.Summary,
		SummaryJSON:     r.SummaryJSON,
		SimilarJSON:     r.SimilarJSON,
		DiagnosticsJSON: r.DiagnosticsJSON,
		Error:           r.Error,
		StaleReason:     r.StaleReason,
		Tags:            append([]string(nil), r.Tags...),
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
	}
}

func queryFromRequest(r *http.Request) (store.Query, error) {
	values := r.URL.Query()
	q := store.Query{
		Q:      values.Get("q"),
		Tags:   parseTags(values["tags"]),
		Status: reading.Status(values.Get("status")),
		Sort:   store.SortMode(values.Get("sort")),
		Limit:  parseLimit(values.Get("limit")),
	}
	if q.Sort == "" {
		q.Sort = store.SortNewest
	}
	switch q.Sort {
	case store.SortNewest, store.SortOldest, store.SortTitle:
	default:
		return store.Query{}, fmt.Errorf("unsupported sort %q", q.Sort)
	}
	switch q.Status {
	case "", reading.Pending, reading.Running, reading.Ready, reading.Failed:
	default:
		return store.Query{}, fmt.Errorf("unsupported status %q", q.Status)
	}
	if c := values.Get("cursor"); c != "" {
		cursor, err := decodeCursor(c)
		if err != nil {
			return store.Query{}, err
		}
		q.Cursor = cursor
	}
	return q, nil
}

func parseTags(values []string) []string {
	var out []string
	for _, value := range values {
		for _, tag := range strings.Split(value, ",") {
			tag = strings.TrimSpace(tag)
			if tag != "" {
				out = append(out, tag)
			}
		}
	}
	return out
}

func parseLimit(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	return n
}

func encodeCursor(c store.Cursor) string {
	data, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeCursor(raw string) (store.Cursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return store.Cursor{}, fmt.Errorf("invalid cursor")
	}
	var c store.Cursor
	if err := json.Unmarshal(data, &c); err != nil || !c.Valid {
		return store.Cursor{}, fmt.Errorf("invalid cursor")
	}
	return c, nil
}

func bookmarkURLsFromRequest(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "request body is too large")
		return nil, false
	}

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		writeErr(w, http.StatusBadRequest, "invalid_request", "bookmarks are required")
		return nil, false
	}

	urls, err := bookmarks.Parse(data, r.Header.Get("Content-Type"))
	if err != nil {
		if errors.Is(err, bookmarks.ErrMultipleJSONValues) {
			writeErr(w, http.StatusBadRequest, "invalid_json", "request body must contain one JSON value")
			return nil, false
		}
		writeErr(w, http.StatusBadRequest, "invalid_json", "invalid JSON request body")
		return nil, false
	}
	return urls, true
}

func decode[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var out T
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", "invalid JSON request body")
		return out, false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid_json", "request body must contain one JSON value")
		return out, false
	}
	return out, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]map[string]string{
		"error": {
			"code":    code,
			"message": msg,
		},
	})
}

func jsonMuxErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cw := &muxErrorWriter{ResponseWriter: w}
		next.ServeHTTP(cw, r)
		switch cw.capturedStatus {
		case http.StatusNotFound:
			writeErr(w, http.StatusNotFound, "not_found", "not found")
		case http.StatusMethodNotAllowed:
			writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		}
	})
}

var requestSeq atomic.Uint64

func (s *Server) logRequests(next http.Handler) http.Handler {
	if s.Logger == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(lw, r)
		reqID := fmt.Sprintf("req-%d", requestSeq.Add(1))
		s.Logger.InfoContext(r.Context(), "http_request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", lw.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", reqID,
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

type muxErrorWriter struct {
	http.ResponseWriter
	capturedStatus int
	wrote          bool
}

func (w *muxErrorWriter) WriteHeader(status int) {
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		w.capturedStatus = status
		return
	}
	w.wrote = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *muxErrorWriter) Write(p []byte) (int, error) {
	if w.capturedStatus != 0 {
		return len(p), nil
	}
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}
