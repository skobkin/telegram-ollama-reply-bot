# Sticker catalog and model-driven sticker sending (PR5)

Status: implemented in PR5. Section 9 records the decisions taken during implementation; where it is more specific than the sections above, it governs.

## 1. Goals and scope

Give the Telegram bot a curated, persistent catalog of regular Telegram stickers so the LLM can find and send appropriate stickers by **semantic IDs** rather than guessing Telegram `file_id` values. Integrate with the existing tool framework, administrative Telegram UI, execution events, statistics and SQLite storage.

Support regular static stickers, and regular animated/video stickers when the existing Telegram SDK can handle them with little extra complexity. Mask sets and custom-emoji sets are out of scope. Do not generate stickers, inspect images with an LLM, or collect a catalog automatically from chat traffic.

All catalog management is administrator-only. Models must never be allowed to import, edit, or delete catalog entries.

## 2. Identity and persistence

- Persist curated sticker sets and sticker entries in SQLite using a versioned migration. The catalog survives bot restarts. The existing volatile conversation-history subsystem remains unrelated to this feature.
- Load an immutable, bounded search/send index into RAM at startup. After a successful catalog transaction, publish a replacement index atomically; readers must not observe partial imports.
- Identify a sticker within a set by `(set_name, file_unique_id)`. This identifies the media across re-fetches and is not a send token. Store the current Telegram `file_id` separately for `sendSticker`.
- Each **enabled** sticker has a globally unique, stable, human-readable semantic `id`, e.g. `kitsu_happy`. Persist English `description`, `tags`, the Telegram emoji where available, format, set metadata, last sync time and enabled/availability state. Other-language tags are optional.
- Reject duplicate semantic IDs and ambiguous identity mappings. Re-importing unchanged metadata is idempotent. Do not silently replace a known sticker's semantic ID with another ID merely because its position in a set changed.
- Removed/unavailable stickers must not be exposed for model selection or sending. Prefer retaining their annotations for possible restoration; avoid destructive deletion during ordinary synchronization.
- Keep reasonable caps on YAML bytes, sets, entries, tag counts, field lengths and in-memory index size. Final values should be selected and tested in the implementation plan, based on the current codebase and Telegram limits.

## 3. Two-stage administrator workflow: export then import YAML

### Stage A — export template

- From a private, access-checked Telegram admin workflow, the administrator supplies either a sticker-set short name or a `t.me/addstickers/<name>` URL.
- Fetch the current set using `getStickerSet`; reject unsupported set types, malformed names and fetch errors with actionable feedback.
- Return a downloadable UTF-8 YAML document containing the set identity, Telegram-provided sticker identities and emojis, and editable `id`, `description`, `tags` fields. The bot supplies no invented descriptions or semantic IDs. Include a human-friendly current position/index for matching entries to the visible Telegram set, but never use that position as persistent identity.
- **Critical: a repeat export for a set that has already been imported must merge current Telegram sticker data with saved catalog annotations. Every previously filled semantic ID, description and tag list must appear prefilled for the same `(set_name, file_unique_id)`, irrespective of sticker order. Newly added stickers must have empty editable fields.**
- A repeat export must explicitly disclose currently missing/replaced stickers rather than silently losing their annotations. Missing entries may appear in a separate clearly marked section or a separate summary; they must not be confused with active Telegram entries.
- Export must be read-only with respect to SQLite. It must not activate new stickers, delete old stickers, or alter saved annotations.
- The output must be deterministic for the same Telegram response and saved state; use stable ordering and a schema version.

Illustrative YAML (field names may be refined during planning):

```yaml
schema_version: 1
set: KitsuStickers
stickers:
  - file_unique_id: "telegram-provided-unique-id"
    position: 1 # display aid, not identity
    emoji: "🦊"
    id: kitsu_happy
    description: Happy kitsune waving hello
    tags: [happy, greeting, hello]
  - file_unique_id: "new-telegram-unique-id"
    position: 2
    emoji: "😴"
    id: ""
    description: ""
    tags: []
```

### Stage B — import edited YAML

