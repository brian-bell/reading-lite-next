package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bbell/reading-lite/internal/reading"
	"github.com/bbell/reading-lite/internal/store/storedb"
)

// Postgres is a pgx-backed Store implementation.
type Postgres struct {
	pool    *pgxpool.Pool
	queries *storedb.Queries
}

// NewPostgres returns a Store backed by pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool, queries: storedb.New(pool)}
}

// SaveReading inserts r and returns ErrConflict when id or url_key already exists.
func (p *Postgres) SaveReading(ctx context.Context, r reading.Reading) error {
	_, err := p.queries.CreateReading(ctx, storedb.CreateReadingParams{
		ID:              r.ID,
		Url:             r.URL,
		UrlKey:          r.URLKey,
		Status:          string(r.Status),
		SourceKind:      string(r.SourceKind),
		Column6:         r.Title,
		Column7:         r.Author,
		Column8:         r.Site,
		Column9:         r.Lang,
		WordCount:       int32Ptr(r.WordCount),
		Column11:        r.ExtractionMode,
		Column12:        r.ContentKey,
		Column13:        r.RawKey,
		Column14:        r.Summary,
		SummaryJson:     r.SummaryJSON,
		SimilarJson:     r.SimilarJSON,
		DiagnosticsJson: r.DiagnosticsJSON,
		Column18:        r.Error,
		ProcessAttempts: int32(r.ProcessAttempts),
		Tags:            normalizeTags(r.Tags),
		CreatedAt:       timestamptz(r.CreatedAt),
		StartedAt:       timestamptzPtr(r.StartedAt),
		FinishedAt:      timestamptzPtr(r.FinishedAt),
		UpdatedAt:       timestamptz(r.UpdatedAt),
	})
	if errors.Is(err, pgx.ErrNoRows) || isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

// GetByID returns one reading by id.
func (p *Postgres) GetByID(ctx context.Context, id string) (reading.Reading, error) {
	row, err := p.queries.GetReadingByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return reading.Reading{}, ErrNotFound
	}
	if err != nil {
		return reading.Reading{}, err
	}
	return readingFromGetByID(row), nil
}

// GetByURLKey returns one reading by normalized URL key.
func (p *Postgres) GetByURLKey(ctx context.Context, key string) (reading.Reading, error) {
	row, err := p.queries.GetReadingByURLKey(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return reading.Reading{}, ErrNotFound
	}
	if err != nil {
		return reading.Reading{}, err
	}
	return readingFromGetByURLKey(row), nil
}

