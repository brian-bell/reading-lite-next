// Package store defines the readings metadata persistence port.
package store

import (
	"context"
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

// StatusFields carries optional metadata to apply during UpdateStatus.
type StatusFields struct {
	Now             time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time
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
	ReplaceTags(ctx context.Context, id string, tags []string) error
	Search(ctx context.Context, q Query) (Page, error)
	ListNonTerminal(ctx context.Context, runningCutoff time.Time) ([]Pending, error)
	Delete(ctx context.Context, id string) error
}
