package summarize

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/bbell/reading-lite/internal/httpx"
)

// BatchClient creates Anthropic Message Batches, retrieves their metadata, and
// downloads their result JSONL.
type BatchClient interface {
	NewBatchRequest(customID string, in SummaryInput) BatchRequest
	CreateBatch(ctx context.Context, requests []BatchRequest) (Batch, error)
	GetBatch(ctx context.Context, id string) (Batch, error)
	BatchResults(ctx context.Context, id string, expectedCustomIDs []string) ([]BatchResult, error)
}

// BatchRequest is one request item in an Anthropic Message Batch.
type BatchRequest struct {
	CustomID string
	params   anthropicRequest
}

// Batch is Anthropic Message Batch metadata.
type Batch struct {
	ID               string             `json:"id"`
	Type             string             `json:"type"`
	ProcessingStatus string             `json:"processing_status"`
	RequestCounts    BatchRequestCounts `json:"request_counts"`
	ResultsURL       string             `json:"results_url"`
}

// BatchRequestCounts reports Anthropic's per-outcome batch item counts.
type BatchRequestCounts struct {
	Processing int `json:"processing"`
	Succeeded  int `json:"succeeded"`
	Errored    int `json:"errored"`
	Canceled   int `json:"canceled"`
	Expired    int `json:"expired"`
}

// BatchResultType is the terminal outcome of one Anthropic batch item.
type BatchResultType string

// Batch result types returned by Anthropic Message Batches.
const (
	BatchResultSucceeded BatchResultType = "succeeded"
	BatchResultErrored   BatchResultType = "errored"
	BatchResultCanceled  BatchResultType = "canceled"
	BatchResultExpired   BatchResultType = "expired"
)

// BatchResultError is Anthropic's error payload for an errored batch item.
type BatchResultError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type anthropicErrorEnvelope struct {
	Type  string           `json:"type"`
	Error BatchResultError `json:"error"`
}

// BatchResult is one parsed Anthropic batch result.
type BatchResult struct {
	CustomID string
	Type     BatchResultType
	Summary  Summary
	Error    BatchResultError
}

// NewBatchRequest builds one batch item using the same forced emit_reading
// Messages request shape as [Anthropic.Summarize].
func (a *Anthropic) NewBatchRequest(customID string, in SummaryInput) BatchRequest {
	return BatchRequest{
		CustomID: customID,
		params:   a.messageRequest(in),
	}
}

// MarshalJSON emits the Anthropic batch request shape while keeping the raw
// Messages params internal to this package.
func (r BatchRequest) MarshalJSON() ([]byte, error) {
	var wire struct {
		CustomID string           `json:"custom_id"`
		Params   anthropicRequest `json:"params"`
	}
	wire.CustomID = r.CustomID
	wire.Params = r.params
	return json.Marshal(wire)
}

