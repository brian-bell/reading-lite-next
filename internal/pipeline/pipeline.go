// Package pipeline orchestrates the processing of a submitted reading: it
// acquires the source content, extracts and indexes it, finds similar past
// readings, summarizes it, and optionally notifies — all behind ports so the
// whole flow is testable against fakes with zero I/O.
//
// [Pipeline.Process] is the dispatcher's Handler: it returns a [dispatch.Result]
// the dispatcher maps to the reading's lifecycle status. The pipeline owns the
// reading's content fields; the dispatcher owns its status.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"strings"

	"github.com/bbell/reading-lite/internal/blobs"
	"github.com/bbell/reading-lite/internal/clock"
	"github.com/bbell/reading-lite/internal/dispatch"
	"github.com/bbell/reading-lite/internal/embed"
	"github.com/bbell/reading-lite/internal/extract"
	"github.com/bbell/reading-lite/internal/fetch"
	"github.com/bbell/reading-lite/internal/notify"
	"github.com/bbell/reading-lite/internal/reading"
	"github.com/bbell/reading-lite/internal/store"
	"github.com/bbell/reading-lite/internal/summarize"
	"github.com/bbell/reading-lite/internal/vector"
)

// Store is the narrow persistence surface the pipeline needs.
type Store interface {
	GetByID(ctx context.Context, id string) (reading.Reading, error)
	UpdateContent(ctx context.Context, id string, fields store.ContentFields) error
	ReplaceTags(ctx context.Context, id string, tags []string) error
}

// Config tunes pipeline behavior.
type Config struct {
	// TopK bounds how many similar readings to retrieve.
	TopK int
	// NotifyEnabled turns the final notification email on.
	NotifyEnabled bool
	// NotifyFrom is the notification sender address.
	NotifyFrom string
	// NotifyTo is the notification recipient address.
	NotifyTo string
}

// Pipeline processes one reading at a time through the full ingest flow.
type Pipeline struct {
	Store      Store
	Blobs      blobs.Blobs
	Fetcher    fetch.Fetcher
	Extractor  extract.Extractor
	Embedder   embed.Embedder
	Vectors    vector.Index
	Summarizer summarize.Summarizer
	// Notifier is optional; when nil (or Config.NotifyEnabled is false) the
	// notification step is skipped.
	Notifier notify.Notifier
	Clock    clock.Clock
	Config   Config
}

// RedditGuidance is the operator-facing message recorded when a Reddit URL is
// submitted. Reddit blocks automated fetching, so the reading fails permanently
// with instructions to import the content another way.
const RedditGuidance = "reddit cannot be fetched automatically; export the post or comment and import it as markdown"

// SimilarItem is one hydrated similar reading snapshotted into similar_json.
type SimilarItem struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
	Title string  `json:"title,omitempty"`
	URL   string  `json:"url,omitempty"`
}

// diagnostics records per-step pipeline outcomes, persisted as diagnostics_json.
type diagnostics struct {
	Source         string `json:"source"`
	ExtractionMode string `json:"extraction_mode,omitempty"`
	SimilarCount   int    `json:"similar_count"`
	Reused         bool   `json:"reused,omitempty"`
	NotifyError    string `json:"notify_error,omitempty"`
}

// content is the result of acquiring, extracting, and indexing a source.
type content struct {
	title       string
	author      string
	site        string
	lang        string
	wordCount   int
	mode        string
	markdown    string
	contentKey  string
	rawKey      string
	similarJSON json.RawMessage
}

