// Package vectortest defines the behavioral contract shared by VectorIndex
// implementations. It runs against vector.Memory on every test and against the
// pgvector-backed vector.Postgres under -tags integration, so the cosine math and
// result ordering of the fake and the production adapter cannot diverge.
package vectortest

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/bbell/reading-lite/internal/vector"
)

// Factory returns a fresh, empty vector index for one contract test.
type Factory func(t *testing.T) vector.Index

// RunContract runs the VectorIndex conformance suite.
func RunContract(t *testing.T, newIndex Factory) {
	t.Helper()

	t.Run("QueryRanksByCosine", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		idx := newIndex(t)
		// a points along axis 0, b along axis 1, c at 45° between them.
		mustUpsert(t, idx, "a", vec(1, 0))
		mustUpsert(t, idx, "b", vec(0, 1))
		mustUpsert(t, idx, "c", vec(1, 1))

		// A query along axis 0 is identical to a (cos 1), 45° from c (cos ~0.707),
		// orthogonal to b (cos 0): the ranking must be a, then c, then b.
		got, err := idx.Query(ctx, vec(1, 0), 3, "")
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if ids := matchIDs(got); !equalStrings(ids, []string{"a", "c", "b"}) {
			t.Fatalf("ranking = %v, want [a c b]", ids)
		}
		if !(got[0].Score > got[1].Score && got[1].Score > got[2].Score) {
			t.Fatalf("scores not strictly descending: %v", matchScores(got))
		}
		if math.Abs(got[0].Score-1) > 1e-6 {
			t.Fatalf("top score = %v, want ~1.0 (identical direction)", got[0].Score)
		}
	})

	t.Run("ExcludesSelf", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		idx := newIndex(t)
		mustUpsert(t, idx, "self", vec(1, 0))
		mustUpsert(t, idx, "other", vec(1, 0.1))

		got, err := idx.Query(ctx, vec(1, 0), 10, "self")
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		for _, m := range got {
			if m.ID == "self" {
				t.Fatalf("excluded id present in results: %v", matchIDs(got))
			}
		}
		if !equalStrings(matchIDs(got), []string{"other"}) {
			t.Fatalf("results = %v, want [other]", matchIDs(got))
		}
	})

	t.Run("DeleteRemoves", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		idx := newIndex(t)
		mustUpsert(t, idx, "a", vec(1, 0))
		mustUpsert(t, idx, "b", vec(0, 1))

		if err := idx.Delete(ctx, "a"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		got, err := idx.Query(ctx, vec(1, 0), 10, "")
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if !equalStrings(matchIDs(got), []string{"b"}) {
			t.Fatalf("after delete = %v, want [b]", matchIDs(got))
		}

		// Deleting an absent id is a no-op, not an error.
		if err := idx.Delete(ctx, "a"); err != nil {
			t.Fatalf("Delete absent = %v, want nil", err)
		}
	})

	t.Run("UpsertReplaces", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		idx := newIndex(t)
		mustUpsert(t, idx, "a", vec(1, 0))
		mustUpsert(t, idx, "a", vec(0, 1)) // replace direction

		got, err := idx.Query(ctx, vec(0, 1), 10, "")
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if !equalStrings(matchIDs(got), []string{"a"}) {
			t.Fatalf("results = %v, want a once (no duplicate)", matchIDs(got))
		}
		if math.Abs(got[0].Score-1) > 1e-6 {
			t.Fatalf("score = %v, want ~1.0 (upsert replaced the vector)", got[0].Score)
		}
	})

	t.Run("TopKBounds", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		idx := newIndex(t)
		mustUpsert(t, idx, "a", vec(1, 0))
		mustUpsert(t, idx, "b", vec(0, 1))
		mustUpsert(t, idx, "c", vec(1, 1))

		top2, err := idx.Query(ctx, vec(1, 0), 2, "")
		if err != nil {
			t.Fatalf("Query topK=2: %v", err)
		}
		if len(top2) != 2 || !equalStrings(matchIDs(top2), []string{"a", "c"}) {
			t.Fatalf("topK=2 = %v, want [a c]", matchIDs(top2))
		}

		none, err := idx.Query(ctx, vec(1, 0), 0, "")
		if err != nil {
			t.Fatalf("Query topK=0: %v", err)
		}
		if len(none) != 0 {
			t.Fatalf("topK=0 = %v, want empty", matchIDs(none))
		}

		all, err := idx.Query(ctx, vec(1, 0), 99, "")
		if err != nil {
			t.Fatalf("Query topK=99: %v", err)
		}
		if len(all) != 3 {
			t.Fatalf("topK>count = %d results, want 3 (all available)", len(all))
		}
	})

	t.Run("EmptyExcludeIDExcludesNothing", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		idx := newIndex(t)
		// An empty string is a valid id, and the zero-value excludeID must not drop it.
		mustUpsert(t, idx, "", vec(1, 0))
		mustUpsert(t, idx, "a", vec(1, 0))

		got, err := idx.Query(ctx, vec(1, 0), 10, "")
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if !equalStrings(matchIDs(got), []string{"", "a"}) {
			t.Fatalf("results = %v, want both ids (\"\" excludes nothing)", matchIDs(got))
		}
	})

	t.Run("EmptyIndexReturnsNoMatches", func(t *testing.T) {
		t.Parallel()

		got, err := newIndex(t).Query(context.Background(), vec(1, 0), 10, "")
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("empty index = %v, want no matches", got)
		}
	})

	t.Run("RejectsWrongDimension", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		idx := newIndex(t)
		short := make([]float32, vector.Dim-1)

		if err := idx.Upsert(ctx, "a", short, nil); !errors.Is(err, vector.ErrDimension) {
			t.Fatalf("Upsert wrong dim = %v, want ErrDimension", err)
		}
		if _, err := idx.Query(ctx, short, 10, ""); !errors.Is(err, vector.ErrDimension) {
			t.Fatalf("Query wrong dim = %v, want ErrDimension", err)
		}
	})

	t.Run("GenerationFenceRejectsStaleUpsert", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		idx := newIndex(t)
		older := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		newer := older.Add(time.Hour)

		mustUpsertAt(t, idx, "a", vec(1, 0), &newer)

		// A stale write from an older generation is a silent no-op, not an
		// error, and must not overwrite the newer vector.
		if err := idx.Upsert(ctx, "a", vec(0, 1), &older); err != nil {
			t.Fatalf("Upsert stale generation = %v, want nil (silent no-op)", err)
		}
		got, err := idx.Query(ctx, vec(1, 0), 1, "")
		if err != nil {
			t.Fatalf("Query after stale upsert: %v", err)
		}
		if len(got) != 1 || math.Abs(got[0].Score-1) > 1e-6 {
			t.Fatalf("stale generation overwrote newer vector: %v, want [a] at score ~1.0", got)
		}

		// An equal generation (idempotent retry of the same run) still applies.
		mustUpsertAt(t, idx, "a", vec(0, 1), &newer)
		got, err = idx.Query(ctx, vec(0, 1), 1, "")
		if err != nil {
			t.Fatalf("Query after same-generation upsert: %v", err)
		}
		if len(got) != 1 || math.Abs(got[0].Score-1) > 1e-6 {
			t.Fatalf("same-generation upsert did not apply: %v, want [a] at score ~1.0", got)
		}

		// A nil generation always applies (unfenced).
		mustUpsertAt(t, idx, "a", vec(1, 0), nil)
		got, err = idx.Query(ctx, vec(1, 0), 1, "")
		if err != nil {
			t.Fatalf("Query after unfenced upsert: %v", err)
		}
		if len(got) != 1 || math.Abs(got[0].Score-1) > 1e-6 {
			t.Fatalf("unfenced upsert did not apply: %v, want [a] at score ~1.0", got)
		}

		// A row that has never carried a generation (e.g. a pre-migration
		// reading_vectors row, whose generation column starts NULL) accepts its
		// first fenced write unconditionally, since there is no established
		// generation to compare against.
		mustUpsertAt(t, idx, "b", vec(1, 0), nil)
		mustUpsertAt(t, idx, "b", vec(0, 1), &newer)
		got, err = idx.Query(ctx, vec(0, 1), 1, "")
		if err != nil {
			t.Fatalf("Query after first-fenced-write-on-unfenced-row: %v", err)
		}
		if !equalStrings(matchIDs(got), []string{"b"}) || math.Abs(got[0].Score-1) > 1e-6 {
			t.Fatalf("first fenced write on a never-fenced row was rejected: %v, want [b] at score ~1.0", got)
		}

		// A nil-generation write is unconditional in both directions (per the
		// Index.Upsert doc comment): it doesn't just skip being fenced by an
		// established entry, it also clears that entry's own generation, so a
		// stale write that later reuses the same older generation would no
		// longer be rejected — until a fresh non-nil write re-establishes the
		// fence.
		mustUpsertAt(t, idx, "a", vec(1, 0), &newer)
		mustUpsertAt(t, idx, "a", vec(0, 1), nil) // un-fences "a"
		if err := idx.Upsert(ctx, "a", vec(1, 0), &older); err != nil {
			t.Fatalf("Upsert with stale generation against an un-fenced row = %v, want nil", err)
		}
		got, err = idx.Query(ctx, vec(1, 0), 1, "")
		if err != nil {
			t.Fatalf("Query after write against un-fenced row: %v", err)
		}
		if !equalStrings(matchIDs(got), []string{"a"}) || math.Abs(got[0].Score-1) > 1e-6 {
			t.Fatalf("stale write against an un-fenced row was rejected: %v, want [a] at score ~1.0 (nil-generation writes clear the fence)", got)
		}
	})
}

