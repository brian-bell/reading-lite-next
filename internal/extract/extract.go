// Package extract defines the article-extraction port and an in-memory fake.
//
// The production adapter wraps a readability + HTML-to-markdown pipeline with a
// salvage ladder (Phase 7); [Fake] is the scriptable double used by pipeline tests.
package extract

import (
	"bytes"
	"context"
	"slices"
	"sync"

	"github.com/bbell/reading-lite/internal/fetch"
)

// Mode identifies which extraction tier produced an [Article]. It mirrors a
// reading's extraction_mode.
type Mode string

const (
	// ModeReadability is the primary tier: a clean readability extraction.
	ModeReadability Mode = "readability"
	// ModeRawDOM is the salvage tier: best-effort text from the raw DOM.
	ModeRawDOM Mode = "raw_dom"
	// ModeRawOnly is the floor tier: the stripped raw body, no structure.
	ModeRawOnly Mode = "raw_only"
)

// Article is the extracted, structured content of a fetched resource.
type Article struct {
	// Title is the article title.
	Title string
	// Author is the article author.
	Author string
	// Site is the source site name.
	Site string
	// Lang is the detected content language.
	Lang string
	// Markdown is the extracted body as markdown.
	Markdown string
	// Mode records which extraction tier produced this article.
	Mode Mode
	// WordCount is the extracted body's word count.
	WordCount int
}

// Extractor turns a fetched [fetch.Resource] into a structured [Article].
type Extractor interface {
	Extract(ctx context.Context, r fetch.Resource) (Article, error)
}

// Fake is a concurrency-safe, scriptable [Extractor] for tests. Set the scripted
// fields before first use (they are read under the lock but not meant to change
// once workers may call concurrently); resources passed in are recorded for assertions.
type Fake struct {
	// Article is returned on success.
	Article Article
	// Err, when non-nil, is returned instead of Article.
	Err error

	mu        sync.Mutex
	calls     int
	resources []fetch.Resource
}

// Extract records the resource and returns the scripted article or error.
func (f *Fake) Extract(ctx context.Context, r fetch.Resource) (Article, error) {
	if err := ctx.Err(); err != nil {
		return Article{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	// Record a copy with its own Body so a caller mutating the input slice after
	// this call cannot corrupt the recorded history.
	rec := r
	rec.Body = bytes.Clone(r.Body)
	f.resources = append(f.resources, rec)

	if f.Err != nil {
		return Article{}, f.Err
	}
	// Article has no slice or map fields, so returning it by value is a full copy.
	return f.Article, nil
}

// Calls is the number of times Extract was invoked.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}

// Resources returns every resource passed to Extract, in call order. Each
// returned Body is a fresh copy so a caller cannot corrupt the recorded history.
func (f *Fake) Resources() []fetch.Resource {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := slices.Clone(f.resources)
	for i := range out {
		out[i].Body = bytes.Clone(out[i].Body)
	}
	return out
}
