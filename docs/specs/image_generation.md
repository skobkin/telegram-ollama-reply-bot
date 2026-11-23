# Image generation (#189)

The model can generate an image from a text prompt and send it to the chat (`generate_image`). The first backend is
**OpenAI-compatible** (`POST {base}/images/generations`) and is not tied to OpenAI models: it works with any gateway
or provider that mirrors that API, for example MiniMax `MiniMax-H3-Image`. The feature is built so another backend is
one file plus one `case`.

The tool follows the tool framework contract (`tool_framework.md`): native schema, structured errors, result budgets,
authoritative records and automatic tool statistics (`tool_statistics.md`).

## Architecture

| Piece | Where | Responsibility |
|---|---|---|
| `imagegen.Backend` | `internal/imagegen` | Wire format of one provider. `Name()` and `Generate(ctx, Request) (Image, error)`. No retries. |
| `imagegen.Service` | `internal/imagegen` | Backend-independent bounds: prompt/model/aspect validation, global concurrency cap, per-request timeout, decoded size limit, MIME sniffing. |
| `imagegen.NewFromConfig` | `internal/imagegen/factory.go` | The single place backends are registered. Returns no service when disabled. |
| `generate_image` | `internal/tooluse/tool_generate_image.go` | Model-facing tool: arguments, gating, spend bounds, delivery, error mapping. |
| `Runtime.sendPhotoToScope` | `internal/tooluse/send_photo.go` | Shared photo delivery (chat, topic, caption) also used by `generate_chart`. |

`imagegen` imports no Telegram code and stores nothing. Delivery goes through the runtime's `TelegramActionSender`
(the event-publishing wrapper in production), so the sent photo publishes `MessageSent` like every other tool send.

### Adding a backend

1. Implement `imagegen.Backend` in its own file. Return `*provider.Error` values for provider failures (classify by
   HTTP status and machine codes, never by message text); never put provider text, prompts or credentials in an error;
   do not retry (see Spend bounds).
2. Add a subsection to `config.ImageGenerationConfig` with its endpoint and credentials, validated in
   `ImageGenerationConfig.Validate`, and a `config.ImageGenerationBackend…` constant.
3. Add a `case` in `imagegen.NewFromConfig`.
4. Register every new secret in `newSecretScrubber` (`internal/app/scrubbing.go`): tokens in `scalarSecretSpecs`,
   credential-bearing URLs in `endpointSecretSpecs`.
5. Add tests like `openai_compat_test.go`: request shape, status/code mapping, provider text never surfaces, bounded
   body, sanitized transport errors.

## The OpenAI-compatible backend

- Request: `POST {BASE_URL}/images/generations`, `Authorization: Bearer <token>`, body
  `{"model","prompt","n":1,"size","response_format":"b64_json"}`. No provider-specific parameters are sent.
- `BASE_URL` is the OpenAI-style base **including the version segment**, like `LLM__BACKENDS__OPENAI_COMPAT__BASE_URL`
  (for MiniMax: `https://api.minimax.io/v1`).
- Aspect to `size`: `square` → `1024x1024`, `landscape` → `1536x1024`, `portrait` → `1024x1536`. These are accepted by
  OpenAI's gpt-image models and by MiniMax-H3-Image (area 655,360–20,000,000 px, ratio 1:3–3:1).
- `response_format` is always `b64_json`. MiniMax defaults to `url`, which is why it is sent explicitly. A response
  that carries only a `url` is treated as malformed: fetching provider-chosen URLs would need the SSRF-hardened content
  fetcher and is out of scope for v1.
- HTTP 200 does not imply success: the first `data` item is checked for a per-image `error` before its payload is used.
- The response body is read through a hard cap (the largest accepted image as base64 plus a 1 MiB envelope) **before**
  the status verdict, so an oversized body of any status is never buffered whole.
- Errors map to `provider` kinds from the HTTP status plus the provider's machine `error.type`/`error.code`:

| Condition | Kind |
|---|---|
| 401, 403 | `auth_failed` |
| 402 | `credits_exhausted` |
| 422; code `content_policy_violation`, `moderation_blocked`, `1026`, `1027` | `content_rejected` |
| 429 | `rate_limited` |
| 400 and other 4xx | `bad_request` |
| 408, 5xx, transport failure | `upstream_unavailable` |

  Provider message text is never surfaced or logged (it can echo prompts). Logs carry only the HTTP status and a short
  machine code (free-form text in those fields is dropped). Transport errors are sanitized with the cause discarded.

