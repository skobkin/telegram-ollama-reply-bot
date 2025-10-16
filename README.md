# Telegram Ollama Bot

[![Build Status](https://ci.skobk.in/api/badges/skobkin/telegram-ollama-reply-bot/status.svg)](https://ci.skobk.in/skobkin/telegram-ollama-reply-bot)

![Project Banner](/img/banner.jpeg)

## Functionality

- Context-dependent dialogue in chats
- Summarization of articles by provided link
- Image recognition and description

## Configuration

The bot can be configured using the following environment variables:

| Variable                  | Description                                        | Required | Default |
|---------------------------|----------------------------------------------------|----------|--------|
| `OPENAI_API_TOKEN`        | API token for OpenAI compatible API                | Yes      | -      |
| `OPENAI_API_BASE_URL`     | Base URL for OpenAI compatible API                 | Yes      | -      |
| `TELEGRAM_TOKEN`          | Telegram Bot API token                             | Yes      | -      |
| `MODEL_TEXT_REQUEST`      | Model name for text requests                       | Yes      | -      |
| `MODEL_SUMMARIZE_REQUEST` | Model name for summarization requests              | Yes      | -      |
| `MODEL_IMAGE_RECOGNITION` | Model name for image recognition                   | No       | -      |
| `BOT_HISTORY_LENGTH`      | Number of messages to keep in conversation history | No       | 150    |
| `LLM_UNCOMPRESSED_HISTORY_LIMIT` | Recent chat messages sent verbatim to LLM; older ones summarized. Set to `0` to disable summarization | No | 15 |
| `LLM_HISTORY_SUMMARY_THRESHOLD` | Extra messages beyond the limit before summarization triggers again | No | 5 |
| `BOT_PROCESSING_TIMEOUT` | Timeout for processing incoming requests (includes LLM calls). Accepts Go duration strings (e.g. `45s`, `1m30s`). | No | `30s` |
| `SENTRY_DSN`              | Sentry DSN for error tracking                      | No       | empty  |
| `RESPONSE_LANGUAGE`       | Language for bot responses                          | No       | Russian |
| `RESPONSE_GENDER`         | Gender for bot responses                            | No       | neutral |
| `MAX_SUMMARY_LENGTH`      | Maximum length of generated summaries              | No       | 2000   |
| `PROMPT_CHAT`             | System prompt for chat interactions                 | No       | See [config.go](config/config.go) |
| `PROMPT_SUMMARIZE`        | System prompt for summarization                    | No       | See [config.go](config/config.go) |
| `PROMPT_IMAGE_RECOGNITION`| System prompt for image recognition                | No       | See [config.go](config/config.go) |
| `BOT_ADMIN_IDS`           | Comma-separated list of admin user IDs             | No       | empty  |

### Prompt placeholders

Prompt environment variables support Go's [`text/template`](https://pkg.go.dev/text/template) placeholders. The following
placeholders are available:

- **`PROMPT_CHAT`** – `{{.Model}}`, `{{.Language}}`, `{{.Gender}}`, `{{.Context}}`
- **`PROMPT_SUMMARIZE`** – `{{.Language}}`, `{{.MaxLength}}`
- **`PROMPT_IMAGE_RECOGNITION`** – `{{.Language}}`

`{{.Model}}` is the model name, `{{.Language}}` is the response language, `{{.Gender}}` defines how the bot speaks about
itself, `{{.Context}}` is the recent conversation history, and `{{.MaxLength}}` limits summary size.

## Usage

The bot supports the following commands:

| Command     | Description                                    | Example |
|-------------|------------------------------------------------|---------|
| `/start`    | Start the bot and get a welcome message        | `/start` |
| `/help`     | Show help message with available commands      | `/help` |
| `/summarize`, `/s` | Summarize text from the provided link   | `/summarize https://ex.co/article`, `/s https://ex.co/article concentrate on tech stuff` |
| `/stats`    | Show bot statistics (admin only)               | `/stats` |
| `/reset`    | Reset current chat history (admin only)        | `/reset` |

You can also interact with the bot by:
- Mentioning it in a message
- Replying to its messages
- Sending direct messages in private chat (if enabled)
- Sending images (the bot will describe what it sees in the image)

## Running

### Docker

```shell
docker run \
  -e OPENAI_API_TOKEN=123 \
  -e OPENAI_API_BASE_URL=http://ollama.localhost:11434/v1 \
  -e TELEGRAM_TOKEN=12345 \
  -e MODEL_TEXT_REQUEST=gemma3:27b \
  -e MODEL_SUMMARIZE_REQUEST=gemma3:12b \
  -e MODEL_IMAGE_RECOGNITION=gemma3:12b \
  -e BOT_HISTORY_LENGTH=150 \
  -e LLM_UNCOMPRESSED_HISTORY_LIMIT=15 \
  -e SENTRY_DSN=https://your-sentry-dsn \
  -e BOT_ADMIN_IDS=123456789,987654321 \
  skobkin/telegram-llm-bot
```
