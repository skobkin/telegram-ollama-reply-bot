package logging

import (
	"io"
	"log/slog"
	"os"

	"telegram-ollama-reply-bot/internal/support/secretscrub"
)

// Options configures logger construction and global default replacement.
type Options struct {
	Level      string
	Writer     io.Writer
	SetDefault bool
	// Scrubber, when non-nil, wraps every handler so registered secret
	// values are replaced in messages and attributes before they reach any
	// writer. The manager keeps the scrubber across Configure calls, so
	// runtime logger reconfiguration never drops scrubbing.
	Scrubber *secretscrub.Scrubber
}

func buildLogger(opts Options, scrubber *secretscrub.Scrubber) (*slog.Logger, error) {
	handler, err := buildHandler(opts, scrubber)
	if err != nil {
		return nil, err
	}

	return slog.New(handler), nil
}

func buildHandler(opts Options, scrubber *secretscrub.Scrubber) (slog.Handler, error) {
	level, err := parseLevel(opts.Level)
	if err != nil {
		return nil, err
	}

	writer := opts.Writer
	if writer == nil {
		writer = os.Stdout
	}

	var handler slog.Handler = slog.NewJSONHandler(writer, &slog.HandlerOptions{
		Level: level,
	})

	if scrubber != nil {
		handler = newScrubHandler(scrubber, handler)
	}

	return handler, nil
}
