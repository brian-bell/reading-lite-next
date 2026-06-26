// Package readerctl implements the operator command core.
package readerctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/bbell/reading-lite/internal/blobs"
	"github.com/bbell/reading-lite/internal/bookmarks"
	"github.com/bbell/reading-lite/internal/clock"
	"github.com/bbell/reading-lite/internal/reading"
	"github.com/bbell/reading-lite/internal/readingops"
	"github.com/bbell/reading-lite/internal/store"
	"github.com/bbell/reading-lite/internal/vector"
)

const pageLimit = 100
const defaultSmokeTimeout = 15 * time.Second

// Step is one external command a deploy or staging plan would run.
type Step struct {
	Label   string
	Command string
	Args    []string
	Dir     string
	Env     []string
}

// Runner applies deploy and staging steps.
type Runner interface {
	Run(ctx context.Context, step Step) error
}

// BlobInventory optionally lists stored blob keys for audit orphan checks.
type BlobInventory interface {
	ListBlobKeys(ctx context.Context) ([]string, error)
}

// VectorInventory optionally lists indexed vector ids for audit orphan checks.
type VectorInventory interface {
	ListVectorIDs(ctx context.Context) ([]string, error)
}

// Command holds injected dependencies for the readerctl command core.
type Command struct {
	Store           store.Store
	Blobs           blobs.Blobs
	Vector          vector.Index
	Dispatcher      readingops.Dispatcher
	Clock           clock.Clock
	TTLs            reading.TTLs
	NewID           func() string
	HTTPClient      *http.Client
	Runner          Runner
	BlobInventory   BlobInventory
	VectorInventory VectorInventory
	Stdout          io.Writer
	Stderr          io.Writer
}

// Main constructs only Phase-10-safe default dependencies and runs readerctl.
func Main(args []string, stdout, stderr io.Writer) int {
	cmd := &Command{
		HTTPClient: &http.Client{Timeout: defaultSmokeTimeout},
		Stdout:     stdout,
		Stderr:     stderr,
	}
	return cmd.Run(context.Background(), args)
}

// Run executes one readerctl command and returns a process exit code.
func (c *Command) Run(ctx context.Context, args []string) int {
	if c.Stdout == nil {
		c.Stdout = io.Discard
	}
	if c.Stderr == nil {
		c.Stderr = io.Discard
	}
	if len(args) == 0 {
		return c.usage("missing command")
	}
	if hasJSONFlag(args) && args[0] != "audit" && args[0] != "recover" {
		return c.configError("--json is only supported for audit and recover")
	}

	switch args[0] {
	case "import":
		return c.runImport(ctx, args[1:])
	case "audit":
		return c.runAudit(ctx, args[1:])
	case "recover":
		return c.runRecover(ctx, args[1:])
	case "drop":
		return c.runDrop(ctx, args[1:])
	case "smoke":
		return c.runSmoke(ctx, args[1:])
	case "deploy":
		return c.runDeploy(ctx, args[1:])
	case "staging":
		return c.runStaging(ctx, args[1:])
	default:
		return c.usage(fmt.Sprintf("unknown command %q", args[0]))
	}
}

func (c *Command) runImport(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return c.usage("import requires a subcommand")
	}

	switch args[0] {
	case "url":
		if err := c.requireURLImportDeps(); err != nil {
			return c.configError(err.Error())
		}
		return c.runImportURL(ctx, args[1:])
	case "markdown":
		if err := c.requireMarkdownImportDeps(); err != nil {
			return c.configError(err.Error())
		}
		return c.runImportMarkdown(ctx, args[1:])
	case "bookmarks":
		if err := c.requireURLImportDeps(); err != nil {
			return c.configError(err.Error())
		}
		return c.runImportBookmarks(ctx, args[1:])
	default:
		return c.usage(fmt.Sprintf("unknown import subcommand %q", args[0]))
	}
}

func (c *Command) runImportURL(ctx context.Context, args []string) int {
	fs := newFlagSet("import url")
	if err := fs.Parse(args); err != nil {
		return c.usage(err.Error())
	}
	if fs.NArg() != 1 {
		return c.usage("import url requires one URL")
	}
	rawURL := fs.Arg(0)
	label, err := c.preflightURL(ctx, rawURL)
	if err != nil {
		return c.validationOrRuntimeError(err)
	}
	res, err := c.ops().IngestURL(ctx, rawURL)
	if err != nil {
		return c.validationOrRuntimeError(err)
	}
	label = importResultLabel(label, res)
	_, _ = fmt.Fprintf(c.Stdout, "id=%s status=%s result=%s\n", res.ID, res.Status, label)
	return 0
}

