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

// urlPattern matches http(s) URLs embedded in text. It stops at whitespace,
// double quotes, and angle brackets — the characters error formatters use to
// delimit URLs (`*url.Error` renders as `Op "URL": cause`). Apostrophes are
// deliberately NOT delimiters: they are valid query characters and appear
// inside real URLs (?filter='active'), so stopping at one would leave the
// rest of the query — and its credentials — unredacted.
var urlPattern = regexp.MustCompile(`https?://[^\s"<>]+`)

// Redact strips the userinfo, query string, and fragment from every http(s)
// URL embedded in s. URLs that cannot be parsed (e.g. invalid
// percent-escapes) are cut at the first ? or # instead. Text without URLs is
// returned unchanged.
func Redact(s string) string {
	if !strings.Contains(s, "://") {
		return s
	}

	return urlPattern.ReplaceAllStringFunc(s, redactOneURL)
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
	if err != nil {
		if cut := strings.IndexAny(match, "?#"); cut != -1 {
			match = match[:cut]
		}

		return match + trailingColon
	}
	if u.Host == "" {
		return match + trailingColon
	}

	return u.Scheme + "://" + u.Host + u.Path + trailingColon
}
