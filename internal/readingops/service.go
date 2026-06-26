// Package readingops owns the multi-resource reading workflows.
package readingops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bbell/reading-lite/internal/blobs"
	"github.com/bbell/reading-lite/internal/clock"
	"github.com/bbell/reading-lite/internal/reading"
	"github.com/bbell/reading-lite/internal/store"
)

// Dispatcher is the queue surface the command service needs.
type Dispatcher interface {
	Submit(id string)
	ForceSubmitAfter(ctx context.Context, id string, beforeQueue func() error) error
}

// Service coordinates store, blob, and dispatch operations for reading commands.
type Service struct {
	Store      store.Store
	Blobs      blobs.Blobs
	Dispatcher Dispatcher
	Clock      clock.Clock
	TTLs       reading.TTLs
	NewID      func() string
}

// StatusResult reports the current lifecycle state after a command.
type StatusResult struct {
	ID      string
	Status  reading.Status
	Created bool
}

// MarkdownImport is a user-supplied markdown body to store as the source.
type MarkdownImport struct {
	URL      string
	Markdown string
	Title    string
	Tags     []string
}

// BookmarkResult is one result in a bulk bookmark import.
type BookmarkResult struct {
	URL    string
	ID     string
	Result string
}

// IngestURL creates or reuses a fetchable URL reading and submits new or failed
// readings for processing.
func (s *Service) IngestURL(ctx context.Context, rawURL string) (StatusResult, error) {
	key, err := reading.URLKey(rawURL)
	if err != nil {
		return StatusResult{}, fmt.Errorf("%w: %v", reading.ErrInvalidURL, err)
	}

	existing, err := s.Store.GetByURLKey(ctx, key)
	switch {
	case err == nil:
		if existing.Status == reading.Failed {
			if err := s.markPending(ctx, existing.ID); err != nil {
				return StatusResult{}, err
			}
			s.Dispatcher.Submit(existing.ID)
			return StatusResult{ID: existing.ID, Status: reading.Pending}, nil
		}
		return StatusResult{ID: existing.ID, Status: existing.Status}, nil
	case !errors.Is(err, store.ErrNotFound):
		return StatusResult{}, err
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
				return StatusResult{ID: got.ID, Status: got.Status}, nil
			}
		}
		return StatusResult{}, err
	}
	s.Dispatcher.Submit(id)
	return StatusResult{ID: id, Status: reading.Pending, Created: true}, nil
}

