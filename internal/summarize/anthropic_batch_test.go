package summarize_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bbell/reading-lite/internal/dispatch"
	"github.com/bbell/reading-lite/internal/summarize"
)

func TestAnthropicBatch_CreateAndGetContract(t *testing.T) {
	t.Parallel()

	type batchCreateReq struct {
		Requests []struct {
			CustomID string       `json:"custom_id"`
			Params   anthropicReq `json:"params"`
		} `json:"requests"`
	}

	var gotCreate batchCreateReq
	var gotCreateAPIKey, gotCreateVersion string
	var gotCreateContentType string
	var gotGetAPIKey, gotGetVersion string
	var gotMethods []string
	var gotPaths []string

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethods = append(gotMethods, r.Method)
		gotPaths = append(gotPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/v1/messages/batches":
			gotCreateAPIKey = r.Header.Get("x-api-key")
			gotCreateVersion = r.Header.Get("anthropic-version")
			gotCreateContentType = r.Header.Get("Content-Type")
			if err := json.NewDecoder(r.Body).Decode(&gotCreate); err != nil {
				t.Errorf("decode create request: %v", err)
			}
			_, _ = w.Write([]byte(`{
				"id": "msgbatch_123",
				"type": "message_batch",
				"processing_status": "in_progress",
				"request_counts": {"processing": 1, "succeeded": 0, "errored": 0, "canceled": 0, "expired": 0}
			}`))
		case "/v1/messages/batches/msgbatch_123":
			gotGetAPIKey = r.Header.Get("x-api-key")
			gotGetVersion = r.Header.Get("anthropic-version")
			_, _ = w.Write([]byte(`{
				"id": "msgbatch_123",
				"type": "message_batch",
				"processing_status": "ended",
				"request_counts": {"processing": 0, "succeeded": 1, "errored": 0, "canceled": 0, "expired": 0},
				"results_url": "https://api.anthropic.com/v1/messages/batches/msgbatch_123/results"
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := summarize.NewAnthropic(
		"batch-key",
		summarize.WithBaseURL(srv.URL),
		summarize.WithHTTPClient(srv.Client()),
		summarize.WithModel("test-model"),
		summarize.WithMaxTokens(321),
	)
	req := client.NewBatchRequest("reading-2", summarize.SummaryInput{
		Title:    "Original",
		Author:   "Jane",
		Site:     "example.com",
		URL:      "https://example.com/post",
		Markdown: "The full article body.",
	})
	encodedReq, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal batch request: %v", err)
	}
	var restoredReq summarize.BatchRequest
	if err := json.Unmarshal(encodedReq, &restoredReq); err != nil {
		t.Fatalf("unmarshal batch request: %v", err)
	}

	created, err := client.CreateBatch(context.Background(), []summarize.BatchRequest{restoredReq})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	got, err := client.GetBatch(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}

	if gotCreateAPIKey != "batch-key" || gotGetAPIKey != "batch-key" {
		t.Fatalf("x-api-key create/get = %q/%q, want batch-key for both", gotCreateAPIKey, gotGetAPIKey)
	}
	if gotCreateVersion != "2023-06-01" || gotGetVersion != "2023-06-01" {
		t.Fatalf("anthropic-version create/get = %q/%q, want 2023-06-01 for both", gotCreateVersion, gotGetVersion)
	}
	if gotCreateContentType != "application/json" {
		t.Fatalf("create Content-Type = %q, want application/json", gotCreateContentType)
	}
	if len(gotMethods) != 2 || gotMethods[0] != http.MethodPost || gotMethods[1] != http.MethodGet {
		t.Fatalf("methods = %v, want [POST GET]", gotMethods)
	}
	if len(gotPaths) != 2 || gotPaths[0] != "/v1/messages/batches" || gotPaths[1] != "/v1/messages/batches/msgbatch_123" {
		t.Fatalf("paths = %v, want create and get batch endpoints", gotPaths)
	}
	if created.ID != "msgbatch_123" || created.ProcessingStatus != "in_progress" {
		t.Fatalf("created batch = %+v, want msgbatch_123 in_progress", created)
	}
	if got.ProcessingStatus != "ended" || got.RequestCounts.Succeeded != 1 || got.ResultsURL == "" {
		t.Fatalf("retrieved batch = %+v, want ended with one success and results_url", got)
	}

	if len(gotCreate.Requests) != 1 {
		t.Fatalf("create requests = %d, want 1", len(gotCreate.Requests))
	}
	gotReq := gotCreate.Requests[0]
	if gotReq.CustomID != "reading-2" {
		t.Errorf("custom_id = %q, want reading-2", gotReq.CustomID)
	}
	if len(gotReq.Params.Tools) != 1 || gotReq.Params.Tools[0].Name != "emit_reading" {
		t.Errorf("tools = %+v, want one emit_reading tool", gotReq.Params.Tools)
	}
	if gotReq.Params.Model != "test-model" || gotReq.Params.MaxTokens != 321 {
		t.Errorf("model/max_tokens = %q/%d, want test-model/321", gotReq.Params.Model, gotReq.Params.MaxTokens)
	}
	if gotReq.Params.ToolChoice.Type != "tool" || gotReq.Params.ToolChoice.Name != "emit_reading" {
		t.Errorf("tool_choice = %+v, want forced emit_reading", gotReq.Params.ToolChoice)
	}
	if len(gotReq.Params.Messages) == 0 || gotReq.Params.Messages[0].Content == "" {
		t.Errorf("messages = %+v, want rendered article prompt", gotReq.Params.Messages)
	}
}

func TestParseBatchResults_OutOfOrderTerminalOutcomes(t *testing.T) {
	t.Parallel()

	expectedCustomIDs := []string{"reading-1", "reading-2", "reading-3", "reading-4"}
	jsonl := strings.NewReader(`
{"custom_id":"reading-3","result":{"type":"canceled"}}
{"custom_id":"reading-1","result":{"type":"succeeded","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"emit_reading","input":{"title":"One","summary":"First summary","tags":["one"]}}]}}}
{"custom_id":"reading-4","result":{"type":"expired"}}
{"custom_id":"reading-2","result":{"type":"errored","error":{"type":"error","error":{"type":"invalid_request_error","message":"bad request"}}}}
`)

	got, err := summarize.ParseBatchResults(jsonl, expectedCustomIDs)
	if err != nil {
		t.Fatalf("ParseBatchResults: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("results = %d, want 4", len(got))
	}
	if got[0].CustomID != "reading-1" || got[0].Type != summarize.BatchResultSucceeded {
		t.Fatalf("result[0] = %+v, want reading-1 succeeded", got[0])
	}
	if got[0].Summary.Title != "One" || got[0].Summary.Summary != "First summary" {
		t.Fatalf("success summary = %+v, want parsed emit_reading input", got[0].Summary)
	}
	if got[1].CustomID != "reading-2" || got[1].Type != summarize.BatchResultErrored {
		t.Fatalf("result[1] = %+v, want reading-2 errored", got[1])
	}
	if got[1].Error.Type != "invalid_request_error" || got[1].Error.Message != "bad request" {
		t.Fatalf("error = %+v, want Anthropic error payload", got[1].Error)
	}
	if got[2].CustomID != "reading-3" || got[2].Type != summarize.BatchResultCanceled {
		t.Fatalf("result[2] = %+v, want reading-3 canceled", got[2])
	}
	if got[3].CustomID != "reading-4" || got[3].Type != summarize.BatchResultExpired {
		t.Fatalf("result[3] = %+v, want reading-4 expired", got[3])
	}
}

func TestAnthropicBatch_ResultsContract(t *testing.T) {
	t.Parallel()

	var gotAPIKey, gotVersion, gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/x-jsonlines")
		_, _ = w.Write([]byte(`{"custom_id":"reading-1","result":{"type":"succeeded","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"emit_reading","input":{"title":"One","summary":"First summary","tags":["one"]}}]}}}` + "\n"))
	}))
	defer srv.Close()

	client := summarize.NewAnthropic("batch-key", summarize.WithBaseURL(srv.URL))

	got, err := client.BatchResults(context.Background(), "msgbatch_123", []string{"reading-1"})
	if err != nil {
		t.Fatalf("BatchResults: %v", err)
	}
	if gotAPIKey != "batch-key" || gotVersion != "2023-06-01" {
		t.Fatalf("headers x-api-key/version = %q/%q, want batch-key/2023-06-01", gotAPIKey, gotVersion)
	}
	if gotMethod != http.MethodGet || gotPath != "/v1/messages/batches/msgbatch_123/results" {
		t.Fatalf("method/path = %s %s, want GET /v1/messages/batches/msgbatch_123/results", gotMethod, gotPath)
	}
	if len(got) != 1 || got[0].Type != summarize.BatchResultSucceeded || got[0].Summary.Title != "One" {
		t.Fatalf("results = %+v, want parsed succeeded result", got)
	}
}

func TestParseBatchResults_RejectsInvalidResults(t *testing.T) {
	t.Parallel()

	expectedCustomIDs := []string{"reading-1"}
	cases := []struct {
		name              string
		expectedCustomIDs []string
		jsonl             string
		want              string
	}{
		{
			name: "malformed line",
			jsonl: `{not-json}
`,
			want: "decode result line",
		},
		{
			name: "unknown custom id",
			jsonl: `{"custom_id":"unknown","result":{"type":"canceled"}}
`,
			want: "unknown custom_id",
		},
		{
			name: "duplicate custom id",
			jsonl: `{"custom_id":"reading-1","result":{"type":"canceled"}}
{"custom_id":"reading-1","result":{"type":"expired"}}
`,
			want: "duplicate custom_id",
		},
		{
			name: "missing expected result",
			expectedCustomIDs: []string{
				"reading-1",
				"reading-2",
			},
			jsonl: `{"custom_id":"reading-1","result":{"type":"canceled"}}
`,
			want: "missing result",
		},
		{
			name: "errored missing error payload",
			jsonl: `{"custom_id":"reading-1","result":{"type":"errored"}}
`,
			want: "missing error payload",
		},
		{
			name: "errored blank error fields",
			jsonl: `{"custom_id":"reading-1","result":{"type":"errored","error":{"type":"error","error":{"type":"invalid_request_error","message":"   "}}}}
`,
			want: "missing error payload",
		},
		{
			name: "missing tool use",
			jsonl: `{"custom_id":"reading-1","result":{"type":"succeeded","message":{"stop_reason":"end_turn","content":[{"type":"text","text":"no tool"}]}}}
`,
			want: "missing emit_reading tool_use",
		},
		{
			name: "blank summary field",
			jsonl: `{"custom_id":"reading-1","result":{"type":"succeeded","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"emit_reading","input":{"title":"One","summary":"   ","tags":["one"]}}]}}}
`,
			want: "missing a required title or summary",
		},
		{
			name: "invalid summary field",
			jsonl: `{"custom_id":"reading-1","result":{"type":"succeeded","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"emit_reading","input":{"title":"One","summary":"First summary","tags":"one"}}]}}}
`,
			want: "decode emit_reading input",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			expected := expectedCustomIDs
			if c.expectedCustomIDs != nil {
				expected = c.expectedCustomIDs
			}
			_, err := summarize.ParseBatchResults(strings.NewReader(c.jsonl), expected)
			if err == nil {
				t.Fatal("ParseBatchResults error = nil, want validation error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want substring %q", err, c.want)
			}
		})
	}
}

func TestAnthropicBatch_RateLimitMapsToRateLimitError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error"}}`))
	}))
	defer srv.Close()

	client := summarize.NewAnthropic("k", summarize.WithBaseURL(srv.URL))
	req := client.NewBatchRequest("reading-1", summarize.SummaryInput{Markdown: "one"})
	_, err := client.CreateBatch(context.Background(), []summarize.BatchRequest{req})

	var rl *dispatch.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("429 = %v, want *dispatch.RateLimitError", err)
	}
	if rl.RetryAfter != 7*time.Second {
		t.Fatalf("RetryAfter = %v, want 7s", rl.RetryAfter)
	}
}
