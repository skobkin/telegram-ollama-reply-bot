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
	Providers   ProvidersConfig
	Search      SearchConfig
}

const (
	LLMBackendOpenAICompat = "openai_compat"

	SearchBackendNone   = "none"
	SearchBackendTavily = "tavily"
	SearchBackendKagi   = "kagi"
	SearchBackendChain  = "chain"
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

// ProvidersConfig contains credentials for external providers that may support multiple capabilities.
type ProvidersConfig struct {
	Tavily TavilyProviderConfig
	Kagi   KagiProviderConfig
}

// TavilyProviderConfig contains Tavily credentials.
type TavilyProviderConfig struct {
	APIKey string
}

// KagiProviderConfig contains Kagi credentials.
type KagiProviderConfig struct {
	APIKey string
}

// SearchConfig contains search capability routing.
type SearchConfig struct {
	Backend string
	Chain   []string
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
}

// OpenAICompatBackendConfig contains configuration for OpenAI-compatible backends.
type OpenAICompatBackendConfig struct {
	BaseURL  string
	APIToken string
}

// FeatureConfig contains per-feature model selection.
type FeatureConfig struct {
	Chat             FeatureRouteConfig
	Summarize        FeatureRouteConfig
	ImageRecognition FeatureRouteConfig
}

// FeatureRouteConfig contains model selection for one feature.
type FeatureRouteConfig struct {
	Model string
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

	chatModel := os.Getenv("LLM_FEATURE_CHAT_MODEL")
	summarizeModel := getEnvOrDefault("LLM_FEATURE_SUMMARIZE_MODEL", chatModel)
	imageRecognitionModel := getEnvOrDefault("LLM_FEATURE_IMAGE_RECOGNITION_MODEL", chatModel)
	toolLoopMaxIterations := intFromEnv("LLM_TOOL_LOOP_MAX_ITERATIONS", 6)
	searchBackend := strings.TrimSpace(os.Getenv("SEARCH_BACKEND"))
	if searchBackend == "" {
		searchBackend = SearchBackendNone
	}

	return &Config{
		LLM: LLMConfig{
			Backends: BackendConfig{
				OpenAICompat: OpenAICompatBackendConfig{
					BaseURL:  os.Getenv("LLM_BACKEND_OPENAI_COMPAT_BASE_URL"),
					APIToken: os.Getenv("LLM_BACKEND_OPENAI_COMPAT_API_TOKEN"),
				},
			},
			Features: FeatureConfig{
				Chat: FeatureRouteConfig{
					Model: chatModel,
				},
				Summarize: FeatureRouteConfig{
					Model: summarizeModel,
				},
				ImageRecognition: FeatureRouteConfig{
					Model: imageRecognitionModel,
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
		Providers: ProvidersConfig{
			Tavily: TavilyProviderConfig{
				APIKey: os.Getenv("PROVIDER_TAVILY_API_KEY"),
			},
			Kagi: KagiProviderConfig{
				APIKey: os.Getenv("PROVIDER_KAGI_API_KEY"),
			},
		},
		Search: SearchConfig{
			Backend: searchBackend,
			Chain:   csvFromEnv("SEARCH_BACKEND_CHAIN"),
		},
	}
}

func (c LLMConfig) RouteForFeature(feature string) (FeatureRouteConfig, error) {
	switch feature {
	case "chat":
		return c.Features.Chat, nil
	case "summarize":
		return c.Features.Summarize, nil
	case "image_recognition":
		return c.Features.ImageRecognition, nil
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

func csvFromEnv(name string) []string {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil
	}

	items := strings.Split(raw, ",")
	result := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}

		result = append(result, item)
	}

	return result
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}

	return defaultValue
}