// UpdateStatus changes a reading status and advances status timestamps.
func (p *Postgres) UpdateStatus(ctx context.Context, id string, status reading.Status, fields StatusFields) error {
	r, err := p.GetByID(ctx, id)
	if err != nil {
		return err
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

	rows, err := p.queries.UpdateReadingStatus(ctx, storedb.UpdateReadingStatusParams{
		ID:              r.ID,
		Status:          string(r.Status),
		StartedAt:       timestamptzPtr(r.StartedAt),
		FinishedAt:      timestamptzPtr(r.FinishedAt),
		Column5:         r.Error,
		ProcessAttempts: int32(r.ProcessAttempts),
		UpdatedAt:       timestamptz(r.UpdatedAt),
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateContent overwrites a reading's processed content fields without touching
// its lifecycle status, timestamps, error, attempt count, or tags.
func (p *Postgres) UpdateContent(ctx context.Context, id string, fields ContentFields) error {
	now := fields.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	rows, err := p.queries.UpdateReadingContent(ctx, storedb.UpdateReadingContentParams{
		ID:              id,
		Column2:         fields.Title,
		Column3:         fields.Author,
		Column4:         fields.Site,
		Column5:         fields.Lang,
		WordCount:       int32Ptr(fields.WordCount),
		Column7:         fields.ExtractionMode,
		Column8:         fields.ContentKey,
		Column9:         fields.RawKey,
		Column10:        fields.Summary,
		SummaryJson:     fields.SummaryJSON,
		SimilarJson:     fields.SimilarJSON,
		DiagnosticsJson: fields.DiagnosticsJSON,
		UpdatedAt:       timestamptz(now),
		Column15:        timestamptzPtr(fields.ExpectedStartedAt),
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		if fields.ExpectedStartedAt != nil {
			if _, getErr := p.GetByID(ctx, id); getErr == nil {
				return ErrConflict
			}
		}
		return ErrNotFound
	}
	return nil
}

// UpdateImport replaces a reading's source metadata with a user-supplied import
// and clears derived content so the next pipeline run starts from that import.
func (p *Postgres) UpdateImport(ctx context.Context, id string, fields ImportFields) error {
	now := fields.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	rows, err := p.queries.UpdateReadingImport(ctx, storedb.UpdateReadingImportParams{
		ID:         id,
		SourceKind: string(fields.SourceKind),
		Column3:    fields.Title,
		Column4:    fields.RawKey,
		Tags:       normalizeTags(fields.Tags),
		UpdatedAt:  timestamptz(now),
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// Reprocess atomically clears derived content and marks a reading pending for a
// fresh operator-requested run.
func (p *Postgres) Reprocess(ctx context.Context, id string, fields ReprocessFields) error {
	now := fields.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	rows, err := p.queries.ReprocessReading(ctx, storedb.ReprocessReadingParams{
		ID:        id,
		Column2:   fields.RawKey,
		Column3:   fields.Title,
		UpdatedAt: timestamptz(now),
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// ReplaceTags replaces a reading's tag set.
func (p *Postgres) ReplaceTags(ctx context.Context, id string, tags []string, fields TagFields) error {
	now := fields.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	rows, err := p.queries.ReplaceReadingTags(ctx, storedb.ReplaceReadingTagsParams{
		ID:        id,
		Tags:      normalizeTags(tags),
		UpdatedAt: timestamptz(now),
		Column4:   timestamptzPtr(fields.ExpectedStartedAt),
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		if fields.ExpectedStartedAt != nil {
			if _, getErr := p.GetByID(ctx, id); getErr == nil {
				return ErrConflict
			}
		}
		return ErrNotFound
	}
	return nil
}

// Search returns a filtered, sorted, bounded page of readings.
func (p *Postgres) Search(ctx context.Context, q Query) (Page, error) {
	tags := cloneStrings(q.Tags)
	if tags == nil {
		tags = []string{}
	}
	status := string(q.Status)
	total, err := p.queries.CountReadings(ctx, storedb.CountReadingsParams{
		Column1: q.Q,
		Column2: tags,
		Column3: status,
	})
	if err != nil {
		return Page{}, err
	}

	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	matches, err := p.searchRows(ctx, q, tags, status, limit+1)
	if err != nil {
		return Page{}, err
	}

	page := Page{Total: int(total)}
	if len(matches) > limit {
		last := matches[limit-1]
		page.NextCursor = Cursor{
			CreatedAt: last.reading.CreatedAt,
			ID:        last.reading.ID,
			Title:     last.reading.Title,
			Rank:      float64(last.rank),
			Valid:     true,
		}
		matches = matches[:limit]
	}
	page.Readings = make([]reading.Reading, len(matches))
	for i, match := range matches {
		page.Readings[i] = match.reading
	}
	return page, nil
}

// ListNonTerminal returns pending readings and running readings started before runningCutoff.
func (p *Postgres) ListNonTerminal(ctx context.Context, runningCutoff time.Time) ([]Pending, error) {
	rows, err := p.queries.ListNonTerminalReadings(ctx, timestamptz(runningCutoff))
	if err != nil {
		return nil, err
	}
	out := make([]Pending, len(rows))
	for i, row := range rows {
		out[i] = Pending{ID: row.ID, ProcessAttempts: int(row.ProcessAttempts)}
	}
	return out, nil
}

// Delete removes one reading.
func (p *Postgres) Delete(ctx context.Context, id string) error {
	rows, err := p.queries.DeleteReading(ctx, id)
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// CreatePlannedBatch stores a new planned manual batch and its request items.
func (p *Postgres) CreatePlannedBatch(ctx context.Context, fields BatchCreateFields) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback(context.Background())
	}()

	q := p.queries.WithTx(tx)
	now := fields.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if err := q.CreateManualBatch(ctx, storedb.CreateManualBatchParams{
		ID:        fields.ID,
		State:     string(BatchStatePlanned),
		CreatedAt: timestamptz(now),
		UpdatedAt: timestamptz(now),
	}); err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	}
	for i, item := range fields.Items {
		if !json.Valid(item.RequestJSON) {
			return ErrConflict
		}
		if err := q.CreateManualBatchItem(ctx, storedb.CreateManualBatchItemParams{
			CustomID:    item.CustomID,
			BatchID:     fields.ID,
			ReadingID:   item.ReadingID,
			State:       string(BatchItemStatePlanned),
			ItemIndex:   int32(i),
			RequestJson: cloneBytes(item.RequestJSON),
			CreatedAt:   timestamptz(now),
			UpdatedAt:   timestamptz(now),
		}); err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return err
	}
	return nil
}

// SubmitBatch records remote metadata and marks the batch's planned items submitted.
func (p *Postgres) SubmitBatch(ctx context.Context, id string, fields BatchSubmitFields) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback(context.Background())
	}()

	q := p.queries.WithTx(tx)
	batchRow, err := q.GetManualBatch(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	batch := manualBatchFromDB(batchRow)
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

	items, err := q.ListManualBatchItems(ctx, id)
	if err != nil {
		return err
	}
	for _, item := range items {
		if BatchItemState(item.State) != BatchItemStatePlanned {
			return ErrConflict
		}
	}

	now := fields.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	rows, err := q.SubmitManualBatch(ctx, storedb.SubmitManualBatchParams{
		ID:              id,
		Column2:         fields.RemoteID,
		Column3:         fields.ResultsURL,
		ProcessingCount: int32(fields.Counts.Processing),
		SucceededCount:  int32(fields.Counts.Succeeded),
		ErroredCount:    int32(fields.Counts.Errored),
		CanceledCount:   int32(fields.Counts.Canceled),
		ExpiredCount:    int32(fields.Counts.Expired),
		SubmittedAt:     timestamptz(now),
	})
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	}
	if rows == 0 {
		return ErrConflict
	}
	itemRows, err := q.SubmitManualBatchItems(ctx, storedb.SubmitManualBatchItemsParams{
		BatchID:     id,
		SubmittedAt: timestamptz(now),
	})
	if err != nil {
		return err
	}
	if itemRows != int64(len(items)) {
		return ErrConflict
	}
	if err := tx.Commit(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return err
	}
	return nil
}

// UpdateBatchState records a manual batch state transition.
func (p *Postgres) UpdateBatchState(ctx context.Context, id string, state BatchState, fields BatchStateFields) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback(context.Background())
	}()

	q := p.queries.WithTx(tx)
	row, err := q.GetManualBatch(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	batch := manualBatchFromDB(row)
	if fields.Counts != nil {
		if err := validateBatchCounts(*fields.Counts); err != nil {
			return err
		}
	}
	sameState := batch.State == state
	if !sameState && state == BatchStateApplied {
		activeItems, err := q.CountActiveManualBatchItems(ctx, id)
		if err != nil {
			return err
		}
		if activeItems > 0 {
			return ErrConflict
		}
	}
	if !sameState && !validBatchTransition(batch.State, state) {
		return ErrConflict
	}

	now := fields.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	counts := batch.Counts
	if fields.Counts != nil {
		counts = *fields.Counts
	}
	resultsURL := batch.ResultsURL
	if fields.ResultsURL != "" {
		resultsURL = fields.ResultsURL
	}
	finishedAt := batch.FinishedAt
	appliedAt := batch.AppliedAt
	if !sameState {
		switch state {
		case BatchStateResultsReady:
			finishedAt = cloneTimePtr(&now)
		case BatchStateApplied:
			if finishedAt == nil {
				finishedAt = cloneTimePtr(&now)
			}
			appliedAt = cloneTimePtr(&now)
		case BatchStateCanceled, BatchStateFailed:
			finishedAt = cloneTimePtr(&now)
		}
	}

	rows, err := q.UpdateManualBatchState(ctx, storedb.UpdateManualBatchStateParams{
		ID:              id,
		State:           string(state),
		Column3:         resultsURL,
		ProcessingCount: int32(counts.Processing),
		SucceededCount:  int32(counts.Succeeded),
		ErroredCount:    int32(counts.Errored),
		CanceledCount:   int32(counts.Canceled),
		ExpiredCount:    int32(counts.Expired),
		FinishedAt:      timestamptzPtr(finishedAt),
		AppliedAt:       timestamptzPtr(appliedAt),
		UpdatedAt:       timestamptz(now),
		State_2:         string(batch.State),
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrConflict
	}
	if !sameState {
		itemState, ok := terminalItemStateForBatch(state)
		if !ok {
			if err := tx.Commit(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				return err
			}
			return nil
		}
		if _, err := q.UpdateManualBatchActiveItemsTerminal(ctx, storedb.UpdateManualBatchActiveItemsTerminalParams{
			BatchID:    id,
			State:      string(itemState),
			FinishedAt: timestamptz(now),
		}); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return err
	}
	return nil
}

// GetBatch returns one manual batch by id.
func (p *Postgres) GetBatch(ctx context.Context, id string) (ManualBatch, error) {
	row, err := p.queries.GetManualBatch(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ManualBatch{}, ErrNotFound
	}
	if err != nil {
		return ManualBatch{}, err
	}
	return manualBatchFromDB(row), nil
}

// ListBatches returns manual batches matching q.
func (p *Postgres) ListBatches(ctx context.Context, q BatchQuery) ([]ManualBatch, error) {
	rows, err := p.queries.ListManualBatches(ctx, storedb.ListManualBatchesParams{
		Column1: string(q.State),
		Column2: q.ActiveOnly,
	})
	if err != nil {
		return nil, err
	}
	if q.Limit > 0 && len(rows) > q.Limit {
		rows = rows[:q.Limit]
	}
	out := make([]ManualBatch, len(rows))
	for i, row := range rows {
		out[i] = manualBatchFromDB(row)
	}
	return out, nil
}

// ListBatchItems returns every item in a manual batch in creation order.
func (p *Postgres) ListBatchItems(ctx context.Context, batchID string) ([]ManualBatchItem, error) {
	if _, err := p.GetBatch(ctx, batchID); err != nil {
		return nil, err
	}
	rows, err := p.queries.ListManualBatchItems(ctx, batchID)
	if err != nil {
		return nil, err
	}
	out := make([]ManualBatchItem, len(rows))
	for i, row := range rows {
		out[i] = manualBatchItemFromDB(row)
	}
	return out, nil
}

// GetBatchItemByCustomID returns one manual batch item by its stable custom_id.
func (p *Postgres) GetBatchItemByCustomID(ctx context.Context, customID string) (ManualBatchItem, error) {
	row, err := p.queries.GetManualBatchItemByCustomID(ctx, customID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ManualBatchItem{}, ErrNotFound
	}
	if err != nil {
		return ManualBatchItem{}, err
	}
	return manualBatchItemFromDB(row), nil
}

// WriteBatchItemResult stores one remote terminal result by custom_id.
func (p *Postgres) WriteBatchItemResult(ctx context.Context, customID string, fields BatchItemResultFields) error {
	if !fields.State.resultState() {
		return ErrConflict
	}

	item, err := p.GetBatchItemByCustomID(ctx, customID)
	if err != nil {
		return err
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
	rows, err := p.queries.WriteManualBatchItemResult(ctx, storedb.WriteManualBatchItemResultParams{
		CustomID:   customID,
		State:      string(fields.State),
		ResultJson: cloneBytes(fields.ResultJSON),
		Column4:    fields.ErrorType,
		Column5:    fields.ErrorMessage,
		FinishedAt: timestamptz(now),
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		latest, getErr := p.GetBatchItemByCustomID(ctx, customID)
		if getErr != nil {
			return getErr
		}
		equal, equalErr := batchItemResultEqualJSON(latest, fields)
		if equalErr != nil {
			return equalErr
		}
		if equal {
			return nil
		}
		return ErrConflict
	}
	return nil
}

// MarkBatchItemApplied marks a succeeded item as applied to its reading.
func (p *Postgres) MarkBatchItemApplied(ctx context.Context, customID string, fields BatchItemApplyFields) error {
	item, err := p.GetBatchItemByCustomID(ctx, customID)
	if err != nil {
		return err
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
	rows, err := p.queries.MarkManualBatchItemApplied(ctx, storedb.MarkManualBatchItemAppliedParams{
		CustomID:  customID,
		AppliedAt: timestamptz(now),
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		latest, getErr := p.GetBatchItemByCustomID(ctx, customID)
		if getErr != nil {
			return getErr
		}
		if latest.State == BatchItemStateApplied {
			return nil
		}
		return ErrConflict
	}
	return nil
}

func (p *Postgres) searchRows(ctx context.Context, q Query, tags []string, status string, limit int) ([]postgresSearchMatch, error) {
	switch q.Sort {
	case SortOldest:
		rows, err := p.queries.SearchReadingsOldest(ctx, storedb.SearchReadingsOldestParams{
			Column1: q.Q,
			Column2: tags,
			Column3: status,
			Column4: q.Cursor.Valid,
			Column5: float32(q.Cursor.Rank),
			Column6: timestamptz(q.Cursor.CreatedAt),
			Column7: q.Cursor.ID,
			Limit:   int32(limit),
		})
		if err != nil {
			return nil, err
		}
		return readingsFromOldestRows(rows), nil
	case SortTitle:
		rows, err := p.queries.SearchReadingsTitle(ctx, storedb.SearchReadingsTitleParams{
			Column1: q.Q,
			Column2: tags,
			Column3: status,
			Column4: q.Cursor.Valid,
			Column5: float32(q.Cursor.Rank),
			Column6: q.Cursor.Title,
			Column7: q.Cursor.ID,
			Limit:   int32(limit),
		})
		if err != nil {
			return nil, err
		}
		return readingsFromTitleRows(rows), nil
	default:
		rows, err := p.queries.SearchReadingsNewest(ctx, storedb.SearchReadingsNewestParams{
			Column1: q.Q,
			Column2: tags,
			Column3: status,
			Column4: q.Cursor.Valid,
			Column5: float32(q.Cursor.Rank),
			Column6: timestamptz(q.Cursor.CreatedAt),
			Column7: q.Cursor.ID,
			Limit:   int32(limit),
		})
		if err != nil {
			return nil, err
		}
		return readingsFromNewestRows(rows), nil
	}
}

func readingFromGetByID(row storedb.GetReadingByIDRow) reading.Reading {
	return readingFromFields(readingFields{
		id:              row.ID,
		url:             row.Url,
		urlKey:          row.UrlKey,
		status:          row.Status,
		sourceKind:      row.SourceKind,
		title:           row.Title,
		author:          row.Author,
		site:            row.Site,
		lang:            row.Lang,
		wordCount:       row.WordCount,
		extractionMode:  row.ExtractionMode,
		contentKey:      row.ContentKey,
		rawKey:          row.RawKey,
		summary:         row.Summary,
		summaryJSON:     row.SummaryJson,
		similarJSON:     row.SimilarJson,
		diagnosticsJSON: row.DiagnosticsJson,
		err:             row.Error,
		processAttempts: row.ProcessAttempts,
		tags:            row.Tags,
		createdAt:       row.CreatedAt,
		startedAt:       row.StartedAt,
		finishedAt:      row.FinishedAt,
		updatedAt:       row.UpdatedAt,
	})
}

func readingFromGetByURLKey(row storedb.GetReadingByURLKeyRow) reading.Reading {
	return readingFromFields(readingFields{
		id:              row.ID,
		url:             row.Url,
		urlKey:          row.UrlKey,
		status:          row.Status,
		sourceKind:      row.SourceKind,
		title:           row.Title,
		author:          row.Author,
		site:            row.Site,
		lang:            row.Lang,
		wordCount:       row.WordCount,
		extractionMode:  row.ExtractionMode,
		contentKey:      row.ContentKey,
		rawKey:          row.RawKey,
		summary:         row.Summary,
		summaryJSON:     row.SummaryJson,
		similarJSON:     row.SimilarJson,
		diagnosticsJSON: row.DiagnosticsJson,
		err:             row.Error,
		processAttempts: row.ProcessAttempts,
		tags:            row.Tags,
		createdAt:       row.CreatedAt,
		startedAt:       row.StartedAt,
		finishedAt:      row.FinishedAt,
		updatedAt:       row.UpdatedAt,
	})
}

func manualBatchFromDB(row storedb.ManualBatch) ManualBatch {
	return ManualBatch{
		ID:         row.ID,
		State:      BatchState(row.State),
		RemoteID:   stringValue(row.RemoteID),
		ResultsURL: stringValue(row.ResultsUrl),
		Counts: BatchCounts{
			Processing: int(row.ProcessingCount),
			Succeeded:  int(row.SucceededCount),
			Errored:    int(row.ErroredCount),
			Canceled:   int(row.CanceledCount),
			Expired:    int(row.ExpiredCount),
		},
		CreatedAt:   timeValue(row.CreatedAt),
		SubmittedAt: timePtrValue(row.SubmittedAt),
		FinishedAt:  timePtrValue(row.FinishedAt),
		AppliedAt:   timePtrValue(row.AppliedAt),
		UpdatedAt:   timeValue(row.UpdatedAt),
	}
}

func manualBatchItemFromDB(row storedb.ManualBatchItem) ManualBatchItem {
	return ManualBatchItem{
		BatchID:      row.BatchID,
		ReadingID:    row.ReadingID,
		CustomID:     row.CustomID,
		State:        BatchItemState(row.State),
		RequestJSON:  jsonValue(row.RequestJson),
		ResultJSON:   jsonValue(row.ResultJson),
		ErrorType:    stringValue(row.ErrorType),
		ErrorMessage: stringValue(row.ErrorMessage),
		CreatedAt:    timeValue(row.CreatedAt),
		SubmittedAt:  timePtrValue(row.SubmittedAt),
		FinishedAt:   timePtrValue(row.FinishedAt),
		AppliedAt:    timePtrValue(row.AppliedAt),
		UpdatedAt:    timeValue(row.UpdatedAt),
	}
}

func batchItemResultEqualJSON(item ManualBatchItem, fields BatchItemResultFields) (bool, error) {
	if item.State != fields.State || item.ErrorType != fields.ErrorType || item.ErrorMessage != fields.ErrorMessage {
		return false, nil
	}
	equal, err := jsonRawEqual(item.ResultJSON, fields.ResultJSON)
	if err != nil {
		return false, err
	}
	return equal, nil
}

func jsonRawEqual(a, b json.RawMessage) (bool, error) {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b), nil
	}
	var av any
	if err := json.Unmarshal(a, &av); err != nil {
		return false, err
	}
	var bv any
	if err := json.Unmarshal(b, &bv); err != nil {
		return false, err
	}
	return reflect.DeepEqual(av, bv), nil
}

type postgresSearchMatch struct {
	reading reading.Reading
	rank    float32
}

func readingsFromNewestRows(rows []storedb.SearchReadingsNewestRow) []postgresSearchMatch {
	out := make([]postgresSearchMatch, len(rows))
	for i, row := range rows {
		out[i] = postgresSearchMatch{reading: readingFromFields(readingFields{
			id:              row.ID,
			url:             row.Url,
			urlKey:          row.UrlKey,
			status:          row.Status,
			sourceKind:      row.SourceKind,
			title:           row.Title,
			author:          row.Author,
			site:            row.Site,
			lang:            row.Lang,
			wordCount:       row.WordCount,
			extractionMode:  row.ExtractionMode,
			contentKey:      row.ContentKey,
			rawKey:          row.RawKey,
			summary:         row.Summary,
			summaryJSON:     row.SummaryJson,
			similarJSON:     row.SimilarJson,
			diagnosticsJSON: row.DiagnosticsJson,
			err:             row.Error,
			processAttempts: row.ProcessAttempts,
			tags:            row.Tags,
			createdAt:       row.CreatedAt,
			startedAt:       row.StartedAt,
			finishedAt:      row.FinishedAt,
			updatedAt:       row.UpdatedAt,
		}), rank: row.SearchRank}
	}
	return out
}

func readingsFromOldestRows(rows []storedb.SearchReadingsOldestRow) []postgresSearchMatch {
	out := make([]postgresSearchMatch, len(rows))
	for i, row := range rows {
		out[i] = postgresSearchMatch{reading: readingFromFields(readingFields{
			id:              row.ID,
			url:             row.Url,
			urlKey:          row.UrlKey,
			status:          row.Status,
			sourceKind:      row.SourceKind,
			title:           row.Title,
			author:          row.Author,
			site:            row.Site,
			lang:            row.Lang,
			wordCount:       row.WordCount,
			extractionMode:  row.ExtractionMode,
			contentKey:      row.ContentKey,
			rawKey:          row.RawKey,
			summary:         row.Summary,
			summaryJSON:     row.SummaryJson,
			similarJSON:     row.SimilarJson,
			diagnosticsJSON: row.DiagnosticsJson,
			err:             row.Error,
			processAttempts: row.ProcessAttempts,
			tags:            row.Tags,
			createdAt:       row.CreatedAt,
			startedAt:       row.StartedAt,
			finishedAt:      row.FinishedAt,
			updatedAt:       row.UpdatedAt,
		}), rank: row.SearchRank}
	}
	return out
}

func readingsFromTitleRows(rows []storedb.SearchReadingsTitleRow) []postgresSearchMatch {
	out := make([]postgresSearchMatch, len(rows))
	for i, row := range rows {
		out[i] = postgresSearchMatch{reading: readingFromFields(readingFields{
			id:              row.ID,
			url:             row.Url,
			urlKey:          row.UrlKey,
			status:          row.Status,
			sourceKind:      row.SourceKind,
			title:           row.Title,
			author:          row.Author,
			site:            row.Site,
			lang:            row.Lang,
			wordCount:       row.WordCount,
			extractionMode:  row.ExtractionMode,
			contentKey:      row.ContentKey,
			rawKey:          row.RawKey,
			summary:         row.Summary,
			summaryJSON:     row.SummaryJson,
			similarJSON:     row.SimilarJson,
			diagnosticsJSON: row.DiagnosticsJson,
			err:             row.Error,
			processAttempts: row.ProcessAttempts,
			tags:            row.Tags,
			createdAt:       row.CreatedAt,
			startedAt:       row.StartedAt,
			finishedAt:      row.FinishedAt,
			updatedAt:       row.UpdatedAt,
		}), rank: row.SearchRank}
	}
	return out
}

type readingFields struct {
	id              string
	url             string
	urlKey          string
	status          string
	sourceKind      string
	title           *string
	author          *string
	site            *string
	lang            *string
	wordCount       *int32
	extractionMode  *string
	contentKey      *string
	rawKey          *string
	summary         *string
	summaryJSON     []byte
	similarJSON     []byte
	diagnosticsJSON []byte
	err             *string
	processAttempts int32
	tags            []string
	createdAt       pgtype.Timestamptz
	startedAt       pgtype.Timestamptz
	finishedAt      pgtype.Timestamptz
	updatedAt       pgtype.Timestamptz
}

func readingFromFields(f readingFields) reading.Reading {
	r := reading.Reading{
		ID:              f.id,
		URL:             f.url,
		URLKey:          f.urlKey,
		Status:          reading.Status(f.status),
		SourceKind:      reading.SourceKind(f.sourceKind),
		Title:           stringValue(f.title),
		Author:          stringValue(f.author),
		Site:            stringValue(f.site),
		Lang:            stringValue(f.lang),
		WordCount:       intValue(f.wordCount),
		ExtractionMode:  stringValue(f.extractionMode),
		ContentKey:      stringValue(f.contentKey),
		RawKey:          stringValue(f.rawKey),
		Summary:         stringValue(f.summary),
		SummaryJSON:     jsonValue(f.summaryJSON),
		SimilarJSON:     jsonValue(f.similarJSON),
		DiagnosticsJSON: jsonValue(f.diagnosticsJSON),
		Error:           stringValue(f.err),
		ProcessAttempts: int(f.processAttempts),
		Tags:            cloneStrings(f.tags),
		CreatedAt:       timeValue(f.createdAt),
		StartedAt:       timePtrValue(f.startedAt),
		FinishedAt:      timePtrValue(f.finishedAt),
		UpdatedAt:       timeValue(f.updatedAt),
	}
	if r.Tags == nil {
		r.Tags = []string{}
	}
	return r
}

func int32Ptr(v int) *int32 {
	if v == 0 {
		return nil
	}
	out := int32(v)
	return &out
}

func stringValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func jsonValue(v []byte) json.RawMessage {
	if len(v) == 0 {
		return nil
	}
	return json.RawMessage(v)
}

func intValue(v *int32) int {
	if v == nil {
		return 0
	}
	return int(*v)
}

func timestamptz(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func timestamptzPtr(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return timestamptz(*t)
}

func timeValue(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time.UTC()
}

func timePtrValue(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	out := t.Time.UTC()
	return &out
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
