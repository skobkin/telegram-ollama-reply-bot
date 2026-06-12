# Upgrade Notes

## Optional external MCP tool servers (Streamable HTTP)

The bot can now connect to one or more external [Model Context Protocol](https://modelcontextprotocol.io)
(MCP) servers over the Streamable HTTP transport and expose their tools to the LLM alongside the built-ins.
Each remote tool is registered under the namespaced name `mcp_<server>__<tool>` (lowercased and
`[a-z0-9_]`-normalized). MCP is off by default; nothing is required to keep using the bot as before.

New env vars (all under `MCP__SERVERS__<NAME>__*`):

| Variable                                  | Description                                                                                              |
|-------------------------------------------|----------------------------------------------------------------------------------------------------------|
| `MCP__SERVERS__<NAME>__URL`               | Streamable HTTP endpoint (e.g. `https://mcp.example.com/mcp`)                                            |
| `MCP__SERVERS__<NAME>__HEADERS__<KEY>`    | HTTP header sent to that server. Values are never logged.                                                |
| `MCP__SERVERS__<NAME>__INSECURE`          | Allow plain `http://` for this server.                                                                   |
| `MCP__SERVERS__<NAME>__INVOCATION_POLICY` | Default invocation policy: `discretionary` or `explicit_request_only`.                                  |
| `MCP__SERVERS__<NAME>__SIDE_EFFECTING`    | Force `true`/`false` for the side-effecting flag.                                                        |
| `MCP__SERVERS__<NAME>__ALLOWED_TOOLS`     | Whitelist of tool names to expose. Misnamed entries fail at startup.                                     |
| `MCP__SERVERS__<NAME>__RESTRICTED_TOOLS`  | Deny-list of tool names to drop.                                                                         |
| `MCP__SERVERS__<NAME>__OPTIONAL`          | When `true`, a connection failure on startup is logged and skipped instead of aborting the bot.         |

Notable behaviours:

- HTTPS is the default per server; `INSECURE=true` is required to use a `http://` URL.
- An entry in `ALLOWED_TOOLS` that the server does not advertise is a hard startup error (catches typos early).
- The default invocation policy is `discretionary` for tools with `readOnlyHint=true`, and `explicit_request_only`
  otherwise. The default side-effecting flag is `destructiveHint`, treated as `true` when the annotation is
  absent; `readOnlyHint=true` always forces the side-effecting flag to `false` per the MCP spec.
- A successful `MCP__SERVERS__*` config registers only the namespaced tools; the built-in toolset is untouched.
- STDIO transport is intentionally not supported in this iteration; only Streamable HTTP is wired.

## Configuration loader migrated to Koanf (env var path delimiter changed to `__`)

The hand-rolled environment loader was replaced with
[`knadh/koanf`](https://github.com/knadh/koanf) v2 using its
[`env/v2`](https://pkg.go.dev/github.com/knadh/koanf/providers/env/v2) provider.
The provider splits env var names on a `__` delimiter to build a config path
through the `Config` struct; snake_case `_` inside a single path segment is
preserved as-is. As a result, every bot env var has been renamed.

This is a breaking change. Containers started with the old env var names will
silently fall back to defaults (or fail to boot when a required value is
missing). Rename every variable in your deployment before pulling the new
image.

The full mapping:

| Old (single underscore)                          | New (double underscore)                              |
|--------------------------------------------------|------------------------------------------------------|
| `TELEGRAM_TOKEN`                                 | `BOT__TELEGRAM__TOKEN`                               |
| `LLM_BACKEND_OPENAI_COMPAT_BASE_URL`             | `LLM__BACKENDS__OPENAI_COMPAT__BASE_URL`             |
| `LLM_BACKEND_OPENAI_COMPAT_API_TOKEN`            | `LLM__BACKENDS__OPENAI_COMPAT__API_TOKEN`            |
| `LLM_FEATURE_CHAT_MODEL`                         | `LLM__FEATURES__CHAT__MODEL`                         |
| `LLM_FEATURE_SUMMARIZE_MODEL`                    | `LLM__FEATURES__SUMMARIZE__MODEL`                    |
| `LLM_FEATURE_IMAGE_RECOGNITION_MODEL`            | `LLM__FEATURES__IMAGE_RECOGNITION__MODEL`            |
| `LLM_TOOL_LOOP_MAX_ITERATIONS`                   | `LLM__TOOL_LOOP_MAX_ITERATIONS`                     |
| `LLM_UNCOMPRESSED_HISTORY_LIMIT`                 | `LLM__UNCOMPRESSED_HISTORY_LIMIT`                   |
| `LLM_HISTORY_SUMMARY_THRESHOLD`                  | `LLM__HISTORY_SUMMARY_THRESHOLD`                    |
| `BOT_ADMIN_IDS`                                  | `BOT__ADMIN_IDS`                                    |
| `BOT_PROCESSING_TIMEOUT`                         | `BOT__PROCESSING_TIMEOUT`                           |
| `STATE_MAX_BYTES`                                | `STATE__MAX_BYTES`                                  |
| `STATE_HISTORY_MAX_BYTES`                        | `STATE__HISTORY_MAX_BYTES`                          |
| `STATE_HISTORY_STREAMS_MAX`                      | `STATE__HISTORY_STREAMS_MAX`                        |
| `STATE_IMAGE_CACHE_MAX_BYTES`                    | `STATE__IMAGE_CACHE_MAX_BYTES`                      |
| `STATE_IMAGE_CACHE_TTL`                          | `STATE__IMAGE_CACHE_TTL`                            |
| `SENTRY_DSN`                                     | `SENTRY__DSN`                                       |
| `LOG_LEVEL`                                      | `LOG__LEVEL`                                        |
| `PERSISTENT_STORE_PATH`                          | `PERSISTENT__STORE_PATH`                            |
| `PROVIDER_TAVILY_API_KEY`                        | `PROVIDERS__TAVILY__API_KEY`                        |
| `PROVIDER_KAGI_API_KEY`                          | `PROVIDERS__KAGI__API_KEY`                          |
| `SEARCH_BACKEND`                                 | `SEARCH__BACKEND`                                   |
| `SEARCH_BACKEND_CHAIN`                           | `SEARCH__CHAIN`                                     |

Notes:

- `PROVIDER_*` is now `PROVIDERS__*` (the key was previously singular).
- `SEARCH_BACKEND_CHAIN` was folded into the `SEARCH__CHAIN` config field
  (the `SEARCH_BACKEND__CHAIN` sub-key is no longer supported).
- Defaults are unchanged. See `README.md` for the current defaults and the
  full env var table.

The `internal/config.Load()` signature changed from `Load() *Config` to
`Load() (*Config, error)`. Errors are returned when an env value is present
but cannot be parsed (e.g. an integer that fails to convert). Callers must
handle the new error return.

## 2026-05-18 Changes against previous stable version.

These notes are for users upgrading from previous stable to the latest version that introduces persistent admin configuration, privacy-bounded in-memory state, tool-assisted chat, reminders, optional web search, and OpenAI-compatible-only LLM access.

### LLM Configuration

The bot uses only an OpenAI-compatible LLM API

Replace the old flat variables:

- `OPENAI_API_BASE_URL` -> `LLM_BACKEND_OPENAI_COMPAT_BASE_URL`
- `OPENAI_API_TOKEN` -> `LLM_BACKEND_OPENAI_COMPAT_API_TOKEN`
- `MODEL_TEXT_REQUEST` -> `LLM_FEATURE_CHAT_MODEL`
- `MODEL_SUMMARIZE_REQUEST` -> `LLM_FEATURE_SUMMARIZE_MODEL`
- `MODEL_IMAGE_RECOGNITION` -> `LLM_FEATURE_IMAGE_RECOGNITION_MODEL`

Remove these branch-only backend routing variables if you tested an earlier revision of this update:

- `LLM_BACKEND_OLLAMA_BASE_URL`
- `LLM_FEATURE_CHAT_BACKEND`
- `LLM_FEATURE_SUMMARIZE_BACKEND`
- `LLM_FEATURE_IMAGE_RECOGNITION_BACKEND`
- `LLM_FEATURE_TOOL_USE_BACKEND`

If you run Ollama, point the bot at Ollama's OpenAI-compatible endpoint:

```env
LLM_BACKEND_OPENAI_COMPAT_BASE_URL=http://ollama.localhost:11434/v1
LLM_BACKEND_OPENAI_COMPAT_API_TOKEN=dummy
LLM_FEATURE_CHAT_MODEL=gemma4:e4b
```

`LLM_FEATURE_CHAT_MODEL` is required and must be set explicitly, including in Docker deployments. The container image no longer provides baked-in model defaults.

`LLM_FEATURE_SUMMARIZE_MODEL` and `LLM_FEATURE_IMAGE_RECOGNITION_MODEL` inherit `LLM_FEATURE_CHAT_MODEL` when unset or empty. Set them only when you intentionally want summarization or image recognition to use different models.

### Persistent Store And Admin Configuration

The bot now uses SQLite for operator-managed configuration and durable reminders.

Set or mount:

- `PERSISTENT_STORE_PATH`

Default: `/data/db.sqlite`

Use a durable path outside the project directory. In containers, mount a volume at `/data` or override the path explicitly.

These prompt and persona environment variables were removed:

- `PROMPT_CHAT`
- `PROMPT_SUMMARIZE`
- `PROMPT_IMAGE_RECOGNITION`
- `RESPONSE_LANGUAGE`
- `RESPONSE_GENDER`
- `MAX_SUMMARY_LENGTH`

Their defaults are seeded into SQLite on first boot. Manage prompts, response language, persona settings, whitelist entries, and per-chat overrides from Telegram admin DMs.

`BOT_ADMIN_IDS` is now the trust root for admin access. If it is empty, admin controls are disabled.

Chat interactivity is disabled by default. Enable it from an admin DM, for example:

- `/config_set_global default_interactivity_mode mentions_or_replies`
- `/config_set_chat <chat_id> interactivity_mode mentions_only`

Per-chat persona overrides now use `character_name` instead of `alias`:

- old: `/config_set_chat <chat_id> alias <name>`
- new: `/config_set_chat <chat_id> character_name <name>`

Existing SQLite `chat_settings.alias` values migrate automatically to `chat_settings.character_name`. Custom chat
prompt templates should keep using `{{.CharacterName}}`; `{{.Alias}}` is not exposed.

### State And History

The bot now keeps privacy-bounded in-memory chat state, image cache data, summaries, and history indexes.

The old history size variable was removed:

- `STATE_HISTORY_MESSAGES_PER_STREAM`

Use these state limits instead:

- `STATE_MAX_BYTES`
- `STATE_HISTORY_MAX_BYTES`
- `STATE_HISTORY_STREAMS_MAX`
- `STATE_IMAGE_CACHE_MAX_BYTES`
- `STATE_IMAGE_CACHE_TTL`

The in-memory history store keeps full raw per-scope history until the configured memory limits force trimming or stream eviction. `LLM_UNCOMPRESSED_HISTORY_LIMIT` still controls how many newest messages are sent verbatim to the model.

### Tool-Assisted Chat

Ordinary chat replies now always use the tool-capable chat workflow. This is a breaking change: `LLM_FEATURE_CHAT_MODEL` must support OpenAI-compatible tool calls, or ordinary chat requests will fail at runtime.

Removed variable:

- `LLM_FEATURE_TOOL_USE_MODEL`

If you customized the old `tool_use` prompt template, move the relevant content manually into the `chat` prompt template. Existing custom `tool_use` rows are not copied or used. Existing `chat` prompt templates are upgraded automatically only when they exactly match the previous built-in default.

New variable:

- `LLM_TOOL_LOOP_MAX_ITERATIONS`

Default: `6`

The current tool set includes:

- `fetch_url_content`
- `search_web`
- `search_history`
- `get_conversation_summary`
- `get_current_time`
- `datetime_math`
- `datetime_format`
- `get_chat_activity_window`
- `get_history_bounds`
- `get_message_thread_context`
- `send_poll`
- `send_quiz`
- `send_dice`
- `list_chat_schedule`
- `add_schedule_item`
- `remove_schedule_item`

`/summarize` still uses the direct extractor-plus-summary workflow and does not depend on the tool loop.

### Reminders

Reminder tools are implemented and persist through SQLite.

Supported behavior:

- one-shot reminders use exact RFC3339 timestamps internally
- recurring reminders support interval days, interval weeks, weekday rules, and monthly rules
- calendar-based reminders require an IANA timezone such as `Europe/Moscow`
- reminders survive restarts and are delivered back into the original chat topic when `topic_id` is present
- after downtime, recurring reminders emit only the latest missed occurrence and then continue on schedule

### Optional Web Search

External web search is disabled by default.

New variables:

- `SEARCH_BACKEND`
- `SEARCH_BACKEND_CHAIN`
- `PROVIDER_TAVILY_API_KEY`
- `PROVIDER_KAGI_API_KEY`

Set `SEARCH_BACKEND` to one of:

- `none`
- `tavily`
- `kagi`
- `chain`

When using `SEARCH_BACKEND=chain`, set `SEARCH_BACKEND_CHAIN` to a comma-separated ordered list such as:

```env
SEARCH_BACKEND_CHAIN=tavily,kagi
```

If `SEARCH_BACKEND` is unset or set to `none`, the `search_web` tool is not exposed to the model.