// UnmarshalJSON accepts the persisted Anthropic batch request shape without
// requiring callers to name the internal Messages params type.
func (r *BatchRequest) UnmarshalJSON(b []byte) error {
	var wire struct {
		CustomID string           `json:"custom_id"`
		Params   anthropicRequest `json:"params"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	r.CustomID = wire.CustomID
	r.params = wire.Params
	return nil
}

// CreateBatch submits a set of message requests to Anthropic Message Batches.
func (a *Anthropic) CreateBatch(ctx context.Context, requests []BatchRequest) (Batch, error) {
	var body struct {
		Requests []BatchRequest `json:"requests"`
	}
	body.Requests = requests

	reqBody, err := json.Marshal(body)
	if err != nil {
		return Batch{}, fmt.Errorf("summarize batch: marshal create request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+anthropicBatchesPath, bytes.NewReader(reqBody))
	if err != nil {
		return Batch{}, fmt.Errorf("summarize batch: build create request: %w", err)
	}
	a.setHeaders(req)

	resp, err := a.client.Do(req)
	if err != nil {
		return Batch{}, fmt.Errorf("summarize batch: create: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Batch{}, httpx.ClassifyResponse("summarize batch", resp)
	}

	var parsed Batch
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return Batch{}, fmt.Errorf("summarize batch: decode create response: %w", err)
	}
	return parsed, nil
}

// GetBatch retrieves Anthropic Message Batch metadata by id.
func (a *Anthropic) GetBatch(ctx context.Context, id string) (Batch, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+anthropicBatchesPath+"/"+url.PathEscape(id), nil)
	if err != nil {
		return Batch{}, fmt.Errorf("summarize batch: build get request: %w", err)
	}
	a.setHeaders(req)

	resp, err := a.client.Do(req)
	if err != nil {
		return Batch{}, fmt.Errorf("summarize batch: get: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Batch{}, httpx.ClassifyResponse("summarize batch", resp)
	}

	var parsed Batch
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return Batch{}, fmt.Errorf("summarize batch: decode get response: %w", err)
	}
	return parsed, nil
}

// BatchResults downloads and parses Anthropic Message Batch result JSONL.
func (a *Anthropic) BatchResults(ctx context.Context, id string, expectedCustomIDs []string) ([]BatchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+anthropicBatchesPath+"/"+url.PathEscape(id)+"/results", nil)
	if err != nil {
		return nil, fmt.Errorf("summarize batch: build results request: %w", err)
	}
	a.setHeaders(req)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("summarize batch: results: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, httpx.ClassifyResponse("summarize batch", resp)
	}

	results, err := ParseBatchResults(resp.Body, expectedCustomIDs)
	if err != nil {
		return nil, err
	}
	return results, nil
}

// ParseBatchResults parses Anthropic's JSONL results, validates every custom_id
// against the submitted custom IDs, and returns results in that expected order.
func ParseBatchResults(r io.Reader, expectedCustomIDs []string) ([]BatchResult, error) {
	order := make([]string, 0, len(expectedCustomIDs))
	want := make(map[string]struct{}, len(expectedCustomIDs))
	for _, customID := range expectedCustomIDs {
		if customID == "" {
			return nil, fmt.Errorf("summarize batch: expected request has empty custom_id")
		}
		if _, ok := want[customID]; ok {
			return nil, fmt.Errorf("summarize batch: duplicate expected custom_id %q", customID)
		}
		want[customID] = struct{}{}
		order = append(order, customID)
	}

	seen := make(map[string]BatchResult, len(expectedCustomIDs))
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var raw struct {
			CustomID string `json:"custom_id"`
			Result   struct {
				Type    BatchResultType        `json:"type"`
				Message anthropicResponse      `json:"message"`
				Error   anthropicErrorEnvelope `json:"error"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			return nil, fmt.Errorf("summarize batch: decode result line %d: %w", lineNo, err)
		}
		if _, ok := want[raw.CustomID]; !ok {
			return nil, fmt.Errorf("summarize batch: unknown custom_id %q on line %d", raw.CustomID, lineNo)
		}
		if _, ok := seen[raw.CustomID]; ok {
			return nil, fmt.Errorf("summarize batch: duplicate custom_id %q on line %d", raw.CustomID, lineNo)
		}

		out := BatchResult{CustomID: raw.CustomID, Type: raw.Result.Type}
		switch raw.Result.Type {
		case BatchResultSucceeded:
			summary, err := parseToolUse(raw.Result.Message)
			if err != nil {
				return nil, fmt.Errorf("summarize batch: parse %q result: %w", raw.CustomID, err)
			}
			out.Summary = summary
		case BatchResultErrored:
			errPayload := raw.Result.Error.Error
			if strings.TrimSpace(errPayload.Type) == "" || strings.TrimSpace(errPayload.Message) == "" {
				return nil, fmt.Errorf("summarize batch: errored result for custom_id %q missing error payload", raw.CustomID)
			}
			out.Error = errPayload
		case BatchResultCanceled, BatchResultExpired:
		default:
			return nil, fmt.Errorf("summarize batch: unknown result type %q for custom_id %q", raw.Result.Type, raw.CustomID)
		}
		seen[raw.CustomID] = out
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("summarize batch: read results: %w", err)
	}

	out := make([]BatchResult, 0, len(order))
	for _, customID := range order {
		result, ok := seen[customID]
		if !ok {
			return nil, fmt.Errorf("summarize batch: missing result for custom_id %q", customID)
		}
		out = append(out, result)
	}
	return out, nil
}