// Process runs the full pipeline for one reading and reports the outcome. The
// dispatcher has already marked the reading running; on success the dispatcher
// marks it ready after this returns Done.
func (p *Pipeline) Process(ctx context.Context, id string) dispatch.Result {
	r, err := p.Store.GetByID(ctx, id)
	if err != nil {
		return dispatch.Classify(err)
	}

	if r.SourceKind == reading.SourceReddit {
		// Reddit blocks automated fetching: fail permanently with guidance rather
		// than burning the retry budget on a request that can never succeed.
		return dispatch.Result{Outcome: dispatch.Fail, Err: fmt.Errorf("%w: %s", dispatch.ErrPermanent, RedditGuidance)}
	}

	diag := diagnostics{Source: string(r.SourceKind)}

	// Idempotent re-entry: a non-empty content_key means a prior run already
	// acquired, extracted, embedded, and indexed this reading and checkpointed
	// the result. Resume from the stored content instead of redoing that work —
	// this is what makes a summarize-step Retry cheap and safe.
	if r.ContentKey != "" {
		c, err := p.reuse(ctx, r, &diag)
		if err != nil {
			return dispatch.Classify(err)
		}
		return p.summarizeAndFinish(ctx, r, c, diag)
	}

	c, err := p.acquire(ctx, r, &diag)
	if err != nil {
		return dispatch.Classify(err)
	}

	// Checkpoint the acquired content before the (separately retryable)
	// summarize step, so a summarize failure does not force re-fetching,
	// re-extracting, and re-embedding on the retried run.
	if err := p.Store.UpdateContent(ctx, id, p.contentFields(c, diag, summarize.Summary{})); err != nil {
		return dispatch.Classify(err)
	}

	return p.summarizeAndFinish(ctx, r, c, diag)
}

// summarizeAndFinish runs the summarize step once, sends the optional
// notification, and persists the final content and tags. It is the shared tail
// of a fresh run and an idempotent re-entry.
//
// Summarize runs once per Process call (PLAN §7's "summarize once" is within a
// run): a retry after a post-summarize failure (e.g. the ReplaceTags write) does
// re-summarize. That is intentional and spec-permitted; the content checkpoint
// deliberately stops before summarize so the cheap steps are skipped on retry,
// not the LLM call. Checkpointing the summary too is a possible future cost
// optimization, not a correctness requirement.
func (p *Pipeline) summarizeAndFinish(ctx context.Context, r reading.Reading, c content, diag diagnostics) dispatch.Result {
	sum, err := p.Summarizer.Summarize(ctx, summarize.SummaryInput{
		Title:    c.title,
		Author:   c.author,
		Site:     c.site,
		URL:      r.URL,
		Markdown: c.markdown,
	})
	if err != nil {
		return dispatch.Classify(err)
	}

	// Persist content and tags durably BEFORE notifying. If a persist fails
	// transiently the dispatcher re-dispatches the reading, so notifying earlier
	// could send a "ready" email for a still-pending reading — and a duplicate on
	// the retry. Notify only once the writes that gate readiness have succeeded.
	if err := p.Store.UpdateContent(ctx, r.ID, p.contentFields(c, diag, sum)); err != nil {
		return dispatch.Classify(err)
	}
	if err := p.Store.ReplaceTags(ctx, r.ID, sum.Tags); err != nil {
		return dispatch.Classify(err)
	}

	// Best-effort notify after the durable content/tag writes; a failure never
	// fails the reading, but it is recorded in diagnostics with one more best-effort
	// write.
	//
	// Notification is at-least-once. The terminal `ready` status is written by the
	// dispatcher after Process returns (intentionally best-effort, recovered by the
	// startup sweep + read-time stale annotation, PLAN §1.4/§1.7), so on the rare
	// path where that status write drops, a later sweep reprocess can re-notify.
	// PLAN §7 requires only "notify, and a notify failure must not fail the reading"
	// — not exactly-once delivery — so for this single-instance personal service a
	// rare duplicate is acceptable; idempotent/exactly-once delivery belongs to the
	// real Notifier adapter (Phase 6) and lifecycle hardening (Phase 11).
	if p.notifyBestEffort(ctx, r, sum, &diag) {
		_ = p.Store.UpdateContent(ctx, r.ID, p.contentFields(c, diag, sum))
	}

	return dispatch.Result{Outcome: dispatch.Done}
}

// reuse reconstructs the content from a prior run's checkpoint: the extracted
// markdown comes from the content blob, the rest from the persisted reading.
func (p *Pipeline) reuse(ctx context.Context, r reading.Reading, diag *diagnostics) (content, error) {
	data, _, err := p.Blobs.Get(ctx, r.ContentKey)
	if err != nil {
		return content{}, err
	}
	diag.Reused = true
	diag.ExtractionMode = r.ExtractionMode
	diag.SimilarCount = countItems(r.SimilarJSON)
	return content{
		title:       r.Title,
		author:      r.Author,
		site:        r.Site,
		lang:        r.Lang,
		wordCount:   r.WordCount,
		mode:        r.ExtractionMode,
		markdown:    string(data),
		contentKey:  r.ContentKey,
		rawKey:      r.RawKey,
		similarJSON: r.SimilarJSON,
	}, nil
}

