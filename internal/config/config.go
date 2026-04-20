package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config represents the root configuration structure.
type Config struct {
	LLM         LLMConfig
	Sentry      SentryConfig
	Bot         BotConfig
	State       StateConfig
	Logging     LoggingConfig
	Persistence PersistenceConfig
}

const (
	LLMBackendOpenAICompat = "openai_compat"
	LLMBackendOllama       = "ollama"
)

// LLMConfig contains configuration for the LLM service.
type LLMConfig struct {
	Backends              BackendConfig
	Features              FeatureConfig
	ToolLoopMaxIterations int
}

// SentryConfig contains configuration for Sentry error tracking.
type SentryConfig struct {
	DSN string
}

// LoggingConfig contains configuration for structured logging.
type LoggingConfig struct {
	Level string
}

// BotConfig contains configuration for bot settings.
type BotConfig struct {
	AdminIDs                 []int64
	Telegram                 TelegramConfig
	UncompressedHistoryLimit int
	HistorySummaryThreshold  int
	ProcessingTimeout        time.Duration
}

// PersistenceConfig contains durable storage configuration.
type PersistenceConfig struct {
	StorePath string
}

type StateConfig struct {
	MaxBytes           int64
	HistoryMaxBytes    int64
	HistoryStreamsMax  int
	ImageCacheMaxBytes int64
	ImageCacheTTL      time.Duration
}

// BackendConfig contains configuration for all supported LLM backends.
type BackendConfig struct {
	OpenAICompat OpenAICompatBackendConfig
	Ollama       OllamaBackendConfig
}

// OpenAICompatBackendConfig contains configuration for OpenAI-compatible backends.
type OpenAICompatBackendConfig struct {
	BaseURL  string
	APIToken string
}

// OllamaBackendConfig contains configuration for the Ollama native API.
type OllamaBackendConfig struct {
	BaseURL string
}

// FeatureConfig contains per-feature backend routing and model selection.
type FeatureConfig struct {
	Chat             FeatureRouteConfig
	Summarize        FeatureRouteConfig
	ImageRecognition FeatureRouteConfig
	ToolUse          FeatureRouteConfig
}

// FeatureRouteConfig contains backend and model selection for one feature.
type FeatureRouteConfig struct {
	Backend string
	Model   string
}

// TelegramConfig contains configuration for Telegram bot.
type TelegramConfig struct {
	Token string
}

