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

func (r *Runtime) sendQuiz(ctx context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var payload struct {
		Question           string   `json:"question"`
		Options            []string `json:"options"`
		CorrectOptionIndex int      `json:"correct_option_index"`
		Explanation        string   `json:"explanation"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return toolResult{}, fmt.Errorf("parse arguments: %w", err)
	}

	options, err := validatePollPayload(payload.Question, payload.Options)
	if err != nil {
		return toolResult{}, err
	}
	if payload.CorrectOptionIndex < 0 || payload.CorrectOptionIndex >= len(options) {
		return toolResult{}, errors.New("correct_option_index must reference an existing option")
	}

	params := &t.SendPollParams{
		ChatID:           tu.ID(callCtx.Scope.ChatID),
		Question:         strings.TrimSpace(payload.Question),
		Options:          options,
		Type:             "quiz",
		CorrectOptionIDs: []int{payload.CorrectOptionIndex},
	}
	if explanation := strings.TrimSpace(payload.Explanation); explanation != "" {
		params.Explanation = explanation
	}
	if callCtx.Scope.TopicID != 0 {
		params.MessageThreadID = callCtx.Scope.TopicID
	}

	message, err := r.actions.SendPoll(ctx, params)
	if err != nil {
		return toolResult{}, err
	}

	data := map[string]any{
		"chat_id":              callCtx.Scope.ChatID,
		"topic_id":             callCtx.Scope.TopicID,
		"message_id":           message.MessageID,
		"question":             strings.TrimSpace(payload.Question),
		"options":              trimmedOptionTexts(options),
		"correct_option_index": payload.CorrectOptionIndex,
	}
	if params.Explanation != "" {
		data["explanation"] = params.Explanation
	}

	result := toolResult{
		Status:  "ok",
		Summary: "Quiz sent",
		Data:    data,
	}
	logToolResult(callCtx, result, "message_id", message.MessageID, "options_count", len(options), "correct_option_index", payload.CorrectOptionIndex)

	return result, nil
}
