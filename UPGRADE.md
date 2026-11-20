# Upgrade Notes

## 0.30.0 (unreleased) — changes since 0.29.1

### MCP side-effect classification

MCP tools without `readOnlyHint=true` are now treated as potentially side-effecting even when
`destructiveHint=false`. Ambiguous failures will not recommend replay. Operators can explicitly
set `MCP__SERVERS__<NAME>__SIDE_EFFECTING` when their server requires an override.

### Conversation metadata replaces redundant tools (breaking, #179)

`get_conversation_summary`, `get_chat_activity_window`, and `get_history_bounds`
have been removed. Earlier summaries are already included in context; compact
metadata now supplies current server time/timezone, available retained-history
bounds and count, and the number of conversation messages actually supplied.
Retained RAM history is not complete Telegram history.

Custom persona templates continue to work without new placeholders or a database
migration. Remove instructions that ask the model to call the deleted tools;
use the automatically supplied summary and metadata instead. Custom prompts are
not rewritten. `get_current_time`, `datetime_format`, `datetime_math`,
`decision_choose`, and `decision_assess` remain supported.

### Model-authored reactions (#177)

Reaction candidates now follow cached Telegram chat availability (30-minute TTL).
Empty supported sets and unsuitable album targets omit the tool. Whitespace and
equivalent Unicode presentation variants are accepted. A turn can make up to
three outgoing attempts after definitive recoverable failures, with at most one
confirmed success; ambiguous failures and rate limits stop further reaction sends.
There is no automatic emoji substitution or retry and no new configuration.

### Tool result and error contract (#176)

Tool errors now carry stable codes, classifications, retryability and recovery hints.
Oversized results are reduced structurally and remain valid JSON, with explicit
truncation metadata. A result-delivery failure does not undo a confirmed action;
models are told not to repeat it blindly. No database migration is needed.

Explicit `MCP__SERVERS__<NAME>__RESULT_CHAR_BUDGET` values must now be at least
`512`; smaller values fail startup validation. Increase them before upgrading,
or omit the setting to retain the `3500` default.

### Intentional silent completion and failed-preview cleanup (#159, #156)

The generation model gains a discretionary no-argument `say_nothing` tool that ends the current turn
without another inference request or additional text. Prior tool actions remain applied; subsequent pending
calls are skipped. `set_reaction` still permits a later text reply, and restricted proactive React is unchanged.
Failed explicit turns now delete their temporary group preview best-effort instead of leaving it visible.
Private drafts stop refreshing on intentional completion and expire naturally. No configuration, database,
or custom-prompt migration is needed.

### Tone mode and teasing prompt parameters removed (breaking, #153)

The `tone_mode` and `allow_teasing` settings are gone: both were pure prompt fragments (no code branched on
them), and the editable prompt templates are strictly more expressive — bake any tone or teasing rules directly
into the `chat` prompt template (admin panel → Prompts → chat).

- The default chat prompt no longer contains the «Твой режим тона: …» and «Шутливые поддразнивания …» lines, and
  the `{{.ToneMode}}` / `{{.AllowTeasing}}` placeholders no longer exist in the template data. Schema version 11
  (applied automatically at startup) refreshes stored copies of the built-in default prompt and drops the
  `tone_mode` / `allow_teasing` columns from `global_settings` and `chat_settings`; no operator action is needed
  for stock deployments.
- **Breaking for customized prompts**: the migration does not touch custom template text (only copies that
  byte-exactly match the previous built-in default are auto-upgraded). If your edited `chat` prompt still
  contains `{{.ToneMode}}` or `{{.AllowTeasing}}`, remove those lines manually before upgrading — after the
  upgrade the placeholders render nothing, template execution fails, and the bot stops replying in chats served
  by that template.

### Runtime model selection, interactive participation, and System One decision tools (additive, #128 #157 #158)

Model selection moves from ENV-only routes into SQLite settings (global defaults + per-chat overrides), a System One
decision backend gains opt-in proactive participation and two decision tools, and everything ships behind defaults
that preserve current behavior. Schema version 12 (applied automatically at startup) adds the new columns with
behavior-preserving defaults; no operator action is required for an upgrade that changes nothing.

