// Package safeerr converts raw external errors into sanitized, typed errors
// that are safe to log, return, and report to Sentry.
//
// Errors returned by HTTP clients and SDKs routinely embed the full request
// URL (which may carry credentials or secret-bearing query parameters) and
// provider response bodies (which may echo prompts or keys). A wrapper with a
// safe Error() but an Unwrap() leading to the original error is not a fix:
// Sentry's exception chain traverses wrapped causes and captures their text
// verbatim. WrapTransport therefore never retains the original error.
// Cancellation and deadline signals are preserved by wrapping only the
// standard library context sentinels, so errors.Is keeps working for
// context.Canceled and context.DeadlineExceeded while every node reachable in
// the chain is one of our own messages.
package safeerr

import (
	"context"
	"errors"
	"fmt"
)

// WrapTransport classifies cause and returns a sanitized error wrapping kind.
//
// The original cause is discarded entirely: it never appears in the returned
// error, its message, or any error chain rooted at the result. When cause
// signals cancellation or a deadline, the corresponding context sentinel is
// wrapped so callers can still match it with errors.Is; any other failure
// yields a stable typed error with no cause retained.
func WrapTransport(kind error, cause error) error {
	if kind == nil {
		kind = errors.New("request failed")
	}
	if cause == nil {
		return kind
	}

	switch {
	case errors.Is(cause, context.Canceled):
		return fmt.Errorf("%w: %w", kind, context.Canceled)
	case errors.Is(cause, context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", kind, context.DeadlineExceeded)
	default:
		return fmt.Errorf("%w: request failed", kind)
	}
}
