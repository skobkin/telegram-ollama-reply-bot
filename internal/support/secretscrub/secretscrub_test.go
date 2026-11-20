package secretscrub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Fake secret fixtures. The strings are deliberately not credential-shaped to
// stay out of secret-scanning; they only need to be unique literals.
var (
	leakA = "TESTSECRET"
	leakB = "SECONDSECRET"
)

func testScrubber() *Scrubber {
	return New([]Secret{
		{Name: "a", Value: leakA},
		{Name: "b", Value: leakB},
	})
}

func TestNewSkipsEmptyAndDedupes(t *testing.T) {
	t.Parallel()

	scrubber := New([]Secret{
		{Name: "empty", Value: ""},
		{Name: "a", Value: leakA},
		{Name: "a-dup", Value: leakA},
		{Name: "b", Value: leakB},
	})

	got := scrubber.Scrub("x " + leakA + " y")
	if got != "x [REDACTED:a] y" {
		t.Fatalf("duplicate value kept the wrong name: %q", got)
	}

	if got := scrubber.Scrub("x " + leakB + " y"); got != "x [REDACTED:b] y" {
		t.Fatalf("unexpected scrub result: %q", got)
	}
}

func TestScrubReplacesEveryOccurrence(t *testing.T) {
	t.Parallel()

	scrubber := testScrubber()

	got := scrubber.Scrub(leakA + " then " + leakA + " and " + leakB)
	want := "[REDACTED:a] then [REDACTED:a] and [REDACTED:b]"
	if got != want {
		t.Fatalf("scrub result mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestScrubPrefersLongestSecret(t *testing.T) {
	t.Parallel()

	prefix := leakA
	longer := leakA + "-tail"

	scrubber := New([]Secret{
		{Name: "prefix", Value: prefix},
		{Name: "longer", Value: longer},
	})

	// The longer value must be matched first; a naive first-match on the
	// prefix would leave "-tail" dangling in the marker.
	if got, want := scrubber.Scrub(longer), "[REDACTED:longer]"; got != want {
		t.Fatalf("longest-secret match failed: got %q, want %q", got, want)
	}

	if got, want := scrubber.Scrub(prefix), "[REDACTED:prefix]"; got != want {
		t.Fatalf("prefix secret vanished: got %q, want %q", got, want)
	}
}

func TestScrubLeavesCleanTextUnchanged(t *testing.T) {
	t.Parallel()

	scrubber := testScrubber()

	text := "Post \"https://api.telegram.org/sendMessage\": dial tcp: connection refused"
	if got := scrubber.Scrub(text); got != text {
		t.Fatalf("clean text was modified: %q", got)
	}

	if scrubber.Contains(text) {
		t.Fatal("Contains reported a secret in clean text")
	}
}

func TestScrubEmptyScrubberPassthrough(t *testing.T) {
	t.Parallel()

	scrubber := New(nil)

	text := "anything at all"
	if got := scrubber.Scrub(text); got != text {
		t.Fatalf("empty scrubber modified text: %q", got)
	}

	if scrubber.Contains(text) {
		t.Fatal("empty scrubber reported a secret")
	}
}

func TestScrubIsIdempotent(t *testing.T) {
	t.Parallel()

	scrubber := testScrubber()

	once := scrubber.Scrub(leakA + " and " + leakB)
	twice := scrubber.Scrub(once)
	if once != twice {
		t.Fatalf("scrubbing is not idempotent: %q -> %q", once, twice)
	}
}

// assertSecretFree fails when the text carries any of the scrubber's
// registered values — including inside generated markers.
func assertSecretFree(t *testing.T, scrubber *Scrubber, text string) {
	t.Helper()

	if scrubber.containsAnyValue(text) {
		t.Fatalf("registered secret survived in output: %q", text)
	}
}

func TestScrubMarkerCollisionNeverReintroducesSecret(t *testing.T) {
	t.Parallel()

	// A secret literally equal to the marker prefix would ride inside every
	// standard marker; the construction-time fallback must deny it.
	scrubber := New([]Secret{
		{Name: "trap", Value: "REDACTED"},
		{Name: "normal", Value: leakA},
	})

	got := scrubber.Scrub("before REDACTED after " + leakA)
	assertSecretFree(t, scrubber, got)

	twice := scrubber.Scrub(got)
	if twice != got {
		t.Fatalf("collision scrubbing is not idempotent: %q -> %q", got, twice)
	}
}

func TestScrubSequentialMarkersStayIntact(t *testing.T) {
	t.Parallel()

	// One secret's value is a fragment of the standard marker shape, so a
	// sequential per-secret ReplaceAll would rewrite an earlier marker.
	scrubber := New([]Secret{
		{Name: "full", Value: "REDACTED:zz"},
		{Name: "frag", Value: "zz"},
	})

	if got, want := scrubber.Scrub("REDACTED:zz"), "[REDACTED:full]"; got != want {
		t.Fatalf("fragment corrupted the longer match: got %q, want %q", got, want)
	}

	if got, want := scrubber.Scrub("a zz b"), "a [REDACTED:frag] b"; got != want {
		t.Fatalf("fragment secret lost: got %q, want %q", got, want)
	}

	assertSecretFree(t, scrubber, scrubber.Scrub("REDACTED:zz and zz"))
}

func TestScrubOverlappingSecrets(t *testing.T) {
	t.Parallel()

	scrubber := New([]Secret{
		{Name: "short", Value: "cde"},
		{Name: "long", Value: "abcdef"},
	})

	if got, want := scrubber.Scrub("x abcdef y"), "x [REDACTED:long] y"; got != want {
		t.Fatalf("overlap picked the short match: got %q, want %q", got, want)
	}

	assertSecretFree(t, scrubber, scrubber.Scrub("x abcdef y"))
}

func TestScrubShortCredentialMarkerFallback(t *testing.T) {
	t.Parallel()

	// A credential whose own name collides with its value cannot use the
	// standard marker; the name-less fallback keeps the output secret-free.
	scrubber := New([]Secret{{Name: "ab", Value: "ab"}})

	got := scrubber.Scrub("token=ab end")
	assertSecretFree(t, scrubber, got)

	if !strings.Contains(got, "token=") {
		t.Fatalf("surrounding text was mutilated: %q", got)
	}
}

func TestScrubSplicedResultFailsClosed(t *testing.T) {
	t.Parallel()

	// Scrubbing "aREDACTEDb" removes the whole secret "REDACTED" — its
	// marker fell back to empty — and splices the surrounding text into
	// "ab", the other registered secret. The complete-result check must
	// catch the splice and fail closed instead of emitting the joined
	// secret.
	scrubber := New([]Secret{
		{Name: "whole", Value: "REDACTED"},
		{Name: "splice", Value: "ab"},
	})

	got := scrubber.Scrub("aREDACTEDb")
	assertSecretFree(t, scrubber, got)
}

func TestScrubCompleteResultCheckNoFalseTrigger(t *testing.T) {
	t.Parallel()

	// Without a splice, normal multi-secret scrubbing is untouched:
	// removing one secret must not wholesale-replace the rest of the text.
	scrubber := New([]Secret{
		{Name: "first", Value: leakA},
		{Name: "second", Value: leakB},
	})

	if got, want := scrubber.Scrub("x "+leakA+" y "+leakB+" z"), "x [REDACTED:first] y [REDACTED:second] z"; got != want {
		t.Fatalf("complete-result check altered normal scrubbing: got %q, want %q", got, want)
	}
}

func TestOmissionMarkersAreCollisionSafe(t *testing.T) {
	t.Parallel()

	// A registered secret embedded in the standard omission marker text
	// must not ride back into scrubbed output through the marker itself;
	// the markers are chosen per scrubber like every other replacement
	// string.
	scrubber := New([]Secret{
		{Name: "trap", Value: "[REDACTED:depth-limit]"},
		{Name: "normal", Value: leakA},
	})

	for _, omission := range []string{scrubber.TruncatedMarker(), scrubber.UnserializableMarker()} {
		if scrubber.containsAnyValue(omission) {
			t.Fatalf("omission marker carries a registered value: %q", omission)
		}
	}

	// Unaffected registries keep the standard marker text.
	plain := testScrubber()
	if got, want := plain.TruncatedMarker(), "[REDACTED:depth-limit]"; got != want {
		t.Fatalf("unexpected truncation marker: got %q, want %q", got, want)
	}

	if got, want := plain.UnserializableMarker(), "[REDACTED:unserializable]"; got != want {
		t.Fatalf("unexpected unserializable marker: got %q, want %q", got, want)
	}
}

// leakingDoer returns the given error from every Do call.
type leakingDoer struct {
	resp *http.Response
	err  error
}

func (d leakingDoer) Do(*http.Request) (*http.Response, error) {
	return d.resp, d.err
}

func TestWrapDoerPassesResponseThrough(t *testing.T) {
	t.Parallel()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
	}
	doer := leakingDoer{resp: resp}

	wrapped := WrapDoer(testScrubber(), doer)

	got, err := wrapped.Do(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	defer func() { _ = got.Body.Close() }()

	if got != resp {
		t.Fatal("response was not passed through by identity")
	}
}

func TestWrapDoerReturnsCleanErrorUnchanged(t *testing.T) {
	t.Parallel()

	orig := errors.New("dial tcp: connection refused")
	doer := leakingDoer{err: orig}

	wrapped := WrapDoer(testScrubber(), doer)

	_, err := wrapped.Do(nil) //nolint:bodyclose // error path: the response is always nil
	if err != nil {
		if err != orig { //nolint:errorlint // identity comparison is the assertion: the same object must come back
			t.Fatalf("clean error was rebuilt: %v", err)
		}

		return
	}

	t.Fatal("expected an error")
}

// closedListenerURL starts and immediately closes a local listener, so a
// request to the returned address fails fast with connection refused while
// still producing a real *url.Error from http.Client.Do.
func closedListenerURL(t *testing.T, path string) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	address := listener.Addr().String()

	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	return "http://" + address + path
}

func TestWrapDoerScrubsURLErrorURL(t *testing.T) {
	t.Parallel()

	client := &http.Client{Timeout: 2 * time.Second}
	wrapped := WrapDoer(testScrubber(), client)

	// The request URL carries the fake secret in its path, mimicking the
	// telego "api.telegram.org/bot<token>/<method>" shape.
	target := closedListenerURL(t, "/bot"+leakA+"/sendMessage")
	req, err := http.NewRequest(http.MethodPost, target, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	_, err = wrapped.Do(req) //nolint:bodyclose // error path: the response is always nil
	if err == nil {
		t.Fatal("expected a transport error")
	}

	if !strings.Contains(err.Error(), "[REDACTED:a]") {
		t.Fatalf("URL was not scrubbed: %v", err)
	}

	if strings.Contains(err.Error(), leakA) {
		t.Fatalf("secret survived in error: %v", err)
	}

	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Fatalf("*url.Error type lost after rebuild: %T", err)
	}

	if strings.Contains(urlErr.URL, leakA) {
		t.Fatalf("secret survived in rebuilt URL field: %s", urlErr.URL)
	}
}

func TestWrapDoerPreservesDeadlineExceededThroughRebuild(t *testing.T) {
	t.Parallel()

	// A slow endpoint plus a short client timeout: the resulting *url.Error
	// carries a secret-bearing URL and a context deadline inside, so the
	// rebuild path must keep the inner chain intact.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := &http.Client{Timeout: 10 * time.Millisecond}
	wrapped := WrapDoer(testScrubber(), client)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/bot"+leakA+"/getMe", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	_, err = wrapped.Do(req) //nolint:bodyclose // error path: the response is always nil
	if err == nil {
		t.Fatal("expected a timeout error")
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("context.DeadlineExceeded no longer matches after rebuild: %v", err)
	}

	if strings.Contains(err.Error(), leakA) {
		t.Fatalf("secret survived the rebuild: %v", err)
	}
}

type typedLeakCause struct {
	flag string
}

func (e *typedLeakCause) Error() string { return "upstream said " + leakA + " (" + e.flag + ")" }

// assertChainCleanSentry walks an error chain exactly like sentry-go's
// exception conversion (Unwrap() []error, then Unwrap() error, then
// Cause() error) and fails if any reachable node's message carries a fake
// secret. It is intentionally an independent re-implementation, not a call
// to Scrubber.Leaks, so it can catch bugs in the traversal itself.
func assertChainCleanSentry(t *testing.T, err error) {
	t.Helper()

	if err == nil {
		return
	}

	for _, secret := range []string{leakA, leakB} {
		if strings.Contains(err.Error(), secret) { //nolint:gosec // fake test fixture, not a credential
			t.Fatalf("chain node leaks secret: %v", err)
		}
	}

	switch v := err.(type) { //nolint:errorlint // mirroring sentry-go's traversal order, not matching a target
	case interface{ Unwrap() []error }:
		for _, cause := range v.Unwrap() {
			assertChainCleanSentry(t, cause)
		}
	case interface{ Unwrap() error }:
		assertChainCleanSentry(t, v.Unwrap())
	case interface{ Cause() error }:
		assertChainCleanSentry(t, v.Cause())
	}
}

func TestWrapDoerCollapsesLeakingInnerChain(t *testing.T) {
	t.Parallel()

	cause := &typedLeakCause{flag: "trace-1"}
	doer := leakingDoer{err: fmt.Errorf("call failed: %w", cause)}
	scrubber := testScrubber()

	_, err := WrapDoer(scrubber, doer).Do(nil) //nolint:bodyclose // error path: the response is always nil
	if err == nil {
		t.Fatal("expected an error")
	}

	if strings.Contains(err.Error(), leakA) {
		t.Fatalf("secret survived the collapse: %v", err)
	}

	if !strings.Contains(err.Error(), "[REDACTED:a]") {
		t.Fatalf("collapse lost the marker: %v", err)
	}

	assertChainCleanSentry(t, err)

	// The whole chain carries the secret, so the top-level error is not a
	// *url.Error and must have collapsed into a scrubbedError exposing no
	// unwrap surface at all.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		t.Fatalf("unexpected *url.Error surface after collapse: %v", err)
	}

	if scrubber.Leaks(err) {
		t.Fatalf("Leaks still reports a leak after scrubbing: %v", err)
	}
}

