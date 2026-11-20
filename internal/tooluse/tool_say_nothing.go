package tooluse

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"telegram-ollama-reply-bot/internal/execution"
)

func sayNothingHandler(ctx context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	args = bytes.TrimSpace(args)
	if len(args) != 0 && !bytes.Equal(args, []byte("null")) {
		var fields map[string]json.RawMessage
		valid := json.Unmarshal(args, &fields) == nil && fields != nil && len(fields) == 0
		if !valid {
			return toolResult{Status: execution.StatusError, Error: "say_nothing takes no arguments; provide an empty JSON object."}, nil
		}
	}

	// This check is the acceptance boundary. A later cancellation must not
	// turn an already accepted intentional completion into an error notice.
	if err := toolContextError(ctx); err != nil {
		return toolResult{}, err
	}

	result := toolResult{Status: execution.StatusOK, endTurn: true}
	logToolResult(callCtx, result)

	return result, nil
}

// Transports may observe a deadline before its context timer fires.
func toolContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}

	return nil
}
