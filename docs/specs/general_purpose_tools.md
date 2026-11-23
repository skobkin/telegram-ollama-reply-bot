# General-purpose utility tools (#180)

Built-in tools: `calculate`, `random_choice`, `get_message_context`, `generate_chart` and (#181)
`convert_units`. All follow the
tool framework contract (`tool_framework.md`): native schema, structured errors, result budgets,
authoritative records and automatic tool statistics (`tool_statistics.md`).

## calculate

`expr` is used only as a **parser**. The parsed tree is validated and evaluated by the tool itself in
float64, so the checked tree is the executed tree (no expr compiler, optimizer, VM or builtins).
Allowed: numeric literals, unary `+ -`, binary `+ - * / ^ **`, constants `pi` and `e`, and a fixed
function table with exact arity. Everything else is `unsupported_operation`.

- `^` / `**` are right-associative and bind tighter than unary minus: `2^3^2 = 512`, `-2^2 = -4`.
  `/` is real division (`1/2 = 0.5`). Trigonometry uses radians.
- Bounds: 256 characters, 64 AST nodes, depth 32.
- Any non-finite intermediate or final value is `non_finite_result`.
- The expression is never echoed, so the result size is constant.
- Error codes (class `invalid_arguments`, retryable): `invalid_expression`, `unsupported_operation`,
  `wrong_arity`, `expression_too_complex`, `non_finite_result`.

## convert_units (#181)

Local, deterministic unit conversion with no external service. Arguments: `value` (finite number),
`from` and `to` (unit ids, at most 32 characters).

- Dimensions: length, mass, volume, temperature, speed, pressure, energy, data. Each unit is defined by a
  factor (and, for temperature, an offset) relative to the dimension's base unit; the table in
  `tool_convert_units.go` is the single source of truth and also generates the model-facing description.
- Unit ids are case-sensitive. A few unambiguous aliases are accepted (`celsius`, `liter`, `kph`, `feet`, ...).
  Ambiguous spellings (`KB`, `Kb`, `mb`, `gb`, `C`, `F`) are rejected so the caller must choose.
- Data sizes keep decimal (`kB`, `MB`, `GB`, ... = 1000-based) and binary (`KiB`, `MiB`, `GiB`, ... =
  1024-based) units apart; `bit` and `B` (byte) are distinct.
- Results are rounded to 12 significant digits to hide float64 noise (`0 degC -> 32 degF`, not
  `31.999999999999996`). The input value is never echoed; the result carries the canonical `from`, `to` and
  `dimension`.
- Error codes (class `invalid_arguments`, retryable): `unknown_unit`, `incompatible_units`,
  `below_absolute_zero`, `invalid_value`, `result_out_of_range`. Echoed unit strings are truncated and quoted.
- Out of scope: currency conversion and live exchange rates, calendar and time units (use the datetime
  tools).

## random_choice

1..50 items (trimmed, non-empty, no control characters, at most 100 characters), `count` 1..10 distinct
picks without replacement. Duplicate texts are distinct entries. Indexes are **zero-based** and the
result reports `index_base: 0`. Before drawing, the worst-case selection (largest escaped items) is
serialized against the 3000-character budget; anything that would not be delivered intact is
`selection_too_large`. The random source is `math/rand/v2`'s concurrency-safe global generator.

## get_message_context

Reads only the current chat/topic snapshot. Messages are folded by message ID (duplicates merged,
ID 0 skipped) and ordered by ID, independent of insertion order. `before` and `after` (0..10, default 3)
are listed **nearest first**, so budget reduction drops the farthest messages first.

`retained_window.start_reached` / `end_reached` mean only "the returned window includes the oldest /
newest retained message"; they are not evidence that older or newer messages existed and were
discarded. Author `name` / `username` are clamped (64 / 32) with `name_truncated` /
`username_truncated`; `text_truncated` is set only on actual shortening. A target that is not retained
(or belongs to another topic) is `message_not_in_history` (`not_found`).

Window flags describe the neighbours actually serialized: the handler drops the farthest neighbours until
the result fits the budget and only then computes the flags, so a removed boundary message is never
reported as reached. The omission is reported through `truncated` and `reductions` (`data.before` / `data.after` with omitted counts); that metadata is part of every fitting candidate.

Visibility is `VisibilityStandard` on purpose: like `search_history`, the lookup should appear in normal
tool history. Visibility only affects presentation, not availability or access control.

#185 (per-tool result reduction) stays deferred: nearest-first ordering plus clamped fields satisfy
the budget guarantees with the generic reducer.

## generate_chart

Package `internal/content/chart` (independent of `internal/statistics`). Line, bar (vertical,
horizontal, stacked) and pie. Bounds: 50 categories, 8 series, 400 points, 12 pie slices, labels 32
characters, title 60, |value| ≤ 1e15, width 600–1600, height 360–1200. Titles, labels and legend names
are measured with the real font and shortened with an ellipsis (display only).

Flow: validate → cancellation check → render → cancellation check → `SendPhoto`. A cancellation seen
before dispatch is `cancelled` with side effect `not_applied`; a failure after dispatch is classified
by `ClassifyError` (definitive Telegram 4xx/429 → not applied, otherwise `outcome_unknown`). Like
polls and dice, the sent chart publishes `MessageSent` and is not added to retained history.
