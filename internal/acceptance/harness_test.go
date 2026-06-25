//go:build verify

// Package acceptance_test is the executable form of docs/ACCEPTANCE.md.
//
// Each test corresponds to a section of that plan:
//
//	Section A (build & static analysis) -> TestStaticAnalysis_*, TestLint_*
//	Section B6 (codegen drift)          -> TestCodegen_SqlcNoDrift
//	Section C (component behavior)      -> TestAcceptance_*, TestPorts_*
//	Section D (conventions)             -> TestConventions_*
//
// It is blackbox: it imports only exported surfaces and inspects the repository
// from the outside (source files + the toolchain), exactly as a human verifier
// would. Run it with `make verify` (go test -tags verify ./internal/acceptance/...).
//
// Tests that shell out to the toolchain (go, gofmt, golangci-lint, sqlc) skip
// rather than fail when the tool is unavailable, mirroring how the integration
// suite skips when Docker is absent.
package acceptance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/bbell/reading-lite/internal/blobs"
	"github.com/bbell/reading-lite/internal/clock"
	"github.com/bbell/reading-lite/internal/dispatch"
	"github.com/bbell/reading-lite/internal/embed"
	"github.com/bbell/reading-lite/internal/extract"
	"github.com/bbell/reading-lite/internal/fetch"
	"github.com/bbell/reading-lite/internal/notify"
	"github.com/bbell/reading-lite/internal/pipeline"
	"github.com/bbell/reading-lite/internal/reading"
	"github.com/bbell/reading-lite/internal/store"
	"github.com/bbell/reading-lite/internal/store/storetest"
	"github.com/bbell/reading-lite/internal/summarize"
	"github.com/bbell/reading-lite/internal/vector"
	"github.com/bbell/reading-lite/internal/vector/vectortest"
)

// --- Compile-time port conformance (Section C: ports have the expected shape) ---

var (
	_ store.Store      = (*store.Memory)(nil)
	_ store.Store      = (*store.Postgres)(nil)
	_ clock.Clock      = clock.System{}
	_ clock.Clock      = (*clock.Fake)(nil)
	_ dispatch.Store   = (*store.Memory)(nil) // the memory backend satisfies the dispatcher's narrow Store port
	_ dispatch.Delayer = (*dispatch.FakeDelayer)(nil)
	_ dispatch.Delayer = dispatch.RealDelayer{}

	// Phase 4 external-service ports: each in-memory fake/backend satisfies its port.
	_ blobs.Blobs          = (*blobs.Memory)(nil)
	_ embed.Embedder       = (*embed.Fake)(nil)
	_ fetch.Fetcher        = (*fetch.Fake)(nil)
	_ extract.Extractor    = (*extract.Fake)(nil)
	_ summarize.Summarizer = (*summarize.Fake)(nil)
	_ notify.Notifier      = (*notify.Fake)(nil)
	_ vector.Index         = (*vector.Memory)(nil)

	// Phase 6 real adapters: each production adapter satisfies the same port as its fake.
	_ fetch.Fetcher        = (*fetch.HTTP)(nil)
	_ embed.Embedder       = (*embed.OpenAI)(nil)
	_ summarize.Summarizer = (*summarize.Anthropic)(nil)
	_ notify.Notifier      = (*notify.Resend)(nil)
	_ vector.Index         = (*vector.Postgres)(nil)
	_ blobs.Blobs          = (*blobs.R2)(nil)

	// Phase 7 real extractor: the readability tier ladder satisfies the Extractor
	// port. (extract.YouTube is intentionally not an Extractor — its oEmbed floor
	// takes a video URL and makes its own requests, so it sits beside the ladder.)
	_ extract.Extractor = (*extract.Readability)(nil)

	// Phase 5 pipeline: the memory store satisfies the pipeline's narrow Store
	// port, and Pipeline.Process has the dispatcher's Handler signature.
	_ pipeline.Store                                = (*store.Memory)(nil)
	_ func(context.Context, string) dispatch.Result = (&pipeline.Pipeline{}).Process
)

// ---------------------------------------------------------------------------
// Section A — build & static analysis
// ---------------------------------------------------------------------------

func TestStaticAnalysis_GoBuild(t *testing.T) {
	root, goBin := repoRoot(t), goBin(t)
	if out, err := runTool(t, root, goBin, "build", "./..."); err != nil {
		t.Fatalf("go build ./... failed: %v\n%s", err, out)
	}
}

func TestStaticAnalysis_GoVet(t *testing.T) {
	root, goBin := repoRoot(t), goBin(t)
	if out, err := runTool(t, root, goBin, "vet", "./..."); err != nil {
		t.Fatalf("go vet ./... failed: %v\n%s", err, out)
	}
}

func TestStaticAnalysis_GoVetIntegrationTag(t *testing.T) {
	// Proves every integration-tagged test still compiles under the integration
	// build tag (plan step A5): the store contract, the Phase 6 vector.Postgres
	// contract, and the Phase 6 blobs.R2 MinIO round-trip.
	root, goBin := repoRoot(t), goBin(t)
	if out, err := runTool(t, root, goBin, "vet", "-tags", "integration", "./..."); err != nil {
		t.Fatalf("go vet -tags integration ./... failed: %v\n%s", err, out)
	}
}

func TestStaticAnalysis_GoVetVerifyHarness(t *testing.T) {
	// TestStaticAnalysis_GoVet runs `go vet ./...` under the default tags, which
	// excludes the //go:build verify files — so this harness would never vet
	// itself. Vet the acceptance package under the verify tag to close that gap.
	root, goBin := repoRoot(t), goBin(t)
	if out, err := runTool(t, root, goBin, "vet", "-tags", "verify", "./internal/acceptance/..."); err != nil {
		t.Fatalf("go vet -tags verify ./internal/acceptance/... failed: %v\n%s", err, out)
	}
}

func TestStaticAnalysis_Gofmt(t *testing.T) {
	root, gofmt := repoRoot(t), gofmtBin(t)
	out, err := runTool(t, root, gofmt, "-l", ".")
	if err != nil {
		t.Fatalf("gofmt -l . failed: %v\n%s", err, out)
	}
	if files := strings.TrimSpace(out); files != "" {
		t.Fatalf("gofmt -l . reported unformatted files (run `gofmt -w .`):\n%s", files)
	}
}

func TestLint_Golangci(t *testing.T) {
	root := repoRoot(t)
	bin := optionalTool(t, "golangci-lint")
	if out, err := runTool(t, root, bin, "run", "./..."); err != nil {
		t.Fatalf("golangci-lint run ./... reported issues: %v\n%s", err, out)
	}
}

// ---------------------------------------------------------------------------
// Section B6 — generated code is reproducible (no sqlc drift)
// ---------------------------------------------------------------------------

