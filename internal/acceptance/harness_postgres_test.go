//go:build verify

package acceptance_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bbell/reading-lite/internal/store"
	"github.com/bbell/reading-lite/internal/store/storetest"
	"github.com/bbell/reading-lite/internal/vector"
	"github.com/bbell/reading-lite/internal/vector/vectortest"
)

// backend names a store implementation the blackbox acceptance tests run against.
// factory(t) sets the backend up (and may t.Skip, e.g. Postgres when Docker is
// unavailable) and returns a storetest.Factory that mints fresh, isolated stores.
type backend struct {
	name    string
	factory func(t *testing.T) storetest.Factory
}

// storeBackends is the matrix every store-backed acceptance test iterates: the
// in-memory fake (always available) and real Postgres via testcontainers (skips
// when Docker/the provider is unavailable, or uses DATABASE_URL when set).
func storeBackends() []backend {
	return []backend{
		{
			name:    "memory",
			factory: func(*testing.T) storetest.Factory { return memoryFactory },
		},
		{
			name:    "postgres",
			factory: postgresFactory,
		},
	}
}

func memoryFactory(*testing.T) store.Store { return store.NewMemory() }

// Shared Postgres container, started lazily on first use and torn down in
// TestMain. Each minted store gets its own schema so the contract suite's
// parallel subtests stay isolated.
var (
	pgInit       sync.Once
	pgDSN        string
	pgSkipReason string
	pgContainer  *tcpostgres.PostgresContainer
	pgSchemaCtr  atomic.Int64
)

func TestMain(m *testing.M) {
	code := m.Run()
	if pgContainer != nil {
		_ = pgContainer.Terminate(context.Background())
	}
	os.Exit(code)
}

func postgresFactory(t *testing.T) storetest.Factory {
	dsn := postgresDSN(t) // skips the calling (sub)test when Postgres is unavailable
	return func(t *testing.T) store.Store {
		return newPostgresStoreFromDSN(t, dsn)
	}
}

func postgresDSN(t *testing.T) string {
	t.Helper()
	pgInit.Do(func() {
		ctx := context.Background()
		if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
			pgDSN = dsn
			return
		}
		container, err := tcpostgres.Run(ctx,
			"pgvector/pgvector:pg16",
			tcpostgres.WithDatabase("reading_lite"),
			tcpostgres.WithUsername("reader"),
			tcpostgres.WithPassword("reader"),
			tcpostgres.BasicWaitStrategies(),
		)
		if err != nil {
			pgSkipReason = "testcontainers Postgres unavailable " +
				"(start Docker, run via `sg docker -c …`, or set DATABASE_URL): " + err.Error()
			return
		}
		dsn, err := container.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			_ = container.Terminate(ctx)
			pgSkipReason = "postgres connection string: " + err.Error()
			return
		}
		pgContainer = container
		pgDSN = dsn
	})
	if pgSkipReason != "" {
		t.Skip(pgSkipReason)
	}
	return pgDSN
}

// newPostgresStoreFromDSN returns a store.Postgres bound to a fresh, migrated
// schema so concurrent contract subtests cannot see each other's rows.
func newPostgresStoreFromDSN(t *testing.T, dsn string) store.Store {
	t.Helper()
	return store.NewPostgres(newSchemaPool(t, dsn))
}

// newSchemaPool creates a fresh, isolated schema (search_path-scoped), applies the
// store migrations into it, and returns a pool bound to it. Each minted backend
// gets its own schema so the contract suite's parallel subtests stay isolated.
func newSchemaPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	root, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open root postgres pool: %v", err)
	}
	t.Cleanup(root.Close)

	schema := fmt.Sprintf("acceptance_%d", pgSchemaCtr.Add(1))
	if _, err := root.Exec(ctx, "create schema "+quoteIdent(schema)); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec(context.Background(), "drop schema if exists "+quoteIdent(schema)+" cascade")
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

	if err := store.ApplyMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return pool
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// vectorBackends is the matrix every vector-contract acceptance test iterates: the
// in-memory cosine index (always) and the pgvector adapter via testcontainers
// (skips when Docker/the provider is unavailable). Running both here proves
// vector.Memory↔vector.Postgres parity inside `make verify`, mirroring the store.
func vectorBackends() []vectorBackend {
	return []vectorBackend{
		{
			name: "memory",
			factory: func(*testing.T) vectortest.Factory {
				return func(*testing.T) vector.Index { return vector.NewMemory() }
			},
		},
		{
			name:    "postgres",
			factory: postgresVectorFactory,
		},
	}
}

type vectorBackend struct {
	name    string
	factory func(t *testing.T) vectortest.Factory
}

func postgresVectorFactory(t *testing.T) vectortest.Factory {
	dsn := postgresDSN(t) // skips the calling (sub)test when Postgres is unavailable
	return func(t *testing.T) vector.Index {
		return &seedingVectorIndex{pool: newSchemaPool(t, dsn)}
	}
}

// seedingVectorIndex wraps vector.Postgres for the contract run. reading_vectors
// FK-references readings(id), so a bare-id upsert (which the contract does, lacking
// a readings row) needs a minimal row seeded first; production always creates the
// reading before its vector. Query/Delete and dimension rejection delegate
// unchanged, so the suite still exercises the real adapter's behavior.
type seedingVectorIndex struct {
	pool *pgxpool.Pool
}

func (s *seedingVectorIndex) Upsert(ctx context.Context, id string, vec []float32, generation *time.Time) error {
	if len(vec) != vector.Dim {
		return vector.NewPostgres(s.pool).Upsert(ctx, id, vec, generation) // returns ErrDimension without touching the DB
	}
	if _, err := s.pool.Exec(ctx, `
insert into readings (id, url, url_key, status, source_kind, created_at, updated_at)
values ($1, $2, $3, 'ready', 'web', now(), now())
on conflict (id) do nothing`,
		id, "https://example.com/"+id, "veckey-"+id); err != nil {
		return fmt.Errorf("seed reading %q: %w", id, err)
	}
	return vector.NewPostgres(s.pool).Upsert(ctx, id, vec, generation)
}

func (s *seedingVectorIndex) Query(ctx context.Context, vec []float32, topK int, excludeID string) ([]vector.Match, error) {
	return vector.NewPostgres(s.pool).Query(ctx, vec, topK, excludeID)
}

func (s *seedingVectorIndex) Delete(ctx context.Context, id string) error {
	return vector.NewPostgres(s.pool).Delete(ctx, id)
}
