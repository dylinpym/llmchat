// Package config loads runtime configuration from environment variables.
//
// Deliberately not viper: this service has a dozen settings, all of them
// env-provided in a container. A struct tag plus one os.Getenv call is easier to
// audit than a config-file precedence system, and `kubectl set env` works.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/dylin/llmchat/internal/contextpack"
	"github.com/dylin/llmchat/internal/service"
)

type Config struct {
	Addr         string
	InternalAddr string
	DatabaseURL  string
	RedisURL     string

	Provider      string // "openai" | "mock"
	OpenAIBaseURL string
	OpenAIAPIKey  string
	DefaultModel  string
	SummaryModel  string

	Ctx service.Config

	// AllowedModelWindow maps a model to its context window, so a misconfigured
	// MaxContext cannot silently overflow a smaller model.
	AllowedOrigins []string

	ShutdownGrace time.Duration
}

// Load reads configuration and validates combinations that would otherwise fail
// at runtime in confusing ways.
func Load() (Config, error) {
	c := Config{
		Addr:           env("ADDR", ":8080"),
		InternalAddr:   env("INTERNAL_ADDR", ":9090"),
		DatabaseURL:    os.Getenv("DATABASE_URL"),
		RedisURL:       env("REDIS_URL", ""),
		Provider:       env("LLM_PROVIDER", "mock"),
		OpenAIBaseURL:  env("OPENAI_BASE_URL", "https://api.openai.com/v1"),
		OpenAIAPIKey:   os.Getenv("OPENAI_API_KEY"),
		DefaultModel:   env("LLM_MODEL", "ecnu-plus"),
		SummaryModel:   env("LLM_SUMMARY_MODEL", ""),
		AllowedOrigins: splitCSV(env("ALLOWED_ORIGINS", "*")),
		ShutdownGrace:  envDuration("SHUTDOWN_GRACE", 15*time.Second),
		Ctx: service.Config{
			Strategy:     contextpack.Strategy(env("CTX_STRATEGY", string(contextpack.StrategyTruncate))),
			MaxContext:   envInt("CTX_MAX_TOKENS", 32_000),
			ReserveReply: envInt("CTX_RESERVE_REPLY", 4_096),
			ReserveInput: envInt("CTX_RESERVE_INPUT", 2_048),
			PinFirst:     envBool("CTX_PIN_FIRST", true),
		},
	}
	if c.SummaryModel == "" {
		c.SummaryModel = c.DefaultModel
	}

	switch c.Provider {
	case "openai":
		if c.OpenAIAPIKey == "" {
			return c, fmt.Errorf("config: LLM_PROVIDER=openai requires OPENAI_API_KEY")
		}
	case "mock":
		// No credentials needed; used in tests and local runs.
	default:
		return c, fmt.Errorf("config: unknown LLM_PROVIDER %q (want openai or mock)", c.Provider)
	}

	if c.Ctx.MaxContext <= c.Ctx.ReserveReply+c.Ctx.ReserveInput {
		// Would leave zero room for history and make every reply amnesiac; far
		// better to refuse to boot than to ship that silently.
		return c, fmt.Errorf(
			"config: CTX_MAX_TOKENS (%d) must exceed reserves (%d+%d)",
			c.Ctx.MaxContext, c.Ctx.ReserveReply, c.Ctx.ReserveInput)
	}

	// No model-window check happens here on purpose. Private gateways routinely
	// serve models this service has never heard of, and CTX_MAX_TOKENS is always
	// trusted over any built-in guess about a model's window. An earlier version
	// looked the model up in a table and discarded the result, which read like
	// validation while doing nothing; a windowOf(model) helper was removed with it.
	// The budget invariant enforced above is the only thing that can make the
	// service unsafe to start.
	return c, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	out := make([]string, 0, 4)
	for _, p := range splitOn(s, ',') {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func splitOn(s string, sep byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			out = append(out, trim(s[start:i]))
			start = i + 1
		}
	}
	return append(out, trim(s[start:]))
}

func trim(s string) string {
	for len(s) > 0 && s[0] == ' ' {
		s = s[1:]
	}
	for len(s) > 0 && s[len(s)-1] == ' ' {
		s = s[:len(s)-1]
	}
	return s
}
