package interactivity

import (
	"log/slog"
	"time"
)

// testTime is a fixed instant for deterministic history fixtures.
func testTime() time.Time {
	return time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
}

// discardLogger keeps engine logs out of test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}
