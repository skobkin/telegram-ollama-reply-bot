# System One runtime model selection (extension to #128)

## 1. Scope

Extend, do not replace, the existing #128 runtime model selection design. The normal LLM used for text generation and the System One model used for semantic decisions are separate capabilities, settings, endpoints and clients. Reuse the existing admin settings architecture and storage contracts. Default behavior with no System One configuration is unchanged.

## 2. Configuration and resolution

- System One endpoint/base URL (explicit contract for `/v1/systemone`), credentials and infrastructure controls remain deployment configuration (ENV or existing secret mechanism), not user-editable DB settings. Do not infer this endpoint from model names or automatically send content to cloud providers.
- The default System One model ID is a persisted global runtime setting. Per-chat settings can inherit or override it. Expose an explicit unset/disabled state if supported by the existing settings model; distinguish that from inheritance. Existing generation model settings keep their semantics.
- Use the existing model catalog / selector mechanism where applicable; a configured list or validated explicit ID must work even if backend discovery is unavailable or does not distinguish decision models. Do not assume every `/v1/models` entry is System One capable. Do not send real conversation content for model discovery or probe requests.
- At operation start resolve an immutable effective configuration (chat, model, timeout, policy and settings revision where needed). A model switch takes effect for subsequent operations, does not rewrite in-flight requests, and does not persist secrets.
- Use the same client/provider for interaction engine and explicit decision tools; no requirement for an extra service. The backend can be llama-swap with llama.cpp or another compatible provider. No automatic remote fallback.
- A configured model is not an opt-in to unsolicited activity. Global interaction preset defaults to `Non-Interactive`; no existing chat changes behavior on migration. Decision tool availability is independent from interactivity.

## 3. Client boundary

Define an application-owned small interface accepting typed choice/noul/score requests and returning validated typed answers. Actual wire names/types must match the target `/v1/systemone` contract, not illustrative examples from planning. Prefer an existing maintained native Go SDK if its local endpoint, cancellation, timeout, response validation, and multi-question support are compatible. Otherwise use a minimal `net/http` adapter. Keep SDK wire types out of business logic. Use caller context, bounded request time and response size; avoid retries that can exceed the time budget. Reject missing, malformed, non-finite or out-of-range answers. Never log HTTP bodies or raw backend error bodies.

## 4. Admin UI and persistence

Add a separate System One selector in global model settings and in per-chat model settings, showing inherited/effective values. Reuse existing auth and callback protections. Changes persist and apply without process restart. Add migration/config docs and default values; existing installations continue to work without the new settings.

## 5. Tests

- Global, inherited, overridden, disabled/unconfigured cases; switching generation model does not switch decision model or vice versa.
- Restart persists model settings but not chat history or decision content; no startup failure solely due to an optional missing decision backend.
- Transport tests with a fake HTTP server: typed batches, timeout, cancellation, invalid response and backend errors, no payload logging.
- Runtime model switch during request: stable snapshot and no cross-chat leakage.
- Smoke test against a locally configured System One backend using synthetic data, optional in CI; never require a running real model for unit tests.
