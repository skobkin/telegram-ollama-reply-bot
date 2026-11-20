package tooluse

import (
	"context"
	"encoding/json"
	"errors"

	"telegram-ollama-reply-bot/internal/provider"
	"telegram-ollama-reply-bot/internal/reminders"

	"github.com/mymmrac/telego/telegoapi"
)

// ErrorClass is safe, content-free diagnostic metadata, shared by records and events.
type ErrorClass = string

const (
	ErrorArguments   = "invalid_arguments"
	ErrorNotFound    = "not_found"
	ErrorUnavailable = "unavailable"
	ErrorRateLimited = "rate_limited"
	ErrorPermanent   = "permanent"
	ErrorTransient   = "transient"
	ErrorCancelled   = "cancelled"
	ErrorAmbiguous   = "ambiguous_outcome"
)

// ToolError contains only model-safe explanations; it never wraps a raw cause.
type ToolError struct {
	Code              string     `json:"code"`
	Class             ErrorClass `json:"class"`
	Message           string     `json:"message"`
	Retryable         bool       `json:"retryable"`
	RecoveryHint      string     `json:"recovery_hint,omitempty"`
	RetryAfterSeconds int        `json:"retry_after_seconds,omitempty"`
}

func (e *ToolError) Error() string { return e.Message }

func NewToolError(code, class, message string, retryable bool, hint string) *ToolError {
	return &ToolError{Code: code, Class: class, Message: message, Retryable: retryable, RecoveryHint: hint}
}

func ErrorResult(err *ToolError) ToolResult { return ToolResult{Status: "error", Error: err} }

func invalidArguments(message string) error {
	return NewToolError("invalid_arguments", ErrorArguments, message, true, "Correct the arguments and try again.")
}

// ClassifyError discards raw error text. sideEffecting means dispatch may already
// have applied an action; unknown transport failures must not invite replay.
func ClassifyError(err error, sideEffecting bool) *ToolError {
	var typed *ToolError
	if errors.As(err, &typed) {
		return typed
	}
	var reminderInput *reminders.ValidationError
	if errors.As(err, &reminderInput) {
		return NewToolError("invalid_arguments", ErrorArguments, reminderInput.Message, true, "Correct the selected schedule fields before retrying.")
	}
	var syntax *json.SyntaxError
	var value *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax), errors.As(err, &value):
		return NewToolError("invalid_arguments", ErrorArguments, "Arguments must match the tool's JSON schema.", true, "Correct the JSON object and try again.")
	case errors.Is(err, errDecisionInvalidArguments):
		return NewToolError("invalid_arguments", ErrorArguments, err.Error(), true, "Correct the decision arguments and try again.")
	case errors.Is(err, errDecisionRequestTooLarge):
		return NewToolError("request_too_large", ErrorArguments, "Decision input exceeds the request budget.", true, "Shorten the state, instructions or question batch.")
	case errors.Is(err, errDecisionUnavailable):
		return NewToolError("tool_unavailable", ErrorUnavailable, "The decision backend is unavailable for this turn.", false, "")
	case errors.Is(err, errDecisionBackendFailed):
		return NewToolError("decision_unavailable", ErrorTransient, "The decision backend could not complete the request.", true, "")
	case errors.Is(err, errDecisionInvalidResponse):
		return NewToolError("invalid_response", ErrorTransient, "The decision backend returned an invalid response.", true, "")
	case errors.Is(err, errDecisionResultTooLarge):
		return NewToolError("result_too_large", ErrorArguments, "The decision result exceeds its budget.", true, "Use fewer options or questions.")
	case errors.Is(err, reminders.ErrReminderNotFound):
		return NewToolError("reminder_not_found", ErrorNotFound, "The reminder no longer exists.", true, "Use list_chat_schedule to obtain a current reminder ID.")
	case errors.Is(err, reminders.ErrReminderForbidden):
		return NewToolError("reminder_forbidden", ErrorPermanent, "The requester cannot manage this reminder.", false, "")
	}
	var api *telegoapi.Error
	if errors.As(err, &api) {
		if api.ErrorCode == 429 {
			result := NewToolError("rate_limited", ErrorRateLimited, "Telegram rate-limited this action.", true, "Wait until the retry delay has passed; do not retry immediately.")
			if api.Parameters != nil {
				result.RetryAfterSeconds = api.Parameters.RetryAfter
			}
			return result
		}
		if api.ErrorCode >= 400 && api.ErrorCode < 500 {
			return NewToolError("telegram_rejected", ErrorPermanent, "Telegram rejected this action.", false, "Check the target and the bot's permissions.")
		}
	}
	if sideEffecting {
		return NewToolError("outcome_unknown", ErrorAmbiguous, "The action was not confirmed; it may already have been applied.", false, "Do not repeat the action without verifying its outcome.")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return NewToolError("cancelled", ErrorCancelled, "The call was cancelled or its deadline elapsed.", false, "")
	}
	switch provider.KindOf(err) {
	case provider.ErrorKindRateLimited:
		return NewToolError("rate_limited", ErrorRateLimited, "The provider rate-limited the call.", true, "Wait before trying again.")
	case provider.ErrorKindAuthFailed, provider.ErrorKindCreditsExhausted, provider.ErrorKindMisconfigured, provider.ErrorKindUnsupported:
		return NewToolError("provider_unavailable", ErrorPermanent, "The provider cannot perform this call with its current configuration.", false, "")
	case provider.ErrorKindUpstreamUnavailable:
		return NewToolError("provider_unavailable", ErrorTransient, "The provider is temporarily unavailable.", true, "")
	default:
		return NewToolError("execution_failed", ErrorPermanent, "The tool could not complete the call.", false, "Use another available method or report the failure.")
	}
}

