# Conversation history storage contract and invariants

**Status:** Draft for review  
**Repository:** `skobkin/telegram-ollama-reply-bot`  
**Proposed repository path:** `docs/specs/history_storage_contract.md`  
**Parent initiative:** Optional volatile history daemon (`historyd`)

## 1. Purpose and scope

Refactor conversation history behind a storage contract that supports both an in-process memory store and a future ConnectRPC remote client, without duplicating storage algorithms or changing the privacy model.

This specification covers the storage contract, raw-history/summary semantics, retention rules, transactional behavior, bounded reads, errors, and shared contract testing. It does **not** implement the RPC daemon, remote client, authentication, deployment, or semantic embeddings; those are follow-up specifications and tasks.

The existing memory engine remains the canonical implementation of all storage behavior. The remote mode will expose that **same engine** in a different process; the bot must not maintain a second writable conversation store or reconstruct state from wholesale remote snapshots.

Only `state.ConversationStore` is in scope. Image caches, process-local execution events, administrative SQLite data, stats counters unrelated to history, and other `internal/state` members are not moved by this work.

## 2. Privacy and lifetime invariants

1. Raw messages, replies/quoted content, reactions, summaries, search indexes, and future embeddings remain **volatile**. Neither implementation may write these data to durable storage, backup volumes, or logs.
2. Restarting only the bot should preserve remote history. Restarting the history daemon clears it. Restarting the local-memory bot clears local history.
3. Do not log or report message text, summaries, prompts, search excerpts, embeddings, or image descriptions at any log level. Metrics may contain aggregates only; do not expose per-message content through diagnostics.
4. The daemon's RAM-only guarantee requires separate operational controls (swap, hibernation, core dumps, container mounts, diagnostic dumps), specified in the deployment document. Process memory is not protected against a live host compromise.
5. `ENV` is the authority for external backend selection, transport credentials, resource ceilings, and endpoints. None of these are editable via Telegram admin UI.

## 3. Two representations, two independent operations

### 3.1 Raw history

- The authoritative retained sequence of original messages, including metadata, `ReplyTo`, images/metadata as currently modeled, and reactions.
- Retained raw messages remain searchable using existing lexical/fuzzy search, even when covered by a summary.
- Raw messages disappear only through an explicit reset or **storage retention/eviction** (resource limits); context summarization must never delete them.

### 3.2 LLM context view

- A derived view: an earlier LLM-produced summary plus selected recent/unsummarized raw messages.
- **Context compaction** changes this view, not raw-history retention.
- Summary generation stays in the bot/LLM layer. The storage engine stores the generated summary and validates its coverage and revision; neither memory store nor daemon invokes an LLM.
- A stored summary is a cache of a prefix, not a replacement for the original messages while those messages remain retained.
- Preserve current observable prompt and lexical-search behavior unless an independently specified change requires otherwise.

## 4. Stable sequencing and summary coverage

Replace `SummaryMessageCount` as the *authoritative* summary cursor: array positions change when the head is trimmed.

- Each conversation scope (`ChatID`, `TopicID`) receives an internal monotonically increasing logical sequence for appended raw messages. Do not confuse this with Telegram `MessageID` (which has different semantics and is shared across topics).
- A summary carries a `summarized_through` logical sequence (inclusive), a provenance/generation marker if needed, and an optimistic revision/validation token for the covered history.
- Append-only additions after the covered prefix should not invalidate a summary that describes an earlier prefix.
- Reaction edits and other modifications affecting a summarized message must invalidate/rebuild affected summary content or reject an in-flight summary commit. Changes to a covered raw prefix must never silently leave a known-stale summary in service.
- A validated summary may survive head eviction **only when the removed messages were already covered by it**, and retained messages still form a consistent tail after its watermark.
- Evicting unsummarized raw messages must not claim they were summarized. Preserve the existing valid summary if applicable, but mark/report any uncovered history gap in the internal view; do not fabricate content to fill it.
- A full-stream reset/eviction deletes that stream's raw messages, summary, search index, and associated reaction ordering data. It does not silently recreate the stream from a previous generation.
- Concurrent stale summary commits must be rejected atomically. Maintain or strengthen the existing revision-based protection.
- Define an explicit representation for an empty stream versus a stream with a valid summary but zero remaining raw messages.

**Implementation note:** During contract refactoring, preserve existing history summaries and their tests. The migration from array-based coverage to stable sequence/watermark is a separate, narrowly scoped phase with its own tests; do not combine it with introducing RPC.

## 5. Required storage operations

Preserve the existing capabilities of `state.ConversationStore` while making them suitable for a remote implementation:

- Append one message to the relevant chat/topic scope.
- Read a **bounded tail** for constructing LLM context, together with coherent summary metadata.
- Read a **bounded sequence range** for incremental summary generation and explicit history retrieval.
- Obtain a full snapshot only for operations that really need one (primarily testing/debugging/internal migration); never fetch all raw history per ordinary reply.
- Search all retained raw messages within the authorized scope, with current query semantics preserved.
- Atomically commit/update a summary with its expected history revision and coverage watermark.
- Apply incoming reactions to a bot message using Telegram update-order staleness checks across topics of the same chat.
- Apply bot-authored reactions to a user message in its precise scope; preserve existing idempotent replacement semantics.
- Reset one topic/scope or an entire chat.
- Read content-free store metrics and memory/eviction information (may initially be a separate small interface).

