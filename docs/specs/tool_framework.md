# Tool representation and runtime (#176)

Native tool definitions own descriptions and argument schemas. Generated policy
contains the general tool guidance and tool names grouped by advisory invocation
category; it does not authorize calls or classify user intent.

Each generation iteration resolves private presentations from registered definitions,
the operation's captured model configuration, existing run restrictions and local
availability state. Registry definitions and schema buffers remain unchanged.
Dispatch requires membership in that iteration's advertised set and a fresh local
availability check. Earlier calls in a sequential batch may make later calls
unavailable. External availability refreshes happen before generation, not during
ordinary argument validation.

The loop retains its iteration limit, sequential calls, events and authoritative
returned records. Terminal say_nothing stops pending calls and generation;
ordinary side effects remain non-terminal. Restricted React exposes only its
allowed tool and completes on a confirmed reaction.

## Errors, results and authoritative execution

Model-facing errors use `code`, `class`, `message`, `retryable`, optional
`recovery_hint` and `retry_after_seconds`. Invalid local arguments permit corrected
model calls. A rate limit requires waiting; cancellation and ambiguous side effects
must not trigger immediate replay. There is no automatic tool retry mechanism.
Raw external error text is not forwarded to logs or model-facing failures.

Returned records include handler execution, error classification/retryability and
side-effect certainty (`none`, `not_applied`, `confirmed`, `unknown`). They remain
authoritative independently of event delivery and are suitable input for future
aggregate tool statistics; this feature adds no collector or persistent traces.

Final JSON budgets count escaped serialized characters. Reduction shortens text
and omits complete collection items with explicit `truncated` and bounded
`reductions` metadata. Identity fields, numeric values and indivisible semantic
units are never rewritten. An irreducible/unserializable result produces an
explicit delivery error carrying the original `execution_status`, without changing
the authoritative call status or confirmed effects. The minimum budget is 512;
explicit smaller MCP deployment budgets fail boot validation.
