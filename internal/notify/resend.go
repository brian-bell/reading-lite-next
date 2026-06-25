package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	resendDefaultBaseURL = "https://api.resend.com"
	resendEmailsPath     = "/emails"
	errSnippetMax        = 512
)

// Resend is the production [Notifier]: it posts an email to the Resend API. A
// non-2xx response is returned as an error; the pipeline treats notification as
// best-effort and never fails a reading on a notify error, so Resend does no
// retry classification.
type Resend struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

// Option configures a [Resend] notifier.
type Option func(*Resend)

// WithBaseURL overrides the API base URL (used to point at a test server).
func WithBaseURL(u string) Option {
	return func(r *Resend) { r.baseURL = u }
}

// WithHTTPClient overrides the underlying http.Client.
func WithHTTPClient(c *http.Client) Option {
	return func(r *Resend) { r.client = c }
}

// NewResend returns a Resend notifier authenticated with apiKey.
func NewResend(apiKey string, opts ...Option) *Resend {
	r := &Resend{
		apiKey:  apiKey,
		baseURL: resendDefaultBaseURL,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

type resendRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	HTML    string   `json:"html"`
}

// Notify sends n via Resend, returning an error on any non-2xx response.
func (r *Resend) Notify(ctx context.Context, n Email) error {
	reqBody, err := json.Marshal(resendRequest{
		From:    n.From,
		To:      []string{n.To},
		Subject: n.Subject,
		HTML:    n.HTML,
	})
	if err != nil {
		return fmt.Errorf("notify: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+resendEmailsPath, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("notify: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("notify: post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, errSnippetMax))
		return fmt.Errorf("notify: status %d: %s", resp.StatusCode, bytes.TrimSpace(snippet))
	}
	return nil
}
