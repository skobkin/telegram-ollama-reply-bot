// Package secretscrub removes configured application secrets from arbitrary
// text, log records, and HTTP client errors, so that logs, Sentry events, and
// error chains cannot retain credential material.
//
// It complements urlredact (structural URL stripping) and safeerr (typed
// transport-error replacement): where those layers sanitize by shape and by
// call site, this package sanitizes by content — a registered secret value is
// replaced wherever it appears, whatever the surrounding text.
//
// Matching is literal. Percent-escaped variants of a secret inside a URL are
// not matched; URL-shaped surfaces are covered by urlredact's structural
// stripping instead.
package secretscrub

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Secret names one piece of credential material for scrubbing.
type Secret struct {
	// Name is a stable label embedded in the replacement marker (for
	// example "telegram-token").
	Name string
	// Value is the exact literal replaced everywhere it appears. Empty
	// values are ignored. Values must be genuine secrets: short or generic
	// strings would corrupt surrounding text, and there is deliberately no
	// length floor that could silently disable redaction of a short real
	// key.
	Value string
}

// Scrubber replaces registered secret values with "[REDACTED:<name>]". It is
// immutable after New and safe for concurrent use.
type Scrubber struct {
	names    []string // parallel to values
	values   []string // longest first
	replacer *strings.Replacer
}

// New builds a Scrubber. Empty values are skipped and duplicate values are
// collapsed (the first name wins). Secrets are ordered longest-value-first,
// so a value that is a prefix of another is replaced as part of the longer
// match.
//
// Replacement is a single left-to-right pass that never rescans emitted
// markers, and every marker is verified at construction time to contain no
// registered value: a secret equal to (or embedded in) a marker string is
// otherwise reintroduced by the redaction itself, and sequential
// replacements could corrupt earlier markers. Markers fall back to the
// name-less "[REDACTED]" and then to an empty replacement, in that order —
// so the result of Scrub can never carry a registered value, whatever the
// configured secrets are.
func New(secrets []Secret) *Scrubber {
	byValue := make(map[string]string, len(secrets))
	for _, secret := range secrets {
		if secret.Value == "" {
			continue
		}

		if _, dup := byValue[secret.Value]; dup {
			continue
		}

		byValue[secret.Value] = secret.Name
	}

	s := &Scrubber{}
	for value, name := range byValue {
		s.values = append(s.values, value)
		s.names = append(s.names, name)
	}

	// Map iteration order is nondeterministic; sorting by descending value
	// length makes replacement deterministic and prefix-correct.
	order := make([]int, len(s.values))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return len(s.values[order[a]]) > len(s.values[order[b]])
	})

	names := make([]string, len(order))
	values := make([]string, len(order))
	for i, idx := range order {
		names[i] = s.names[idx]
		values[i] = s.values[idx]
	}
	s.names = names
	s.values = values

	pairs := make([]string, 0, len(s.values)*2)
	for i := range s.values {
		pairs = append(pairs, s.values[i], s.safeMarker(s.names[i]))
	}

	if len(pairs) > 0 {
		s.replacer = strings.NewReplacer(pairs...)
	}

	return s
}

// safeMarker picks the replacement string for one secret: the first of the
// standard marker, the name-less fallback, and the empty replacement that
// contains no registered value. Empty always qualifies, so the search
// terminates with a secret-free result.
func (s *Scrubber) safeMarker(name string) string {
	for _, candidate := range []string{marker(name), "[REDACTED]", ""} {
		if !s.containsAnyValue(candidate) {
			return candidate
		}
	}

	return ""
}

func (s *Scrubber) containsAnyValue(text string) bool {
	for _, value := range s.values {
		if strings.Contains(text, value) {
			return true
		}
	}

	return false
}

// Scrub replaces every occurrence of every registered secret value in text
// with "[REDACTED:<name>]". Text carrying no secret is returned unchanged,
// and scrubbing is idempotent: the pass never rescans its own output and
// every marker is constructed to contain no registered value.
func (s *Scrubber) Scrub(text string) string {
	if s == nil || s.replacer == nil || !s.Contains(text) {
		return text
	}

	return s.replacer.Replace(text)
}

// Contains reports whether text carries any registered secret value. It is a
// cheap pre-check for callers that only need the verdict.
func (s *Scrubber) Contains(text string) bool {
	if s == nil {
		return false
	}

	for _, value := range s.values {
		if strings.Contains(text, value) {
			return true
		}
	}

	return false
}

