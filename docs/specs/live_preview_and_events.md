# Implement event-driven execution, live progress previews, and LLM response streaming

## Project

Repository: `skobkin/telegram-ollama-reply-bot`

Base branch: `main`, **after `issue-100-reactions` has been merged**.

Implement the changes as **one integrated feature branch and one PR**, with intermediate implementation checkpoints and commits. Do not split the work into independently released features.

Read `AGENTS.md`, the existing architecture, admin UI implementation, and relevant tests before making changes.

## 1. Goals

Introduce a lightweight, typed, in-process event architecture to make the bot's execution flow observable and easier to extend.

Use that architecture to implement:

1. Execution lifecycle events for conversations, LLM generation, and tool calls.
2. Live progress previews for private chats, groups, and supergroups.
3. Streaming LLM responses through the existing OpenAI-compatible backend.
4. Tool execution history, including success/error status and execution duration.
5. Collapsible tool execution history in final Telegram Rich Messages.
6. Global and per-chat settings, managed through the existing Telegram admin UI.
7. Appropriate error handling, rate limiting, fallback behavior, and regression tests.

The bot serves both ordinary conversations and roleplay characters. Progress indicators must remain neutral and must not reveal the model's hidden reasoning, system instructions, or character-specific internal behavior.

Group and supergroup support is a **first-class requirement**, not a later enhancement.

## Implementation phases

The work is split into two phases. This document remains the single source of requirements for both.

**Phase 1 (current PR)** — implemented now:

- Event architecture (§3), execution instrumentation (§4), tool visibility classification (§5).
- Live progress previews in groups, supergroups, and private chats (§6), showing only the neutral indicator and tool execution events.
- Final message with collapsible tool history (§8).
- Persistent admin settings and admin UI (§9).
- Compatibility, operational, testing, and documentation requirements (§10-§12, §14) except the streaming areas.
- `TextDelta` (§3.2) ships as a typed event with **no producer** — it is the extension point for Phase 2.

**Phase 2 (deferred to a follow-up issue/PR)** — LLM response streaming:

- §7 (LLM response live streaming) in its entirety.
- The streaming sentence in §6.1 ("When LLM text streaming is enabled…").
- The "LLM streaming" test block in §12.
- Checkpoints 6-7 in §13 (streaming support, connecting streaming to previews).
- Acceptance criterion 6.

All Phase 2 sections are marked "(Phase 2 — deferred)" below; their normative text is intentionally left intact as the follow-up's requirements.

## 2. Scope boundaries

Do not implement:

- Reaction-triggered LLM execution.
- Bot-authored reactions.
- Migration of the existing reaction implementation to events.
- User-initiated or admin-initiated generation cancellation.
- Native Telegram draft Stop buttons.
- Persistent event logs or event sourcing.
- External brokers (Redis, NATS, Kafka, etc.).
- Unnecessary changes to unrelated bot functionality.

Reaction events may be added later in a separate task, after this architecture is released and validated.

Do not unnecessarily redesign the existing Telegram routing, admin authorization, persistence, or conversation history infrastructure.

## 3. Event architecture

### 3.1 Design principles

Implement a small, typed, in-process event mechanism.

Preserve the existing command/handler execution model:

- Telegram handlers remain responsible for accepting and routing updates.
- Application logic decides what actions to execute.
- The LLM does not subscribe directly to arbitrary Telegram updates.
- Events describe execution state and completed actions.
- Subscribers consume relevant events without coupling producers to their implementations.

Do not create a generic application framework, dynamically typed event dictionaries, or unnecessary reflection-based abstractions.

Prefer Go interfaces, explicit event types, and straightforward dependency injection.

### 3.2 Event families

Design around three logical families:

**Conversation lifecycle**
- `RunStarted`
- `RunCompleted`
- `RunFailed`

**Execution**
- `GenerationStarted`
- `GenerationCompleted`
- `ToolStarted`
- `ToolCompleted`
- `TextDelta`

**Interaction (future extension)**
- `MessageReceived`
- `ReactionChanged`

The last family should be accommodated by the design, but does not require new event producers in this PR.

Event names and package placement may be adjusted to fit the repository.

Avoid creating events that do not have a real current or reasonably anticipated consumer.

### 3.3 Event identity and correlation

Events should contain enough structured metadata to associate them with their originating execution.

At minimum, where relevant:

- `RunID`: unique identifier for a processing run.
- Telegram `ChatID`.
- Telegram `TopicID` / thread identifier.
- Originating user `MessageID`.
- `CallID`: unique identifier for a tool invocation.
- Tool name.
- Event timestamp.
- Tool execution status.
- Tool execution duration.

