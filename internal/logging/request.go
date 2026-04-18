package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
)

const requestIDLengthBytes = 8

func NewRequestID() (string, error) {
	buf := make([]byte, requestIDLengthBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}

	return hex.EncodeToString(buf), nil
}

func WithRequestLogger(ctx context.Context, logger *slog.Logger, requestID string) context.Context {
	if logger == nil {
		logger = slog.Default()
	}

	ctx = WithRequestID(ctx, requestID)

	return WithLogger(ctx, logger.With("request_id", requestID))
}
