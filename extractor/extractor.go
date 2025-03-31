package extractor

import (
	"errors"
	"log/slog"
	"time"

	"github.com/getsentry/sentry-go"
)

const (
	ExtractionTimeout = 10 * time.Second
)

var (
	ErrExtractFailed = errors.New("extraction failed")
)

type Article struct {
	Title string
	Text  string
	Url   string
}

type Extractor interface {
	GetArticleFromUrl(url string) (Article, error)
}

type MultiExtractor struct {
	primary  Extractor
	fallback Extractor
}

func NewMultiExtractor() *MultiExtractor {
	return &MultiExtractor{
		primary:  NewReadabilityExtractor(),
		fallback: NewGoOseExtractor(),
	}
}

func (e *MultiExtractor) GetArticleFromUrl(url string) (Article, error) {
	slog.Info("multi-extractor: requested extraction from URL ", "url", url)

	article, err := e.primary.GetArticleFromUrl(url)
	if err == nil && article.Text != "" {
		slog.Info("multi-extractor: successfully extracted using primary extractor")

		return article, nil
	} else if err != nil {
		slog.Error("multi-extractor: primary extractor failed", "url", url, "error", err)
		sentry.CaptureException(err)
	}

	slog.Info("multi-extractor: primary extractor failed or returned empty text, trying fallback")
	article, err = e.fallback.GetArticleFromUrl(url)
	if err == nil && article.Text != "" {
		slog.Info("multi-extractor: successfully extracted using fallback extractor")

		return article, nil
	} else if err != nil {
		slog.Error("multi-extractor: fallback extractor failed", "url", url, "error", err)
		sentry.CaptureException(err)
	}

	slog.Error("multi-extractor: both extractors failed", "url", url)

	return Article{}, ErrExtractFailed
}

func NewExtractor() Extractor {
	return NewMultiExtractor()
}