Use the existing conversation scope semantics.

Multiple concurrent requests, including requests in the same chat, must not have their events or previews mixed.

Preserve ordering within an individual execution. Do not assume global ordering between unrelated executions.

### 3.4 Event delivery and lifecycle

The dispatcher must be:

- In-process.
- Context-aware.
- Concurrency-safe.
- Bounded in memory.
- Explicitly managed during startup and shutdown.

Slow Telegram API calls must not block LLM generation or tool execution.

Use a reasonable approach to event coalescing and asynchronous UI updates. Frequent text deltas may be coalesced, but important state transitions (tool completion, failure, run completion) must remain observable.

Do not introduce unbounded goroutines or queues.

The execution runtime should remain the authoritative source of execution state. A slow preview subscriber must not be able to corrupt it.

Subscribers responsible for best-effort UI updates must not be able to fail the underlying execution.

Keep the implementation testable without real Telegram or LLM services.

### 3.5 Privacy

The bot intentionally maintains minimal persistent state.

**Do not persist execution events or conversational data for this feature.**

Events may contain transient response text when necessary for streaming, but diagnostic events should otherwise use minimal metadata.

Do not include raw tool arguments, tool results, prompts, authentication data, or potentially sensitive exception strings in preview metadata.

Preserve the existing distinction between:

- Ephemeral conversational state.
- Persistent operator-managed configuration.

## 4. Instrument the existing execution flow

Inspect and integrate with:

- `internal/telegram/bot`
- `internal/tooluse`
- `internal/llm`
- `internal/mcp`
- `internal/app`

The central tool execution loop in `tooluse.Runtime.ReplyWithTools()` is a natural event producer.

Emit appropriate events for:

- Starting a request.
- Entering an LLM generation step.
- Starting a tool.
- Finishing a tool successfully.
- Finishing a tool with an error.
- Returning to LLM generation after tool execution.
- Completing or failing a request.

### Tool completion semantics

A tool call should be considered successful only if its actual result indicates success.

Handle both:

- Go errors.
- Structured tool-level failures, including MCP `IsError` results.

Do not report a tool as successful merely because its handler returned a nil Go error.

Measure execution duration using monotonic elapsed time.

Preserve existing tool execution order, result handling, side effects, and iteration limits.

## 5. Tool visibility classification

Introduce explicit display metadata for tools.

At least two categories are needed:

- `standard`: ordinary tools.
- `internal`: tools primarily concerned with context management or internal operation.

Examples of internal tools:

- `get_conversation_summary`
- `get_history_bounds`

Examples of standard tools:

- `search_web`
- `fetch_url_content`

Review the entire existing tool registry and classify tools deliberately.

Do not hardcode tool classifications inside the Telegram renderer.

MCP tools should participate in the same mechanism, with sensible defaults and a design that permits future visibility overrides.

Classification controls presentation only. It must not change tool availability, permissions, LLM tool selection, or execution behavior.

## 6. Live progress previews

Implement a dedicated presentation component that consumes execution events and maintains a per-run preview.

A preview should display:

- A neutral active indicator.
- The chronological sequence of visible tool calls.
- `🔄` for a running tool.
- `✅` for a successful tool.
- `❌` for a failed tool.
- Execution duration for completed tools in detailed mode.
- Optionally, the currently generated answer text when response streaming is enabled and safe to display.

Example:

```text
🔄 …

✅ search_history() ⌛ 5s
✅ search_web() ⌛ 1m20s
❌ fetch_url_content() ⌛ 3s
🔄 get_conversation_summary()
```

Formatting should be readable and compact.

Use human-readable durations, for example:

- `0.2s`
- `5s`
- `1m20s`

Avoid status phrases such as "I'm thinking about how to respond" or other text implying that the character's hidden reasoning is being displayed.

A neutral `🔄 …` is sufficient.

### 6.1 Preview modes

Support three effective modes:

| Mode | Behavior |
|---|---|
| `off` | Existing typing indicator behavior |
| `tools` | Neutral progress indicator and standard tools |
| `all_tools` | Neutral progress indicator, all tools including internal ones, and execution durations |

*(Phase 2 — deferred.)* When LLM text streaming is enabled, both active modes may also display partial user-facing response text.

The distinction between `tools` and `all_tools` applies primarily to tool visibility and detail level.

Do not create a separate status-only mode.

### 6.2 Groups and supergroups

Use an ordinary temporary message, updated in place.

Flow:

