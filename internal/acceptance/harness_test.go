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
	"errors"
	"go/parser"
	"go/token"
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

	"github.com/bbell/reading-lite/internal/clock"
	"github.com/bbell/reading-lite/internal/reading"
	"github.com/bbell/reading-lite/internal/store"
	"github.com/bbell/reading-lite/internal/store/storetest"
)

// --- Compile-time port conformance (Section C: ports have the expected shape) ---

var (
	_ store.Store = (*store.Memory)(nil)
	_ store.Store = (*store.Postgres)(nil)
	_ clock.Clock = clock.System{}
	_ clock.Clock = (*clock.Fake)(nil)
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
	// Proves the integration-tagged store tests still compile (plan step A5).
	root, goBin := repoRoot(t), goBin(t)
	if out, err := runTool(t, root, goBin, "vet", "-tags", "integration", "./internal/store/"); err != nil {
		t.Fatalf("go vet -tags integration ./internal/store/ failed: %v\n%s", err, out)
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

// ---------------------------------------------------------------------------
// Section D — conventions audit (source inspection)
// ---------------------------------------------------------------------------

// TestConventions_TestPackagesAreBlackbox asserts every _test.go file uses an
// external (_test) package. Add a justified white-box file to whiteboxAllowed.
func TestConventions_TestPackagesAreBlackbox(t *testing.T) {
	root := repoRoot(t)
	whiteboxAllowed := map[string]bool{}

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
