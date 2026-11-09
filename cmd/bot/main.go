package main

import (
	"context"
	"fmt"
	"os"
	"telegram-ollama-reply-bot/internal/app"
)

func main() {
	if err := app.Run(context.Background()); err != nil {
		// app.Run validates config before logging exists, so an early
		// failure would otherwise exit silently: no log output, no stderr,
		// nothing for an operator to act on. The error text comes from
		// config validation and wiring, which name the failing setting —
		// keep that diagnostic visible at bootstrap.
		fmt.Fprintf(os.Stderr, "startup failed: %v\n", err)
		os.Exit(1)
	}
}
