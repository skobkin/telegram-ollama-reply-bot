package app

import (
	"context"
	"time"

	"telegram-ollama-reply-bot/internal/execution"

	tg "github.com/mymmrac/telego"
)

// eventPublishingBot wraps the Telegram API client for the senders whose
// messages can land in a chat the bot serves but which bypass the bot
// package's own send paths: the reminder scheduler (deliveries into admin
// DMs are always allowed) and the model-authored poll/quiz/dice actions.
// Every successful send publishes the MessageSent interaction event so
// chat-activity consumers (the admin UI menu re-creation) observe them like
// any other message. Publication is best-effort: errors and nil dispatchers
// never affect the send itself.
//
// The wrapper satisfies the narrow sender interfaces its consumers use
// (reminders.Sender, tooluse.TelegramActionSender); it is not a general
// facade over the API client.
type eventPublishingBot struct {
	api    *tg.Bot
	events execution.Dispatcher
}

func (b *eventPublishingBot) publish(chatID int64, messageID int) {
	if b.events == nil {
		return
	}

	b.events.Publish(execution.MessageSent{
		RunContext: execution.RunContext{ChatID: chatID},
		At:         time.Now(),
		MessageID:  messageID,
	})
}

func (b *eventPublishingBot) SendMessage(ctx context.Context, params *tg.SendMessageParams) (*tg.Message, error) {
	message, err := b.api.SendMessage(ctx, params)
	if err != nil {
		return message, err
	}
	b.publish(params.ChatID.ID, message.MessageID)

	return message, nil
}

func (b *eventPublishingBot) SendPoll(ctx context.Context, params *tg.SendPollParams) (*tg.Message, error) {
	message, err := b.api.SendPoll(ctx, params)
	if err != nil {
		return message, err
	}
	b.publish(params.ChatID.ID, message.MessageID)

	return message, nil
}

func (b *eventPublishingBot) SendDice(ctx context.Context, params *tg.SendDiceParams) (*tg.Message, error) {
	message, err := b.api.SendDice(ctx, params)
	if err != nil {
		return message, err
	}
	b.publish(params.ChatID.ID, message.MessageID)

	return message, nil
}

func (b *eventPublishingBot) SendPhoto(ctx context.Context, params *tg.SendPhotoParams) (*tg.Message, error) {
	message, err := b.api.SendPhoto(ctx, params)
	if err != nil {
		return message, err
	}
	b.publish(params.ChatID.ID, message.MessageID)

	return message, nil
}

// SetMessageReaction sets an outgoing bot reaction. It creates no message,
// so no MessageSent is published here; the tool runtime publishes the
// run-correlated BotReactionSet event itself after the reaction and the
// history write are confirmed.
func (b *eventPublishingBot) SetMessageReaction(ctx context.Context, params *tg.SetMessageReactionParams) error {
	return b.api.SetMessageReaction(ctx, params)
}
