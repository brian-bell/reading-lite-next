package config_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bbell/reading-lite/internal/config"
)

func TestLoadEnv_MinimalValidConfigWithDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.LoadEnv(validEnv())
	if err != nil {
		t.Fatalf("LoadEnv: %v", err)
	}

	if cfg.APIToken != "api-token" {
		t.Fatalf("APIToken = %q, want api-token", cfg.APIToken)
	}
	if cfg.DatabaseURL != "postgres://reader:secret@db.example.com:5432/reading?sslmode=require" {
		t.Fatalf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.R2.Region != "auto" {
		t.Fatalf("R2.Region = %q, want auto", cfg.R2.Region)
	}
	if cfg.PendingTTL != 15*time.Minute || cfg.RunningTTL != 20*time.Minute {
		t.Fatalf("TTLs = %v/%v, want 15m/20m", cfg.PendingTTL, cfg.RunningTTL)
	}
	if cfg.MaxAttempts != 5 || cfg.WorkerConcurrency != 3 || cfg.DispatchBuffer != 64 || cfg.PGMaxConns != 7 {
		t.Fatalf("tuning = max %d workers %d buffer %d conns %d, want 5/3/64/7", cfg.MaxAttempts, cfg.WorkerConcurrency, cfg.DispatchBuffer, cfg.PGMaxConns)
	}
	if cfg.ListenAddr != "127.0.0.1:8080" {
		t.Fatalf("ListenAddr = %q, want 127.0.0.1:8080", cfg.ListenAddr)
	}
	if cfg.FetchTimeout != 30*time.Second {
		t.Fatalf("FetchTimeout = %v, want 30s default", cfg.FetchTimeout)
	}
	if cfg.FetchMaxBytes != 10<<20 {
		t.Fatalf("FetchMaxBytes = %d, want 10MiB default", cfg.FetchMaxBytes)
	}
	if cfg.ShutdownTimeout != 10*time.Second {
		t.Fatalf("ShutdownTimeout = %v, want 10s default", cfg.ShutdownTimeout)
	}
	if cfg.Notify.From != "reader@example.com" || cfg.Notify.To != "me@example.com" {
		t.Fatalf("notify = %q/%q, want configured addresses", cfg.Notify.From, cfg.Notify.To)
	}
	if cfg.Summary.Provider != config.SummaryProviderAnthropic {
		t.Fatalf("Summary.Provider = %q, want anthropic default", cfg.Summary.Provider)
	}
	if cfg.Summary.OpenAI.Model != "gpt-5.5" || cfg.Summary.OpenAI.ReasoningEffort != "medium" || cfg.Summary.OpenAI.MaxOutputTokens != 25000 {
		t.Fatalf("OpenAI summary defaults = %+v, want gpt-5.5/medium/25000", cfg.Summary.OpenAI)
	}
}

func TestLoadEnv_CORSAllowedOriginsDefaultsToNone(t *testing.T) {
	t.Parallel()

	cfg, err := config.LoadEnv(validEnv())
	if err != nil {
		t.Fatalf("LoadEnv: %v", err)
	}
	if len(cfg.CORSAllowedOrigins) != 0 {
		t.Fatalf("CORSAllowedOrigins = %v, want none", cfg.CORSAllowedOrigins)
	}

	cfg, err = config.LoadEnv(validEnv("CORS_ALLOWED_ORIGINS= \t "))
	if err != nil {
		t.Fatalf("LoadEnv with blank CORS_ALLOWED_ORIGINS: %v", err)
	}
	if len(cfg.CORSAllowedOrigins) != 0 {
		t.Fatalf("blank CORSAllowedOrigins = %v, want none", cfg.CORSAllowedOrigins)
	}
}

func TestLoadEnv_CORSAllowedOriginsParsesAllowlist(t *testing.T) {
	t.Parallel()

	cfg, err := config.LoadEnv(validEnv("CORS_ALLOWED_ORIGINS= HTTPS://APP.Example.com , http://localhost:5173, https://app.example.com, http://127.0.0.1:5173, https://app.example.com:443, HTTP://APP.Example.com:80 "))
	if err != nil {
		t.Fatalf("LoadEnv: %v", err)
	}
	want := []string{
		"https://app.example.com",
		"http://localhost:5173",
		"http://127.0.0.1:5173",
		"http://app.example.com",
	}
	if !slices.Equal(cfg.CORSAllowedOrigins, want) {
		t.Fatalf("CORSAllowedOrigins = %v, want %v", cfg.CORSAllowedOrigins, want)
	}
}

