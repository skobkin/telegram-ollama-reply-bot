// Package errreport is the single entry point for reporting request-scoped
// errors to Sentry.
//
// A request that exceeds its processing deadline, or whose context is
// cancelled while it is being served, fails at several independent steps at
// once (backend call, prompt rendering, typing indicator, image description).
// The top-level handler already maps that outcome to a dedicated timeout error
// and records statistics, so reporting every step's context error only fans a
// single timeout out into several unactionable Sentry issues. Capture drops
// those errors; structured logs at the call sites remain the record of them.
package errreport

import (
	"context"
	"errors"

	"github.com/getsentry/sentry-go"
)

// IsContextError reports whether err signals context cancellation or deadline
// expiry anywhere in its chain.
func IsContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// Capture reports err to Sentry unless it is nil or a context error.
func Capture(err error) {
	captureWith(sentry.CurrentHub(), err)
}

func captureWith(hub *sentry.Hub, err error) {
	if err == nil || IsContextError(err) {
		return
	}

	hub.CaptureException(err)
}
