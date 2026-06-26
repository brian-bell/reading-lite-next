// Package summarize defines the article-summarization port and an in-memory fake.
//
// The production adapter is Anthropic with a forced emit_reading tool call
// (Phase 6) and an Anthropic Message Batches client for operator batch workflows;
// [Fake] is the scriptable double used by pipeline tests.
package summarize

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
)

// SummaryInput is the article context handed to the summarizer.
type SummaryInput struct {
	// Title is the extracted article title, if any.
	Title string
	// Author is the extracted article author, if any.
	Author string
	// Site is the source site name, if any.
	Site string
	// URL is the original reading URL.
	URL string
	// Markdown is the extracted body to summarize.
	Markdown string
}

// Summary is the structured result of summarizing an article.
type Summary struct {
	// Title is the (possibly refined) title.
	Title string
	// Summary is the human-readable summary text.
	Summary string
	// Tags are the suggested tags for the reading.
	Tags []string
	// JSON is the raw structured emit_reading payload, persisted as summary_json.
	JSON json.RawMessage
}

// Summarizer turns article context into a structured [Summary].
type Summarizer interface {
	Summarize(ctx context.Context, in SummaryInput) (Summary, error)
}

// Fake is a concurrency-safe, scriptable [Summarizer] for tests. Set the scripted
// fields before first use (they are read under the lock but not meant to change
// once workers may call concurrently); inputs are recorded for assertions.
type Fake struct {
	// Summary is returned on success.
	Summary Summary
	// Err, when non-nil, is returned instead of Summary.
	Err error

	mu     sync.Mutex
	calls  int
	inputs []SummaryInput
}

// Summarize records the input and returns the scripted summary or error.
func (f *Fake) Summarize(ctx context.Context, in SummaryInput) (Summary, error) {
	if err := ctx.Err(); err != nil {
		return Summary{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	f.inputs = append(f.inputs, in)

	if f.Err != nil {
		return Summary{}, f.Err
	}
	// Copy the slice-backed fields (Tags, and JSON which is a json.RawMessage)
	// so a caller mutating them cannot corrupt the script for later calls.
	out := f.Summary
	out.Tags = slices.Clone(f.Summary.Tags)
	out.JSON = slices.Clone(f.Summary.JSON)
	return out, nil
}

// Calls is the number of times Summarize was invoked.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}

// Inputs returns every input passed to Summarize, in call order.
func (f *Fake) Inputs() []SummaryInput {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.inputs)
}
