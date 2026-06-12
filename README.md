# Telegram LLM Bot

[![Build Status](https://ci.skobk.in/api/badges/skobkin/telegram-ollama-reply-bot/status.svg)](https://ci.skobk.in/skobkin/telegram-ollama-reply-bot)

![Project Banner](/img/banner.webp)

## Functionality

- Context-dependent dialogue in chats
- Summarization of articles by provided link
- Image recognition and description
- Tool-assisted free-form chat with
  - URL retrieval
  - Optional external web search
  - History lookup
  - Summary lookup
  - Current-time lookup
  - Poll creation
  - Durable reminders
- Optional [Model Context Protocol](https://modelcontextprotocol.io) (MCP) servers that publish additional tools at startup (Streamable HTTP transport)

## Configuration

The bot can be configured using the following environment variables:

| Variable                                            | Description                                                                                                       | Required | Default                  |
|-----------------------------------------------------|-------------------------------------------------------------------------------------------------------------------|----------|--------------------------|
| `BOT__TELEGRAM__TOKEN`                              | Telegram Bot API token                                                                                            | Yes      | -                        |
| `LLM__BACKENDS__OPENAI_COMPAT__BASE_URL`            | Base URL for OpenAI-compatible backend                                                                            | No       | empty                    |
| `LLM__BACKENDS__OPENAI_COMPAT__API_TOKEN`           | API token for OpenAI-compatible backend                                                                           | No       | empty                    |
| `LLM__FEATURES__CHAT__MODEL`                        | Model name for normal chat requests                                                                               | Yes      | -                        |
| `LLM__FEATURES__SUMMARIZE__MODEL`                   | Model name for summarization                                                                                      | No       | chat model               |
| `LLM__FEATURES__IMAGE_RECOGNITION__MODEL`           | Model name for image recognition                                                                                  | No       | chat model               |
| `LLM__TOOL_LOOP_MAX_ITERATIONS`                     | Maximum tool-use loop iterations for one conversational reply                                                     | No       | `6`                      |
| `LLM__IMAGE_RECOGNITION_ENABLED`                    | Toggle image recognition. Set to `false` to treat images as placeholders without making vision model calls.       | No       | `true`                   |
| `STATE__MAX_BYTES`                                  | Soft total in-memory state budget in bytes                                                                        | No       | `268435456`              |
| `STATE__HISTORY_MAX_BYTES`                          | Soft history bucket budget in bytes                                                                               | No       | `167772160`              |
| `STATE__HISTORY_STREAMS_MAX`                        | Maximum number of active conversation scopes kept in RAM                                                          | No       | `1024`                   |
| `STATE__IMAGE_CACHE_MAX_BYTES`                      | Maximum image-description cache size in bytes                                                                     | No       | `67108864`               |
| `STATE__IMAGE_CACHE_TTL`                            | TTL for cached image descriptions. Accepts Go duration strings (e.g. `1h`, `24h`).                                | No       | `24h`                    |
| `LLM__UNCOMPRESSED_HISTORY_LIMIT`                   | Recent chat messages sent verbatim to LLM; older ones summarized. Set to `0` to disable summarization             | No       | 15                       |
| `LLM__HISTORY_SUMMARY_THRESHOLD`                    | Extra messages beyond the limit before summarization triggers again                                               | No       | 5                        |
| `BOT__PROCESSING_TIMEOUT`                           | Timeout for processing incoming requests (includes LLM calls). Accepts Go duration strings (e.g. `45s`, `1m30s`). | No       | `30s`                    |
| `SEARCH__BACKEND`                                   | External search backend for the `search_web` tool: `none`, `tavily`, `kagi`, or `chain`                           | No       | `none`                   |
| `SEARCH__CHAIN`                                     | Comma-separated backend order used when `SEARCH__BACKEND=chain`                                                   | No       | empty                    |
| `PROVIDERS__TAVILY__API_KEY`                        | Tavily provider API key                                                                                           | No       | empty                    |
| `PROVIDERS__KAGI__API_KEY`                          | Kagi provider API key                                                                                             | No       | empty                    |
| `LOG__LEVEL`                                        | Structured log verbosity: `debug`, `info`, `warn`, or `error`                                                     | No       | `info`                   |
| `SENTRY__DSN`                                       | Sentry DSN for error tracking                                                                                     | No       | empty                    |
| `PERSISTENT__STORE_PATH`                            | Path to the SQLite database used for durable bot data                                                             | No       | `/data/db.sqlite`        |
| `BOT__ADMIN_IDS`                                    | Comma-separated list of admin user IDs                                                                            | No       | empty                    |
| `MCP__SERVERS__<NAME>__URL`                         | Streamable HTTP endpoint of an MCP server (e.g. `https://mcp.example.com/mcp`)                                    | No       | empty                    |
| `MCP__SERVERS__<NAME>__HEADERS__<KEY>`             | HTTP header sent to that MCP server (e.g. `Authorization=Bearer …`). Values are never logged.                     | No       | empty                    |
| `MCP__SERVERS__<NAME>__INSECURE`                   | Allow plain `http://` for this server. Required for local development.                                            | No       | `false`                  |
| `MCP__SERVERS__<NAME>__INVOCATION_POLICY`          | Default invocation policy for tools from this server: `discretionary` or `explicit_request_only`                 | No       | derived from `readOnlyHint` |
| `MCP__SERVERS__<NAME>__SIDE_EFFECTING`             | Force `true`/`false` for the side-effecting flag of every tool from this server                                   | No       | derived from `destructiveHint` |
| `MCP__SERVERS__<NAME>__ALLOWED_TOOLS`              | Comma-separated whitelist of tool names to expose from this server. Misnamed entries fail at startup.            | No       | empty                    |
| `MCP__SERVERS__<NAME>__RESTRICTED_TOOLS`           | Comma-separated deny-list of tool names to drop from this server                                                  | No       | empty                    |
| `MCP__SERVERS__<NAME>__OPTIONAL`                   | When `true`, a connection failure on startup is logged and skipped instead of aborting the bot                    | No       | `false`                  |

### Prompt and persona management

Prompt templates, response language, character name, tone mode, interactivity mode, whitelist rules, and per-chat
overrides are now stored in SQLite and managed from Telegram admin DMs.

Stored prompt templates use Go's [`text/template`](https://pkg.go.dev/text/template) placeholders:

- `chat` – `{{.Model}}`, `{{.Language}}`, `{{.Gender}}`, `{{.CharacterName}}`, `{{.ToneMode}}`, `{{.AllowTeasing}}`, `{{.Context}}`, `{{.ToolPolicy}}`
- `summarize` – `{{.Language}}`, `{{.MaxLength}}`
- `image_recognition` – `{{.Language}}`

Global fields: `character_name`, `language`, `gender`, `tone_mode`, `allow_teasing`, `default_interactivity_mode`.
Per-chat fields: `character_name`, `language`, `gender`, `tone_mode`, `allow_teasing`, `interactivity_mode`.

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

Ordinary chat replies always use the tool-capable chat workflow. `LLM__FEATURES__CHAT__MODEL` must point to a model and backend combination that supports OpenAI-compatible tool calls. The current tool set is:

- `search_web` for explicit internet lookups when `SEARCH__BACKEND` is configured to something other than `none`
- `get_current_time` for time-sensitive reasoning and schedule anchoring
- `datetime_math` for exact datetime diff, shift, weekday, and timezone conversion
- `datetime_format` for compact user-facing timestamp formatting
- `fetch_url_content` for explicit link-analysis requests in free-form chat
- `search_history` for indexed keyword and fuzzy full in-memory history lookup in the current chat/topic
- `get_conversation_summary` for the current in-memory earlier summary
- `get_chat_activity_window` for in-memory message cadence heuristics in the current chat/topic
- `get_history_bounds` for exact full-history and recent-history coverage in the current chat/topic
- `get_message_thread_context` for compact reply-chain reconstruction when an older replied-to message has scrolled out of the recent verbatim context
- `send_poll` for explicit vote/poll requests
- `send_quiz` for explicit quiz and trivia requests
- `send_dice` for explicit dice-roll and mini-game requests
- `list_chat_schedule`, `add_schedule_item`, `remove_schedule_item` for durable chat reminders

### External MCP tools

The bot can also expose tools published by external [Model Context Protocol](https://modelcontextprotocol.io) (MCP)
servers over Streamable HTTP. Each configured server is connected at startup, its `tools/list` is fetched, and
each remote tool is registered under the namespaced name `mcp_<server>__<tool>` (lowercased and `[a-z0-9_]`-normalized).
The LLM sees MCP tools alongside the built-ins; tool calls are routed back to the originating server.

Header values are never written to logs.

A worked example: a hosted browser/search MCP server with bearer-token auth, only exposing two specific tools:

```env
MCP__SERVERS__BROWSER__URL=https://mcp.example.com/mcp
MCP__SERVERS__BROWSER__HEADERS__AUTHORIZATION=Bearer secret-token
MCP__SERVERS__BROWSER__ALLOWED_TOOLS=fetch_url,summarize_page
```

A local development server (plain HTTP, fail-soft):

```env
MCP__SERVERS__LOCAL__URL=http://localhost:8765/mcp
MCP__SERVERS__LOCAL__INSECURE=true
MCP__SERVERS__LOCAL__OPTIONAL=true
```

Startup behaviour:

- If a server's `URL` is unreachable, the bot fails to start unless `OPTIONAL=true` is set for that server.
- An entry in `ALLOWED_TOOLS` that the server does not advertise is a hard startup error (catches typos early).
- Tool invocation policy defaults to `discretionary` when the server's tool annotation sets `readOnlyHint=true`,
  and `explicit_request_only` otherwise. The default side-effecting flag is `destructiveHint` (treated as `true`
  when the annotation is absent), and is forced to `false` whenever `readOnlyHint=true` per the MCP spec.
- `INSECURE=true` is required to use a `http://` URL. HTTPS is the default and is enforced per server.

Reminder behavior in the first implementation slice:

- one-shot reminders use an exact RFC3339 timestamp internally
- recurring reminders support `interval_days`, `interval_weeks`, weekday rules, and monthly rules
- calendar-based reminders require an IANA timezone such as `Europe/Moscow`
- reminders survive restarts and are delivered back into the original chat topic when `topic_id` is present
- after downtime, recurring reminders emit only the latest missed occurrence and then continue on schedule

`/summarize` still uses the direct extractor-plus-summary workflow and does not depend on the tool loop.

If `SEARCH__BACKEND` is unset or set to `none`, the `search_web` tool is not exposed to the model.

By default, chat interactivity is `disabled`. Configure it from an admin DM before expecting the bot to answer normal
chat messages.

### Admin DM commands

These commands work only in private chat with the bot and only for users listed in `BOT__ADMIN_IDS`. If `BOT__ADMIN_IDS`
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
  -e BOT__TELEGRAM__TOKEN=12345 \
  -e LLM__BACKENDS__OPENAI_COMPAT__BASE_URL=http://ollama.localhost:11434/v1 \
  -e LLM__BACKENDS__OPENAI_COMPAT__API_TOKEN=dummy \
  -e LLM__FEATURES__CHAT__MODEL=gemma4:e4b \
  -e PERSISTENT__STORE_PATH=/data/db.sqlite \
  -e STATE__HISTORY_STREAMS_MAX=1024 \
  -e STATE__IMAGE_CACHE_TTL=24h \
  -e LLM__UNCOMPRESSED_HISTORY_LIMIT=15 \
  -e LOG__LEVEL=info \
  -e SENTRY__DSN=https://your-sentry-dsn \
  -e BOT__ADMIN_IDS=123456789,987654321 \
  -e MCP__SERVERS__BROWSER__URL=https://mcp.example.com/mcp \
  -e MCP__SERVERS__BROWSER__HEADERS__AUTHORIZATION="Bearer secret-token" \
  -v bot-data:/data \
  skobkin/telegram-llm-bot
```

The bot uses only the OpenAI-compatible LLM API. For Ollama deployments, use Ollama's `/v1` OpenAI-compatible endpoint rather than the native Ollama API.

`LLM__FEATURES__SUMMARIZE__MODEL` and `LLM__FEATURES__IMAGE_RECOGNITION__MODEL` normally inherit `LLM__FEATURES__CHAT__MODEL`.
Set them only when you intentionally want summarization or image recognition to use different models.

### Docker Compose

An example Compose-based deployment is maintained in the existing stack repository:

https://git.skobk.in/skobkin/docker-stacks/src/branch/master/telegram-llm-bot