func TestCodegen_SqlcNoDrift(t *testing.T) {
	root := repoRoot(t)
	sqlc := optionalTool(t, "sqlc")

	origCfg, err := os.ReadFile(filepath.Join(root, "sqlc.yaml"))
	if err != nil {
		t.Fatalf("read sqlc.yaml: %v", err)
	}

	tmp := t.TempDir()
	genDir := filepath.Join(tmp, "storedb")

	// sqlc resolves schema/queries/out relative to the config file's directory,
	// so rewrite the config (which lives in tmp) to point back at the real schema
	// and queries via paths relative to tmp, and emit into tmp/storedb. The
	// working tree is never mutated. Replacement is single-pass.
	schemaRel := relTo(t, tmp, filepath.Join(root, "internal/store/migrations"))
	queriesRel := relTo(t, tmp, filepath.Join(root, "internal/store/query.sql"))
	cfg := strings.NewReplacer(
		"internal/store/storedb", "storedb",
		"internal/store/migrations", schemaRel,
		"internal/store/query.sql", queriesRel,
	).Replace(string(origCfg))
	cfgPath := filepath.Join(tmp, "sqlc.verify.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write temp sqlc config: %v", err)
	}

	if out, err := runTool(t, tmp, sqlc, "generate", "-f", cfgPath); err != nil {
		t.Fatalf("sqlc generate failed: %v\n%s", err, out)
	}

	committed := filepath.Join(root, "internal/store/storedb")
	assertGeneratedMatchesCommitted(t, committed, genDir)
}

// ---------------------------------------------------------------------------
// Section C — component behavior (blackbox, through public surfaces)
// ---------------------------------------------------------------------------

// TestAcceptance_StoreContract runs the shared Store conformance suite against
// every backend: the in-memory fake (always) and real Postgres via testcontainers
// (skips when Docker is unavailable). Running both here is how the harness proves
// fake<->Postgres parity directly, rather than deferring it to -tags integration.
func TestAcceptance_StoreContract(t *testing.T) {
	for _, be := range storeBackends() {
		t.Run(be.name, func(t *testing.T) {
			storetest.RunContract(t, be.factory(t))
		})
	}
}

// TestAcceptance_VectorIndexContract runs the shared VectorIndex conformance suite
// against every backend: the in-memory cosine index (always) and the pgvector
// adapter via testcontainers (skips without Docker). Running both here proves
// vector.Memory↔vector.Postgres parity inside `make verify` — the same mechanism
// TestAcceptance_StoreContract uses for the store — so the fake's ranking and the
// production adapter's cannot diverge.
func TestAcceptance_VectorIndexContract(t *testing.T) {
	for _, be := range vectorBackends() {
		t.Run(be.name, func(t *testing.T) {
			vectortest.RunContract(t, be.factory(t))
		})
	}
}

// TestPorts_FakesAreScriptableAndRecordCalls verifies, through each port's public
// surface, that the Phase 4 in-memory fakes are faithful doubles: they return a
// scripted result, can be scripted to error, and record their call count — the
// contract the pipeline (Phase 5) depends on to drive every Process branch with no
// real network. The vector and blob backends carry real behavior and are proven by
// their own contract/round-trip tests (TestAcceptance_VectorIndexContract and the
// blobs package suite), so this focuses on the five scriptable stubs.
func TestPorts_FakesAreScriptableAndRecordCalls(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("scripted failure")

	t.Run("fetch", func(t *testing.T) {
		f := &fetch.Fake{Resource: fetch.Resource{Status: 200, Body: []byte("body")}}
		if got, err := f.Get(ctx, "https://example.com"); err != nil || got.Status != 200 || string(got.Body) != "body" {
			t.Fatalf("Get = %+v/%v, want status 200 body \"body\"", got, err)
		}
		f.Err = boom
		if _, err := f.Get(ctx, "https://example.com"); !errors.Is(err, boom) {
			t.Fatalf("scripted error = %v, want boom", err)
		}
		if f.Calls() != 2 || len(f.URLs()) != 2 {
			t.Fatalf("Calls/URLs = %d/%d, want 2/2", f.Calls(), len(f.URLs()))
		}
	})

	t.Run("extract", func(t *testing.T) {
		f := &extract.Fake{Article: extract.Article{Title: "T", Mode: extract.ModeReadability}}
		if got, err := f.Extract(ctx, fetch.Resource{}); err != nil || got.Mode != extract.ModeReadability {
			t.Fatalf("Extract = %+v/%v, want mode readability", got, err)
		}
		f.Err = boom
		if _, err := f.Extract(ctx, fetch.Resource{}); !errors.Is(err, boom) {
			t.Fatalf("scripted error = %v, want boom", err)
		}
		if f.Calls() != 2 {
			t.Fatalf("Calls = %d, want 2", f.Calls())
		}
	})

	t.Run("embed", func(t *testing.T) {
		f := &embed.Fake{}
		if got, err := f.Embed(ctx, "hi"); err != nil || len(got) != embed.Dim {
			t.Fatalf("Embed len = %d/%v, want %d", len(got), err, embed.Dim)
		}
		f.Err = boom
		if _, err := f.Embed(ctx, "hi"); !errors.Is(err, boom) {
			t.Fatalf("scripted error = %v, want boom", err)
		}
		if f.Calls() != 2 {
			t.Fatalf("Calls = %d, want 2", f.Calls())
		}
	})

	t.Run("summarize", func(t *testing.T) {
		f := &summarize.Fake{Summary: summarize.Summary{Title: "T", Tags: []string{"go"}}}
		if got, err := f.Summarize(ctx, summarize.SummaryInput{}); err != nil || got.Title != "T" {
			t.Fatalf("Summarize = %+v/%v, want title T", got, err)
		}
		f.Err = boom
		if _, err := f.Summarize(ctx, summarize.SummaryInput{}); !errors.Is(err, boom) {
			t.Fatalf("scripted error = %v, want boom", err)
		}
		if f.Calls() != 2 {
			t.Fatalf("Calls = %d, want 2", f.Calls())
		}
	})

	t.Run("notify", func(t *testing.T) {
		f := &notify.Fake{}
		if err := f.Notify(ctx, notify.Email{To: "me@example.com"}); err != nil {
			t.Fatalf("Notify = %v, want nil", err)
		}
		f.Err = boom
		if err := f.Notify(ctx, notify.Email{To: "me@example.com"}); !errors.Is(err, boom) {
			t.Fatalf("scripted error = %v, want boom", err)
		}
		// A failed send is attempted but not recorded as sent — matching the pipeline's
		// "notify failure never fails a reading" policy.
		if f.Calls() != 2 || len(f.Sent()) != 1 {
			t.Fatalf("Calls/Sent = %d/%d, want 2/1", f.Calls(), len(f.Sent()))
		}
	})
}