func (c *Command) runImportMarkdown(ctx context.Context, args []string) int {
	file, url, title, tags, err := parseMarkdownArgs(args)
	if err != nil {
		return c.usage(err.Error())
	}
	if strings.TrimSpace(url) == "" {
		return c.usage("import markdown requires a file and --url")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return c.runtimeError(err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return c.usage("markdown is required")
	}
	label, err := c.preflightURL(ctx, url)
	if err != nil {
		return c.validationOrRuntimeError(err)
	}
	res, err := c.ops().ImportMarkdown(ctx, readingops.MarkdownImport{
		URL:      url,
		Markdown: string(data),
		Title:    title,
		Tags:     tags,
	})
	if err != nil {
		return c.validationOrRuntimeError(err)
	}
	label = importResultLabel(label, res)
	_, _ = fmt.Fprintf(c.Stdout, "id=%s status=%s result=%s\n", res.ID, res.Status, label)
	return 0
}

func (c *Command) runImportBookmarks(ctx context.Context, args []string) int {
	fs := newFlagSet("import bookmarks")
	if err := fs.Parse(args); err != nil {
		return c.usage(err.Error())
	}
	if fs.NArg() != 1 {
		return c.usage("import bookmarks requires a file")
	}
	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return c.runtimeError(err)
	}
	urls, err := bookmarks.Parse(data, "")
	if err != nil {
		return c.usage("invalid bookmark file")
	}
	if len(urls) == 0 {
		return c.usage("bookmarks are required")
	}
	results, err := c.ops().ImportBookmarks(ctx, urls)
	if err != nil {
		return c.runtimeError(err)
	}
	for _, res := range results {
		_, _ = fmt.Fprintf(c.Stdout, "url=%s id=%s result=%s\n", res.URL, res.ID, res.Result)
	}
	return 0
}

func (c *Command) runAudit(ctx context.Context, args []string) int {
	fs := newFlagSet("audit")
	jsonOut := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return c.usage(err.Error())
	}
	if fs.NArg() != 0 {
		return c.usage("audit takes no arguments")
	}
	if err := c.requireReadDeps(); err != nil {
		return c.configError(err.Error())
	}
	report, err := c.auditReport(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(c.Stderr, "audit: %v\n", err)
		return 1
	}
	if *jsonOut {
		_ = json.NewEncoder(c.Stdout).Encode(report)
		return 0
	}
	c.writeAuditText(report)
	return 0
}

func (c *Command) runRecover(ctx context.Context, args []string) int {
	fs := newFlagSet("recover")
	apply := fs.Bool("apply", false, "")
	jsonOut := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return c.usage(err.Error())
	}
	if fs.NArg() != 0 {
		return c.usage("recover takes no arguments")
	}
	if err := c.requireRecoverDeps(); err != nil {
		return c.configError(err.Error())
	}
	rows, err := c.scanReadings(ctx)
	if err != nil {
		return c.runtimeError(err)
	}
	results := make([]recoverItem, 0)
	exit := 0
	for _, r := range rows {
		annotated := reading.AnnotateStale(r, c.now(), c.ttls())
		if r.Status != reading.Failed && annotated.Status != reading.Failed {
			continue
		}
		item := recoverItem{ID: r.ID, Status: annotated.Status, Error: ""}
		if !*apply {
			item.Action = "would_reprocess"
			results = append(results, item)
			continue
		}
		res, err := c.ops().Reprocess(ctx, r.ID)
		if err != nil {
			item.Action = "failed"
			item.Error = err.Error()
			exit = 1
		} else {
			item.Action = "reprocessed"
			item.Status = res.Status
		}
		results = append(results, item)
	}
	out := recoverReport{DryRun: !*apply, Results: results}
	if *jsonOut {
		_ = json.NewEncoder(c.Stdout).Encode(out)
		return exit
	}
	_, _ = fmt.Fprintf(c.Stdout, "dry_run=%t\n", out.DryRun)
	for _, item := range out.Results {
		_, _ = fmt.Fprintf(c.Stdout, "id=%s action=%s status=%s error=%s\n", item.ID, item.Action, item.Status, item.Error)
	}
	return exit
}