## Configuration

Deployment configuration (environment): endpoints, credentials and budgets.

| Variable | Meaning | Default |
|---|---|---|
| `IMAGE_GENERATION__BACKEND` | `openai_compat`, or empty to disable the whole feature | empty |
| `IMAGE_GENERATION__OPENAI_COMPAT__BASE_URL` | Endpoint base including the version segment. Required with the backend. Never inherited from `LLM__BACKENDS__*`. | empty |
| `IMAGE_GENERATION__OPENAI_COMPAT__API_TOKEN` | Bearer token. Required with the backend. Registered in the secret scrubber. | empty |
| `IMAGE_GENERATION__TIMEOUT` | Upper bound for one generation request | `60s` |
| `IMAGE_GENERATION__MAX_IMAGE_BYTES` | Largest accepted decoded image. The default is Telegram's "10 MB" photo limit read as decimal bytes. | `10000000` |
| `IMAGE_GENERATION__MAX_CONCURRENT` | Simultaneous generation requests across all chats | `2` |
| `IMAGE_GENERATION__CHAT_COOLDOWN` | Spacing between generation attempts per chat; `0s` disables it | `60s` |

Limits are validated at boot. Connection settings are only validated once a backend is selected, so a disabled feature
demands nothing. The URL check accepts `http` and `https` (like the other optional endpoints).