// TestAcceptance_ReadingMetadataLifecycle exercises the phases 0-2 slice end to
// end through public APIs only: URL normalization -> source classification ->
// persist -> idempotency -> guarded status transitions with an injected clock ->
// search -> delete.
func TestAcceptance_ReadingMetadataLifecycle(t *testing.T) {
	for _, be := range storeBackends() {
		t.Run(be.name, func(t *testing.T) {
			runReadingMetadataLifecycle(t, be.factory(t)(t))
		})
	}
}

func runReadingMetadataLifecycle(t *testing.T, s store.Store) {
	t.Helper()
	ctx := context.Background()
	clk := clock.NewFake(time.Unix(1_700_000_000, 0).UTC())

	const raw = "HTTPS://Example.com/Posts/Hello/?utm_source=news&id=42#frag"
	key, err := reading.URLKey(raw)
	if err != nil {
		t.Fatalf("URLKey(%q): %v", raw, err)
	}
	if want := "https://example.com/Posts/Hello?id=42"; key != want {
		t.Fatalf("URLKey = %q, want %q", key, want)
	}
	if got := reading.ClassifySource(key); got != reading.SourceWeb {
		t.Fatalf("ClassifySource = %q, want %q", got, reading.SourceWeb)
	}

	created := clk.Now()
	r := reading.Reading{
		ID: "r1", URL: raw, URLKey: key,
		Status: reading.Pending, SourceKind: reading.SourceWeb,
		Title:     "Hello Kubernetes World",
		CreatedAt: created, UpdatedAt: created,
	}
	if err := s.SaveReading(ctx, r); err != nil {
		t.Fatalf("SaveReading: %v", err)
	}

	// Idempotency is DB-enforced on url_key: a different id, same key -> conflict.
	dup := r
	dup.ID = "r2"
	if err := s.SaveReading(ctx, dup); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate url_key save = %v, want ErrConflict", err)
	}

	// Transition gating is an explicit allow-table.
	if !reading.CanTransition(reading.Pending, reading.Running) {
		t.Fatal("pending->running must be allowed")
	}
	if reading.CanTransition(reading.Pending, reading.Ready) {
		t.Fatal("pending->ready must be rejected")
	}

	clk.Advance(time.Minute)
	started := clk.Now()
	if err := s.UpdateStatus(ctx, "r1", reading.Running, store.StatusFields{Now: started}); err != nil {
		t.Fatalf("UpdateStatus running: %v", err)
	}
	got, err := s.GetByID(ctx, "r1")
	if err != nil {
		t.Fatalf("GetByID running: %v", err)
	}
	if got.Status != reading.Running || got.StartedAt == nil || !got.StartedAt.Equal(started) || !got.UpdatedAt.Equal(started) {
		t.Fatalf("running snapshot = %+v, want Running started/updated %v", got, started)
	}

	clk.Advance(2 * time.Minute)
	finished := clk.Now()
	if err := s.UpdateStatus(ctx, "r1", reading.Ready, store.StatusFields{Now: finished}); err != nil {
		t.Fatalf("UpdateStatus ready: %v", err)
	}
	got, err = s.GetByID(ctx, "r1")
	if err != nil {
		t.Fatalf("GetByID ready: %v", err)
	}
	if got.Status != reading.Ready || got.FinishedAt == nil || !got.FinishedAt.Equal(finished) {
		t.Fatalf("ready snapshot = %+v, want Ready finished %v", got, finished)
	}

	page, err := s.Search(ctx, store.Query{Q: "kubernetes", Sort: store.SortNewest, Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Readings) != 1 || page.Total != 1 || page.Readings[0].ID != "r1" {
		t.Fatalf("Search kubernetes = %d rows (total %d), want single r1", len(page.Readings), page.Total)
	}

	if err := s.Delete(ctx, "r1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.GetByID(ctx, "r1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetByID after delete = %v, want ErrNotFound", err)
	}
}

// TestAcceptance_StaleAnnotationReadOverlay confirms the read-time overlay flips
// stale non-terminal readings to failed without mutating the input.
func TestAcceptance_StaleAnnotationReadOverlay(t *testing.T) {
	now := time.Unix(10_000, 0).UTC()
	ttls := reading.TTLs{Pending: 10 * time.Minute, Running: 30 * time.Minute}

	pending := reading.Reading{Status: reading.Pending, CreatedAt: now.Add(-11 * time.Minute)}
	if got := reading.AnnotateStale(pending, now, ttls); got.Status != reading.Failed || !strings.Contains(got.StaleReason, "timed out") {
		t.Fatalf("expired pending overlay = %q/%q, want failed/timed out", got.Status, got.StaleReason)
	}
	if pending.Status != reading.Pending || pending.StaleReason != "" {
		t.Fatalf("AnnotateStale mutated its input: %+v", pending)
	}

	startedStale := now.Add(-31 * time.Minute)
	running := reading.Reading{Status: reading.Running, StartedAt: &startedStale}
	if got := reading.AnnotateStale(running, now, ttls); got.Status != reading.Failed || !strings.Contains(got.StaleReason, "stalled") {
		t.Fatalf("stuck running overlay = %q/%q, want failed/stalled", got.Status, got.StaleReason)
	}

	ready := reading.Reading{Status: reading.Ready, CreatedAt: now.Add(-1000 * time.Hour)}
	if got := reading.AnnotateStale(ready, now, ttls); got.Status != reading.Ready || got.StaleReason != "" {
		t.Fatalf("ready overlay = %q/%q, want ready/unchanged", got.Status, got.StaleReason)
	}
}

// TestAcceptance_ClockDeterminism mirrors the plan's phase-0 deliverable test.
func TestAcceptance_ClockDeterminism(t *testing.T) {
	c := clock.NewFake(time.Unix(1000, 0))
	start := c.Now()
	c.Advance(90 * time.Second)
	if got := c.Now().Sub(start); got != 90*time.Second {
		t.Fatalf("Advance moved now %v, want 90s", got)
	}
	c.Set(time.Unix(5000, 0).UTC())
	if got := c.Now(); !got.Equal(time.Unix(5000, 0).UTC()) {
		t.Fatalf("Set now = %v, want 5000", got)
	}
}

