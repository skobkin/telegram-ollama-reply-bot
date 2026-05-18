package main

import (
	"context"
	"os"
	"telegram-ollama-reply-bot/internal/app"
)

func main() {
	if err := app.Run(context.Background()); err != nil {
		os.Exit(1)
	}
}
