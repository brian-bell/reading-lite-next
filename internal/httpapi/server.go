// Package httpapi exposes the JSON HTTP surface for reading-lite.
package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
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
	"github.com/bbell/reading-lite/internal/store"
)

const maxBodyBytes = 1 << 20

// Dispatcher is the queue surface the HTTP layer needs.
type Dispatcher interface {
	Submit(id string)
}

type forceDispatcher interface {
	ForceSubmit(id string)
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
	res, status, err := s.ingestURL(r.Context(), req.URL)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, status, res)
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
	res, status, err := s.importMarkdownReading(r.Context(), req.URL, req.Markdown, req.Title, req.Tags)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, status, res)
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

	results := make([]bookmarkResult, 0, len(urls))
	for _, rawURL := range urls {
		res, status, err := s.ingestURL(r.Context(), rawURL)
		switch {
		case err != nil && errors.Is(err, reading.ErrInvalidURL):
			results = append(results, bookmarkResult{URL: rawURL, Result: "invalid"})
		case err != nil:
			s.writeError(w, err)
			return
		case status == http.StatusCreated:
			results = append(results, bookmarkResult{URL: rawURL, ID: res.ID, Result: "created"})
		default:
			results = append(results, bookmarkResult{URL: rawURL, ID: res.ID, Result: "existing"})
		}
	}
	writeJSON(w, http.StatusOK, map[string][]bookmarkResult{"results": results})
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
	got, err := s.Store.GetByID(r.Context(), id)
	if err != nil {
		s.writeError(w, err)
		return
	}

	annotated := reading.AnnotateStale(got, s.now(), s.TTLs)
	if (got.Status == reading.Pending || got.Status == reading.Running) && annotated.Status != reading.Failed {
		writeJSON(w, http.StatusAccepted, statusResponse{ID: id, Status: got.Status})
		return
	}
	force := got.Status == reading.Running && annotated.Status == reading.Failed

	if err := s.markPending(r.Context(), id); err != nil {
		s.writeError(w, err)
		return
	}
	s.submit(id, force)
	writeJSON(w, http.StatusAccepted, statusResponse{ID: id, Status: reading.Pending})
}

