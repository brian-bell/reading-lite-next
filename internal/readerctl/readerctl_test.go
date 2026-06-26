package readerctl_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bbell/reading-lite/internal/blobs"
	"github.com/bbell/reading-lite/internal/clock"
	"github.com/bbell/reading-lite/internal/readerctl"
	"github.com/bbell/reading-lite/internal/reading"
	"github.com/bbell/reading-lite/internal/store"
	"github.com/bbell/reading-lite/internal/vector"
)

func TestRun_UnknownCommandPrintsUsageAndReturnsNonZero(t *testing.T) {
	t.Parallel()

	cmd, _, stderr := newCommand(t)
	if code := cmd.Run(context.Background(), []string{"bogus"}); code != 2 {
		t.Fatalf("Run() = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "usage: readerctl") {
		t.Fatalf("stderr = %q, want usage", stderr.String())
	}
}

func TestRun_ImportURLPreflightsFailedAsReprocessedAndPrintsStatusLine(t *testing.T) {
	t.Parallel()

	cmd, stdout, _ := newCommand(t)
	ctx := context.Background()
	failed := readingAt("old", "https://example.com/article", reading.Failed, time.Unix(1, 0))
	saveReading(t, cmd.Store, failed)

	if code := cmd.Run(ctx, []string{"import", "url", "https://example.com/article"}); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	want := "id=old status=pending result=reprocessed\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestRun_ImportURLPreflightFailedButServiceReturnsReadyPrintsExisting(t *testing.T) {
	t.Parallel()

	cmd, stdout, _ := newCommand(t)
	failed := readingAt("same", "https://example.com/race", reading.Failed, time.Unix(1, 0))
	ready := failed
	ready.Status = reading.Ready
	cmd.Store = &flappingURLStore{
		Store:  cmd.Store,
		first:  failed,
		second: ready,
	}

	if code := cmd.Run(context.Background(), []string{"import", "url", "https://example.com/race"}); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	want := "id=same status=ready result=existing\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestRun_ImportURLRejectsInvalidURLAsValidationError(t *testing.T) {
	t.Parallel()

	cmd, _, stderr := newCommand(t)
	if code := cmd.Run(context.Background(), []string{"import", "url", "notaurl"}); code != 2 {
		t.Fatalf("Run() = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "invalid reading url") {
		t.Fatalf("stderr = %q, want invalid URL validation", stderr.String())
	}
}

func TestRun_ImportMarkdownCreatesRawBlobAndPrintsStatusLine(t *testing.T) {
	t.Parallel()

	cmd, stdout, _ := newCommand(t)
	path := tempFile(t, "body.md", "# Title\n\nBody")
	if code := cmd.Run(context.Background(), []string{"import", "markdown", path, "--url", "https://example.com/md", "--title", "Title", "--tag", "go", "--tag", "read"}); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "id=id-1 status=pending result=created") {
		t.Fatalf("stdout = %q, want created status line", stdout.String())
	}
	got, err := cmd.Store.GetByID(context.Background(), "id-1")
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	data, ctype, err := cmd.Blobs.Get(context.Background(), got.RawKey)
	if err != nil {
		t.Fatalf("Blobs.Get() error = %v", err)
	}
	if string(data) != "# Title\n\nBody" || ctype != "text/markdown" {
		t.Fatalf("raw blob = %q %q, want markdown body/text type", string(data), ctype)
	}
}

func TestRun_ImportMarkdownRejectsEmptyFile(t *testing.T) {
	t.Parallel()

	cmd, _, stderr := newCommand(t)
	path := tempFile(t, "empty.md", " \n")
	if code := cmd.Run(context.Background(), []string{"import", "markdown", path, "--url", "https://example.com/md"}); code != 2 {
		t.Fatalf("Run() = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "markdown is required") {
		t.Fatalf("stderr = %q, want markdown validation", stderr.String())
	}
}

func TestRun_ImportMarkdownRejectsInvalidURLAsValidationError(t *testing.T) {
	t.Parallel()

	cmd, _, stderr := newCommand(t)
	path := tempFile(t, "body.md", "body")
	if code := cmd.Run(context.Background(), []string{"import", "markdown", path, "--url", "notaurl"}); code != 2 {
		t.Fatalf("Run() = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "invalid reading url") {
		t.Fatalf("stderr = %q, want invalid URL validation", stderr.String())
	}
}

func TestRun_ImportBookmarksPrintsCreatedExistingInvalidLines(t *testing.T) {
	t.Parallel()

	cmd, stdout, _ := newCommand(t)
	saveReading(t, cmd.Store, readingAt("existing", "https://existing.test", reading.Ready, time.Unix(1, 0)))
	path := tempFile(t, "bookmarks.json", `[{"url":"https://new.test"},{"url":"https://existing.test"},{"url":"notaurl"}]`)

	if code := cmd.Run(context.Background(), []string{"import", "bookmarks", path}); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	want := strings.Join([]string{
		"url=https://new.test id=id-1 result=created",
		"url=https://existing.test id=existing result=existing",
		"url=notaurl id= result=invalid",
		"",
	}, "\n")
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestRun_ImportBookmarksAllInvalidStillReturnsZero(t *testing.T) {
	t.Parallel()

	cmd, stdout, _ := newCommand(t)
	path := tempFile(t, "bookmarks.html", `<a href="notaurl">bad</a>`)
	if code := cmd.Run(context.Background(), []string{"import", "bookmarks", path}); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "result=invalid") {
		t.Fatalf("stdout = %q, want invalid item", stdout.String())
	}
}

func TestRun_ImportURLAndBookmarksDoNotRequireBlobs(t *testing.T) {
	t.Parallel()

	cmd, stdout, _ := newCommand(t)
	cmd.Blobs = nil
	if code := cmd.Run(context.Background(), []string{"import", "url", "https://example.com/no-blobs"}); code != 0 {
		t.Fatalf("import url Run() = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "result=created") {
		t.Fatalf("stdout = %q, want created import", stdout.String())
	}

	stdout.Reset()
	path := tempFile(t, "bookmarks.json", `[{"url":"https://example.com/bookmark-no-blobs"}]`)
	if code := cmd.Run(context.Background(), []string{"import", "bookmarks", path}); code != 0 {
		t.Fatalf("import bookmarks Run() = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "result=created") {
		t.Fatalf("stdout = %q, want created bookmark", stdout.String())
	}
}

func TestRun_ImportMarkdownRequiresBlobs(t *testing.T) {
	t.Parallel()

	cmd, _, stderr := newCommand(t)
	cmd.Blobs = nil
	path := tempFile(t, "body.md", "body")
	if code := cmd.Run(context.Background(), []string{"import", "markdown", path, "--url", "https://example.com/md"}); code != 2 {
		t.Fatalf("Run() = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "blobs dependency is not configured") {
		t.Fatalf("stderr = %q, want blobs dependency error", stderr.String())
	}
}

func TestRun_JSONFlagRejectedOutsideAuditAndRecover(t *testing.T) {
	t.Parallel()

	cmd, _, stderr := newCommand(t)
	if code := cmd.Run(context.Background(), []string{"import", "url", "--json", "https://example.com"}); code != 2 {
		t.Fatalf("Run() = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--json is only supported for audit and recover") {
		t.Fatalf("stderr = %q, want json scope error", stderr.String())
	}
}

func TestRun_AuditTextReportsStatusStaleMissingBlobAndInventoryLines(t *testing.T) {
	t.Parallel()

	cmd, stdout, _ := newCommand(t)
	now := time.Unix(10_000, 0)
	cmd.Clock = clock.NewFake(now)
	cmd.BlobInventory = inventoryBlobs{"orphan-blob"}
	cmd.VectorInventory = inventoryVectors{"orphan-vector"}
	saveReading(t, cmd.Store, readingAt("ready", "https://ready.test", reading.Ready, now.Add(-4*time.Hour)))
	saveReading(t, cmd.Store, readingAt("pending", "https://pending.test", reading.Pending, now.Add(-2*time.Hour)))
	saveReading(t, cmd.Store, readingWithKeys("missing", "https://missing.test", reading.Ready, now.Add(-time.Hour), "raw-missing", "content-missing"))

	if code := cmd.Run(context.Background(), []string{"audit"}); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"status pending=1 running=0 ready=2 failed=0",
		"stale id=pending stored_status=pending reason=timed out before processing after 1h0m0s",
		"missing_blob id=missing field=raw key=raw-missing",
		"missing_blob id=missing field=content key=content-missing",
		"orphaned_blob key=orphan-blob",
		"orphaned blobs: count=1",
		"orphaned_vector id=orphan-vector",
		"orphaned vectors: count=1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, want line containing %q", out, want)
		}
	}
}

func TestRun_AuditJSONReportsCompleteSchema(t *testing.T) {
	t.Parallel()

	cmd, stdout, _ := newCommand(t)
	saveReading(t, cmd.Store, readingAt("r1", "https://r1.test", reading.Ready, time.Unix(1, 0)))
	if code := cmd.Run(context.Background(), []string{"audit", "--json"}); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	for _, want := range []string{
		`"status_counts":{"pending":0,"running":0,"ready":1,"failed":0}`,
		`"stale":[]`,
		`"missing_blobs":[]`,
		`"orphaned_blobs":{"status":"not_checked","keys":[],"count":0}`,
		`"orphaned_vectors":{"status":"not_checked","ids":[],"count":0}`,
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout = %q, want JSON fragment %q", stdout.String(), want)
		}
	}
}

func TestRun_RecoverDryRunTargetsFailedAndStaleOnlyInUnifiedScanOrder(t *testing.T) {
	t.Parallel()

	cmd, stdout, _ := newCommand(t)
	now := time.Unix(10_000, 0)
	cmd.Clock = clock.NewFake(now)
	saveReading(t, cmd.Store, readingAt("fresh-running", "https://fresh-running.test", reading.Running, now.Add(-time.Minute)))
	saveReading(t, cmd.Store, readingAt("failed", "https://failed.test", reading.Failed, now.Add(-2*time.Minute)))
	saveReading(t, cmd.Store, readingAt("stale-pending", "https://stale-pending.test", reading.Pending, now.Add(-3*time.Hour)))
	saveReading(t, cmd.Store, readingAt("ready", "https://ready.test", reading.Ready, now.Add(-4*time.Hour)))

	if code := cmd.Run(context.Background(), []string{"recover"}); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	want := strings.Join([]string{
		"dry_run=true",
		"id=failed action=would_reprocess status=failed error=",
		"id=stale-pending action=would_reprocess status=failed error=",
		"",
	}, "\n")
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestRun_RecoverApplyContinuesAfterPerIDFailure(t *testing.T) {
	t.Parallel()

	cmd, stdout, _ := newCommand(t)
	now := time.Unix(10_000, 0)
	cmd.Clock = clock.NewFake(now)
	saveReading(t, cmd.Store, readingAt("fail", "https://fail.test", reading.Pending, now.Add(-3*time.Hour)))
	saveReading(t, cmd.Store, readingAt("ok", "https://ok.test", reading.Failed, now.Add(-2*time.Hour)))
	cmd.Dispatcher = &dispatchRecorder{forceErrByID: map[string]error{"fail": errors.New("boom")}}

	if code := cmd.Run(context.Background(), []string{"recover", "--apply"}); code != 1 {
		t.Fatalf("Run() = %d, want 1", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "id=fail action=failed status=failed error=boom") {
		t.Fatalf("stdout = %q, want failed item", out)
	}
	if !strings.Contains(out, "id=ok action=reprocessed status=pending error=") {
		t.Fatalf("stdout = %q, want continued success", out)
	}
}

func TestRun_RecoverApplyDoesNotRequireIDGenerator(t *testing.T) {
	t.Parallel()

	cmd, stdout, _ := newCommand(t)
	cmd.NewID = nil
	saveReading(t, cmd.Store, readingAt("failed", "https://failed.test", reading.Failed, time.Unix(1, 0)))

	if code := cmd.Run(context.Background(), []string{"recover", "--apply"}); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "id=failed action=reprocessed status=pending error=") {
		t.Fatalf("stdout = %q, want reprocessed item", stdout.String())
	}
}

func TestRun_RecoverApplyDoesNotRequireBlobs(t *testing.T) {
	t.Parallel()

	cmd, stdout, _ := newCommand(t)
	cmd.Blobs = nil
	saveReading(t, cmd.Store, readingAt("failed", "https://failed-noblobs.test", reading.Failed, time.Unix(1, 0)))

	if code := cmd.Run(context.Background(), []string{"recover", "--apply"}); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "id=failed action=reprocessed status=pending error=") {
		t.Fatalf("stdout = %q, want reprocessed item", stdout.String())
	}
}

func TestRun_DropWithoutYesIsDryRunAndDoesNotMutate(t *testing.T) {
	t.Parallel()

	cmd, stdout, _ := newCommand(t)
	saveReading(t, cmd.Store, readingWithKeys("r1", "https://r1.test", reading.Ready, time.Unix(1, 0), "raw", "content"))

	if code := cmd.Run(context.Background(), []string{"drop", "r1"}); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	if _, err := cmd.Store.GetByID(context.Background(), "r1"); err != nil {
		t.Fatalf("dry-run deleted reading: %v", err)
	}
	for _, want := range []string{
		"dry_run=true",
		"id=r1 delete=raw key=raw",
		"id=r1 delete=content key=content",
		"id=r1 delete=vector",
		"id=r1 delete=metadata",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout = %q, want %q", stdout.String(), want)
		}
	}
}

func TestRun_DropExplicitMissingIDAbortsBeforeMutation(t *testing.T) {
	t.Parallel()

	cmd, _, stderr := newCommand(t)
	saveReading(t, cmd.Store, readingAt("keep", "https://keep.test", reading.Ready, time.Unix(1, 0)))
	if code := cmd.Run(context.Background(), []string{"drop", "keep", "missing", "--yes"}); code != 2 {
		t.Fatalf("Run() = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "drop: missing id=missing") {
		t.Fatalf("stderr = %q, want missing id", stderr.String())
	}
	if _, err := cmd.Store.GetByID(context.Background(), "keep"); err != nil {
		t.Fatalf("drop mutated before preflight completed: %v", err)
	}
}

func TestRun_DropYesDeletesRawContentVectorThenMetadata(t *testing.T) {
	t.Parallel()

	cmd, stdout, _ := newCommand(t)
	cmd.Vector = vector.NewMemory()
	saveReading(t, cmd.Store, readingWithKeys("r1", "https://r1.test", reading.Ready, time.Unix(1, 0), "raw", "content"))
	putBlob(t, cmd.Blobs, "raw")
	putBlob(t, cmd.Blobs, "content")
	if err := cmd.Vector.Upsert(context.Background(), "r1", make([]float32, vector.Dim)); err != nil {
		t.Fatalf("Vector.Upsert() error = %v", err)
	}

	if code := cmd.Run(context.Background(), []string{"drop", "r1", "--yes"}); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	if _, err := cmd.Store.GetByID(context.Background(), "r1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetByID() error = %v, want ErrNotFound", err)
	}
	if got := stdout.String(); got != "dry_run=false\nid=r1 result=deleted error=\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestRun_SmokePostsIngestJSONAndValidatesResponse(t *testing.T) {
	t.Parallel()

	var sawAuth, sawContentType bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/healthz":
			if r.Method != http.MethodGet {
				t.Fatalf("health method = %s, want GET", r.Method)
			}
			w.WriteHeader(http.StatusOK)
		case "/api/readings":
			if r.Method != http.MethodPost {
				t.Fatalf("ingest method = %s, want POST", r.Method)
			}
			sawAuth = r.Header.Get("Authorization") == "Bearer secret"
			sawContentType = r.Header.Get("Content-Type") == "application/json"
			_, _ = fmt.Fprint(w, `{"id":"r1","status":"pending"}`)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd, stdout, _ := newCommand(t)
	if code := cmd.Run(context.Background(), []string{"smoke", "--base-url", server.URL + "///", "--ingest-url", "https://example.com", "--token", "secret"}); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	if !sawAuth || !sawContentType {
		t.Fatalf("headers auth=%v content-type=%v, want true true", sawAuth, sawContentType)
	}
	if !strings.Contains(stdout.String(), "healthz=ok\ningest=ok status=200 id=r1 reading_status=pending") {
		t.Fatalf("stdout = %q, want smoke success", stdout.String())
	}
}

func TestRun_SmokeReadsTokenFromEnvironment(t *testing.T) {
	t.Setenv("READERCTL_TEST_TOKEN", "from-env")
	var sawAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/healthz":
			w.WriteHeader(http.StatusOK)
		case "/api/readings":
			sawAuth = r.Header.Get("Authorization")
			_, _ = fmt.Fprint(w, `{"id":"r1","status":"pending"}`)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd, _, _ := newCommand(t)
	if code := cmd.Run(context.Background(), []string{"smoke", "--base-url", server.URL, "--ingest-url", "https://example.com", "--token-env", "READERCTL_TEST_TOKEN"}); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	if sawAuth != "Bearer from-env" {
		t.Fatalf("Authorization = %q, want bearer from env", sawAuth)
	}
}

func TestRun_DeployApplyRunsExactStepsAndStopsOnFailure(t *testing.T) {
	t.Parallel()

	runner := &runnerRecorder{failLabel: "go test"}
	cmd, stdout, stderr := newCommand(t)
	cmd.Runner = runner
	code := cmd.Run(context.Background(), []string{"deploy", "--target", "staging", "--base-url", "https://api.test", "--smoke-ingest-url", "https://smoke.test", "--smoke-token-env", "READER_API_TOKEN", "--image", "app:1", "--apply"})
	if code != 1 {
		t.Fatalf("Run() = %d, want 1", code)
	}
	wantSteps := []readerctl.Step{
		{Label: "go build", Command: "go", Args: []string{"build", "./..."}},
		{Label: "go test", Command: "go", Args: []string{"test", "./..."}},
	}
	if !reflect.DeepEqual(runner.steps, wantSteps) {
		t.Fatalf("steps = %#v, want %#v", runner.steps, wantSteps)
	}
	if !strings.Contains(stdout.String(), "apply=true\nstep label=go build command=go args=[\"build\",\"./...\"]\nstep label=go test command=go args=[\"test\",\"./...\"]") {
		t.Fatalf("stdout = %q, want apply steps", stdout.String())
	}
	if !strings.Contains(stderr.String(), "run go test: step failed") {
		t.Fatalf("stderr = %q, want failure", stderr.String())
	}
}

func TestRun_StagingDownDoesNotRequireSmokeInputs(t *testing.T) {
	t.Parallel()

	cmd, stdout, _ := newCommand(t)
	if code := cmd.Run(context.Background(), []string{"staging", "down", "--target", "branch-1"}); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	want := "target=branch-1 apply=false\nstep label=staging down command=stagingctl args=[\"stop\",\"branch-1\"]\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestMain_DefaultDeployApplyRefusesWithoutRunner(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := readerctl.Main([]string{"deploy", "--target", "local", "--base-url", "https://api.test", "--smoke-ingest-url", "https://smoke.test", "--smoke-token-env", "READER_API_TOKEN", "--apply"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("Main() = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "runner dependency is not configured") {
		t.Fatalf("stderr = %q, want dependency error", stderr.String())
	}
}

func newCommand(t *testing.T) (*readerctl.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()

	var stdout, stderr bytes.Buffer
	cmd := &readerctl.Command{
		Store:      store.NewMemory(),
		Blobs:      blobs.NewMemory(),
		Vector:     vector.NewMemory(),
		Dispatcher: &dispatchRecorder{},
		Clock:      clock.NewFake(time.Unix(5_000, 0)),
		TTLs:       reading.TTLs{Pending: time.Hour, Running: time.Hour},
		NewID:      idSeq(),
		Stdout:     &stdout,
		Stderr:     &stderr,
	}
	return cmd, &stdout, &stderr
}

func idSeq() func() string {
	n := 0
	return func() string {
		n++
		return fmt.Sprintf("id-%d", n)
	}
}

func tempFile(t *testing.T, name, body string) string {
	t.Helper()
	path := t.TempDir() + "/" + name
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

func saveReading(t *testing.T, s store.Store, r reading.Reading) {
	t.Helper()
	if r.URLKey == "" {
		key, err := reading.URLKey(r.URL)
		if err != nil {
			t.Fatalf("URLKey() error = %v", err)
		}
		r.URLKey = key
	}
	if err := s.SaveReading(context.Background(), r); err != nil {
		t.Fatalf("SaveReading(%s) error = %v", r.ID, err)
	}
}

func readingAt(id, rawURL string, status reading.Status, createdAt time.Time) reading.Reading {
	return reading.Reading{
		ID:         id,
		URL:        rawURL,
		Status:     status,
		SourceKind: reading.SourceWeb,
		CreatedAt:  createdAt,
		UpdatedAt:  createdAt,
	}
}

func readingWithKeys(id, rawURL string, status reading.Status, createdAt time.Time, rawKey, contentKey string) reading.Reading {
	r := readingAt(id, rawURL, status, createdAt)
	r.RawKey = rawKey
	r.ContentKey = contentKey
	return r
}

func putBlob(t *testing.T, b blobs.Blobs, key string) {
	t.Helper()
	if err := b.Put(context.Background(), key, []byte(key), "text/plain"); err != nil {
		t.Fatalf("Put(%s) error = %v", key, err)
	}
}

type dispatchRecorder struct {
	submitted    []string
	forceErrByID map[string]error
}

func (d *dispatchRecorder) Submit(id string) {
	d.submitted = append(d.submitted, id)
}

type flappingURLStore struct {
	store.Store
	calls  int
	first  reading.Reading
	second reading.Reading
}

func (s *flappingURLStore) GetByURLKey(context.Context, string) (reading.Reading, error) {
	s.calls++
	if s.calls == 1 {
		return s.first, nil
	}
	return s.second, nil
}

func (d *dispatchRecorder) ForceSubmitAfter(ctx context.Context, id string, beforeQueue func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := d.forceErrByID[id]; err != nil {
		return err
	}
	if err := beforeQueue(); err != nil {
		return err
	}
	d.Submit(id)
	return nil
}

type inventoryBlobs []string

func (i inventoryBlobs) ListBlobKeys(context.Context) ([]string, error) {
	return append([]string(nil), i...), nil
}

type inventoryVectors []string

func (i inventoryVectors) ListVectorIDs(context.Context) ([]string, error) {
	return append([]string(nil), i...), nil
}

type runnerRecorder struct {
	steps     []readerctl.Step
	failLabel string
}

func (r *runnerRecorder) Run(ctx context.Context, step readerctl.Step) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.steps = append(r.steps, step)
	if step.Label == r.failLabel {
		return errors.New("step failed")
	}
	return nil
}
