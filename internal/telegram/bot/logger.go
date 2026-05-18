package bot

import (
	"fmt"
	"log/slog"
	"strings"
)

type Logger struct {
	logger *slog.Logger
	token  string
}

func NewLogger(logger *slog.Logger, token string) Logger {
	return Logger{
		logger: logger,
		token:  token,
	}
}

func (l Logger) Debugf(format string, args ...any) {
	l.logger.Debug(l.sanitize(fmt.Sprintf(format, args...)))
}

func (l Logger) Errorf(format string, args ...any) {
	l.logger.Error(l.sanitize(fmt.Sprintf(format, args...)))
}

func (l Logger) sanitize(message string) string {
	if l.token == "" {
		return message
	}

	return strings.ReplaceAll(message, l.token, "[REDACTED]")
}