// normalizeResult lifts legacy handler payloads at the shared boundary. This
// also keeps third-party adapters from bypassing the model-facing contract.
func normalizeResult(result toolResult) toolResult {
	if result.Error != nil {
		e := *result.Error
		if len([]rune(e.Message)) > 160 || len([]rune(e.RecoveryHint)) > 128 {
			result.Truncated = true
		}
		e.Message = truncateUTF8(e.Message, 160)
		e.RecoveryHint = truncateUTF8(e.RecoveryHint, 128)
		result.Error = &e
	}
	if result.Status != "error" {
		return result
	}
	if result.Error == nil {
		if data, ok := result.Data.(map[string]any); ok {
			var payload *ToolError
			switch v := data["error"].(type) {
			case toolErrorPayload:
				payload = &v
			case map[string]any:
				code, _ := v["code"].(string)
				message, _ := v["message"].(string)
				payload = &ToolError{Code: code, Message: message}
			}
			if payload != nil {
				copyData := make(map[string]any, len(data))
				for k, v := range data {
					if k != "error" {
						copyData[k] = v
					}
				}
				result.Data = copyData
				result.Error = payload
			}
		}
	}
	if result.Error == nil {
		result.Error = NewToolError("execution_failed", ErrorPermanent, "The tool reported a failure.", false, "")
	}
	if result.Error.Class == "" {
		e := *result.Error
		switch e.Code {
		case "missing_required_field", "invalid_timestamp", "invalid_timezone", "invalid_operation", "invalid_style", "empty_shift", "invalid_match_mode", "prompt_invalid", "invalid_arguments", "invalid_emoji":
			e.Class = ErrorArguments
			e.Retryable = true
			e.RecoveryHint = "Correct the arguments using the tool schema and try again."
		case "image_not_found":
			e.Class = ErrorNotFound
			e.Retryable = true
			e.RecoveryHint = "Use list_recent_images to find an available message_id."
		case "rate_limited":
			e.Class = ErrorRateLimited
			e.RecoveryHint = "Wait before trying again."
		case "cancelled":
			e.Class = ErrorCancelled
		case "request_failed":
			e.Class = ErrorAmbiguous
		default:
			e.Class = ErrorPermanent
		}
		result.Error = &e
	}
	if result.Error != nil {
		e := *result.Error
		if len([]rune(e.Message)) > 160 || len([]rune(e.RecoveryHint)) > 128 {
			result.Truncated = true
		}
		e.Message = truncateUTF8(e.Message, 160)
		e.RecoveryHint = truncateUTF8(e.RecoveryHint, 128)
		result.Error = &e
	}
	return result
}
