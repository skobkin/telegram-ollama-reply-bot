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

Runs carry a `Proactive` flag set from the trigger the bot derived from Telegram
metadata (never from model output). Tools use it for limits that must not get in
the way of an explicit request, such as the proactive-only sticker cooldown.

## Errors, results and authoritative execution

Model-facing errors use `code`, `class`, `message`, `retryable`, optional
`recovery_hint` and `retry_after_seconds`. Invalid local arguments permit corrected
model calls. A rate limit requires waiting; cancellation and ambiguous side effects
must not trigger immediate replay. There is no automatic tool retry mechanism.
Raw external error text is not forwarded to logs or model-facing failures.

Returned records include handler execution, error classification/retryability and
side-effect certainty (`none`, `not_applied`, `confirmed`, `unknown`). They remain
authoritative independently of event delivery and are the input of the aggregate
tool statistics (`docs/specs/tool_statistics.md`); records themselves are never
persisted.

Statistics-facing record metadata (#178):

- `Rejection` distinguishes calls rejected before handler execution:
  `not_registered` (the model-chosen name matches no registered tool),
  `not_advertised` (registered, but not in that iteration's advertised set) and
  `unavailable` (advertised, but the fresh availability check failed). The
  model-facing result and the events stay identical (`tool_unavailable`). The raw
  name of a `not_registered` call is never an aggregation key.
- `ResultPreparation` (`complete`, `reduced`, `failed`) classifies how the
  model-facing result message was *prepared*. It does not claim delivery: terminal
  completion, cancellation, restricted completion or loop exhaustion can end the
  run before any later generation request carries the message. Status and side
  effects stay authoritative and independent of it. The classification is
  returned by the serializer from the path it actually took (not inferred from
  the produced JSON): `reduced` covers normalization shortening an error message
  or hint, budget reduction (also of an already truncated result) and an error
  contract kept while optional data was dropped; `failed` is the generic delivery
  failure replacing an irreducible or unserializable result. The wire format is
  unchanged.
- `Result.AdvertisedTools` is the sorted union of names advertised in generation
  requests that were actually submitted (an iteration whose request preparation
  failed contributes nothing); each name counts once per run.
- `Runtime.ToolNames()` is the registered catalog in registration order. It is
  static after startup.
- `empty` is a successful status (`execution.StatusEmpty`): the tool ran and
  validly found nothing.
- Calls skipped after an accepted terminal completion or a context-done batch
  abort produce no record.

Final JSON budgets count escaped serialized characters. Reduction shortens text
and omits complete collection items with explicit `truncated` and bounded
`reductions` metadata. Identity fields, numeric values and indivisible semantic
units are never rewritten. An irreducible/unserializable result produces an
explicit delivery error carrying the original `execution_status`, without changing
the authoritative call status or confirmed effects. The minimum budget is 512;
explicit smaller MCP deployment budgets fail boot validation.
