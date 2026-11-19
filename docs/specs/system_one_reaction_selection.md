# Optional System One emoji selection

Follow-up to interactive participation; default off. Add a separate global/per-chat inherited setting in the Interactivity submenu choosing the emoji selector: generation LLM (existing default) or System One (explicit opt-in). Only affect proactive `React` actions, never ordinary LLM turns or `set_reaction` calls.

Ask the effective System One model a bounded choice question using the effective persona, relevant chat history, current target and actual allowed emoji candidates plus `none`. Validate candidate IDs and Telegram permissions; reuse existing reaction side-effect execution and history/event conventions. No generation fallback on selector errors and no text: fail silent. Never persist prompt or decision data. Prefer the shared System One client; no new inference service.

Acceptance: default behavior unchanged, inherited and overridden settings apply at runtime, `none` and malformed/unsupported results cause no action, a valid choice can react without a generation call, explicit replies and tools unaffected. Test locally with synthetic inputs.
