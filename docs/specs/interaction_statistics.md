# Privacy-preserving interaction statistics

## 1. Scope (follow-up after interactive participation)

Measure frequency and performance of proactive decisions without recording conversations. Statistics are aggregate only and volatile. No per-decision trace, message text, prompt, user attribution, message IDs, raw model response or timestamped activity series. Do not reconstruct history from metrics. No permanent database tables or filesystem cache for statistics. Export is an explicit admin action.

## 2. Collection

Maintain concurrency-safe counters and bounded histograms in process memory, grouped by chat and effective decision model/profile (and optional fixed rubric/preset version). Do not group by senders or free-form persona/instructions. Bound cardinality; follow existing admin authorization and chat-scoping rules. Reset on restart; support explicit reset if useful. Distinguish live and optional shadow observations so dry runs do not contaminate actual participation frequencies. When shadow observation is explicitly enabled for an opted-in chat/profile, evaluate as usual with all proactive side effects suppressed; explicit invocations remain unchanged. No text trace even for shadow mode.

Collect eligible/evaluated messages, ignored/selected React/selected Reply, no-op/errors, and actual successful reactions and text replies separately (a Reply turn may produce text and a reaction, only a reaction, or nothing, e.g. through `say_nothing` when implemented). Count successful text and reaction effects independently; both can count within the same turn. Track the number of turns with any effect separately when reporting participation frequency. Keep distinct denominators for selected and executed actions. Include fixed-bin score histograms (base and optional component scores) and latency histogram/summary. Do not retain individual observations to calculate percentiles; approximate from histogram and mark approximations. Track counts for invalid/timeout/backend failures without raw errors. Do not count disabled NoOp messages as evaluated model calls.

## 3. Admin export

Provide global or per-chat authorized export through existing admin menu (or existing stats command if compatible). Use a consistent snapshot. Prefer versioned JSON as canonical format; CSV optional for analysis. Include aggregation scope, counters, histogram bins, model/profile IDs and observation period/reset time; omit conversation and participant content. Export in memory (bytes/reader) and send only to the authorized private admin context, not a group. Exported Telegram attachments create a copy of aggregate information outside RAM; make explicit before download if needed. Do not log export bodies or silently retain uploaded document copies locally.

## 4. Subtask: generate PNG chart report

Generate a PNG from the same immutable export snapshot. Use native Go rendering (e.g. existing chart library or standard image/drawing plus embedded font assets, subject to dependency review), entirely local/in memory. No external plotting service, HTML renderer/network request, or raw conversation data.

Include selected action distribution (Ignore/React/Reply), counts and denominator; separately show actual executed effects, latency summary and suitability score histogram with threshold markers when comparing a single profile. Clearly label shadow/live, model and preset; avoid mixing incompatible thresholds on one distribution. For zero observations render a clear empty state, not misleading percentages. Attach PNG alongside machine-readable file in admin DM; if chart generation fails, numeric export remains available with sanitized error.

## 5. Tests

- Counts under concurrent updates, separate buckets after runtime profile/model changes, resets and bounds.
- Chosen vs executed vs failed outcomes and shadow vs live separation.
- Histogram boundaries and percentile approximation, empty snapshot, export consistency.
- Authorization and chat scope; JSON/CSV/PNG contain no raw texts, prompts, message IDs or participant identities. No files on disk or persistent counters after restart.
- PNG renders legibly from representative synthetic/zero-data snapshots; numeric export survives chart failure.
