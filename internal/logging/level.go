package logging

import (
	"fmt"
	"log/slog"
	"strings"
)

func parseLevel(raw string) (slog.Leveler, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return nil, fmt.Errorf("unsupported log level: %q", raw)
	}
}
