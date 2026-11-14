# Model-authored reactions (`set_reaction`)

Companion to [`reaction_changed_event.md`](reaction_changed_event.md), which specified the
incoming-reaction pipeline this feature builds on, and to
[`live_preview_and_events.md`](live_preview_and_events.md), which established the typed
in-process event architecture (see also [`message_sent_event.md`](message_sent_event.md)).
This document specifies issue #135: letting the model react to its triggering Telegram message,
with or without a subsequent text reply. It is the single source of requirements for the
`set_reaction` tool.

## 1. Goals and behavioral contract

A model may call `set_reaction` to acknowledge the current user message with an ordinary Telegram emoji. The model can then finish **without** a text reply if the reaction fully conveys its response, **or** continue and write a normal answer. Reaction + text is explicitly valid. The tool is available in all otherwise-authorized conversational runs by default, with no additional global or per-chat configuration.

This is an ordinary discretionary, side-effecting tool. **It must not be terminal.** Do not add `say_nothing` or a special completion protocol. Reuse `ReplyWithTools()`'s existing successful-empty-completion path following successful side effects. Do not force a textual reply after a successful reaction.

Examples: a joke may get 🤣 alone; a thank-you may get 🙏 alone; a useful answer may get 👍 plus text. Genuine questions and actionable requests generally deserve substantive responses.

## 2. Tool API and precise target binding

Name: `set_reaction`.

Model-facing arguments:

```json
{"emoji":"👍"}
```

Schema: object, **required** `emoji: string`, exact `enum` of supported Unicode emoji, `additionalProperties: false`. **No `chat_id`, `message_id`, target selector, or hidden alternate target.**

Resolve the target exclusively from the current invocation's immutable request context:

- `chat_id = CallContext.Scope.ChatID`.
- `message_id = CallContext.Requester.MessageID`.
- `topic_id = CallContext.Scope.TopicID` (for consistency/validation only; Telegram `setMessageReaction` does not take a topic ID).
- Reject missing/nonpositive message ID, empty/invalid chat ID, mismatched request chat/topic metadata, and bot-authored requests before any Telegram call.

**Critical comment in implementation and tool description:** `Requester.MessageID` is the Telegram message that triggered **this run**. It is **not** “the latest message in the chat.” It does not change when other users send messages or multiple runs execute concurrently. Do not use the last message from history or a global “current message” variable.

Do **not** inject message IDs into model-visible history, prompts, or tool responses as an aid to targeting. Reactions to earlier messages or the bot's previous message are out of scope.

**Telegram media-group edge case:** the Bot API can apply a reaction to the first non-deleted message of an album rather than the specified member. For v1, do not falsely attribute such a reaction in history. Carry a minimal request-scoped indication from the original Telegram `Message.MediaGroupID` and reject media-group triggers if the effective target cannot be determined reliably; do not expose album metadata to the model. Tests must cover this safety choice. Avoid expanding historical message schema merely for this request flag.

## 3. Emoji policy: one canonical source, displayed to the model

Reuse **exactly** the current `trackedReactionEmojis` whitelist in `internal/telegram/bot/reactions.go` as the initial supported set. Extract it to a small dependency-neutral shared package (e.g. `internal/telegram/reactionemoji`) so inbound handling and `tooluse` refer to the **same** definition.

Initial list, preserving exact Telegram Unicode forms:

`👍 👎 ❤ 😂 🤣 😭 😴 💯 🏆 ⚡ 🙏 🤝 👏 👌 🔥 🎉 🤔 👀 😐 🤨 😎 🥰 🤩 😍 🥳 🫡 🤯 😱 💔 😈 🤡 🤓 🥴 🥱 🤮 💩`

In particular **`❤` is U+2764 without U+FE0F**; do not silently normalize or replace it with `❤️`.