func TestLoadEnv_CORSAllowedOriginsRejectsUnsafeValuesWithoutLeaks(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"wildcard", "*"},
		{"invalid URL", "://not-a-url"},
		{"ftp", "ftp://app.example.com"},
		{"userinfo", "https://user@app.example.com"},
		{"path", "https://app.example.com/path"},
		{"query", "https://app.example.com?x=1"},
		{"fragment", "https://app.example.com#x"},
		{"doubled comma", "https://app.example.com,,https://other.example.com"},
		{"trailing comma", "https://app.example.com,"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.LoadEnv(validEnv("CORS_ALLOWED_ORIGINS=" + tc.raw))
			if err == nil {
				t.Fatal("LoadEnv = nil error, want invalid CORS_ALLOWED_ORIGINS")
			}
			if !strings.Contains(err.Error(), "CORS_ALLOWED_ORIGINS") {
				t.Fatalf("error = %q, want CORS_ALLOWED_ORIGINS named", err)
			}
			if strings.Contains(err.Error(), tc.raw) {
				t.Fatalf("error = %q leaked raw CORS value %q", err, tc.raw)
			}
		})
	}
}

func TestLoadEnv_ErrorsNameFieldsButRedactValues(t *testing.T) {
	t.Parallel()

	secret := "sentinel-secret-value"
	_, err := config.LoadEnv(validEnv(
		"READER_API_TOKEN=",
		"OPENAI_API_KEY="+secret,
		"DATABASE_URL=postgres://reader:"+secret+"@db.example.com:5432/reading?sslmode=disable",
		"R2_SECRET_ACCESS_KEY=",
	))
	if err == nil {
		t.Fatal("LoadEnv = nil error, want validation error")
	}
	msg := err.Error()
	for _, want := range []string{"READER_API_TOKEN", "DATABASE_URL", "R2_SECRET_ACCESS_KEY"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not name %s", msg, want)
		}
	}
	if strings.Contains(msg, secret) || strings.Contains(msg, "sslmode=disable") {
		t.Fatalf("error %q leaked submitted values", msg)
	}
}

func TestLoadEnv_DatabaseURLRequiresPostgresWithStrictSSLMode(t *testing.T) {
	t.Parallel()

	bad := map[string]string{
		"missing":         "",
		"bad scheme":      "mysql://reader:secret@db.example.com/reading?sslmode=require",
		"missing sslmode": "postgres://reader:secret@db.example.com/reading",
		"disable":         "postgres://reader:secret@db.example.com/reading?sslmode=disable",
		"allow":           "postgres://reader:secret@db.example.com/reading?sslmode=allow",
		"prefer":          "postgres://reader:secret@db.example.com/reading?sslmode=prefer",
		"malformed":       "://not-a-url",
	}
	for name, raw := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := config.LoadEnv(validEnv("DATABASE_URL=" + raw))
			if err == nil {
				t.Fatal("LoadEnv = nil error, want invalid DATABASE_URL")
			}
			if !strings.Contains(err.Error(), "DATABASE_URL") {
				t.Fatalf("error = %q, want DATABASE_URL named", err)
			}
			if raw != "" && strings.Contains(err.Error(), raw) {
				t.Fatalf("error = %q leaked raw URL", err)
			}
		})
	}

	for _, sslmode := range []string{"require", "verify-ca", "verify-full"} {
		t.Run(sslmode, func(t *testing.T) {
			_, err := config.LoadEnv(validEnv("DATABASE_URL=postgresql://reader:secret@db.example.com/reading?sslmode=" + sslmode))
			if err != nil {
				t.Fatalf("LoadEnv with sslmode=%s: %v", sslmode, err)
			}
		})
	}
}

func TestLoadEnv_TuningRequiresPositiveValues(t *testing.T) {
	t.Parallel()

	for _, field := range []string{
		"PENDING_TTL", "RUNNING_TTL", "MAX_ATTEMPTS", "WORKER_CONCURRENCY",
		"DISPATCH_BUFFER", "PG_MAX_CONNS", "FETCH_TIMEOUT", "FETCH_MAX_BYTES", "SHUTDOWN_TIMEOUT",
		"SUMMARY_OPENAI_MAX_OUTPUT_TOKENS",
	} {
		t.Run(field, func(t *testing.T) {
			_, err := config.LoadEnv(validEnv(field + "=0"))
			if err == nil {
				t.Fatal("LoadEnv = nil error, want positive-value validation")
			}
			if !strings.Contains(err.Error(), field) {
				t.Fatalf("error = %q, want %s named", err, field)
			}
		})
	}
}

