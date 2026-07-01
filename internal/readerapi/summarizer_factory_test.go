package readerapi_test

import (
	"reflect"
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

		got := readerapi.BuildSummarizer(cfg)
		if _, ok := got.(*summarize.OpenAI); !ok {
			t.Fatalf("buildSummarizer = %T, want *summarize.OpenAI", got)
		}
		v := reflect.ValueOf(got).Elem()
		if model := v.FieldByName("model").String(); model != "gpt-5.5-mini" {
			t.Fatalf("model = %q, want configured model", model)
		}
		if effort := v.FieldByName("reasoningEffort").String(); effort != "high" {
			t.Fatalf("reasoningEffort = %q, want configured effort", effort)
		}
		if tokenLimit := int(v.FieldByName("maxOutputTokens").Int()); tokenLimit != 32000 {
			t.Fatalf("maxOutputTokens = %d, want configured tokens", tokenLimit)
		}
		if apiKey := v.FieldByName("apiKey").String(); apiKey != "openai-key" {
			t.Fatalf("apiKey = %q, want existing OpenAI key", apiKey)
		}
	})
}
