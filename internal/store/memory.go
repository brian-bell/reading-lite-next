package store

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/bbell/reading-lite/internal/reading"
)

const defaultLimit = 50

// Memory is an in-memory Store implementation for fast tests and zero-infra development.
type Memory struct {
	mu       sync.RWMutex
	byID     map[string]reading.Reading
	byURLKey map[string]string
}

// NewMemory returns an empty, concurrency-safe in-memory store.
func NewMemory() *Memory {
	return &Memory{
		byID:     map[string]reading.Reading{},
		byURLKey: map[string]string{},
	}
}

// SaveReading inserts r and returns ErrConflict when id or url_key already exists.
func (m *Memory) SaveReading(ctx context.Context, r reading.Reading) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.byID[r.ID]; ok {
		return ErrConflict
	}
	if _, ok := m.byURLKey[r.URLKey]; ok {
		return ErrConflict
	}

	r.Tags = normalizeTags(r.Tags)
	m.byID[r.ID] = cloneReading(r)
	m.byURLKey[r.URLKey] = r.ID
	return nil
}

// GetByID returns one reading by id.
func (m *Memory) GetByID(ctx context.Context, id string) (reading.Reading, error) {
	if err := ctx.Err(); err != nil {
		return reading.Reading{}, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	r, ok := m.byID[id]
	if !ok {
		return reading.Reading{}, ErrNotFound
	}
	return cloneReading(r), nil
}

// GetByURLKey returns one reading by normalized URL key.
func (m *Memory) GetByURLKey(ctx context.Context, key string) (reading.Reading, error) {
	if err := ctx.Err(); err != nil {
		return reading.Reading{}, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	id, ok := m.byURLKey[key]
	if !ok {
		return reading.Reading{}, ErrNotFound
	}
	return cloneReading(m.byID[id]), nil
}

// UpdateStatus changes a reading status and advances status timestamps.
func (m *Memory) UpdateStatus(ctx context.Context, id string, status reading.Status, fields StatusFields) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.byID[id]
	if !ok {
		return ErrNotFound
	}

	now := fields.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	r.Status = status
	r.UpdatedAt = now
	if fields.StartedAt != nil {
		r.StartedAt = cloneTimePtr(fields.StartedAt)
	} else if status == reading.Running {
		r.StartedAt = cloneTimePtr(&now)
	}
	if fields.FinishedAt != nil {
		r.FinishedAt = cloneTimePtr(fields.FinishedAt)
	} else if status.IsTerminal() {
		r.FinishedAt = cloneTimePtr(&now)
	}
	if fields.Error != nil {
		r.Error = *fields.Error
	} else if fields.ClearError {
		r.Error = ""
	}
	if fields.ProcessAttempts != nil {
		r.ProcessAttempts = *fields.ProcessAttempts
	}

	m.byID[id] = cloneReading(r)
	return nil
}

// ReplaceTags replaces a reading's tag set.
func (m *Memory) ReplaceTags(ctx context.Context, id string, tags []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.byID[id]
	if !ok {
		return ErrNotFound
	}
	r.Tags = normalizeTags(tags)
	r.UpdatedAt = time.Now().UTC()
	m.byID[id] = cloneReading(r)
	return nil
}

// Search returns a filtered, sorted, bounded page of readings.
func (m *Memory) Search(ctx context.Context, q Query) (Page, error) {
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}

	m.mu.RLock()
	rows := make([]reading.Reading, 0, len(m.byID))
	for _, r := range m.byID {
		rows = append(rows, cloneReading(r))
	}
	m.mu.RUnlock()

	terms := tokenize(q.Q)
	matches := make([]searchMatch, 0, len(rows))
	for _, r := range rows {
		if q.Status != "" && r.Status != q.Status {
			continue
		}
		if !hasAllTags(r.Tags, q.Tags) {
			continue
		}
		score := scoreReading(r, terms)
		if len(terms) > 0 && score == 0 {
			continue
		}
		matches = append(matches, searchMatch{reading: r, score: score})
	}

	sortMatches(matches, q.Sort, len(terms) > 0)
	total := len(matches)
	matches = applyCursor(matches, q)

	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	hasNext := len(matches) > limit
	if hasNext {
		matches = matches[:limit]
	}

	page := Page{
		Readings: make([]reading.Reading, len(matches)),
		Total:    total,
	}
	for i, match := range matches {
		page.Readings[i] = cloneReading(match.reading)
	}
	if hasNext && len(matches) > 0 {
		last := matches[len(matches)-1]
		page.NextCursor = Cursor{
			CreatedAt: last.reading.CreatedAt,
			ID:        last.reading.ID,
			Title:     last.reading.Title,
			Rank:      float64(last.score),
			Valid:     true,
		}
	}
	return page, nil
}