// TestAcceptance_DispatcherLifecycle exercises the Phase 3 in-process dispatcher
// through its exported surface (PLAN.md §5): a submitted reading runs to ready, a
// rate limit re-dispatches without consuming an attempt, a spent retry budget
// fails retryably (and stays reprocessable), and the startup recovery sweep
// re-dispatches only the readings a crash would have stranded — each resuming at
// its stored attempt count. It runs inline with a FakeDelayer so every retry,
// backoff, and requeue path is deterministic: no goroutines, timers, or sleeps.
func TestAcceptance_DispatcherLifecycle(t *testing.T) {
	t.Run("SubmitProcessesToReady", func(t *testing.T) {
		st := store.NewMemory()
		clk := clock.NewFake(time.Unix(1_700_000_000, 0).UTC())
		seedDispatchReading(t, st, "r1", reading.Pending, 0, clk.Now(), nil)

		var calls int
		d := &dispatch.Dispatcher{
			Handler: func(context.Context, string) dispatch.Result {
				calls++
				return dispatch.Result{Outcome: dispatch.Done}
			},
			Store: st, Clock: clk, Delay: &dispatch.FakeDelayer{}, Max: 3, Inline: true,
		}

		d.Submit("r1")

		if calls != 1 {
			t.Fatalf("handler calls = %d, want 1", calls)
		}
		if got := mustGetReading(t, st, "r1"); got.Status != reading.Ready {
			t.Fatalf("status = %q, want ready", got.Status)
		}
	})

	t.Run("RateLimitRequeueKeepsAttempt", func(t *testing.T) {
		st := store.NewMemory()
		clk := clock.NewFake(time.Unix(1_700_000_000, 0).UTC())
		seedDispatchReading(t, st, "r1", reading.Pending, 0, clk.Now(), nil)

		delay := &dispatch.FakeDelayer{}
		var call int
		d := &dispatch.Dispatcher{
			Handler: func(context.Context, string) dispatch.Result {
				call++
				if call == 1 {
					return dispatch.Result{Outcome: dispatch.Requeue, After: 30 * time.Second}
				}
				return dispatch.Result{Outcome: dispatch.Done}
			},
			Store: st, Clock: clk, Delay: delay, Max: 3, Inline: true,
		}

		d.Submit("r1")

		// The rate limit schedules a re-dispatch after the upstream's delay and leaves
		// the reading pending without spending an attempt.
		if got := delay.Durations(); len(got) != 1 || got[0] != 30*time.Second {
			t.Fatalf("scheduled delays = %v, want [30s]", got)
		}
		if got := mustGetReading(t, st, "r1"); got.Status != reading.Pending || got.ProcessAttempts != 0 {
			t.Fatalf("after requeue = %q/attempts %d, want pending/0", got.Status, got.ProcessAttempts)
		}

		delay.FireAll()

		got := mustGetReading(t, st, "r1")
		if got.Status != reading.Ready {
			t.Fatalf("status after redispatch = %q, want ready", got.Status)
		}
		if got.ProcessAttempts != 0 {
			t.Fatalf("process_attempts = %d, want 0 (a rate limit must not consume an attempt)", got.ProcessAttempts)
		}
	})

	t.Run("RetryExhaustionFailsRetryable", func(t *testing.T) {
		st := store.NewMemory()
		clk := clock.NewFake(time.Unix(1_700_000_000, 0).UTC())
		seedDispatchReading(t, st, "r1", reading.Pending, 0, clk.Now(), nil)

		delay := &dispatch.FakeDelayer{}
		var calls int
		// Max=2: attempt 0 retries after backoff, attempt 1 spends the budget -> failed.
		d := &dispatch.Dispatcher{
			Handler: func(context.Context, string) dispatch.Result {
				calls++
				return dispatch.Result{Outcome: dispatch.Retry, Err: errors.New("upstream 503")}
			},
			Store: st, Clock: clk, Delay: delay, Max: 2, Inline: true,
		}

		d.Submit("r1")
		delay.FireAll()

		if calls != 2 {
			t.Fatalf("handler calls = %d, want 2", calls)
		}
		got := mustGetReading(t, st, "r1")
		if got.Status != reading.Failed {
			t.Fatalf("status = %q, want failed", got.Status)
		}
		if !strings.Contains(got.Error, "exhausted") {
			t.Fatalf("error = %q, want it to name the spent retry budget", got.Error)
		}
		if delay.PendingLen() != 0 {
			t.Fatalf("pending delays = %d, want 0 (no schedule once the budget is spent)", delay.PendingLen())
		}

		// A retry-exhausted reading stays reprocessable: a fresh dispatch runs it again.
		calls = 0
		d2 := &dispatch.Dispatcher{
			Handler: func(context.Context, string) dispatch.Result {
				calls++
				return dispatch.Result{Outcome: dispatch.Done}
			},
			Store: st, Clock: clk, Delay: &dispatch.FakeDelayer{}, Max: 2, Inline: true,
		}
		d2.Submit("r1")
		if reprocessed := mustGetReading(t, st, "r1"); calls != 1 || reprocessed.Status != reading.Ready {
			t.Fatalf("reprocess after failure: calls=%d status=%q, want 1/ready", calls, reprocessed.Status)
		}
	})

	t.Run("RecoverySweepReenqueuesNonTerminal", func(t *testing.T) {
		st := store.NewMemory()
		clk := clock.NewFake(time.Unix(1_700_000_000, 0).UTC())

		staleStart := clk.Now().Add(-31 * time.Minute)
		freshStart := clk.Now().Add(-5 * time.Minute)
		seedDispatchReading(t, st, "r-pending", reading.Pending, 0, clk.Now(), nil)
		seedDispatchReading(t, st, "r-running-stale", reading.Running, 0, staleStart, &staleStart)
		seedDispatchReading(t, st, "r-running-fresh", reading.Running, 0, freshStart, &freshStart)
		seedDispatchReading(t, st, "r-ready", reading.Ready, 0, clk.Now(), nil)
		seedDispatchReading(t, st, "r-failed", reading.Failed, 0, clk.Now(), nil)

		// Inline Sweep runs each recovered handler synchronously in one goroutine, so a
		// plain slice records the swept ids without a lock.
		var swept []string
		d := &dispatch.Dispatcher{
			Handler: func(_ context.Context, id string) dispatch.Result {
				swept = append(swept, id)
				return dispatch.Result{Outcome: dispatch.Done}
			},
			Store: st, Clock: clk, Delay: &dispatch.FakeDelayer{}, Max: 3, RunningTTL: 30 * time.Minute, Inline: true,
		}

		if err := d.Sweep(context.Background()); err != nil {
			t.Fatalf("Sweep: %v", err)
		}

		slices.Sort(swept)
		if want := []string{"r-pending", "r-running-stale"}; !slices.Equal(swept, want) {
			t.Fatalf("swept ids = %v, want %v (only pending + stale running)", swept, want)
		}
	})

	t.Run("SweepResumesAtStoredAttempt", func(t *testing.T) {
		st := store.NewMemory()
		clk := clock.NewFake(time.Unix(1_700_000_000, 0).UTC())
		seedDispatchReading(t, st, "r1", reading.Pending, 2, clk.Now(), nil) // process_attempts already 2

		delay := &dispatch.FakeDelayer{}
		d := &dispatch.Dispatcher{
			Handler: func(context.Context, string) dispatch.Result {
				return dispatch.Result{Outcome: dispatch.Retry}
			},
			Store: st, Clock: clk, Delay: delay, Max: 3, Inline: true,
		}

		// Resuming at attempt 2 with Max=3 spends the last of the budget on this one
		// run, so a single sweep fails it; a fresh attempt-0 start would re-dispatch
		// instead. That is how MAX_ATTEMPTS is honored across a restart.
		if err := d.Sweep(context.Background()); err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		got := mustGetReading(t, st, "r1")
		if got.Status != reading.Failed {
			t.Fatalf("status = %q, want failed (Max honored across restart)", got.Status)
		}
		if got.ProcessAttempts != 2 {
			t.Fatalf("process_attempts = %d, want 2", got.ProcessAttempts)
		}
		if delay.Total() != 0 {
			t.Fatalf("delays scheduled = %d, want 0 (the resumed run exhausts immediately)", delay.Total())
		}
	})
}

