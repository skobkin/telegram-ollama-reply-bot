# Add a terminal `say_nothing` tool

Independent of proactive interaction and System One. An authorized generation model can explicitly end its current turn without sending a text response, including after a reaction or another completed tool action.

## Tool and execution contract

- Name: `say_nothing`. No arguments: empty-object schema, no additional properties. Omitted/blank arguments, `null`, and `{}` are accepted; supplied fields, other JSON values, malformed JSON, and trailing JSON are rejected without terminating.
- Description: "End the current turn without sending additional text. Previously completed tool actions remain unaffected."
- Discretionary, internal visibility, not side-effecting. Follow existing registration, permission, logging, and execution-event conventions; no configuration or durable trace is added.
- Execute tool calls sequentially in the order returned by the model. A successful call returns a terminal outcome to the orchestrator immediately: no further model request and no execution, record, or invocation event for subsequent pending calls. Preserve all earlier completed actions and records, including failed calls.
- Suppress unsent assistant text accompanying a terminal batch. The call's success and zero-length run completion are ordinary execution events; aggregate usage includes all completed generation rounds. Terminal control flow is not serialized into model-facing tool results.
- Reject unauthorized/unavailable calls normally and continue the bounded loop. Cancellation or an elapsed deadline before acceptance fails the run, retaining completed records. Once accepted with a live context, intentional completion wins over later cancellation/deadline expiry: no late timeout/error notice or timeout count.

`set_reaction` remains non-terminal in normal turns. Models may react and reply, react and call `say_nothing`, or call `say_nothing` without a reaction. The existing successful-side-effect empty-reply behavior remains available independently. Restricted proactive React still permits exactly `set_reaction` and completes on a confirmed reaction; it does not advertise or execute `say_nothing`.

## Presentation, history, and statistics

Discard the preview through its existing finalization and bounded deletion mechanisms. Stop the worker and typing fallback, delete group previews best-effort, and send no final status/tool-history-only draft or message. Private drafts stop refreshing and expire naturally within Telegram's temporary 30-second draft lifetime; there is no immediate dismissal mechanism in the current integration. Cleanup errors cannot change accepted completion into a generation failure.

Currently generation is non-streaming and `TextDelta` has no producer: previews contain progress and tool execution only. Any content already published by an earlier action or future streaming implementation cannot be retroactively unsent. Unsent text is discarded.

Keep history unchanged except for prior actions' normal writes: no empty assistant message, terminal marker, or stored tool transcript. Derive confirmed effects from existing authoritative call records. `say_nothing` is neither text nor another side effect; a silent turn without prior effects is a successful turn without effect, while preceding reactions/actions keep their existing classifications. No statistics schema, collector, or export changes.

Acceptance: terminal call ends loop with zero further requests and no unsent text published; earlier reaction remains; ordinary reaction followed by text still works; unauthorized/disabled tool calls do not terminate; mixed tool calls, cancellation, live preview cleanup and existing history behavior tested. Not a blocker for the restricted proactive React path, which independently enforces no text.
