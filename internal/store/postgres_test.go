//go:build integration

package store_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bbell/reading-lite/internal/reading"
	"github.com/bbell/reading-lite/internal/store"
	"github.com/bbell/reading-lite/internal/store/storetest"
)

func TestPostgresStoreContract(t *testing.T) {
	ctx := context.Background()
	var schemaCounter atomic.Int64
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
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

		dsn, err = container.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			t.Fatalf("postgres connection string: %v", err)
		}
	}

	storetest.RunContract(t, func(t *testing.T) store.Store {
		t.Helper()
		return newPostgresStoreFromDSN(t, ctx, dsn, &schemaCounter)
	})
}

func TestPostgresMigrationsAreIdempotent(t *testing.T) {
	ctx := context.Background()
	var schemaCounter atomic.Int64
	pool := newPostgresPool(t, ctx, &schemaCounter)

	if err := store.ApplyMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations first: %v", err)
	}
	if err := store.ApplyMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations second: %v", err)
	}
}

func TestPostgresDeleteCascadesVector(t *testing.T) {
	ctx := context.Background()
	var schemaCounter atomic.Int64
	pool := newPostgresPool(t, ctx, &schemaCounter)
	if err := store.ApplyMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	s := store.NewPostgres(pool)
	key, err := reading.URLKey("https://example.com/vector")
	if err != nil {
		t.Fatalf("URLKey: %v", err)
	}
	now := time.Unix(100, 0).UTC()
	if err := s.SaveReading(ctx, reading.Reading{
		ID:         "r1",
		URL:        "https://example.com/vector",
		URLKey:     key,
		Status:     reading.Ready,
		SourceKind: reading.SourceWeb,
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		t.Fatalf("SaveReading: %v", err)
	}
	if _, err := pool.Exec(ctx, `insert into reading_vectors (reading_id, embedding) values ($1, $2::vector)`, "r1", zeroVectorLiteral(1536)); err != nil {
		t.Fatalf("insert vector: %v", err)
	}
	if err := s.Delete(ctx, "r1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `select count(*) from reading_vectors where reading_id = $1`, "r1").Scan(&count); err != nil {
		t.Fatalf("count vectors: %v", err)
	}
	if count != 0 {
		t.Fatalf("vector rows after delete = %d, want 0", count)
	}
}

func newPostgresStoreFromDSN(t *testing.T, ctx context.Context, dsn string, counter *atomic.Int64) store.Store {
	t.Helper()
	pool := newPostgresPoolFromDSN(t, ctx, dsn, counter)
	if err := store.ApplyMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return store.NewPostgres(pool)
}

func newPostgresPool(t *testing.T, ctx context.Context, counter *atomic.Int64) *pgxpool.Pool {
	t.Helper()
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		return newPostgresPoolFromDSN(t, ctx, dsn, counter)
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
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open postgres pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newPostgresPoolFromDSN(t *testing.T, ctx context.Context, dsn string, counter *atomic.Int64) *pgxpool.Pool {
	t.Helper()
	root, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open root postgres pool: %v", err)
	}
	t.Cleanup(root.Close)

	schema := fmt.Sprintf("store_contract_%d", counter.Add(1))
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

func zeroVectorLiteral(dim int) string {
	parts := make([]string, dim)
	for i := range parts {
		parts[i] = strconv.Itoa(0)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
