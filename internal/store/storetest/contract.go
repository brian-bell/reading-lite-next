// Package storetest defines the behavioral contract shared by Store implementations.
package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/bbell/reading-lite/internal/reading"
	"github.com/bbell/reading-lite/internal/store"
)

// Factory returns a fresh, empty Store for one contract test.
type Factory func(t *testing.T) store.Store

// RunContract runs the Store conformance suite.
func RunContract(t *testing.T, newStore Factory) {
	t.Helper()

	t.Run("RoundTrip", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		want := sampleReading("r1", "https://example.com/one", at(1), reading.Ready)
		want.Title = "Kubernetes for personal services"
		want.Author = "Ada"
		want.Summary = "A practical guide to a small database-backed service."
		want.Tags = []string{"go", "db"}

		if err := s.SaveReading(ctx, want); err != nil {
			t.Fatalf("SaveReading: %v", err)
		}
		got, err := s.GetByID(ctx, want.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("GetByID mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("SaveReadingAcceptsNilTags", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		r := sampleReading("r1", "https://example.com/tagless", at(1), reading.Pending)
		r.Tags = nil

		if err := s.SaveReading(ctx, r); err != nil {
			t.Fatalf("SaveReading with nil tags: %v", err)
		}
		got, err := s.GetByID(ctx, r.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if got.Tags == nil {
			t.Fatalf("Tags = nil, want non-nil empty slice")
		}
		if len(got.Tags) != 0 {
			t.Fatalf("Tags = %v, want empty", got.Tags)
		}
	})

	t.Run("URLKeyIdempotency", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		first := sampleReading("first", "https://example.com/a", at(1), reading.Pending)
		second := sampleReading("second", "https://example.com/a?utm_source=x", at(2), reading.Pending)
		second.URLKey = first.URLKey

		if err := s.SaveReading(ctx, first); err != nil {
			t.Fatalf("SaveReading first: %v", err)
		}
		if err := s.SaveReading(ctx, second); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("SaveReading duplicate url key error = %v, want ErrConflict", err)
		}
		duplicateID := sampleReading(first.ID, "https://example.com/other", at(3), reading.Pending)
		if err := s.SaveReading(ctx, duplicateID); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("SaveReading duplicate id error = %v, want ErrConflict", err)
		}
		got, err := s.GetByURLKey(ctx, first.URLKey)
		if err != nil {
			t.Fatalf("GetByURLKey existing: %v", err)
		}
		if got.ID != first.ID {
			t.Fatalf("GetByURLKey returned id %q, want first id %q", got.ID, first.ID)
		}
		if _, err := s.GetByURLKey(ctx, "https://example.com/missing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetByURLKey missing error = %v, want ErrNotFound", err)
		}
	})

	t.Run("SearchFTS", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		seed(ctx, t, s,
			withText(sampleReading("high", "https://example.com/high", at(3), reading.Ready), "Kubernetes Kubernetes", "Rae", "Kubernetes operators for Go services"),
			withText(sampleReading("low", "https://example.com/low", at(2), reading.Ready), "Personal infrastructure", "Sol", "Keeping kubernetes small"),
			withText(sampleReading("miss", "https://example.com/miss", at(1), reading.Ready), "SQLite notes", "Max", "Single-node storage"),
		)

		page, err := s.Search(ctx, store.Query{Q: "kubernetes", Sort: store.SortNewest, Limit: 10})
		if err != nil {
			t.Fatalf("Search kubernetes: %v", err)
		}
		gotIDs := ids(page.Readings)
		if want := []string{"high", "low"}; !slices.Equal(gotIDs, want) {
			t.Fatalf("Search kubernetes ids = %v, want ranked %v", gotIDs, want)
		}
		if page.Total != 2 {
			t.Fatalf("Search kubernetes total = %d, want 2", page.Total)
		}

		if _, err := s.Search(ctx, store.Query{Q: `'AND OR " 🧪`, Limit: 10}); err != nil {
			t.Fatalf("Search adversarial query returned error: %v", err)
		}
	})

	t.Run("TagFilterAND", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		a := sampleReading("a", "https://example.com/a", at(3), reading.Ready)
		a.Title = "Go database migrations"
		a.Tags = []string{"go", "db", "ops"}
		b := sampleReading("b", "https://example.com/b", at(2), reading.Ready)
		b.Title = "Go testing"
		b.Tags = []string{"go"}
		c := sampleReading("c", "https://example.com/c", at(1), reading.Ready)
		c.Title = "Database notes"
		c.Tags = []string{"db"}
		seed(ctx, t, s, a, b, c)

		page, err := s.Search(ctx, store.Query{Tags: []string{"go", "db"}, Sort: store.SortNewest, Limit: 10})
		if err != nil {
			t.Fatalf("Search tags: %v", err)
		}
		if got, want := ids(page.Readings), []string{"a"}; !slices.Equal(got, want) {
			t.Fatalf("Search tags ids = %v, want %v", got, want)
		}

		page, err = s.Search(ctx, store.Query{Q: "migrations", Tags: []string{"go", "db"}, Sort: store.SortNewest, Limit: 10})
		if err != nil {
			t.Fatalf("Search q+tags: %v", err)
		}
		if got, want := ids(page.Readings), []string{"a"}; !slices.Equal(got, want) {
			t.Fatalf("Search q+tags ids = %v, want %v", got, want)
		}
	})

	t.Run("StatusFilterAndSortModes", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		alpha := sampleReading("alpha", "https://example.com/alpha", at(1), reading.Ready)
		alpha.Title = "Alpha"
		bravo := sampleReading("bravo", "https://example.com/bravo", at(3), reading.Failed)
		bravo.Title = "Bravo"
		charlie := sampleReading("charlie", "https://example.com/charlie", at(2), reading.Ready)
		charlie.Title = "Charlie"
		seed(ctx, t, s, alpha, bravo, charlie)

		page, err := s.Search(ctx, store.Query{Status: reading.Ready, Sort: store.SortNewest, Limit: 10})
		if err != nil {
			t.Fatalf("Search status: %v", err)
		}
		if got, want := ids(page.Readings), []string{"charlie", "alpha"}; !slices.Equal(got, want) {
			t.Fatalf("Search ready newest ids = %v, want %v", got, want)
		}

		assertOrder(t, s, store.SortNewest, []string{"bravo", "charlie", "alpha"})
		assertOrder(t, s, store.SortOldest, []string{"alpha", "charlie", "bravo"})
		assertOrder(t, s, store.SortTitle, []string{"alpha", "bravo", "charlie"})
	})

	t.Run("KeysetPagination", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		for i := range 25 {
			r := sampleReading(fmt.Sprintf("r%02d", i), fmt.Sprintf("https://example.com/%02d", i), at(i), reading.Ready)
			r.Title = fmt.Sprintf("Reading %02d", i)
			if err := s.SaveReading(ctx, r); err != nil {
				t.Fatalf("SaveReading %s: %v", r.ID, err)
			}
		}

		var all []string
		var cursor store.Cursor
		for {
			page, err := s.Search(ctx, store.Query{Sort: store.SortNewest, Cursor: cursor, Limit: 10})
			if err != nil {
				t.Fatalf("Search page after cursor %+v: %v", cursor, err)
			}
			if page.Total != 25 {
				t.Fatalf("Search total = %d, want 25", page.Total)
			}
			all = append(all, ids(page.Readings)...)
			if !page.NextCursor.Valid {
				break
			}
			cursor = page.NextCursor
		}
		if len(all) != 25 {
			t.Fatalf("walked %d readings, want 25: %v", len(all), all)
		}
		seen := map[string]bool{}
		for _, id := range all {
			if seen[id] {
				t.Fatalf("duplicate id %q while walking pages: %v", id, all)
			}
			seen[id] = true
		}
		if got, wantFirst, wantLast := all[0], "r24", "r00"; got != wantFirst || all[len(all)-1] != wantLast {
			t.Fatalf("page order first/last = %q/%q, want %q/%q", got, all[len(all)-1], wantFirst, wantLast)
		}
		page, err := s.Search(ctx, store.Query{Sort: store.SortNewest, Cursor: cursor, Limit: 10})
		if err != nil {
			t.Fatalf("Search after terminal cursor: %v", err)
		}
		if page.NextCursor.Valid {
			t.Fatalf("Search after terminal cursor advertised next cursor: %+v", page.NextCursor)
		}
	})

	t.Run("SortTitlePagination", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		titles := []string{"Delta", "Alpha", "Charlie", "Bravo", "Echo"}
		for i, title := range titles {
			r := sampleReading(fmt.Sprintf("r%d", i), fmt.Sprintf("https://example.com/title/%d", i), at(i), reading.Ready)
			r.Title = title
			if err := s.SaveReading(ctx, r); err != nil {
				t.Fatalf("SaveReading %s: %v", r.ID, err)
			}
		}

		var all []string
		var cursor store.Cursor
		for {
			page, err := s.Search(ctx, store.Query{Sort: store.SortTitle, Cursor: cursor, Limit: 2})
			if err != nil {
				t.Fatalf("Search title page after cursor %+v: %v", cursor, err)
			}
			all = append(all, ids(page.Readings)...)
			if !page.NextCursor.Valid {
				break
			}
			cursor = page.NextCursor
		}
		if want := []string{"r1", "r3", "r2", "r0", "r4"}; !slices.Equal(all, want) {
			t.Fatalf("title pagination ids = %v, want %v", all, want)
		}
	})

	t.Run("RankedSearchPagination", func(t *testing.T) {
		t.Parallel()

		cases := []struct {
			name string
			sort store.SortMode
			want []string
		}{
			{
				name: "newest",
				sort: store.SortNewest,
				want: []string{"rank-high-new", "rank-high-old", "rank-low-new", "rank-low-old"},
			},
			{
				name: "oldest",
				sort: store.SortOldest,
				want: []string{"rank-high-old", "rank-high-new", "rank-low-old", "rank-low-new"},
			},
			{
				name: "title",
				sort: store.SortTitle,
				want: []string{"rank-high-new", "rank-high-old", "rank-low-new", "rank-low-old"},
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				ctx := context.Background()
				s := newStore(t)
				rankHighOld := withText(sampleReading("rank-high-old", "https://example.com/rank-high-old", at(1), reading.Ready), "Zulu Kubernetes Kubernetes", "", "")
				rankLowNew := withText(sampleReading("rank-low-new", "https://example.com/rank-low-new", at(4), reading.Ready), "Bravo Kubernetes", "", "")
				rankHighNew := withText(sampleReading("rank-high-new", "https://example.com/rank-high-new", at(3), reading.Ready), "Alpha Kubernetes Kubernetes", "", "")
				rankLowOld := withText(sampleReading("rank-low-old", "https://example.com/rank-low-old", at(2), reading.Ready), "Charlie Kubernetes", "", "")
				seed(ctx, t, s, rankHighOld, rankLowNew, rankHighNew, rankLowOld)

				var all []string
				var cursor store.Cursor
				for {
					page, err := s.Search(ctx, store.Query{Q: "kubernetes", Sort: tc.sort, Cursor: cursor, Limit: 1})
					if err != nil {
						t.Fatalf("Search ranked page after cursor %+v: %v", cursor, err)
					}
					all = append(all, ids(page.Readings)...)
					if !page.NextCursor.Valid {
						break
					}
					cursor = page.NextCursor
				}
				if !slices.Equal(all, tc.want) {
					t.Fatalf("ranked pagination ids = %v, want %v", all, tc.want)
				}
			})
		}
	})

	t.Run("UpdateStatusAdvancesTimestamps", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		r := sampleReading("r1", "https://example.com/one", at(1), reading.Pending)
		seed(ctx, t, s, r)

		startedAt := at(2)
		attempts := 2
		if err := s.UpdateStatus(ctx, r.ID, reading.Running, store.StatusFields{
			Now:             startedAt,
			ProcessAttempts: &attempts,
		}); err != nil {
			t.Fatalf("UpdateStatus running: %v", err)
		}
		got, err := s.GetByID(ctx, r.ID)
		if err != nil {
			t.Fatalf("GetByID running: %v", err)
		}
		if got.Status != reading.Running || got.StartedAt == nil || !got.StartedAt.Equal(startedAt) {
			t.Fatalf("running status/start = %q/%v, want %q/%v", got.Status, got.StartedAt, reading.Running, startedAt)
		}
		if !got.UpdatedAt.Equal(startedAt) {
			t.Fatalf("running UpdatedAt = %v, want %v", got.UpdatedAt, startedAt)
		}
		if got.ProcessAttempts != attempts {
			t.Fatalf("ProcessAttempts = %d, want %d", got.ProcessAttempts, attempts)
		}

		finishedAt := at(3)
		if err := s.UpdateStatus(ctx, r.ID, reading.Ready, store.StatusFields{Now: finishedAt}); err != nil {
			t.Fatalf("UpdateStatus ready: %v", err)
		}
		got, err = s.GetByID(ctx, r.ID)
		if err != nil {
			t.Fatalf("GetByID ready: %v", err)
		}
		if got.Status != reading.Ready || got.FinishedAt == nil || !got.FinishedAt.Equal(finishedAt) {
			t.Fatalf("ready status/finish = %q/%v, want %q/%v", got.Status, got.FinishedAt, reading.Ready, finishedAt)
		}
	})

	t.Run("UpdateStatusErrorSemantics", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		r := sampleReading("r1", "https://example.com/one", at(1), reading.Pending)
		seed(ctx, t, s, r)

		if err := s.UpdateStatus(ctx, r.ID, reading.Failed, store.StatusFields{Now: at(2), Error: stringPtr("temporary failure")}); err != nil {
			t.Fatalf("UpdateStatus failed with error: %v", err)
		}
		if err := s.UpdateStatus(ctx, r.ID, reading.Pending, store.StatusFields{Now: at(3)}); err != nil {
			t.Fatalf("UpdateStatus pending preserving error: %v", err)
		}
		got, err := s.GetByID(ctx, r.ID)
		if err != nil {
			t.Fatalf("GetByID after preserve: %v", err)
		}
		if got.Error != "temporary failure" {
			t.Fatalf("preserved Error = %q, want temporary failure", got.Error)
		}
		if err := s.UpdateStatus(ctx, r.ID, reading.Running, store.StatusFields{Now: at(4), ClearError: true}); err != nil {
			t.Fatalf("UpdateStatus running clearing error: %v", err)
		}
		got, err = s.GetByID(ctx, r.ID)
		if err != nil {
			t.Fatalf("GetByID after clear: %v", err)
		}
		if got.Error != "" {
			t.Fatalf("cleared Error = %q, want empty", got.Error)
		}
	})

	t.Run("UpdateStatusClearsTimestampsWhenRequested", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		startedAt := at(2)
		finishedAt := at(3)
		r := sampleReading("r1", "https://example.com/one", at(1), reading.Ready)
		r.StartedAt = &startedAt
		r.FinishedAt = &finishedAt
		seed(ctx, t, s, r)

		if err := s.UpdateStatus(ctx, r.ID, reading.Pending, store.StatusFields{
			Now:             at(4),
			ClearStartedAt:  true,
			ClearFinishedAt: true,
		}); err != nil {
			t.Fatalf("UpdateStatus clear timestamps: %v", err)
		}
		got, err := s.GetByID(ctx, r.ID)
		if err != nil {
			t.Fatalf("GetByID after clear: %v", err)
		}
		if got.StartedAt != nil || got.FinishedAt != nil {
			t.Fatalf("timestamps = %v/%v, want both nil", got.StartedAt, got.FinishedAt)
		}
	})

	t.Run("SaveReadingPersistsJSON", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		r := sampleReading("r1", "https://example.com/one", at(1), reading.Ready)
		r.SummaryJSON = []byte(`{"tags":["go"]}`)
		r.SimilarJSON = []byte(`[{"id":"r2","score":0.5}]`)
		r.DiagnosticsJSON = []byte(`{"source":"web"}`)

		if err := s.SaveReading(ctx, r); err != nil {
			t.Fatalf("SaveReading: %v", err)
		}
		got, err := s.GetByID(ctx, r.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}

		// Both backends must round-trip the JSON columns set at save time, so the
		// fake and Postgres satisfy the same data contract.
		assertJSONEqual(t, "SummaryJSON", r.SummaryJSON, got.SummaryJSON)
		assertJSONEqual(t, "SimilarJSON", r.SimilarJSON, got.SimilarJSON)
		assertJSONEqual(t, "DiagnosticsJSON", r.DiagnosticsJSON, got.DiagnosticsJSON)
	})

	t.Run("UpdateContent", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		r := sampleReading("r1", "https://example.com/one", at(1), reading.Running)
		startedAt := at(2)
		r.StartedAt = &startedAt
		r.ProcessAttempts = 1
		r.Error = "transient"
		r.Tags = []string{"keep"}
		seed(ctx, t, s, r)

		updatedAt := at(5)
		fields := store.ContentFields{
			Now:             updatedAt,
			Title:           "Extracted title",
			Author:          "Ada",
			Site:            "example.com",
			Lang:            "en",
			WordCount:       1234,
			ExtractionMode:  "readability",
			ContentKey:      "readings/r1/content",
			RawKey:          "readings/r1/raw",
			Summary:         "A concise summary.",
			SummaryJSON:     []byte(`{"tags":["go"]}`),
			SimilarJSON:     []byte(`[{"id":"r2","score":0.9}]`),
			DiagnosticsJSON: []byte(`{"source":"web"}`),
		}
		if err := s.UpdateContent(ctx, r.ID, fields); err != nil {
			t.Fatalf("UpdateContent: %v", err)
		}

		got, err := s.GetByID(ctx, r.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}

		if got.Title != fields.Title || got.Author != fields.Author || got.Site != fields.Site || got.Lang != fields.Lang {
			t.Fatalf("content text fields = %q/%q/%q/%q, want %q/%q/%q/%q",
				got.Title, got.Author, got.Site, got.Lang, fields.Title, fields.Author, fields.Site, fields.Lang)
		}
		if got.WordCount != fields.WordCount || got.ExtractionMode != fields.ExtractionMode {
			t.Fatalf("word_count/extraction_mode = %d/%q, want %d/%q", got.WordCount, got.ExtractionMode, fields.WordCount, fields.ExtractionMode)
		}
		if got.ContentKey != fields.ContentKey || got.RawKey != fields.RawKey || got.Summary != fields.Summary {
			t.Fatalf("keys/summary = %q/%q/%q, want %q/%q/%q", got.ContentKey, got.RawKey, got.Summary, fields.ContentKey, fields.RawKey, fields.Summary)
		}
		// Compare JSON semantically: a jsonb-backed adapter canonicalizes whitespace
		// and key order on read, so byte equality is not a valid cross-backend check.
		assertJSONEqual(t, "SummaryJSON", fields.SummaryJSON, got.SummaryJSON)
		assertJSONEqual(t, "SimilarJSON", fields.SimilarJSON, got.SimilarJSON)
		assertJSONEqual(t, "DiagnosticsJSON", fields.DiagnosticsJSON, got.DiagnosticsJSON)
		if !got.UpdatedAt.Equal(updatedAt) {
			t.Fatalf("UpdatedAt = %v, want %v", got.UpdatedAt, updatedAt)
		}

		// UpdateContent must not disturb lifecycle status, timestamps, error,
		// attempt count, or tags — those belong to UpdateStatus/ReplaceTags.
		if got.Status != reading.Running {
			t.Fatalf("Status = %q, want unchanged running", got.Status)
		}
		if got.StartedAt == nil || !got.StartedAt.Equal(startedAt) {
			t.Fatalf("StartedAt = %v, want unchanged %v", got.StartedAt, startedAt)
		}
		if got.ProcessAttempts != 1 {
			t.Fatalf("ProcessAttempts = %d, want unchanged 1", got.ProcessAttempts)
		}
		if got.Error != "transient" {
			t.Fatalf("Error = %q, want unchanged 'transient'", got.Error)
		}
		if !slices.Equal(got.Tags, []string{"keep"}) {
			t.Fatalf("Tags = %v, want unchanged [keep]", got.Tags)
		}

		// Mutating the caller's JSON slices after the write must not corrupt the
		// stored value (the store must copy in, like SaveReading/the read path).
		fields.SummaryJSON[0] = 'X'
		fields.SimilarJSON[0] = 'X'
		fields.DiagnosticsJSON[0] = 'X'
		afterMutate, err := s.GetByID(ctx, r.ID)
		if err != nil {
			t.Fatalf("GetByID after caller mutation: %v", err)
		}
		assertJSONEqual(t, "SummaryJSON after caller mutation", []byte(`{"tags":["go"]}`), afterMutate.SummaryJSON)

		// Use a fresh, valid value here: the slices above were mutated to invalid
		// JSON to prove the defensive copy, and a jsonb-backed adapter binds the
		// parameters before it can report the missing row.
		if err := s.UpdateContent(ctx, "missing", store.ContentFields{Now: updatedAt}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("UpdateContent missing error = %v, want ErrNotFound", err)
		}
	})

	t.Run("UpdateImport", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		r := sampleReading("r1", "https://example.com/one", at(1), reading.Failed)
		r.SourceKind = reading.SourceReddit
		r.Title = "Old title"
		r.Author = "Old author"
		r.Site = "old.example"
		r.Lang = "en"
		r.WordCount = 100
		r.ExtractionMode = "readability"
		r.ContentKey = "readings/r1/content"
		r.RawKey = "readings/r1/raw"
		r.Summary = "Old summary"
		r.SummaryJSON = []byte(`{"old":true}`)
		r.SimilarJSON = []byte(`[{"id":"old"}]`)
		r.DiagnosticsJSON = []byte(`{"source":"reddit"}`)
		r.Tags = []string{"old"}
		seed(ctx, t, s, r)

		if err := s.UpdateImport(ctx, r.ID, store.ImportFields{
			Now:        at(4),
			SourceKind: reading.SourceMarkdown,
			Title:      "Imported title",
			RawKey:     "readings/r1/import.md",
			Tags:       []string{"imported"},
		}); err != nil {
			t.Fatalf("UpdateImport: %v", err)
		}
		got, err := s.GetByID(ctx, r.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}

		if got.Status != reading.Pending || got.Error != "" || got.ProcessAttempts != 0 || got.StartedAt != nil || got.FinishedAt != nil {
			t.Fatalf("lifecycle = status %q error %q attempts %d started %v finished %v, want reset pending",
				got.Status, got.Error, got.ProcessAttempts, got.StartedAt, got.FinishedAt)
		}
		if got.SourceKind != reading.SourceMarkdown || got.Title != "Imported title" || got.RawKey != "readings/r1/import.md" {
			t.Fatalf("import fields = source %q title %q raw %q, want markdown/imported title/import raw",
				got.SourceKind, got.Title, got.RawKey)
		}
		if got.Author != "" || got.Site != "" || got.Lang != "" || got.WordCount != 0 ||
			got.ExtractionMode != "" || got.ContentKey != "" || got.Summary != "" ||
			len(got.SummaryJSON) != 0 || len(got.SimilarJSON) != 0 || len(got.DiagnosticsJSON) != 0 {
			t.Fatalf("derived fields not cleared: %+v", got)
		}
		if !slices.Equal(got.Tags, []string{"imported"}) {
			t.Fatalf("Tags = %v, want [imported]", got.Tags)
		}
		if !got.UpdatedAt.Equal(at(4)) {
			t.Fatalf("UpdatedAt = %v, want %v", got.UpdatedAt, at(4))
		}
		if err := s.UpdateImport(ctx, "missing", store.ImportFields{Now: at(5)}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("UpdateImport missing error = %v, want ErrNotFound", err)
		}
	})

	t.Run("Reprocess", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		startedAt := at(2)
		finishedAt := at(3)
		r := sampleReading("r1", "https://example.com/one", at(1), reading.Ready)
		r.Title = "Old title"
		r.Author = "Old author"
		r.Site = "old.example"
		r.Lang = "en"
		r.WordCount = 100
		r.ExtractionMode = "readability"
		r.ContentKey = "readings/r1/content"
		r.RawKey = "readings/r1/raw"
		r.Summary = "Old summary"
		r.SummaryJSON = []byte(`{"old":true}`)
		r.SimilarJSON = []byte(`[{"id":"old"}]`)
		r.DiagnosticsJSON = []byte(`{"source":"web"}`)
		r.Error = "old error"
		r.ProcessAttempts = 3
		r.StartedAt = &startedAt
		r.FinishedAt = &finishedAt
		seed(ctx, t, s, r)

		if err := s.Reprocess(ctx, r.ID, store.ReprocessFields{
			Now:    at(4),
			RawKey: "readings/r1/raw",
			Title:  "Imported title",
		}); err != nil {
			t.Fatalf("Reprocess: %v", err)
		}
		got, err := s.GetByID(ctx, r.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if got.Status != reading.Pending || got.Error != "" || got.ProcessAttempts != 0 || got.StartedAt != nil || got.FinishedAt != nil {
			t.Fatalf("lifecycle = status %q error %q attempts %d started %v finished %v, want reset pending",
				got.Status, got.Error, got.ProcessAttempts, got.StartedAt, got.FinishedAt)
		}
		if got.RawKey != "readings/r1/raw" {
			t.Fatalf("RawKey = %q, want preserved raw", got.RawKey)
		}
		if got.Title != "Imported title" {
			t.Fatalf("Title = %q, want preserved import title", got.Title)
		}
		if got.Author != "" || got.Site != "" || got.Lang != "" || got.WordCount != 0 ||
			got.ExtractionMode != "" || got.ContentKey != "" || got.Summary != "" ||
			len(got.SummaryJSON) != 0 || len(got.SimilarJSON) != 0 || len(got.DiagnosticsJSON) != 0 {
			t.Fatalf("derived fields not cleared: %+v", got)
		}
		if !got.UpdatedAt.Equal(at(4)) {
			t.Fatalf("UpdatedAt = %v, want %v", got.UpdatedAt, at(4))
		}
		if err := s.Reprocess(ctx, "missing", store.ReprocessFields{Now: at(5)}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Reprocess missing error = %v, want ErrNotFound", err)
		}
	})

	t.Run("ReplaceTags", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		r := sampleReading("r1", "https://example.com/one", at(1), reading.Ready)
		r.Title = "Tag replacement"
		r.Tags = []string{"old"}
		seed(ctx, t, s, r)

		if err := s.ReplaceTags(ctx, r.ID, []string{"go", "db"}); err != nil {
			t.Fatalf("ReplaceTags first: %v", err)
		}
		if err := s.ReplaceTags(ctx, r.ID, []string{"go", "db"}); err != nil {
			t.Fatalf("ReplaceTags idempotent: %v", err)
		}
		got, err := s.GetByID(ctx, r.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if !slices.Equal(got.Tags, []string{"go", "db"}) {
			t.Fatalf("Tags = %v, want [go db]", got.Tags)
		}
		page, err := s.Search(ctx, store.Query{Q: "db", Tags: []string{"go"}, Limit: 10})
		if err != nil {
			t.Fatalf("Search replaced tags: %v", err)
		}
		if got := ids(page.Readings); !slices.Equal(got, []string{r.ID}) {
			t.Fatalf("Search replaced tags ids = %v, want [%s]", got, r.ID)
		}
	})

	t.Run("ListNonTerminal", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		pending := sampleReading("pending", "https://example.com/pending", at(1), reading.Pending)
		pending.ProcessAttempts = 1
		runningFresh := sampleReading("running-fresh", "https://example.com/running-fresh", at(2), reading.Running)
		runningFresh.StartedAt = ptr(at(9))
		runningStale := sampleReading("running-stale", "https://example.com/running-stale", at(3), reading.Running)
		runningStale.StartedAt = ptr(at(4))
		ready := sampleReading("ready", "https://example.com/ready", at(4), reading.Ready)
		failed := sampleReading("failed", "https://example.com/failed", at(5), reading.Failed)
		seed(ctx, t, s, pending, runningFresh, runningStale, ready, failed)

		got, err := s.ListNonTerminal(ctx, at(5))
		if err != nil {
			t.Fatalf("ListNonTerminal: %v", err)
		}
		want := []store.Pending{
			{ID: "pending", ProcessAttempts: 1},
			{ID: "running-stale", ProcessAttempts: 0},
		}
		slices.SortFunc(got, func(a, b store.Pending) int { return strings.Compare(a.ID, b.ID) })
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("ListNonTerminal mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		r := sampleReading("r1", "https://example.com/one", at(1), reading.Ready)
		seed(ctx, t, s, r)

		if err := s.Delete(ctx, r.ID); err != nil {
			t.Fatalf("Delete existing: %v", err)
		}
		if _, err := s.GetByID(ctx, r.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetByID deleted error = %v, want ErrNotFound", err)
		}
		if err := s.Delete(ctx, r.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Delete missing error = %v, want ErrNotFound", err)
		}
	})

	t.Run("ConcurrentSaves", func(t *testing.T) {
		ctx := context.Background()
		s := newStore(t)
		const n = 64

		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				r := sampleReading(fmt.Sprintf("r%02d", i), fmt.Sprintf("https://example.com/%02d", i), at(i), reading.Pending)
				if err := s.SaveReading(ctx, r); err != nil {
					t.Errorf("SaveReading %s: %v", r.ID, err)
				}
			}(i)
		}
		wg.Wait()

		page, err := s.Search(ctx, store.Query{Limit: n, Sort: store.SortNewest})
		if err != nil {
			t.Fatalf("Search after concurrent saves: %v", err)
		}
		if len(page.Readings) != n || page.Total != n {
			t.Fatalf("Search returned len/total %d/%d, want %d/%d", len(page.Readings), page.Total, n, n)
		}
	})

	t.Run("DefensiveCopies", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		startedAt := at(2)
		r := sampleReading("r1", "https://example.com/one", at(1), reading.Running)
		r.StartedAt = &startedAt
		r.Tags = []string{"go"}
		seed(ctx, t, s, r)

		got, err := s.GetByID(ctx, r.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		got.Tags[0] = "mutated"
		got.StartedAt = ptr(at(99))

		again, err := s.GetByID(ctx, r.ID)
		if err != nil {
			t.Fatalf("GetByID again: %v", err)
		}
		if !slices.Equal(again.Tags, []string{"go"}) {
			t.Fatalf("stored Tags mutated through returned slice: %v", again.Tags)
		}
		if again.StartedAt == nil || !again.StartedAt.Equal(startedAt) {
			t.Fatalf("stored StartedAt mutated through returned pointer: %v, want %v", again.StartedAt, startedAt)
		}
	})
}

