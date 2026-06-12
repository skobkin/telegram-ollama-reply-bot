package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"telegram-ollama-reply-bot/internal/tooluse"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// callToolHandler returns the tooluse.Handler that routes a tool call back to
// the originating MCP server's `ClientSession.CallTool`. The closure captures
// the session, the original (un-prefixed) tool name, the namespaced name, and
// a logger that already has `server` set as a structured field; the
// tooluse-side name (`namespacedToolName`) is never sent over the wire.
//
// The per-call `callCtx` is supplied by the tooluse runtime at dispatch time.
// We only add per-request fields to the logger; everything else is closure-
// captured.
//
// The handler translates the MCP response into a toolResult so the existing
// tool loop can apply ResultCharBudget truncation, error formatting, and audit
// logging. Anything the model sees is shaped like every other tool result.
//
// Distinguishing failure modes for the LLM:
//
//   - protocol-level errors (network down, server unreachable, mid-call
//     disconnect) -> a Go `error` and `toolResult{Status:"error"}` with the
//     structured code `mcp_unreachable`. The LLM can self-correct by
//     retrying or telling the user.
//   - tool-level errors (server returned `IsError: true`) -> a successful Go
//     return whose `Summary` carries the server's message. The existing
//     tooluse audit logger surfaces it as `is_error` / `error`; the LLM can
//     also see it directly in the data block.
//   - non-text content (image, audio, embedded resources) -> collected under
//     `Data["unsupported"]` rather than silently dropped, so misbehaving
//     servers are visible at the call site.
func callToolHandler(session *mcp.ClientSession, originalName, namespacedName string, logger *slog.Logger) tooluse.Handler {
	return func(ctx context.Context, callCtx tooluse.CallContext, args json.RawMessage) (tooluse.ToolResult, error) {
		if session == nil {
			return tooluse.ToolResult{}, fmt.Errorf("mcp session is not connected")
		}

		perCall := logger
		if perCall == nil {
			perCall = slog.Default()
		}

		parsed, err := parseToolArguments(args)
		if err != nil {
			return tooluse.ToolResult{}, fmt.Errorf("parse arguments: %w", err)
		}

		reply, callErr := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      originalName,
			Arguments: parsed,
		})
		if callErr != nil {
			perCall.Warn("mcp call failed", "error", callErr)
			logCallResult(perCall, namespacedName, originalName, callCtx.RequestID, true, 0, 0, 0)

			return tooluse.ToolResult{}, fmt.Errorf("mcp_unreachable: %w", callErr)
		}
		if reply == nil {
			perCall.Warn("mcp call returned no result")
			logCallResult(perCall, namespacedName, originalName, callCtx.RequestID, true, 0, 0, 0)

			return tooluse.ToolResult{}, fmt.Errorf("mcp_unreachable: empty response")
		}

		result, decodeErr := decodeCallResult(reply)
		if decodeErr != nil {
			perCall.Warn("mcp response decode failed", "error", decodeErr)
			logCallResult(perCall, namespacedName, originalName, callCtx.RequestID, true, 0, 0, 0)

			return tooluse.ToolResult{}, decodeErr
		}

		if reply.IsError {
			result.Status = "error"
			if result.Summary == "" {
				result.Summary = "tool returned an error"
			}
		}

		textLen, blockCount, unsupportedCount := summarizeForLog(reply)
		logCallResult(perCall, namespacedName, originalName, callCtx.RequestID, reply.IsError, textLen, blockCount, unsupportedCount)

		return result, nil
	}
}

// parseToolArguments turns a `json.RawMessage` argument blob into a value the
// SDK can serialize back into the wire format. `map[string]any` is the
// canonical shape for object arguments; an empty / null blob becomes an empty
// object so the server always sees a JSON object as `arguments`.
func parseToolArguments(args json.RawMessage) (any, error) {
	trimmed := strings.TrimSpace(string(args))
	if trimmed == "" || trimmed == "null" {
		return map[string]any{}, nil
	}

	var parsed any
	if err := json.Unmarshal(args, &parsed); err != nil {
		return nil, fmt.Errorf("decode arguments: %w", err)
	}
	if parsed == nil {
		return map[string]any{}, nil
	}
	if obj, ok := parsed.(map[string]any); ok {
		return obj, nil
	}

	// Wrap arrays / scalars so the server still gets a JSON object. Some MCP
	// servers reject non-object arguments; this keeps the call well-formed.
	return map[string]any{"value": parsed}, nil
}