- Accept the edited YAML through the correlated, administrator-only document upload workflow. Verify its schema version, size, set identity, uniqueness, types, bounded fields and that its sticker identities belong to the current Telegram set. Never trust arbitrary `file_id` or chat IDs from the uploaded YAML.
- Fetch authoritative current Telegram metadata before committing. A stale template (set changed after export) must not silently remove or remap entries; report a precise diff/conflict and guide the administrator to re-export if necessary.
- Preserve existing annotations that are absent/blank unintentionally; never silently wipe a previously filled description, tag list or semantic ID on routine re-import. If deliberate deletion/renaming is supported, require an explicit operation with clear validation and conflict reporting.
- **Partially filled imports are allowed:** unannotated new entries remain inactive/unselectable, while existing curated entries remain usable. A set upgrade does not force re-annotating every sticker in the set. Decide exact activation criteria (`id` and useful description/tags) and document them.
- Validate the full prospective catalog before one atomic DB transaction. Return concise counts of added, updated, unchanged, incomplete, removed/unavailable and rejected entries. Do not publish a new RAM index until the transaction commits.
- Provide admin operations to list cataloged sets, re-export a set, disable/remove a set, and inspect the current catalog status. Prefer the existing admin UI over creating a separate interface.
- The template is configuration data, not an instruction stream for the model. Render admin-supplied text safely in Telegram and enforce model-facing output budgets.

## 4. LLM tools

### `list_stickers`

- Read-only, discretionary tool, scoped to the active global catalog. Accept an optional bounded natural-language query and/or tags and a bounded `limit`.
- Use a local, deterministic text/tag search against semantic ID, English description, tags and emoji. No external search or embedding service. Keep search results ordered stably, with an explicit ranking policy; define predictable empty-query behavior.
- Return the stable semantic `id`, description, relevant emoji/tags, and (optionally) set title; do not return `file_id`, `file_unique_id` or uncurated records. Apply the existing result budget and report omission/truncation where relevant.

### `send_sticker`

- Side-effecting, discretionary tool. Accept **only** a catalog semantic sticker `id`; resolve it against the current, enabled RAM catalog and send the corresponding current `file_id` with the existing Telegram action sender.
- Target the current conversation's chat and topic only. Do not accept arbitrary chat IDs, URLs, file IDs, or model-selected Telegram identities.
- Publish the existing `MessageSent` event for a confirmed send and produce normal `ToolCallRecord`, model-facing structured result and automatic tool-usage statistics. Follow `send_poll`/`generate_chart` history conventions: no retained-history entry in PR5.
- Distinguish a definite pre-dispatch rejection/cancellation from ambiguous post-dispatch transport failure. Do not automatically retry an ambiguous send.
- Confirmed sends return the Telegram message ID; avoid reporting success for a nil or otherwise invalid successful-send response.
- At most **one admitted sticker-send dispatch attempt per model turn**, including attempts with unknown outcomes, enforced by runtime, not merely prompt instructions. A failed or ambiguous send cannot be worked around by calling `send_sticker` again in the same turn. A sticker can be a complete reply (the model may use `say_nothing` after sending).

## 5. Anti-spam and group behavior

- Invocation policy: `discretionary` — the model may send an appropriate sticker without an explicit request.
- Hard limit: one dispatched send attempt per turn (not merely one confirmed success), including concurrent/multi-tool-call turns; enforce the limit independently of model compliance.
- A group-chat cooldown is **not yet finalized**. Proposed starting point: roughly **30 seconds per chat** for spontaneous sticker sends, instead of two minutes. Before implementing a hard gate, assess how to distinguish explicit requests reliably and whether an absolute cooldown would improperly reject them. Do not rely solely on a model-provided `explicit=true` flag. Prefer simple, predictable, race-safe admission; avoid a new general rate-limit framework.
- Preserve existing proactive turn and reaction-only participation rules. A sticker must not bypass an interaction policy that disallows replies.
- Make any declined send visible to the model via a structured, non-retryable reason; do not silently pretend it was sent.

## 6. Operational and security properties

- Fail cleanly if catalog DB restore fails; do not advertise unusable stickers or expose stale/new unvalidated entries. Choose and document a safe startup/recovery behavior.
- Keep admin-only import/export actions distinct from general user messages, preventing unauthorized ingestion of YAML or arbitrary files.
- Bound Telegram API calls, document download sizes and concurrency. Perform import validation before mutation. Make repeated requests and partial failures safe.
- Do not log arbitrary sticker descriptions, secrets or uploaded YAML bodies by default. Use metadata-only diagnostic logs.
- Reuse existing project abstractions, metrics, integration patterns and tests. Keep the persistent catalog separate from the future external conversation-history daemon.

## 7. Testing and documentation

