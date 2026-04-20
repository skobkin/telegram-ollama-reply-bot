# Telegram Ollama Bot

[![Build Status](https://ci.skobk.in/api/badges/skobkin/telegram-ollama-reply-bot/status.svg)](https://ci.skobk.in/skobkin/telegram-ollama-reply-bot)

![Project Banner](/img/banner.jpeg)

## Functionality

- Context-dependent dialogue in chats
- Summarization of articles by provided link
- Image recognition and description
- Tool-assisted free-form chat with URL retrieval, history lookup, summary lookup, current-time lookup, poll creation, and durable reminders

## Configuration

The bot can be configured using the following environment variables:

| Variable                                | Description                                                                                                       | Required | Default                  |
|-----------------------------------------|-------------------------------------------------------------------------------------------------------------------|----------|--------------------------|
| `TELEGRAM_TOKEN`                        | Telegram Bot API token                                                                                            | Yes      | -                        |
| `LLM_BACKEND_OPENAI_COMPAT_BASE_URL`    | Base URL for OpenAI-compatible backend                                                                            | No       | empty                    |
| `LLM_BACKEND_OPENAI_COMPAT_API_TOKEN`   | API token for OpenAI-compatible backend                                                                           | No       | empty                    |
| `LLM_BACKEND_OLLAMA_BASE_URL`           | Base URL for Ollama native API                                                                                    | No       | `http://localhost:11434` |
| `LLM_FEATURE_CHAT_BACKEND`              | Backend for normal chat requests: `openai_compat` or `ollama`                                                     | No       | `openai_compat`          |
| `LLM_FEATURE_CHAT_MODEL`                | Model name for normal chat requests                                                                               | Yes      | -                        |
| `LLM_FEATURE_SUMMARIZE_BACKEND`         | Backend for summarization                                                                                         | No       | chat backend             |
| `LLM_FEATURE_SUMMARIZE_MODEL`           | Model name for summarization                                                                                      | No       | chat model               |
| `LLM_FEATURE_IMAGE_RECOGNITION_BACKEND` | Backend for image recognition                                                                                     | No       | chat backend             |
| `LLM_FEATURE_IMAGE_RECOGNITION_MODEL`   | Model name for image recognition                                                                                  | No       | chat model               |
| `LLM_FEATURE_TOOL_USE_BACKEND`          | Backend used for conversational tool calling in ordinary chat replies                                             | No       | chat backend             |
| `LLM_FEATURE_TOOL_USE_MODEL`            | Model used for conversational tool calling in ordinary chat replies                                               | No       | chat model               |
| `LLM_TOOL_LOOP_MAX_ITERATIONS`          | Maximum tool-use loop iterations for one conversational reply                                                     | No       | `6`                      |
| `STATE_MAX_BYTES`                       | Soft total in-memory state budget in bytes                                                                        | No       | `268435456`              |
| `STATE_HISTORY_MAX_BYTES`               | Soft history bucket budget in bytes                                                                               | No       | `167772160`              |
| `STATE_HISTORY_STREAMS_MAX`             | Maximum number of active conversation scopes kept in RAM                                                          | No       | `1024`                   |
| `STATE_HISTORY_MESSAGES_PER_STREAM`     | Number of messages to keep per chat/topic history stream                                                          | No       | `150`                    |
| `STATE_IMAGE_CACHE_MAX_BYTES`           | Maximum image-description cache size in bytes                                                                     | No       | `67108864`               |
| `STATE_IMAGE_CACHE_TTL`                 | TTL for cached image descriptions. Accepts Go duration strings (e.g. `1h`, `24h`).                                | No       | `24h`                    |
| `LLM_UNCOMPRESSED_HISTORY_LIMIT`        | Recent chat messages sent verbatim to LLM; older ones summarized. Set to `0` to disable summarization             | No       | 15                       |
| `LLM_HISTORY_SUMMARY_THRESHOLD`         | Extra messages beyond the limit before summarization triggers again                                               | No       | 5                        |
| `BOT_PROCESSING_TIMEOUT`                | Timeout for processing incoming requests (includes LLM calls). Accepts Go duration strings (e.g. `45s`, `1m30s`). | No       | `30s`                    |
| `LOG_LEVEL`                             | Structured log verbosity: `debug`, `info`, `warn`, or `error`                                                     | No       | `info`                   |
| `SENTRY_DSN`                            | Sentry DSN for error tracking                                                                                     | No       | empty                    |
| `PERSISTENT_STORE_PATH`                 | Path to the SQLite database used for durable bot data                                                             | No       | `/data/db.sqlite`        |
| `BOT_ADMIN_IDS`                         | Comma-separated list of admin user IDs                                                                            | No       | empty                    |

### Prompt and persona management

Prompt templates, response language, character name, tone mode, interactivity mode, whitelist rules, and per-chat
overrides are now stored in SQLite and managed from Telegram admin DMs.

Stored prompt templates use Go's [`text/template`](https://pkg.go.dev/text/template) placeholders:

- `chat` – `{{.Model}}`, `{{.Language}}`, `{{.Gender}}`, `{{.CharacterName}}`, `{{.ToneMode}}`, `{{.AllowTeasing}}`, `{{.Context}}`
- `summarize` – `{{.Language}}`, `{{.MaxLength}}`
- `image_recognition` – `{{.Language}}`
- `tool_use` – `{{.Model}}`, `{{.Language}}`, `{{.Gender}}`, `{{.CharacterName}}`, `{{.ToneMode}}`, `{{.AllowTeasing}}`, `{{.Context}}`, `{{.ToolPolicy}}`

## Usage

The bot supports the following commands:

| Command            | Description                               | Example                                                                                  |
|--------------------|-------------------------------------------|------------------------------------------------------------------------------------------|
| `/start`           | Start the bot and get a welcome message   | `/start`                                                                                 |
| `/help`            | Show help message with available commands | `/help`                                                                                  |
| `/summarize`, `/s` | Summarize text from the provided link     | `/summarize https://ex.co/article`, `/s https://ex.co/article concentrate on tech stuff` |
| `/stats`           | Show bot statistics (admin only)          | `/stats`                                                                                 |
| `/reset`           | Reset current chat history (admin only)   | `/reset`                                                                                 |
| `/admin_help`      | Show DM admin commands                    | `/admin_help`                                                                            |

You can also interact with the bot by:
- Mentioning it in a message
- Replying to its messages
- Sending direct messages in private chat when interactivity is enabled for that chat
- Sending images (the bot will describe what it sees in the image)

When `LLM_FEATURE_TOOL_USE_*` is configured, ordinary chat replies may use tools before answering. The current tool set is:

- `get_current_time` for time-sensitive reasoning and schedule anchoring
- `datetime_math` for exact datetime diff, shift, weekday, and timezone conversion
- `datetime_format` for compact user-facing timestamp formatting
- `fetch_url_content` for explicit link-analysis requests in free-form chat
- `search_recent_history` for in-memory recent-message lookup in the current chat/topic
- `get_conversation_summary` for the current in-memory earlier summary
- `create_poll` for explicit vote/poll requests
- `list_chat_schedule`, `add_schedule_item`, `remove_schedule_item` for durable chat reminders

Reminder behavior in the first implementation slice:

- one-shot reminders use an exact RFC3339 timestamp internally
- recurring reminders support `interval_days`, `interval_weeks`, weekday rules, and monthly rules
- calendar-based reminders require an IANA timezone such as `Europe/Moscow`
- reminders survive restarts and are delivered back into the original chat topic when `topic_id` is present
- after downtime, recurring reminders emit only the latest missed occurrence and then continue on schedule

`/summarize` still uses the direct extractor-plus-summary workflow and does not depend on the tool loop.

By default, chat interactivity is `disabled`. Configure it from an admin DM before expecting the bot to answer normal
chat messages.

### Admin DM commands

These commands work only in private chat with the bot and only for users listed in `BOT_ADMIN_IDS`. If `BOT_ADMIN_IDS`
is empty, admin controls are disabled.

- `/admin_help`
- `/chat_list`
- `/config_fields_global`
- `/config_fields_chat`
- `/prompt_features`
- `/config_show [chat_id]`
- `/config_set_global <field> <value>`
- `/config_set_chat <chat_id> <field> <value>`
- `/config_clear_chat <chat_id> <field|all>`
- `/prompt_show <feature> [chat_id]`
- `/prompt_set_global <feature> <template>`
- `/prompt_set_chat <chat_id> <feature> <template>`
- `/prompt_clear_chat <chat_id> <feature>`
- `/whitelist_add <chat_id>`
- `/whitelist_remove <chat_id>`
- `/whitelist_list`

## Running

### Local build

```shell
go build -o /tmp/telegram-ollama-reply-bot ./cmd/bot
```

### Lint

```shell
golangci-lint run ./cmd/bot/... ./internal/...
```

### Docker

```shell
docker run \
  -e TELEGRAM_TOKEN=12345 \
  -e LLM_BACKEND_OLLAMA_BASE_URL=http://ollama.localhost:11434 \
  -e LLM_FEATURE_CHAT_BACKEND=ollama \
  -e LLM_FEATURE_CHAT_MODEL=gemma3:27b \
  -e LLM_FEATURE_SUMMARIZE_MODEL=gemma3:12b \
  -e LLM_FEATURE_IMAGE_RECOGNITION_MODEL=gemma3:12b \
  -e PERSISTENT_STORE_PATH=/data/db.sqlite \
  -e STATE_HISTORY_MESSAGES_PER_STREAM=150 \
  -e STATE_HISTORY_STREAMS_MAX=1024 \
  -e STATE_IMAGE_CACHE_TTL=24h \
  -e LLM_UNCOMPRESSED_HISTORY_LIMIT=15 \
  -e LOG_LEVEL=info \
  -e SENTRY_DSN=https://your-sentry-dsn \
  -e BOT_ADMIN_IDS=123456789,987654321 \
  -v bot-data:/data \
  skobkin/telegram-llm-bot
```

To keep the current OpenAI-compatible path, point `LLM_BACKEND_OPENAI_COMPAT_BASE_URL` and `LLM_BACKEND_OPENAI_COMPAT_API_TOKEN` at that backend and leave feature backends on the default `openai_compat`.

### Docker Compose

An example Compose-based deployment is maintained in the existing stack repository:

https://git.skobk.in/skobkin/docker-stacks/src/branch/master/telegram-llm-bot