func TestLoadEnv_SummaryProviderSelectsOpenAIWithoutAnthropicKey(t *testing.T) {
	t.Parallel()

	cfg, err := config.LoadEnv(validEnv(
		"SUMMARY_PROVIDER=openai",
		"ANTHROPIC_API_KEY=",
		"SUMMARY_OPENAI_MODEL=gpt-5.5-mini",
		"SUMMARY_OPENAI_REASONING_EFFORT=high",
		"SUMMARY_OPENAI_MAX_OUTPUT_TOKENS=32000",
	))
	if err != nil {
		t.Fatalf("LoadEnv: %v", err)
	}
	if cfg.OpenAIAPIKey != "openai-key" {
		t.Fatalf("OpenAIAPIKey = %q, want embedding/summarizer key", cfg.OpenAIAPIKey)
	}
	if cfg.AnthropicAPIKey != "" {
		t.Fatalf("AnthropicAPIKey = %q, want empty allowed for OpenAI summarizer", cfg.AnthropicAPIKey)
	}
	if cfg.Summary.Provider != config.SummaryProviderOpenAI {
		t.Fatalf("Summary.Provider = %q, want openai", cfg.Summary.Provider)
	}
	if cfg.Summary.OpenAI.Model != "gpt-5.5-mini" || cfg.Summary.OpenAI.ReasoningEffort != "high" || cfg.Summary.OpenAI.MaxOutputTokens != 32000 {
		t.Fatalf("OpenAI summary config = %+v, want override values", cfg.Summary.OpenAI)
	}
}

func TestLoadEnv_SummaryOpenAIMaxOutputTokensFloor(t *testing.T) {
	t.Parallel()

	_, err := config.LoadEnv(validEnv("SUMMARY_OPENAI_MAX_OUTPUT_TOKENS=15"))
	if err == nil {
		t.Fatal("LoadEnv = nil error, want floor validation for max output tokens below 16")
	}
	if !strings.Contains(err.Error(), "SUMMARY_OPENAI_MAX_OUTPUT_TOKENS") {
		t.Fatalf("error = %q, want SUMMARY_OPENAI_MAX_OUTPUT_TOKENS named", err)
	}
	if !strings.Contains(err.Error(), "16") {
		t.Fatalf("error = %q, want the OpenAI floor 16 stated", err)
	}

	cfg, err := config.LoadEnv(validEnv("SUMMARY_OPENAI_MAX_OUTPUT_TOKENS=16"))
	if err != nil {
		t.Fatalf("LoadEnv with floor value 16: %v", err)
	}
	if cfg.Summary.OpenAI.MaxOutputTokens != 16 {
		t.Fatalf("MaxOutputTokens = %d, want 16", cfg.Summary.OpenAI.MaxOutputTokens)
	}
}

func TestLoadEnv_SummaryProviderValidation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		env   []string
		field string
	}{
		{"invalid provider", []string{"SUMMARY_PROVIDER=bogus"}, "SUMMARY_PROVIDER"},
		{"invalid reasoning effort", []string{"SUMMARY_OPENAI_REASONING_EFFORT=huge"}, "SUMMARY_OPENAI_REASONING_EFFORT"},
		{"non integer max tokens", []string{"SUMMARY_OPENAI_MAX_OUTPUT_TOKENS=lots"}, "SUMMARY_OPENAI_MAX_OUTPUT_TOKENS"},
		{"anthropic provider requires key", []string{"SUMMARY_PROVIDER=anthropic", "ANTHROPIC_API_KEY="}, "ANTHROPIC_API_KEY"},
		{"default provider requires key", []string{"ANTHROPIC_API_KEY="}, "ANTHROPIC_API_KEY"},
		{"openai provider still requires openai key", []string{"SUMMARY_PROVIDER=openai", "OPENAI_API_KEY="}, "OPENAI_API_KEY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.LoadEnv(validEnv(tc.env...))
			if err == nil {
				t.Fatal("LoadEnv = nil error, want validation failure")
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("error = %q, want %s named", err, tc.field)
			}
		})
	}
}

func TestLoadEnv_ValidatesOperationalEndpointsAndRanges(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		field string
		value string
	}{
		{"LISTEN_ADDR", "not an address"},
		{"LISTEN_ADDR", "127.0.0.1"},
		{"LISTEN_ADDR", "127.0.0.1:notaport"},
		{"LISTEN_ADDR", "127.0.0.1:70000"},
		{"R2_ENDPOINT", "not a url"},
		{"R2_ENDPOINT", "ftp://account.example.com"},
		{"R2_ENDPOINT", "http://account.example.com"},
		{"PG_MAX_CONNS", "2147483648"},
	} {
		t.Run(tc.field+"="+tc.value, func(t *testing.T) {
			_, err := config.LoadEnv(validEnv(tc.field + "=" + tc.value))
			if err == nil {
				t.Fatal("LoadEnv = nil error, want validation failure")
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("error = %q, want %s named", err, tc.field)
			}
			if strings.Contains(err.Error(), tc.value) {
				t.Fatalf("error = %q leaked raw value %q", err, tc.value)
			}
		})
	}
}

