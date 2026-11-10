package tooluse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/vision"
)

const (
	defaultRecentImagesResultLimit = 5
	maxRecentImagesResultLimit     = 10

	recentImagesDescriptionCharLimit = 200
	recentImagesTextCharLimit        = 180
	analyzeImageAnalysisCharLimit    = 2200
	analyzeImagePromptMaxChars       = 4000
)

// Stable error codes surfaced to the model by the image tools.
const (
	analyzeErrorCodeImageNotFound     = "image_not_found"
	analyzeErrorCodePromptInvalid     = "prompt_invalid"
	analyzeErrorCodeImageTooLarge     = "image_too_large"
	analyzeErrorCodeUnsupportedType   = "unsupported_image_type"
	analyzeErrorCodeRecognitionFailed = "recognition_failed"
)

// listRecentImages lists recent image-bearing messages in the current
// chat/topic so the model can pick the right message_id before a paid
// analysis. It is strictly local: no cache wiring, no network fetch, no
// recognition.
func (r *Runtime) listRecentImages(_ context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var payload struct {
		Limit int `json:"limit"`
	}
	if err := json.Unmarshal(args, &payload); err != nil && string(args) != "" && string(args) != "null" {
		return toolResult{}, fmt.Errorf("parse arguments: %w", err)
	}
	if payload.Limit <= 0 {
		payload.Limit = defaultRecentImagesResultLimit
	}
	if payload.Limit > maxRecentImagesResultLimit {
		payload.Limit = maxRecentImagesResultLimit
	}

	snapshot := r.history.Snapshot(callCtx.Scope)
	candidates := scopeMessageCandidates(callCtx.Scope, callCtx.Requester, snapshot.Messages)

	images := make([]map[string]any, 0, payload.Limit)
	for _, msg := range candidates {
		if len(images) >= payload.Limit {
			break
		}
		if msg.MessageID == 0 || !msg.HasImage {
			continue
		}

		entry := map[string]any{
			"message_id":          msg.MessageID,
			"reply_to_message_id": replyToMessageID(msg),
			"from_id":             msg.FromID,
			"name":                msg.Name,
			"username":            msg.Username,
			"created_at":          formatToolTime(msg.CreatedAt),
			"has_description":     msg.Image != "",
		}
		// The description fields mirror the stored message only: hydration
		// fills cloned prompt copies and caches text in the image cache, so
		// the stored text is often empty — has_description:false is normal
		// and does not mean the image was never described.
		if msg.Image != "" {
			entry["description_preview"] = truncateUTF8(normalizeWhitespace(msg.Image), recentImagesDescriptionCharLimit)
		}
		if text := normalizeWhitespace(msg.Text); text != "" {
			entry["text_preview"] = truncateUTF8(text, recentImagesTextCharLimit)
		}

		images = append(images, entry)
	}

	if len(images) == 0 {
		result := toolResult{
			Status:  "empty",
			Summary: "No image-bearing messages were found in the current chat/topic history",
			Data: map[string]any{
				"chat_id":  callCtx.Scope.ChatID,
				"topic_id": callCtx.Scope.TopicID,
			},
		}
		logToolResult(callCtx, result, "images_count", 0)

		return result, nil
	}

	result := toolResult{
		Status:  "ok",
		Summary: fmt.Sprintf("Found %d recent image-bearing message(s)", len(images)),
		Data: map[string]any{
			"chat_id":  callCtx.Scope.ChatID,
			"topic_id": callCtx.Scope.TopicID,
			"images":   images,
		},
	}
	logToolResult(callCtx, result, "images_count", len(images))

	return result, nil
}

