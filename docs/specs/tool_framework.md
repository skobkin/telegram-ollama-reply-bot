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