// Leaks reports whether any error reachable from err carries a registered
// secret in its message. The traversal mirrors sentry-go's exception
// conversion (Unwrap() []error, then Unwrap() error, then Cause() error), so
// a false result means the whole chain is safe to serialize.
func (s *Scrubber) Leaks(err error) bool {
	if err == nil {
		return false
	}

	if s.Contains(err.Error()) {
		return true
	}

	// Order matches sentry-go exception.go convertErrorDFS.
	switch v := err.(type) { //nolint:errorlint // enumerating unwrap structure exactly like sentry-go, not matching a target
	case interface{ Unwrap() []error }:
		for _, cause := range v.Unwrap() {
			if s.Leaks(cause) {
				return true
			}
		}

		return false
	case interface{ Unwrap() error }:
		return s.Leaks(v.Unwrap())
	case interface{ Cause() error }:
		return s.Leaks(v.Cause())
	default:
		return false
	}
}

func marker(name string) string {
	return "[REDACTED:" + name + "]"
}

// HTTPDoer executes HTTP requests. It matches the injectable client
// interface of go-openai (openai.HTTPDoer) and satisfies it structurally.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// WrapDoer returns an HTTPDoer whose error path is secret-free while the
// success path is returned untouched (the request is never modified).
//
// Error semantics: an error whose whole chain is secret-free is returned as
// the same object. Otherwise a top-level *url.Error — what http.Client.Do
// always returns — is rebuilt with a scrubbed URL and its original inner
// error whenever that inner chain is clean, keeping errors.Is/As and
// net.Error semantics intact (a context deadline still matches
// context.DeadlineExceeded). Chains that themselves carry secret material
// collapse into a scrubbedError.
//
// The URL is scrubbed literally, not structurally: the non-secret remainder
// stays diagnosable, e.g.
// "https://api.telegram.org/bot[REDACTED:telegram-token]/sendMessage".
func WrapDoer(s *Scrubber, next HTTPDoer) HTTPDoer {
	return doer{scrubber: s, next: next}
}

type doer struct {
	scrubber *Scrubber
	next     HTTPDoer
}

func (d doer) Do(req *http.Request) (*http.Response, error) {
	resp, err := d.next.Do(req)
	if err == nil {
		return resp, nil
	}

	return nil, d.scrubber.scrubError(err)
}

func (s *Scrubber) scrubError(err error) error {
	if err == nil {
		return nil
	}

	// Identity fast path: the common case for header-authenticated APIs,
	// whose errors never carry secret text. Returning the original object
	// keeps *url.Error typing, Timeout()/Permanent(), and the full chain.
	if !s.Leaks(err) {
		return err
	}

	if urlErr, ok := err.(*url.Error); ok { //nolint:errorlint // only the exact top-level type is rebuilt; wrapped url.Errors collapse below
		return &url.Error{
			Op:  urlErr.Op,
			URL: s.Scrub(urlErr.URL),
			Err: s.scrubInner(urlErr.Err),
		}
	}

	return s.collapse(err)
}

// scrubInner keeps a secret-free inner error by identity so the rebuilt
// *url.Error preserves the original chain, and collapses a leaking one.
func (s *Scrubber) scrubInner(err error) error {
	if err == nil || !s.Leaks(err) {
		return err
	}

	return s.collapse(err)
}

func (s *Scrubber) collapse(err error) error {
	return &scrubbedError{msg: s.Scrub(err.Error()), cause: err}
}

// scrubbedError replaces an error whose message or chain carries secret
// material. It deliberately implements no Unwrap, Unwrap([]error), Cause, or
// As: sentry-go's exception conversion walks the first three and serializes
// every reachable node's message verbatim, so retaining the original would
// re-leak what was scrubbed (the same reasoning as safeerr discarding its
// cause), and errors.As delegation would hand callers the original object
// whose Error() renders the secret. Is still delegates to the original
// error, keeping sentinel matching (context.Canceled, package error kinds)
// without exposing any instance.
type scrubbedError struct {
	msg   string
	cause error
}

func (e *scrubbedError) Error() string { return e.msg }

func (e *scrubbedError) Is(target error) bool { return errors.Is(e.cause, target) }
