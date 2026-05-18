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

func (r *Runtime) sendPoll(ctx context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var payload struct {
		Question              string   `json:"question"`
		Options               []string `json:"options"`
		AllowsMultipleAnswers bool     `json:"allows_multiple_answers"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return toolResult{}, fmt.Errorf("parse arguments: %w", err)
	}
	options, err := validatePollPayload(payload.Question, payload.Options)
	if err != nil {
		return toolResult{}, err
	}

	params := &t.SendPollParams{
		ChatID:                tu.ID(callCtx.Scope.ChatID),
		Question:              strings.TrimSpace(payload.Question),
		Options:               options,
		Type:                  "regular",
		AllowsMultipleAnswers: payload.AllowsMultipleAnswers,
	}
	if callCtx.Scope.TopicID != 0 {
		params.MessageThreadID = callCtx.Scope.TopicID
	}

	message, err := r.actions.SendPoll(ctx, params)
	if err != nil {
		return toolResult{}, err
	}

	result := toolResult{
		Status:  "ok",
		Summary: "Poll sent",
		Data: map[string]any{
			"chat_id":    callCtx.Scope.ChatID,
			"topic_id":   callCtx.Scope.TopicID,
			"message_id": message.MessageID,
			"question":   strings.TrimSpace(payload.Question),
			"options":    trimmedOptionTexts(options),
		},
	}
	logToolResult(callCtx, result, "message_id", message.MessageID, "options_count", len(options))

	return result, nil
}

func validatePollPayload(question string, rawOptions []string) ([]t.InputPollOption, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return nil, errors.New("question is required")
	}
	if len(rawOptions) < 2 || len(rawOptions) > 10 {
		return nil, errors.New("options must contain 2 to 10 entries")
	}

	options := make([]t.InputPollOption, 0, len(rawOptions))
	for _, option := range rawOptions {
		option = strings.TrimSpace(option)
		if option == "" {
			return nil, errors.New("poll options must be non-empty")
		}

		options = append(options, t.InputPollOption{Text: option})
	}

	return options, nil
}

func trimmedOptionTexts(options []t.InputPollOption) []string {
	result := make([]string, 0, len(options))
	for _, option := range options {
		result = append(result, option.Text)
	}

	return result
}
