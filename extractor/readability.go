package extractor

import (
	"log/slog"

	"github.com/getsentry/sentry-go"
	"github.com/go-shiori/go-readability"
)

type ReadabilityExtractor struct{}

func NewReadabilityExtractor() *ReadabilityExtractor {
	return &ReadabilityExtractor{}
}

func (e *ReadabilityExtractor) GetArticleFromUrl(url string) (Article, error) {
	slog.Info("readability-extractor: requested extraction from URL ", "url", url)

	article, err := readability.FromURL(url, ExtractionTimeout)
	if err != nil {
		slog.Error("readability-extractor: failed extracting from URL", "url", url)
		sentry.CaptureException(err)

		return Article{}, ErrExtractFailed
	}

	slog.Debug("readability-extractor: article extracted", "article", article)

	return Article{
		Title: article.Title,
		Text:  article.TextContent,
		Url:   url,
	}, nil
}
