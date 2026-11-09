package urlredact

import "testing"

// TestRedact pins the redaction rules: query strings (the common
// credential-in-URL vector), fragments, and userinfo are dropped; scheme,
// host, and path survive; text without URLs is returned unchanged.
func TestRedact(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "url_error_style",
			in:   `Post "https://mcp.example.invalid/mcp?api_key=hunter2": context deadline exceeded`,
			want: `Post "https://mcp.example.invalid/mcp": context deadline exceeded`,
		},
		{
			name: "quoted_url_with_multiple_params",
			in:   `dial tcp: Get "http://127.0.0.1:9999/mcp?api_key=s3cr3t&token=abc": connection refused`,
			want: `dial tcp: Get "http://127.0.0.1:9999/mcp": connection refused`,
		},
		{
			name: "unquoted_endpoint_in_wrap",
			in:   `connect to https://mcp.example.invalid/mcp?api_key=hunter2: EOF`,
			want: `connect to https://mcp.example.invalid/mcp: EOF`,
		},
		{ //nolint:gosec // test fixture: redaction must drop userinfo credentials
			name: "userinfo_dropped",
			in:   `Post "https://user:pass@mcp.example.invalid/mcp": EOF`,
			want: `Post "https://mcp.example.invalid/mcp": EOF`,
		},
		{
			name: "fragment_dropped",
			in:   `Post "https://mcp.example.invalid/mcp#section": EOF`,
			want: `Post "https://mcp.example.invalid/mcp": EOF`,
		},
		{
			name: "url_without_query_untouched",
			in:   `Post "https://mcp.example.invalid/mcp": EOF`,
			want: `Post "https://mcp.example.invalid/mcp": EOF`,
		},
		{
			name: "invalid_escape_cut_at_query",
			in:   `Post "https://mcp.example.invalid/mcp?token=a%zz": EOF`,
			want: `Post "https://mcp.example.invalid/mcp": EOF`,
		},
		{ //nolint:gosec // test fixture: redaction must drop userinfo credentials
			// Regression: the parse-failure fallback used to cut only at ?/#,
			// so an unparseable URL with no query came through with its
			// userinfo verbatim.
			name: "unparseable_url_userinfo_stripped",
			in:   `Post "https://user:pass@host/%zz": EOF`,
			want: `Post "https://host/": EOF`,
		},
		{ //nolint:gosec // test fixture: redaction must drop userinfo credentials
			// Regression: a host-less URL used to be returned verbatim, so
			// its query — the most common credential spot — survived.
			name: "hostless_url_query_cut",
			in:   `connect https://?api_key=hunter2 refused`,
			want: `connect https:// refused`,
		},
		{
			name: "hostless_url_userinfo_and_query_cut",
			in:   `dial https://user:pass@?api_key=hunter2: refused`,
			want: `dial https://: refused`,
		},
		{
			name: "no_urls_unchanged",
			in:   `sending "tools/call": context deadline exceeded`,
			want: `sending "tools/call": context deadline exceeded`,
		},
		{
			name: "multiple_urls",
			in:   `first https://a.example.invalid/x?k=1 then https://b.example.invalid/y?k=2 end`,
			want: `first https://a.example.invalid/x then https://b.example.invalid/y end`,
		},
		{
			// Regression: apostrophes are valid query characters. Stopping the
			// match at one used to leave the remainder of the query — and its
			// credentials — completely unredacted.
			name: "apostrophe_in_query",
			in:   `Post "https://mcp.example.invalid/mcp?filter='active'&api_key=hunter2": EOF`,
			want: `Post "https://mcp.example.invalid/mcp": EOF`,
		},
		{
			// Documented trade-off of the above: prose that quotes a URL with
			// single quotes loses the closing quote (the whole run up to the
			// next whitespace is treated as part of the URL). Go error
			// formatters use double quotes, so this only affects prose.
			name: "single_quoted_prose_loses_closing_quote",
			in:   `error: 'https://x.invalid/p?k=v' refused`,
			want: `error: 'https://x.invalid/p refused`,
		},
		{
			// Regression: the pattern used to be case-sensitive, so an
			// endpoint configured as HTTPS://... fell through unredacted and
			// leaked into startup diagnostics verbatim.
			name: "uppercase_scheme_redacted",
			in:   `connect to HTTPS://host/mcp?api_key=hunter2: EOF`,
			want: `connect to https://host/mcp: EOF`,
		},
		{
			name: "mixed_case_scheme_redacted",
			in:   `Post "HtTpS://host/mcp?api_key=hunter2": EOF`,
			want: `Post "https://host/mcp": EOF`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Redact(tc.in); got != tc.want {
				t.Fatalf("Redact:\n  in:  %s\n  got: %s\n  want:%s", tc.in, got, tc.want)
			}
		})
	}
}

// TestRedactOne pins the single-URL rules used for configured endpoints:
// any scheme is accepted (callers often redact an endpoint whose scheme is
// about to be rejected), and userinfo never survives a failed parse — the
// textual fallback strips it before cutting the rest.
func TestRedactOne(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{ //nolint:gosec // test fixture: redaction must drop userinfo credentials
			name: "non_http_scheme_userinfo_dropped",
			in:   `ftp://user:pass@host/mcp`,
			want: `ftp://host/mcp`,
		},
		{ //nolint:gosec // test fixture: redaction must drop userinfo credentials
			name: "uppercase_scheme_lowercased_and_cleaned",
			in:   `HTTPS://user:pass@host/mcp?k=s`,
			want: `https://host/mcp`,
		},
		{ //nolint:gosec // test fixture: redaction must drop userinfo credentials
			name: "unparseable_escape_userinfo_stripped_and_cut",
			in:   `https://user:pass@host/%zz`,
			want: `https://host/`,
		},
		{
			name: "control_char_cut_at_query",
			in:   "http://host/mcp?x=\x7f",
			want: `http://host/mcp`,
		},
		{ //nolint:gosec // test fixture: redaction must drop userinfo credentials
			name: "hostless_url_query_cut",
			in:   `https://?api_key=hunter2`,
			want: `https://`,
		},
		{
			name: "not_a_url_unchanged",
			in:   `not a url`,
			want: `not a url`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactOne(tc.in); got != tc.want {
				t.Fatalf("RedactOne:\n  in:  %s\n  got: %s\n  want:%s", tc.in, got, tc.want)
			}
		})
	}
}
