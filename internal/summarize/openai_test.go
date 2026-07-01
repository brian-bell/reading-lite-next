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

type openAIReq struct {
	Model           string `json:"model"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	Store           bool   `json:"store"`
	Reasoning       struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	Input []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"input"`
	Text struct {
		Format struct {
			Type   string          `json:"type"`
			Name   string          `json:"name"`
			Strict bool            `json:"strict"`
			Schema json.RawMessage `json:"schema"`
		} `json:"format"`
	} `json:"text"`
	Tools []json.RawMessage `json:"tools,omitempty"`
}

func TestOpenAI_RequestShapeAndParse(t *testing.T) {
	t.Parallel()

	var gotAuth, gotPath string
	var gotReq openAIReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"status": "completed",
			"output": [
				{"type": "message", "role": "assistant", "status": "completed", "content": [
					{"type": "output_text", "text": "{\"title\":\"Refined Title\",\"summary\":\"A concise summary.\",\"tags\":[\"go\",\"databases\"]}"}
				]}
			]
		}`))
	}))
	defer srv.Close()

	out, err := summarize.NewOpenAI("test-key", summarize.WithOpenAIBaseURL(srv.URL)).Summarize(context.Background(), summarize.SummaryInput{
		Title:    "Original",
		Author:   "Jane",
		Site:     "example.com",
		URL:      "https://example.com/post",
		Markdown: "The full article body.",
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want bearer key", gotAuth)
	}
	if gotPath != "/v1/responses" {
		t.Errorf("path = %q, want /v1/responses", gotPath)
	}
	if gotReq.Model != "gpt-5.5" {
		t.Errorf("model = %q, want gpt-5.5", gotReq.Model)
	}
	if gotReq.Reasoning.Effort != "medium" {
		t.Errorf("reasoning.effort = %q, want medium", gotReq.Reasoning.Effort)
	}
	if gotReq.MaxOutputTokens != 25000 {
		t.Errorf("max_output_tokens = %d, want 25000", gotReq.MaxOutputTokens)
	}
	if gotReq.Store {
		t.Error("store = true, want false")
	}
	if len(gotReq.Input) != 2 || gotReq.Input[0].Role != "system" || gotReq.Input[1].Role != "user" {
		t.Fatalf("input = %+v, want system and user messages", gotReq.Input)
	}
	userContent := gotReq.Input[1].Content
	if !strings.Contains(userContent, "The full article body.") || !strings.Contains(userContent, "https://example.com/post") {
		t.Errorf("user content = %q, want article body and URL", userContent)
	}
	allInput, _ := json.Marshal(gotReq.Input)
	if strings.Contains(string(allInput), "emit_reading") || strings.Contains(strings.ToLower(string(allInput)), "tool") {
		t.Errorf("OpenAI prompt referenced Anthropic tool use: %s", allInput)
	}
	if len(gotReq.Tools) != 0 {
		t.Fatalf("tools = %v, want none", gotReq.Tools)
	}
	assertReadingSummarySchema(t, gotReq.Text.Format)

	if out.Title != "Refined Title" || out.Summary != "A concise summary." {
		t.Errorf("summary = %+v, want refined title + summary", out)
	}
	if len(out.Tags) != 2 || out.Tags[0] != "go" || out.Tags[1] != "databases" {
		t.Errorf("tags = %v, want [go databases]", out.Tags)
	}
	if string(out.JSON) != `{"title":"Refined Title","summary":"A concise summary.","tags":["go","databases"]}` {
		t.Errorf("Summary.JSON = %s, want raw output_text JSON", out.JSON)
	}
}

func TestOpenAI_ResponseValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		body        string
		wantText    string
		wantOutcome dispatch.Outcome
	}{
		{
			name:        "refusal content fails permanently",
			body:        `{"status":"completed","output":[{"type":"message","status":"completed","content":[{"type":"refusal","refusal":"no"}]}]}`,
			wantText:    "refusal",
			wantOutcome: dispatch.Fail,
		},
		{
			name:        "missing output text",
			body:        `{"status":"completed","output":[{"type":"message","status":"completed","content":[]}]}`,
			wantText:    "one output_text",
			wantOutcome: dispatch.Retry,
		},
		{
			name:        "missing message status",
			body:        `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"title\":\"T\",\"summary\":\"S\",\"tags\":[]}"}]}]}`,
			wantText:    "message status",
			wantOutcome: dispatch.Retry,
		},
		{
			name:        "multiple output text items",
			body:        `{"status":"completed","output":[{"type":"message","status":"completed","content":[{"type":"output_text","text":"{\"title\":\"T\",\"summary\":\"S\",\"tags\":[]}"},{"type":"output_text","text":"{\"title\":\"T2\",\"summary\":\"S2\",\"tags\":[]}"}]}]}`,
			wantText:    "one output_text",
			wantOutcome: dispatch.Retry,
		},
		{
			name:        "incomplete status with reason",
			body:        `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`,
			wantText:    "max_output_tokens",
			wantOutcome: dispatch.Retry,
		},
		{
			name:        "non completed status",
			body:        `{"status":"failed","output":[]}`,
			wantText:    "status",
			wantOutcome: dispatch.Retry,
		},
		{
			name:        "invalid summary json",
			body:        `{"status":"completed","output":[{"type":"message","status":"completed","content":[{"type":"output_text","text":"not json"}]}]}`,
			wantText:    "decode",
			wantOutcome: dispatch.Retry,
		},
		{
			name:        "blank title",
			body:        `{"status":"completed","output":[{"type":"message","status":"completed","content":[{"type":"output_text","text":"{\"title\":\" \",\"summary\":\"S\",\"tags\":[]}"}]}]}`,
			wantText:    "title or summary",
			wantOutcome: dispatch.Retry,
		},
		{
			name:        "blank summary",
			body:        `{"status":"completed","output":[{"type":"message","status":"completed","content":[{"type":"output_text","text":"{\"title\":\"T\",\"summary\":\" \",\"tags\":[]}"}]}]}`,
			wantText:    "title or summary",
			wantOutcome: dispatch.Retry,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			_, err := summarize.NewOpenAI("k", summarize.WithOpenAIBaseURL(srv.URL)).Summarize(context.Background(), summarize.SummaryInput{Markdown: "body"})
			if err == nil {
				t.Fatal("Summarize = nil error, want validation error")
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.wantText)
			}
			if got := dispatch.Classify(err); got.Outcome != tc.wantOutcome {
				t.Fatalf("Classify(validation error) = %v, want %v", got.Outcome, tc.wantOutcome)
			}
		})
	}
}

func TestOpenAI_UpstreamErrorsClassifyThroughHTTPX(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		status  int
		header  http.Header
		outcome dispatch.Outcome
	}{
		{"rate limit requeues", http.StatusTooManyRequests, http.Header{"Retry-After": {"12"}}, dispatch.Requeue},
		{"client error fails", http.StatusBadRequest, nil, dispatch.Fail},
		{"server error retries", http.StatusBadGateway, nil, dispatch.Retry},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, vs := range tc.header {
					for _, v := range vs {
						w.Header().Set(k, v)
					}
				}
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, `{"error":{"message":"upstream failed"}}`)
			}))
			defer srv.Close()

			_, err := summarize.NewOpenAI("k", summarize.WithOpenAIBaseURL(srv.URL)).Summarize(context.Background(), summarize.SummaryInput{Markdown: "body"})
			if got := dispatch.Classify(err).Outcome; got != tc.outcome {
				t.Fatalf("Classify(status %d) = %v, want %v (err %v)", tc.status, got, tc.outcome, err)
			}
			var rl *dispatch.RateLimitError
			if tc.status == http.StatusTooManyRequests && (!errors.As(err, &rl) || rl.RetryAfter != 12*time.Second) {
				t.Fatalf("429 err = %v, want RateLimitError with 12s", err)
			}
		})
	}
}

func assertReadingSummarySchema(t *testing.T, format struct {
	Type   string          `json:"type"`
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}) {
	t.Helper()
	if format.Type != "json_schema" || format.Name != "reading_summary" || !format.Strict {
		t.Fatalf("text.format = %+v, want strict reading_summary json_schema", format)
	}
	var schema struct {
		Type                 string `json:"type"`
		AdditionalProperties bool   `json:"additionalProperties"`
		Required             []string
		Properties           map[string]struct {
			Type  string `json:"type"`
			Items *struct {
				Type string `json:"type"`
			} `json:"items,omitempty"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(format.Schema, &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if schema.Type != "object" || schema.AdditionalProperties {
		t.Fatalf("schema object/additionalProperties = %q/%v, want object/false", schema.Type, schema.AdditionalProperties)
	}
	if strings.Join(schema.Required, ",") != "title,summary,tags" {
		t.Fatalf("required = %v, want title,summary,tags", schema.Required)
	}
	if schema.Properties["title"].Type != "string" || schema.Properties["summary"].Type != "string" {
		t.Fatalf("title/summary schema = %+v/%+v, want strings", schema.Properties["title"], schema.Properties["summary"])
	}
	tags := schema.Properties["tags"]
	if tags.Type != "array" || tags.Items == nil || tags.Items.Type != "string" {
		t.Fatalf("tags schema = %+v, want array of strings", tags)
	}
}