1. Begin processing the user request.
2. Create a silent preview message if execution lasts long enough to justify one.
3. Reply to the triggering message and preserve the correct topic/thread.
4. Update the preview as execution events arrive.
5. When execution completes successfully, send the final reply as a **new ordinary Telegram message**.
6. After successful final delivery, delete the temporary preview on a best-effort basis.

Do not simply edit the preview into the final answer. The final response should remain a newly delivered message.

Avoid generating notifications for repeated preview updates.

Keep preview activity within reasonable Telegram API rate limits.

A short initial debounce is acceptable to avoid creating temporary messages for nearly instantaneous responses.

### 6.3 Private chats

Use Telegram's `sendRichMessageDraft` when supported.

Requirements:

- Reuse the same nonzero `draft_id` for updates belonging to a single run.
- Keep simultaneous runs isolated.
- Refresh active drafts when required by their ephemeral lifetime.
- Do not enable `can_stop` or `keep_on_stop` in this implementation.
- Persist the final answer through `sendRichMessage`.
- Gracefully fall back to the existing typing indicator and normal final delivery if draft previews cannot be used.

Do not require Telegram Premium from the bot owner or the user.

Refer to the official Bot API documentation:

https://core.telegram.org/bots/api#sendrichmessagedraft

### 6.4 Preview lifecycle and failures

The preview is a best-effort user interface.

If Telegram rejects an update:

- Log a sanitized diagnostic event.
- Apply reasonable retry/backoff behavior where appropriate.
- Disable further preview updates for the affected run if necessary.
- Continue LLM and tool processing.

Respect Telegram rate limits and `retry_after`.

Coalesce frequent updates rather than issuing an API call for every token.

On successful final message delivery, attempt to clean up the temporary group preview.

If execution fails, preserving the last preview is acceptable. Do not add persistent recovery infrastructure solely to clean up abandoned progress messages.

Likewise, a crash may leave a temporary group message behind. This is an accepted limitation.

When an execution successfully completes without a final text response (for example, a side-effecting tool completed its action), do not leave an indefinitely active-looking preview. Finish or remove it on a best-effort basis.

## 7. LLM response live streaming (Phase 2 — deferred)

Extend `internal/llm` with streaming support for the existing OpenAI-compatible backend.

Preserve the existing non-streaming implementation and provide a safe fallback when streaming is unavailable.

### 7.1 Requirements

Support:

- Text deltas.
- Streaming tool-call metadata.
- Incrementally assembled tool-call arguments.
- Multiple tool calls.
- Multiple LLM generation iterations.
- Finish reasons.
- Context cancellation and deadlines.
- Token usage accounting where the backend provides it.
- Provider incompatibilities and interrupted streams.

Use the existing `go-openai` dependency if practical.

Tool calls must be reconstructed fully before execution. Never execute a tool using incomplete streamed arguments.

Avoid accidental duplicate tool execution when handling retries or interrupted streams.

The runtime should consume streaming results through the `internal/llm` abstraction rather than depending directly on OpenAI SDK response types.

### 7.2 Privacy and roleplay isolation

Only stream actual user-facing assistant text.

Never stream:

- Hidden reasoning or reasoning-specific API fields.
- System/developer prompts.
- Tool-call arguments.
- Raw tool responses.
- Internal error details.

Some models may emit text alongside tool calls. Do not assume every assistant content fragment belongs to the final user-facing answer.

Use conservative buffering or filtering for ambiguous intermediate turns so that internal tool-planning text is not exposed as the character's final response.

If safe incremental delivery cannot be established for a particular turn or provider, continue showing tool progress and deliver confirmed answer text normally.

Preserve the bot's existing character prompt and generation behavior.

### 7.3 Response delivery

Stream partial answer text into the same preview used for tool progress.

Do not create separate preview messages for each LLM generation iteration.

Only the finalized answer should be saved to conversation history.

Retain existing handling of empty responses, failed generations, tool-only outcomes, response truncation, and provider errors.

Ensure usage metrics are neither double-counted nor silently corrupted by switching to streaming.

## 8. Final message and collapsible tool history

When tools are displayed and tool history retention is enabled, include their completed execution history in the final Telegram Rich Message.

Use Telegram-supported rich formatting with a collapsible `<details>` / `<summary>` section.

Conceptual example:

```text
Here is the answer to your question...

<details>
<summary>Tool calls (3)</summary>

✅ `search_history()` ⌛ 5s
✅ `search_web()` ⌛ 1m20s
❌ `fetch_url_content()` ⌛ 3s

</details>
```

