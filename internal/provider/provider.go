package provider

import (
	"errors"
	"fmt"
)

type ErrorKind string

const (
	ErrorKindAuthFailed       ErrorKind = "auth_failed"
	ErrorKindCreditsExhausted ErrorKind = "credits_exhausted"
	ErrorKindRateLimited      ErrorKind = "rate_limited"
	ErrorKindBadRequest       ErrorKind = "bad_request"
	// ErrorKindContentRejected means the provider refused the request on
	// content-safety grounds (a prompt or a generated result). Retrying the
	// same input cannot succeed, so it is permanent for that input.
	ErrorKindContentRejected     ErrorKind = "content_rejected"
	ErrorKindUpstreamUnavailable ErrorKind = "upstream_unavailable"
	ErrorKindUnsupported         ErrorKind = "unsupported"
	ErrorKindMisconfigured       ErrorKind = "misconfigured"
	ErrorKindUnknown             ErrorKind = "unknown"
)

type Error struct {
	Provider string
	Kind     ErrorKind
}

func (e *Error) Error() string {
	if e == nil {
		return string(ErrorKindUnknown)
	}
	if e.Provider == "" {
		return string(e.Kind)
	}

	return fmt.Sprintf("%s: %s", e.Provider, e.Kind)
}

func (e *Error) Summary() string {
	return e.Error()
}

func NewError(provider string, kind ErrorKind) error {
	return &Error{Provider: provider, Kind: kind}
}

func KindOf(err error) ErrorKind {
	if err == nil {
		return ""
	}

	var typed *Error
	if errors.As(err, &typed) {
		return typed.Kind
	}

	return ErrorKindUnknown
}

// IsDefinitiveRejection reports whether err is a provider rejection that
// retrying cannot fix: the credentials were refused (auth failed) or the
// account cannot pay for the call (credits exhausted). Everything else —
// rate limits, network failures, upstream outages — is transient from a
// caller's perspective.
func IsDefinitiveRejection(err error) bool {
	switch KindOf(err) {
	case ErrorKindAuthFailed, ErrorKindCreditsExhausted:
		return true
	default:
		return false
	}
}

type Attempt struct {
	Provider string
	Kind     ErrorKind
}

func (a Attempt) Summary() string {
	if a.Provider == "" {
		return string(a.Kind)
	}

	return fmt.Sprintf("%s: %s", a.Provider, a.Kind)
}

type ChainError struct {
	Attempts []Attempt
}

func (e *ChainError) Error() string {
	if e == nil || len(e.Attempts) == 0 {
		return string(ErrorKindUnknown)
	}

	result := "all providers failed: "
	for i, attempt := range e.Attempts {
		if i > 0 {
			result += "; "
		}
		result += attempt.Summary()
	}

	return result
}

func AttemptsFromErrors(pairs map[string]error, order []string) []Attempt {
	result := make([]Attempt, 0, len(order))
	for _, providerName := range order {
		err, ok := pairs[providerName]
		if !ok || err == nil {
			continue
		}

		result = append(result, Attempt{
			Provider: providerName,
			Kind:     KindOf(err),
		})
	}

	return result
}
