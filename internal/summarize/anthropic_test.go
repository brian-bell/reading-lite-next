package summarize_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bbell/reading-lite/internal/dispatch"
	"github.com/bbell/reading-lite/internal/summarize"
)

// anthropicReq captures the request fields the adapter must send for forced
// tool use.
type anthropicReq struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	Messages  []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name        string          `json:"name"`
		InputSchema json.RawMessage `json:"input_schema"`
	} `json:"tools"`
	ToolChoice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"tool_choice"`
}

func TestAnthropic_ForcedToolRequestAndParse(t *testing.T) {
	t.Parallel()

	var gotAPIKey, gotVersion, gotPath string
	var gotReq anthropicReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"stop_reason": "tool_use",
			"content": [
				{"type": "text", "text": "thinking..."},
				{"type": "tool_use", "id": "toolu_1", "name": "emit_reading",
				 "input": {"title": "Refined Title", "summary": "A concise summary.", "tags": ["go", "databases"]}}
			]
		}`))
	}))
	defer srv.Close()

	s := summarize.NewAnthropic("test-key", summarize.WithBaseURL(srv.URL))
	out, err := s.Summarize(context.Background(), summarize.SummaryInput{
		Title:    "Original",
		Author:   "Jane",
		Site:     "example.com",
		URL:      "https://example.com/post",
		Markdown: "The full article body.",
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	// Request shape: authenticated, versioned, correct endpoint.
	if gotAPIKey != "test-key" {
		t.Errorf("x-api-key = %q, want test-key", gotAPIKey)
	}
	if gotVersion == "" {
		t.Errorf("anthropic-version header missing")
	}
	if gotPath != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", gotPath)
	}
	// Forced tool use: a single emit_reading tool and a tool_choice pinning it.
	if len(gotReq.Tools) != 1 || gotReq.Tools[0].Name != "emit_reading" {
		t.Errorf("tools = %+v, want one emit_reading tool", gotReq.Tools)
	}
	if len(gotReq.Tools) == 1 && len(gotReq.Tools[0].InputSchema) == 0 {
		t.Errorf("emit_reading tool missing input_schema")
	}
	if gotReq.ToolChoice.Type != "tool" || gotReq.ToolChoice.Name != "emit_reading" {
		t.Errorf("tool_choice = %+v, want {tool, emit_reading} (forced tool use)", gotReq.ToolChoice)
	}
	if gotReq.Model == "" || gotReq.MaxTokens <= 0 {
		t.Errorf("model/max_tokens = %q/%d, want both set", gotReq.Model, gotReq.MaxTokens)
	}
	// The article context must reach the model.
	if len(gotReq.Messages) == 0 || !strings.Contains(gotReq.Messages[0].Content, "The full article body.") {
		t.Errorf("messages = %+v, want the markdown in the user content", gotReq.Messages)
	}

	// Parse: the tool_use input becomes the structured Summary.
	if out.Title != "Refined Title" || out.Summary != "A concise summary." {
		t.Errorf("summary = %+v, want refined title + summary", out)
	}
	if len(out.Tags) != 2 || out.Tags[0] != "go" || out.Tags[1] != "databases" {
		t.Errorf("tags = %v, want [go databases]", out.Tags)
	}
	// JSON preserves the raw tool input for summary_json.
	var raw map[string]any
	if err := json.Unmarshal(out.JSON, &raw); err != nil {
		t.Fatalf("Summary.JSON not valid json: %v", err)
	}
	if raw["title"] != "Refined Title" {
		t.Errorf("Summary.JSON = %s, want the raw emit_reading input", out.JSON)
	}
}

func TestAnthropic_MissingToolUseIsError(t *testing.T) {
	t.Parallel()

	// A response with only a text block (no tool_use) violates the forced-tool
	// contract and must surface as an error, not a silent empty summary.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"stop_reason":"end_turn","content":[{"type":"text","text":"no tool here"}]}`))
	}))
	defer srv.Close()

	s := summarize.NewAnthropic("k", summarize.WithBaseURL(srv.URL))
	if _, err := s.Summarize(context.Background(), summarize.SummaryInput{Markdown: "x"}); err == nil {
		t.Fatal("missing tool_use = nil error, want an error")
	}
}

func TestAnthropic_EmptyToolInputIsError(t *testing.T) {
	t.Parallel()

	// Forced tool use makes the model *call* emit_reading, but the API does not
	// enforce the input_schema, so a tool_use block with valid-but-incomplete JSON
	// is possible. The adapter must reject a blank title/summary rather than persist
	// an empty summary and let the reading be marked ready. The error is transient
	// (a retry gives the non-deterministic model another chance).
	cases := []struct{ name, input string }{
		{"null input", `null`},
		{"empty object", `{}`},
		{"missing title", `{"summary":"S","tags":["go"]}`},
		{"blank summary", `{"title":"T","summary":"   ","tags":["go"]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, `{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"emit_reading","input":%s}]}`, c.input)
			}))
			defer srv.Close()

			s := summarize.NewAnthropic("k", summarize.WithBaseURL(srv.URL))
			_, err := s.Summarize(context.Background(), summarize.SummaryInput{Markdown: "x"})
			if err == nil {
				t.Fatalf("input %s = nil error, want an error (a blank summary must not be accepted)", c.input)
			}
			if got := dispatch.Classify(err); got.Outcome != dispatch.Retry {
				t.Fatalf("Classify(incomplete tool input) = %v, want Retry", got.Outcome)
			}
		})
	}
}

func TestAnthropic_RateLimitMapsToRateLimitError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error"}}`))
	}))
	defer srv.Close()

	s := summarize.NewAnthropic("k", summarize.WithBaseURL(srv.URL))
	_, err := s.Summarize(context.Background(), summarize.SummaryInput{Markdown: "x"})

	var rl *dispatch.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("429 = %v, want *dispatch.RateLimitError", err)
	}
	if rl.RetryAfter != 12*time.Second {
		t.Fatalf("RetryAfter = %v, want 12s", rl.RetryAfter)
	}
}
