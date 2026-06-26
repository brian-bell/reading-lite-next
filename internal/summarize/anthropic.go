package summarize

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bbell/reading-lite/internal/httpx"
)

// Anthropic request defaults. Summarization uses forced tool use: the model must
// answer by calling the emit_reading tool, so the structured fields arrive as a
// validated tool input rather than free text to parse.
const (
	anthropicDefaultBaseURL = "https://api.anthropic.com"
	anthropicMessagesPath   = "/v1/messages"
	anthropicBatchesPath    = "/v1/messages/batches"
	anthropicVersion        = "2023-06-01"
	// Sonnet (not Opus) is the default: summarization is a structured-extraction
	// task well within Sonnet's reach, and it is the cheaper choice for a personal
	// service. Override with WithModel when a different tier is wanted.
	anthropicModel     = "claude-sonnet-4-6"
	anthropicMaxTokens = 1024
	toolName           = "emit_reading"
)

// emitReadingSchema is the JSON schema for the emit_reading tool input. The model
// is forced to produce exactly these fields.
var emitReadingSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "title": {"type": "string", "description": "A clear, descriptive title for the reading."},
    "summary": {"type": "string", "description": "A concise prose summary of the article."},
    "tags": {"type": "array", "items": {"type": "string"}, "description": "Topic tags for the reading."}
  },
  "required": ["title", "summary", "tags"]
}`)

// Anthropic is the production [Summarizer]: it calls the Anthropic Messages API
// with forced emit_reading tool use and parses the tool input into a [Summary].
// HTTP errors are dispatcher-classified (429 → requeue, 4xx → permanent, 5xx →
// retry).
type Anthropic struct {
	apiKey    string
	baseURL   string
	model     string
	version   string
	maxTokens int
	client    *http.Client
}

// Option configures an [Anthropic] summarizer.
type Option func(*Anthropic)

// WithBaseURL overrides the API base URL (used to point at a test server).
func WithBaseURL(u string) Option {
	return func(a *Anthropic) { a.baseURL = u }
}

// WithModel overrides the model id.
func WithModel(m string) Option {
	return func(a *Anthropic) { a.model = m }
}

// WithMaxTokens overrides the response token budget.
func WithMaxTokens(n int) Option {
	return func(a *Anthropic) { a.maxTokens = n }
}

// WithHTTPClient overrides the underlying http.Client.
func WithHTTPClient(c *http.Client) Option {
	return func(a *Anthropic) { a.client = c }
}

// NewAnthropic returns an Anthropic summarizer authenticated with apiKey.
func NewAnthropic(apiKey string, opts ...Option) *Anthropic {
	a := &Anthropic{
		apiKey:    apiKey,
		baseURL:   anthropicDefaultBaseURL,
		model:     anthropicModel,
		version:   anthropicVersion,
		maxTokens: anthropicMaxTokens,
		client:    &http.Client{Timeout: 60 * time.Second},
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type anthropicRequest struct {
	Model      string              `json:"model"`
	MaxTokens  int                 `json:"max_tokens"`
	Messages   []anthropicMessage  `json:"messages"`
	Tools      []anthropicTool     `json:"tools"`
	ToolChoice anthropicToolChoice `json:"tool_choice"`
}

type anthropicResponse struct {
	StopReason string `json:"stop_reason"`
	Content    []struct {
		Type  string          `json:"type"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
}

// Summarize sends the article context to Anthropic with forced emit_reading tool
// use and returns the structured summary.
func (a *Anthropic) Summarize(ctx context.Context, in SummaryInput) (Summary, error) {
	reqBody, err := json.Marshal(a.messageRequest(in))
	if err != nil {
		return Summary{}, fmt.Errorf("summarize: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+anthropicMessagesPath, bytes.NewReader(reqBody))
	if err != nil {
		return Summary{}, fmt.Errorf("summarize: build request: %w", err)
	}
	a.setHeaders(req)

	resp, err := a.client.Do(req)
	if err != nil {
		return Summary{}, fmt.Errorf("summarize: post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Summary{}, httpx.ClassifyResponse("summarize", resp)
	}

	var parsed anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return Summary{}, fmt.Errorf("summarize: decode response: %w", err)
	}
	return parseToolUse(parsed)
}

func (a *Anthropic) messageRequest(in SummaryInput) anthropicRequest {
	return anthropicRequest{
		Model:     a.model,
		MaxTokens: a.maxTokens,
		Messages:  []anthropicMessage{{Role: "user", Content: buildPrompt(in)}},
		Tools: []anthropicTool{{
			Name:        toolName,
			Description: "Emit the structured summary of the article as a reading.",
			InputSchema: emitReadingSchema,
		}},
		ToolChoice: anthropicToolChoice{Type: "tool", Name: toolName},
	}
}

func (a *Anthropic) setHeaders(req *http.Request) {
	req.Header.Set("x-api-key", a.apiKey)
	req.Header.Set("anthropic-version", a.version)
	req.Header.Set("Content-Type", "application/json")
}

// parseToolUse extracts the emit_reading tool input from the response and maps it
// to a Summary. A response without the forced tool_use block is an error: the
// model failed to honor the tool contract.
func parseToolUse(resp anthropicResponse) (Summary, error) {
	for _, block := range resp.Content {
		if block.Type != "tool_use" || block.Name != toolName {
			continue
		}
		var fields struct {
			Title   string   `json:"title"`
			Summary string   `json:"summary"`
			Tags    []string `json:"tags"`
		}
		if err := json.Unmarshal(block.Input, &fields); err != nil {
			return Summary{}, fmt.Errorf("summarize: decode %s input: %w", toolName, err)
		}
		// The API forces the tool *call* but does not enforce the input_schema, so a
		// tool_use block can still carry valid-but-incomplete JSON (null, {}, or a
		// blank summary). Reject an empty title/summary rather than persist a blank
		// summary and mark the reading ready. This is transient: a retry gives the
		// non-deterministic model another chance. (Tags may legitimately be empty.)
		if strings.TrimSpace(fields.Title) == "" || strings.TrimSpace(fields.Summary) == "" {
			return Summary{}, fmt.Errorf("summarize: %s input is missing a required title or summary", toolName)
		}
		return Summary{
			Title:   fields.Title,
			Summary: fields.Summary,
			Tags:    fields.Tags,
			JSON:    block.Input,
		}, nil
	}
	// A forced tool_choice should always yield the tool_use block; its absence
	// usually means the model truncated (stop_reason "max_tokens") or the contract
	// was violated. Surface stop_reason so the failure is diagnosable.
	return Summary{}, fmt.Errorf("summarize: response missing %s tool_use block (stop_reason=%q)", toolName, resp.StopReason)
}

// buildPrompt renders the article context into the user message. The extracted
// markdown is the substance; the title/author/site/url frame it.
func buildPrompt(in SummaryInput) string {
	var b strings.Builder
	b.WriteString("Summarize the following article and emit it with the emit_reading tool.\n\n")
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