### API guidance

- Use `context.Context` and explicit `error` returns on operations that may cross an IO boundary.
- Distinguish: no matching stream/message, rejected stale/conflicting mutation, backend unavailable, deadline/cancellation, invalid input, unauthorized/incompatible remote peer.
- Preserve matching/in-band no-op semantics where useful, rather than conflating normal absence with infrastructure failure.
- Use result structs/typed statuses instead of an excessive number of booleans; do not force one exact method layout before examining call sites.
- `ReadTail` and summary metadata must be mutually coherent; do not make the caller race separate summary and message reads.
- Consider an interface split for readers, writers, and metrics only if it reduces complexity in real call sites; avoid speculative abstraction proliferation.
- Keep `state.Message` independent of any specific RPC DTO. The remote adapter maps domain types to/from versioned Protobuf types.

## 6. Atomicity, retries, and concurrency

- The storage engine (in memory or inside `historyd`) is responsible for serialization, indexes, reaction ordering, memory accounting, and compare-and-set summary commits. The RPC client does not implement read-modify-write cycles.
- A timeout after sending an RPC mutation may be ambiguous: the server may already have committed it. Do **not** automatically retry a non-idempotent write without protection.
- Design stable operation identities or equivalent deduplication for **retryable mutations**, especially message append and destructive reset. Preserve Telegram-specific message/update identities where they are sufficient; handle bot-generated messages consistently.
- Incoming reaction updates retain their Telegram `UpdateID` staleness rules, including across concurrent handlers.
- `SetBotReaction` remains idempotent for identical actor/emoji and does not invent a Telegram update sequence.
- Preserve per-scope isolation; an incoming reaction may require a chat-wide target lookup because Telegram reaction updates carry no topic identifier.
- An unavailable remote store is an error; **never** silently switch to in-process history.

## 7. Resource accounting and retention

- Preserve configured maximum history bytes, maximum conversation streams, and total state budget where applicable.
- In-memory estimated byte accounting must include messages, reactions, summaries, search indexes, and any future additional indexes. Distinguish estimates from real process RSS/heap metrics.
- History daemon will have its own budget. Initial target for deployment evaluation: **256–512 MiB logical history budget**, configurable via ENV; this is not a mandatory change to the existing local default or a promise about RSS.
- Eviction should be deterministic under tests and must never leave invalid indexes or falsely valid summary coverage.
- Retaining all raw messages *within the configured memory budget* is a deliberate feature, not technical debt.
- A future semantic index must support incremental additions, deletion on eviction/reset, rebuild on daemon restart, and a stable search abstraction; do not implement embeddings in this phase.

## 8. Error propagation and bot behavior

- Update every history call site, including middleware ingress, reply construction, summarization, tool search, reactions, and `/reset`, to handle errors explicitly.
- A failed history write must never be treated as successful when it was not confirmed.
- Avoid sending a response built from an unknown/partial history snapshot. Any outage policy belongs to the later bot-integration spec; this phase must preserve enough error information to implement fail-closed behavior.
- The current `llmcontext.ReplyBuilder.BuildReplyContext` does not return an error; adapt its signature/callers rather than discarding storage failures.
- Do not assume existing `AppendMessage` is idempotent. Document or implement the selected deduplication strategy before enabling automatic RPC retries for it.

## 9. Test and acceptance requirements

- Add a reusable **storage contract test suite** that can test the in-memory implementation immediately and the RPC-backed implementation later.
- Cover chat/topic separation, ordered appends, coherent bounded reads, full snapshots, search, reactions, stale Telegram updates, resets, concurrent mutations, summary CAS, and retention/eviction.
- Cover stable summary watermark and preservation through covered-prefix trimming as a separate phase; cover invalidation on modifying covered raw messages and unsummarized eviction gaps.
- Cover context cancellation and error propagation through all relevant bot paths using test doubles.
- Preserve existing user-visible behavior and local-memory startup defaults until remote integration is explicitly enabled.
- `go test ./...`, targeted `-race` tests, `golangci-lint`, and build checks must succeed after each implementation milestone.

## 10. Explicitly deferred

- ConnectRPC schema/server/client details and authentication (separate service spec).
- Docker service topology, independent image pinning, and failure/recovery procedure (separate integration/deployment spec).
- Embedding generation, vector indexes, ANN/HNSW, and hybrid reranking (separate search issue).
- Durable storage, replication, transparent failover, zero-downtime daemon migration, or mandatory cross-version RPC compatibility.

## 11. Follow-up specifications

1. `docs/specs/volatile_history_service.md` — daemon, ConnectRPC, security, protocol version, runtime lifecycle.
2. `docs/specs/history_service_integration.md` — bot startup, fail-closed behavior, container deployment, retention/metrics, operational procedures.

Add concise architectural references to these specifications in `AGENTS.md` once approved. Do not modify repository files as part of writing this draft.