func (c *Command) runDrop(ctx context.Context, args []string) int {
	ids, all, yes, err := parseDropArgs(args)
	if err != nil {
		return c.usage(err.Error())
	}
	if err := c.requireDropDeps(); err != nil {
		return c.configError(err.Error())
	}
	var targets []reading.Reading
	if all {
		if len(ids) != 0 {
			return c.usage("drop --all does not accept explicit IDs")
		}
		rows, err := c.scanReadings(ctx)
		if err != nil {
			return c.runtimeError(err)
		}
		targets = rows
	} else {
		ids := dedupeStrings(ids)
		if len(ids) == 0 {
			return c.usage("drop requires IDs or --all")
		}
		for _, id := range ids {
			r, err := c.Store.GetByID(ctx, id)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					_, _ = fmt.Fprintf(c.Stderr, "drop: missing id=%s\n", id)
					return 2
				}
				return c.runtimeError(err)
			}
			targets = append(targets, r)
		}
	}
	if !yes {
		_, _ = fmt.Fprintln(c.Stdout, "dry_run=true")
		for _, r := range targets {
			for _, artifact := range dropArtifacts(r) {
				if artifact.Key == "" {
					_, _ = fmt.Fprintf(c.Stdout, "id=%s delete=%s\n", r.ID, artifact.Kind)
				} else {
					_, _ = fmt.Fprintf(c.Stdout, "id=%s delete=%s key=%s\n", r.ID, artifact.Kind, artifact.Key)
				}
			}
		}
		return 0
	}

	_, _ = fmt.Fprintln(c.Stdout, "dry_run=false")
	exit := 0
	for _, r := range targets {
		if err := c.dropOne(ctx, r, all); err != nil {
			_, _ = fmt.Fprintf(c.Stdout, "id=%s result=failed error=%s\n", r.ID, err)
			exit = 1
			continue
		}
		_, _ = fmt.Fprintf(c.Stdout, "id=%s result=deleted error=\n", r.ID)
	}
	return exit
}

