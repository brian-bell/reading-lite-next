package store

import (
	"cmp"
	"context"
	"encoding/json"
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
	mu                sync.RWMutex
	byID              map[string]reading.Reading
	byURLKey          map[string]string
	batches           map[string]ManualBatch
	batchItems        map[string]ManualBatchItem
	batchItemsByBatch map[string][]string
}

// NewMemory returns an empty, concurrency-safe in-memory store.
func NewMemory() *Memory {
	return &Memory{
		byID:              map[string]reading.Reading{},
		byURLKey:          map[string]string{},
		batches:           map[string]ManualBatch{},
		batchItems:        map[string]ManualBatchItem{},
		batchItemsByBatch: map[string][]string{},
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
	if fields.ClearStartedAt {
		r.StartedAt = nil
	} else if fields.StartedAt != nil {
		r.StartedAt = cloneTimePtr(fields.StartedAt)
	} else if status == reading.Running {
		r.StartedAt = cloneTimePtr(&now)
	}
	if fields.ClearFinishedAt {
		r.FinishedAt = nil
	} else if fields.FinishedAt != nil {
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

// UpdateContent overwrites a reading's processed content fields without touching
// its lifecycle status, timestamps, error, attempt count, or tags.
func (m *Memory) UpdateContent(ctx context.Context, id string, fields ContentFields) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.byID[id]
	if !ok {
		return ErrNotFound
	}
	if fields.ExpectedStartedAt != nil && (r.Status != reading.Running || r.StartedAt == nil || !r.StartedAt.Equal(*fields.ExpectedStartedAt)) {
		return ErrConflict
	}

	now := fields.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	r.Title = fields.Title
	r.Author = fields.Author
	r.Site = fields.Site
	r.Lang = fields.Lang
	r.WordCount = fields.WordCount
	r.ExtractionMode = fields.ExtractionMode
	r.ContentKey = fields.ContentKey
	r.RawKey = fields.RawKey
	r.Summary = fields.Summary
	r.SummaryJSON = fields.SummaryJSON
	r.SimilarJSON = fields.SimilarJSON
	r.DiagnosticsJSON = fields.DiagnosticsJSON
	r.UpdatedAt = now

	m.byID[id] = cloneReading(r)
	return nil
}

// UpdateImport replaces a reading's source metadata with a user-supplied import
// and clears derived content so the next pipeline run starts from that import.
func (m *Memory) UpdateImport(ctx context.Context, id string, fields ImportFields) error {
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

	r.Status = reading.Pending
	r.SourceKind = fields.SourceKind
	r.Title = fields.Title
	r.Author = ""
	r.Site = ""
	r.Lang = ""
	r.WordCount = 0
	r.ExtractionMode = ""
	r.ContentKey = ""
	r.RawKey = fields.RawKey
	r.Summary = ""
	r.SummaryJSON = nil
	r.SimilarJSON = nil
	r.DiagnosticsJSON = nil
	r.Error = ""
	r.StartedAt = nil
	r.FinishedAt = nil
	r.ProcessAttempts = 0
	r.Tags = normalizeTags(fields.Tags)
	r.UpdatedAt = now

	m.byID[id] = cloneReading(r)
	return nil
}

// Reprocess atomically clears derived content and marks a reading pending for a
// fresh operator-requested run.
func (m *Memory) Reprocess(ctx context.Context, id string, fields ReprocessFields) error {
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

	r.Status = reading.Pending
	r.Title = fields.Title
	r.Author = ""
	r.Site = ""
	r.Lang = ""
	r.WordCount = 0
	r.ExtractionMode = ""
	r.ContentKey = ""
	r.RawKey = fields.RawKey
	r.Summary = ""
	r.SummaryJSON = nil
	r.SimilarJSON = nil
	r.DiagnosticsJSON = nil
	r.Error = ""
	r.StartedAt = nil
	r.FinishedAt = nil
	r.ProcessAttempts = 0
	r.UpdatedAt = now

	m.byID[id] = cloneReading(r)
	return nil
}

// ReplaceTags replaces a reading's tag set.
func (m *Memory) ReplaceTags(ctx context.Context, id string, tags []string, fields TagFields) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.byID[id]
	if !ok {
		return ErrNotFound
	}
	if fields.ExpectedStartedAt != nil && (r.Status != reading.Running || r.StartedAt == nil || !r.StartedAt.Equal(*fields.ExpectedStartedAt)) {
		return ErrConflict
	}
	now := fields.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	r.Tags = normalizeTags(tags)
	r.UpdatedAt = now
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

// CreatePlannedBatch stores a new planned manual batch and its request items.
func (m *Memory) CreatePlannedBatch(ctx context.Context, fields BatchCreateFields) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.batches[fields.ID]; ok {
		return ErrConflict
	}
	seenCustomIDs := map[string]bool{}
	seenReadingIDs := map[string]bool{}
	for _, item := range fields.Items {
		if !json.Valid(item.RequestJSON) {
			return ErrConflict
		}
		if seenCustomIDs[item.CustomID] {
			return ErrConflict
		}
		seenCustomIDs[item.CustomID] = true
		if seenReadingIDs[item.ReadingID] {
			return ErrConflict
		}
		seenReadingIDs[item.ReadingID] = true
		if _, ok := m.batchItems[item.CustomID]; ok {
			return ErrConflict
		}
		for _, existing := range m.batchItems {
			if existing.ReadingID == item.ReadingID && existing.State.active() {
				return ErrConflict
			}
		}
	}

	now := fields.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	batch := ManualBatch{
		ID:        fields.ID,
		State:     BatchStatePlanned,
		CreatedAt: now,
		UpdatedAt: now,
	}
	m.batches[fields.ID] = cloneManualBatch(batch)

	customIDs := make([]string, 0, len(fields.Items))
	for _, item := range fields.Items {
		stored := ManualBatchItem{
			BatchID:     fields.ID,
			ReadingID:   item.ReadingID,
			CustomID:    item.CustomID,
			State:       BatchItemStatePlanned,
			RequestJSON: cloneBytes(item.RequestJSON),
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		m.batchItems[item.CustomID] = cloneManualBatchItem(stored)
		customIDs = append(customIDs, item.CustomID)
	}
	m.batchItemsByBatch[fields.ID] = customIDs
	return nil
}

// SubmitBatch records remote metadata and marks the batch's planned items submitted.
func (m *Memory) SubmitBatch(ctx context.Context, id string, fields BatchSubmitFields) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	batch, ok := m.batches[id]
	if !ok {
		return ErrNotFound
	}
	if batch.SubmittedAt != nil {
		if batchSubmitEqual(batch, fields) {
			return nil
		}
		return ErrConflict
	}
	if batch.State != BatchStatePlanned {
		return ErrConflict
	}
	if err := validateBatchCounts(fields.Counts); err != nil {
		return err
	}
	if fields.RemoteID != "" {
		for otherID, other := range m.batches {
			if otherID != id && other.RemoteID == fields.RemoteID {
				return ErrConflict
			}
		}
	}

	customIDs := m.batchItemsByBatch[id]
	for _, customID := range customIDs {
		if m.batchItems[customID].State != BatchItemStatePlanned {
			return ErrConflict
		}
	}

	now := fields.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	batch.State = BatchStateSubmitted
	batch.RemoteID = fields.RemoteID
	batch.ResultsURL = fields.ResultsURL
	batch.Counts = fields.Counts
	batch.SubmittedAt = cloneTimePtr(&now)
	batch.UpdatedAt = now
	m.batches[id] = cloneManualBatch(batch)

	for _, customID := range customIDs {
		item := m.batchItems[customID]
		item.State = BatchItemStateSubmitted
		item.SubmittedAt = cloneTimePtr(&now)
		item.UpdatedAt = now
		m.batchItems[customID] = cloneManualBatchItem(item)
	}
	return nil
}

// UpdateBatchState records a manual batch state transition.
func (m *Memory) UpdateBatchState(ctx context.Context, id string, state BatchState, fields BatchStateFields) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	batch, ok := m.batches[id]
	if !ok {
		return ErrNotFound
	}
	if fields.Counts != nil {
		if err := validateBatchCounts(*fields.Counts); err != nil {
			return err
		}
	}
	sameState := batch.State == state
	if !sameState {
		if state == BatchStateApplied && m.hasActiveBatchItemLocked(id) {
			return ErrConflict
		}
		if !validBatchTransition(batch.State, state) {
			return ErrConflict
		}
	}

	now := fields.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if !sameState {
		batch.State = state
	}
	if fields.ResultsURL != "" {
		batch.ResultsURL = fields.ResultsURL
	}
	if fields.Counts != nil {
		batch.Counts = *fields.Counts
	}
	if !sameState {
		switch state {
		case BatchStateResultsReady:
			batch.FinishedAt = cloneTimePtr(&now)
		case BatchStateApplied:
			if batch.FinishedAt == nil {
				batch.FinishedAt = cloneTimePtr(&now)
			}
			batch.AppliedAt = cloneTimePtr(&now)
		case BatchStateCanceled, BatchStateFailed:
			batch.FinishedAt = cloneTimePtr(&now)
		}
	}
	batch.UpdatedAt = now
	m.batches[id] = cloneManualBatch(batch)
	if !sameState {
		itemState, ok := terminalItemStateForBatch(state)
		if !ok {
			return nil
		}
		for _, customID := range m.batchItemsByBatch[id] {
			item := m.batchItems[customID]
			if !item.State.active() {
				continue
			}
			item.State = itemState
			item.FinishedAt = cloneTimePtr(&now)
			item.UpdatedAt = now
			m.batchItems[customID] = cloneManualBatchItem(item)
		}
	}
	return nil
}

