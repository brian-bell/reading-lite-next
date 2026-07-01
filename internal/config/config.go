// Package config loads reader-api runtime configuration from environment values.
package config

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// DefaultFetchTimeout bounds outbound source fetches when FETCH_TIMEOUT is unset.
	DefaultFetchTimeout = 30 * time.Second
	// DefaultFetchMaxBytes bounds fetched response bodies when FETCH_MAX_BYTES is unset.
	DefaultFetchMaxBytes = 10 << 20
	// DefaultShutdownTimeout bounds graceful shutdown when SHUTDOWN_TIMEOUT is unset.
	DefaultShutdownTimeout = 10 * time.Second
	// DefaultR2Region is Cloudflare R2's default signing region.
	DefaultR2Region = "auto"
	// DefaultSummaryProvider preserves the existing Anthropic summarizer unless
	// OpenAI is explicitly selected.
	DefaultSummaryProvider = SummaryProviderAnthropic
	// DefaultSummaryOpenAIModel is the OpenAI Responses model for summaries.
	DefaultSummaryOpenAIModel = "gpt-5.5"
	// DefaultSummaryOpenAIReasoningEffort is the Responses reasoning effort for summaries.
	DefaultSummaryOpenAIReasoningEffort = "medium"
	// DefaultSummaryOpenAIMaxOutputTokens reserves room for reasoning and visible output.
	DefaultSummaryOpenAIMaxOutputTokens = 25000
	// MinSummaryOpenAIMaxOutputTokens is the OpenAI Responses API minimum for max_output_tokens.
	MinSummaryOpenAIMaxOutputTokens = 16
)

// SummaryProvider identifies the production summarizer adapter.
type SummaryProvider string

const (
	// SummaryProviderAnthropic selects the Anthropic Messages summarizer.
	SummaryProviderAnthropic SummaryProvider = "anthropic"
	// SummaryProviderOpenAI selects the OpenAI Responses summarizer.
	SummaryProviderOpenAI SummaryProvider = "openai"
)

// Config is the complete production reader-api runtime configuration.
type Config struct {
	APIToken           string
	DatabaseURL        string
	OpenAIAPIKey       string
	AnthropicAPIKey    string
	Summary            SummaryConfig
	R2                 R2Config
	ResendAPIKey       string
	Notify             NotifyConfig
	PendingTTL         time.Duration
	RunningTTL         time.Duration
	MaxAttempts        int
	WorkerConcurrency  int
	DispatchBuffer     int
	PGMaxConns         int32
	ListenAddr         string
	FetchTimeout       time.Duration
	FetchMaxBytes      int64
	ShutdownTimeout    time.Duration
	CORSAllowedOrigins []string
}

// SummaryConfig holds summarizer provider selection and provider-specific knobs.
type SummaryConfig struct {
	Provider SummaryProvider
	OpenAI   SummaryOpenAIConfig
}

// SummaryOpenAIConfig holds OpenAI Responses summarizer options.
type SummaryOpenAIConfig struct {
	Model           string
	ReasoningEffort string
	MaxOutputTokens int
}

// R2Config holds the S3-compatible object-store configuration.
type R2Config struct {
	Endpoint        string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	Bucket          string
}

// NotifyConfig holds the ready-notification sender and recipient.
type NotifyConfig struct {
	From string
	To   string
}

// LoadEnv parses and validates a process environ slice.
func LoadEnv(environ []string) (Config, error) {
	values := envMap(environ)
	var errs fieldErrors
	cfg := Config{
		APIToken:           required(values, "READER_API_TOKEN", &errs),
		DatabaseURL:        required(values, "DATABASE_URL", &errs),
		OpenAIAPIKey:       required(values, "OPENAI_API_KEY", &errs),
		ResendAPIKey:       required(values, "RESEND_API_KEY", &errs),
		PendingTTL:         requiredDuration(values, "PENDING_TTL", &errs),
		RunningTTL:         requiredDuration(values, "RUNNING_TTL", &errs),
		MaxAttempts:        requiredPositiveInt(values, "MAX_ATTEMPTS", &errs),
		WorkerConcurrency:  requiredPositiveInt(values, "WORKER_CONCURRENCY", &errs),
		DispatchBuffer:     requiredPositiveInt(values, "DISPATCH_BUFFER", &errs),
		PGMaxConns:         requiredPositiveInt32(values, "PG_MAX_CONNS", &errs),
		ListenAddr:         required(values, "LISTEN_ADDR", &errs),
		FetchTimeout:       optionalDuration(values, "FETCH_TIMEOUT", DefaultFetchTimeout, &errs),
		FetchMaxBytes:      int64(optionalPositiveInt(values, "FETCH_MAX_BYTES", DefaultFetchMaxBytes, &errs)),
		ShutdownTimeout:    optionalDuration(values, "SHUTDOWN_TIMEOUT", DefaultShutdownTimeout, &errs),
		CORSAllowedOrigins: optionalCORSAllowedOrigins(values, &errs),
	}
	cfg.Summary = SummaryConfig{
		Provider: optionalSummaryProvider(values, &errs),
		OpenAI: SummaryOpenAIConfig{
			Model:           optional(values, "SUMMARY_OPENAI_MODEL", DefaultSummaryOpenAIModel),
			ReasoningEffort: optionalSummaryOpenAIReasoningEffort(values, &errs),
			MaxOutputTokens: optionalSummaryOpenAIMaxOutputTokens(values, &errs),
		},
	}
	cfg.AnthropicAPIKey = strings.TrimSpace(values["ANTHROPIC_API_KEY"])
	if cfg.Summary.Provider == SummaryProviderAnthropic && cfg.AnthropicAPIKey == "" {
		errs = append(errs, fieldError{name: "ANTHROPIC_API_KEY", problem: "is required"})
	}
	cfg.R2 = R2Config{
		Endpoint:        required(values, "R2_ENDPOINT", &errs),
		Region:          optional(values, "R2_REGION", DefaultR2Region),
		AccessKeyID:     required(values, "R2_ACCESS_KEY_ID", &errs),
		SecretAccessKey: required(values, "R2_SECRET_ACCESS_KEY", &errs),
		Bucket:          required(values, "R2_BUCKET", &errs),
	}
	cfg.Notify = NotifyConfig{
		From: required(values, "NOTIFY_FROM", &errs),
		To:   required(values, "NOTIFY_TO", &errs),
	}
	if cfg.DatabaseURL != "" {
		validateDatabaseURL(cfg.DatabaseURL, &errs)
	}
	if cfg.ListenAddr != "" {
		validateListenAddr(cfg.ListenAddr, &errs)
	}
	if cfg.R2.Endpoint != "" {
		validateHTTPURL("R2_ENDPOINT", cfg.R2.Endpoint, &errs)
	}
	if len(errs) > 0 {
		return Config{}, errs
	}
	return cfg, nil
}

