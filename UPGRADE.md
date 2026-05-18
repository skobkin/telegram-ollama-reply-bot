# Upgrade Notes

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
LLM_FEATURE_SUMMARIZE_MODEL=gemma4:e4b
LLM_FEATURE_IMAGE_RECOGNITION_MODEL=gemma4:e4b
```

`LLM_FEATURE_SUMMARIZE_MODEL` and `LLM_FEATURE_IMAGE_RECOGNITION_MODEL` inherit `LLM_FEATURE_CHAT_MODEL` when unset.

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