// GetBatch returns one manual batch by id.
func (m *Memory) GetBatch(ctx context.Context, id string) (ManualBatch, error) {
	if err := ctx.Err(); err != nil {
		return ManualBatch{}, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	batch, ok := m.batches[id]
	if !ok {
		return ManualBatch{}, ErrNotFound
	}
	return cloneManualBatch(batch), nil
}

// ListBatches returns manual batches matching q.
func (m *Memory) ListBatches(ctx context.Context, q BatchQuery) ([]ManualBatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	out := make([]ManualBatch, 0, len(m.batches))
	for _, batch := range m.batches {
		if q.State != "" && batch.State != q.State {
			continue
		}
		if q.ActiveOnly && !batch.State.active() {
			continue
		}
		out = append(out, cloneManualBatch(batch))
	}
	m.mu.RUnlock()

	sortBatches(out)
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// ListBatchItems returns every item in a manual batch in creation order.
func (m *Memory) ListBatchItems(ctx context.Context, batchID string) ([]ManualBatchItem, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	customIDs, ok := m.batchItemsByBatch[batchID]
	if !ok {
		return nil, ErrNotFound
	}
	out := make([]ManualBatchItem, 0, len(customIDs))
	for _, customID := range customIDs {
		out = append(out, cloneManualBatchItem(m.batchItems[customID]))
	}
	return out, nil
}

// GetBatchItemByCustomID returns one manual batch item by its stable custom_id.
func (m *Memory) GetBatchItemByCustomID(ctx context.Context, customID string) (ManualBatchItem, error) {
	if err := ctx.Err(); err != nil {
		return ManualBatchItem{}, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	item, ok := m.batchItems[customID]
	if !ok {
		return ManualBatchItem{}, ErrNotFound
	}
	return cloneManualBatchItem(item), nil
}

func (m *Memory) hasActiveBatchItemLocked(batchID string) bool {
	for _, customID := range m.batchItemsByBatch[batchID] {
		if m.batchItems[customID].State.active() {
			return true
		}
	}
	return false
}

// WriteBatchItemResult stores one remote terminal result by custom_id.
func (m *Memory) WriteBatchItemResult(ctx context.Context, customID string, fields BatchItemResultFields) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !fields.State.resultState() {
		return ErrConflict
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	item, ok := m.batchItems[customID]
	if !ok {
		return ErrNotFound
	}
	if item.FinishedAt != nil {
		equal, err := batchItemResultEqualJSON(item, fields)
		if err != nil {
			return err
		}
		if equal {
			return nil
		}
		return ErrConflict
	}
	if item.State != BatchItemStateSubmitted {
		return ErrConflict
	}
	if !json.Valid(fields.ResultJSON) {
		return ErrConflict
	}

	now := fields.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	item.State = fields.State
	item.ResultJSON = cloneBytes(fields.ResultJSON)
	item.ErrorType = fields.ErrorType
	item.ErrorMessage = fields.ErrorMessage
	item.FinishedAt = cloneTimePtr(&now)
	item.UpdatedAt = now
	m.batchItems[customID] = cloneManualBatchItem(item)
	return nil
}

// MarkBatchItemApplied marks a succeeded item as applied to its reading.
func (m *Memory) MarkBatchItemApplied(ctx context.Context, customID string, fields BatchItemApplyFields) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	item, ok := m.batchItems[customID]
	if !ok {
		return ErrNotFound
	}
	if item.State == BatchItemStateApplied {
		return nil
	}
	if item.State != BatchItemStateSucceeded {
		return ErrConflict
	}

	now := fields.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	item.State = BatchItemStateApplied
	item.AppliedAt = cloneTimePtr(&now)
	item.UpdatedAt = now
	m.batchItems[customID] = cloneManualBatchItem(item)
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
	r.SummaryJSON = cloneBytes(r.SummaryJSON)
	r.SimilarJSON = cloneBytes(r.SimilarJSON)
	r.DiagnosticsJSON = cloneBytes(r.DiagnosticsJSON)
	return r
}

func cloneManualBatch(batch ManualBatch) ManualBatch {
	batch.SubmittedAt = cloneTimePtr(batch.SubmittedAt)
	batch.FinishedAt = cloneTimePtr(batch.FinishedAt)
	batch.AppliedAt = cloneTimePtr(batch.AppliedAt)
	return batch
}

func cloneManualBatchItem(item ManualBatchItem) ManualBatchItem {
	item.RequestJSON = cloneBytes(item.RequestJSON)
	item.ResultJSON = cloneBytes(item.ResultJSON)
	item.SubmittedAt = cloneTimePtr(item.SubmittedAt)
	item.FinishedAt = cloneTimePtr(item.FinishedAt)
	item.AppliedAt = cloneTimePtr(item.AppliedAt)
	return item
}

func sortBatches(batches []ManualBatch) {
	slices.SortFunc(batches, func(a, b ManualBatch) int {
		if n := b.CreatedAt.Compare(a.CreatedAt); n != 0 {
			return n
		}
		return strings.Compare(b.ID, a.ID)
	})
}

func (state BatchState) active() bool {
	switch state {
	case BatchStatePlanned, BatchStateSubmitted, BatchStateResultsReady:
		return true
	default:
		return false
	}
}

func validBatchTransition(from, to BatchState) bool {
	switch from {
	case BatchStatePlanned:
		return to == BatchStateCanceled || to == BatchStateFailed
	case BatchStateSubmitted:
		return to == BatchStateResultsReady || to == BatchStateCanceled || to == BatchStateFailed
	case BatchStateResultsReady:
		return to == BatchStateApplied || to == BatchStateCanceled || to == BatchStateFailed
	default:
		return false
	}
}

func terminalItemStateForBatch(state BatchState) (BatchItemState, bool) {
	switch state {
	case BatchStateCanceled:
		return BatchItemStateCanceled, true
	case BatchStateFailed:
		return BatchItemStateFailed, true
	default:
		return "", false
	}
}

func (state BatchItemState) active() bool {
	switch state {
	case BatchItemStatePlanned, BatchItemStateSubmitted, BatchItemStateSucceeded:
		return true
	default:
		return false
	}
}

func (state BatchItemState) resultState() bool {
	switch state {
	case BatchItemStateSucceeded, BatchItemStateErrored, BatchItemStateCanceled, BatchItemStateExpired:
		return true
	default:
		return false
	}
}

func batchSubmitEqual(batch ManualBatch, fields BatchSubmitFields) bool {
	return batch.RemoteID == fields.RemoteID &&
		batch.ResultsURL == fields.ResultsURL &&
		batch.Counts == fields.Counts
}

func cloneBytes(in []byte) []byte {
	if in == nil {
		return nil
	}
	return slices.Clone(in)
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
