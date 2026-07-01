package readerapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bbell/reading-lite/internal/config"
	"github.com/bbell/reading-lite/internal/readerapi"
	"github.com/bbell/reading-lite/internal/summarize"
)

func TestBuildSummarizer_SelectsConfiguredProvider(t *testing.T) {
	t.Parallel()

	t.Run("anthropic default", func(t *testing.T) {
		t.Parallel()
		cfg := config.Config{
			AnthropicAPIKey: "anthropic-key",
			OpenAIAPIKey:    "openai-key",
			Summary: config.SummaryConfig{
				Provider: config.SummaryProviderAnthropic,
			},
		}

		got := readerapi.BuildSummarizer(cfg)
		if _, ok := got.(*summarize.Anthropic); !ok {
			t.Fatalf("buildSummarizer = %T, want *summarize.Anthropic", got)
		}
	})

	t.Run("openai with configured knobs", func(t *testing.T) {
		t.Parallel()
		cfg := config.Config{
			AnthropicAPIKey: "anthropic-key",
			OpenAIAPIKey:    "openai-key",
			Summary: config.SummaryConfig{
				Provider: config.SummaryProviderOpenAI,
				OpenAI: config.SummaryOpenAIConfig{
					Model:           "gpt-5.5-mini",
					ReasoningEffort: "high",
					MaxOutputTokens: 32000,
				},
			},
		}

		var gotAuth string
		var gotReq struct {
			Model           string `json:"model"`
			MaxOutputTokens int    `json:"max_output_tokens"`
			Reasoning       struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
				t.Errorf("decode request: %v", err)
			}
			_, _ = w.Write([]byte(`{"status":"completed","output":[{"type":"message","status":"completed","content":[{"type":"output_text","text":"{\"title\":\"T\",\"summary\":\"S\",\"tags\":[]}"}]}]}`))
		}))
		defer srv.Close()

		got := readerapi.BuildSummarizer(cfg, summarize.WithOpenAIBaseURL(srv.URL))
		if _, ok := got.(*summarize.OpenAI); !ok {
			t.Fatalf("buildSummarizer = %T, want *summarize.OpenAI", got)
		}
		if _, err := got.Summarize(context.Background(), summarize.SummaryInput{Markdown: "body"}); err != nil {
			t.Fatalf("Summarize: %v", err)
		}
		if gotAuth != "Bearer openai-key" {
			t.Fatalf("Authorization = %q, want existing OpenAI key", gotAuth)
		}
		if gotReq.Model != "gpt-5.5-mini" || gotReq.Reasoning.Effort != "high" || gotReq.MaxOutputTokens != 32000 {
			t.Fatalf("request config = model %q effort %q max %d, want configured OpenAI knobs", gotReq.Model, gotReq.Reasoning.Effort, gotReq.MaxOutputTokens)
		}
	})
}