- **What is new**:
  - `chat_model`, `summarize_model`, `image_recognition_model` (global, with per-chat override for `chat_model`) —
    when set, the stored model overrides the `LLM__FEATURES__*` ENV route for that feature; ENV remains the fallback
    for unset fields, and a global clear action ("Reset to ENV default" in the admin UI) returns a field to the route.
  - **Startup model validation checks the effective selection** (the global override when set, otherwise the ENV
    route) against the backend's served model list. An obsolete ENV route model no longer blocks boot while a
    working runtime override is set — the unused route is reported as a warning instead. An unset override still
    validates the route, so resetting a model to an unavailable ENV fallback fails the check loudly, and a feature
    whose effective model cannot be verified fails the check as before.
  - `default_interaction_profile` / `interaction_profile` — interactive participation presets
    (`non_interactive` by default, so nothing proactive happens until an operator opts in). Enabling any interactive
    profile as the *global* default asks for a scope confirmation in the admin UI (every inheriting chat starts
    participating); per-chat overrides apply directly.
  - `systemone_model` (global default + per-chat override) — the decision model used by proactive participation
    and the `decision_choose` / `decision_assess` tools. Clearing the *global* default ("Clear global default
    model") does **not** disable the backend: chats with explicit per-chat overrides keep their effective model.
    With no override anywhere effective, proactive participation ignores messages and the decision tools are not
    advertised — the backend endpoint itself stays configured.
  - `decision_instructions` (global + per-chat, multi-line, up to 4000 chars) — operator guidance mixed into every
    decision request. A per-chat value of exactly `''` is not representable as "explicitly empty" (empty means
    inherit), the same limitation as every other inheritable string field.
  - New `SYSTEMONE__*` environment section (`BASE_URL`, `API_KEY`, `TIMEOUT` default `60s`,
    `MAX_RESPONSE_BYTES` default 4 MiB, `INPUT_BUDGET_BYTES` default 96 KiB) — see README. The endpoint and its
    credentials deliberately stay deployment configuration; model selection lives in the database.
- **Deployment guidance**: the decision backend must speak the Jev protocol (`POST /v1/systemone`) — llama.cpp
  ≥ b11361 (`llama-server` with a decision GGUF) or llama-swap ≥ v262 (routes by the body `model` field;
  deployments typically require a Bearer token via `SYSTEMONE__API_KEY`). Decision-model suggestions: Laya,
  Julia-1, and Clef are Apache-2.0; OpenJev-27B is CC BY-NC — check the license before deploying commercially.
- **Operator responsibilities**: `SYSTEMONE__INPUT_BUDGET_BYTES` is a composition limit on the serialized request,
  not a fit guarantee — keeping requests inside the decision model's context window and the server's batch limits
  (llama.cpp rejects oversized batches with HTTP 500) is the operator's job. Backends with known cross-request
  concurrency issues (llama.cpp router mode, llama-swap model swapping under parallel decision traffic) may need
  conservative interaction-profile rollouts: proactive evaluation serializes per chat but several chats, explicit
  turns, and decision-tool calls can still overlap on one backend. Consider enabling request-payload logging on
  the backend during initial calibration to see exactly what the bot sends.
- **Calibration caveat**: decision temperatures live in the GGUF metadata, so reported probabilities are not
  guaranteed calibrated; the built-in profile thresholds are heuristic starting points and may need real-world
  tuning for a specific model (profiles are thresholds over relevance/contribution/intrusion/suitability scores —
  start from `Interactive-Reactions-Low` and watch the logs).

## 0.29.1 (2026-11-15) — changes since 0.29.0

### Admin menu is not re-created while a conversation run is active (#148)

The admin menu re-creation added in 0.29.0 (#145) fires ~1.5 s after chat activity goes quiet — the same
delay at which a conversation run's live preview draft appears in private chats, so in an admin DM with an
open session the fresh menu send landed right after the draft and flash-removed it, replacing the tool-status
preview with the admin menu for the whole run. Re-creation is now deferred while a conversation run (LLM/tool
loop) is active in that admin DM (tracked via run-lifecycle events) and happens only after the run finishes.

## 0.29.0 (2026-11-15) — changes since 0.28.0

### Bot's Telegram handle available in chat prompts (#142)

The chat system prompt now knows the bot's own Telegram handle. A new `{{.BotUsername}}` placeholder
(the handle without the leading `@`, from GetMe; empty when unknown) is available to `chat` templates,
and the built-in default template gained a conditional line introducing it
(`Твой username в Telegram: @...`). Schema version 9 (applied automatically) upgrades stored copies of
the previous built-in default; custom templates are untouched — add `{{.BotUsername}}` where you want
the handle to appear (it renders as nothing when unknown, so guard with `{{if .BotUsername}}`).

### Admin menu is re-created when chat activity buries it (#145)

The interactive admin panel's menu message no longer stays scrolled away when other messages
land in the admin chat (bot replies, `/summarize` output, reminders, tool-created polls, the
admin's own messages). After chat activity stays quiet for ~1.5 s, the bot sends a fresh menu
message at the bottom of the chat and deletes the buried one; button sequences stay valid and
session lifetime is unaffected. While an input prompt (ForceReply) or chat-picker keyboard is
awaiting a reply, re-creation is suppressed so the input stays at the bottom. Callback presses
never trigger re-creation (the admin pressing a button proves the menu is visible).

### Startup validates MiniMax, Tavily, and Kagi credentials (#143)

The bot now probes the configured MiniMax VLM key and the configured search
provider keys (Tavily, Kagi, including each chain member) at startup with one
minimal real API call each (bounded at 15 s per probe). A definitive
rejection — the key is refused or the account is out of credits — aborts the
boot with the provider named in the error; transient failures (network,
timeouts, rate limits, upstream errors) are logged and the boot continues.
This catches dead keys at deploy time instead of at first use. Deployments
booting today with invalid or exhausted keys will now exit 1 until the key
is fixed or the feature is disabled. No Sentry DSN validation is added:
sentry-go verifies the DSN format at Init and offers no verification API.
The chat/image LLM tokens keep being validated by the existing model
listing; the Telegram token by GetMe; MCP by its connect handshake.

### The model can react to messages with `set_reaction` (#135)

New `set_reaction` tooluse tool: the model can attach one reaction from a fixed, Telegram-valid emoji
whitelist to messages in the chat (see README, "Model-authored reactions"). The bot's own reactions are
recorded on the history entries of the target messages like user reactions, so later model requests see them
in the same `[reactions: …]` context and can reuse them as outgoing candidates. Guards: already-used
("spent") reaction attempts are rejected before delivery, per-message cooldowns are pruned on ordinary sends,
and ambiguous send outcomes are reported to the model as unknown instead of guessed.

## 0.28.0 (2026-11-14) — changes since 0.27.0

### Live reply previews and collapsible tool history

The bot can now show a live tool-execution preview while generating a reply — a silent temporary message in groups,
a rich-message draft in private chats — and append a collapsible tool-history section to the final reply. See
README.md ("Live reply previews") for behavior details.

- New persistent settings, added by an automatic SQLite migration: global `show_reply_previews` (default **off**),
  `default_preview_mode` (default `tools`), `keep_tool_history` (default **on**); per-chat `preview_mode` and
  `keep_tool_history` (unset = inherit the global value).
- With the master switch off (the migrated default), runtime behavior is unchanged from the previous release: the
  plain typing indicator, no previews, no tool-history block.
- The master switch governs the per-chat fields: overrides stay stored but inert while it is off, and the chat
  admin screens hide the preview rows until it is enabled again.
- LLM token streaming (partial answer text inside the preview) is intentionally **not** part of this release and is
  planned as a follow-up; the internal event infrastructure already carries the event type for it.
- Rollback: redeploying the previous version leaves the new columns in place (they are simply ignored); no manual
  data migration is needed.

## 0.27.0 (2026-11-13) — changes since 0.26.0

### Reactions to bot replies are recorded and shown to the model (#100)

The bot now subscribes to `message_reaction` updates and stores reactions to its own replies on the
conversation history entry; later model requests and history summaries include them with reactor names
(`[reactions: 👍 Alice, 👎 Bob]`). Receive-only, fixed emoji set, no configuration.

- The update stream is now requested explicitly (`message`, `callback_query`, `message_reaction`) instead of
  Telegram's default set. Previously-delivered-but-ignored update types stop arriving; nothing in the old behavior is
  lost, since no handler consumed them.
- **Group chats**: Telegram delivers per-user reaction updates only to bots that are administrators in the chat.
  Deployments where the bot is a plain group member silently see no reactions; private chats are unaffected.
- Rollback is a plain redeploy of the old version: its long polling sends no `allowed_updates`, so Telegram returns to
  the default delivery set.

## 0.26.0 (2026-11-12) — changes since 0.25.0

### Extraction gate tolerates reference-dense pages; backend order is re-ranked per page

Non-breaking behavior changes in article extraction (`/summarize`, `fetch_url_content`, #132):

- The quality gate's link-density penalty now saturates — it grows with the square root of
  the density excess above the allowance — instead of linearly. Reference-heavy pages
  (Wikipedia articles, documentation wikis) legitimately extract at 8–24 links per 1000
  characters, which the linear penalty rejected on every backend, so they always fell to
  rescue mode; the probed long Wikipedia articles now pass the gate directly. At the top of
  that density range only long extractions clear the gate (roughly 36k+ characters with a
  title at 24 links/k); shorter reference-dense pages can still land in rescue mode, where
  the best non-empty result is still delivered. Boilerplate-dominated junk (nav menus, link
  farms) is still rejected — by narrower margins than before, since the saturating penalty
  is strictly weaker at every density: text-heavy boilerplate at moderate densities (up to
  roughly 12–22 links/k depending on length) that the linear penalty rejected now passes
  the gate outright, while high-density link farms (≳30 links/k with little prose) still
  fail.
- The configured backend order is re-ranked per page by built-in evaluators before
  extraction: MediaWiki pages (body markers or a wiki host) favor `readability` over
  `domdistiller`; fetched bodies of 128 KiB or more favor `domdistiller` over `trafilatura`
  (long news pages used to lock in a first-backend truncation that passed the gate).
  `CONTENT__EXTRACT__BACKENDS` keeps its meaning — the registered set, the base order, and
  the tiebreak — and no operator action is needed.
- Rescue-mode tie-break: when nothing passes the quality gate, the delivered result is still
  the best-scoring non-empty extraction, but exact score ties between backends are now
  resolved in the per-page voted order rather than the configured order.

## 0.25.0 (2026-11-11) — changes since 0.24.0 (breaking)

### GoOse extraction backend removed (breaking)

The `goose` extraction backend is gone, replaced by a new `domdistiller` backend
([go-domdistiller](https://github.com/markusmobius/go-domdistiller), a Go port of Chromium's
DOM Distiller).

- **Breaking**: `CONTENT__EXTRACT__BACKENDS` no longer accepts `goose`; a configured list
  containing it fails validation at startup (the error lists the valid names). Drop `goose`
  from the value — the default chain is now `readability,trafilatura,domdistiller`.
- Why: the #127 extraction benchmark shows GoOse pathological on large pages (~1.7 s CPU and
  ~9.6 GB of allocations on a 142 KB fixture) and systematically leaking navigation/comment
  boilerplate into its output, while domdistiller posts the best mean F1 on non-Wikipedia
  pages. Pagination following is disabled in the backend — the pipeline never follows
  next-page links.
- Pages where every configured backend abstains (no extractable text — near-empty stubs,
  some media-only shells) now end in an extraction error, where GoOse previously delivered a
  title-only remnant through the rescue path.

## 0.24.0 (2026-11-11) — changes since 0.23.1 (breaking)

### Auto-summarizer mode

New optional per-chat mode: the bot watches ordinary chat messages for links to long-form content and posts an
automatic summary as a reply to the linking message.

- The database gains `chat_settings.auto_summary_enabled` (nullable) and
  `global_settings.default_auto_summary_enabled` columns (schema version 7). The migration applies automatically at
  startup; no operator action is needed.
- The feature is **off everywhere** after upgrade — enabling it is an explicit per-chat (or global-default) decision
  made in the admin UI.
- New operational variables `BOT__AUTO_SUMMARY__MIN_TEXT_LENGTH` (default `2000`),
  `BOT__AUTO_SUMMARY__URL_COOLDOWN` (default `12h`), and `BOT__AUTO_SUMMARY__MAX_LINKS_PER_MESSAGE` (default `3`).
- In groups, Telegram's BotFather privacy mode must be disabled (or the bot promoted to admin) — otherwise the bot
  never receives ordinary group messages and the mode silently no-ops. See the README "Auto-summarizer" section.

### Web content fetching is SSRF-protected by default (breaking)

Article fetching (`/summarize` and the `fetch_url_content` tool) now goes through one shared,
SSRF-hardened fetcher instead of each extraction backend performing its own HTTP requests.

- **Breaking**: URLs pointing into internal networks are refused by default — loopback, private
  ranges (RFC 1918 and IPv6 equivalents), link-local (including cloud metadata endpoints
  `169.254.169.254`), CGNAT (`100.64.0.0/10`), multicast, and unspecified addresses are blocked.
  Installs that previously let the bot read articles from LAN/internal URLs must rework that
  flow; there is no allow-list. Deny lists (`CONTENT__FETCH__DENY_DOMAINS`,
  `CONTENT__FETCH__DENY_CIDRS`) only extend the built-in policy.
- DNS is validated before connecting: every address a host resolves to must pass the policy, so
  DNS rebinding to a private address fails closed.
- Redirects are re-validated per hop and capped at `CONTENT__FETCH__MAX_REDIRECTS` (default `5`);
  exceeding the budget fails the fetch instead of returning the last response.
- Response bodies are capped at `CONTENT__FETCH__MAX_BODY_BYTES` (default 2 MiB) and decoded to
  UTF-8 before extraction (windows-1251/1252, KOI8-R and other legacy encodings now extract
  correctly). Non-HTML responses are rejected early.
- New fetch-related variables: `CONTENT__FETCH__TIMEOUT` (default `10s`, previously a hardcoded
  constant), `CONTENT__FETCH__MAX_BODY_BYTES`, `CONTENT__FETCH__MAX_REDIRECTS`,
  `CONTENT__FETCH__USER_AGENT` (previously unset: Go's default for readability, a desktop Safari
  string for GoOse).
- Proxies are honored via the standard `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` environment
  variables. Note the documented tradeoff: when a proxy is used, destination IP/CIDR validation
  is delegated to the proxy (scheme and deny-domain checks still apply locally); direct
  connections (including `NO_PROXY` matches) always get the full dial-level IP policy. Closing
  this gap is tracked in the issue tracker.

### Extraction pipeline, quality gating, and Markdown output (breaking)

Extraction is now a configurable pipeline (`CONTENT__EXTRACT__BACKENDS`, default
`readability,trafilatura,goose`) run over one fetched page per request, replacing the fixed
"readability primary, GoOse fallback" pair.

- **Breaking**: `/summarize` and `fetch_url_content` now feed the model Markdown rendered from
  the winning extraction (headings, lists, links, tables, code preserved; relative links
  resolved against the final URL) instead of plain text. Summaries of structured articles
  change in character; the plain text rendering is kept as fallback and remains available on
  the result.
- A lightweight quality gate replaces "any non-empty text is success": results dominated by
  navigation/link boilerplate fall through to the next backend; the best-scoring non-empty
  result is still delivered as a rescue when nothing passes, and legitimately short articles
  are never rejected on length alone.
- The readability backend now runs on
  [go-readabilityV2](https://github.com/markusmobius/go-readabilityV2) (the same
  mozilla/readability lineage as the previous Readeck package, tracked against its v2.1.2).
- The new `trafilatura` backend ([go-trafilatura](https://github.com/markusmobius/go-trafilatura))
  adds the strongest extractor in upstream quality benchmarks with its internal fallback
  engines disabled (the pipeline itself is the fallback chain).
- `fetch_url_content` keeps its `text` field name and 2600-rune truncation; the content it
  carries is Markdown when available.
- The GoOse backend itself remains in 0.24.0; its removal (replaced by the new
  `domdistiller` backend) ships in 0.25.0 — see below. The extraction benchmark that drove
  that decision is tracked in issue #127.

## 0.23.0 (2026-11-11) — changes since 0.22.1 (breaking)

All sections below, up to and including the Koanf migration, ship in 0.23.0 and break compatibility
in two places: every environment variable is renamed to the `__`-delimited Koanf scheme, and chat
access becomes an explicit policy. Rename your env vars (see the Koanf section) and review chat
access (see the access policy section) before pulling the new image.
0.23.1 (2026-11-11) re-releases the same application code with only the release pipeline fixed;
there is nothing additional to do when upgrading from 0.23.0.

## Explicit access policy (breaking)

Access control is now an explicit policy instead of the old implicit rule "an empty whitelist means allow everyone".

- The database gains an `access_policy` column (`allowlist` or `allow_all`), migrated to **`allowlist`** for every existing install.
- **Breaking for installs with an empty whitelist**: previously the bot answered every chat; now all ordinary chats are denied until you either add them to the allowlist or explicitly switch the policy to `allow_all`. Installs with a populated whitelist behave exactly as before — entries are preserved and still allowed.
- Admin DMs (`BOT__ADMIN_IDS`) are always exempt, and `BOT__DM_ADMINS_ONLY` keeps working unchanged. Switching the policy never modifies allowlist membership, so you can switch back and forth without data loss.
- **Scheduled reminders are affected**: reminders for chats denied by the new policy stop delivering (their occurrences are skipped and schedules advance); reminders to admin DMs keep working.
- Remediation after upgrade: open `/admin` → *Access* in an admin DM, add the chats you serve, or confirm the switch to `allow_all` there.

## Interactive admin UI replaces the admin commands

The 16 IRC-style admin DM commands are removed. Administration now happens in a Telegram-native interactive panel:
send `/admin` in a private chat with the bot (users listed in `BOT__ADMIN_IDS`).

- Typing a removed command (e.g. `/config_show`) is silently ignored — it never reaches the model.
- Unknown `/commands` are dropped in all chats (debug log only); they never reach the model either.
- Expired, cancelled, or restart-orphaned replies to admin input prompts get a "no longer active" notice instead of
  reaching the model; replies that cannot be classified in admin DMs are dropped the same way (fail closed).
- `/stats` and `/reset` keep working with the same authorization (groups included) but are no longer recorded into
  conversation history or ordinary request statistics. They are advertised only in admin DM command menus; `/admin`
  never appears in group menus.
- Chosen defaults: session lifetime 30 minutes (sliding), pending inputs 10 minutes, 3 attempts on invalid input,
  catalog page size 8, prompt uploads capped at 32 KiB (enforced on actual bytes), prompt view sent as a document
  above ~3500 characters, `/cancel` with nothing pending reopens the dashboard, unreachable added chats are labeled
  "added manually (unverified)", admin screens render as plain text.

Capability map:

| Old command | New home |
|------------|----------|
| `/admin_help` | `/admin` dashboard |
| `/chat_list` | Dashboard → Chats |
| `/config_fields_global`, `/config_fields_chat` | Settings screens list every field inline |
| `/config_show [chat_id]` | Global/chat settings screens and the chat card |
| `/config_set_global <field> <value>` | Global settings → field editor (buttons or reply) |
| `/config_set_chat <chat_id> <field> <value>` | Chat card → Edit settings, or the on-card interactivity buttons |
| `/config_clear_chat <chat_id> <field\|all>` | Field editor `Inherit`, or the chat card's confirmed `Reset overrides` |
| `/prompt_features` | Dashboard → Prompts |
| `/prompt_show <feature> [chat_id]` | Prompt detail → View |
| `/prompt_set_global <feature> <template>` | Prompt detail → Replace via text |
| `/prompt_set_chat <chat_id> <feature> <template>` | Prompt detail (chat scope) → Replace via text / Upload |
| `/prompt_clear_chat <chat_id> <feature>` | Prompt detail (chat scope) → Reset to global |
| `/whitelist_add <chat_id>` | Chat card → Allowlist add; Access → Add chat (native picker or numeric ID) |
| `/whitelist_remove <chat_id>` | Access screen or chat card → remove (confirmed) |
| `/whitelist_list` | Access screen |

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

Requirements: `telego` upgraded v1.8.0 → v1.12.1; Go ≥ 1.27.3 is now needed to build.

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
  Tools are considered side-effecting unless `readOnlyHint=true` or the operator overrides the flag;
  `destructiveHint=false` does not imply that a tool is read-only.
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
