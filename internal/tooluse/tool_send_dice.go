package tooluse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	t "github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

var validDiceEmojis = map[string]struct{}{
	"🎲": {},
	"🎯": {},
	"🏀": {},
	"⚽": {},
	"🎳": {},
	"🎰": {},
}

func (r *Runtime) sendDice(ctx context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var payload struct {
		Emoji string `json:"emoji"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return toolResult{}, fmt.Errorf("parse arguments: %w", err)
	}

	params := &t.SendDiceParams{
		ChatID: tu.ID(callCtx.Scope.ChatID),
	}
	if emoji := strings.TrimSpace(payload.Emoji); emoji != "" {
		if _, ok := validDiceEmojis[emoji]; !ok {
			return toolResult{}, errors.New("emoji must be one of 🎲, 🎯, 🏀, ⚽, 🎳, or 🎰")
		}

		params.Emoji = emoji
	}
	if callCtx.Scope.TopicID != 0 {
		params.MessageThreadID = callCtx.Scope.TopicID
	}

	message, err := r.actions.SendDice(ctx, params)
	if err != nil {
		return toolResult{}, err
	}

	data := map[string]any{
		"chat_id":    callCtx.Scope.ChatID,
		"topic_id":   callCtx.Scope.TopicID,
		"message_id": message.MessageID,
		"emoji":      params.Emoji,
	}
	if message.Dice != nil {
		data["emoji"] = message.Dice.Emoji
		data["value"] = message.Dice.Value
	}

	result := toolResult{
		Status:  "ok",
		Summary: "Dice sent",
		Data:    data,
	}
	logToolResult(callCtx, result, "message_id", message.MessageID, "emoji", data["emoji"], "value", data["value"])

	return result, nil
}