// TestAcceptance_DispatcherClassifiesErrors pins the shared error classifier
// (PLAN.md §5.1) that the dispatcher and the pipeline both use: a rate limit
// requeues carrying its delay, a permanent error fails, nil is success, and
// anything else is a transient retry. Wrapped and direct errors classify alike.
func TestAcceptance_DispatcherClassifiesErrors(t *testing.T) {
	if got := dispatch.Classify(nil); got.Outcome != dispatch.Done {
		t.Fatalf("Classify(nil) = %v, want Done", got.Outcome)
	}

	rl := dispatch.Classify(fmt.Errorf("embed: %w", &dispatch.RateLimitError{RetryAfter: 30 * time.Second}))
	if rl.Outcome != dispatch.Requeue || rl.After != 30*time.Second {
		t.Fatalf("Classify(rate limit) = %v/%v, want Requeue/30s", rl.Outcome, rl.After)
	}

	if got := dispatch.Classify(fmt.Errorf("reddit: %w", dispatch.ErrPermanent)); got.Outcome != dispatch.Fail {
		t.Fatalf("Classify(permanent) = %v, want Fail", got.Outcome)
	}

	if got := dispatch.Classify(errors.New("connection reset")); got.Outcome != dispatch.Retry {
		t.Fatalf("Classify(transient) = %v, want Retry", got.Outcome)
	}
}

// TestAcceptance_PipelineProcess proves the Phase 5 processing pipeline wired to
// fakes and driven through the in-process dispatcher: a web URL processes to
// ready with content/summary/similar persisted, a Reddit URL fails with
// guidance, and a rate-limited step requeues without spending an attempt.
func TestAcceptance_PipelineProcess(t *testing.T) {
	newPipeline := func() (*pipeline.Pipeline, *store.Memory, *clock.Fake, *embed.Fake, *notify.Fake) {
		st := store.NewMemory()
		clk := clock.NewFake(time.Unix(1_700_000_000, 0).UTC())
		emb := &embed.Fake{Vec: pipelineUnitVec()}
		ntf := &notify.Fake{}
		p := &pipeline.Pipeline{
			Store:      st,
			Blobs:      blobs.NewMemory(),
			Fetcher:    &fetch.Fake{Resource: fetch.Resource{Body: []byte("<html>hi</html>"), ContentType: "text/html", Status: 200}},
			Extractor:  &extract.Fake{Article: extract.Article{Title: "T", Author: "A", Markdown: "body", Mode: extract.ModeReadability, WordCount: 1}},
			Embedder:   emb,
			Vectors:    vector.NewMemory(),
			Summarizer: &summarize.Fake{Summary: summarize.Summary{Title: "Refined", Summary: "sum", Tags: []string{"go"}}},
			Notifier:   ntf,
			Clock:      clk,
			Config:     pipeline.Config{TopK: 5, NotifyEnabled: true, NotifyFrom: "a@b.c", NotifyTo: "d@e.f"},
		}
		return p, st, clk, emb, ntf
	}

	seed := func(t *testing.T, st *store.Memory, clk *clock.Fake, id, rawURL string) {
		t.Helper()
		key, err := reading.URLKey(rawURL)
		if err != nil {
			t.Fatalf("URLKey: %v", err)
		}
		r := reading.Reading{
			ID: id, URL: rawURL, URLKey: key,
			Status: reading.Pending, SourceKind: reading.ClassifySource(key),
			CreatedAt: clk.Now(), UpdatedAt: clk.Now(),
		}
		if err := st.SaveReading(context.Background(), r); err != nil {
			t.Fatalf("seed %q: %v", id, err)
		}
	}

	t.Run("WebProcessesToReady", func(t *testing.T) {
		p, st, clk, _, ntf := newPipeline()
		seed(t, st, clk, "r1", "https://example.com/post")
		d := &dispatch.Dispatcher{Handler: p.Process, Store: st, Clock: clk, Delay: &dispatch.FakeDelayer{}, Max: 3, Inline: true}

		d.Submit("r1")

		got := mustGetReading(t, st, "r1")
		if got.Status != reading.Ready {
			t.Fatalf("status = %q, want ready", got.Status)
		}
		if got.ExtractionMode != string(extract.ModeReadability) || got.Summary != "sum" || got.ContentKey == "" {
			t.Fatalf("content not persisted: mode=%q summary=%q content_key=%q", got.ExtractionMode, got.Summary, got.ContentKey)
		}
		if len(got.SimilarJSON) == 0 || len(got.DiagnosticsJSON) == 0 {
			t.Fatalf("similar/diagnostics not snapshotted: %s / %s", got.SimilarJSON, got.DiagnosticsJSON)
		}
		if len(ntf.Sent()) != 1 {
			t.Fatalf("notify sent = %d, want 1", len(ntf.Sent()))
		}
	})

	t.Run("RedditFailsWithGuidance", func(t *testing.T) {
		p, st, clk, _, _ := newPipeline()
		seed(t, st, clk, "r1", "https://www.reddit.com/r/golang/comments/x/y")
		d := &dispatch.Dispatcher{Handler: p.Process, Store: st, Clock: clk, Delay: &dispatch.FakeDelayer{}, Max: 3, Inline: true}

		d.Submit("r1")

		got := mustGetReading(t, st, "r1")
		if got.Status != reading.Failed {
			t.Fatalf("status = %q, want failed", got.Status)
		}
		if !strings.Contains(got.Error, pipeline.RedditGuidance) {
			t.Fatalf("error = %q, want the reddit guidance", got.Error)
		}
	})

	t.Run("RateLimitRequeuesWithoutAttempt", func(t *testing.T) {
		p, st, clk, emb, _ := newPipeline()
		emb.Err = &dispatch.RateLimitError{RetryAfter: 30 * time.Second}
		seed(t, st, clk, "r1", "https://example.com/post")
		delay := &dispatch.FakeDelayer{}
		d := &dispatch.Dispatcher{Handler: p.Process, Store: st, Clock: clk, Delay: delay, Max: 3, Inline: true}

		d.Submit("r1")

		got := mustGetReading(t, st, "r1")
		if got.Status != reading.Pending || got.ProcessAttempts != 0 {
			t.Fatalf("after rate limit = %q/attempts %d, want pending/0", got.Status, got.ProcessAttempts)
		}
		if ds := delay.Durations(); len(ds) != 1 || ds[0] != 30*time.Second {
			t.Fatalf("scheduled delays = %v, want [30s]", ds)
		}
	})
}

