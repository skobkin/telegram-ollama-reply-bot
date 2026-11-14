# The `ReactionChanged` interaction event

Companion document to [`live_preview_and_events.md`](live_preview_and_events.md). That document
introduced the typed, in-process event architecture and, as part of its historical scope,
explicitly deferred reaction migration (§3.2 "Interaction (future extension)", §10 out-of-scope
list). This document specifies the interaction family it reserved: the migration of Telegram
reaction handling onto that architecture. It is a record of guarantees, not a feature request.

## What `ReactionChanged` means

`ReactionChanged` (declared in `internal/execution/events.go`) reports **one authorized,
relevant incoming Telegram reaction update targeting a message in a chat the bot serves**.

Breaking that down:

- **Authorized and relevant** — the update passed the reaction pipeline's stages: it carries a
  per-user actor (anonymous reaction-count updates are ignored), access restrictions held
  (`BOT__DM_ADMINS_ONLY`, chat allowlist), and the tracked-emoji whitelist found something to
  interpret (a new tracked set, or a change away from a previously tracked set). Rejected or
  irrelevant updates never produce an event.
- **Targeting a message** — `TargetMessageID` identifies the message the incoming reaction
  targets. It is **not** a claim that the target was verified as a stored bot reply: an eligible
  update on an unknown or no-longer-stored message (user message, restart, history trim, reset)
  still publishes. The store's chat-wide lookup decides whether history changes; the event does
  not.
- **An observation, not a commit** — the event never guarantees that the target existed in
  history, that the change was applied rather than rejected as stale, or that a newer state was
  not committed afterwards. Conversation history is the authoritative record; this event is
  observational output of the pipeline.

Payload summary: embedded `RunContext` with **only `ChatID` set** (no `RunID` — reactions are not
part of any processing run; no `TopicID` — Telegram reaction updates carry none), `At` (Telegram
event time when the update provides it, arrival time otherwise), `TargetMessageID`, actor
attribution (`UserID`, `Name`, `Username`), `UpdateID`, and `Reactions` — the actor's complete
tracked set after the change as exact Telegram emoji strings, with **an empty set meaning
removal**. The payload is immutable after publication: the publisher builds a fresh slice, and
subscribers must not mutate it.

Note the timestamp duality: `At` reflects Telegram time, while the stored
`state.Reaction.UpdatedAt` reflects the store's apply-time clock. This is pre-existing behavior,
preserved deliberately.

## Why history mutation stays synchronous and authoritative

The reaction pipeline keeps exactly one authoritative state-mutation path:
`applyReactionChange` → `ConversationStore.SetMessageReactions`, applied synchronously on the
Telegram handler's goroutine. The store guards each user/message pair by Telegram's sequential
update ID, so concurrent handlers, delayed updates, and replays cannot resurrect stale state,
and it bumps the history revision and invalidates affected summaries so derived state cannot go
stale.

This ordering is deliberate: the dispatcher is best-effort (next section), so any design that
mutated history from an event subscriber could silently lose reactions. The event is published
**after** the synchronous processing attempt, and nothing downstream can influence what was
already committed.

## Why the dispatcher is observational and best-effort

`Publish` never blocks and never fails a producer; under extreme backpressure a lagging
subscriber is unsubscribed and its pending events are lost (see the dispatcher documentation in
`internal/execution/dispatcher.go`). A stopped or unwired dispatcher turns publication into a
no-op. `ReactionChanged` is a non-expendable event like every transition event: while a
subscriber is healthy it is delivered in publish order, but delivery is a courtesy, not a
contract. Consumers must be correct without events; the reaction pipeline is, because history
mutation does not depend on delivery.

## Why `UpdateID` remains authoritative for ordering

Telegram delivers updates to concurrent handler goroutines, so arrival order is not update
order. The dispatcher preserves publication order **per producer only** — it provides no global
ordering across concurrent handlers, and event delivery order must never be read as reaction
order. The authority is the store-side per-user and per-message `UpdateID` comparison carried by
`reactionChange.updateSeq`: anything not strictly newer is rejected as stale, removals included.
The event's `UpdateID` field exposes that same envelope sequence for subscribers that want to
reason about ordering; it is descriptive, not a delivery guarantee.

## Publish-once semantics

Each eligible incoming update publishes exactly one `ReactionChanged`, at the ingress, after the
synchronous processing attempt. Updates held in the pending-reaction buffer (because the bot
reply's history insertion was still in flight) publish at ingress eligibility like any other;
the later replay onto freshly inserted history mutates history directly and **never publishes
again**. The store's sequence guard makes the live-apply/replay interleaving idempotent, so
there is one authoritative write and one event per eligible update.

## Why events do not trigger model generation

`ReactionChanged` is an extension point, not a trigger. There is deliberately no mechanism by
which any execution event causes an LLM request — that boundary was established when the event
architecture was introduced (see §10 of the live-preview spec) and is preserved here. Reaction
data reaches the model only through conversation history, as it did before this migration.

## Privacy and lifetime

Events are transient and in-process: nothing persists them. Payloads exclude raw Telegram
structures and carry only the attribution the history rendering needs. Do not log actor names,
usernames, or reaction payloads; normal processing logs use IDs and emoji-set summaries only.

## Current subscribers

The live preview manager ignores interaction events: with an empty `RunID` they match no
preview run, create none, and change no progress behavior. No subscriber duplicates the
reaction-history mutation.