- Use a canonical, deterministic ordered list with a membership check.
- Generate BOTH the complete, readable `Available emoji: ...` list in the model-visible tool `Description` and the JSON Schema `enum` from this source; never maintain a second hand-written list.
- Local validation must accept only exact members of this list. Empty or unsupported values produce a normal tool error **before** any Telegram request.
- Telegram's actual permitted emoji may be restricted per chat and message, regardless of local whitelist. Handle API rejection as a normal tool error; do not substitute another emoji or falsely report success.
- Only ordinary `ReactionTypeEmoji` is supported. No custom emoji, paid reactions, removal API, or animated `is_big` option in v1.
- The list and tool description should make this distinction clear: the listed emoji are the bot's supported candidates, not a guarantee Telegram accepts them in every chat.

**Implementation note (2026-11-15):** of the tracked emoji, `😂` and `🥳` are not in Telegram's `ReactionTypeEmoji` emoji list at all — they can appear in incoming updates never, and sending one is rejected outright, which would burn a run's only reaction attempt. The shared package therefore exposes the tracked list (the inbound filter, unchanged from the list above) and a derived reaction-candidate subset (the tool's description, enum, and validation). If Telegram extends its reaction list, removing an entry from the package's exclusion map promotes it back to a candidate. `🤣`, which Telegram does offer as a reaction, is tracked and advertised as the laughter candidate in place of `😂`.

## 4. Prompt guidance and empty final response

Add concise instruction to the **effective tool policy/description delivered to the model** (independent of operator-editable prompt templates):

> `set_reaction` reacts to the user message that triggered this run, never the most recent chat message. Choose an emoji only from the listed supported values. If a reaction fully communicates the response, you may end the turn without any additional text. You may also react and then provide a text reply when useful. Do not replace a substantive answer to a genuine question or request with a reaction alone. Do not repeat or change reactions unnecessarily.

The model should receive this instruction in the tool-enabled generation context even with custom chat templates. The tool description should remain compact beyond the explicit emoji list; do not repeat the entire list in multiple prompt sections unnecessarily.

No special end-of-turn sentinel, forced tool termination, or extra model-facing `say_nothing` tool. Verify the existing `allowEmptyReplyCompletion` behavior in `internal/tooluse/runtime.go`: only successful side effects permit a subsequent empty reply; failed attempts alone cannot suppress an answer. Side-effecting tool usage must preserve ordinary text+reaction flows and existing error behavior.

## 5. Telegram execution and rate-limit safety

Extend the existing `TelegramActionSender` with the Telego `SetMessageReaction` method, using `SetMessageReactionParams` with the resolved chat/message IDs and exactly one `ReactionTypeEmoji`. Keep `is_big=false`. Use the request `context.Context` for cancellation/timeouts.

- Treat **only confirmed Telegram success** as success. Do not write a reaction or publish a success event after transport, validation, permission, or API errors.
- Do not automatically choose a different emoji. Return a compact, safe, actionable tool error, without token-bearing URLs or secrets (follow existing `safeerr` conventions).
- Allow at most **one actual outgoing set-reaction API attempt per processing run** (invalid local arguments do not consume it), protecting against repeated model attempts. Rejected follow-up attempts return a tool error. A reaction does **not** prevent the model from calling unrelated tools or producing text.
- Coordinate reaction attempts per chat with a small, bounded cooldown and bounded per-target serialization so concurrent runs cannot race different bot reactions on the same Telegram message. Use an explicit **run-local attempt guard** (initialized in `ReplyWithTools()` and shared across that run's tool calls), rather than a long-lived map keyed by request IDs. Extend `CallContext` minimally to carry the effective `RunID` from `runEmitter.rc`, which may be generated when `ChatRequest.RunID` is empty; do not assume the logging `RequestID` is equivalent. Do not serialize all chats or LLM generations. Respect `retry_after` from HTTP 429 for subsequent reaction attempts; avoid unbounded waiting or infinite retries. Do not automatically retry an ambiguous transport failure, as the remote side may have applied the reaction.
- Keep this narrowly scoped to reaction sends. Reuse an existing suitable limiter if practical, but do not refactor the preview/final-reply delivery pipeline or invent a general message scheduler for this feature.
- Telegram may reject reactions on certain service/otherwise restricted messages; surface the error without fictitious local state.

Reference: <https://core.telegram.org/bots/api#setmessagereaction>. The API currently permits up to one bot reaction per message and may redirect media-group reactions to the first non-deleted item; allowed reactions are also constrained by chat configuration.

## 6. Authoritative local history and bot identity

A successful bot reaction must be recorded on the **existing triggering user message**, not as a fabricated new assistant message. Show it in later prompts with the existing reaction suffix, for example:

```text
Alice: That's a terrible pun.
[reactions: 🤣 MyBot]
```

Re-use `state.Reaction` including real bot `UserID`, display name/username, emoji and timestamp. Update its documentation to permit bot-authored reactions on user messages. Update `llmcontext` comments/tests to reflect the broader semantics; **do not** hide bot-authored reactions from the model.

The actual bot identity is obtained by the existing `Bot.Run()` `GetMe` call. Wire this verified identity into `tooluse.Runtime` **once before processing updates**, through a minimal startup hook or equivalent. Avoid making a new `GetMe` request per reaction, invented actor IDs, or cyclic package imports. A missing/uninitialized bot identity is a pre-send error.

Extend `state.ConversationStore` with a separate, explicit atomic operation for bot-authored reactions (e.g. `SetBotReaction(...)`). The current `SetMessageReactions(...)` must retain its incoming-reaction-only contract: it applies to `IsMe` bot messages, rejects stale Telegram update IDs, and remains unchanged for #100.

The new operation must:

- Locate the target by actual `(chat ID, message ID)`, additionally match the known topic/request scope where appropriate, and require a **user** target (`!IsMe`).
- Replace only the bot's own reaction for that target (Telegram allows one bot reaction); preserve other users' data and preserve message content/identity.
- Be atomic and safe under concurrent reactions and history trimming/reset/eviction. The sending side must serialize per target to keep local order consistent with successful Telegram writes. No fake Telegram `UpdateID` values for bot-authored actions.
- Apply the same byte-budget accounting, timestamp, revision bump, in-flight summary commit invalidation, cached-summary clearing and eviction rules as real reaction mutations.
- Avoid recording duplicates/repeated identical reactions as distinct entries.
- Return whether the target was actually found/updated, **not** claim local persistence if the message has expired or been evicted. A confirmed Telegram send still counts as a successful tool action in that case; emit a bounded diagnostic, never resurrect the missing message or create fake text.

If an identical reaction is already recorded and this request still needs a Telegram call, do not create duplicate local records; keep summary behavior sensible. Do not conflate the incoming Telegram `ReactionChanged` event with an outgoing action.

## 7. Event integration

Add a distinct typed event **`execution.BotReactionSet`** to `internal/execution/events.go`, e.g.:

```go
// Published after Telegram confirms an outgoing bot reaction and after
// the synchronous best-effort history mutation attempt.
type BotReactionSet struct {
    RunContext                   // includes RunID, ChatID, TopicID, originating MessageID
    At              time.Time
    TargetMessageID int
    BotUserID       int64
    Emoji           string
}
```

- Register it in the sealed `Event` family and `RunContextOf`. Populate event `RunContext` using the **effective** current run ID, actual scope and triggering message ID; pass that ID through the tool invocation context explicitly, since the current `CallContext` does not expose it.
- Publish only after Telegram success and the synchronous history-update attempt, even if target storage has already been evicted. The event means **Telegram confirmed the reaction**, not that history necessarily retained it.
- Never manufacture an incoming `ReactionChanged` or Telegram `UpdateID` for this action.
- In-process delivery is observational/best-effort. Neither Telegram outcome nor stored state depends on dispatcher availability/ordering, and the event is not used to drive the history update.
- The preview subscriber must ignore `BotReactionSet` as a state transition even when its `RunID` matches an active run. Existing generic `ToolStarted`, `ToolCompleted` and `RunCompleted` events still describe the tool/run lifecycle.
- No sensitive message text, user data or raw API errors in `BotReactionSet`.

## 8. UI and conversation lifecycle

- No new admin configuration, command, switch, or migration.
- Successful reaction with no following text: `ReplyWithTools()` returns `Reply == ""` successfully; `processMention()` uses existing `CompleteNoText(toolCalls)` to clean up group preview or finalize private draft. Do not send a blank Telegram message or invent a bot history message.
- Successful reaction followed by text: existing rich/fallback delivery, tool history, final message ID storage, and `FinalDelivered()` work unchanged.
- Failed reaction: no local state or `BotReactionSet`; ordinary tool failure lets the model recover or answer in text. Do not treat failure as successful side effect that permits silent completion.
- No change to inbound user reactions, summary formatting, `AllowedUpdates`, or the recently merged `ReactionChanged` pipeline.

## 9. Test plan and acceptance criteria

**Schema and prompt**

1. Tool advertised by default; its only argument is `emoji`; rejected extra fields/invalid emoji do not reach Telegram.
2. Tool description and JSON Schema `enum` enumerate **exactly** the shared whitelist; test all values and `❤` vs `❤️`.
3. Effective tool policy explicitly permits reaction-only and reaction+text, with the immutable current-run target rule; no fake message IDs added to prompts.

**Execution**

4. Two simultaneous runs in the same chat target their respective `Requester.MessageID`, never whichever Telegram message arrived last; the run-local attempt guard and propagated effective `RunID` must not leak across runs. Also verify separation by chat/topic.
5. Missing/invalid or bot-authored target and media-group targets fail safely before Telegram; one outgoing request carries the correct chat/message/emoji.
6. Unsupported/chat-restricted reactions and Telegram errors produce tool errors without state/event emission, and no automatic emoji fallback.
7. Cap repeated API attempts in one run, and cover concurrent same-target sends, 429 `retry_after`, timeouts and cancellation without cross-chat serialization or unbounded waits.
8. Fake LLM: reaction only → subsequent successful empty completion → no text message; reaction + text → both appear; failed reaction cannot authorize empty success; other side-effecting tools retain their existing semantics.

**State/events/preview**

9. Real bot identity is used; reaction persists on existing user message and appears once in subsequent model context. No invented bot message.
10. Incoming `SetMessageReactions` behavior remains unchanged; user reactions on bot replies and bot reactions on user messages are distinct and preserved.
11. Summarized history is invalidated appropriately; stale in-flight summary commits fail; memory budgets, reset/trim, concurrent mutation and missing-target handling are tested.
12. Exactly one `BotReactionSet` per confirmed outgoing action with proper run correlation, no fake incoming event, no duplicate on replay; event delivery failure must not affect state.
13. Preview subscriber ignores the new event while ordinary tool lifecycle remains visible; successful no-text completion cleans up the group preview and handles private draft according to existing design.
14. Existing event dispatcher, bot, tooluse, `llmcontext`, memory store and reaction tests all remain green, including race detection.

## 10. Out of scope

- Optional `message_id`, reacting to historical messages or the bot's last reply.
- Adding message identifiers to LLM-visible history.
- New configuration toggles, settings or per-chat emoji sets.
- `say_nothing`, terminal tool semantics, forcing silence after a reaction.
- Receiving the bot's own reaction as an incoming Telegram update.
- Custom/premium/paid emojis, reaction removal or animation options.
- Durable history/events, a new queue, UI redesign, LLM response streaming, or generation cancellation.

## 11. Delivery

Create the specification in-repository at `docs/specs/model_authored_reactions.md` (this document may be adapted to repo style), then implement in a dedicated feature branch from current `main`. Keep the historical scope of the previous reaction and live-preview specifications intact. Include unit/integration/race tests and updates to relevant comments. No `UPGRADE.md` change unless implementation unexpectedly requires a breaking behavior/configuration change.

Before completion, follow `AGENTS.md`: format Go, run `golangci-lint run ./cmd/bot/... ./internal/...`, `go test ./...`, build into `/tmp`, and `go test -race` for affected packages.