- YAML export/import round trip; **repeat export retains all previously completed annotations and leaves only newly added stickers blank**; reordering; removed/replaced media; stale templates; duplicate and malformed IDs; incomplete entries; idempotent import.
- Atomic transaction/index swap; rollback on validation/write failure; bounded inputs; admin authorization and correlated uploads.
- Search correctness, stable ordering, multilingual tag matching, result budget, disabled and missing items.
- Side-effect outcome, per-turn send cap, chat/topic routing, successful `MessageSent`, ambiguous transport failures and cancellation; regression tests for concurrent turns.
- Static/animated/video formats supported by current SDK; unsupported set types rejected.
- Update `AGENTS.md`, relevant specifications, README and migrations only as appropriate. Avoid unrelated refactors.

## 8. Ordering and deferred work

1. Implement this curated sticker catalog and its tools (PR5).
2. Separately implement uniform retained-history records for bot-generated non-text messages (polls, dice, charts, stickers, etc.), including reply references.
3. Only then extract the completed conversation-history contract into local and ConnectRPC-backed implementations. The daemon should be restartable less frequently than the bot; avoid leaking changing bot-specific tool schemas into the daemon contract.

Out of scope in PR5: generic non-text history, history daemon, vision tagging, sticker generation, custom emojis/masks, vector search, new web UI, model-authored catalog administration, and unrelated tool framework changes.

## 9. Implementation resolutions (PR5)

### Storage and startup

- Schema version 14 adds `sticker_sets` and `sticker_entries`. A set row carries a `revision` (created at 1, incremented by every committed write) that guards each write against a stale read; a mismatch fails the operation without changing anything and asks the administrator to retry.
- An unassigned semantic ID is stored as SQL `NULL`, never as an empty string. The unique index is partial (`WHERE semantic_id IS NOT NULL`) and a `CHECK` rejects empty strings, so any number of unannotated stickers coexist. Uniqueness is global across **all** stored annotated rows, including retained unavailable ones, so restoring a sticker can never collide with an ID in use.
- Startup restore is all or nothing. The complete catalog (disabled, incomplete and unavailable rows included) is read and held in memory, so later imports always merge against everything stored. Every row is validated with the same rules as an import; any invalid row or database error aborts boot, with an error that names set and sticker identity and the violated rule, never annotation text. No partial or unvalidated catalog is ever advertised. Recovery: fix the row, or delete it (or the whole set) in SQLite if the data is to be discarded.
- Every catalog change builds the full prospective state, validates it and builds its index **before** the transaction, commits in one transaction, and only then publishes state and index. A failed validation performs no database call; a failed commit leaves the old snapshot serving.

### Formats and supported sets

- The Bot API reports the format per sticker (`is_animated`, `is_video`), not per set, so a set may mix formats and each entry stores `static`, `animated` or `video`. `sendSticker` by `file_id` handles all three. Sets and stickers whose type is not `regular` (masks, custom emoji) are rejected.
- There is no background synchronisation. `file_id` and availability are refreshed only by an import; a sticker removed from Telegram afterwards stays selectable until the next import and its send then fails with a definite Telegram error.

### YAML template

- Schema version 1: `schema_version`, `set`, `title` (informational), `stickers[]` and `missing[]`. A sticker entry is `file_unique_id` (identity), `position`, `emoji` and `format` (display aids, ignored on import), and the editable `id`, `description`, `tags`, `enabled` and `reset`. A `missing` entry is `file_unique_id`, its saved `id`, `description` and `tags` (informational) and `forget`.
- Export is read-only and deterministic. Saved annotations (including `enabled`) are matched by `(set, file_unique_id)` regardless of order and prefilled; new stickers are blank with `enabled: true`; stickers Telegram no longer reports are listed under `missing`. `file_id` never appears in a template.
- Import accepts at most 256 KiB, valid UTF-8, one document, no YAML anchors or aliases, no unknown fields and no duplicate keys.
- Merge rules: a blank `id`, `description` or `tags` keeps the saved value; an omitted `enabled` keeps the saved flag (only an explicit `false` disables); a non-blank value replaces the saved one. `reset: true` overrides every editable field of its entry: it clears id, description and tags and restores `enabled: true`, ignoring (and not validating) whatever else the entry holds, so flipping that single flag works directly on a populated repeat-export template. `forget: true` in `missing` deletes that retained row and frees its ID. Edits to other `missing` fields are ignored and counted.
- A stale template (a listed sticker that is no longer in the Telegram set, or `forget` on a sticker that is back) is rejected with the exact identities and the advice to re-export. Telegram stickers the file does not list are left untouched and reported.
- Import is all or nothing: any problem rejects the whole file with up to 20 itemised issues. "Rejected" in the summary therefore means the file was rejected.
- Inside the transaction SQLite checks the unique index per statement, so changed IDs are released first (forgotten rows deleted, surviving rows whose ID changes set to `NULL`), then every entry is upserted with its final values. Swaps, reuse of a reset ID and reuse of a forgotten ID therefore work while the final catalog is still validated as a whole.

