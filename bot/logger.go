package bot

import (
	"fmt"
	"log/slog"
)

type Logger struct {
	prefix string
}

func NewLogger(prefix string) Logger {
	return Logger{
		prefix: prefix,
	}
}

func (l Logger) Debugf(format string, args ...any) {
	slog.Debug(l.prefix + fmt.Sprintf(format, args...))
}

func (l Logger) Errorf(format string, args ...any) {
	slog.Error(l.prefix + fmt.Sprintf(format, args...))
}
