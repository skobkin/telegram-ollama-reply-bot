package safeerr

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestWrapTransport_PreservesContextCanceled(t *testing.T) {
	t.Parallel()

	// A raw url.Error whose text embeds a secret-bearing URL, as produced by
	// http.Client.Do.
	cause := &url.Error{Op: "Post", URL: "https://host/v1?token=TESTSECRET", Err: context.Canceled}

	err := WrapTransport(errSentinelForTest, cause)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected errors.Is(err, context.Canceled), got %v", err)
	}
	if !errors.Is(err, errSentinelForTest) {
		t.Fatalf("expected kind sentinel match, got %v", err)
	}
	assertChainClean(t, err, "TESTSECRET")
}

var errSentinelForTest = errors.New("sentinel")

func TestWrapTransport_PreservesDeadlineExceeded(t *testing.T) {
	t.Parallel()

	cause := fmt.Errorf("wrapped: %w", context.DeadlineExceeded)

	err := WrapTransport(errSentinelForTest, cause)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected errors.Is(err, context.DeadlineExceeded), got %v", err)
	}
}

func TestWrapTransport_DiscardsUnknownCause(t *testing.T) {
	t.Parallel()

	cause := &url.Error{Op: "Get", URL: "https://host/file/botTESTSECRET/photo.jpg", Err: errors.New("connection reset")}

	err := WrapTransport(errSentinelForTest, cause)

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unexpected context sentinel match for unknown cause: %v", err)
	}
	if !errors.Is(err, errSentinelForTest) {
		t.Fatalf("expected kind sentinel match, got %v", err)
	}
	assertChainClean(t, err, "TESTSECRET", "connection reset")
}

func TestWrapTransport_NilCause(t *testing.T) {
	t.Parallel()

	err := WrapTransport(errSentinelForTest, nil)

	if !errors.Is(err, errSentinelForTest) || err.Error() != errSentinelForTest.Error() {
		t.Fatalf("expected bare kind, got %v", err)
	}
}

// assertChainClean recursively walks the error chain (single and multi
// Unwrap) — the same traversal Sentry's exception chain performs — and fails
// when any node's message contains one of the sentinels.
func assertChainClean(t *testing.T, err error, sentinels ...string) {
	t.Helper()

	seen := make(map[error]bool)

	var visit func(error)
	visit = func(e error) {
		if e == nil || seen[e] {
			return
		}
		seen[e] = true

		msg := e.Error()
		for _, s := range sentinels {
			if strings.Contains(msg, s) {
				t.Fatalf("error chain leaks %q: %q", s, msg)
			}
		}

		//nolint:errorlint // intentional: enumerating the direct unwrap structure like Sentry, not matching a target type
		switch unwrapper := e.(type) {
		case interface{ Unwrap() error }:
			visit(unwrapper.Unwrap())
		case interface{ Unwrap() []error }:
			for _, child := range unwrapper.Unwrap() {
				visit(child)
			}
		}
	}

	visit(err)
}