func TestLoadEnv_AdapterAndNotificationFieldsRequired(t *testing.T) {
	t.Parallel()

	for _, field := range []string{
		"OPENAI_API_KEY", "R2_ENDPOINT", "R2_ACCESS_KEY_ID",
		"R2_SECRET_ACCESS_KEY", "R2_BUCKET", "RESEND_API_KEY", "NOTIFY_FROM", "NOTIFY_TO",
	} {
		t.Run(field, func(t *testing.T) {
			_, err := config.LoadEnv(validEnv(field + "="))
			if err == nil {
				t.Fatal("LoadEnv = nil error, want missing-field validation")
			}
			if !strings.Contains(err.Error(), field) {
				t.Fatalf("error = %q, want %s named", err, field)
			}
		})
	}
}

func TestConfig_SafeFieldsExcludeSecrets(t *testing.T) {
	t.Parallel()

	cfg, err := config.LoadEnv(validEnv(
		"READER_API_TOKEN=sentinel-api-token",
		"OPENAI_API_KEY=sentinel-openai",
		"ANTHROPIC_API_KEY=sentinel-anthropic",
		"R2_SECRET_ACCESS_KEY=sentinel-r2-secret",
		"RESEND_API_KEY=sentinel-resend",
	))
	if err != nil {
		t.Fatalf("LoadEnv: %v", err)
	}
	logValue := config.LogValue(cfg)
	for _, secret := range []string{"sentinel-api-token", "sentinel-openai", "sentinel-anthropic", "sentinel-r2-secret", "sentinel-resend", "secret@db.example.com"} {
		if strings.Contains(logValue, secret) {
			t.Fatalf("LogValue leaked %q in %q", secret, logValue)
		}
	}
	for _, want := range []string{"listen_addr", "worker_concurrency", "pg_max_conns", "r2_bucket", "notify_from"} {
		if !strings.Contains(logValue, want) {
			t.Fatalf("LogValue %q missing operational field %s", logValue, want)
		}
	}
	for _, want := range []string{`"summary_provider":"anthropic"`, `"summary_openai_model":"gpt-5.5"`, `"summary_openai_reasoning_effort":"medium"`, `"summary_openai_max_output_tokens":25000`} {
		if !strings.Contains(logValue, want) {
			t.Fatalf("LogValue %q missing summary field %s", logValue, want)
		}
	}
}

func TestConfig_SafeFieldsLogsCORSCountOnly(t *testing.T) {
	t.Parallel()

	cfg, err := config.LoadEnv(validEnv("CORS_ALLOWED_ORIGINS=https://app.example.com,http://localhost:5173"))
	if err != nil {
		t.Fatalf("LoadEnv: %v", err)
	}
	logValue := config.LogValue(cfg)
	if !strings.Contains(logValue, `"cors_allowed_origin_count":2`) {
		t.Fatalf("LogValue = %q, want CORS origin count", logValue)
	}
	for _, forbidden := range []string{"https://app.example.com", "http://localhost:5173"} {
		if strings.Contains(logValue, forbidden) {
			t.Fatalf("LogValue leaked CORS origin %q in %q", forbidden, logValue)
		}
	}
}

func validEnv(overrides ...string) []string {
	env := []string{
		"READER_API_TOKEN=api-token",
		"DATABASE_URL=postgres://reader:secret@db.example.com:5432/reading?sslmode=require",
		"OPENAI_API_KEY=openai-key",
		"ANTHROPIC_API_KEY=anthropic-key",
		"R2_ENDPOINT=https://account.r2.cloudflarestorage.com",
		"R2_ACCESS_KEY_ID=r2-access",
		"R2_SECRET_ACCESS_KEY=r2-secret",
		"R2_BUCKET=reading",
		"PENDING_TTL=15m",
		"RUNNING_TTL=20m",
		"MAX_ATTEMPTS=5",
		"WORKER_CONCURRENCY=3",
		"DISPATCH_BUFFER=64",
		"PG_MAX_CONNS=7",
		"LISTEN_ADDR=127.0.0.1:8080",
		"RESEND_API_KEY=resend-key",
		"NOTIFY_FROM=reader@example.com",
		"NOTIFY_TO=me@example.com",
	}
	env = append(env, overrides...)
	return env
}
