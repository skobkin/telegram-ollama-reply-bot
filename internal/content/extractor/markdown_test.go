package extractor

import (
	"strings"
	"testing"
)

func TestRenderMarkdownPreservesStructure(t *testing.T) {
	t.Parallel()

	html := `<h2>Design notes</h2>` +
		`<p>The <strong>fast path</strong> skips validation and the <em>slow path</em> audits it.</p>` +
		`<ul><li>first</li><li>second</li></ul>` +
		`<ol><li>one</li><li>two</li></ol>` +
		`<p>Use <code>config.Reload()</code> at runtime.</p>` +
		`<pre><code>func main() {}</code></pre>` +
		`<p>Deprecated: <del>old API</del>.</p>` +
		`<table><tr><th>Mode</th><th>Cost</th></tr><tr><td>fast</td><td>high</td></tr></table>` +
		`<blockquote><p>Cite your sources.</p></blockquote>` +
		`<p>See <a href="/docs/guide">the guide</a>.</p>`

	markdown, err := renderMarkdown(html, "https://example.com/article")
	if err != nil {
		t.Fatalf("renderMarkdown returned error: %v", err)
	}

	expectations := []struct {
		name string
		want string
	}{
		{"heading", "## Design notes"},
		{"strong", "**fast path**"},
		{"emphasis", "*slow path*"},
		{"unordered list", "- first"},
		{"ordered list", "1. one"},
		{"inline code", "`config.Reload()`"},
		{"code block", "func main() {}"},
		{"strikethrough", "~~old API~~"},
		{"table", "| Mode | Cost |"},
		{"table row", "| fast | high |"},
		{"blockquote", "> Cite your sources."},
		{"absolute link", "](https://example.com/docs/guide)"},
	}

	for _, tc := range expectations {
		if !strings.Contains(markdown, tc.want) {
			t.Errorf("markdown missing %s (%q), got:\n%s", tc.name, tc.want, markdown)
		}
	}
}

func TestRenderMarkdownResolvesRelativeLinksAgainstFinalURL(t *testing.T) {
	t.Parallel()

	markdown, err := renderMarkdown(`<a href="/docs">docs</a>`, "https://example.com/articles/post/1")
	if err != nil {
		t.Fatalf("renderMarkdown returned error: %v", err)
	}

	if !strings.Contains(markdown, "https://example.com/docs") {
		t.Fatalf("expected domain-rooted absolute link, got %q", markdown)
	}
}

func TestRenderMarkdownEmptyInput(t *testing.T) {
	t.Parallel()

	markdown, err := renderMarkdown("", "https://example.com")
	if err != nil {
		t.Fatalf("renderMarkdown returned error: %v", err)
	}
	if markdown != "" {
		t.Fatalf("expected empty output for empty input, got %q", markdown)
	}
}