// vec builds a Dim-length vector whose leading entries are components and whose
// remaining entries are zero, so contract tests can reason about cosine angles in
// the first few dimensions without writing out 1536 values.
func vec(components ...float32) []float32 {
	v := make([]float32, vector.Dim)
	copy(v, components)
	return v
}

func mustUpsert(t *testing.T, idx vector.Index, id string, v []float32) {
	t.Helper()
	if err := idx.Upsert(context.Background(), id, v, nil); err != nil {
		t.Fatalf("Upsert %q: %v", id, err)
	}
}

// mustUpsertAt is like mustUpsert but carries an explicit generation fence,
// for the generation-fence contract case below. mustUpsert (nil generation)
// stays untouched for the other, unfenced contract cases above.
func mustUpsertAt(t *testing.T, idx vector.Index, id string, v []float32, generation *time.Time) {
	t.Helper()
	if err := idx.Upsert(context.Background(), id, v, generation); err != nil {
		t.Fatalf("Upsert %q at %v: %v", id, generation, err)
	}
}

func matchIDs(matches []vector.Match) []string {
	ids := make([]string, len(matches))
	for i, m := range matches {
		ids[i] = m.ID
	}
	return ids
}

func matchScores(matches []vector.Match) []float64 {
	scores := make([]float64, len(matches))
	for i, m := range matches {
		scores[i] = m.Score
	}
	return scores
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
