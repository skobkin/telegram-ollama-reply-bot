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
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Redact(tc.in); got != tc.want {
				t.Fatalf("Redact:\n  in:  %s\n  got: %s\n  want:%s", tc.in, got, tc.want)
			}
		})
	}
}
