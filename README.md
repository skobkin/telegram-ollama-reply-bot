# Telegram Ollama Bot

[![Build Status](https://ci.skobk.in/api/badges/skobkin/telegram-ollama-reply-bot/status.svg)](https://ci.skobk.in/skobkin/telegram-ollama-reply-bot)

![Project Banner](/img/banner.jpeg)

## Functionality

- Context-dependent dialogue in chats
- Summarization of articles by provided link

## Configuration

The bot can be configured using the following environment variables:

| Variable                  | Description                                        | Required | Default |
|---------------------------|----------------------------------------------------|----------|--------|
| `OPENAI_API_TOKEN`        | API token for OpenAI compatible API                | Yes      | -      |
| `OPENAI_API_BASE_URL`     | Base URL for OpenAI compatible API                 | Yes      | -      |
| `TELEGRAM_TOKEN`          | Telegram Bot API token                             | Yes      | -      |
| `MODEL_TEXT_REQUEST`      | Model name for text requests                       | Yes      | -      |
| `MODEL_SUMMARIZE_REQUEST` | Model name for summarization requests              | Yes      | -      |
| `BOT_HISTORY_LENGTH`      | Number of messages to keep in conversation history | No       | 150    |
| `SENTRY_DSN`              | Sentry DSN for error tracking                      | No       | empty  |
| `RESPONSE_LANGUAGE`       | Language for bot responses                          | No       | Russian |
| `RESPONSE_GENDER`         | Gender for bot responses                            | No       | neutral |
| `MAX_SUMMARY_LENGTH`      | Maximum length of generated summaries              | No       | 2000   |
| `PROMPT_CHAT`             | System prompt for chat interactions                 | No       | See [config.go](config/config.go) |
| `PROMPT_SUMMARIZE`        | System prompt for summarization                    | No       | See [config.go](config/config.go) |

## Usage

### Docker

```shell
docker run \
  -e OPENAI_API_TOKEN=123 \
  -e OPENAI_API_BASE_URL=http://ollama.localhost:11434/v1 \
  -e TELEGRAM_TOKEN=12345 \
  -e MODEL_TEXT_REQUEST=gemma3:27b \
  -e MODEL_SUMMARIZE_REQUEST=gemma3:12b \
  -e BOT_HISTORY_LENGTH=150 \
  -e SENTRY_DSN=https://your-sentry-dsn \
  skobkin/telegram-llm-bot
```