func (c *Command) runSmoke(ctx context.Context, args []string) int {
	fs := newFlagSet("smoke")
	baseURL := fs.String("base-url", "", "")
	ingestURL := fs.String("ingest-url", "", "")
	token := fs.String("token", "", "")
	tokenEnv := fs.String("token-env", "", "")
	if err := fs.Parse(args); err != nil {
		return c.usage(err.Error())
	}
	if fs.NArg() != 0 || strings.TrimSpace(*baseURL) == "" || strings.TrimSpace(*ingestURL) == "" {
		return c.usage("smoke requires --base-url and --ingest-url")
	}
	smokeToken := *token
	if *tokenEnv != "" {
		smokeToken = os.Getenv(*tokenEnv)
		if smokeToken == "" {
			return c.configError("smoke token environment variable is empty")
		}
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultSmokeTimeout}
	}
	base := strings.TrimRight(*baseURL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/healthz", nil)
	if err != nil {
		return c.runtimeError(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return c.runtimeError(err)
	}
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	if resp.StatusCode != http.StatusOK {
		return c.runtimeError(fmt.Errorf("healthz status %d", resp.StatusCode))
	}
	_, _ = fmt.Fprintln(c.Stdout, "healthz=ok")

	payload, _ := json.Marshal(map[string]string{"url": *ingestURL})
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/readings", bytes.NewReader(payload))
	if err != nil {
		return c.runtimeError(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if smokeToken != "" {
		req.Header.Set("Authorization", "Bearer "+smokeToken)
	}
	resp, err = client.Do(req)
	if err != nil {
		return c.runtimeError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return c.runtimeError(fmt.Errorf("ingest status %d", resp.StatusCode))
	}
	var out struct {
		ID     string         `json:"id"`
		Status reading.Status `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return c.runtimeError(err)
	}
	if out.ID == "" || out.Status == "" {
		return c.runtimeError(errors.New("ingest response missing id or status"))
	}
	_, _ = fmt.Fprintf(c.Stdout, "ingest=ok status=%d id=%s reading_status=%s\n", resp.StatusCode, out.ID, out.Status)
	return 0
}

func (c *Command) runDeploy(ctx context.Context, args []string) int {
	fs := newFlagSet("deploy")
	target := fs.String("target", "", "")
	baseURL := fs.String("base-url", "", "")
	smokeURL := fs.String("smoke-ingest-url", "", "")
	smokeTokenEnv := fs.String("smoke-token-env", "", "")
	image := fs.String("image", "", "")
	apply := fs.Bool("apply", false, "")
	if err := fs.Parse(args); err != nil {
		return c.usage(err.Error())
	}
	if fs.NArg() != 0 || !validDeployTarget(*target) || strings.TrimSpace(*baseURL) == "" || strings.TrimSpace(*smokeURL) == "" || strings.TrimSpace(*smokeTokenEnv) == "" {
		return c.usage("deploy requires --target local|staging|production, --base-url, --smoke-ingest-url, and --smoke-token-env")
	}
	steps := deploySteps(*baseURL, *smokeURL, *smokeTokenEnv, *image)
	return c.runPlan(ctx, *target, *apply, steps)
}

func (c *Command) runStaging(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return c.usage("staging requires an action")
	}
	action := args[0]
	fs := newFlagSet("staging " + action)
	target := fs.String("target", "", "")
	baseURL := fs.String("base-url", "", "")
	smokeURL := fs.String("smoke-ingest-url", "", "")
	smokeTokenEnv := fs.String("smoke-token-env", "", "")
	apply := fs.Bool("apply", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return c.usage(err.Error())
	}
	if fs.NArg() != 0 || strings.TrimSpace(*target) == "" {
		return c.usage("staging requires --target")
	}
	steps, err := stagingSteps(action, *target, *baseURL, *smokeURL, *smokeTokenEnv)
	if err != nil {
		return c.usage(err.Error())
	}
	if action != "down" && (strings.TrimSpace(*baseURL) == "" || strings.TrimSpace(*smokeURL) == "" || strings.TrimSpace(*smokeTokenEnv) == "") {
		return c.usage("staging up/promote require --base-url, --smoke-ingest-url, and --smoke-token-env")
	}
	return c.runPlan(ctx, *target, *apply, steps)
}

func (c *Command) runPlan(ctx context.Context, target string, apply bool, steps []Step) int {
	if !apply {
		_, _ = fmt.Fprintf(c.Stdout, "target=%s apply=false\n", target)
		for _, step := range steps {
			c.writeStep(step)
		}
		return 0
	}
	if c.Runner == nil {
		return c.configError("runner dependency is not configured")
	}
	_, _ = fmt.Fprintln(c.Stdout, "apply=true")
	for _, step := range steps {
		c.writeStep(step)
		if err := c.Runner.Run(ctx, step); err != nil {
			_, _ = fmt.Fprintf(c.Stderr, "run %s: %v\n", step.Label, err)
			return 1
		}
	}
	return 0
}

func (c *Command) writeStep(step Step) {
	args, _ := json.Marshal(step.Args)
	_, _ = fmt.Fprintf(c.Stdout, "step label=%s command=%s args=%s\n", step.Label, step.Command, args)
}

func (c *Command) preflightURL(ctx context.Context, rawURL string) (string, error) {
	key, err := reading.URLKey(rawURL)
	if err != nil {
		return "", err
	}
	existing, err := c.Store.GetByURLKey(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if existing.Status == reading.Failed {
		return "reprocessed", nil
	}
	return "existing", nil
}

func importResultLabel(preflight string, res readingops.StatusResult) string {
	if res.Created {
		return "created"
	}
	if preflight == "reprocessed" && res.Status == reading.Pending {
		return "reprocessed"
	}
	return "existing"
}

func (c *Command) ops() *readingops.Service {
	return &readingops.Service{
		Store:      c.Store,
		Blobs:      c.Blobs,
		Dispatcher: c.Dispatcher,
		Clock:      c.Clock,
		TTLs:       c.ttls(),
		NewID:      c.newID,
	}
}

func (c *Command) scanReadings(ctx context.Context) ([]reading.Reading, error) {
	var out []reading.Reading
	seen := map[string]bool{}
	var cursor store.Cursor
	for {
		page, err := c.Store.Search(ctx, store.Query{Sort: store.SortNewest, Limit: pageLimit, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, r := range page.Readings {
			if !seen[r.ID] {
				out = append(out, r)
				seen[r.ID] = true
			}
		}
		if !page.NextCursor.Valid {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

func (c *Command) auditReport(ctx context.Context) (auditReport, error) {
	rows, err := c.scanReadings(ctx)
	if err != nil {
		return auditReport{}, err
	}
	report := auditReport{
		StatusCounts:    statusCounts{},
		Stale:           []staleItem{},
		MissingBlobs:    []missingBlobItem{},
		OrphanedBlobs:   orphanReport{Status: "not_checked", Keys: []string{}},
		OrphanedVectors: orphanVectorReport{Status: "not_checked", IDs: []string{}},
	}
	referencedBlobs := map[string]bool{}
	for _, r := range rows {
		report.StatusCounts.increment(r.Status)
		annotated := reading.AnnotateStale(r, c.now(), c.ttls())
		if (r.Status == reading.Pending || r.Status == reading.Running) && annotated.Status == reading.Failed {
			report.Stale = append(report.Stale, staleItem{ID: r.ID, StoredStatus: r.Status, Reason: annotated.StaleReason})
		}
		for _, ref := range blobRefs(r) {
			if referencedBlobs[ref.Key] {
				continue
			}
			referencedBlobs[ref.Key] = true
			_, _, err := c.Blobs.Get(ctx, ref.Key)
			if errors.Is(err, blobs.ErrNotFound) {
				report.MissingBlobs = append(report.MissingBlobs, missingBlobItem{ID: r.ID, Field: ref.Field, Key: ref.Key})
				continue
			}
			if err != nil {
				return auditReport{}, fmt.Errorf("blob check failed: %w", err)
			}
		}
	}
	if c.BlobInventory != nil {
		keys, err := c.BlobInventory.ListBlobKeys(ctx)
		if err != nil {
			return auditReport{}, err
		}
		report.OrphanedBlobs.Status = "checked"
		report.OrphanedBlobs.Keys = orphanedStrings(keys, referencedBlobs)
	}
	report.OrphanedBlobs.Count = len(report.OrphanedBlobs.Keys)
	if c.VectorInventory != nil {
		ids, err := c.VectorInventory.ListVectorIDs(ctx)
		if err != nil {
			return auditReport{}, err
		}
		referencedIDs := map[string]bool{}
		for _, r := range rows {
			referencedIDs[r.ID] = true
		}
		report.OrphanedVectors.Status = "checked"
		report.OrphanedVectors.IDs = orphanedStrings(ids, referencedIDs)
	}
	report.OrphanedVectors.Count = len(report.OrphanedVectors.IDs)
	return report, nil
}

func (c *Command) writeAuditText(report auditReport) {
	_, _ = fmt.Fprintf(c.Stdout, "status pending=%d running=%d ready=%d failed=%d\n",
		report.StatusCounts.Pending,
		report.StatusCounts.Running,
		report.StatusCounts.Ready,
		report.StatusCounts.Failed,
	)
	for _, item := range report.Stale {
		_, _ = fmt.Fprintf(c.Stdout, "stale id=%s stored_status=%s reason=%s\n", item.ID, item.StoredStatus, item.Reason)
	}
	for _, item := range report.MissingBlobs {
		_, _ = fmt.Fprintf(c.Stdout, "missing_blob id=%s field=%s key=%s\n", item.ID, item.Field, item.Key)
	}
	for _, key := range report.OrphanedBlobs.Keys {
		_, _ = fmt.Fprintf(c.Stdout, "orphaned_blob key=%s\n", key)
	}
	if report.OrphanedBlobs.Status == "checked" {
		_, _ = fmt.Fprintf(c.Stdout, "orphaned blobs: count=%d\n", report.OrphanedBlobs.Count)
	} else {
		_, _ = fmt.Fprintln(c.Stdout, "orphaned blobs: not checked")
	}
	for _, id := range report.OrphanedVectors.IDs {
		_, _ = fmt.Fprintf(c.Stdout, "orphaned_vector id=%s\n", id)
	}
	if report.OrphanedVectors.Status == "checked" {
		_, _ = fmt.Fprintf(c.Stdout, "orphaned vectors: count=%d\n", report.OrphanedVectors.Count)
	} else {
		_, _ = fmt.Fprintln(c.Stdout, "orphaned vectors: not checked")
	}
}

func (c *Command) dropOne(ctx context.Context, r reading.Reading, all bool) error {
	var errs []error
	for _, artifact := range dropArtifacts(r) {
		switch artifact.Kind {
		case "raw", "content":
			if err := c.Blobs.Delete(ctx, artifact.Key); err != nil {
				errs = append(errs, err)
			}
		case "vector":
			if err := c.Vector.Delete(ctx, r.ID); err != nil {
				errs = append(errs, err)
			}
		}
		if len(errs) > 0 {
			return errors.Join(errs...)
		}
	}
	if err := c.Store.Delete(ctx, r.ID); err != nil {
		if all && errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	return nil
}

func (c *Command) requireReadDeps() error {
	if c.Store == nil {
		return errors.New("store dependency is not configured")
	}
	if c.Blobs == nil {
		return errors.New("blobs dependency is not configured")
	}
	return nil
}

func (c *Command) requireURLImportDeps() error {
	if c.Store == nil {
		return errors.New("store dependency is not configured")
	}
	if c.Dispatcher == nil {
		return errors.New("dispatcher dependency is not configured")
	}
	if c.NewID == nil {
		return errors.New("id generator dependency is not configured")
	}
	return nil
}

func (c *Command) requireMarkdownImportDeps() error {
	if err := c.requireURLImportDeps(); err != nil {
		return err
	}
	if c.Blobs == nil {
		return errors.New("blobs dependency is not configured")
	}
	return nil
}

func (c *Command) requireRecoverDeps() error {
	if c.Store == nil {
		return errors.New("store dependency is not configured")
	}
	if c.Dispatcher == nil {
		return errors.New("dispatcher dependency is not configured")
	}
	return nil
}

func (c *Command) requireDropDeps() error {
	if err := c.requireReadDeps(); err != nil {
		return err
	}
	if c.Vector == nil {
		return errors.New("vector dependency is not configured")
	}
	return nil
}

func (c *Command) now() time.Time {
	if c.Clock == nil {
		return time.Now().UTC()
	}
	return c.Clock.Now()
}

func (c *Command) ttls() reading.TTLs {
	if c.TTLs.Pending == 0 && c.TTLs.Running == 0 {
		return reading.TTLs{Pending: time.Hour, Running: time.Hour}
	}
	return c.TTLs
}

func (c *Command) newID() string {
	if c.NewID == nil {
		return ""
	}
	return c.NewID()
}

func (c *Command) usage(msg string) int {
	if msg != "" {
		_, _ = fmt.Fprintln(c.Stderr, msg)
	}
	_, _ = fmt.Fprintln(c.Stderr, "usage: readerctl <import|audit|recover|drop|smoke|deploy|staging> ...")
	return 2
}

func (c *Command) configError(msg string) int {
	_, _ = fmt.Fprintln(c.Stderr, msg)
	return 2
}

func (c *Command) runtimeError(err error) int {
	_, _ = fmt.Fprintln(c.Stderr, err)
	return 1
}

func (c *Command) validationOrRuntimeError(err error) int {
	if errors.Is(err, reading.ErrInvalidURL) {
		return c.usage("invalid reading url")
	}
	return c.runtimeError(err)
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func hasJSONFlag(args []string) bool {
	return slices.Contains(args, "--json")
}

func parseMarkdownArgs(args []string) (file, url, title string, tags []string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--url", "--title", "--tag":
			if i+1 >= len(args) {
				return "", "", "", nil, fmt.Errorf("%s requires a value", arg)
			}
			i++
			switch arg {
			case "--url":
				url = args[i]
			case "--title":
				title = args[i]
			case "--tag":
				tags = append(tags, args[i])
			}
		default:
			if strings.HasPrefix(arg, "-") {
				return "", "", "", nil, fmt.Errorf("unsupported flag %s", arg)
			}
			if file != "" {
				return "", "", "", nil, errors.New("import markdown requires one file")
			}
			file = arg
		}
	}
	if file == "" {
		return "", "", "", nil, errors.New("import markdown requires a file and --url")
	}
	return file, url, title, tags, nil
}

func parseDropArgs(args []string) (ids []string, all, yes bool, err error) {
	for _, arg := range args {
		switch arg {
		case "--all":
			all = true
		case "--yes":
			yes = true
		default:
			if strings.HasPrefix(arg, "-") {
				return nil, false, false, fmt.Errorf("unsupported flag %s", arg)
			}
			ids = append(ids, arg)
		}
	}
	return ids, all, yes, nil
}

func validDeployTarget(target string) bool {
	switch target {
	case "local", "staging", "production":
		return true
	default:
		return false
	}
}

func deploySteps(baseURL, smokeURL, smokeTokenEnv, image string) []Step {
	steps := []Step{
		{Label: "go build", Command: "go", Args: []string{"build", "./..."}},
		{Label: "go test", Command: "go", Args: []string{"test", "./..."}},
	}
	if image != "" {
		steps = append(steps,
			Step{Label: "docker build", Command: "docker", Args: []string{"build", "-t", image, "."}},
			Step{Label: "docker push", Command: "docker", Args: []string{"push", image}},
		)
	}
	return append(steps, smokeStep(baseURL, smokeURL, smokeTokenEnv))
}

func stagingSteps(action, target, baseURL, smokeURL, smokeTokenEnv string) ([]Step, error) {
	switch action {
	case "up":
		return []Step{
			{Label: "staging up", Command: "stagingctl", Args: []string{"provision", target}},
			smokeStep(baseURL, smokeURL, smokeTokenEnv),
		}, nil
	case "down":
		return []Step{{Label: "staging down", Command: "stagingctl", Args: []string{"stop", target}}}, nil
	case "promote":
		return []Step{
			{Label: "staging promote", Command: "stagingctl", Args: []string{"promote", target}},
			smokeStep(baseURL, smokeURL, smokeTokenEnv),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported staging action %q", action)
	}
}

func smokeStep(baseURL, smokeURL, tokenEnv string) Step {
	return Step{
		Label:   "smoke",
		Command: "readerctl",
		Args:    []string{"smoke", "--base-url", baseURL, "--ingest-url", smokeURL, "--token-env", tokenEnv},
	}
}

func dropArtifacts(r reading.Reading) []dropArtifact {
	var out []dropArtifact
	seenKeys := map[string]bool{}
	for _, ref := range blobRefs(r) {
		if seenKeys[ref.Key] {
			continue
		}
		seenKeys[ref.Key] = true
		out = append(out, dropArtifact{Kind: ref.Field, Key: ref.Key})
	}
	return append(out,
		dropArtifact{Kind: "vector"},
		dropArtifact{Kind: "metadata"},
	)
}

func blobRefs(r reading.Reading) []blobRef {
	var out []blobRef
	if r.RawKey != "" {
		out = append(out, blobRef{Field: "raw", Key: r.RawKey})
	}
	if r.ContentKey != "" {
		out = append(out, blobRef{Field: "content", Key: r.ContentKey})
	}
	return out
}

func orphanedStrings(candidates []string, referenced map[string]bool) []string {
	out := make([]string, 0)
	seen := map[string]bool{}
	for _, v := range candidates {
		if v == "" || referenced[v] || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func dedupeStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

type blobRef struct {
	Field string
	Key   string
}

type dropArtifact struct {
	Kind string
	Key  string
}

type auditReport struct {
	StatusCounts    statusCounts       `json:"status_counts"`
	Stale           []staleItem        `json:"stale"`
	MissingBlobs    []missingBlobItem  `json:"missing_blobs"`
	OrphanedBlobs   orphanReport       `json:"orphaned_blobs"`
	OrphanedVectors orphanVectorReport `json:"orphaned_vectors"`
}

type statusCounts struct {
	Pending int `json:"pending"`
	Running int `json:"running"`
	Ready   int `json:"ready"`
	Failed  int `json:"failed"`
}

func (c *statusCounts) increment(status reading.Status) {
	switch status {
	case reading.Pending:
		c.Pending++
	case reading.Running:
		c.Running++
	case reading.Ready:
		c.Ready++
	case reading.Failed:
		c.Failed++
	}
}

type staleItem struct {
	ID           string         `json:"id"`
	StoredStatus reading.Status `json:"stored_status"`
	Reason       string         `json:"reason"`
}

type missingBlobItem struct {
	ID    string `json:"id"`
	Field string `json:"field"`
	Key   string `json:"key"`
}

type orphanReport struct {
	Status string   `json:"status"`
	Keys   []string `json:"keys"`
	Count  int      `json:"count"`
}

type orphanVectorReport struct {
	Status string   `json:"status"`
	IDs    []string `json:"ids"`
	Count  int      `json:"count"`
}

type recoverReport struct {
	DryRun  bool          `json:"dry_run"`
	Results []recoverItem `json:"results"`
}

type recoverItem struct {
	ID     string         `json:"id"`
	Action string         `json:"action"`
	Status reading.Status `json:"status"`
	Error  string         `json:"error"`
}