// acquire produces the content to index, dispatching on source kind: markdown
// imports skip fetch/extract and read the stored body; everything else (web and
// YouTube) fetches and extracts. YouTube has no dedicated branch here — its
// oEmbed floor and transcript handling live in the Phase 7 extractor adapter, so
// in Phase 5 a YouTube URL flows through the same fetch+extract path as web (it
// is fetchable, unlike Reddit). Both paths share the embed/upsert/query/blob
// tail in index.
func (p *Pipeline) acquire(ctx context.Context, r reading.Reading, diag *diagnostics) (content, error) {
	if r.SourceKind == reading.SourceMarkdown {
		return p.acquireMarkdown(ctx, r, diag)
	}
	return p.acquireFetched(ctx, r, diag)
}

// acquireFetched fetches and extracts a web (or YouTube) source, then indexes it.
func (p *Pipeline) acquireFetched(ctx context.Context, r reading.Reading, diag *diagnostics) (content, error) {
	res, err := p.Fetcher.Get(ctx, r.URL)
	if err != nil {
		return content{}, err
	}
	if err := classifyFetchStatus(res.Status); err != nil {
		return content{}, err
	}

	article, err := p.Extractor.Extract(ctx, res)
	if err != nil {
		return content{}, err
	}

	c := content{
		title:      article.Title,
		author:     article.Author,
		site:       article.Site,
		lang:       article.Lang,
		wordCount:  article.WordCount,
		mode:       string(article.Mode),
		markdown:   article.Markdown,
		contentKey: contentKey(r.ID),
		rawKey:     rawKey(r.ID),
	}
	diag.ExtractionMode = c.mode

	if err := p.Blobs.Put(ctx, c.rawKey, res.Body, res.ContentType); err != nil {
		return content{}, err
	}
	if err := p.index(ctx, r, &c, diag); err != nil {
		return content{}, err
	}
	return c, nil
}

// acquireMarkdown reads the markdown body stored at ingest (the raw blob) and
// indexes it without fetching or extracting.
func (p *Pipeline) acquireMarkdown(ctx context.Context, r reading.Reading, diag *diagnostics) (content, error) {
	if r.RawKey == "" {
		return content{}, fmt.Errorf("%w: markdown reading has no stored body", dispatch.ErrPermanent)
	}
	data, _, err := p.Blobs.Get(ctx, r.RawKey)
	if err != nil {
		return content{}, err
	}

	c := content{
		markdown:   string(data),
		contentKey: contentKey(r.ID),
		rawKey:     r.RawKey,
		// A markdown import is not extracted from a fetched page, so it has no
		// extraction tier: mode is intentionally left empty (the {readability,
		// raw_dom, raw_only} tiers describe extracted web content only).
	}
	if err := p.index(ctx, r, &c, diag); err != nil {
		return content{}, err
	}
	return c, nil
}

// index embeds the content, upserts and queries the vector index to find similar
// readings, snapshots them, and writes the extracted content blob. It is the
// shared tail of every acquire path.
func (p *Pipeline) index(ctx context.Context, r reading.Reading, c *content, diag *diagnostics) error {
	vec, err := p.Embedder.Embed(ctx, embedText(*c))
	if err != nil {
		return err
	}
	if err := p.Vectors.Upsert(ctx, r.ID, vec); err != nil {
		return err
	}

	matches, err := p.Vectors.Query(ctx, vec, p.topK(), r.ID)
	if err != nil {
		return err
	}
	similar := p.hydrate(ctx, matches)
	diag.SimilarCount = len(similar)
	c.similarJSON, err = marshalSimilar(similar)
	if err != nil {
		return err
	}

	// Write the content blob before the caller persists content_key (the re-entry
	// signal). This order is load-bearing: a crash between the two leaves an orphan
	// blob but no dangling pointer, so a retried run cleanly redoes acquisition.
	return p.Blobs.Put(ctx, c.contentKey, []byte(c.markdown), "text/markdown")
}