func (s *Server) ingestURL(ctx context.Context, rawURL string) (statusResponse, int, error) {
	key, err := reading.URLKey(rawURL)
	if err != nil {
		return statusResponse{}, 0, fmt.Errorf("%w: %v", reading.ErrInvalidURL, err)
	}

	existing, err := s.Store.GetByURLKey(ctx, key)
	switch {
	case err == nil:
		if existing.Status == reading.Failed {
			if err := s.markPending(ctx, existing.ID); err != nil {
				return statusResponse{}, 0, err
			}
			s.Dispatcher.Submit(existing.ID)
			return statusResponse{ID: existing.ID, Status: reading.Pending}, http.StatusOK, nil
		}
		return statusResponse{ID: existing.ID, Status: existing.Status}, http.StatusOK, nil
	case !errors.Is(err, store.ErrNotFound):
		return statusResponse{}, 0, err
	}

	id := s.newID()
	now := s.now()
	rec := reading.Reading{
		ID:         id,
		URL:        strings.TrimSpace(rawURL),
		URLKey:     key,
		Status:     reading.Pending,
		SourceKind: urlIngestSourceKind(key),
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.Store.SaveReading(ctx, rec); err != nil {
		if errors.Is(err, store.ErrConflict) {
			got, getErr := s.Store.GetByURLKey(ctx, key)
			if getErr == nil {
				return statusResponse{ID: got.ID, Status: got.Status}, http.StatusOK, nil
			}
		}
		return statusResponse{}, 0, err
	}
	s.Dispatcher.Submit(id)
	return statusResponse{ID: id, Status: reading.Pending}, http.StatusCreated, nil
}

func (s *Server) submit(id string, force bool) {
	if force {
		if d, ok := s.Dispatcher.(forceDispatcher); ok {
			d.ForceSubmit(id)
			return
		}
	}
	s.Dispatcher.Submit(id)
}

func urlIngestSourceKind(key string) reading.SourceKind {
	kind := reading.ClassifySource(key)
	if kind == reading.SourceMarkdown {
		return reading.SourceWeb
	}
	return kind
}

func (s *Server) importMarkdownReading(ctx context.Context, rawURL, markdown, title string, tags []string) (statusResponse, int, error) {
	key, err := reading.URLKey(rawURL)
	if err != nil {
		return statusResponse{}, 0, fmt.Errorf("%w: %v", reading.ErrInvalidURL, err)
	}
	if existing, err := s.Store.GetByURLKey(ctx, key); err == nil {
		if existing.Status == reading.Failed {
			return s.replaceFailedWithMarkdown(ctx, existing, markdown, title, tags)
		}
		return statusResponse{ID: existing.ID, Status: existing.Status}, http.StatusOK, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return statusResponse{}, 0, err
	}

	id := s.newID()
	if _, err := s.Store.GetByID(ctx, id); err == nil {
		return statusResponse{}, 0, store.ErrConflict
	} else if !errors.Is(err, store.ErrNotFound) {
		return statusResponse{}, 0, err
	}
	rawKey := "readings/" + id + "/raw.md"
	now := s.now()
	rec := reading.Reading{
		ID:         id,
		URL:        strings.TrimSpace(rawURL),
		URLKey:     key,
		Status:     reading.Pending,
		SourceKind: reading.SourceMarkdown,
		Title:      title,
		RawKey:     rawKey,
		Tags:       tags,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.Blobs.Put(ctx, rawKey, []byte(markdown), "text/markdown"); err != nil {
		return statusResponse{}, 0, err
	}
	if err := s.Store.SaveReading(ctx, rec); err != nil {
		if errors.Is(err, store.ErrConflict) {
			got, getErr := s.Store.GetByURLKey(ctx, key)
			if getErr == nil {
				_ = s.Blobs.Delete(context.Background(), rawKey)
				return statusResponse{ID: got.ID, Status: got.Status}, http.StatusOK, nil
			}
			return statusResponse{}, 0, err
		}
		_ = s.Blobs.Delete(context.Background(), rawKey)
		return statusResponse{}, 0, err
	}
	s.Dispatcher.Submit(id)
	return statusResponse{ID: id, Status: reading.Pending}, http.StatusCreated, nil
}

func (s *Server) replaceFailedWithMarkdown(ctx context.Context, existing reading.Reading, markdown, title string, tags []string) (statusResponse, int, error) {
	now := s.now()
	rawKey := replacementRawKey(existing.ID, now)
	if err := s.Blobs.Put(ctx, rawKey, []byte(markdown), "text/markdown"); err != nil {
		return statusResponse{}, 0, err
	}
	if err := s.Store.UpdateImport(ctx, existing.ID, store.ImportFields{
		Now:        now,
		SourceKind: reading.SourceMarkdown,
		Title:      title,
		RawKey:     rawKey,
		Tags:       tags,
	}); err != nil {
		_ = s.Blobs.Delete(context.Background(), rawKey)
		return statusResponse{}, 0, err
	}
	if existing.RawKey != "" && existing.RawKey != rawKey {
		_ = s.Blobs.Delete(context.Background(), existing.RawKey)
	}
	if existing.ContentKey != "" {
		_ = s.Blobs.Delete(context.Background(), existing.ContentKey)
	}
	s.Dispatcher.Submit(existing.ID)
	return statusResponse{ID: existing.ID, Status: reading.Pending}, http.StatusOK, nil
}

func replacementRawKey(id string, now time.Time) string {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return "readings/" + id + "/raw-" + strconv.FormatInt(now.UnixNano(), 10) + ".md"
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

func (s *Server) markPending(ctx context.Context, id string) error {
	r, err := s.Store.GetByID(ctx, id)
	if err != nil {
		return err
	}
	rawKey := ""
	title := ""
	if r.SourceKind == reading.SourceMarkdown {
		rawKey = r.RawKey
		title = r.Title
	}
	return s.Store.Reprocess(ctx, id, store.ReprocessFields{
		Now:    s.now(),
		RawKey: rawKey,
		Title:  title,
	})
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
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.Token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) now() time.Time {
	if s.Clock == nil {
		return time.Now().UTC()
	}
	return s.Clock.Now()
}

func (s *Server) newID() string {
	if s.NewID != nil {
		return s.NewID()
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
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
