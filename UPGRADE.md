# Upgrade Notes

## Explicit access policy (breaking)

Access control is now an explicit policy instead of the old implicit rule "an empty whitelist means allow everyone".

- The database gains an `access_policy` column (`allowlist` or `allow_all`), migrated to **`allowlist`** for every existing install.
- **Breaking for installs with an empty whitelist**: previously the bot answered every chat; now all ordinary chats are denied until you either add them to the allowlist or explicitly switch the policy to `allow_all`. Installs with a populated whitelist behave exactly as before — entries are preserved and still allowed.
- Admin DMs (`BOT__ADMIN_IDS`) are always exempt, and `BOT__DM_ADMINS_ONLY` keeps working unchanged. Switching the policy never modifies allowlist membership, so you can switch back and forth without data loss.
- **Scheduled reminders are affected**: reminders for chats denied by the new policy stop delivering (their occurrences are skipped and schedules advance); reminders to admin DMs keep working.
- Remediation after upgrade: open `/admin` → *Access* in an admin DM, add the chats you serve, or confirm the switch to `allow_all` there.

## DM restrictions (#62)

New optional env var `BOT__DM_ADMINS_ONLY` (default `false` — unset means today's behavior):

- `true`: private-chat messages from users not listed in `BOT__ADMIN_IDS` are silently dropped
  before any processing — nothing is stored in history, cataloged, counted, or answered, and no
  command (including `/summarize`) runs for them. Group/supergroup chats and the existing
  whitelist/interactivity rules are unaffected.
- **Warning:** admins are identified solely by `BOT__ADMIN_IDS`, so enabling the flag with an empty
  `BOT__ADMIN_IDS` drops **all** private messages, including admin commands sent from the operator's
  own DMs. Set `BOT__ADMIN_IDS` first.

## Model-prompted image re-analysis tools (#107/#90 hybrid)

While image recognition is enabled, the model receives two new tools: `list_recent_images` (identify recent
image-bearing messages by `message_id` in the current chat/topic) and `analyze_image` (re-analyze one of those images
under the model's own focused prompt). The baseline cached-description flow is unchanged, and prompt-specific analyses
are never cached or stored.

- No configuration changes: the tools appear only while `LLM__IMAGE_RECOGNITION_ENABLED=true` and work with both
  the `openai_compat` and `minimax_vlm` recognizers.
- Image originals are not stored locally; the bot re-fetches them from Telegram through the `file_id` references
  already kept in ephemeral in-memory history. An image stays analyzable while a reference is reachable from the
  current chat/topic context — the stored history, the current message, or its reply chain — even if the original
  message was trimmed from the stored history. Once no reachable reference remains, the tool reports that
  predictably. A restart drops all previously retained history; images in messages arriving after the restart —
  or quoted in their reply chains — become analyzable again as soon as those references exist.
- Each valid, resolved `analyze_image` invocation can trigger one recognition operation — and only after the
  image download and validation succeed: a failed download, an image over the size bound, or an unsupported
  image type all fail before any recognizer call. Charges depend on the provider and plan (a self-hosted
  OpenAI-compatible backend may be free). Invalid prompts and unresolved images also cause no recognition
  operation, and the model may invoke the tool several times within a single reply:
  `LLM__TOOL_LOOP_MAX_ITERATIONS` bounds model/tool rounds, not the number of tool calls inside one round.
  There is no fixed recognition count per user message.
- Resolution only ever accepts the current message, its reply chain, and the current chat/topic history — never
  model-supplied file IDs, URLs, or paths.

## Image recognition back-ends (MiniMax VLM support)

Image recognition is extracted behind a dedicated recognizer layer. The default configuration is unchanged: the
`openai_compat` recognizer keeps using the multimodal chat model (`LLM__FEATURES__IMAGE_RECOGNITION__MODEL`, inheriting
the chat model) on the conversational backend, and `LLM__IMAGE_RECOGNITION_ENABLED` keeps working exactly as before.

New optional env vars (all additive — one new default applies, see `MAX_IMAGE_BYTES` below):

| Variable                                           | Description                                                                                            | Default                  |
|----------------------------------------------------|--------------------------------------------------------------------------------------------------------|--------------------------|
| `LLM__IMAGE_RECOGNITION__RECOGNIZER`               | `openai_compat` (default) or `minimax_vlm`                                                              | `openai_compat`          |
| `LLM__IMAGE_RECOGNITION__MAX_IMAGE_BYTES`          | Client-side bound on downloaded image size, in bytes. Must be a positive integer; values `<= 0` are a startup error. Raise it if your users send larger images (e.g. `20971520` for 20 MiB) | `10485760` (10 MiB) |
| `LLM__IMAGE_RECOGNITION__MINIMAX_VLM__BASE_URL`    | MiniMax endpoint base; must match the key's region                                                      | `https://api.minimax.io` |
| `LLM__IMAGE_RECOGNITION__MINIMAX_VLM__API_KEY`     | MiniMax key; required only when the recognizer is `minimax_vlm` **and** recognition is enabled; never inherited from the OpenAI-compat token | -                        |
| `LLM__IMAGE_RECOGNITION__MINIMAX_VLM__TIMEOUT`     | Per-request HTTP timeout for MiniMax recognition calls; earlier parent-context cancellation can shorten it (see notes below) | `60s`                    |
| `LLM__IMAGE_RECOGNITION__OPENAI_COMPAT__BASE_URL`  | Optional dedicated OpenAI-compatible endpoint for recognition. Empty reuses the conversational backend and its token. When set, recognition uses this endpoint with its own `API_TOKEN` — the conversational token is not reused | empty                    |
| `LLM__IMAGE_RECOGNITION__OPENAI_COMPAT__API_TOKEN` | Token for the dedicated endpoint; only valid together with its `BASE_URL`                              | empty                    |

The dedicated OpenAI-compatible endpoint (like the default route) still selects its model with
`LLM__FEATURES__IMAGE_RECOGNITION__MODEL`, inherited from `LLM__FEATURES__CHAT__MODEL` when unset —
make sure the model exists on whichever endpoint serves recognition.

Example — MiniMax as the recognizer:

```env
LLM__IMAGE_RECOGNITION_ENABLED=true
LLM__IMAGE_RECOGNITION__RECOGNIZER=minimax_vlm
LLM__IMAGE_RECOGNITION__MINIMAX_VLM__API_KEY=your-minimax-key
# optional: LLM__IMAGE_RECOGNITION__MINIMAX_VLM__BASE_URL=https://api.minimaxi.com  (China region)
```

Example — a dedicated OpenAI-compatible endpoint just for recognition:

```env
LLM__IMAGE_RECOGNITION_ENABLED=true
LLM__IMAGE_RECOGNITION__RECOGNIZER=openai_compat
LLM__IMAGE_RECOGNITION__OPENAI_COMPAT__BASE_URL=https://vision.example.com/v1
LLM__IMAGE_RECOGNITION__OPENAI_COMPAT__API_TOKEN=your-vision-token
LLM__FEATURES__IMAGE_RECOGNITION__MODEL=your-vision-model
```

Behavior changes to be aware of:

- The MiniMax per-request timeout (`LLM__IMAGE_RECOGNITION__MINIMAX_VLM__TIMEOUT`, default `60s`) can be
  shortened by earlier parent-context cancellation. `analyze_image` runs inside the tool loop under
  `BOT__PROCESSING_TIMEOUT` (default `30s`) and shares its remaining budget, so raise that too when you want
  longer tool-driven analyses. The initial baseline description of an incoming image is hydrated before that
  processing deadline is applied, so `BOT__PROCESSING_TIMEOUT` is not a universal limit on image processing.

- Image MIME types are now detected from the image's leading byte signature (JPEG/PNG/WebP accepted; other formats
  are rejected locally). Previously the bot hardcoded `image/jpeg`. Signature detection selects the format — it is
  not an integrity check of the image data.
- Telegram file downloads and recognition input are bounded by `LLM__IMAGE_RECOGNITION__MAX_IMAGE_BYTES`
  (a bot-side limit, independent of any provider-side limits). Files whose advertised size exceeds the
  bound are skipped without downloading; unknown or underreported sizes are bounded during the download itself
  (the download is stopped once the transferred data is known to exceed the bound).
- With recognition disabled (`LLM__IMAGE_RECOGNITION_ENABLED=false`) or a model-free recognizer (`minimax_vlm`)
  selected, the startup check no longer requires an image model to be present. Previously the bot refused to boot
  when the image model was missing even with recognition disabled. With a dedicated image endpoint configured, the
  image model is checked against that endpoint, not the conversational one.
- The MiniMax route uses no image LLM model and reports no token usage; because usage is unknown, its requests
  are not tracked in the token/cost statistics (`/stats` unchanged for the OpenAI-compatible route).

## Bot output now uses Telegram Rich Messages

LLM replies, `/summarize` and `/stats` output are sent as [Rich Messages](https://core.telegram.org/bots/api)
(rich-message sending was introduced in
[Bot API 10.1](https://core.telegram.org/bots/api-changelog#june-11-2026)) and render GitHub Flavored
Markdown natively: headings, tables, task lists, footnotes, spoilers and code fences now look the way
the model intended them. The reply limit rose from 4000 to 32768 characters; over-length replies are
cropped with an ellipsis as before.

Self-hosted Bot API endpoints need a build that supports the rich-message method. Against an older
endpoint the rich send simply fails and the automatic fallback below is used — upgrade the endpoint
to get rich output.

The old MarkdownV2 sanitizer pipeline is kept as an automatic per-message fallback: when a rich
message send fails for any reason, the reply is re-sent through the legacy pipeline (still subject
to the legacy 4000-character limit) before the plain error message. Crops on either path reserve
room for appended suffixes such as the `/summarize` source link, so the suffix always survives.
There are no configuration changes.

Requirements: `telego` upgraded v1.8.0 → v1.12.1; Go ≥ 1.26.7 is now needed to build.

## Optional external MCP tool servers (Streamable HTTP)

The bot can now connect to one or more external [Model Context Protocol](https://modelcontextprotocol.io)
(MCP) servers over the Streamable HTTP transport and expose their tools to the LLM alongside the built-ins.
Each remote tool is registered under the namespaced name `mcp_<server>__<tool>` (lowercased and
`[a-z0-9_]`-normalized). MCP is off by default (`MCP__ENABLED=false`); nothing is required to keep using the
bot as before. When servers are configured while the flag is left disabled, the bot logs a warning at startup and
runs without MCP tools; per-server validation is skipped while disabled, so a forgotten enable flag cannot block boot.

New env vars (`MCP__ENABLED`, plus per-server `MCP__SERVERS__<NAME>__*`):

| Variable                                    | Description                                                                                               |
|---------------------------------------------|-----------------------------------------------------------------------------------------------------------|
| `MCP__ENABLED`                              | Master switch; MCP tools are registered only when `true`.                                                 |
| `MCP__SERVERS__<NAME>__URL`                 | Streamable HTTP endpoint (e.g. `https://mcp.example.com/mcp`)                                             |
| `MCP__SERVERS__<NAME>__HEADERS__<SEG>`      | HTTP header; multiple segments after `HEADERS` join with `-` (e.g. `X__API__KEY` = `X-Api-Key`).          |
| `MCP__SERVERS__<NAME>__INSECURE`            | Allow plain `http://` for this server.                                                                    |
| `MCP__SERVERS__<NAME>__TIMEOUT`             | Per-tool-call timeout (Go duration, default `15s`); caps tool calls, not the standing connection.         |
| `MCP__SERVERS__<NAME>__INVOCATION_POLICY`   | Default invocation policy: `discretionary`, `discretionary_paid`, or `explicit_request_only`.             |
| `MCP__SERVERS__<NAME>__SIDE_EFFECTING`      | Force `true`/`false` for the side-effecting flag.                                                         |
| `MCP__SERVERS__<NAME>__RESULT_CHAR_BUDGET`  | Maximum characters of a tool result forwarded to the model (default `3500`); must be positive when set.   |
| `MCP__SERVERS__<NAME>__ALLOWED_TOOLS`       | Whitelist of tool names to expose. Misnamed entries fail at startup.                                      |
| `MCP__SERVERS__<NAME>__RESTRICTED_TOOLS`    | Deny-list of tool names to drop. Misnamed entries fail at startup.                                        |
| `MCP__SERVERS__<NAME>__OPTIONAL`            | When `true`, a connection failure on startup is logged and skipped instead of aborting the bot.           |

Notable behaviours:

- HTTPS is the default per server; `INSECURE=true` is required to use a `http://` URL.
- Entries in `ALLOWED_TOOLS` or `RESTRICTED_TOOLS` that the server does not advertise are a hard startup error
  for non-optional servers (catches typos early).
- Header names are given as path segments after `HEADERS`: multiple segments join with `-`
  (`HEADERS__X__API__KEY=v` sends `X-Api-Key`), while a single segment keeps its literal underscores
  (`HEADERS__X_API_KEY=v` sends `X_Api_Key`).
- `TIMEOUT` has no upper limit (slow tools such as browser automation can use `120s` or more); it caps individual
  tool calls, not the standing server connection.
- `RESULT_CHAR_BUDGET` must be a positive integer when set; values `<= 0` are a startup configuration error.
- The default invocation policy is `discretionary` for tools with `readOnlyHint=true`, and `explicit_request_only`
  otherwise. `discretionary_paid` behaves like `discretionary` but tells the model to reserve the tool for calls
  that need accuracy or freshness, since each call consumes a paid external resource.
  The default side-effecting flag is `destructiveHint`, treated as `true` when the annotation is
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
| `LLM_IMAGE_RECOGNITION_ENABLED`                  | `LLM__IMAGE_RECOGNITION_ENABLED`                     |
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

### Value parsing semantics changed

- Booleans: parsed with `strconv.ParseBool` semantics, so `TRUE`, `True`, `t`, and `1`
  all mean true. Previously anything but the exact string `true` meant false, so
  `LLM__IMAGE_RECOGNITION_ENABLED=TRUE` (or `=1`), which used to leave image recognition
  disabled, now enables it. Values like `off`/`on` previously silently meant false; they
  now abort startup.
- Integers: parsed in base 10, so leading zeros no longer switch to octal (`010` is 10,
  `08` is 8). A value that fails to parse is a hard startup error instead of silently
  falling back to the default.
- `BOT__ADMIN_IDS`: an unparseable entry is a fatal startup error. The old loader
  silently skipped it, which could silently drop an admin's access.
- An environment variable whose path collides with a config section of mismatched type
  (e.g. `MCP__SERVERS=prod`) now aborts startup. Unknown top-level prefixes (`FOO__BAR`)
  are still ignored.

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
