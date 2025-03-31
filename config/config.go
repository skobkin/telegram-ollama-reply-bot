package config

import (
	"os"
	"strconv"
)

// Config represents the root configuration structure
type Config struct {
	LLM      LLMConfig
	Sentry   SentryConfig
	Telegram TelegramConfig
	Bot      BotConfig
}

// LLMConfig contains configuration for the LLM connector
type LLMConfig struct {
	APIBaseURL string
	APIToken   string
	Prompts    PromptConfig
}

// PromptConfig contains configuration for prompts
type PromptConfig struct {
	ChatSystemPrompt string
	SummarizePrompt  string
	Language         string
	Gender           string
	MaxSummaryLength int
}

// SentryConfig contains configuration for Sentry error tracking
type SentryConfig struct {
	DSN string
}

// TelegramConfig contains configuration for Telegram bot
type TelegramConfig struct {
	Token string
}

// BotConfig contains configuration for bot settings
type BotConfig struct {
	HistoryLength int
	Models        ModelSelection
}

// ModelSelection contains configuration for LLM models
type ModelSelection struct {
	TextRequestModel string
	SummarizeModel   string
}

// Load creates a new Config instance populated from environment variables
func Load() *Config {
	historyLength := 150 // default value
	if lengthStr := os.Getenv("BOT_HISTORY_LENGTH"); lengthStr != "" {
		if length, err := strconv.Atoi(lengthStr); err == nil {
			historyLength = length
		}
	}

	maxSummaryLength := 2000 // default value
	if lengthStr := os.Getenv("MAX_SUMMARY_LENGTH"); lengthStr != "" {
		if length, err := strconv.Atoi(lengthStr); err == nil {
			maxSummaryLength = length
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

	return &Config{
		LLM: LLMConfig{
			APIBaseURL: os.Getenv("OPENAI_API_BASE_URL"),
			APIToken:   os.Getenv("OPENAI_API_TOKEN"),
			Prompts: PromptConfig{
				ChatSystemPrompt: getEnvOrDefault("PROMPT_CHAT", defaultChatPrompt),
				SummarizePrompt:  getEnvOrDefault("PROMPT_SUMMARIZE", defaultSummarizePrompt),
				Language:         getEnvOrDefault("RESPONSE_LANGUAGE", "Russian"),
				Gender:           getEnvOrDefault("RESPONSE_GENDER", "neutral"),
				MaxSummaryLength: maxSummaryLength,
			},
		},
		Sentry: SentryConfig{
			DSN: os.Getenv("SENTRY_DSN"),
		},
		Telegram: TelegramConfig{
			Token: os.Getenv("TELEGRAM_TOKEN"),
		},
		Bot: BotConfig{
			HistoryLength: historyLength,
			Models: ModelSelection{
				TextRequestModel: os.Getenv("MODEL_TEXT_REQUEST"),
				SummarizeModel:   os.Getenv("MODEL_SUMMARIZE_REQUEST"),
			},
		},
	}
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
