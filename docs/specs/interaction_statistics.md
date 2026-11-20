# Privacy-preserving interaction statistics

## 1. Scope (follow-up after interactive participation)

Measure frequency and performance of proactive decisions without recording conversations. Statistics are aggregate only and **persistent** (#182, superseding the original volatility requirement): the accumulated aggregates survive restarts and container updates in the existing SQLite database (§6). No per-decision trace, message text, prompt, user attribution, message IDs, raw model response or timestamped activity series; the only persisted timestamps are the observation-period start and the snapshot save time. Do not reconstruct history from metrics. No per-event rows and no time buckets. Export is an explicit admin action.

## 2. Collection

Maintain concurrency-safe counters and bounded histograms in process memory, grouped by chat and effective decision model/profile (and optional fixed rubric/preset version). Do not group by senders or free-form persona/instructions. Bound cardinality (chats, profiles, decision models, generation models; overflow keys are explicit tags, never labels); follow existing admin authorization and chat-scoping rules. Counters are restored on startup and reset only by the explicit, independent admin reset. Distinguish live and optional shadow observations so dry runs do not contaminate actual participation frequencies. When shadow observation is explicitly enabled for an opted-in chat/profile, evaluate as usual with all proactive side effects suppressed; explicit invocations remain unchanged. No text trace even for shadow mode.

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
- Authorization and chat scope; JSON/CSV/PNG contain no raw texts, prompts, message IDs or participant identities. No statistics files on disk; persisted state is aggregate-only (§6).
- PNG renders legibly from representative synthetic/zero-data snapshots; numeric export survives chart failure.

## 6. Persistence (#182)

**Storage.** One row per dataset in `statistics_snapshots` (migration v13): the whole aggregate state as versioned JSON (`format_version`, dictionary-encoded chat/profile/model references, explicit overflow tags, histogram bin arrays), plus the `(generation, revision)` pair and the save time. Rows are replaced whole. The interaction dataset is stored under `interaction`; general bot statistics stay ephemeral. The Persister is shared with tool-usage statistics.

**Consistency.** A snapshot is captured under the collector lock as one internally consistent copy; JSON encoding and the database write run after the lock is released. Saves are batched on a 30 s ticker (never per decision) and only while the dataset is dirty. Dirty means the complete `(generation, revision)` pair differs from the last acknowledged pair; a successful save acknowledges exactly the pair it wrote, so mutations or resets during a write stay dirty. The write is a conditional upsert that only accepts a strictly newer pair, so a delayed write cannot restore counters after an administrative reset. A generation is the reset epoch: it is restored at startup and stays monotonic across restarts.

**Restore.** Restoration runs synchronously before any producer records. The payload is validated before it is installed (dictionary caps, reference integrity, histogram bin counts, global totals equal to bucket sums, per-bucket histogram/counter agreement, ledger feasibility). Overflow-tagged buckets are restored as overflow and global totals and fold counters are restored verbatim; restoration never counts as new folds. A payload that fails validation is quarantined atomically (`interaction.unreadable`, one overwritten slot) and the dataset starts fresh; if quarantine fails, persistence is disabled for the dataset and the original row is never replaced. A row with a newer `format_version` (application downgrade) is left untouched: statistics are collected in memory only and the state is disclosed (`disabled_newer_format`). A database read error fails startup rather than starting empty.

**Interrupted operations.** Live operation state is never restored as if it could resume. At restore, operations that were open at the last successful save are reconciled once into explicit counters, without inventing outcomes:

- `submission.interrupted_by_restart` (per chat and global) receives `Submitted − SubmitRejected − AcceptedThenCancelled − Preflight − Decisions − InterruptedByRestart`. This includes provisional submissions (counted by `BeginSubmission`, not yet resolved by `Submit`).
- `execution.turns_interrupted` (per bucket and global) receives `Dispatched − Completed − TurnsInterrupted`, where Completed is the execution-duration histogram total.

Both formulas subtract the counters reconciled earlier, so repeated restore/save/restart cycles never double-count. Interrupted turns are not a failure class and contribute no effects. An impossible ledger (a negative intermediate) marks the payload malformed. Operations that actually finished after the last successful save but before termination are indistinguishable and are also counted as interrupted. The statistics epoch is process-local; restoration installs the persisted generation before any submission, so stale epochs cannot cross a restart.

**Durability.** While saving succeeds, an abrupt termination loses at most the observations since the last successful save (about 30 s plus one in-progress write). A graceful stop flushes after every producer has stopped, closes admission, joins the loop and any in-flight write (never abandoning them, so the store is never closed under a worker), then runs the final flush under a 5 s budget; waiting longer than that for a worker is reported in the shutdown error. This relies on the database driver honouring context cancellation. During a write-failure streak the aggregate stays in memory and dirty (nothing is discarded, the full state is retried each tick, only the first failure of a streak is reported) and the possible loss window extends to the start of the streak. A payload larger than 32 MiB is never written.

**Reset.** The interaction reset is independent of the general and tool resets. It starts a new generation, is persisted synchronously and is acknowledged only when the persisted generation reaches it (a periodic save that already persisted it counts). A failed reset write followed by a crash before successful persistence restores the previous dataset: the admin is told "applied, but saving failed — will be retried"; with persistence disabled the admin is told the reset is memory-only and is not retried.

**Bounds.** Chats 32 (+overflow), profiles 8 (+overflow), decision and generation models 8 (+overflow) each, 128-byte identifiers. The worst case (every dimension saturated, maximum-width counters) is 2,673 decision and 24,057 execution buckets: about 27 MB strict payload (derived and asserted by a test from the real constants), about 8 MB of counters in RAM. Realistic deployments stay in the tens of kilobytes.

**Export.** `tg-llm-bot/interaction-statistics` schema version 2 (additive over 1): `persistence` block (`state`, `restored`, `last_saved_at`), `interrupted_by_restart`, `turns_interrupted`, `profile_overflow` tags and `profile_overflow_counts`. `observation_period.started_at` now survives restarts. The PNG shows interrupted counts.
