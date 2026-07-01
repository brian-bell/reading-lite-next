//go:build integration

package vector_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bbell/reading-lite/internal/store"
	"github.com/bbell/reading-lite/internal/vector"
	"github.com/bbell/reading-lite/internal/vector/vectortest"
)

// TestPostgresVectorContract runs the shared VectorIndex conformance suite against
// the pgvector-backed adapter, proving its cosine ranking, self-exclusion, topK
// bounds, and dimension enforcement match vector.Memory's.
func TestPostgresVectorContract(t *testing.T) {
	ctx := context.Background()
	var schemaCounter atomic.Int64
	dsn := postgresDSN(t, ctx)

	vectortest.RunContract(t, func(t *testing.T) vector.Index {
		t.Helper()
		pool := newSchemaPool(t, ctx, dsn, &schemaCounter)
		if err := store.ApplyMigrations(ctx, pool); err != nil {
			t.Fatalf("apply migrations: %v", err)
		}
		return &seedingIndex{pool: pool, idx: vector.NewPostgres(pool)}
	})
}

// seedingIndex wraps vector.Postgres for the contract run. reading_vectors.reading_id
// FK-references readings(id), so a vector cannot be inserted for an id with no
// readings row. In production the pipeline always creates the reading first; the
// contract suite upserts bare ids, so the wrapper seeds a minimal readings row
// before each Upsert. Query/Delete delegate unchanged.
type seedingIndex struct {
	pool *pgxpool.Pool
	idx  *vector.Postgres
}

func (s *seedingIndex) Upsert(ctx context.Context, id string, vec []float32, generation *time.Time) error {
	// Dimension rejection must happen before touching the DB, matching the port
	// contract (and avoiding a stray seeded row for an invalid upsert).
	if len(vec) != vector.Dim {
		return s.idx.Upsert(ctx, id, vec, generation)
	}
	// Seed only the row's existence (never any vector data); "do nothing" on
	// conflict so the contract's UpsertReplaces case is decided entirely by the
	// real adapter's "on conflict (reading_id) do update", not by this wrapper.
	if _, err := s.pool.Exec(ctx, `
insert into readings (id, url, url_key, status, source_kind, created_at, updated_at)
values ($1, $2, $3, 'ready', 'web', now(), now())
on conflict (id) do nothing`,
		id, "https://example.com/"+id, "veckey-"+id); err != nil {
		return fmt.Errorf("seed reading %q: %w", id, err)
	}
	return s.idx.Upsert(ctx, id, vec, generation)
}

func (s *seedingIndex) Query(ctx context.Context, vec []float32, topK int, excludeID string) ([]vector.Match, error) {
	return s.idx.Query(ctx, vec, topK, excludeID)
}

func (s *seedingIndex) Delete(ctx context.Context, id string) error {
	return s.idx.Delete(ctx, id)
}

func postgresDSN(t *testing.T, ctx context.Context) string {
	t.Helper()
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		return dsn
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)
	container, err := tcpostgres.Run(ctx,
		"pgvector/pgvector:pg16",
		tcpostgres.WithDatabase("reading_lite"),
		tcpostgres.WithUsername("reader"),
		tcpostgres.WithPassword("reader"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	testcontainers.CleanupContainer(t, container)

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres connection string: %v", err)
	}
	return dsn
}

func newSchemaPool(t *testing.T, ctx context.Context, dsn string, counter *atomic.Int64) *pgxpool.Pool {
	t.Helper()
	root, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open root postgres pool: %v", err)
	}
	t.Cleanup(root.Close)

	schema := fmt.Sprintf("vector_contract_%d", counter.Add(1))
	if _, err := root.Exec(ctx, `create schema `+quoteIdent(schema)); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec(context.Background(), `drop schema if exists `+quoteIdent(schema)+` cascade`)
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse postgres config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open schema postgres pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
