package mcp

import (
	"context"
	"log/slog"

	"telegram-ollama-reply-bot/internal/config"
)

// NewFromConfig is the application-layer entry point. It mirrors the
// `content/search.NewFromConfig` shape (config + logger -> wired
// dependency) and is what `app.Run` calls.
//
// Pass `ctx` as the bootstrap context; it bounds the initial Connect +
// ListTools handshake per server. A misconfigured non-optional server is
// returned as an error so the application can fail fast at startup.
func NewFromConfig(ctx context.Context, cfg config.MCPConfig, logger *slog.Logger) (*Manager, error) {
	return NewManager(ctx, cfg, logger)
}