func pipelineUnitVec() []float32 {
	v := make([]float32, embed.Dim)
	v[0] = 1
	return v
}

// TestAcceptance_RealAdapters proves the Phase 6 production HTTP adapters through
// their public surfaces against httptest upstreams (PLAN §8): each composes the
// right request and maps an upstream failure to the SAME dispatch.Classify outcome
// the pipeline relies on, so the dispatcher and pipeline route real-world failures
// identically. The DB/object-store adapters (vector.Postgres, blobs.R2) carry no
// httptest surface — they are proven by their own contract / round-trip suites
// under -tags integration, plus the compile-time port conformance above.
func TestAcceptance_RealAdapters(t *testing.T) {
	ctx := context.Background()

	t.Run("EmbedClassifiesUpstreamErrors", func(t *testing.T) {
		cases := []struct {
			name    string
			status  int
			header  http.Header
			outcome dispatch.Outcome
		}{
			{"rate limit requeues", http.StatusTooManyRequests, http.Header{"Retry-After": {"30"}}, dispatch.Requeue},
			{"client error fails", http.StatusBadRequest, nil, dispatch.Fail},
			{"server error retries", http.StatusBadGateway, nil, dispatch.Retry},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					for k, vs := range c.header {
						for _, v := range vs {
							w.Header().Set(k, v)
						}
					}
					w.WriteHeader(c.status)
				}))
				defer srv.Close()

				_, err := embed.NewOpenAI("k", embed.WithBaseURL(srv.URL)).Embed(ctx, "x")
				if got := dispatch.Classify(err).Outcome; got != c.outcome {
					t.Fatalf("Classify(embed %d) = %v, want %v", c.status, got, c.outcome)
				}
			})
		}
	})

	t.Run("SummarizeForcedToolUse", func(t *testing.T) {
		var forced bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				ToolChoice struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"tool_choice"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			forced = body.ToolChoice.Type == "tool" && body.ToolChoice.Name == "emit_reading"
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","name":"emit_reading","input":{"title":"T","summary":"S","tags":["go"]}}]}`))
		}))
		defer srv.Close()

		out, err := summarize.NewAnthropic("k", summarize.WithBaseURL(srv.URL)).Summarize(ctx, summarize.SummaryInput{Markdown: "body"})
		if err != nil {
			t.Fatalf("Summarize: %v", err)
		}
		if !forced {
			t.Fatal("request did not force emit_reading tool use")
		}
		if out.Title != "T" || out.Summary != "S" || len(out.Tags) != 1 {
			t.Fatalf("summary = %+v, want fields parsed from the tool_use input", out)
		}

		// An incomplete tool input (the API forces the call but not the schema) must
		// be rejected, not persisted as a blank summary that marks the reading ready.
		blank := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","name":"emit_reading","input":{}}]}`))
		}))
		defer blank.Close()
		if _, err := summarize.NewAnthropic("k", summarize.WithBaseURL(blank.URL)).Summarize(ctx, summarize.SummaryInput{Markdown: "body"}); err == nil {
			t.Fatal("blank emit_reading input = nil error, want an error (no blank summary accepted)")
		}
	})

	t.Run("NotifyErrorIsSwallowable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		if err := notify.NewResend("k", notify.WithBaseURL(srv.URL)).Notify(ctx, notify.Email{To: "x@example.com"}); err == nil {
			t.Fatal("non-2xx notify = nil error, want an error (the pipeline swallows it, but the adapter must report it)")
		}
	})

	t.Run("FetchRateLimitRequeues", func(t *testing.T) {
		// A fetched 429 must classify as a Requeue (rate limit), not a permanent
		// Fail — a throttled origin should re-dispatch, not burn the reading.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "15")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer srv.Close()
		_, err := fetch.NewHTTP().Get(ctx, srv.URL)
		if got := dispatch.Classify(err); got.Outcome != dispatch.Requeue || got.After != 15*time.Second {
			t.Fatalf("Classify(fetch 429) = %v/%v, want Requeue/15s", got.Outcome, got.After)
		}

		// A bare 429 (no Retry-After) must requeue with a bounded, positive delay,
		// never zero — a zero delay would spin the dispatcher (requeue consumes no
		// attempt) on an origin that always rate-limits.
		bare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer bare.Close()
		_, err = fetch.NewHTTP().Get(ctx, bare.URL)
		if got := dispatch.Classify(err); got.Outcome != dispatch.Requeue || got.After != dispatch.DefaultRateLimitDelay {
			t.Fatalf("Classify(bare fetch 429) = %v/%v, want Requeue/DefaultRateLimitDelay", got.Outcome, got.After)
		}
	})

	t.Run("FetchGuardsSchemeAndSize", func(t *testing.T) {
		if _, err := fetch.NewHTTP().Get(ctx, "ftp://example.com/x"); !errors.Is(err, fetch.ErrUnsupportedScheme) {
			t.Fatalf("ftp scheme = %v, want ErrUnsupportedScheme", err)
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("x", 100)))
		}))
		defer srv.Close()
		_, err := fetch.NewHTTP(fetch.WithMaxBytes(10)).Get(ctx, srv.URL)
		if !errors.Is(err, fetch.ErrBodyTooLarge) {
			t.Fatalf("oversize body = %v, want ErrBodyTooLarge", err)
		}
		// An oversized body is permanent: the dispatcher must fail it, not retry it.
		if got := dispatch.Classify(err).Outcome; got != dispatch.Fail {
			t.Fatalf("Classify(oversize body) = %v, want Fail", got)
		}
	})
}