// ListNonTerminal returns pending readings and running readings started before runningCutoff.
func (m *Memory) ListNonTerminal(ctx context.Context, runningCutoff time.Time) ([]Pending, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]Pending, 0)
	for _, r := range m.byID {
		switch r.Status {
		case reading.Pending:
			out = append(out, Pending{ID: r.ID, ProcessAttempts: r.ProcessAttempts})
		case reading.Running:
			if r.StartedAt != nil && r.StartedAt.Before(runningCutoff) {
				out = append(out, Pending{ID: r.ID, ProcessAttempts: r.ProcessAttempts})
			}
		}
	}
	return out, nil
}

// Delete removes one reading.
func (m *Memory) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.byID[id]
	if !ok {
		return ErrNotFound
	}
	delete(m.byID, id)
	delete(m.byURLKey, r.URLKey)
	return nil
}

type searchMatch struct {
	reading reading.Reading
	score   int
}

func sortMatches(matches []searchMatch, sort SortMode, rank bool) {
	slices.SortFunc(matches, func(a, b searchMatch) int {
		if rank {
			if n := cmp.Compare(b.score, a.score); n != 0 {
				return n
			}
		}

		switch sort {
		case SortOldest:
			return compareCreatedAsc(a.reading, b.reading)
		case SortTitle:
			return compareTitleAsc(a.reading, b.reading)
		default:
			return compareCreatedDesc(a.reading, b.reading)
		}
	})
}

func applyCursor(matches []searchMatch, q Query) []searchMatch {
	if !q.Cursor.Valid {
		return matches
	}

	for i, match := range matches {
		if isAfterCursor(match, q.Sort, q.Cursor) {
			return matches[i:]
		}
	}
	return nil
}

func isAfterCursor(match searchMatch, sort SortMode, cursor Cursor) bool {
	r := match.reading
	if cursor.Rank > 0 || match.score > 0 {
		if float64(match.score) != cursor.Rank {
			return float64(match.score) < cursor.Rank
		}
	}

	switch sort {
	case SortTitle:
		if n := strings.Compare(strings.ToLower(r.Title), strings.ToLower(cursor.Title)); n != 0 {
			return n > 0
		}
		return r.ID > cursor.ID
	case SortOldest:
		if r.CreatedAt.Equal(cursor.CreatedAt) {
			return r.ID > cursor.ID
		}
		return r.CreatedAt.After(cursor.CreatedAt)
	default:
		if r.CreatedAt.Equal(cursor.CreatedAt) {
			return r.ID < cursor.ID
		}
		return r.CreatedAt.Before(cursor.CreatedAt)
	}
}

func compareCreatedDesc(a, b reading.Reading) int {
	if n := b.CreatedAt.Compare(a.CreatedAt); n != 0 {
		return n
	}
	return strings.Compare(b.ID, a.ID)
}

func compareCreatedAsc(a, b reading.Reading) int {
	if n := a.CreatedAt.Compare(b.CreatedAt); n != 0 {
		return n
	}
	return strings.Compare(a.ID, b.ID)
}

func compareTitleAsc(a, b reading.Reading) int {
	aTitle := strings.ToLower(a.Title)
	bTitle := strings.ToLower(b.Title)
	if n := strings.Compare(aTitle, bTitle); n != 0 {
		return n
	}
	return strings.Compare(a.ID, b.ID)
}

func hasAllTags(have, want []string) bool {
	if len(want) == 0 {
		return true
	}
	set := map[string]bool{}
	for _, tag := range have {
		set[tag] = true
	}
	for _, tag := range want {
		if !set[tag] {
			return false
		}
	}
	return true
}

func scoreReading(r reading.Reading, terms []string) int {
	if len(terms) == 0 {
		return 0
	}
	haystack := strings.ToLower(strings.Join([]string{
		r.Title,
		r.Author,
		r.Summary,
		strings.Join(r.Tags, " "),
	}, " "))

	score := 0
	for _, term := range terms {
		score += strings.Count(haystack, term)
	}
	return score
}

func tokenize(q string) []string {
	return strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func cloneReading(r reading.Reading) reading.Reading {
	r.Tags = cloneStrings(r.Tags)
	r.StartedAt = cloneTimePtr(r.StartedAt)
	r.FinishedAt = cloneTimePtr(r.FinishedAt)
	return r
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	return slices.Clone(in)
}

func normalizeTags(in []string) []string {
	if in == nil {
		return []string{}
	}
	return slices.Clone(in)
}

func cloneTimePtr(in *time.Time) *time.Time {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}
