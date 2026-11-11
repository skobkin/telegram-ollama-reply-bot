package extractor

import (
	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/strikethrough"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
)

// markdownConverter is shared across extractions: the converter holds no
// per-document state and is safe for concurrent use. The table and
// strikethrough plugins preserve GFM constructs that carry meaning for LLM
// consumption.
var markdownConverter = converter.NewConverter(
	converter.WithPlugins(
		base.NewBasePlugin(),
		commonmark.NewCommonmarkPlugin(),
		table.NewTablePlugin(),
		strikethrough.NewStrikethroughPlugin(),
	),
)

// renderMarkdown converts extracted article HTML to Markdown. baseURL (the
// final page URL) resolves relative links and image sources to absolute ones.
func renderMarkdown(htmlSrc, baseURL string) (string, error) {
	if htmlSrc == "" {
		return "", nil
	}

	opts := make([]converter.ConvertOptionFunc, 0, 1)
	if baseURL != "" {
		opts = append(opts, converter.WithDomain(baseURL))
	}

	markdown, err := markdownConverter.ConvertString(htmlSrc, opts...)
	if err != nil {
		return "", err
	}

	return markdown, nil
}
