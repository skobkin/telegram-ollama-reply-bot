# Conversation metadata and tool cleanup (#179)

Chat generation automatically appends compact metadata after persona rendering,
without new template placeholders. Custom templates and captured personas retain
their existing semantics. Every request includes fresh server time, timezone and
UTC offset, retained raw-history count and available timestamp bounds, and the
number of conversation-message entries actually supplied (selected history plus
the trigger slot). Summaries, system messages and tool protocol messages do not
inflate this count; no history selection or deduplication behavior changes here.

The reply builder computes retained metadata from the same single snapshot used
for history selection. Metadata remains fixed across generation iterations and
later store changes; only current request time advances. Empty retained history
has zero messages and empty bounds. Missing metadata or any unknown timestamps
are reported as unavailable, never invented dates. Bounds use the earliest/latest
timestamps rather than assuming append order matches timestamp order.

Retained RAM history is explicitly distinguished from complete Telegram history.
Earlier summaries remain included independently. Three redundant tools are removed
as documented in UPGRADE.md; time/date and decision tools remain available. No
persistent history, database migration, template rewrite or new setting is added.
