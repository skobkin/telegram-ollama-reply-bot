# Model-authored reactions (`set_reaction`)

Companion to [`reaction_changed_event.md`](reaction_changed_event.md), which specified the
incoming-reaction pipeline this feature builds on, and to
[`live_preview_and_events.md`](live_preview_and_events.md), which established the typed
in-process event architecture (see also [`message_sent_event.md`](message_sent_event.md)).
This document specifies issue #135: letting the model react to its triggering Telegram message,
with or without a subsequent text reply. It is the single source of requirements for the
`set_reaction` tool.

## 1. Goals and behavioral contract

A model may call `set_reaction` to acknowledge the current user message with an ordinary Telegram emoji. The model can then finish **without** a text reply if the reaction fully conveys its response, **or** continue and write a normal answer. Reaction + text is explicitly valid. The tool is available in otherwise-authorized conversational runs when target eligibility, chat availability and remaining run attempts permit it, with no additional global or per-chat configuration.

This is an ordinary discretionary, side-effecting tool. **It must not be terminal.** Reuse `ReplyWithTools()`'s existing successful-empty-completion path following successful side effects. The separate [terminal `say_nothing` tool (#159)](say_nothing.md) also lets the model end the turn intentionally after a reaction. Do not force a textual reply after a successful reaction.

Examples: a joke may get 🤣 alone; a thank-you may get 🙏 alone; a useful answer may get 👍 plus text. Do not add guidance that discourages reaction-only responses or requires a substantive text answer.

## 2. Tool API and precise target binding

Name: `set_reaction`.

Model-facing arguments:

```json
{"emoji":"👍"}
```

Schema: object, **required** `emoji: string`, dynamic `enum` of available supported Unicode emoji, `additionalProperties: false`. Trim whitespace and normalize equivalent Unicode presentation selectors, without changing semantic emoji identity. **No `chat_id`, `message_id`, target selector, or hidden alternate target.**

Resolve the target exclusively from the current invocation's immutable request context:

- `chat_id = CallContext.Scope.ChatID`.
- `message_id = CallContext.Requester.MessageID`.
- `topic_id = CallContext.Scope.TopicID` (for consistency/validation only; Telegram `setMessageReaction` does not take a topic ID).
- Reject missing/nonpositive message ID, empty/invalid chat ID, mismatched request chat/topic metadata, and bot-authored requests before any Telegram call.

**Critical implementation invariant:** `Requester.MessageID` is the Telegram message that triggered **this run**. It is **not** “the latest message in the chat.” It does not change when other users send messages or multiple runs execute concurrently. Do not use the last message from history or a global “current message” variable.

Do **not** inject message IDs into model-visible history, prompts, or tool responses as an aid to targeting. Reactions to earlier messages or the bot's previous message are out of scope.

**Telegram media-group edge case:** the Bot API can apply a reaction to the first non-deleted message of an album rather than the specified member. For v1, do not falsely attribute such a reaction in history. Carry a minimal request-scoped indication from the original Telegram `Message.MediaGroupID` and reject media-group triggers if the effective target cannot be determined reliably; do not expose album metadata to the model. Tests must cover this safety choice. Avoid expanding historical message schema merely for this request flag.

## 3. Emoji policy and cached chat availability (#177)

The canonical outgoing subset comes from `reactionemoji.ReactionCandidates`; the
inbound tracked whitelist remains unchanged. Obtain chat availability with getChat:
omitted available_reactions allows the outgoing whitelist; an explicit list is
intersected with ordinary supported emoji; an empty intersection disables the tool.
Paid and custom emoji are excluded. The model's enum is generated dynamically in
canonical order, without repeating the emoji list in the description or policy.

Cache availability in RAM by chat ID for 30 minutes, with at most 4096 entries
including in-flight refreshes. Deduplicate concurrent refreshes and never evict an
active one. Lookups respect the caller context and a five-second timeout. Failed
lookups use the last successful set (including an empty set), or the unverified
outgoing whitelist if none exists. Retry lookups after one minute, respecting a
longer Telegram retry_after. Failure does not mean disabled reactions.

An empty supported set from a successful lookup omits set_reaction and warns with
chat ID/title once per unchanged empty state. Known unsuitable targets, including
albums, omit the tool without a lookup. Argument validation uses local candidates
and never calls getChat. Invalidation supersedes in-flight cache responses; cache
eviction must not broaden an already advertised run's candidate set.

## 4. Prompt guidance and empty final response

Add concise instruction to the **effective tool policy/description delivered to the model** (independent of operator-editable prompt templates):

> React to the triggering message using one available emoji. You may reply afterwards or call say_nothing.

Retain immutable triggering-message binding in code and argument constraints in the native schema. For restricted proactive React turns, omit the continuation sentence: those runs neither expose `say_nothing` nor publish text. Adapt only this reaction description for the run; no general dynamic tool framework is required.