// hydrate turns vector matches into snapshot items by loading each match's
// title and URL from the store. A match is skipped when its reading row cannot
// be loaded (e.g. it was deleted while its vector lingered) or is not ready: the
// pipeline upserts a vector before the reading reaches ready, so a pending or
// retry-exhausted reading can still carry an indexed vector, and only successfully
// processed readings belong in a similarity snapshot. One bad match never fails
// the run; similar_json holds only displayable, ready entries.
func (p *Pipeline) hydrate(ctx context.Context, matches []vector.Match) []SimilarItem {
	out := make([]SimilarItem, 0, len(matches))
	for _, m := range matches {
		r, err := p.Store.GetByID(ctx, m.ID)
		if err != nil || r.Status != reading.Ready {
			continue
		}
		out = append(out, SimilarItem{ID: m.ID, Score: m.Score, Title: r.Title, URL: r.URL})
	}
	return out
}

// notifyBestEffort sends the optional ready notification and reports whether it
// failed (so the caller can persist the recorded error). It never returns an
// error: a notify failure must not fail the reading.
func (p *Pipeline) notifyBestEffort(ctx context.Context, r reading.Reading, sum summarize.Summary, diag *diagnostics) bool {
	if !p.Config.NotifyEnabled || p.Notifier == nil {
		return false
	}
	// The summary is derived from untrusted fetched content via the summarizer, so
	// escape it before embedding it in the HTML body to prevent markup injection.
	err := p.Notifier.Notify(ctx, notify.Email{
		From:    p.Config.NotifyFrom,
		To:      p.Config.NotifyTo,
		Subject: "Reading ready: " + summaryTitle(sum, r),
		HTML:    "<p>" + html.EscapeString(sum.Summary) + "</p>",
	})
	if err != nil {
		diag.NotifyError = err.Error()
		return true
	}
	return false
}

func (p *Pipeline) contentFields(c content, diag diagnostics, sum summarize.Summary) store.ContentFields {
	title := c.title
	if sum.Title != "" {
		title = sum.Title
	}
	return store.ContentFields{
		Now:             p.Clock.Now(),
		Title:           title,
		Author:          c.author,
		Site:            c.site,
		Lang:            c.lang,
		WordCount:       c.wordCount,
		ExtractionMode:  c.mode,
		ContentKey:      c.contentKey,
		RawKey:          c.rawKey,
		Summary:         sum.Summary,
		SummaryJSON:     sum.JSON,
		SimilarJSON:     c.similarJSON,
		DiagnosticsJSON: marshalDiagnostics(diag),
	}
}

func (p *Pipeline) topK() int {
	if p.Config.TopK <= 0 {
		return 5
	}
	return p.Config.TopK
}

func summaryTitle(sum summarize.Summary, r reading.Reading) string {
	if sum.Title != "" {
		return sum.Title
	}
	if r.Title != "" {
		return r.Title
	}
	return r.URL
}

func embedText(c content) string {
	return strings.TrimSpace(c.title + "\n\n" + c.markdown)
}

func marshalSimilar(items []SimilarItem) (json.RawMessage, error) {
	return json.Marshal(items)
}

// countItems reports how many similar items a snapshot holds, best-effort.
func countItems(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var items []SimilarItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return 0
	}
	return len(items)
}

func marshalDiagnostics(diag diagnostics) json.RawMessage {
	b, err := json.Marshal(diag)
	if err != nil {
		return nil
	}
	return b
}

// classifyFetchStatus maps an HTTP status to a pipeline error: 4xx is permanent
// (the resource is gone or forbidden), 5xx is transient (retry). 2xx is success.
func classifyFetchStatus(status int) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status >= 400 && status < 500:
		return fmt.Errorf("%w: fetch returned status %d", dispatch.ErrPermanent, status)
	default:
		return fmt.Errorf("fetch returned status %d", status)
	}
}

func rawKey(id string) string {
	return "readings/" + id + "/raw"
}

func contentKey(id string) string {
	return "readings/" + id + "/content.md"
}
