package tooluse

import (
	"context"
	"time"

	"telegram-ollama-reply-bot/internal/state"

	t "github.com/mymmrac/telego"
)

// OutgoingRecorder appends a bot message that Telegram has just confirmed to
// the retained conversation history. The bot implements it so that, besides
// the append, buffered user reactions that arrived during the send window are
// applied to the new entry. It never stores anything durably: retained history
// is in-memory state with its usual retention and privacy guarantees.
type OutgoingRecorder interface {
	RecordBotMessage(ctx context.Context, scope state.ConversationScope, msg state.Message)
}

// AttachOutgoingRecorder wires the recorder used for confirmed non-text sends.
// Like the other Attach methods it runs after construction, because the bot is
// built with the runtime as a dependency. Without a recorder, confirmed sends
// are appended to the runtime's own history store directly.
func (r *Runtime) AttachOutgoingRecorder(recorder OutgoingRecorder) {
	r.recorder = recorder
}

// recordSent records one confirmed outgoing non-text message so the model can
// refer to its own earlier actions in later turns. It is the single path every
// sending tool uses, which keeps entries uniform and the rules in one place:
//
//   - Only confirmed sends are recorded: a nil message or a non-positive
//     message ID confirms nothing, and failed sends never reach this call.
//   - The entry carries Telegram's real message ID and timestamp, the call's
//     chat/topic scope, and bot authorship. The text is empty: the attachment
//     is the content.
//   - No reply relationship is stored: these sends carry no reply parameters,
//     so none exists in the chat.
//   - Nothing here depends on execution events or tool history, so a send is
//     never recorded twice.
func (r *Runtime) recordSent(ctx context.Context, callCtx CallContext, sent *t.Message, attachment *state.Attachment) {
	if sent == nil || sent.MessageID <= 0 || attachment == nil {
		return
	}

	createdAt := time.Now().UTC()
	if sent.Date > 0 {
		createdAt = time.Unix(sent.Date, 0).UTC()
	}
	msg := state.Message{
		Name:       r.config.BotIdentity.Name,
		Username:   r.config.BotIdentity.Username,
		IsMe:       true,
		MessageID:  sent.MessageID,
		CreatedAt:  createdAt,
		Attachment: attachment,
	}

	scope := callCtx.Scope
	if r.recorder != nil {
		r.recorder.RecordBotMessage(ctx, scope, msg)

		return
	}
	if r.history == nil {
		return
	}

	msg.ChatID, msg.TopicID = scope.ChatID, scope.TopicID
	r.history.AppendMessage(scope, msg)
}
