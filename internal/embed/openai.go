package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/bbell/reading-lite/internal/dispatch"
	"github.com/bbell/reading-lite/internal/httpx"
)

// OpenAI request/response defaults.
const (
	openAIDefaultBaseURL = "https://api.openai.com"
	openAIModel          = "text-embedding-3-small"
	openAIEmbedPath      = "/v1/embeddings"
)

// OpenAI is the production [Embedder]: it calls OpenAI's embeddings endpoint and
// returns the first result's vector. HTTP errors are classified for the dispatcher:
// 429 → [dispatch.RateLimitError] (honoring Retry-After), 4xx → permanent, 5xx →
// transient retry.
type OpenAI struct {
	apiKey  string
	baseURL string
	model   string
	client  *http.Client
}

// Option configures an [OpenAI] embedder.
type Option func(*OpenAI)

// WithBaseURL overrides the API base URL (used to point at a test server).
func WithBaseURL(u string) Option {
	return func(o *OpenAI) { o.baseURL = u }
}

// WithModel overrides the embedding model.
func WithModel(m string) Option {
	return func(o *OpenAI) { o.model = m }
}

// WithHTTPClient overrides the underlying http.Client.
func WithHTTPClient(c *http.Client) Option {
	return func(o *OpenAI) { o.client = c }
}

// NewOpenAI returns an OpenAI embedder authenticated with apiKey.
func NewOpenAI(apiKey string, opts ...Option) *OpenAI {
	o := &OpenAI{
		apiKey:  apiKey,
		baseURL: openAIDefaultBaseURL,
		model:   openAIModel,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

type embeddingRequest struct {
	Input string `json:"input"`
	Model string `json:"model"`
}

type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed returns the embedding vector for text.
func (o *OpenAI) Embed(ctx context.Context, text string) ([]float32, error) {
	reqBody, err := json.Marshal(embeddingRequest{Input: text, Model: o.model})
	if err != nil {
		return nil, fmt.Errorf("embed: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+openAIEmbedPath, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("embed: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+o.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, httpx.ClassifyResponse("embed", resp)
	}

	var parsed embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("embed: decode response: %w", err)
	}
	if len(parsed.Data) == 0 {
		return nil, fmt.Errorf("embed: response carried no embedding data")
	}
	// The Embedder contract (and the vector index) require a Dim-length vector. A
	// different length means a misconfigured model or a bad response; fail
	// permanently rather than passing it downstream to fail (and retry) at Upsert.
	if got := len(parsed.Data[0].Embedding); got != Dim {
		return nil, fmt.Errorf("%w: embed: response vector has %d dimensions, want %d", dispatch.ErrPermanent, got, Dim)
	}
	return parsed.Data[0].Embedding, nil
}
