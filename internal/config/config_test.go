package config_test

import (
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
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "R2_ENDPOINT", "R2_ACCESS_KEY_ID",
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
