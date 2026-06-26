// Package store defines the readings metadata persistence port.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/bbell/reading-lite/internal/reading"
)

// Sentinel store errors shared by every Store implementation.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

// SortMode selects result ordering for Search.
type SortMode string

const (
	// SortNewest orders newest readings first.
	SortNewest SortMode = "newest"
	// SortOldest orders oldest readings first.
	SortOldest SortMode = "oldest"
	// SortTitle orders by title, then id.
	SortTitle SortMode = "title"
)

// Cursor is an opaque keyset position returned by Search.
type Cursor struct {
	CreatedAt time.Time
	ID        string
	Title     string
	Rank      float64
	Valid     bool
}

// Query describes a bounded store search.
type Query struct {
	Q      string
	Tags   []string
	Status reading.Status
	Sort   SortMode
	Cursor Cursor
	Limit  int
}

// Page is one result page plus the total count before pagination.
type Page struct {
	Readings   []reading.Reading
	Total      int
	NextCursor Cursor
}

// Pending is a non-terminal reading selected for recovery.
type Pending struct {
	ID              string
	ProcessAttempts int
}

// ContentFields carries the processed content a pipeline run persists. It
// overwrites the extraction/summary columns and updated_at without touching the
// lifecycle status, timestamps, error, attempt count, or tags (the dispatcher
// owns status; tags go through ReplaceTags).
type ContentFields struct {
	// Now is the updated_at timestamp to record (falls back to wall clock when zero).
	Now time.Time
	// ExpectedStartedAt, when set, fences the write to the run that started at
	// this timestamp. The write succeeds only while the row is still running with
	// that same started_at, preventing stale in-flight handlers from overwriting
	// replacement content after a manual reprocess.
	ExpectedStartedAt *time.Time
	// Title is the extracted or refined article title.
	Title string
	// Author is the extracted article author.
	Author string
	// Site is the extracted or inferred site name.
	Site string
	// Lang is the detected content language.
	Lang string
	// WordCount is the extracted body's word count.
	WordCount int
	// ExtractionMode records which extraction tier produced the content.
	ExtractionMode string
	// ContentKey points at the extracted content blob.
	ContentKey string
	// RawKey points at the raw source blob.
	RawKey string
	// Summary is the generated human-readable summary.
	Summary string
	// SummaryJSON is the raw structured summarizer payload.
	SummaryJSON json.RawMessage
	// SimilarJSON is the snapshot of similar readings.
	SimilarJSON json.RawMessage
	// DiagnosticsJSON records per-step pipeline diagnostics.
	DiagnosticsJSON json.RawMessage
}

// ImportFields carries the metadata for replacing a failed reading with a
// user-supplied markdown import while preserving its stable id/url_key.
type ImportFields struct {
	Now        time.Time
	SourceKind reading.SourceKind
	Title      string
	RawKey     string
	Tags       []string
}

// ReprocessFields carries the metadata for an operator-requested reprocess.
// RawKey and Title are preserved only for sources, such as markdown imports,
// whose original body and user-provided title must remain available to the
// pipeline.
type ReprocessFields struct {
	Now    time.Time
	RawKey string
	Title  string
}

// TagFields carries optional metadata for replacing tags.
type TagFields struct {
	Now time.Time
	// ExpectedStartedAt has the same lease semantics as ContentFields.
	ExpectedStartedAt *time.Time
}

// StatusFields carries optional metadata to apply during UpdateStatus.
type StatusFields struct {
	Now             time.Time
	StartedAt       *time.Time
	ClearStartedAt  bool
	FinishedAt      *time.Time
	ClearFinishedAt bool
	Error           *string
	ClearError      bool
	ProcessAttempts *int
}

// Store persists readings metadata and exposes the indexed query shapes used by the service.
type Store interface {
	SaveReading(ctx context.Context, r reading.Reading) error
	GetByID(ctx context.Context, id string) (reading.Reading, error)
	GetByURLKey(ctx context.Context, key string) (reading.Reading, error)
	UpdateStatus(ctx context.Context, id string, status reading.Status, fields StatusFields) error
	UpdateContent(ctx context.Context, id string, fields ContentFields) error
	UpdateImport(ctx context.Context, id string, fields ImportFields) error
	Reprocess(ctx context.Context, id string, fields ReprocessFields) error
	ReplaceTags(ctx context.Context, id string, tags []string, fields TagFields) error
	Search(ctx context.Context, q Query) (Page, error)
	ListNonTerminal(ctx context.Context, runningCutoff time.Time) ([]Pending, error)
	Delete(ctx context.Context, id string) error
}
