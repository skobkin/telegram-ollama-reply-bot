# Upgrade Notes

## 2026-04-20 - Full raw in-memory history replaces per-stream message cap

### State Config Migration

The in-memory history store now keeps full raw per-scope history until the global history RAM limits force trimming or stream eviction.

Removed environment variable:

- `STATE_HISTORY_MESSAGES_PER_STREAM`

Keep using:

- `STATE_HISTORY_MAX_BYTES`
- `STATE_HISTORY_STREAMS_MAX`

`LLM_UNCOMPRESSED_HISTORY_LIMIT` still controls how many newest messages are sent verbatim to the model, while history tools operate on the full raw in-memory scope.

### Tooling Changes

- `search_recent_history` was renamed to `search_history`
- `search_history` now accepts `keywords` plus `match_mode` (`all` or `any`) instead of the old single `query` string
- `get_history_bounds` was added

## 2026-04-19 - Conversational tool calling introduced for ordinary chat

### Tool-Use Route

`LLM_FEATURE_TOOL_USE_BACKEND` and `LLM_FEATURE_TOOL_USE_MODEL` now control a real conversational tool-calling path for ordinary chat replies.

If the route is configured and supported by the backend, free-form chat replies may use tools before responding. If tool use is unavailable, the bot falls back to the normal chat route.

`/summarize` remains on the existing direct extractor-plus-summary workflow.

### Tool Loop Config

New environment variable:

- `LLM_TOOL_LOOP_MAX_ITERATIONS`

Default: `6`

This controls how many model-tool iterations one ordinary chat reply may use before the request stops.

### Prompt Features

New prompt feature:

- `tool_use`

It is stored in SQLite alongside the existing prompt templates and can be managed from admin DMs through the existing prompt commands.

### First Tool Slice

The first conversational tool set includes:

- `fetch_url_content`
- `search_history`
- `get_conversation_summary`
- `create_poll`

Reminder tools are also exposed to the model:

- `list_chat_schedule`
- `add_schedule_item`
- `remove_schedule_item`

These reminder tools are currently scaffolding only and return structured `not_implemented` results until scheduling and persistence are implemented.

## 2026-04-19 - SQLite-backed admin config and DM control introduced

### Persistent Store Path

The bot now uses a persistent SQLite database path:

- `PERSISTENT_STORE_PATH`

The default path is `/data/db.sqlite`. Use a durable location outside the project directory. In containers, mount a volume at `/data` or override the path explicitly.

### Prompt And Persona Config Migration

The following environment variables were removed:

- `PROMPT_CHAT`
- `PROMPT_SUMMARIZE`
- `PROMPT_IMAGE_RECOGNITION`
- `RESPONSE_LANGUAGE`
- `RESPONSE_GENDER`
- `MAX_SUMMARY_LENGTH`

Their defaults are now seeded into SQLite on first boot and can be changed from Telegram admin DMs.

### Admin Controls

- `BOT_ADMIN_IDS` remains the trust root for admin access.
- If `BOT_ADMIN_IDS` is empty, nobody can configure the bot.
- Admin DMs bypass chat whitelist checks, but only for configured admin IDs.

### Chat Behavior Default

Chat interactivity is now `disabled` by default. Enable it explicitly with admin DM commands such as:

- `/config_set_global default_interactivity_mode mentions_or_replies`
- `/config_set_chat <chat_id> interactivity_mode mentions_only`

Per-chat overrides now also support:

- `language`
- `gender`

## 2026-04-19 - Privacy-first in-memory state layer introduced

### State Config Migration

The old history size variable was removed.

Add explicit state limits as needed:

- `STATE_MAX_BYTES`
- `STATE_HISTORY_MAX_BYTES`
- `STATE_HISTORY_STREAMS_MAX`
- `STATE_IMAGE_CACHE_MAX_BYTES`
- `STATE_IMAGE_CACHE_TTL`


## 2026-04-18 - Backend abstractions and Ollama backend introduced

### Backend Config Migration

The old flat LLM environment variables were removed.

Replace:

- `OPENAI_API_BASE_URL` -> `LLM_BACKEND_OPENAI_COMPAT_BASE_URL`
- `OPENAI_API_TOKEN` -> `LLM_BACKEND_OPENAI_COMPAT_API_TOKEN`
- `MODEL_TEXT_REQUEST` -> `LLM_FEATURE_CHAT_MODEL`
- `MODEL_SUMMARIZE_REQUEST` -> `LLM_FEATURE_SUMMARIZE_MODEL`
- `MODEL_IMAGE_RECOGNITION` -> `LLM_FEATURE_IMAGE_RECOGNITION_MODEL`

### New Required Routing

Set at least:

- `LLM_FEATURE_CHAT_MODEL`
- `LLM_FEATURE_CHAT_BACKEND` if you want something other than the default `openai_compat`

By default, summarize, image recognition, and future tool use inherit the chat backend/model unless explicitly overridden.

### OpenAI-Compatible Backend

If you use an OpenAI-compatible endpoint, set:

- `LLM_BACKEND_OPENAI_COMPAT_BASE_URL`
- `LLM_BACKEND_OPENAI_COMPAT_API_TOKEN`

Leave feature backends on `openai_compat` or set them explicitly.

### Ollama Native Backend

If you use Ollama native API, set:

- `LLM_BACKEND_OLLAMA_BASE_URL`
- `LLM_FEATURE_CHAT_BACKEND=ollama`
- `LLM_FEATURE_CHAT_MODEL=<your model>`

Optionally override:

- `LLM_FEATURE_SUMMARIZE_BACKEND`
- `LLM_FEATURE_SUMMARIZE_MODEL`
- `LLM_FEATURE_IMAGE_RECOGNITION_BACKEND`
- `LLM_FEATURE_IMAGE_RECOGNITION_MODEL`
- `LLM_FEATURE_TOOL_USE_BACKEND`
- `LLM_FEATURE_TOOL_USE_MODEL`

### Example

Old:

```env
OPENAI_API_BASE_URL=http://ollama.localhost:11434/v1
OPENAI_API_TOKEN=dummy
MODEL_TEXT_REQUEST=gemma3:27b
MODEL_SUMMARIZE_REQUEST=gemma3:12b
MODEL_IMAGE_RECOGNITION=gemma3:12b
```

New:

```env
LLM_BACKEND_OLLAMA_BASE_URL=http://ollama.localhost:11434
LLM_FEATURE_CHAT_BACKEND=ollama
LLM_FEATURE_CHAT_MODEL=gemma3:27b
LLM_FEATURE_SUMMARIZE_MODEL=gemma3:12b
LLM_FEATURE_IMAGE_RECOGNITION_MODEL=gemma3:12b
```