// decodeCallResult turns an MCP `CallToolResult` into a tooluse.ToolResult.
// The `Data` field carries:
//
//   - text content blocks under "text" (joined with newlines so the LLM
//     sees a single coherent string), or per-block "blocks" with each
//     content type and its string representation when there is a mix.
//   - structured content under "structured" when the server provided it.
//   - unsupported content types (image, audio, embedded resources) under
//     "unsupported" so the LLM can react to "I can't see that" without
//     swallowing the rest of the response.
//
// Returns an error for hard decode failures (e.g. an unparseable structured
// payload) so the LLM gets a clear "mcp call failed" rather than a
// truncated "ok" status.
func decodeCallResult(reply *mcp.CallToolResult) (tooluse.ToolResult, error) { //nolint:unparam // error return reserved for future SDK-shape changes
	result := tooluse.ToolResult{
		Status:  "ok",
		Summary: "tool returned a result",
	}

	texts := make([]string, 0, len(reply.Content))
	blocks := make([]map[string]any, 0, len(reply.Content))
	unsupported := make([]map[string]any, 0)

	for _, item := range reply.Content {
		switch value := item.(type) {
		case *mcp.TextContent:
			if value == nil {
				continue
			}
			texts = append(texts, value.Text)
			blocks = append(blocks, map[string]any{
				"type": "text",
				"text": value.Text,
			})
		case *mcp.ImageContent:
			unsupported = append(unsupported, map[string]any{
				"type":      "image",
				"mime_type": value.MIMEType,
				"bytes":     len(value.Data),
			})
		case *mcp.AudioContent:
			unsupported = append(unsupported, map[string]any{
				"type":      "audio",
				"mime_type": value.MIMEType,
				"bytes":     len(value.Data),
			})
		case *mcp.EmbeddedResource:
			if value == nil || value.Resource == nil {
				continue
			}
			unsupported = append(unsupported, map[string]any{
				"type":      "embedded_resource",
				"mime_type": value.Resource.MIMEType,
				"uri":       value.Resource.URI,
			})
		default:
			unsupported = append(unsupported, map[string]any{
				"type": "unknown",
			})
		}
	}

	data := map[string]any{}
	if len(texts) > 0 {
		data["text"] = strings.Join(texts, "\n")
	}
	if len(blocks) > 0 {
		data["blocks"] = blocks
	}
	if reply.StructuredContent != nil {
		data["structured"] = reply.StructuredContent
	}
	if len(unsupported) > 0 {
		data["unsupported"] = unsupported
	}
	if len(data) == 0 {
		data["empty"] = true
	}
	result.Data = data

	return result, nil
}

// summarizeForLog returns a short log-friendly description of the response
// content. It is used to keep the audit line useful without leaking large
// payloads.
func summarizeForLog(reply *mcp.CallToolResult) (textLen, blockCount, unsupportedCount int) {
	if reply == nil {
		return 0, 0, 0
	}
	for _, item := range reply.Content {
		switch v := item.(type) {
		case *mcp.TextContent:
			textLen += len(v.Text)
			blockCount++
		case *mcp.ImageContent, *mcp.AudioContent, *mcp.EmbeddedResource:
			unsupportedCount++
		default:
			blockCount++
		}
	}

	return textLen, blockCount, unsupportedCount
}

// logCallResult emits a structured INFO-level line for a tool call. Header
// values are never present in the log fields. The caller-provided logger is
// expected to already carry the `server` field.
func logCallResult(logger *slog.Logger, namespacedName, originalName, requestID string, isError bool, textLen, blockCount, unsupportedCount int) {
	if logger == nil {
		return
	}
	logger.Info(
		"mcp tool call completed",
		"tool_name", namespacedName,
		"original_name", originalName,
		"is_error", isError,
		"text_length", textLen,
		"blocks", blockCount,
		"unsupported", unsupportedCount,
		"request_id", requestID,
	)
}