// SafeFields returns operational config fields that are safe to log.
func (c Config) SafeFields() map[string]any {
	return map[string]any{
		"listen_addr":                      c.ListenAddr,
		"pending_ttl":                      c.PendingTTL.String(),
		"running_ttl":                      c.RunningTTL.String(),
		"max_attempts":                     c.MaxAttempts,
		"worker_concurrency":               c.WorkerConcurrency,
		"dispatch_buffer":                  c.DispatchBuffer,
		"pg_max_conns":                     c.PGMaxConns,
		"r2_region":                        c.R2.Region,
		"r2_bucket":                        c.R2.Bucket,
		"fetch_timeout":                    c.FetchTimeout.String(),
		"fetch_max_bytes":                  c.FetchMaxBytes,
		"shutdown_timeout":                 c.ShutdownTimeout.String(),
		"summary_provider":                 string(c.Summary.Provider),
		"summary_openai_model":             c.Summary.OpenAI.Model,
		"summary_openai_reasoning_effort":  c.Summary.OpenAI.ReasoningEffort,
		"summary_openai_max_output_tokens": c.Summary.OpenAI.MaxOutputTokens,
		"cors_allowed_origin_count":        len(c.CORSAllowedOrigins),
		"notify_from":                      c.Notify.From,
		"notify_to":                        c.Notify.To,
	}
}

// LogValue renders the log-safe subset of cfg. It never includes credentials,
// bearer tokens, or the raw database URL.
func LogValue(cfg Config) string {
	b, err := json.Marshal(cfg.SafeFields())
	if err != nil {
		return "{}"
	}
	return string(b)
}

func envMap(environ []string) map[string]string {
	out := make(map[string]string, len(environ))
	for _, entry := range environ {
		k, v, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		out[k] = v
	}
	return out
}

func required(values map[string]string, name string, errs *fieldErrors) string {
	v := strings.TrimSpace(values[name])
	if v == "" {
		*errs = append(*errs, fieldError{name: name, problem: "is required"})
	}
	return v
}

func optional(values map[string]string, name, def string) string {
	if v := strings.TrimSpace(values[name]); v != "" {
		return v
	}
	return def
}

func optionalSummaryProvider(values map[string]string, errs *fieldErrors) SummaryProvider {
	raw := strings.ToLower(strings.TrimSpace(values["SUMMARY_PROVIDER"]))
	if raw == "" {
		return DefaultSummaryProvider
	}
	switch SummaryProvider(raw) {
	case SummaryProviderAnthropic, SummaryProviderOpenAI:
		return SummaryProvider(raw)
	default:
		*errs = append(*errs, fieldError{name: "SUMMARY_PROVIDER", problem: "must be anthropic or openai"})
		return DefaultSummaryProvider
	}
}

func optionalSummaryOpenAIReasoningEffort(values map[string]string, errs *fieldErrors) string {
	raw := strings.ToLower(strings.TrimSpace(values["SUMMARY_OPENAI_REASONING_EFFORT"]))
	if raw == "" {
		return DefaultSummaryOpenAIReasoningEffort
	}
	switch raw {
	case "none", "minimal", "low", "medium", "high", "xhigh":
		return raw
	default:
		*errs = append(*errs, fieldError{name: "SUMMARY_OPENAI_REASONING_EFFORT", problem: "must be none, minimal, low, medium, high, or xhigh"})
		return DefaultSummaryOpenAIReasoningEffort
	}
}

