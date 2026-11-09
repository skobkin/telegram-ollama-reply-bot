package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"telegram-ollama-reply-bot/internal/app"
)

func main() {
	// The container runs the bot as PID 1 (exec-form ENTRYPOINT), so
	// `docker stop` delivers SIGTERM to this process. Without a handler
	// the Go runtime's default disposition kills it immediately and the
	// deferred cleanup inside app.Run (MCP session teardown, persistent
	// store close, Sentry flush) never executes. Listen for SIGTERM and
	// SIGINT and cancel the root context instead; app.Run threads it
	// through long polling and the scheduler, so cancellation unwinds
	// Run and every deferred close runs. Cleanup is bounded, so no
	// second-signal hard exit is needed.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	if err := app.Run(ctx); err != nil {
		// app.Run validates config before logging exists, so an early
		// failure would otherwise exit silently: no log output, no stderr,
		// nothing for an operator to act on. The error text comes from
		// config validation and wiring, which name the failing setting —
		// keep that diagnostic visible at bootstrap.
		fmt.Fprintf(os.Stderr, "startup failed: %v\n", err)
		os.Exit(1)
	}
}