### Activation and caps

- A sticker is active (searchable and sendable) when its set is enabled, it is available and enabled, and it has both a semantic ID and a description. Tags are optional. An entry with only an ID is incomplete: its ID is reserved but it cannot be selected.
- Caps: 4 MiB upload (the exporter's worst case for a fully annotated set is about 3 MB; the original 256 KiB could not hold a maximal export); 32 sets; 200 available and 200 retained missing stickers per set; 5000 entries in total; semantic ID `^[a-z][a-z0-9_]{2,47}$`; description at most 240 characters on a single line; at most 16 tags of at most 32 characters (any language, case-insensitive duplicates rejected); set title at most 128 characters.

### Search and `list_stickers`

- Ranking (deterministic, no external service): exact id 100; exact tag 10 per query token; emoji equal to a query field 8; id token 6; description word 3. With a query only matches score above zero; the tag filter is a hard AND; ties break by set name, position, id. An empty query lists in that stable order.
- The handler fits its result to the 2400-character budget itself, using the runtime serializer as the measure so JSON escaping is counted exactly. Each match is placed at the richest tier that fits: full (description capped at 160 characters, up to 8 tags, set title), reduced (description capped at 48) or id and emoji only. The semantic ID is never cut and at least the first match is always returned. `truncated`, `presentation_reduced` and `omitted` report what happened.

### `send_sticker`, per-turn cap and cooldown

- The runtime marks the turn's attempt immediately before the send, so a failed or ambiguous attempt also uses it up; a second call (same response or later) is refused with `sticker_limit_reached`, and the tool stops being advertised. Unknown or malformed IDs never consume the attempt but are bounded at three per turn.
- A nil Telegram response or a message without an ID is never a confirmation: it is reported as an ambiguous outcome. `MessageSent` is published only for a confirmed send. Every dispatch error is non-retryable within the turn.
- **Cooldown.** Proactive turns are serialized per chat but not otherwise rate-limited, so consecutive proactive replies could each attach a sticker; a cheap guard is worthwhile. A hard-coded 30-second per-chat window (`stickerProactiveCooldown`, no configuration) applies to proactive turns only. Whether a turn is proactive is derived from the trigger Telegram metadata gives the bot (mention, reply, private chat versus the participation engine), never from a model-supplied flag. An explicit request is never declined. Every admitted dispatch, explicit or proactive and including an ambiguous one, stamps the chat, so a proactive sticker cannot follow right behind an explicit one. Admission is an atomic check-and-reserve under one mutex and runs after every other pre-dispatch check, immediately before the send; declines, validation failures, cancellations and unknown IDs never stamp. A declined send returns `sticker_cooldown` (rate limited, not retryable). Proactive turns inside the window are not offered the sticker tools at all.
- Reaction-only turns allow only `set_reaction`, so stickers are unavailable there without any extra rule.

- **Stale templates.** Every export carries `catalog_version`, a fingerprint of the set's saved annotations (identity, id, description, tags and enabled flag of every annotated or disabled entry, retained missing ones included; Telegram-derived fields such as `file_id` and position do not count). An import that would change annotations (update, reset or forget) is refused as out of date when the fingerprint no longer matches the saved catalog, so an old export cannot overwrite a curator's later edits; the administrator exports again and redoes the edits. A file that changes nothing, such as re-importing an already applied file, is still accepted. The per-set SQLite revision guard additionally protects against concurrent writes.
- **Search language.** Descriptions are expected mostly in English; the `list_stickers` description tells the model to translate the user's intent into short English search terms, while tags may be in any language and match as written.
- **Stored tags** must be a JSON array of strings; `null`, other JSON types and null elements fail the startup restore like any other corrupt row.

### Admin workflow

- Dashboard → Stickers lists the cataloged sets with counts; a set card offers re-export, import, enable/disable and (confirmed) removal; "Export set…" asks for a set name or link and replies with the template as a document; "Import YAML" asks for the edited file as a correlated reply. The admin document download is bounded by the largest per-kind limit and each kind enforces its own limit on the bytes actually read; a document declared over the bound is refused without being fetched. Everything the admin sees is plain text.
- Operations are serialized (a second concurrent import or change gets a "busy" answer), Telegram fetches time out after 15 seconds and concurrent exports are bounded to two.

