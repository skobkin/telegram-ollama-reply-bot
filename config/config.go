package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config represents the root configuration structure
type Config struct {
	LLM    LLMConfig
	Sentry SentryConfig
	Bot    BotConfig
}

// LLMConfig contains configuration for the LLM connector
type LLMConfig struct {
	APIBaseURL string
	APIToken   string
	Prompts    PromptConfig
	Models     ModelSelection
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

// BotConfig contains configuration for bot settings
type BotConfig struct {
	HistoryLength            int
	AdminIDs                 []int64
	Telegram                 TelegramConfig
	UncompressedHistoryLimit int
	HistorySummaryThreshold  int
	LlmRequestTimeout        time.Duration
}

// ModelSelection contains configuration for LLM models
type ModelSelection struct {
	TextRequestModel      string
	SummarizeModel        string
	ImageRecognitionModel string
}

// TelegramConfig contains configuration for Telegram bot
type TelegramConfig struct {
	Token string
}

// Load creates a new Config instance populated from environment variables
func Load() *Config {
	historyLength := 150
	if lengthStr := os.Getenv("BOT_HISTORY_LENGTH"); lengthStr != "" {
		if length, err := strconv.Atoi(lengthStr); err == nil {
			historyLength = length
		}
	}

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

	requestTimeout := 60
	if toStr := os.Getenv("LLM_REQUEST_TIMEOUT"); toStr != "" {
		if to, err := strconv.Atoi(toStr); err == nil {
			requestTimeout = to
		}
	}
	requestTimeoutDuration := time.Duration(requestTimeout) * time.Second

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

	return &Config{
		LLM: LLMConfig{
			APIBaseURL: os.Getenv("OPENAI_API_BASE_URL"),
			APIToken:   os.Getenv("OPENAI_API_TOKEN"),
			Models: ModelSelection{
				TextRequestModel:      os.Getenv("MODEL_TEXT_REQUEST"),
				SummarizeModel:        os.Getenv("MODEL_SUMMARIZE_REQUEST"),
				ImageRecognitionModel: os.Getenv("MODEL_IMAGE_RECOGNITION"),
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
		Bot: BotConfig{
			HistoryLength: historyLength,
			AdminIDs:      adminIDs,
			Telegram: TelegramConfig{
				Token: os.Getenv("TELEGRAM_TOKEN"),
			},
			UncompressedHistoryLimit: uncompressedHistoryLimit,
			HistorySummaryThreshold:  historySummaryThreshold,
			LlmRequestTimeout:        requestTimeoutDuration,
		},
	}
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
