package mcp

import (
	"net/url"
	"regexp"
	"strings"
)

// urlPattern matches http(s) URLs embedded in error text. It stops at
// whitespace, quotes, and angle brackets — the characters error formatters
// use to delimit URLs (`*url.Error` renders as `Op "URL": cause`).
var urlPattern = regexp.MustCompile(`https?://[^\s"'<>]+`)

// redactURLCredentials strips the userinfo, query string, and fragment from
// every http(s) URL embedded in s. MCP endpoints commonly carry credentials
// as query parameters (?api_key=...), and transport-layer errors
// (`*url.Error`, JSON-RPC wraps) quote the full request URL verbatim. Since
// these errors reach WARN-level logs and the LLM-visible tool response, any
// URL inside them is reduced to scheme://host/path before leaving this
// package.
//
// URLs that cannot be parsed (e.g. invalid percent-escapes) are cut at the
// first ? or # instead.
func redactURLCredentials(s string) string {
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