var errChainSentinel = errors.New("chain sentinel")

func TestWrapDoerCollapsingKeepsSentinelMatching(t *testing.T) {
	t.Parallel()

	// The sentinel sits inside the leaking chain, so matching it after the
	// collapse proves the delegated Is reaches the severed chain.
	doer := leakingDoer{err: fmt.Errorf("request failed: %w (%s)", errChainSentinel, leakA)}

	_, err := WrapDoer(testScrubber(), doer).Do(nil) //nolint:bodyclose // error path: the response is always nil
	if err == nil {
		t.Fatal("expected an error")
	}

	if !errors.Is(err, errChainSentinel) {
		t.Fatalf("errors.Is lost the sentinel through delegation: %v", err)
	}

	// An upstream-style errors.Is through a fresh wrap must work the same.
	if !errors.Is(fmt.Errorf("outer: %w", err), errChainSentinel) {
		t.Fatal("errors.Is through a fresh wrap lost the sentinel")
	}
}

func TestWrapDoerCollapsingHidesTypedCause(t *testing.T) {
	t.Parallel()

	cause := &typedLeakCause{flag: "trace-2"}
	doer := leakingDoer{err: fmt.Errorf("request failed: %w (%s)", cause, leakA)}

	_, err := WrapDoer(testScrubber(), doer).Do(nil) //nolint:bodyclose // error path: the response is always nil
	if err == nil {
		t.Fatal("expected an error")
	}

	if strings.Contains(err.Error(), leakA) {
		t.Fatalf("secret survived the collapse: %v", err)
	}

	// Typed extraction must not recover the original cause: its Error()
	// renders the secret, so handing out the instance would defeat the
	// collapse. Is still matches the original instance through delegation —
	// matching alone never hands out the object.
	var typed *typedLeakCause
	if errors.As(err, &typed) {
		t.Fatalf("errors.As recovered the secret-bearing cause: %+v", typed)
	}

	if !errors.Is(err, cause) {
		t.Fatal("errors.Is must still match the cause through delegation")
	}
}

func TestWrapDoerJoinErrorLeak(t *testing.T) {
	t.Parallel()

	clean := errors.New("second request failed")
	leaking := fmt.Errorf("first request to /bot%s/getMe failed", leakA)
	doer := leakingDoer{err: errors.Join(leaking, clean)}

	_, err := WrapDoer(testScrubber(), doer).Do(nil) //nolint:bodyclose // error path: the response is always nil
	if err == nil {
		t.Fatal("expected an error")
	}

	if strings.Contains(err.Error(), leakA) {
		t.Fatalf("secret survived the join collapse: %v", err)
	}

	assertChainCleanSentry(t, err)
}
