// Package urlredact strips credential-bearing parts (userinfo, query string,
// fragment) from URLs embedded in arbitrary text, typically error messages.
// MCP endpoints commonly carry credentials as query parameters
// (?api_key=...), and transport-layer errors quote full request URLs
// verbatim; anything that surfaces such errors — logs, startup diagnostics,
// LLM-visible tool responses — must see redacted text.
package urlredact

import (
	"net/url"
	"regexp"
	"strings"
)

// urlPattern matches http(s) URLs embedded in text, case-insensitively:
// endpoints configured as HTTPS://... must be redacted just like https://.
// Scheme case is not preserved — url.Parse lowercases u.Scheme, so redacted
// output always carries a lowercase scheme. It stops at whitespace, double
// quotes, and angle brackets — the characters error formatters use to
// delimit URLs (`*url.Error` renders as `Op "URL": cause`). Apostrophes are
// deliberately NOT delimiters: they are valid query characters and appear
// inside real URLs (?filter='active'), so stopping at one would leave the
// rest of the query — and its credentials — unredacted.
var urlPattern = regexp.MustCompile(`(?i)https?://[^\s"<>]+`)

// userInfoPattern matches `scheme://user[:password]@` at the start of a URL;
// the capture keeps the scheme so replacing the match with it leaves
// `scheme://` behind.
var userInfoPattern = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/@]+@`)

// Redact strips the userinfo, query string, and fragment from every http(s)
// URL embedded in s. URLs that cannot be parsed into a usable authority
// (invalid percent-escapes, no host) fall back to a textual strip so their
// userinfo and query cannot survive. Text without URLs is returned unchanged.
func Redact(s string) string {
	if !strings.Contains(s, "://") {
		return s
	}

	return urlPattern.ReplaceAllStringFunc(s, redactOneURL)
}

// RedactOne reduces a single endpoint URL to scheme://host/path, regardless
// of scheme: userinfo, query string, and fragment are always dropped. When
// the URL cannot be parsed (invalid escapes, control characters), userinfo
// is stripped textually and the query/fragment cut. Non-URL strings are
// returned unchanged.
//
// Unlike Redact (which works on embedded text), RedactOne takes a bare URL:
// there is no trailing-colon handling, and any scheme — not just http(s) —
// is accepted, because callers often redact endpoints whose scheme is about
// to be rejected.
func RedactOne(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host + u.Path
	}

	// Not parseable into a usable authority, or not a URL at all.
	return bestEffortStrip(rawURL)
}

// redactOneURL reduces a single URL match to scheme://host/path.
func redactOneURL(match string) string {
	// A URL embedded mid-sentence can greedily pick up the message's own
	// punctuation (`connect to http://x?k=v: boom`); keep a trailing colon
	// outside the redaction so the surrounding sentence survives.
	trailingColon := ""
	if strings.HasSuffix(match, ":") {
		trailingColon = ":"
		match = strings.TrimSuffix(match, ":")
	}

	u, err := url.Parse(match)
	if err != nil || u.Host == "" {
		// Unparseable or host-less: without the fallback, an invalid
		// percent-escape used to leave the userinfo verbatim in the output,
		// and a host-less URL used to come through with its query intact.
		return bestEffortStrip(match) + trailingColon
	}

	return u.Scheme + "://" + u.Host + u.Path + trailingColon
}

// bestEffortStrip is the fallback for URLs that cannot be parsed into a
// usable authority: drop the userinfo textually — it can sit before the part
// that broke the parse — then cut the query/fragment and everything from the
// first invalid percent-escape on, so neither can smuggle credentials into
// the redacted value. Non-URL input passes through unchanged.
func bestEffortStrip(s string) string {
	stripped := userInfoPattern.ReplaceAllString(s, "$1")
	if cut := strings.IndexAny(stripped, "?#"); cut != -1 {
		stripped = stripped[:cut]
	}
	if cut := firstInvalidEscapeIndex(stripped); cut != -1 {
		stripped = stripped[:cut]
	}

	return stripped
}

// firstInvalidEscapeIndex returns the index of the first '%' in s that does
// not start a valid %XX escape, or -1 if all escapes are well-formed.
func firstInvalidEscapeIndex(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			continue
		}
		if i+2 >= len(s) || !isHexDigit(s[i+1]) || !isHexDigit(s[i+2]) {
			return i
		}
		i += 2
	}

	return -1
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
