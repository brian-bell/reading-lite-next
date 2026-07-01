package vector

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

// Postgres is the production [Index]: a pgvector-backed similarity index over the
// reading_vectors table. It is pinned by the same vectortest.RunContract as
// [Memory], so the cosine ranking of the fake and the real adapter cannot diverge.
//
// Vectors are passed as a text literal cast to the vector type ($N::vector), which
// needs no per-connection type registration; the pool only needs the pgvector
// extension installed (the store migrations do this). The caller owns pool setup
// and the prerequisite readings row (reading_vectors.reading_id FK-references it).
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres returns an Index backed by pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool}
}

// Upsert stores or replaces the vector for id. The vector must have length
// [Dim]. generation fences the write per [Index.Upsert]: a stale write against
// an already-newer-generation row resolves the ON CONFLICT as a no-op (0 rows
// touched, original row untouched), matching Memory's silent no-op.
func (p *Postgres) Upsert(ctx context.Context, id string, vec []float32, generation *time.Time) error {
	if len(vec) != Dim {
		return ErrDimension
	}
	_, err := p.pool.Exec(ctx, `
insert into reading_vectors (reading_id, embedding, generation) values ($1, $2::vector, $3)
on conflict (reading_id) do update
  set embedding = excluded.embedding, generation = excluded.generation
  where excluded.generation is null
     or reading_vectors.generation is null
     or excluded.generation >= reading_vectors.generation`,
		id, pgvector.NewVector(vec).String(), generation)
	if err != nil {
		return fmt.Errorf("vector: upsert %q: %w", id, err)
	}
	return nil
}

// Query returns up to topK matches ranked by descending cosine similarity,
// omitting excludeID ("" excludes nothing). The query vector must have length [Dim].
func (p *Postgres) Query(ctx context.Context, vec []float32, topK int, excludeID string) ([]Match, error) {
	if len(vec) != Dim {
		return nil, ErrDimension
	}
	if topK <= 0 {
		return []Match{}, nil
	}

	// pgvector's <=> is cosine distance, so similarity is 1 - distance. Ordering by
	// distance ascending (with an id tie-break) matches Memory's score-descending,
	// id-ascending order exactly.
	rows, err := p.pool.Query(ctx, `
select reading_id, 1 - (embedding <=> $1::vector) as score
from reading_vectors
where ($2 = '' or reading_id <> $2)
order by embedding <=> $1::vector asc, reading_id asc
limit $3`,
		pgvector.NewVector(vec).String(), excludeID, topK)
	if err != nil {
		return nil, fmt.Errorf("vector: query: %w", err)
	}
	defer rows.Close()

	matches := []Match{}
	for rows.Next() {
		var m Match
		if err := rows.Scan(&m.ID, &m.Score); err != nil {
			return nil, fmt.Errorf("vector: scan match: %w", err)
		}
		matches = append(matches, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("vector: iterate matches: %w", err)
	}
	return matches, nil
}

// Delete removes id. A missing id is not an error.
func (p *Postgres) Delete(ctx context.Context, id string) error {
	if _, err := p.pool.Exec(ctx, `delete from reading_vectors where reading_id = $1`, id); err != nil {
		return fmt.Errorf("vector: delete %q: %w", id, err)
	}
	return nil
}