// assertJSONEqual compares two JSON payloads by decoded value, tolerating the
// whitespace/key-order canonicalization a jsonb-backed adapter applies on read.
func assertJSONEqual(t *testing.T, label string, want, got json.RawMessage) {
	t.Helper()

	var wantVal, gotVal any
	if err := json.Unmarshal(want, &wantVal); err != nil {
		t.Fatalf("%s: unmarshal want %q: %v", label, want, err)
	}
	if err := json.Unmarshal(got, &gotVal); err != nil {
		t.Fatalf("%s: unmarshal got %q: %v", label, got, err)
	}
	if diff := cmp.Diff(wantVal, gotVal); diff != "" {
		t.Fatalf("%s mismatch (-want +got):\n%s", label, diff)
	}
}

func assertOrder(t *testing.T, s store.Store, sort store.SortMode, want []string) {
	t.Helper()

	page, err := s.Search(context.Background(), store.Query{Sort: sort, Limit: 10})
	if err != nil {
		t.Fatalf("Search sort %q: %v", sort, err)
	}
	if got := ids(page.Readings); !slices.Equal(got, want) {
		t.Fatalf("Search sort %q ids = %v, want %v", sort, got, want)
	}
}

func seed(ctx context.Context, t *testing.T, s store.Store, readings ...reading.Reading) {
	t.Helper()

	for _, r := range readings {
		if err := s.SaveReading(ctx, r); err != nil {
			t.Fatalf("SaveReading %s: %v", r.ID, err)
		}
	}
}

func sampleReading(id, rawURL string, createdAt time.Time, status reading.Status) reading.Reading {
	key, err := reading.URLKey(rawURL)
	if err != nil {
		panic(err)
	}
	return reading.Reading{
		ID:         id,
		URL:        rawURL,
		URLKey:     key,
		Status:     status,
		SourceKind: reading.ClassifySource(key),
		CreatedAt:  createdAt,
		UpdatedAt:  createdAt,
	}
}

func withText(r reading.Reading, title, author, summary string) reading.Reading {
	r.Title = title
	r.Author = author
	r.Summary = summary
	return r
}

func ids(readings []reading.Reading) []string {
	out := make([]string, len(readings))
	for i, r := range readings {
		out[i] = r.ID
	}
	return out
}

func ptr(t time.Time) *time.Time {
	return &t
}

func stringPtr(s string) *string {
	return &s
}

func at(seconds int) time.Time {
	return time.Unix(int64(seconds), 0).UTC()
}
