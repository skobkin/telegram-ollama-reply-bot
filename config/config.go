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

	return &Config{
		LLM: LLMConfig{
			APIBaseURL: os.Getenv("OPENAI_API_BASE_URL"),
			APIToken:   os.Getenv("OPENAI_API_TOKEN"),
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
