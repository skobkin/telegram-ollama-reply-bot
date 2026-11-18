# Custom interaction profiles and visual editor

## 1. Scope (follow-up)

Add a Custom profile in addition to the five immutable built-in profiles. Allow an administrator to independently tune reaction/reply behavior without editing prompts or source code. Reuse the policy evaluator in `interactive_participation.md`. No new inference backend or policy engine.

## 2. Configuration

Store a versioned validated custom configuration at the selected global/per-chat scope using existing runtime settings conventions. Support copying a built-in profile as a starting point; do not mutate built-ins. Provide enable flags, independent action enable flags, reaction and reply thresholds, relevance/contribution weights, intrusion penalty, independent action-suitability minima and hard intrusion veto. Validate finite values in [0,1], weights sum to 1, threshold ordering when both actions enabled. A disabled reply stays disabled even at maximum score. Turning off both actions is equivalent to NoOp. Explicit opt-in rules remain unchanged; editing a draft does not enable anything. Inheritance/override/clear semantics must be visible and preserve existing settings on migration.

## 3. Telegram HTML Widget

Implementation is gated on native Telegram Widgets support in `telego` (the required bot library). Do not implement a raw Bot API adapter, a Mini App, or a fallback editor to work around missing support. Verify current upstream support before beginning. Official docs: https://core.telegram.org/bots/widgets and https://core.telegram.org/bots/api. Native HTML inputs, including range, are removed. Implement horizontal sliders with ordinary DOM/CSS and pointer events (`touch-action: pan-y`, pointer capture, cancel handling). Buttons for step adjustments and Save/Cancel/Reset provide a non-drag alternative. A color-coded horizontal bar visualizes Ignore / nominal React / Reply regions; labels/numeric values are mandatory. Show the independent semantic gates and explain the possible React fallback above reply threshold. Width is score interval, not message frequency. No vertical drag, keyboard input, external scripts or network.

Change draft locally during drag. Commit only after explicit Save; send compact bounded payload(s) via `TelegramWidget.request()` on a real action, not a burst of requests on each pointer move. Respect documented request/state size and rate limits; use a server-owned short-lived draft/revision token or a compact quantized encoding with validation, never secrets in widget data. Use current authoritative settings on load. Validate actor, administrator status, target scope, session/revision, permissions and all values on bot side for every mutation. Reject stale/replayed saves; server-side validation is authoritative. Prefer ordinary private admin messages, not shared widgets. No sensitive chat content enters the widget. Persist only approved settings, never draft interaction logs.

Old-client inline keyboard compatibility is a separate lowest-priority issue, not a release criterion. Keep normal built-in preset selection available in the ordinary admin menu. No external Mini App hosting required.

## 4. Acceptance tests

- Custom profile persistence and inheritance, reset/copy preset, invalid numbers and threshold combinations, no accidental enablement.
- Preview matches backend algorithm; reaction-only and no-action states, independent gate behavior and explicit Save/Cancel.
- Unauthorized/stale/tampered widget callbacks rejected, draft cancellation leaves stored settings intact.
- Pointer interactions on supported desktop/mobile clients, drag cancel, in-widget step buttons; test actual client compatibility before release (no guarantee provided by Bot API alone).
- No external network/resources or prompt/message data in widget; no writes on each drag.