func optionalSummaryOpenAIMaxOutputTokens(values map[string]string, errs *fieldErrors) int {
	raw := strings.TrimSpace(values["SUMMARY_OPENAI_MAX_OUTPUT_TOKENS"])
	if raw == "" {
		return DefaultSummaryOpenAIMaxOutputTokens
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < MinSummaryOpenAIMaxOutputTokens {
		*errs = append(*errs, fieldError{
			name:    "SUMMARY_OPENAI_MAX_OUTPUT_TOKENS",
			problem: fmt.Sprintf("must be an integer of at least %d", MinSummaryOpenAIMaxOutputTokens),
		})
		return 0
	}
	return n
}

func requiredDuration(values map[string]string, name string, errs *fieldErrors) time.Duration {
	raw := required(values, name, errs)
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		*errs = append(*errs, fieldError{name: name, problem: "must be a positive duration"})
		return 0
	}
	return d
}

func optionalDuration(values map[string]string, name string, def time.Duration, errs *fieldErrors) time.Duration {
	raw := strings.TrimSpace(values[name])
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		*errs = append(*errs, fieldError{name: name, problem: "must be a positive duration"})
		return 0
	}
	return d
}

func requiredPositiveInt(values map[string]string, name string, errs *fieldErrors) int {
	raw := required(values, name, errs)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		*errs = append(*errs, fieldError{name: name, problem: "must be a positive integer"})
		return 0
	}
	return n
}

func optionalPositiveInt(values map[string]string, name string, def int64, errs *fieldErrors) int {
	raw := strings.TrimSpace(values[name])
	if raw == "" {
		return int(def)
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		*errs = append(*errs, fieldError{name: name, problem: "must be a positive integer"})
		return 0
	}
	return n
}

func optionalCORSAllowedOrigins(values map[string]string, errs *fieldErrors) []string {
	raw := strings.TrimSpace(values["CORS_ALLOWED_ORIGINS"])
	if raw == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		origin, ok := parseCORSOrigin(part)
		if !ok {
			*errs = append(*errs, fieldError{name: "CORS_ALLOWED_ORIGINS", problem: "must contain exact http or https origins"})
			return nil
		}
		if _, exists := seen[origin]; exists {
			continue
		}
		seen[origin] = struct{}{}
		out = append(out, origin)
	}
	return out
}

func parseCORSOrigin(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "*" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Host == "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	if u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if strings.TrimSpace(host) == "" {
		return "", false
	}
	port := u.Port()
	if port == "" && strings.Contains(u.Host, ":") && (!strings.HasPrefix(u.Host, "[") || !strings.HasSuffix(u.Host, "]")) {
		return "", false
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", false
		}
		if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
			port = ""
		}
	}
	return scheme + "://" + originHost(host, port), true
}

func originHost(host, port string) string {
	if port != "" {
		return net.JoinHostPort(host, port)
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

func requiredPositiveInt32(values map[string]string, name string, errs *fieldErrors) int32 {
	n := requiredPositiveInt(values, name, errs)
	if n > math.MaxInt32 {
		*errs = append(*errs, fieldError{name: name, problem: "must fit in int32"})
		return 0
	}
	return int32(n)
}

func validateDatabaseURL(raw string, errs *fieldErrors) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Host == "" {
		*errs = append(*errs, fieldError{name: "DATABASE_URL", problem: "must be a valid postgres URL"})
		return
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		*errs = append(*errs, fieldError{name: "DATABASE_URL", problem: "must use postgres or postgresql scheme"})
		return
	}
	if _, err := pgxpool.ParseConfig(raw); err != nil {
		*errs = append(*errs, fieldError{name: "DATABASE_URL", problem: "must be parseable by pgx"})
		return
	}
	switch u.Query().Get("sslmode") {
	case "require", "verify-ca", "verify-full":
	default:
		*errs = append(*errs, fieldError{name: "DATABASE_URL", problem: "must set sslmode=require, verify-ca, or verify-full"})
	}
}

func validateListenAddr(raw string, errs *fieldErrors) {
	_, port, err := net.SplitHostPort(raw)
	if err != nil || port == "" {
		*errs = append(*errs, fieldError{name: "LISTEN_ADDR", problem: "must be host:port"})
		return
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		*errs = append(*errs, fieldError{name: "LISTEN_ADDR", problem: "must use a valid TCP port"})
	}
}

func validateHTTPURL(name, raw string, errs *fieldErrors) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Host == "" {
		*errs = append(*errs, fieldError{name: name, problem: "must be a valid URL"})
		return
	}
	if u.Scheme != "https" {
		*errs = append(*errs, fieldError{name: name, problem: "must use https"})
	}
}

type fieldError struct {
	name    string
	problem string
}

type fieldErrors []fieldError

func (e fieldErrors) Error() string {
	parts := make([]string, len(e))
	for i, err := range e {
		parts[i] = err.name + " " + err.problem
	}
	sort.Strings(parts)
	return fmt.Sprintf("invalid config: %s", strings.Join(parts, "; "))
}
