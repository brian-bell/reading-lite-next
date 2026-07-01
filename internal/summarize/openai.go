package summarize

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bbell/reading-lite/internal/dispatch"
	"github.com/bbell/reading-lite/internal/httpx"
)

const (
	openAIDefaultBaseURL         = "https://api.openai.com"
	openAIResponsesPath          = "/v1/responses"
	openAIModel                  = "gpt-5.5"
	openAIReasoningEffort        = "medium"
	openAIMaxOutputTokens        = 25000
	openAIResponseFormatName     = "reading_summary"
	openAISummarizerInstructions = "Summarize the reading into a concise title, a clear prose summary, and topic tags. Return only the structured fields requested by the response schema."
)

var openAIReadingSummarySchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "title": {"type": "string", "description": "A clear, descriptive title for the reading."},
    "summary": {"type": "string", "description": "A concise prose summary of the article."},
    "tags": {"type": "array", "items": {"type": "string"}, "description": "Topic tags for the reading."}
  },
  "required": ["title", "summary", "tags"],
  "additionalProperties": false
}`)

// OpenAI is a [Summarizer] backed by the OpenAI Responses API. It asks for a
// strict structured text response and parses the single output_text item into a
// [Summary]. HTTP errors are dispatcher-classified via httpx.
type OpenAI struct {
	apiKey          string
	baseURL         string
	model           string
	reasoningEffort string
	maxOutputTokens int
	client          *http.Client
}

// OpenAIOption configures an [OpenAI] summarizer.
type OpenAIOption func(*OpenAI)

// WithOpenAIBaseURL overrides the API base URL (used to point at a test server).
func WithOpenAIBaseURL(u string) OpenAIOption {
	return func(o *OpenAI) { o.baseURL = u }
}

// WithOpenAIModel overrides the Responses model.
func WithOpenAIModel(m string) OpenAIOption {
	return func(o *OpenAI) { o.model = m }
}

// WithOpenAIReasoningEffort overrides reasoning.effort.
func WithOpenAIReasoningEffort(effort string) OpenAIOption {
	return func(o *OpenAI) { o.reasoningEffort = effort }
}

// WithOpenAIMaxOutputTokens overrides max_output_tokens.
func WithOpenAIMaxOutputTokens(n int) OpenAIOption {
	return func(o *OpenAI) { o.maxOutputTokens = n }
}

// WithOpenAIHTTPClient overrides the underlying http.Client.
func WithOpenAIHTTPClient(c *http.Client) OpenAIOption {
	return func(o *OpenAI) { o.client = c }
}

// NewOpenAI returns an OpenAI Responses summarizer authenticated with apiKey.
func NewOpenAI(apiKey string, opts ...OpenAIOption) *OpenAI {
	o := &OpenAI{
		apiKey:          apiKey,
		baseURL:         openAIDefaultBaseURL,
		model:           openAIModel,
		reasoningEffort: openAIReasoningEffort,
		maxOutputTokens: openAIMaxOutputTokens,
		client:          &http.Client{Timeout: 60 * time.Second},
	}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

type openAIRequest struct {
	Model           string               `json:"model"`
	Reasoning       openAIReasoning      `json:"reasoning"`
	MaxOutputTokens int                  `json:"max_output_tokens"`
	Store           bool                 `json:"store"`
	Input           []openAIInputMessage `json:"input"`
	Text            openAITextConfig     `json:"text"`
}

type openAIReasoning struct {
	Effort string `json:"effort"`
}

type openAIInputMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAITextConfig struct {
	Format openAITextFormat `json:"format"`
}

type openAITextFormat struct {
	Type   string          `json:"type"`
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type openAIResponse struct {
	Status            string `json:"status"`
	IncompleteDetails struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []struct {
		Type    string `json:"type"`
		Status  string `json:"status"`
		Content []struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Refusal string `json:"refusal"`
		} `json:"content"`
	} `json:"output"`
}

// Summarize sends the article context to the OpenAI Responses API and returns
// the structured summary from the completed message output.
func (o *OpenAI) Summarize(ctx context.Context, in SummaryInput) (Summary, error) {
	reqBody, err := json.Marshal(o.responseRequest(in))
	if err != nil {
		return Summary{}, fmt.Errorf("summarize: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+openAIResponsesPath, bytes.NewReader(reqBody))
	if err != nil {
		return Summary{}, fmt.Errorf("summarize: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+o.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(req)
	if err != nil {
		return Summary{}, fmt.Errorf("summarize: post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Summary{}, httpx.ClassifyResponse("summarize", resp)
	}

	var parsed openAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return Summary{}, fmt.Errorf("summarize: decode response: %w", err)
	}
	return parseOpenAIResponse(parsed)
}

func (o *OpenAI) responseRequest(in SummaryInput) openAIRequest {
	return openAIRequest{
		Model:           o.model,
		Reasoning:       openAIReasoning{Effort: o.reasoningEffort},
		MaxOutputTokens: o.maxOutputTokens,
		Store:           false,
		Input: []openAIInputMessage{
			{Role: "system", Content: openAISummarizerInstructions},
			{Role: "user", Content: buildOpenAIUserContent(in)},
		},
		Text: openAITextConfig{Format: openAITextFormat{
			Type:   "json_schema",
			Name:   openAIResponseFormatName,
			Strict: true,
			Schema: openAIReadingSummarySchema,
		}},
	}
}

func parseOpenAIResponse(resp openAIResponse) (Summary, error) {
	// Scan for refusals before any shape validation: refusals are
	// deterministic safety refusals, and OpenAI's documented refusal sample
	// omits the message status field, so a status guard running first would
	// misclassify the refusal as retryable and repeat the billable request.
	for _, item := range resp.Output {
		for _, content := range item.Content {
			if content.Type == "refusal" {
				return Summary{}, fmt.Errorf("%w: summarize: response refusal: %s", dispatch.ErrPermanent, content.Refusal)
			}
		}
	}

	switch resp.Status {
	case "completed":
	case "incomplete":
		// A content filter is a deterministic safety block like a refusal, so
		// retrying it only repeats the billable request; other incomplete
		// reasons (e.g. max_output_tokens) may succeed on retry.
		if resp.IncompleteDetails.Reason == "content_filter" {
			return Summary{}, fmt.Errorf("%w: summarize: response incomplete: %s", dispatch.ErrPermanent, resp.IncompleteDetails.Reason)
		}
		return Summary{}, fmt.Errorf("summarize: response incomplete: %s", resp.IncompleteDetails.Reason)
	default:
		return Summary{}, fmt.Errorf("summarize: response status %q is not completed", resp.Status)
	}

	var outputText string
	for _, item := range resp.Output {
		if item.Type != "message" {
			continue
		}
		if item.Status != "completed" {
			return Summary{}, fmt.Errorf("summarize: output message status %q is not completed", item.Status)
		}
		for _, content := range item.Content {
			if content.Type != "output_text" {
				continue
			}
			if strings.TrimSpace(content.Text) == "" {
				continue
			}
			if outputText != "" {
				return Summary{}, fmt.Errorf("summarize: response must contain exactly one output_text item")
			}
			outputText = content.Text
		}
	}
	if outputText == "" {
		return Summary{}, fmt.Errorf("summarize: response must contain exactly one output_text item")
	}

	var fields struct {
		Title   string   `json:"title"`
		Summary string   `json:"summary"`
		Tags    []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(outputText), &fields); err != nil {
		return Summary{}, fmt.Errorf("summarize: decode output_text JSON: %w", err)
	}
	if strings.TrimSpace(fields.Title) == "" || strings.TrimSpace(fields.Summary) == "" {
		return Summary{}, fmt.Errorf("summarize: output_text JSON is missing a required title or summary")
	}
	return Summary{
		Title:   fields.Title,
		Summary: fields.Summary,
		Tags:    fields.Tags,
		JSON:    json.RawMessage(outputText),
	}, nil
}

func buildOpenAIUserContent(in SummaryInput) string {
	var b strings.Builder
	if in.Title != "" {
		fmt.Fprintf(&b, "Title: %s\n", in.Title)
	}
	if in.Author != "" {
		fmt.Fprintf(&b, "Author: %s\n", in.Author)
	}
	if in.Site != "" {
		fmt.Fprintf(&b, "Site: %s\n", in.Site)
	}
	if in.URL != "" {
		fmt.Fprintf(&b, "URL: %s\n", in.URL)
	}
	b.WriteString("\n")
	b.WriteString(in.Markdown)
	return b.String()
}
