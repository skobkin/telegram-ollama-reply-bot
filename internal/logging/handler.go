package logging

import (
	"io"
	"log/slog"
	"os"
)

// Options configures logger construction and global default replacement.
type Options struct {
	Level      string
	Writer     io.Writer
	SetDefault bool
}

func buildLogger(opts Options) (*slog.Logger, error) {
	handler, err := buildHandler(opts)
	if err != nil {
		return nil, err
	}

	return slog.New(handler), nil
}

func buildHandler(opts Options) (slog.Handler, error) {
	level, err := parseLevel(opts.Level)
	if err != nil {
		return nil, err
	}

	writer := opts.Writer
	if writer == nil {
		writer = os.Stdout
	}

	return slog.NewJSONHandler(writer, &slog.HandlerOptions{
		Level: level,
	}), nil
}
