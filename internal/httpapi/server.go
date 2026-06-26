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
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/bbell/reading-lite/internal/blobs"
	"github.com/bbell/reading-lite/internal/clock"
	"github.com/bbell/reading-lite/internal/reading"
	"github.com/bbell/reading-lite/internal/readingops"
	"github.com/bbell/reading-lite/internal/store"
)

const maxBodyBytes = 1 << 20

// Dispatcher is the queue surface the HTTP layer needs.
type Dispatcher interface {
	Submit(id string)
	ForceSubmitAfter(ctx context.Context, id string, beforeQueue func() error) error
}

// Server wires the HTTP API to the core ports.
type Server struct {
	Store      store.Store
	Dispatcher Dispatcher
	Blobs      blobs.Blobs
	Clock      clock.Clock
	Token      string
	TTLs       reading.TTLs
	NewID      func() string
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
	return s.auth(jsonMuxErrors(mux))
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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

	if strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "text/html") || trimmed[0] == '<' {
		return bookmarkHREFs(string(data)), true
	}

	type bookmarkInput struct {
		URL string `json:"url"`
	}
	if trimmed[0] == '[' {
		var bookmarks []bookmarkInput
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&bookmarks); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_json", "invalid JSON request body")
			return nil, false
		}
		if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			writeErr(w, http.StatusBadRequest, "invalid_json", "request body must contain one JSON value")
			return nil, false
		}
		urls := make([]string, 0, len(bookmarks))
		for _, b := range bookmarks {
			urls = append(urls, b.URL)
		}
		return urls, true
	}

	var req struct {
		HTML      string          `json:"html"`
		Bookmarks []bookmarkInput `json:"bookmarks"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", "invalid JSON request body")
		return nil, false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid_json", "request body must contain one JSON value")
		return nil, false
	}

	urls := make([]string, 0, len(req.Bookmarks))
	for _, b := range req.Bookmarks {
		urls = append(urls, b.URL)
	}
	if req.HTML != "" {
		urls = append(urls, bookmarkHREFs(req.HTML)...)
	}
	return urls, true
}

func bookmarkHREFs(raw string) []string {
	tokenizer := html.NewTokenizer(strings.NewReader(raw))
	var out []string
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			if errors.Is(tokenizer.Err(), io.EOF) {
				return out
			}
			return out
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := tokenizer.TagName()
			if string(name) != "a" || !hasAttr {
				continue
			}
			for {
				key, val, more := tokenizer.TagAttr()
				if string(key) == "href" {
					out = append(out, string(val))
					break
				}
				if !more {
					break
				}
			}
		}
	}
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
