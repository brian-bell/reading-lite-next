// Package reading defines the pure domain core for submitted readings.
package reading

import (
	"encoding/json"
	"fmt"
	"time"
)

// Reading is the domain record for a submitted article or source URL.
type Reading struct {
	// ID uniquely identifies the reading.
	ID string `json:"id"`
	// URL is the original submitted URL.
	URL string `json:"url"`
	// URLKey is the normalized idempotency key for URL.
	URLKey string `json:"url_key"`
	// Status is the persisted processing lifecycle state.
	Status Status `json:"status"`
	// SourceKind selects source-specific pipeline behavior.
	SourceKind SourceKind `json:"source_kind"`
	// Title is the extracted article title.
	Title string `json:"title,omitempty"`
	// Author is the extracted article author.
	Author string `json:"author,omitempty"`
	// Site is the extracted or inferred site name.
	Site string `json:"site,omitempty"`
	// Lang is the extracted content language.
	Lang string `json:"lang,omitempty"`
	// WordCount is the extracted article word count.
	WordCount int `json:"word_count,omitempty"`
	// ExtractionMode records the extraction tier used for ready readings.
	ExtractionMode string `json:"extraction_mode,omitempty"`
	// ContentKey points at the extracted content blob.
	ContentKey string `json:"content_key,omitempty"`
	// RawKey points at the raw source blob.
	RawKey string `json:"raw_key,omitempty"`
	// Summary is the generated human-readable summary.
	Summary string `json:"summary,omitempty"`
	// SummaryJSON is the raw structured summarizer payload (emit_reading output).
	SummaryJSON json.RawMessage `json:"summary_json,omitempty"`
	// SimilarJSON is the snapshot of similar readings found by vector search.
	SimilarJSON json.RawMessage `json:"similar_json,omitempty"`
	// DiagnosticsJSON records per-step pipeline diagnostics (tiers, outcomes).
	DiagnosticsJSON json.RawMessage `json:"diagnostics_json,omitempty"`
	// Error is the persisted processing error for failed readings.
	Error string `json:"error,omitempty"`
	// StaleReason is a read-time annotation for stale pending or running readings.
	StaleReason string `json:"stale_reason,omitempty"`
	// ProcessAttempts is the mirrored retry count used by crash recovery.
	ProcessAttempts int `json:"process_attempts"`
	// Tags are user-controlled labels for filtering and search.
	Tags []string `json:"tags,omitempty"`
	// CreatedAt is when the reading was first persisted.
	CreatedAt time.Time `json:"created_at"`
	// StartedAt is when the current processing attempt started.
	StartedAt *time.Time `json:"started_at,omitempty"`
	// FinishedAt is when the latest terminal processing attempt finished.
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// UpdatedAt is when the reading was last changed.
	UpdatedAt time.Time `json:"updated_at"`
}

// TTLs controls when non-terminal readings are reported as stale.
type TTLs struct {
	// Pending is the maximum age before an enqueued reading is stale.
	Pending time.Duration
	// Running is the maximum processing duration before a running reading is stale.
	Running time.Duration
}

// AnnotateStale overlays stale read status on a copy of r without mutating persisted state.
func AnnotateStale(r Reading, now time.Time, ttls TTLs) Reading {
	switch r.Status {
	case Pending:
		if ttls.Pending > 0 && now.Sub(pendingSince(r)) > ttls.Pending {
			r.Status = Failed
			r.StaleReason = fmt.Sprintf("timed out before processing after %s", ttls.Pending)
		}
	case Running:
		if r.StartedAt != nil && ttls.Running > 0 && now.Sub(*r.StartedAt) > ttls.Running {
			r.Status = Failed
			r.StaleReason = fmt.Sprintf("processing stalled after %s", ttls.Running)
		}
	}

	return r
}

func pendingSince(r Reading) time.Time {
	if r.UpdatedAt.After(r.CreatedAt) {
		return r.UpdatedAt
	}
	return r.CreatedAt
}