// ImportMarkdown creates or replaces a failed reading with a user-supplied
// markdown source, staging blob writes so failed metadata updates do not destroy
// previously owned blobs.
func (s *Service) ImportMarkdown(ctx context.Context, req MarkdownImport) (StatusResult, error) {
	key, err := reading.URLKey(req.URL)
	if err != nil {
		return StatusResult{}, fmt.Errorf("%w: %v", reading.ErrInvalidURL, err)
	}

	if existing, err := s.Store.GetByURLKey(ctx, key); err == nil {
		if existing.Status == reading.Failed {
			return s.replaceFailedWithMarkdown(ctx, existing, req)
		}
		return StatusResult{ID: existing.ID, Status: existing.Status}, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return StatusResult{}, err
	}

	id := s.newID()
	if _, err := s.Store.GetByID(ctx, id); err == nil {
		return StatusResult{}, store.ErrConflict
	} else if !errors.Is(err, store.ErrNotFound) {
		return StatusResult{}, err
	}

	rawKey := "readings/" + id + "/raw.md"
	now := s.now()
	rec := reading.Reading{
		ID:         id,
		URL:        strings.TrimSpace(req.URL),
		URLKey:     key,
		Status:     reading.Pending,
		SourceKind: reading.SourceMarkdown,
		Title:      req.Title,
		RawKey:     rawKey,
		Tags:       req.Tags,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.Blobs.Put(ctx, rawKey, []byte(req.Markdown), "text/markdown"); err != nil {
		return StatusResult{}, err
	}
	if err := s.Store.SaveReading(ctx, rec); err != nil {
		if errors.Is(err, store.ErrConflict) {
			got, getErr := s.Store.GetByURLKey(ctx, key)
			if getErr == nil {
				_ = s.Blobs.Delete(context.Background(), rawKey)
				return StatusResult{ID: got.ID, Status: got.Status}, nil
			}
		}
		_ = s.Blobs.Delete(context.Background(), rawKey)
		return StatusResult{}, err
	}
	s.Dispatcher.Submit(id)
	return StatusResult{ID: id, Status: reading.Pending, Created: true}, nil
}

// ImportBookmarks ingests a list of bookmark URLs and records per-item outcomes.
func (s *Service) ImportBookmarks(ctx context.Context, urls []string) ([]BookmarkResult, error) {
	results := make([]BookmarkResult, 0, len(urls))
	for _, rawURL := range urls {
		res, err := s.IngestURL(ctx, rawURL)
		switch {
		case err != nil && errors.Is(err, reading.ErrInvalidURL):
			results = append(results, BookmarkResult{URL: rawURL, Result: "invalid"})
		case err != nil:
			return nil, err
		case res.Created:
			results = append(results, BookmarkResult{URL: rawURL, ID: res.ID, Result: "created"})
		default:
			results = append(results, BookmarkResult{URL: rawURL, ID: res.ID, Result: "existing"})
		}
	}
	return results, nil
}

// Reprocess resets terminal or stale readings for a fresh run. Fresh pending and
// running readings are returned unchanged and are not enqueued again.
func (s *Service) Reprocess(ctx context.Context, id string) (StatusResult, error) {
	got, err := s.Store.GetByID(ctx, id)
	if err != nil {
		return StatusResult{}, err
	}

	annotated := reading.AnnotateStale(got, s.now(), s.TTLs)
	if (got.Status == reading.Pending || got.Status == reading.Running) && annotated.Status != reading.Failed {
		return StatusResult{ID: id, Status: got.Status}, nil
	}
	force := (got.Status == reading.Pending || got.Status == reading.Running) && annotated.Status == reading.Failed

	if force {
		// Once forced recovery cancels an in-flight run, the reset/enqueue must not
		// be aborted by the client disconnecting from the HTTP request.
		recoveryCtx := context.WithoutCancel(ctx)
		if err := s.Dispatcher.ForceSubmitAfter(recoveryCtx, id, func() error {
			return s.markPending(recoveryCtx, id)
		}); err != nil {
			return StatusResult{}, err
		}
	} else {
		if err := s.markPending(ctx, id); err != nil {
			return StatusResult{}, err
		}
		s.Dispatcher.Submit(id)
	}
	return StatusResult{ID: id, Status: reading.Pending}, nil
}

func (s *Service) replaceFailedWithMarkdown(ctx context.Context, existing reading.Reading, req MarkdownImport) (StatusResult, error) {
	rawKey := s.replacementRawKey(existing)
	if err := s.Blobs.Put(ctx, rawKey, []byte(req.Markdown), "text/markdown"); err != nil {
		return StatusResult{}, err
	}
	if err := s.Store.UpdateImport(ctx, existing.ID, store.ImportFields{
		Now:        s.now(),
		SourceKind: reading.SourceMarkdown,
		Title:      req.Title,
		RawKey:     rawKey,
		Tags:       req.Tags,
	}); err != nil {
		_ = s.Blobs.Delete(context.Background(), rawKey)
		return StatusResult{}, err
	}
	if existing.RawKey != "" {
		_ = s.Blobs.Delete(context.Background(), existing.RawKey)
	}
	if existing.ContentKey != "" {
		_ = s.Blobs.Delete(context.Background(), existing.ContentKey)
	}
	s.Dispatcher.Submit(existing.ID)
	return StatusResult{ID: existing.ID, Status: reading.Pending}, nil
}

func (s *Service) replacementRawKey(existing reading.Reading) string {
	base := s.newID()
	for i := range 3 {
		suffix := base
		if i > 0 {
			suffix = fmt.Sprintf("%s-%d", base, i)
		}
		key := "readings/" + existing.ID + "/raw-" + suffix + ".md"
		if key != existing.RawKey && key != existing.ContentKey {
			return key
		}
	}
	return "readings/" + existing.ID + "/raw-" + base + "-" + fmt.Sprintf("%d", s.now().UnixNano()) + ".md"
}

func (s *Service) markPending(ctx context.Context, id string) error {
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

func urlIngestSourceKind(key string) reading.SourceKind {
	kind := reading.ClassifySource(key)
	if kind == reading.SourceMarkdown {
		return reading.SourceWeb
	}
	return kind
}

func (s *Service) now() time.Time {
	if s.Clock == nil {
		return time.Now().UTC()
	}
	return s.Clock.Now()
}

func (s *Service) newID() string {
	if s.NewID != nil {
		return s.NewID()
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
