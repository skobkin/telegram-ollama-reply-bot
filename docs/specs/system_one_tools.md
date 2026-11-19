# System One decision tools for the main LLM

## 1. Purpose and availability

Allow the normal generation model to delegate small semantic decisions to the configured System One model. This capability is independent of unsolicited participation. `Non-Interactive` must not disable explicit tool calls in an otherwise authorized LLM turn. Conversely, enabling the tools must not activate proactive behavior.

Reuse the shared System One transport and runtime model resolution defined in `system_one_runtime_selection.md`. Follow the repository's existing tool registry, explicit/discretionary invocation policy, per-tool budgets, side-effect metadata and event conventions. These tools are read-only with respect to bot state; note that calling an inference backend transmits data to that configured backend.

## 2. Proposed tool surface

Expose two tools, with names aligned to existing registry conventions:

- `decision_choose`: choose from two or more caller-defined options, with a state/context and question/instructions. Returns the selected option ID and validated distribution/scores if provided by the backend.
- `decision_assess`: assess one or a small batch of assertions or ordinal criteria using System One `noul` and `score`. Returns typed answers keyed by question ID, including documented scores/distributions where available.

Prefer compact JSON schemas; no unrestricted raw HTTP, URL, API key, model override, or arbitrary tool execution parameters. Example calls (illustrative, not fixed wire formats):

```json
{"state":"The release note describes a database migration.","question":"Which category fits best?","options":{"bug":"Bug fix","migration":"Data migration","feature":"New feature"}}
```

```json
{"state":"A short excerpt provided by the caller.","questions":[{"id":"relevance","type":"noul","instructions":"Is the excerpt relevant to the current topic?"}]}
```

Return documented semantic answers, not invented explanations. Validate criteria uniqueness, cardinality, length, supported types, result IDs and finite ranges. Do not present model confidence as ground truth. Set finite per-call and per-turn budgets using existing mechanism; a small bounded batch is sufficient. Prevent recursion: the decision client cannot call the generation model or these tools.

## 3. Configuration and privacy

Register/advertise tools only when the effective decision backend/model and existing tool policy permit them. If tools are disabled or unconfigured, no inference occurs. A provider becoming unavailable mid-turn returns a sanitized tool error; generation can continue without decision result if appropriate. No automatic fallback to another provider. The effective runtime model snapshot is consistent with existing turn configuration.

Send only explicitly supplied tool arguments and any minimal necessary trusted metadata; do not append the full chat history or persona automatically. Respect the existing history/privacy policy for tool outputs, events and persisted settings. Never add raw request/response logging or durable tool-trace storage. Do not let a tool modify profiles, settings, reaction restrictions or authorization; it cannot directly produce Telegram side effects. The proactive React-only path does not get general generation tools.

## 4. Acceptance tests

- Tools registered/available with configured backend in Non-Interactive, and existing per-tool policy respected.
- Unconfigured/disabled cases and runtime per-chat model override; no proactive participation caused by calling tools.
- Typed choice and batched assessments validated against fake transport, malformed options and results rejected, timeouts/cancellation sanitized.
- Existing tool invocation, privacy and preview/event behavior preserved without introducing new persistent content; no endpoint/model override from tool arguments and no recursive invocation.