The model should receive this instruction in the tool-enabled generation context even with custom chat templates. The tool description should remain compact beyond the explicit emoji list; do not repeat the entire list in multiple prompt sections unnecessarily.

Issue #135 did not introduce a special completion protocol; #159 independently adds `say_nothing`. Preserve the existing `allowEmptyReplyCompletion` behavior in `internal/tooluse/runtime.go`: only successful side effects permit a subsequent empty reply; failed attempts alone cannot suppress an answer. Side-effecting tool usage must preserve ordinary text+reaction flows and existing error behavior.

Restricted React completion remains successful if cancellation arrives immediately after a confirmed reaction. Stop without executing remaining pending calls or requesting another generation, retaining the successful call record, reaction history, and usage accounting. An elapsed processing deadline still fails the turn, preserving confirmed effects and returned usage; this cancellation rule does not relax the live-context acceptance boundary for `say_nothing`.

## 5. Telegram execution and rate-limit safety

Extend the existing `TelegramActionSender` with the Telego `SetMessageReaction` method, using `SetMessageReactionParams` with the resolved chat/message IDs and exactly one `ReactionTypeEmoji`. Keep `is_big=false`. Use the request `context.Context` for cancellation/timeouts.

- Treat **only confirmed Telegram success** as success. Do not write a reaction or publish a success event after transport, validation, permission, or API errors.
- Do not automatically choose a different emoji. Return a compact, safe, actionable tool error, without token-bearing URLs or secrets (follow existing `safeerr` conventions).
- Allow at most **three outgoing set-reaction API attempts per turn** and **one confirmed success**. Invalid local arguments and cancelled/refused waits consume no attempt. Recognized definitive emoji rejections permit corrected model-driven retries; exclude the rejected emoji for this turn and invalidate availability. Success, exhaustion, rate limiting, permanent target rejection or ambiguous remote outcomes remove the tool from subsequent iterations and reject later calls in the same batch. A reaction does not prevent unrelated tools or text in ordinary turns.
- Coordinate reaction attempts per chat with a small, bounded cooldown and bounded per-target serialization so concurrent runs cannot race different bot reactions on the same Telegram message. Use explicit **run-local attempt/success state** (initialized in `ReplyWithTools()` and shared across that run's tool calls), rather than a long-lived map keyed by request IDs. Extend `CallContext` minimally to carry the effective `RunID` from `runEmitter.rc`, which may be generated when `ChatRequest.RunID` is empty; do not assume the logging `RequestID` is equivalent. Do not serialize all chats or LLM generations. Respect `retry_after` from HTTP 429 for subsequent reaction attempts; avoid unbounded waiting or infinite retries. Do not automatically retry an ambiguous transport failure, as the remote side may have applied the reaction.
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

1. Tool advertised when the triggering target and supported candidates permit it; its only argument is `emoji`; rejected extra fields/invalid emoji do not reach Telegram.
2. The concise description does not duplicate the candidate list. The dynamic enum intersects chat availability and the outgoing whitelist. Trim whitespace and accept equivalent presentation forms (`❤`, `❤️`, `❤︎`) while sending canonical Telegram bytes; reject semantic substitutions and modifiers.
3. Effective tool policy explicitly permits reaction-only and reaction+text, with the immutable current-run target rule; no fake message IDs added to prompts.

**Execution**

4. Two simultaneous runs in the same chat target their respective `Requester.MessageID`, never whichever Telegram message arrived last; the run-local attempt guard and propagated effective `RunID` must not leak across runs. Also verify separation by chat/topic.
5. Missing/invalid or bot-authored target and media-group targets fail safely before Telegram; one outgoing request carries the correct chat/message/emoji.
6. Unsupported/chat-restricted reactions and Telegram errors produce tool errors without state/event emission, and no automatic emoji fallback.
7. Cap outgoing attempts at three and confirmed successes at one; cover corrected retries, exhausted attempts, and concurrent same-target sends, 429 `retry_after`, timeouts and cancellation without cross-chat serialization or unbounded waits.
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
- `say_nothing` and terminal tool semantics were separate from #135 and are specified in [#159](say_nothing.md). Forcing silence after a reaction remains out of scope.
- Receiving the bot's own reaction as an incoming Telegram update.
- Custom/premium/paid emojis, reaction removal or animation options.
- Durable history/events, a new queue, UI redesign, LLM response streaming, or generation cancellation.

## 11. Delivery

Create the specification in-repository at `docs/specs/model_authored_reactions.md` (this document may be adapted to repo style), then implement in a dedicated feature branch from current `main`. Keep the historical scope of the previous reaction and live-preview specifications intact. Include unit/integration/race tests and updates to relevant comments. No `UPGRADE.md` change unless implementation unexpectedly requires a breaking behavior/configuration change.

Before completion, follow `AGENTS.md`: format Go, run `golangci-lint run ./cmd/bot/... ./internal/...`, `go test ./...`, build into `/tmp`, and `go test -race` for affected packages.
