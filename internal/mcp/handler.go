package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"telegram-ollama-reply-bot/internal/tooluse"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// callToolHandler returns the tooluse.Handler that routes a tool call back to
// the originating MCP server's `ClientSession.CallTool`. The closure captures
// the session, the original (un-prefixed) tool name, the namespaced name, the
// per-server call timeout, and a logger that already has `server` set as a
// structured field; the tooluse-side name (`namespacedToolName`) is never sent
// over the wire.
//
// The per-call `ctx` is supplied by the tooluse runtime at dispatch time. Each
// invocation is additionally bounded by `callTimeout` (the per-server
// configured timeout), because the underlying HTTP client deliberately has no
// wall-clock timeout: it must stay free to hold the standing SSE stream open.
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
//     disconnect, per-call timeout exceeded) -> a Go `error` and
//     `toolResult{Status:"error"}` with the structured code `mcp_unreachable`.
//     The LLM can self-correct by retrying or telling the user.
//   - tool-level errors (server returned `IsError: true`) -> a successful Go
//     return whose `Summary` carries the server's message. The existing
//     tooluse audit logger surfaces it as `is_error` / `error`; the LLM can
//     also see it directly in the data block.
//   - non-text content (image, audio, embedded resources) -> collected under
//     `Data["blocks"]` rather than silently dropped, so misbehaving servers
//     are visible at the call site.
func callToolHandler(session *mcp.ClientSession, originalName, namespacedName string, callTimeout time.Duration, logger *slog.Logger) tooluse.Handler {
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

		callCtxTimeout, cancel := context.WithTimeout(ctx, callTimeout)
		defer cancel()

		reply, callErr := session.CallTool(callCtxTimeout, &mcp.CallToolParams{
			Name:      originalName,
			Arguments: parsed,
		})
		if callErr != nil {
			// Transport errors quote the full request URL, which may carry
			// credentials as query parameters. Redact before logging and
			// before the text is copied into the tool response the LLM sees.
			sanitized := redactURLCredentials(callErr.Error())
			perCall.Warn("mcp call failed", "error", sanitized)
			logCallResult(perCall, namespacedName, originalName, callCtx.RequestID, true, 0, 0, 0)

			return tooluse.ToolResult{}, fmt.Errorf("mcp_unreachable: %s", sanitized)
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

		textLen, blockCount, unsupportedCount := summarizeForLog(reply)
		logCallResult(perCall, namespacedName, originalName, callCtx.RequestID, reply.IsError, textLen, blockCount, unsupportedCount)

		return result, nil
	}
}

// parseToolArguments prepares the model's argument JSON for the wire. Object
// arguments are passed through as raw bytes: they end up in
// `CallToolParams.Arguments` as a `json.RawMessage`, which the SDK embeds
// verbatim when it encodes the JSON-RPC request. Decoding into `any` and
// letting the SDK re-marshal would turn every number into a float64 and
// silently corrupt integers above 2^53, so the payload is never re-encoded.
//
// An empty / null blob becomes an empty object so the server always sees a
// JSON object as `arguments`. Non-object arguments (arrays / scalars) are
// wrapped as {"value": ...} because some MCP servers reject non-object
// arguments; wrapping keeps the call well-formed while still not
// re-encoding the wrapped payload.
func parseToolArguments(args json.RawMessage) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(string(args))
	if trimmed == "" || trimmed == "null" {
		return json.RawMessage("{}"), nil
	}

	if !json.Valid([]byte(trimmed)) {
		return nil, fmt.Errorf("decode arguments: invalid JSON")
	}
	if trimmed[0] == '{' {
		return json.RawMessage(trimmed), nil
	}

	wrapped, err := json.Marshal(struct {
		Value json.RawMessage `json:"value"`
	}{Value: json.RawMessage(trimmed)})
	if err != nil {
		return nil, fmt.Errorf("wrap arguments: %w", err)
	}

	return wrapped, nil
}

// decodeCallResult turns an MCP `CallToolResult` into a tooluse.ToolResult.
// Error results (`IsError: true`) become Status "error" with `Summary`
// carrying the server's own message (its text content, joined with newlines),
// or "tool returned an error" when the server sent no text at all.
//
// The `Data` field carries each piece of content exactly once:
//
//   - text content blocks under "text" (joined with newlines so the LLM
//     sees a single coherent string). Text is never duplicated per block.
//   - non-text content types (image, audio, embedded resources) under
//     "blocks", present only when such content exists, so the LLM can
//     react to "I can't see that" without swallowing the rest of the
//     response.
//   - structured content under "structured" when the server provided it.
//
// Returns an error for hard decode failures (e.g. an unparseable structured
// payload) so the LLM gets a clear "mcp call failed" rather than a
// truncated "ok" status.
func decodeCallResult(reply *mcp.CallToolResult) (tooluse.ToolResult, error) { //nolint:unparam // error return reserved for future SDK-shape changes
	result := tooluse.ToolResult{Status: "ok"}

	texts := make([]string, 0, len(reply.Content))
	blocks := make([]map[string]any, 0)

	for _, item := range reply.Content {
		switch value := item.(type) {
		case *mcp.TextContent:
			if value == nil {
				continue
			}
			texts = append(texts, value.Text)
		case *mcp.ImageContent:
			blocks = append(blocks, map[string]any{
				"type":      "image",
				"mime_type": value.MIMEType,
				"bytes":     len(value.Data),
			})
		case *mcp.AudioContent:
			blocks = append(blocks, map[string]any{
				"type":      "audio",
				"mime_type": value.MIMEType,
				"bytes":     len(value.Data),
			})
		case *mcp.EmbeddedResource:
			if value == nil || value.Resource == nil {
				continue
			}
			blocks = append(blocks, map[string]any{
				"type":      "embedded_resource",
				"mime_type": value.Resource.MIMEType,
				"uri":       value.Resource.URI,
			})
		default:
			blocks = append(blocks, map[string]any{
				"type": "unknown",
			})
		}
	}

	if reply.IsError {
		result.Status = "error"
		if summary := strings.Join(texts, "\n"); summary != "" {
			result.Summary = summary
		} else {
			result.Summary = "tool returned an error"
		}
	} else {
		result.Summary = "tool returned a result"
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
