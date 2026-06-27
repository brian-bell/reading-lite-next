package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/bbell/reading-lite/internal/store"
)

// BatchFactory returns a fresh, empty BatchStore for one contract test.
type BatchFactory func(t *testing.T) store.BatchStore

// RunBatchContract runs the BatchStore conformance suite.
func RunBatchContract(t *testing.T, newStore BatchFactory) {
	t.Helper()

	t.Run("CreatePlannedBatchRoundTrip", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		now := at(10)
		items := []store.BatchItemCreateFields{
			{
				ReadingID:   "reading-1",
				CustomID:    "batch-1:reading-1",
				RequestJSON: json.RawMessage(`{"custom_id":"batch-1:reading-1"}`),
			},
			{
				ReadingID:   "reading-2",
				CustomID:    "batch-1:reading-2",
				RequestJSON: json.RawMessage(`{"custom_id":"batch-1:reading-2"}`),
			},
		}

		if err := s.CreatePlannedBatch(ctx, store.BatchCreateFields{
			ID:    "batch-1",
			Now:   now,
			Items: items,
		}); err != nil {
			t.Fatalf("CreatePlannedBatch: %v", err)
		}

		wantBatch := store.ManualBatch{
			ID:        "batch-1",
			State:     store.BatchStatePlanned,
			CreatedAt: now,
			UpdatedAt: now,
		}
		gotBatch, err := s.GetBatch(ctx, "batch-1")
		if err != nil {
			t.Fatalf("GetBatch: %v", err)
		}
		if diff := cmp.Diff(wantBatch, gotBatch); diff != "" {
			t.Fatalf("GetBatch mismatch (-want +got):\n%s", diff)
		}

		gotBatches, err := s.ListBatches(ctx, store.BatchQuery{})
		if err != nil {
			t.Fatalf("ListBatches: %v", err)
		}
		if diff := cmp.Diff([]store.ManualBatch{wantBatch}, gotBatches); diff != "" {
			t.Fatalf("ListBatches mismatch (-want +got):\n%s", diff)
		}

		wantItems := []store.ManualBatchItem{
			{
				BatchID:     "batch-1",
				ReadingID:   "reading-1",
				CustomID:    "batch-1:reading-1",
				State:       store.BatchItemStatePlanned,
				RequestJSON: json.RawMessage(`{"custom_id":"batch-1:reading-1"}`),
				CreatedAt:   now,
				UpdatedAt:   now,
			},
			{
				BatchID:     "batch-1",
				ReadingID:   "reading-2",
				CustomID:    "batch-1:reading-2",
				State:       store.BatchItemStatePlanned,
				RequestJSON: json.RawMessage(`{"custom_id":"batch-1:reading-2"}`),
				CreatedAt:   now,
				UpdatedAt:   now,
			},
		}
		gotItems, err := s.ListBatchItems(ctx, "batch-1")
		if err != nil {
			t.Fatalf("ListBatchItems: %v", err)
		}
		assertBatchItemsEqual(t, "ListBatchItems", wantItems, gotItems)

		gotItem, err := s.GetBatchItemByCustomID(ctx, "batch-1:reading-2")
		if err != nil {
			t.Fatalf("GetBatchItemByCustomID: %v", err)
		}
		assertBatchItemsEqual(t, "GetBatchItemByCustomID", []store.ManualBatchItem{wantItems[1]}, []store.ManualBatchItem{gotItem})

		gotItems[0].RequestJSON[0] = 'X'
		gotItem.RequestJSON[0] = 'X'
		again, err := s.GetBatchItemByCustomID(ctx, "batch-1:reading-1")
		if err != nil {
			t.Fatalf("GetBatchItemByCustomID after caller mutation: %v", err)
		}
		assertJSONEqual(t, "RequestJSON after caller mutation", wantItems[0].RequestJSON, again.RequestJSON)
	})

	t.Run("ActiveItemUniqueness", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		if err := s.CreatePlannedBatch(ctx, store.BatchCreateFields{
			ID:  "batch-1",
			Now: at(10),
			Items: []store.BatchItemCreateFields{
				{
					ReadingID:   "reading-1",
					CustomID:    "batch-1:reading-1",
					RequestJSON: json.RawMessage(`{"custom_id":"batch-1:reading-1"}`),
				},
			},
		}); err != nil {
			t.Fatalf("CreatePlannedBatch first: %v", err)
		}

		err := s.CreatePlannedBatch(ctx, store.BatchCreateFields{
			ID:  "batch-2",
			Now: at(11),
			Items: []store.BatchItemCreateFields{
				{
					ReadingID:   "reading-1",
					CustomID:    "batch-2:reading-1",
					RequestJSON: json.RawMessage(`{"custom_id":"batch-2:reading-1"}`),
				},
			},
		})
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("CreatePlannedBatch with active duplicate reading error = %v, want ErrConflict", err)
		}

		if err := s.SubmitBatch(ctx, "batch-1", store.BatchSubmitFields{
			Now:      at(12),
			RemoteID: "msgbatch_active",
		}); err != nil {
			t.Fatalf("SubmitBatch before terminal item: %v", err)
		}
		if err := s.WriteBatchItemResult(ctx, "batch-1:reading-1", store.BatchItemResultFields{
			Now:          at(12),
			State:        store.BatchItemStateErrored,
			ResultJSON:   json.RawMessage(`{"result":{"type":"errored"}}`),
			ErrorType:    "overloaded_error",
			ErrorMessage: "try later",
		}); err != nil {
			t.Fatalf("WriteBatchItemResult terminal error: %v", err)
		}

		if err := s.CreatePlannedBatch(ctx, store.BatchCreateFields{
			ID:  "batch-2",
			Now: at(13),
			Items: []store.BatchItemCreateFields{
				{
					ReadingID:   "reading-1",
					CustomID:    "batch-2:reading-1",
					RequestJSON: json.RawMessage(`{"custom_id":"batch-2:reading-1"}`),
				},
			},
		}); err != nil {
			t.Fatalf("CreatePlannedBatch after terminal item: %v", err)
		}

		err = s.CreatePlannedBatch(ctx, store.BatchCreateFields{
			ID:  "batch-3",
			Now: at(14),
			Items: []store.BatchItemCreateFields{
				{
					ReadingID:   "reading-2",
					CustomID:    "batch-3:reading-2-a",
					RequestJSON: json.RawMessage(`{"custom_id":"batch-3:reading-2-a"}`),
				},
				{
					ReadingID:   "reading-2",
					CustomID:    "batch-3:reading-2-b",
					RequestJSON: json.RawMessage(`{"custom_id":"batch-3:reading-2-b"}`),
				},
			},
		})
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("CreatePlannedBatch with duplicate reading in request error = %v, want ErrConflict", err)
		}
	})

	t.Run("ListBatchItemsPreservesCreateOrder", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		if err := s.CreatePlannedBatch(ctx, store.BatchCreateFields{
			ID:  "batch-1",
			Now: at(10),
			Items: []store.BatchItemCreateFields{
				{
					ReadingID:   "reading-1",
					CustomID:    "z-custom",
					RequestJSON: json.RawMessage(`{"custom_id":"z-custom"}`),
				},
				{
					ReadingID:   "reading-2",
					CustomID:    "a-custom",
					RequestJSON: json.RawMessage(`{"custom_id":"a-custom"}`),
				},
			},
		}); err != nil {
			t.Fatalf("CreatePlannedBatch: %v", err)
		}
		got, err := s.ListBatchItems(ctx, "batch-1")
		if err != nil {
			t.Fatalf("ListBatchItems: %v", err)
		}
		if len(got) != 2 || got[0].CustomID != "z-custom" || got[1].CustomID != "a-custom" {
			t.Fatalf("ListBatchItems order = %+v, want create order [z-custom a-custom]", got)
		}
	})

	t.Run("SubmitBatchTransitionsItems", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		createdAt := at(10)
		submittedAt := at(20)
		if err := s.CreatePlannedBatch(ctx, store.BatchCreateFields{
			ID:  "batch-1",
			Now: createdAt,
			Items: []store.BatchItemCreateFields{
				{
					ReadingID:   "reading-1",
					CustomID:    "batch-1:reading-1",
					RequestJSON: json.RawMessage(`{"custom_id":"batch-1:reading-1"}`),
				},
				{
					ReadingID:   "reading-2",
					CustomID:    "batch-1:reading-2",
					RequestJSON: json.RawMessage(`{"custom_id":"batch-1:reading-2"}`),
				},
			},
		}); err != nil {
			t.Fatalf("CreatePlannedBatch: %v", err)
		}

		counts := store.BatchCounts{Processing: 2}
		if err := s.SubmitBatch(ctx, "batch-1", store.BatchSubmitFields{
			Now:        submittedAt,
			RemoteID:   "msgbatch_123",
			ResultsURL: "https://api.anthropic.com/v1/messages/batches/msgbatch_123/results",
			Counts:     counts,
		}); err != nil {
			t.Fatalf("SubmitBatch: %v", err)
		}

		wantBatch := store.ManualBatch{
			ID:          "batch-1",
			State:       store.BatchStateSubmitted,
			RemoteID:    "msgbatch_123",
			ResultsURL:  "https://api.anthropic.com/v1/messages/batches/msgbatch_123/results",
			Counts:      counts,
			CreatedAt:   createdAt,
			SubmittedAt: ptr(submittedAt),
			UpdatedAt:   submittedAt,
		}
		gotBatch, err := s.GetBatch(ctx, "batch-1")
		if err != nil {
			t.Fatalf("GetBatch: %v", err)
		}
		if diff := cmp.Diff(wantBatch, gotBatch); diff != "" {
			t.Fatalf("GetBatch after submit mismatch (-want +got):\n%s", diff)
		}

		gotItems, err := s.ListBatchItems(ctx, "batch-1")
		if err != nil {
			t.Fatalf("ListBatchItems: %v", err)
		}
		for _, item := range gotItems {
			if item.State != store.BatchItemStateSubmitted || item.SubmittedAt == nil || !item.SubmittedAt.Equal(submittedAt) {
				t.Fatalf("submitted item %s state/submitted_at = %q/%v, want submitted/%v",
					item.CustomID, item.State, item.SubmittedAt, submittedAt)
			}
		}

		if err := s.CreatePlannedBatch(ctx, store.BatchCreateFields{
			ID:  "batch-2",
			Now: at(21),
			Items: []store.BatchItemCreateFields{
				{
					ReadingID:   "reading-3",
					CustomID:    "batch-2:reading-3",
					RequestJSON: json.RawMessage(`{"custom_id":"batch-2:reading-3"}`),
				},
			},
		}); err != nil {
			t.Fatalf("CreatePlannedBatch second: %v", err)
		}
		err = s.SubmitBatch(ctx, "batch-2", store.BatchSubmitFields{
			Now:      at(22),
			RemoteID: "msgbatch_123",
			Counts:   store.BatchCounts{Processing: 1},
		})
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("SubmitBatch duplicate remote id error = %v, want ErrConflict", err)
		}
	})

	t.Run("BatchStateTransitionsAndActiveListing", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		createdAt := at(10)
		submittedAt := at(20)
		finishedAt := at(30)
		appliedAt := at(40)
		if err := s.CreatePlannedBatch(ctx, store.BatchCreateFields{
			ID:  "batch-1",
			Now: createdAt,
			Items: []store.BatchItemCreateFields{
				{
					ReadingID:   "reading-1",
					CustomID:    "batch-1:reading-1",
					RequestJSON: json.RawMessage(`{"custom_id":"batch-1:reading-1"}`),
				},
			},
		}); err != nil {
			t.Fatalf("CreatePlannedBatch: %v", err)
		}
		if err := s.SubmitBatch(ctx, "batch-1", store.BatchSubmitFields{
			Now:      submittedAt,
			RemoteID: "msgbatch_123",
			Counts:   store.BatchCounts{Processing: 1},
		}); err != nil {
			t.Fatalf("SubmitBatch: %v", err)
		}

		readyCounts := store.BatchCounts{Succeeded: 1}
		if err := s.UpdateBatchState(ctx, "batch-1", store.BatchStateResultsReady, store.BatchStateFields{
			Now:    finishedAt,
			Counts: &readyCounts,
		}); err != nil {
			t.Fatalf("UpdateBatchState results_ready: %v", err)
		}
		gotBatch, err := s.GetBatch(ctx, "batch-1")
		if err != nil {
			t.Fatalf("GetBatch results_ready: %v", err)
		}
		if gotBatch.State != store.BatchStateResultsReady || gotBatch.FinishedAt == nil || !gotBatch.FinishedAt.Equal(finishedAt) {
			t.Fatalf("results-ready state/finished_at = %q/%v, want %q/%v",
				gotBatch.State, gotBatch.FinishedAt, store.BatchStateResultsReady, finishedAt)
		}
		if gotBatch.Counts != readyCounts {
			t.Fatalf("results-ready counts = %+v, want %+v", gotBatch.Counts, readyCounts)
		}

		active, err := s.ListBatches(ctx, store.BatchQuery{ActiveOnly: true})
		if err != nil {
			t.Fatalf("ListBatches active: %v", err)
		}
		if len(active) != 1 || active[0].ID != "batch-1" {
			t.Fatalf("active batches = %+v, want batch-1", active)
		}

		if err := s.WriteBatchItemResult(ctx, "batch-1:reading-1", store.BatchItemResultFields{
			Now:        at(35),
			State:      store.BatchItemStateSucceeded,
			ResultJSON: json.RawMessage(`{"result":{"type":"succeeded"}}`),
		}); err != nil {
			t.Fatalf("WriteBatchItemResult before applied batch: %v", err)
		}
		if err := s.MarkBatchItemApplied(ctx, "batch-1:reading-1", store.BatchItemApplyFields{Now: at(36)}); err != nil {
			t.Fatalf("MarkBatchItemApplied before applied batch: %v", err)
		}
		if err := s.UpdateBatchState(ctx, "batch-1", store.BatchStateApplied, store.BatchStateFields{Now: appliedAt}); err != nil {
			t.Fatalf("UpdateBatchState applied: %v", err)
		}
		gotBatch, err = s.GetBatch(ctx, "batch-1")
		if err != nil {
			t.Fatalf("GetBatch applied: %v", err)
		}
		if gotBatch.State != store.BatchStateApplied || gotBatch.AppliedAt == nil || !gotBatch.AppliedAt.Equal(appliedAt) {
			t.Fatalf("applied state/applied_at = %q/%v, want %q/%v",
				gotBatch.State, gotBatch.AppliedAt, store.BatchStateApplied, appliedAt)
		}

		active, err = s.ListBatches(ctx, store.BatchQuery{ActiveOnly: true})
		if err != nil {
			t.Fatalf("ListBatches active after applied: %v", err)
		}
		if len(active) != 0 {
			t.Fatalf("active batches after applied = %+v, want none", active)
		}
	})

	t.Run("UpdateBatchStateCannotBypassSubmit", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		if err := s.CreatePlannedBatch(ctx, store.BatchCreateFields{
			ID:  "batch-1",
			Now: at(10),
			Items: []store.BatchItemCreateFields{
				{
					ReadingID:   "reading-1",
					CustomID:    "batch-1:reading-1",
					RequestJSON: json.RawMessage(`{"custom_id":"batch-1:reading-1"}`),
				},
			},
		}); err != nil {
			t.Fatalf("CreatePlannedBatch: %v", err)
		}

		err := s.UpdateBatchState(ctx, "batch-1", store.BatchStateSubmitted, store.BatchStateFields{Now: at(20)})
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("UpdateBatchState submitted error = %v, want ErrConflict", err)
		}
		if err := s.SubmitBatch(ctx, "batch-1", store.BatchSubmitFields{
			Now:      at(30),
			RemoteID: "msgbatch_123",
		}); err != nil {
			t.Fatalf("SubmitBatch after rejected state transition: %v", err)
		}
	})

	t.Run("TerminalBatchStateReleasesItems", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		if err := s.CreatePlannedBatch(ctx, store.BatchCreateFields{
			ID:  "batch-1",
			Now: at(10),
			Items: []store.BatchItemCreateFields{
				{
					ReadingID:   "reading-1",
					CustomID:    "batch-1:reading-1",
					RequestJSON: json.RawMessage(`{"custom_id":"batch-1:reading-1"}`),
				},
				{
					ReadingID:   "reading-2",
					CustomID:    "batch-1:reading-2",
					RequestJSON: json.RawMessage(`{"custom_id":"batch-1:reading-2"}`),
				},
			},
		}); err != nil {
			t.Fatalf("CreatePlannedBatch: %v", err)
		}
		if err := s.UpdateBatchState(ctx, "batch-1", store.BatchStateCanceled, store.BatchStateFields{Now: at(20)}); err != nil {
			t.Fatalf("UpdateBatchState canceled: %v", err)
		}

		items, err := s.ListBatchItems(ctx, "batch-1")
		if err != nil {
			t.Fatalf("ListBatchItems canceled: %v", err)
		}
		for _, item := range items {
			if item.State != store.BatchItemStateCanceled || item.FinishedAt == nil || !item.FinishedAt.Equal(at(20)) {
				t.Fatalf("canceled item %s state/finished_at = %q/%v, want canceled/%v",
					item.CustomID, item.State, item.FinishedAt, at(20))
			}
		}

		if err := s.CreatePlannedBatch(ctx, store.BatchCreateFields{
			ID:  "batch-2",
			Now: at(30),
			Items: []store.BatchItemCreateFields{
				{
					ReadingID:   "reading-1",
					CustomID:    "batch-2:reading-1",
					RequestJSON: json.RawMessage(`{"custom_id":"batch-2:reading-1"}`),
				},
			},
		}); err != nil {
			t.Fatalf("CreatePlannedBatch after cancel: %v", err)
		}
	})

	t.Run("BatchItemResultsAreIdempotentUntilApplied", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		if err := s.CreatePlannedBatch(ctx, store.BatchCreateFields{
			ID:  "batch-1",
			Now: at(10),
			Items: []store.BatchItemCreateFields{
				{
					ReadingID:   "reading-1",
					CustomID:    "batch-1:reading-1",
					RequestJSON: json.RawMessage(`{"custom_id":"batch-1:reading-1"}`),
				},
			},
		}); err != nil {
			t.Fatalf("CreatePlannedBatch: %v", err)
		}
		err := s.WriteBatchItemResult(ctx, "batch-1:reading-1", store.BatchItemResultFields{
			Now:        at(15),
			State:      store.BatchItemStateSucceeded,
			ResultJSON: json.RawMessage(`{"result":{"type":"succeeded"}}`),
		})
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("WriteBatchItemResult before submit error = %v, want ErrConflict", err)
		}
		if err := s.SubmitBatch(ctx, "batch-1", store.BatchSubmitFields{
			Now:      at(20),
			RemoteID: "msgbatch_123",
		}); err != nil {
			t.Fatalf("SubmitBatch: %v", err)
		}

		result := store.BatchItemResultFields{
			Now:        at(30),
			State:      store.BatchItemStateSucceeded,
			ResultJSON: json.RawMessage(`{"result":{"type":"succeeded","message":{"id":"one"}}}`),
		}
		if err := s.WriteBatchItemResult(ctx, "batch-1:reading-1", result); err != nil {
			t.Fatalf("WriteBatchItemResult first: %v", err)
		}
		idempotentReplay := result
		idempotentReplay.ResultJSON = json.RawMessage(`{"result":{"message":{"id":"one"},"type":"succeeded"}}`)
		if err := s.WriteBatchItemResult(ctx, "batch-1:reading-1", idempotentReplay); err != nil {
			t.Fatalf("WriteBatchItemResult idempotent semantic repeat: %v", err)
		}
		err = s.WriteBatchItemResult(ctx, "batch-1:reading-1", store.BatchItemResultFields{
			Now:        at(31),
			State:      store.BatchItemStateSucceeded,
			ResultJSON: json.RawMessage(`{"result":{"type":"succeeded","changed":true}}`),
		})
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("WriteBatchItemResult changed repeat error = %v, want ErrConflict", err)
		}

		gotItem, err := s.GetBatchItemByCustomID(ctx, "batch-1:reading-1")
		if err != nil {
			t.Fatalf("GetBatchItemByCustomID: %v", err)
		}
		if gotItem.State != store.BatchItemStateSucceeded || gotItem.FinishedAt == nil || !gotItem.FinishedAt.Equal(at(30)) {
			t.Fatalf("succeeded item state/finished_at = %q/%v, want succeeded/%v",
				gotItem.State, gotItem.FinishedAt, at(30))
		}
		assertJSONEqual(t, "ResultJSON", result.ResultJSON, gotItem.ResultJSON)

		err = s.CreatePlannedBatch(ctx, store.BatchCreateFields{
			ID:  "batch-2",
			Now: at(40),
			Items: []store.BatchItemCreateFields{
				{
					ReadingID:   "reading-1",
					CustomID:    "batch-2:reading-1",
					RequestJSON: json.RawMessage(`{"custom_id":"batch-2:reading-1"}`),
				},
			},
		})
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("CreatePlannedBatch before apply error = %v, want ErrConflict", err)
		}

		if err := s.MarkBatchItemApplied(ctx, "batch-1:reading-1", store.BatchItemApplyFields{Now: at(50)}); err != nil {
			t.Fatalf("MarkBatchItemApplied: %v", err)
		}
		gotItem, err = s.GetBatchItemByCustomID(ctx, "batch-1:reading-1")
		if err != nil {
			t.Fatalf("GetBatchItemByCustomID after apply: %v", err)
		}
		if gotItem.State != store.BatchItemStateApplied || gotItem.AppliedAt == nil || !gotItem.AppliedAt.Equal(at(50)) {
			t.Fatalf("applied item state/applied_at = %q/%v, want applied/%v",
				gotItem.State, gotItem.AppliedAt, at(50))
		}

		if err := s.CreatePlannedBatch(ctx, store.BatchCreateFields{
			ID:  "batch-2",
			Now: at(60),
			Items: []store.BatchItemCreateFields{
				{
					ReadingID:   "reading-1",
					CustomID:    "batch-2:reading-1",
					RequestJSON: json.RawMessage(`{"custom_id":"batch-2:reading-1"}`),
				},
			},
		}); err != nil {
			t.Fatalf("CreatePlannedBatch after apply: %v", err)
		}
	})

	t.Run("BatchCannotApplyWithActiveItems", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		if err := s.CreatePlannedBatch(ctx, store.BatchCreateFields{
			ID:  "batch-1",
			Now: at(10),
			Items: []store.BatchItemCreateFields{
				{
					ReadingID:   "reading-1",
					CustomID:    "batch-1:reading-1",
					RequestJSON: json.RawMessage(`{"custom_id":"batch-1:reading-1"}`),
				},
			},
		}); err != nil {
			t.Fatalf("CreatePlannedBatch: %v", err)
		}
		if err := s.SubmitBatch(ctx, "batch-1", store.BatchSubmitFields{
			Now:      at(20),
			RemoteID: "msgbatch_123",
		}); err != nil {
			t.Fatalf("SubmitBatch: %v", err)
		}
		if err := s.UpdateBatchState(ctx, "batch-1", store.BatchStateResultsReady, store.BatchStateFields{
			Now:    at(30),
			Counts: &store.BatchCounts{Succeeded: 1},
		}); err != nil {
			t.Fatalf("UpdateBatchState results_ready: %v", err)
		}
		err := s.UpdateBatchState(ctx, "batch-1", store.BatchStateApplied, store.BatchStateFields{Now: at(40)})
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("UpdateBatchState applied with active item error = %v, want ErrConflict", err)
		}

		if err := s.WriteBatchItemResult(ctx, "batch-1:reading-1", store.BatchItemResultFields{
			Now:        at(50),
			State:      store.BatchItemStateSucceeded,
			ResultJSON: json.RawMessage(`{"result":{"type":"succeeded"}}`),
		}); err != nil {
			t.Fatalf("WriteBatchItemResult: %v", err)
		}
		if err := s.MarkBatchItemApplied(ctx, "batch-1:reading-1", store.BatchItemApplyFields{Now: at(60)}); err != nil {
			t.Fatalf("MarkBatchItemApplied: %v", err)
		}
		if err := s.UpdateBatchState(ctx, "batch-1", store.BatchStateApplied, store.BatchStateFields{Now: at(70)}); err != nil {
			t.Fatalf("UpdateBatchState applied after item apply: %v", err)
		}
	})

	t.Run("BatchStateCanPersistZeroCounts", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		if err := s.CreatePlannedBatch(ctx, store.BatchCreateFields{
			ID:  "batch-1",
			Now: at(10),
			Items: []store.BatchItemCreateFields{
				{
					ReadingID:   "reading-1",
					CustomID:    "batch-1:reading-1",
					RequestJSON: json.RawMessage(`{"custom_id":"batch-1:reading-1"}`),
				},
			},
		}); err != nil {
			t.Fatalf("CreatePlannedBatch: %v", err)
		}
		if err := s.SubmitBatch(ctx, "batch-1", store.BatchSubmitFields{
			Now:      at(20),
			RemoteID: "msgbatch_123",
			Counts:   store.BatchCounts{Processing: 1},
		}); err != nil {
			t.Fatalf("SubmitBatch: %v", err)
		}

		zero := store.BatchCounts{}
		if err := s.UpdateBatchState(ctx, "batch-1", store.BatchStateSubmitted, store.BatchStateFields{
			Now:    at(30),
			Counts: &zero,
		}); err != nil {
			t.Fatalf("UpdateBatchState submitted zero counts: %v", err)
		}
		got, err := s.GetBatch(ctx, "batch-1")
		if err != nil {
			t.Fatalf("GetBatch: %v", err)
		}
		if got.State != store.BatchStateSubmitted || got.Counts != zero {
			t.Fatalf("state/counts = %q/%+v, want submitted/zero counts", got.State, got.Counts)
		}
		if err := s.UpdateBatchState(ctx, "batch-1", store.BatchStateFailed, store.BatchStateFields{
			Now:    at(40),
			Counts: &zero,
		}); err != nil {
			t.Fatalf("UpdateBatchState failed zero counts: %v", err)
		}
		got, err = s.GetBatch(ctx, "batch-1")
		if err != nil {
			t.Fatalf("GetBatch failed: %v", err)
		}
		if got.State != store.BatchStateFailed || got.Counts != zero {
			t.Fatalf("failed state/counts = %q/%+v, want failed/zero counts", got.State, got.Counts)
		}
	})

	t.Run("RejectsInvalidCounts", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		if err := s.CreatePlannedBatch(ctx, store.BatchCreateFields{
			ID:  "batch-1",
			Now: at(10),
			Items: []store.BatchItemCreateFields{
				{
					ReadingID:   "reading-1",
					CustomID:    "batch-1:reading-1",
					RequestJSON: json.RawMessage(`{"custom_id":"batch-1:reading-1"}`),
				},
			},
		}); err != nil {
			t.Fatalf("CreatePlannedBatch: %v", err)
		}
		err := s.SubmitBatch(ctx, "batch-1", store.BatchSubmitFields{
			Now:      at(20),
			RemoteID: "msgbatch_123",
			Counts:   store.BatchCounts{Processing: -1},
		})
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("SubmitBatch negative counts error = %v, want ErrConflict", err)
		}

		if err := s.SubmitBatch(ctx, "batch-1", store.BatchSubmitFields{
			Now:      at(30),
			RemoteID: "msgbatch_123",
		}); err != nil {
			t.Fatalf("SubmitBatch valid counts: %v", err)
		}
		bad := store.BatchCounts{Succeeded: -1}
		err = s.UpdateBatchState(ctx, "batch-1", store.BatchStateResultsReady, store.BatchStateFields{
			Now:    at(40),
			Counts: &bad,
		})
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("UpdateBatchState negative counts error = %v, want ErrConflict", err)
		}
	})

	t.Run("RejectsInvalidJSON", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s := newStore(t)
		err := s.CreatePlannedBatch(ctx, store.BatchCreateFields{
			ID:  "batch-1",
			Now: at(10),
			Items: []store.BatchItemCreateFields{
				{
					ReadingID:   "reading-1",
					CustomID:    "batch-1:reading-1",
					RequestJSON: json.RawMessage(`{not-json`),
				},
			},
		})
		if err == nil {
			t.Fatal("CreatePlannedBatch with invalid RequestJSON error = nil, want error")
		}

		if err := s.CreatePlannedBatch(ctx, store.BatchCreateFields{
			ID:  "batch-2",
			Now: at(20),
			Items: []store.BatchItemCreateFields{
				{
					ReadingID:   "reading-2",
					CustomID:    "batch-2:reading-2",
					RequestJSON: json.RawMessage(`{"custom_id":"batch-2:reading-2"}`),
				},
			},
		}); err != nil {
			t.Fatalf("CreatePlannedBatch valid: %v", err)
		}
		if err := s.SubmitBatch(ctx, "batch-2", store.BatchSubmitFields{Now: at(30), RemoteID: "msgbatch_123"}); err != nil {
			t.Fatalf("SubmitBatch: %v", err)
		}
		err = s.WriteBatchItemResult(ctx, "batch-2:reading-2", store.BatchItemResultFields{
			Now:        at(40),
			State:      store.BatchItemStateSucceeded,
			ResultJSON: json.RawMessage(`{not-json`),
		})
		if err == nil {
			t.Fatal("WriteBatchItemResult with invalid ResultJSON error = nil, want error")
		}
	})
}

func assertBatchItemsEqual(t *testing.T, label string, want, got []store.ManualBatchItem) {
	t.Helper()

	if diff := cmp.Diff(want, got, cmpopts.IgnoreFields(store.ManualBatchItem{}, "RequestJSON", "ResultJSON")); diff != "" {
		t.Fatalf("%s mismatch (-want +got):\n%s", label, diff)
	}
	for i := range want {
		assertJSONEqual(t, label+" RequestJSON", want[i].RequestJSON, got[i].RequestJSON)
		if len(want[i].ResultJSON) != 0 || len(got[i].ResultJSON) != 0 {
			assertJSONEqual(t, label+" ResultJSON", want[i].ResultJSON, got[i].ResultJSON)
		}
	}
}
