package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config represents the root configuration structure
type Config struct {
	LLM     LLMConfig
	Sentry  SentryConfig
	Bot     BotConfig
	State   StateConfig
	Logging LoggingConfig
}

const (
	LLMBackendOpenAICompat = "openai_compat"
	LLMBackendOllama       = "ollama"
)

// LLMConfig contains configuration for the LLM service
type LLMConfig struct {
	Backends BackendConfig
	Features FeatureConfig
	Prompts  PromptConfig
}

// PromptConfig contains configuration for prompts
type PromptConfig struct {
	ChatSystemPrompt       string
	SummarizePrompt        string
	ImageRecognitionPrompt string
	Language               string
	Gender                 string
	MaxSummaryLength       int
}

// SentryConfig contains configuration for Sentry error tracking
type SentryConfig struct {
	DSN string
}

// LoggingConfig contains configuration for structured logging
type LoggingConfig struct {
	Level string
}

// BotConfig contains configuration for bot settings
type BotConfig struct {
	AdminIDs                 []int64
	Telegram                 TelegramConfig
	UncompressedHistoryLimit int
	HistorySummaryThreshold  int
	ProcessingTimeout        time.Duration
}

type StateConfig struct {
	MaxBytes                 int64
	HistoryMaxBytes          int64
	HistoryStreamsMax        int
	HistoryMessagesPerStream int
	ImageCacheMaxBytes       int64
	ImageCacheTTL            time.Duration
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

// TelegramConfig contains configuration for Telegram bot
type TelegramConfig struct {
	Token string
}

// Load creates a new Config instance populated from environment variables
func Load() *Config {
	maxSummaryLength := 2000
	if lengthStr := os.Getenv("MAX_SUMMARY_LENGTH"); lengthStr != "" {
		if length, err := strconv.Atoi(lengthStr); err == nil {
			maxSummaryLength = length
		}
	}

	uncompressedHistoryLimit := 15
	if lengthStr := os.Getenv("LLM_UNCOMPRESSED_HISTORY_LIMIT"); lengthStr != "" {
		if length, err := strconv.Atoi(lengthStr); err == nil {
			uncompressedHistoryLimit = length
		}
	}

	historySummaryThreshold := 5
	if thrStr := os.Getenv("LLM_HISTORY_SUMMARY_THRESHOLD"); thrStr != "" {
		if thr, err := strconv.Atoi(thrStr); err == nil {
			historySummaryThreshold = thr
		}
	}

	processingTimeout := 30 * time.Second
	if toStr := os.Getenv("BOT_PROCESSING_TIMEOUT"); toStr != "" {
		if to, err := time.ParseDuration(toStr); err == nil {
			processingTimeout = to
		}
	}

	stateMaxBytes := int64(256 << 20)
	if value := os.Getenv("STATE_MAX_BYTES"); value != "" {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
			stateMaxBytes = parsed
		}
	}

	stateHistoryMaxBytes := int64(160 << 20)
	if value := os.Getenv("STATE_HISTORY_MAX_BYTES"); value != "" {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
			stateHistoryMaxBytes = parsed
		}
	}

	stateHistoryStreamsMax := 1024
	if value := os.Getenv("STATE_HISTORY_STREAMS_MAX"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			stateHistoryStreamsMax = parsed
		}
	}

	stateHistoryMessagesPerStream := 150
	if value := os.Getenv("STATE_HISTORY_MESSAGES_PER_STREAM"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			stateHistoryMessagesPerStream = parsed
		}
	}

	stateImageCacheMaxBytes := int64(64 << 20)
	if value := os.Getenv("STATE_IMAGE_CACHE_MAX_BYTES"); value != "" {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
			stateImageCacheMaxBytes = parsed
		}
	}

	stateImageCacheTTL := 24 * time.Hour
	if value := os.Getenv("STATE_IMAGE_CACHE_TTL"); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil {
			stateImageCacheTTL = parsed
		}
	}

	// Parse admin IDs from environment variable
	var adminIDs []int64
	if adminIDsStr := os.Getenv("BOT_ADMIN_IDS"); adminIDsStr != "" {
		// Split by comma and parse each ID
		for _, idStr := range strings.Split(adminIDsStr, ",") {
			idStr = strings.TrimSpace(idStr)
			if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
				adminIDs = append(adminIDs, id)
			}
		}
	}

	defaultChatPrompt := "You're a bot in the Telegram chat.\n" +
		"You're using a model called \"{{.Model}}\".\n" +
		"You should reply in the following language: {{.Language}}.\n" +
		"You should use {{.Gender}} gender when speaking about yourself and neutral gender when speaking about others.\n\n" +
		"{{.Context}}"

	defaultSummarizePrompt := "You're a text shortener. Give a VERY SHORT summary as a list of facts. \n" +
		"Format it like this:\n" +
		"```\n" +
		"- Fact 1\n" +
		"- Fact 2\n\n" +
		"Your short conclusion.\n" +
		"```\n" +
		"Avoid any commentaries and value judgement on the matter unless asked by the user. \n" +
		"Avoid using ANY formatting in the text except simple \"-\" for each fact even if asked to.\n\n" +
		"You should reply in the following language: {{.Language}} (unless specifically asked by the user).\n\n" +
		"Limit the summary to maximum of {{.MaxLength}} characters. \n" +
		"Avoid exceeding it at any cost. Be as brief as possible."

	defaultImageRecognitionPrompt := "You're an image recognition bot. Describe what you see in the image in detail for an LLM to understand.\n" +
		"If you can understand the meaning of the image, describe it in detail. If you can't understand the meaning, describe what you see in general.\n" +
		"You should reply in the following language: {{.Language}}.\n" +
		"Be concise but informative."

	chatBackend := getEnvOrDefault("LLM_FEATURE_CHAT_BACKEND", LLMBackendOpenAICompat)
	chatModel := os.Getenv("LLM_FEATURE_CHAT_MODEL")
	summarizeBackend := getEnvOrDefault("LLM_FEATURE_SUMMARIZE_BACKEND", chatBackend)
	summarizeModel := getEnvOrDefault("LLM_FEATURE_SUMMARIZE_MODEL", chatModel)
	imageRecognitionBackend := getEnvOrDefault("LLM_FEATURE_IMAGE_RECOGNITION_BACKEND", chatBackend)
	imageRecognitionModel := getEnvOrDefault("LLM_FEATURE_IMAGE_RECOGNITION_MODEL", chatModel)
	toolUseBackend := getEnvOrDefault("LLM_FEATURE_TOOL_USE_BACKEND", chatBackend)
	toolUseModel := getEnvOrDefault("LLM_FEATURE_TOOL_USE_MODEL", chatModel)

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
			Prompts: PromptConfig{
				ChatSystemPrompt:       getEnvOrDefault("PROMPT_CHAT", defaultChatPrompt),
				SummarizePrompt:        getEnvOrDefault("PROMPT_SUMMARIZE", defaultSummarizePrompt),
				ImageRecognitionPrompt: getEnvOrDefault("PROMPT_IMAGE_RECOGNITION", defaultImageRecognitionPrompt),
				Language:               getEnvOrDefault("RESPONSE_LANGUAGE", "Russian"),
				Gender:                 getEnvOrDefault("RESPONSE_GENDER", "neutral"),
				MaxSummaryLength:       maxSummaryLength,
			},
		},
		Sentry: SentryConfig{
			DSN: os.Getenv("SENTRY_DSN"),
		},
		State: StateConfig{
			MaxBytes:                 stateMaxBytes,
			HistoryMaxBytes:          stateHistoryMaxBytes,
			HistoryStreamsMax:        stateHistoryStreamsMax,
			HistoryMessagesPerStream: stateHistoryMessagesPerStream,
			ImageCacheMaxBytes:       stateImageCacheMaxBytes,
			ImageCacheTTL:            stateImageCacheTTL,
		},
		Logging: LoggingConfig{
			Level: getEnvOrDefault("LOG_LEVEL", "info"),
		},
		Bot: BotConfig{
			AdminIDs: adminIDs,
			Telegram: TelegramConfig{
				Token: os.Getenv("TELEGRAM_TOKEN"),
			},
			UncompressedHistoryLimit: uncompressedHistoryLimit,
			HistorySummaryThreshold:  historySummaryThreshold,
			ProcessingTimeout:        processingTimeout,
		},
	}
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}

	return defaultValue
}

func (c LLMConfig) UsedBackends() []string {
	used := []string{}

	for _, route := range []FeatureRouteConfig{
		c.Features.Chat,
		c.Features.Summarize,
		c.Features.ImageRecognition,
		c.Features.ToolUse,
	} {
		if route.Backend == "" {
			continue
		}

		duplicate := false
		for _, backend := range used {
			if backend == route.Backend {
				duplicate = true

				break
			}
		}

		if !duplicate {
			used = append(used, route.Backend)
		}
	}

	return used
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
		return FeatureRouteConfig{}, fmt.Errorf("unknown feature: %s", feature)
	}
}