Runtime settings (SQLite, admin UI), per the configuration boundary (#139):

- `image_generation_model` — global only, clearable, manual entry (there is no backend model list). **No environment
  default:** an empty model keeps the tool unavailable, so enabling a backend never starts spending by itself.
- `image_generation_enabled` — global default (**on**) and a per-chat tri-state override (inherit / on / off), in the
  `🛠 Tools` submenu next to web search. The migration (v16) adds both columns plus the nullable chat override.

Both settings are hidden from the admin UI while no backend is configured (render-time only; stored values are kept).
The effective permission and model are resolved once when a request starts and stay fixed for that whole run.

## The `generate_image` tool

Arguments: `prompt` (required, up to 2000 characters), `aspect` (`square` default, `landscape`, `portrait`), `caption`
(optional plain text, up to 1024 characters, sent with the photo). Limits count characters, not bytes. Invocation
policy is **explicit request only**, side-effecting, with a small result budget.

The tool is advertised only when **all** hold: a generator is attached, the effective permission is on, a model is
selected, the turn's single attempt is unused, and the chat is not in its cooldown. The runtime re-checks before
dispatch, and a call to an unadvertised tool is rejected without reaching the provider.

Flow: validate arguments → availability and attempt check → cancellation check → time budget → cooldown admission →
generate → cancellation check → send → record → result.

Result data: `chat_id`, `topic_id`, `message_id`, `aspect` and, when the provider reports one, a `revised_prompt`
truncated to 200 characters. Image bytes and the full prompt never appear in results or in logs above DEBUG.

### Spend bounds

Generation is billed and **not idempotent**: providers charge even if the client disconnects, and a retry generates
and charges again. Hence:

- **No retries** anywhere (backend, service or tool).
- **One attempt per turn.** The attempt is consumed the moment a request is dispatched, so a failed or timed-out
  attempt also uses it up.
- **Per-chat cooldown** (`IMAGE_GENERATION__CHAT_COOLDOWN`). The slot is reserved atomically right before the request,
  so concurrent turns in one chat cannot both pass. Attempts count even when they fail, because a failed request may
  still be billed. Invalid arguments, an expired turn and "not enough time" do **not** consume it.
- **Global concurrency cap** (`IMAGE_GENERATION__MAX_CONCURRENT`). A full cap fails immediately with a rate-limited
  error; it never waits, so queueing cannot eat into the turn.
- **Time budget.** See below.

### Turn deadline

`BOT__PROCESSING_TIMEOUT` (default `30s`) bounds the **whole** turn: the model call that picks the tool, the
generation, and the model call that writes the reply. Image generation commonly takes 10–60 seconds, so the default
cannot fit it.

The tool never starts a request it cannot finish:

```
available = time left in the turn − 10s (reserved for the reply)
budget    = min(IMAGE_GENERATION__TIMEOUT, available)
```

If `available` is below 15 seconds the tool refuses (`insufficient_time`, class `cancelled`, nothing sent, nothing
consumed) instead of making a billed request that is bound to be cut off. Without a turn deadline the configured
timeout applies on its own.

**Operators:** when enabling image generation set `BOT__PROCESSING_TIMEOUT` to at least
`IMAGE_GENERATION__TIMEOUT + 30s` (for the defaults, `90s`). The bot logs a warning at boot when it is shorter. The
default is deliberately **not** raised globally: it applies to every turn, including text-only replies and proactive
participation, so a larger default would also keep a hung model call, the typing indicator and the chat's turn
occupied for twice as long on deployments that never generate images.

### Failure results

Every failure is model-safe (no provider text), non-retryable, and tells the model what to say to the user.

| Code | Class | When |
|---|---|---|
| `invalid_arguments` | `invalid_arguments` | Bad or missing arguments (a raw error, retryable) |
| `tool_unavailable` | `unavailable` | Permission off, no model, no generator, no sender |
| `image_limit_reached` | `permanent` | The turn already used its attempt |
| `image_cooldown` | `rate_limited` | The chat is cooling; carries `retry_after_seconds` |
| `insufficient_time` | `cancelled` | Not enough turn time left; nothing was requested |
| `cancelled` | `cancelled` | The turn ended before generation or before the send |
| `content_rejected` | `permanent` | The provider refused the prompt on content grounds |
| `invalid_request` | `permanent` | The provider rejected the request as malformed |
| `rate_limited` | `rate_limited` | Provider rate limit or the global concurrency cap |
| `provider_unavailable` | `transient` / `permanent` | Outage, or auth/credits/configuration (generic text) |
| `generation_timeout` | `transient` | The generation budget elapsed |
| `image_too_large` | `permanent` | Above `MAX_IMAGE_BYTES` (no document fallback in v1) |
| `invalid_response` | `transient` | Undecodable, empty or unsupported-format result |
| `generation_failed` | `permanent` | Anything unclassified |

A turn that ends after the image was generated returns `cancelled` and sends **nothing**: an expired turn never starts
a Telegram side effect. A send failure is returned raw, so `ClassifyError` marks it ambiguous (`outcome_unknown`) and
the model is told not to repeat the action blindly.

## Privacy and state

- Generated bytes are never cached or persisted; they live only for the duration of the call.
- The send is recorded in retained history (#187) as a text description with the new `generated_image` attachment
  kind: the caption the bot sent, else a short excerpt of the prompt (≤ 80 characters, whitespace-normalized). It is
  narrated to the model as an action (`(sent a generated image via generate_image: …)`), never as a bracketed
  placeholder. No image metadata is stored, so the vision model is never asked to describe the bot's own picture. See
  `outgoing_message_history.md`.
- The prompt excerpt is conversational state: in memory only, like every other history entry.
- Secrets: the API token and the URL password are registered in the secret scrubber; the transport is wrapped by the
  scrubbing HTTP doer, and the startup log never includes the endpoint.

## Telegram constraints

`sendPhoto` accepts photos up to 10 MB with width + height ≤ 10000 and aspect ratio ≤ 20; the supported sizes and the
default `MAX_IMAGE_BYTES` stay within these. Larger images fail with `image_too_large`; falling back to `sendDocument`
would need a new `TelegramActionSender` method and is deferred.

## Not in v1

Asynchronous delivery (the tool would return "queued" and a worker would send later; it needs a shutdown-aware worker,
its own error reporting and breaks the "the result reflects the side effect" contract), downloading url-only
responses, an `upload_photo` chat action while waiting, image edits, a per-chat model override, usage and cost
accounting, and adding the generated photo to history as a describable image.

## Verification checklist for a real deployment

1. `IMAGE_GENERATION__BACKEND=openai_compat`, `…BASE_URL=https://api.minimax.io/v1`, `…API_TOKEN=…`,
   `BOT__PROCESSING_TIMEOUT=90s`.
2. In the admin UI set the global `Image generation model` to `MiniMax-H3-Image`.
3. Ask the bot for a picture (also inside a forum topic) and check the photo, the caption, the cooldown message on a
   second request and the retained history entry.

Generation is billed, so run this by hand, not in CI.