The actual markup should follow the Telegram Rich Message specification and be tested against its supported syntax.

Requirements:

- Collapsed by default.
- Include executed tools in chronological order.
- Respect the effective preview mode's visibility policy.
- Display execution durations in `all_tools` mode.
- No raw arguments, tool results, or error messages.
- Escape tool names and other dynamic content correctly.
- Handle Telegram rich message length and block limits.
- Prioritize preserving the actual answer when content must be truncated.

If the rich message API fails and the existing MarkdownV2 fallback is used, produce a compatible readable fallback. Do not pass unsupported `<details>` markup unchanged into legacy Telegram formatting.

**The tool history block is presentation metadata, not conversational content.**

Save only the actual assistant answer text and the real final Telegram message ID in conversation history.

This is particularly important because the reactions implementation relies on real message identifiers.

## 9. Persistent admin settings

Extend the existing `adminconfig` system and Telegram admin UI.

These are operator-managed settings and should use the existing persistent configuration storage.

Do not move them to environment variables or introduce a separate configuration mechanism.

### 9.1 Global settings

Add:

**Show reply previews**
- Boolean.
- Master switch.
- Default: `Off` for backward-compatible rollout.

**Default preview mode**
- `tools`
- `all_tools`
- Default: `tools`.

**Keep tool history**
- Boolean.
- Default: `On` when previews are enabled.

If the master switch is Off, no previews should be shown, regardless of per-chat overrides.

### 9.2 Per-chat settings

Add:

**Preview mode**
- `inherit`
- `off`
- `tools`
- `all_tools`

**Keep tool history**
- `inherit`
- `on`
- `off`

Chat overrides must take precedence over global defaults, except when the global Show reply previews master switch is Off.

Use the same inheritance and override-clearing conventions as existing admin settings.

### 9.3 Admin UI behavior

When Show reply previews is globally Off:

- Hide the per-chat Preview mode control from the chat configuration keyboard.
- Prefer hiding other preview-only controls that cannot currently have an effect.
- Preserve stored per-chat overrides so re-enabling previews restores their previous settings.

Do not clutter the existing admin UI with unnecessary explanatory buttons.

Use the established admin authorization checks, callback handling, navigation, and persistent settings patterns.

Include the new fields in appropriate settings views and reset operations.

### 9.4 Effective configuration

Resolve the effective settings once for an execution, rather than repeatedly querying persistent configuration for every token or tool event.

A mid-execution settings change does not need to reconfigure an already running preview.

## 10. Conversation history and reaction compatibility

Assume `issue-100-reactions` has already been merged.

Review its final implementation before editing relevant code.

Preserve:

- The existing in-memory history behavior.
- The recording of real Telegram bot reply message IDs.
- Reaction updates and their association with bot replies.
- History revision safeguards.
- Summary invalidation on reaction changes.
- Existing allowed Telegram update types.
- Topic-scoped conversation history.

Do not migrate reactions to event-driven handling in this PR.

However, avoid design choices that would make a future `ReactionChanged` event difficult to introduce.

Do not introduce a generic event-to-LLM trigger mechanism as part of this task.

## 11. Operational and performance requirements

### Reliability

- Preserve existing processing timeouts.
- Respect context cancellation internally.
- Do not introduce user-facing Stop controls.
- Ensure event subscribers cannot prevent final answer delivery.
- Do not allow a failed preview update to cause duplicate LLM or tool execution.
- Do not block indefinitely while attempting preview cleanup.

### Resource usage

- Keep events and preview buffers bounded.
- Avoid unbounded accumulation of token deltas.
- Use appropriate truncation or compaction when an execution invokes many tools.
- Avoid excessive allocations or complete message rebuilding for every token.
- Keep Telegram update frequency under explicit control.

### Security

- Do not log or display confidential tool arguments.
- Do not expose raw MCP errors or responses.
- Do not render arbitrary unescaped tool names as Telegram markup.
- Maintain all existing access restrictions.
- Ensure that preview messages appear only in the correct chat and topic.

### Backward compatibility

With Show reply previews globally Off, the bot should retain its existing externally visible behavior as closely as possible.

If streaming is disabled or unsupported, the existing non-streaming path must remain functional.

## 12. Testing requirements

Implement unit and integration tests with existing repository conventions.

At minimum, cover:

**Event system**
- Correct run/call correlation.
- Ordered events within a run.
- Concurrent independent runs.
- Slow or failing subscribers.
- Bounded buffering and coalescing.
- Clean shutdown and cancellation.