// TestAcceptance_Extraction proves the Phase 7 extraction internals through their
// public surfaces (PLAN §9): the readability tier ladder selects readability /
// raw_dom / raw_only by readerability, the YouTube oEmbed client returns the
// title/author floor (classifying its own upstream errors through dispatch.Classify),
// and the canonical Reddit guidance is the single string the pipeline reuses.
func TestAcceptance_Extraction(t *testing.T) {
	ctx := context.Background()

	t.Run("ReadabilityLadderSelectsTier", func(t *testing.T) {
		const articleHTML = `<!DOCTYPE html><html lang="en"><head><title>Indexing Notes | Site</title></head><body>
<nav><a href="/">Home</a></nav>
<article>
<h1>Indexing Notes</h1>
<p class="byline">By Cal Engineer</p>
<p>A single managed Postgres instance covers metadata, full-text search, and vector
similarity for a personal reading service. Keeping everything in one database means
backups are atomic and similarity ranking is just a join away.</p>
<p>The temptation to add a dedicated vector store should be resisted until the corpus
genuinely outgrows a single node. Until then, fewer moving parts means fewer outages
and a smaller bill, which is what a personal service actually wants.</p>
<p>When growth finally arrives, the ports in front of the store let the backend change
without touching the pipeline, so the simple choice today does not foreclose the
scalable choice tomorrow.</p>
</article>
<footer>copyright</footer>
</body></html>`
		const forumHTML = `<!DOCTYPE html><html lang="en"><head><title>Thread</title></head><body>
<div class="comments">
<div class="comment"><span>For a personal database the cheapest reliable backup is a nightly logical dump shipped to object storage, boring but restorable anywhere.</span></div>
<div class="comment"><span>Logical dumps are fine until the database is large; past a few gigabytes you want physical base backups plus WAL archiving for point in time recovery.</span></div>
</div></body></html>`
		const spaHTML = `<!DOCTYPE html><html lang="en"><head><title>App</title></head><body><div id="root"></div><script>function renderApp(){return 1;}</script></body></html>`

		ext := extract.NewReadability()
		cases := []struct {
			name string
			body string
			want extract.Mode
		}{
			{"readerable article", articleHTML, extract.ModeReadability},
			{"non-article salvages raw_dom", forumHTML, extract.ModeRawDOM},
			{"js-only floors to raw_only", spaHTML, extract.ModeRawOnly},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				got, err := ext.Extract(ctx, fetch.Resource{Body: []byte(c.body), Status: 200})
				if err != nil {
					t.Fatalf("Extract: %v", err)
				}
				if got.Mode != c.want {
					t.Fatalf("Mode = %q, want %q", got.Mode, c.want)
				}
				if strings.TrimSpace(got.Markdown) == "" {
					t.Fatalf("Mode %q produced empty markdown", got.Mode)
				}
			})
		}
	})

	t.Run("YouTubeOEmbedFloor", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/oembed" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if r.URL.Query().Get("format") != "json" {
				t.Errorf("oembed format = %q, want json", r.URL.Query().Get("format"))
			}
			_, _ = w.Write([]byte(`{"title":"A Talk","author_name":"Speaker","provider_name":"YouTube"}`))
		}))
		defer srv.Close()

		got, err := extract.NewYouTube(extract.WithYouTubeBaseURL(srv.URL)).
			Extract(ctx, "https://www.youtube.com/watch?v=abcdEFGHijk")
		if err != nil {
			t.Fatalf("YouTube Extract: %v", err)
		}
		if got.Title != "A Talk" || got.Author != "Speaker" || got.Site != "YouTube" {
			t.Fatalf("floor = %+v, want title/author/site from oEmbed", got)
		}
		if got.Mode != extract.ModeRawOnly {
			t.Fatalf("Mode = %q, want raw_only (oEmbed floor)", got.Mode)
		}

		// A deleted/private video's 404 oEmbed is a permanent failure for the dispatcher.
		gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer gone.Close()
		_, err = extract.NewYouTube(extract.WithYouTubeBaseURL(gone.URL)).
			Extract(ctx, "https://www.youtube.com/watch?v=abcdEFGHijk")
		if got := dispatch.Classify(err).Outcome; got != dispatch.Fail {
			t.Fatalf("Classify(oembed 404) = %v, want Fail", got)
		}
	})

	t.Run("RedditGuidanceIsTheSharedString", func(t *testing.T) {
		if extract.RedditGuidance == "" {
			t.Fatal("RedditGuidance is empty")
		}
		// The pipeline reuses the extract constant verbatim — one source of truth.
		if pipeline.RedditGuidance != extract.RedditGuidance {
			t.Fatalf("pipeline.RedditGuidance = %q, want extract.RedditGuidance %q", pipeline.RedditGuidance, extract.RedditGuidance)
		}
		// The classifier routes Reddit to the kind that triggers the guidance path.
		key, err := reading.URLKey("https://www.reddit.com/r/golang/comments/abc/post")
		if err != nil {
			t.Fatalf("URLKey: %v", err)
		}
		if got := reading.ClassifySource(key); got != reading.SourceReddit {
			t.Fatalf("ClassifySource = %q, want reddit", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Section D — conventions audit (source inspection)
// ---------------------------------------------------------------------------

// TestConventions_TestPackagesAreBlackbox asserts every _test.go file uses an
// external (_test) package. Add a justified white-box file to whiteboxAllowed.
func TestConventions_TestPackagesAreBlackbox(t *testing.T) {
	root := repoRoot(t)
	whiteboxAllowed := map[string]bool{
		// decide and backoff are unexported retry-decision internals; testing them
		// directly is the right boundary (the plan names this file specifically).
		"internal/dispatch/decide_test.go": true,
		// selectTier and the raw_only string helpers are the pure tier-selection
		// internals; the plan requires them "separately tested from the HTML
		// libraries", so testing them white-box is the right boundary.
		"internal/extract/ladder_test.go": true,
	}

	var violations []string
	for _, path := range goFiles(t, root, true) {
		if !strings.HasSuffix(path, "_test.go") {
			continue
		}
		rel := relPath(root, path)
		if whiteboxAllowed[rel] {
			continue
		}
		if pkg := packageName(t, path); !strings.HasSuffix(pkg, "_test") {
			violations = append(violations, rel+" (package "+pkg+")")
		}
	}
	if len(violations) > 0 {
		t.Fatalf("non-blackbox test packages found:\n  %s", strings.Join(violations, "\n  "))
	}
}

// TestConventions_ReadingIsStdlibOnly enforces that the pure domain core imports
// only the standard library (no third-party, no other internal packages).
func TestConventions_ReadingIsStdlibOnly(t *testing.T) {
	root := repoRoot(t)
	var violations []string
	for _, path := range goFiles(t, filepath.Join(root, "internal/reading"), false) {
		for _, imp := range fileImports(t, path) {
			if !isStdlib(imp) {
				violations = append(violations, relPath(root, path)+" imports "+imp)
			}
		}
	}
	if len(violations) > 0 {
		t.Fatalf("internal/reading must depend only on stdlib:\n  %s", strings.Join(violations, "\n  "))
	}
}

// TestConventions_DomainCoreNoNetworkOrRandom keeps the domain core and the
// in-memory fake free of network I/O and RNG (determinism). time is permitted:
// the memory store uses clock fallback, the domain uses time types only.
func TestConventions_DomainCoreNoNetworkOrRandom(t *testing.T) {
	root := repoRoot(t)
	forbidden := map[string]bool{
		"net/http": true, "math/rand": true, "math/rand/v2": true,
	}
	targets := []string{"internal/reading", "internal/store/memory.go", "internal/store/store.go"}

	var violations []string
	for _, target := range targets {
		abs := filepath.Join(root, target)
		var files []string
		if strings.HasSuffix(target, ".go") {
			files = []string{abs}
		} else {
			files = goFiles(t, abs, false)
		}
		for _, path := range files {
			for _, imp := range fileImports(t, path) {
				if forbidden[imp] {
					violations = append(violations, relPath(root, path)+" imports "+imp)
				}
			}
		}
	}
	if len(violations) > 0 {
		t.Fatalf("forbidden network/RNG imports in domain core:\n  %s", strings.Join(violations, "\n  "))
	}
}

// TestConventions_DockerStaysOutOfDefaultBuild enforces that anything pulling in
// testcontainers/Docker is build-tagged out of the default `make test` run — by
// the integration tag (the store integration suite) or the verify tag (this
// harness) — and that such tags are only ever applied to _test.go files. This is
// what keeps the default suite deterministic and Docker-free (CLAUDE.md).
func TestConventions_DockerStaysOutOfDefaultBuild(t *testing.T) {
	root := repoRoot(t)
	var integrationTagged, ungated, mislocated []string

	for _, path := range goFiles(t, root, true) {
		rel := relPath(root, path)
		tags := buildTags(t, path)
		gated := tags["integration"] || tags["verify"]
		if tags["integration"] {
			integrationTagged = append(integrationTagged, rel)
		}
		if gated && !strings.HasSuffix(path, "_test.go") {
			mislocated = append(mislocated, rel)
		}
		usesContainers := false
		for _, imp := range fileImports(t, path) {
			if strings.Contains(imp, "testcontainers") {
				usesContainers = true
				break
			}
		}
		if usesContainers && !gated {
			ungated = append(ungated, rel)
		}
	}

	if len(ungated) > 0 {
		t.Errorf("files importing testcontainers without an integration/verify build tag (would run in default `make test`):\n  %s",
			strings.Join(ungated, "\n  "))
	}
	if len(mislocated) > 0 {
		t.Errorf("integration/verify build tag on non-test files:\n  %s", strings.Join(mislocated, "\n  "))
	}
	if !slices.Contains(integrationTagged, "internal/store/postgres_test.go") {
		t.Errorf("expected internal/store/postgres_test.go to carry the integration tag; integration-tagged=%v", integrationTagged)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// seedDispatchReading persists a reading in a chosen lifecycle state so the
// dispatcher acceptance tests can drive Submit/Sweep against real store state.
func seedDispatchReading(t *testing.T, s store.Store, id string, status reading.Status, attempts int, created time.Time, startedAt *time.Time) {
	t.Helper()
	r := reading.Reading{
		ID: id, URL: "https://example.com/" + id, URLKey: "key-" + id,
		Status: status, SourceKind: reading.SourceWeb,
		ProcessAttempts: attempts, StartedAt: startedAt,
		CreatedAt: created, UpdatedAt: created,
	}
	if err := s.SaveReading(context.Background(), r); err != nil {
		t.Fatalf("seed %q: %v", id, err)
	}
}

func mustGetReading(t *testing.T, s store.Store, id string) reading.Reading {
	t.Helper()
	r, err := s.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetByID(%q): %v", id, err)
	}
	return r
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate repo root")
	}
	dir := filepath.Dir(file)
	for {
		if fileExists(filepath.Join(dir, "go.mod")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found above %s", filepath.Dir(file))
		}
		dir = parent
	}
}

func goBin(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	for _, root := range []string{runtime.GOROOT()} {
		if root == "" {
			continue
		}
		if cand := filepath.Join(root, "bin", "go"); fileExists(cand) {
			return cand
		}
	}
	t.Skip("go binary not found on PATH or in GOROOT")
	return ""
}

func gofmtBin(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("gofmt"); err == nil {
		return p
	}
	if root := runtime.GOROOT(); root != "" {
		if cand := filepath.Join(root, "bin", "gofmt"); fileExists(cand) {
			return cand
		}
	}
	t.Skip("gofmt not found on PATH or in GOROOT")
	return ""
}

func optionalTool(t *testing.T, name string) string {
	t.Helper()
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		if cand := filepath.Join(home, "go", "bin", name); fileExists(cand) {
			return cand
		}
	}
	t.Skipf("%s not found on PATH or in ~/go/bin; skipping", name)
	return ""
}

func runTool(t *testing.T, dir, name string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func goFiles(t *testing.T, dir string, includeTests bool) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "vendor" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if !includeTests && strings.HasSuffix(path, "_test.go") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	slices.Sort(out)
	return out
}

func packageName(t *testing.T, path string) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.PackageClauseOnly)
	if err != nil {
		t.Fatalf("parse package clause %s: %v", path, err)
	}
	return f.Name.Name
}

func fileImports(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse imports %s: %v", path, err)
	}
	out := make([]string, 0, len(f.Imports))
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("unquote import %s in %s: %v", spec.Path.Value, path, err)
		}
		out = append(out, p)
	}
	return out
}

