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

func (r *Runtime) createPoll(ctx context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var payload struct {
		Question              string   `json:"question"`
		Options               []string `json:"options"`
		AllowsMultipleAnswers bool     `json:"allows_multiple_answers"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return toolResult{}, fmt.Errorf("parse arguments: %w", err)
	}
	payload.Question = strings.TrimSpace(payload.Question)
	if payload.Question == "" {
		return toolResult{}, errors.New("question is required")
	}
	if len(payload.Options) < 2 || len(payload.Options) > 10 {
		return toolResult{}, errors.New("options must contain 2 to 10 entries")
	}

	options := make([]t.InputPollOption, 0, len(payload.Options))
	for _, option := range payload.Options {
		option = strings.TrimSpace(option)
		if option == "" {
			return toolResult{}, errors.New("poll options must be non-empty")
		}
		options = append(options, t.InputPollOption{Text: option})
	}

	params := &t.SendPollParams{
		ChatID:                tu.ID(callCtx.Scope.ChatID),
		Question:              payload.Question,
		Options:               options,
		Type:                  "regular",
		AllowsMultipleAnswers: payload.AllowsMultipleAnswers,
	}
	if callCtx.Scope.TopicID != 0 {
		params.MessageThreadID = callCtx.Scope.TopicID
	}

	message, err := r.polls.SendPoll(ctx, params)
	if err != nil {
		return toolResult{}, err
	}

	return toolResult{
		Status:  "ok",
		Summary: "Poll created",
		Data: map[string]any{
			"chat_id":    callCtx.Scope.ChatID,
			"topic_id":   callCtx.Scope.TopicID,
			"message_id": message.MessageID,
			"question":   payload.Question,
			"options":    payload.Options,
		},
	}, nil
}
