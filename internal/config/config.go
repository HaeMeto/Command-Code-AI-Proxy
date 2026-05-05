// Package config loads runtime configuration from environment variables.
package config

import (
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime knobs for the proxy. It is constructed once
// at startup and passed by value/pointer through the application.
type Config struct {
	Port           int
	UpstreamURL    string
	DefaultToken   string
	DefaultVersion string
	DBPath         string
	CORSOrigins    []string
	AdminToken     string
	OpenAIModels   []string
}

// Load reads configuration from environment variables, falling back to
// sensible defaults so the proxy can run without any setup.
func Load() Config {
	return Config{
		Port:           atoi(getenv("PORT", "3000"), 3000),
		UpstreamURL:    getenv("COMMAND_CODE_API_URL", "https://api.commandcode.ai"),
		DefaultToken:   os.Getenv("COMMAND_CODE_TOKEN"),
		DefaultVersion: getenv("COMMAND_CODE_VERSION", "0.18.10"),
		DBPath:         getenv("ANALYTICS_DB_PATH", "./data/analytics.db"),
		CORSOrigins:    splitCSV(getenv("CORS_ORIGINS", "*")),
		AdminToken:     os.Getenv("ADMIN_TOKEN"),
		OpenAIModels:   splitCSV(getenv("OPENAI_MODELS", "moonshotai/Kimi-K2.5")),
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func atoi(s string, fallback int) int {
	if v, err := strconv.Atoi(s); err == nil {
		return v
	}
	return fallback
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