**Tool execution**
- Tool start/completion events.
- Successful and failed calls.
- MCP tool-level errors.
- Multiple calls and iterations.
- Accurate durations.
- Tool visibility classification.

**LLM streaming** *(Phase 2 — deferred)*
- Text-only responses.
- Fragmented tool-call arguments.
- Multiple tool calls.
- Tool-call and text mixed responses.
- Interrupted streams.
- Unsupported streaming fallback.
- Context deadlines.
- Empty responses.
- Usage accounting.
- No duplicate side effects.

**Telegram previews**
- Groups and supergroups.
- Private chat drafts.
- Topic-specific replies.
- Concurrent runs in the same chat.
- Telegram API errors.
- Rate limiting.
- Long-running operations.
- Cleanup after successful final delivery.
- Failure behavior.
- No temporary preview messages in history.

**Admin configuration**
- Global master switch.
- Per-chat inheritance and overrides.
- Hidden controls when previews are disabled.
- Settings persistence across restart.
- Reset/clear override behavior.
- Existing authorization boundaries.

**Final response**
- Valid collapsible tool history.
- All display modes.
- Durations and statuses.
- Escaping and length limits.
- Legacy Markdown fallback.
- Final message ID preservation.
- Compatibility with `issue-100-reactions`.

Use fake Telegram API servers and controllable streaming LLM backends where appropriate.

Do not rely solely on happy-path tests.

## 13. Implementation workflow

Complete this as one long-running implementation task, with the following checkpoints:

1. Inspect repository architecture, merged reaction changes, and existing tests.
2. Design and implement the minimal event layer.
3. Instrument the existing execution flow.
4. Implement tool visibility classification and event-based progress state.
5. Implement Telegram preview rendering for all supported chat types.
6. Add streaming support to `internal/llm`. *(Phase 2 — deferred)*
7. Connect streaming text to live previews. *(Phase 2 — deferred)*
8. Extend persistent admin settings and UI.
9. Add final-message collapsible tool history.
10. Complete regression tests, documentation, and final review.

Use small, logically grouped Conventional Commits on the same feature branch.

Avoid committing partially broken states when practical.

Do not open separate PRs or release intermediate stages.

If an implementation detail conflicts with existing behavior, inspect the code and tests before changing the architecture.

Prefer the least complex implementation that satisfies the specified behavior.

Ask for a decision only if there is a genuine product or architectural conflict not resolved by this specification. Otherwise, make reasonable local implementation decisions and document them.

## 14. Documentation and completion

Update:

- `README.md` with a concise description of preview behavior and configuration.
- `UPGRADE.md` with new persistent settings, defaults, and any operational compatibility notes.
- Relevant existing architecture documentation if applicable.

Follow `AGENTS.md` for validation.

At minimum, run:

```bash
gofmt -w <changed Go files>
golangci-lint run ./cmd/bot/... ./internal/...
go test ./...
go build -o /tmp/telegram-ollama-reply-bot ./cmd/bot
```

Also run relevant race tests for the new concurrency-sensitive components, where practical.

The final report should include:

- Architecture summary.
- Files and packages added or modified.
- Event delivery and lifecycle decisions.
- Streaming implementation and compatibility limitations.
- Telegram preview behavior by chat type.
- New settings and their defaults.
- Test and lint results.
- Known limitations requiring manual Telegram testing.
- Any intentionally deferred work.

## Acceptance criteria

The work is complete when:

1. The bot has a minimal typed in-process event mechanism with explicit lifecycle and concurrency guarantees.
2. LLM and tool execution publish useful events without coupling execution logic to Telegram presentation.
3. Tool progress is visible live in groups, supergroups, and private chats when enabled.
4. Tools display accurate execution status and duration.
5. Internal tools can be shown or hidden according to the effective preview mode.
6. LLM responses can stream incrementally through the OpenAI-compatible connector. *(Phase 2 — deferred)*
7. Draft previews are used in private chats; editable temporary messages are used in groups.
8. Final responses remain separate, persistent Telegram messages.
9. Tool history can be retained in a collapsed Rich Message section.
10. Global and per-chat configuration works through the existing admin UI, including inheritance and hiding inactive controls.
11. No private execution content is exposed or persisted unnecessarily.
12. Existing reaction history, message ID tracking, conversation history, and admin behavior remain correct.
13. The full implementation is committed to one branch, passes required checks, and is ready for one PR and one release.

**Priority:** correctness, privacy, maintainability, and reliable group-chat behavior over elaborate abstractions or visual effects.

**Out of scope for this PR:** migrating reactions to events, reaction triggers, bot-authored reactions, and generation cancellation.
