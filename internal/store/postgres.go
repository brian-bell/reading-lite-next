package store

import (
	"context"
	"encoding/json"
	"errors"
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
	queries *storedb.Queries
}

// NewPostgres returns a Store backed by pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{queries: storedb.New(pool)}
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
	})
	if err != nil {
		return err
	}
	if rows == 0 {
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
func (p *Postgres) ReplaceTags(ctx context.Context, id string, tags []string) error {
	rows, err := p.queries.ReplaceReadingTags(ctx, storedb.ReplaceReadingTagsParams{
		ID:        id,
		Tags:      normalizeTags(tags),
		UpdatedAt: timestamptz(time.Now().UTC()),
	})
	if err != nil {
		return err
	}
	if rows == 0 {
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