// Load creates a new Config instance populated from environment variables.
func Load() *Config {
	uncompressedHistoryLimit := intFromEnv("LLM_UNCOMPRESSED_HISTORY_LIMIT", 15)
	historySummaryThreshold := intFromEnv("LLM_HISTORY_SUMMARY_THRESHOLD", 5)
	processingTimeout := durationFromEnv("BOT_PROCESSING_TIMEOUT", 30*time.Second)

	stateMaxBytes := int64FromEnv("STATE_MAX_BYTES", int64(256<<20))
	stateHistoryMaxBytes := int64FromEnv("STATE_HISTORY_MAX_BYTES", int64(160<<20))
	stateHistoryStreamsMax := intFromEnv("STATE_HISTORY_STREAMS_MAX", 1024)
	stateImageCacheMaxBytes := int64FromEnv("STATE_IMAGE_CACHE_MAX_BYTES", int64(64<<20))
	stateImageCacheTTL := durationFromEnv("STATE_IMAGE_CACHE_TTL", 24*time.Hour)

	chatBackend := getEnvOrDefault("LLM_FEATURE_CHAT_BACKEND", LLMBackendOpenAICompat)
	chatModel := os.Getenv("LLM_FEATURE_CHAT_MODEL")
	summarizeBackend := getEnvOrDefault("LLM_FEATURE_SUMMARIZE_BACKEND", chatBackend)
	summarizeModel := getEnvOrDefault("LLM_FEATURE_SUMMARIZE_MODEL", chatModel)
	imageRecognitionBackend := getEnvOrDefault("LLM_FEATURE_IMAGE_RECOGNITION_BACKEND", chatBackend)
	imageRecognitionModel := getEnvOrDefault("LLM_FEATURE_IMAGE_RECOGNITION_MODEL", chatModel)
	toolUseBackend := getEnvOrDefault("LLM_FEATURE_TOOL_USE_BACKEND", chatBackend)
	toolUseModel := getEnvOrDefault("LLM_FEATURE_TOOL_USE_MODEL", chatModel)
	toolLoopMaxIterations := intFromEnv("LLM_TOOL_LOOP_MAX_ITERATIONS", 6)

	return &Config{
		LLM: LLMConfig{
			Backends: BackendConfig{
				OpenAICompat: OpenAICompatBackendConfig{
					BaseURL:  os.Getenv("LLM_BACKEND_OPENAI_COMPAT_BASE_URL"),
					APIToken: os.Getenv("LLM_BACKEND_OPENAI_COMPAT_API_TOKEN"),
				},
				Ollama: OllamaBackendConfig{
					BaseURL: getEnvOrDefault("LLM_BACKEND_OLLAMA_BASE_URL", "http://localhost:11434"),
				},
			},
			Features: FeatureConfig{
				Chat: FeatureRouteConfig{
					Backend: chatBackend,
					Model:   chatModel,
				},
				Summarize: FeatureRouteConfig{
					Backend: summarizeBackend,
					Model:   summarizeModel,
				},
				ImageRecognition: FeatureRouteConfig{
					Backend: imageRecognitionBackend,
					Model:   imageRecognitionModel,
				},
				ToolUse: FeatureRouteConfig{
					Backend: toolUseBackend,
					Model:   toolUseModel,
				},
			},
			ToolLoopMaxIterations: toolLoopMaxIterations,
		},
		Sentry: SentryConfig{
			DSN: os.Getenv("SENTRY_DSN"),
		},
		Bot: BotConfig{
			AdminIDs:                 adminIDsFromEnv("BOT_ADMIN_IDS"),
			Telegram:                 TelegramConfig{Token: os.Getenv("TELEGRAM_TOKEN")},
			UncompressedHistoryLimit: uncompressedHistoryLimit,
			HistorySummaryThreshold:  historySummaryThreshold,
			ProcessingTimeout:        processingTimeout,
		},
		State: StateConfig{
			MaxBytes:           stateMaxBytes,
			HistoryMaxBytes:    stateHistoryMaxBytes,
			HistoryStreamsMax:  stateHistoryStreamsMax,
			ImageCacheMaxBytes: stateImageCacheMaxBytes,
			ImageCacheTTL:      stateImageCacheTTL,
		},
		Logging: LoggingConfig{
			Level: getEnvOrDefault("LOG_LEVEL", "info"),
		},
		Persistence: PersistenceConfig{
			StorePath: getEnvOrDefault("PERSISTENT_STORE_PATH", "/data/db.sqlite"),
		},
	}
}

func (c LLMConfig) UsedBackends() []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, 2)

	for _, route := range []FeatureRouteConfig{c.Features.Chat, c.Features.Summarize, c.Features.ImageRecognition, c.Features.ToolUse} {
		if route.Backend == "" {
			continue
		}
		if _, ok := seen[route.Backend]; ok {
			continue
		}
		seen[route.Backend] = struct{}{}
		result = append(result, route.Backend)
	}

	return result
}

func (c LLMConfig) RouteForFeature(feature string) (FeatureRouteConfig, error) {
	switch feature {
	case "chat":
		return c.Features.Chat, nil
	case "summarize":
		return c.Features.Summarize, nil
	case "image_recognition":
		return c.Features.ImageRecognition, nil
	case "tool_use":
		return c.Features.ToolUse, nil
	default:
		return FeatureRouteConfig{}, fmt.Errorf("unknown feature %q", feature)
	}
}

func adminIDsFromEnv(name string) []int64 {
	raw := os.Getenv(name)
	if raw == "" {
		return nil
	}

	result := make([]int64, 0, 4)
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if id, err := strconv.ParseInt(item, 10, 64); err == nil {
			result = append(result, id)
		}
	}

	return result
}

func intFromEnv(name string, fallback int) int {
	if raw := os.Getenv(name); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			return parsed
		}
	}

	return fallback
}

func int64FromEnv(name string, fallback int64) int64 {
	if raw := os.Getenv(name); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return parsed
		}
	}

	return fallback
}

func durationFromEnv(name string, fallback time.Duration) time.Duration {
	if raw := os.Getenv(name); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil {
			return parsed
		}
	}

	return fallback
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}

	return defaultValue
}
