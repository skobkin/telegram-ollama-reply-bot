package bot

import (
	"context"
	"time"

	t "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"

	"telegram-ollama-reply-bot/internal/execution"
)

// This file publishes the MessageSent interaction event: an observation that
// a message became visible in a chat the bot serves. Publication is
// best-effort and observational — a nil dispatcher (no dispatcher wired or
// stopped) is a no-op, and no send path depends on delivery. Consumers (the
// admin UI menu re-creation) filter by chat themselves.

// sendMessage sends a plain message and publishes MessageSent on success.
// Ordinary bot sends funnel through here so chat activity stays observable
// without coupling the send paths to any consumer.
func (b *Bot) sendMessage(ctx context.Context, params *t.SendMessageParams) (*t.Message, error) {
	message, err := b.api.SendMessage(ctx, params)
	if err != nil {
		return message, err
	}

	b.publishMessageSent(params.ChatID.ID, message.MessageID)

	return message, nil
}

// publishRichMessageSent publishes MessageSent for a successfully sent rich
// message. The rich pipeline bypasses sendMessage, so its call sites publish
// explicitly.
func (b *Bot) publishRichMessageSent(chatID int64, message *t.Message) {
	messageID := 0
	if message != nil {
		messageID = message.MessageID
	}
	b.publishMessageSent(chatID, messageID)
}

// publishMessageSent publishes the event; never fails, never blocks a send.
func (b *Bot) publishMessageSent(chatID int64, messageID int) {
	if b.events == nil {
		return
	}

	b.events.Publish(execution.MessageSent{
		RunContext: execution.RunContext{ChatID: chatID},
		At:         time.Now(),
		MessageID:  messageID,
	})
}

// publishIncomingMessage is an ingress middleware publishing MessageSent for
// every incoming channel message — bot-conversation text, commands, photos,
// chat-sharing events alike. Commands publish too: /admin reopens a live
// menu by editing it in place, so only the observation of the command itself
// can surface a menu buried under newer messages. Callback presses publish
// nothing (they are not messages): the admin pressing a button proves the
// menu is visible, and re-creating it would be churn. Group events are
// published as well; consumers filter by chat.
func (b *Bot) publishIncomingMessage(ctx *th.Context, update t.Update) error {
	if update.Message != nil {
		b.publishMessageSent(update.Message.Chat.ID, 0)
	}

	return ctx.Next(update)
}