// analyzeImage re-analyzes a retained image under the model's own prompt via
// the attached ImageAnalyzer. Resolution accepts only scope-validated
// message identities; file IDs, URLs, and paths are never accepted from the
// model. Each invocation performs exactly one recognition request.
func (r *Runtime) analyzeImage(ctx context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var payload struct {
		MessageID int    `json:"message_id"`
		Prompt    string `json:"prompt"`
	}
	if !decodeToolJSON(args, &payload) {
		return toolResult{}, errors.New("invalid arguments")
	}
	if payload.MessageID < 1 {
		return toolResult{}, errors.New("message_id must be a positive integer")
	}

	prompt := strings.TrimSpace(payload.Prompt)
	if prompt == "" || utf8.RuneCountInString(prompt) > analyzeImagePromptMaxChars {
		result := imageAnalysisErrorResult(callCtx, analyzeErrorCodePromptInvalid,
			"prompt must be non-empty and at most 4000 characters", payload.MessageID)
		logToolResult(callCtx, result, "message_id", payload.MessageID)

		return result, nil
	}

	snapshot := r.history.Snapshot(callCtx.Scope)
	meta := resolveScopeImage(callCtx.Scope, callCtx.Requester, snapshot.Messages, payload.MessageID)
	if meta == nil {
		// Deliberately indistinguishable for never-existed, evicted,
		// another chat/topic, and text-only message ids — no probing.
		result := toolResult{
			Status:  "error",
			Summary: "No image is available for that message in the current chat/topic",
			Data: map[string]any{
				"chat_id":    callCtx.Scope.ChatID,
				"topic_id":   callCtx.Scope.TopicID,
				"message_id": payload.MessageID,
				"error": toolErrorPayload{
					Code:    analyzeErrorCodeImageNotFound,
					Message: "No image is available for that message in the current chat/topic. Use list_recent_images to find image-bearing message_ids.",
				},
			},
		}
		logToolResult(callCtx, result, "message_id", payload.MessageID)

		return result, nil
	}

	analysis, err := r.imageAnalyzer.AnalyzeImage(ctx, *meta, prompt)
	if err != nil {
		result := imageAnalysisErrorResult(callCtx, analyzeErrorCode(err), err.Error(), payload.MessageID)
		logToolResult(callCtx, result, "message_id", payload.MessageID, "analysis_length", 0)

		return result, nil
	}

	result := toolResult{
		Status:  "ok",
		Summary: "Image analyzed under the requested prompt",
		Data: map[string]any{
			"chat_id":    callCtx.Scope.ChatID,
			"topic_id":   callCtx.Scope.TopicID,
			"message_id": payload.MessageID,
			"analysis":   truncateUTF8(analysis, analyzeImageAnalysisCharLimit),
			"note":       "fresh analysis under your prompt; the cached baseline description is unchanged",
		},
	}
	logToolResult(callCtx, result, "message_id", payload.MessageID, "analysis_length", len(analysis))

	return result, nil
}

// messageInScope enforces the scope-equality rule for every image-tool
// candidate: ChatID and TopicID must match exactly (topic 0 is an exact
// value, never a wildcard).
func messageInScope(msg state.Message, scope state.ConversationScope) bool {
	return msg.ChatID == scope.ChatID && msg.TopicID == scope.TopicID
}

// scopeMessageCandidates returns messages in image-tool resolution order —
// the requester, then its recursive reply chain innermost-first, then the
// current-scope snapshot newest-first — keeping only scope-validated,
// deduplicated (by message_id) nodes. A scope-mismatched reply-chain node is
// skipped and stops the traversal (its ancestors sit outside the current
// scope's context); the chain is bounded by messageThreadChainLimit with
// visited-id cycle protection.
func scopeMessageCandidates(scope state.ConversationScope, requester state.Message, snapshot []state.Message) []state.Message {
	result := make([]state.Message, 0, len(snapshot)+messageThreadChainLimit+1)
	seen := make(map[int]struct{}, len(snapshot)+messageThreadChainLimit+1)

	add := func(msg state.Message) bool {
		if msg.MessageID != 0 {
			if _, ok := seen[msg.MessageID]; ok {
				return false
			}
			seen[msg.MessageID] = struct{}{}
		}
		result = append(result, msg)

		return true
	}

	if messageInScope(requester, scope) {
		add(requester)

		node := requester.ReplyTo
		for depth := 0; node != nil && depth < messageThreadChainLimit; depth++ {
			if !messageInScope(*node, scope) {
				break
			}
			if !add(*node) {
				break
			}
			node = node.ReplyTo
		}
	}

	for i := len(snapshot) - 1; i >= 0; i-- {
		add(snapshot[i])
	}

	return result
}

// resolveScopeImage finds the retained image metadata for messageID using
// only scope-validated candidates. A nil result is definitive and carries no
// information about why: never-existed, evicted, another chat/topic, and
// text-only message ids are indistinguishable.
func resolveScopeImage(scope state.ConversationScope, requester state.Message, snapshot []state.Message, messageID int) *state.ImageMeta {
	for _, msg := range scopeMessageCandidates(scope, requester, snapshot) {
		if msg.MessageID != messageID {
			continue
		}

		// Identity matched a scope-validated message: it is the definitive
		// answer even when it carries no image.
		if msg.HasImage && msg.ImageMeta != nil {
			meta := *msg.ImageMeta

			return &meta
		}

		return nil
	}

	return nil
}

// analyzeErrorCode maps analyzer failures to stable tool error codes. The
// analyzer's error chains are already secret-free (classified and discarded
// at the bot boundary), so their text can surface to the model unchanged.
func analyzeErrorCode(err error) string {
	switch {
	case errors.Is(err, vision.ErrImageTooLarge):
		return analyzeErrorCodeImageTooLarge
	case errors.Is(err, vision.ErrUnsupportedImageType):
		return analyzeErrorCodeUnsupportedType
	default:
		return analyzeErrorCodeRecognitionFailed
	}
}

func imageAnalysisErrorResult(callCtx CallContext, code, message string, messageID int) toolResult {
	data := map[string]any{
		"chat_id":    callCtx.Scope.ChatID,
		"topic_id":   callCtx.Scope.TopicID,
		"message_id": messageID,
		"error": toolErrorPayload{
			Code:    code,
			Message: message,
		},
	}

	return toolResult{
		Status:  "error",
		Summary: "Image analysis failed",
		Data:    data,
	}
}

func formatToolTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}

	return value.UTC().Format(time.RFC3339)
}
