# Non-text messages in retained history (#187)

Retained conversation history used to hold only text. Polls, quizzes, dice, charts and stickers the bot sent
through tools, delivered reminders, and non-text messages from users (stickers, voice notes, documents, ...)
were missing or stored as empty entries, so the model could neither read nor refer to them.

## Persistence decision

**Nothing here is durable.** Descriptions live in the same in-memory history as every other message and follow
its retention, eviction, reset and summarization rules. No new table, file or snapshot is introduced, and no
binary content or Telegram file identifier is retained for these entries.

## Representation

`state.Message.Attachment` (`Kind` + `Label`) is a bounded, single-line, text-only description. Photos keep their
dedicated `HasImage`/`ImageMeta` representation. One renderer, `Attachment.Placeholder()`, produces
`[Poll: "Dinner?" — Pizza / Sushi]`, and every consumer uses it: prompt context, summarization transcripts, the
proactive decision history, the history search index, `search_history`, `get_message_thread_context` and
`get_message_context`. Tool results additionally expose `attachment_kind` (and `search_history` exposes `is_me`).

Labels are built by `internal/telegram/msgdesc` and are privacy-conservative: contacts and locations are
described only as `[Contact]` / `[Location]` (no phone numbers, names or coordinates); quiz answers and
explanations are never part of a label; charts are described from their spec (`bar: Votes`), never from pixels.

## Recording

`Bot.RecordBotMessage` is the single path for bot-authored entries. It stamps authorship and scope, defaults the
timestamp, appends to history and replays reactions that arrived during the send window. Text replies,
tool sends and delivered reminders all use it.

- Tools: `Runtime.recordSent` is called right after a **confirmed** send (a send error, a nil message or a
  non-positive message ID records nothing). The entry carries Telegram's message ID and timestamp, the call's
  chat/topic scope and bot authorship; its text is empty.
- No reply relationship is stored: these sends carry no reply parameters.
- Recording does not depend on execution events or tool history, so a send is recorded exactly once.
- Charts and stickers carry no image metadata, so the vision model is never asked to describe the bot's own output.
- Reminders: `reminders.Service` records the delivered `Reminder: …` text after a confirmed send only.

## Incoming and quoted messages

User messages are labelled in `tgUserMessageToMessageData`. A reply to a bot poll, sticker or chart resolves to
the stored entry, or — when none is stored (restart, eviction) — to the described fallback instead of being
dropped for lacking text. Users' reactions to recorded bot media are stored like reactions on text replies.