// buildTags returns the set of identifiers named in a file's //go:build
// constraint (e.g. "//go:build verify && integration" -> {verify, integration}).
// Operators and parentheses are ignored. Only the header before the package
// clause is scanned.
func buildTags(t *testing.T, path string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	tags := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "package ") {
			break
		}
		expr, ok := strings.CutPrefix(line, "//go:build ")
		if !ok {
			continue
		}
		for _, tok := range strings.FieldsFunc(expr, func(r rune) bool {
			return !(r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r))
		}) {
			tags[tok] = true
		}
	}
	return tags
}

func assertGeneratedMatchesCommitted(t *testing.T, committedDir, genDir string) {
	t.Helper()
	committed := goBaseNames(t, committedDir)
	generated := goBaseNames(t, genDir)

	for _, name := range committed {
		want, err := os.ReadFile(filepath.Join(committedDir, name))
		if err != nil {
			t.Fatalf("read committed %s: %v", name, err)
		}
		got, err := os.ReadFile(filepath.Join(genDir, name))
		if err != nil {
			t.Errorf("sqlc no longer generates %s (committed file is stale); run `make sqlc`", name)
			continue
		}
		if !bytes.Equal(want, got) {
			t.Errorf("sqlc drift in %s: committed code differs from `sqlc generate`; run `make sqlc` and commit", name)
		}
	}
	for _, name := range generated {
		if !slices.Contains(committed, name) {
			t.Errorf("sqlc generates %s but it is not committed; run `make sqlc` and commit", name)
		}
	}
}

func goBaseNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out
}

func isStdlib(importPath string) bool {
	seg := importPath
	if i := strings.IndexByte(importPath, '/'); i >= 0 {
		seg = importPath[:i]
	}
	return !strings.Contains(seg, ".")
}

func relPath(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil {
		return rel
	}
	return path
}

func relTo(t *testing.T, base, target string) string {
	t.Helper()
	rel, err := filepath.Rel(base, target)
	if err != nil {
		t.Fatalf("rel %s -> %s: %v", base, target, err)
	}
	return rel
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
