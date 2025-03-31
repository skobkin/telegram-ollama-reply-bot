package extractor

import (
	"context"
	"log/slog"

	goose "github.com/advancedlogic/GoOse"
	"github.com/getsentry/sentry-go"
)

type GoOseExtractor struct {
	goose *goose.Goose
}

func NewGoOseExtractor() *GoOseExtractor {
	gooseExtractor := goose.New()
	return &GoOseExtractor{
		goose: &gooseExtractor,
	}
}

func (e *GoOseExtractor) GetArticleFromUrl(url string) (Article, error) {
	slog.Info("goose-extractor: requested extraction from URL ", "url", url)

	ctx, cancel := context.WithTimeout(context.Background(), ExtractionTimeout)
	defer cancel()

	resultChan := make(chan struct {
		article *goose.Article
		err     error
	})

	go func() {
		article, err := e.goose.ExtractFromURL(url)
		resultChan <- struct {
			article *goose.Article
			err     error
		}{article, err}
	}()

	select {
	case result := <-resultChan:
		if result.err != nil {
			slog.Error("goose-extractor: failed extracting from URL", "url", url)
			sentry.CaptureException(result.err)
			
			return Article{}, ErrExtractFailed
		}

		slog.Debug("goose-extractor: article extracted", "article", result.article)

		return Article{
			Title: result.article.Title,
			Text:  result.article.CleanedText,
			Url:   result.article.FinalURL,
		}, nil
	case <-ctx.Done():
		slog.Error("goose-extractor: extraction timed out", "url", url)
		sentry.CaptureMessage("Article extraction timed out")
		return Article{}, ErrExtractFailed
	}
}
