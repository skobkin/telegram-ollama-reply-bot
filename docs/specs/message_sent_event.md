# The `MessageSent` interaction event

Companion to [`reaction_changed_event.md`](reaction_changed_event.md), which established the
interaction-event family inside the typed in-process event architecture (see
[`live_preview_and_events.md`](live_preview_and_events.md)). This document specifies the second
interaction event: the observation that a message became visible in a chat the bot serves. It is
a record of guarantees, not a feature request.

## What `MessageSent` means

`MessageSent` (declared in `internal/execution/events.go`) reports that a message became visible
in a chat the bot serves — a message the bot itself sent (ordinary and final LLM replies, command
answers, error notices, reminder deliveries, tool-created polls and dice) or a message a user
sent (published at the update ingress). The current consumer is the admin UI: it re-creates a
menu message buried under newer chat messages (issue #145).

Payload: embedded `RunContext` with **only `ChatID` set** (no `RunID` — sends are not run-bound;
no `TopicID`), `At` (publish-time), and `MessageID` — the Telegram message ID when the publisher
knows it (`0` for the ingress, which publishes before handler dispatch). The event never carries
message text.

## Publishers and exclusions

Publication follows the send path, not the message kind:

- **Bot package** (`internal/telegram/bot`): plain sends funnel through a `sendMessage` wrapper;
  the rich-message pipeline (`SendRichMessage`) publishes explicitly on success. The ingress
  middleware publishes one event per incoming channel message — **commands included**, because
  `/admin` reopens a live menu by editing it in place, and only observing the command itself can
  surface a menu buried under newer messages.
- **App wiring** (`internal/app`): reminder deliveries and model-authored poll/quiz/dice actions
  go through an event-publishing sender wrapper.
- **Excluded, by construction**: callback presses (not messages — an admin pressing a button
  proves the menu is visible, and re-creating it would be churn), rich-message **drafts**
  (private-chat previews are not chat messages), group preview messages (admin menus live only
  in private chats; consumers filter by chat anyway), and everything the admin UI itself sends
  (input prompts, feedback, documents, picker keyboards use the raw API client and publish
  nothing — a pending ForceReply prompt must stay at the bottom of the chat).

## Suppression and re-creation discipline

The admin UI re-creation is debounced (a 1.5 s quiet window, so bursts cost one re-creation) and
re-validated at fire time: only a live session with **no pending input** re-creates, under the
per-admin render gate, presentation-only (`Seq` and session TTL untouched; see
`Manager.RecreateRender`). The old menu message is deleted best-effort after the fresh one is
sent; callbacks on deleted or stale menus are refused by the existing origin/stale-seq checks.

## Best-effort delivery

Like every execution event: `Publish` never blocks or fails a producer, an unwired or stopped
dispatcher makes publication a no-op, and the debounce worker drops events when flooded (the
next quiet window re-anchors the menu anyway). No send path depends on delivery; nothing in the
bot's authoritative behavior changes when events are lost.

## Known interleaving

An admin callback that commits between the re-creation's session snapshot and its Telegram send
re-renders the session's old message ID afterwards and takes the existing message-gone resend
path. The window is one send round-trip; the worst case is a stale copy of the menu next to the
fresh one, recovering via the next navigation (or `/admin`). Closing it fully would require
serializing `Apply` commits behind the render gate — not worth the lock complexity for the
frequency and recoverability of the case.

## Events do not trigger model generation

Preserved boundary: no execution event causes an LLM request. `MessageSent` reaches only
in-process consumers.

## Privacy and lifetime

Transient and in-process; nothing persists events. Payloads carry chat and message IDs only —
never message text, actor names, or payloads. Do not log event contents beyond IDs.
