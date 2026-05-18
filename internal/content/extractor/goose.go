package extractor

import (
	"context"
	"log/slog"
	"telegram-ollama-reply-bot/internal/logging"

	goose "github.com/advancedlogic/GoOse/pkg/goose"
	"github.com/getsentry/sentry-go"
)

type GoOseExtractor struct {
	goose  *goose.Goose
	logger *slog.Logger
}

func NewGoOseExtractor(logger *slog.Logger) *GoOseExtractor {
	gooseExtractor := goose.New()

	return &GoOseExtractor{
		goose:  &gooseExtractor,
		logger: logger,
	}
}

func (e *GoOseExtractor) GetArticleFromURL(ctx context.Context, url string) (Article, error) {
	logger := logging.FromContext(ctx, e.logger)
	logger.Info("extracting article", "url", url)

	timeoutCtx, cancel := context.WithTimeout(context.Background(), ExtractionTimeout)
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
			logger.Warn("extraction failed", "url", url, "error", result.err)
			sentry.CaptureException(result.err)

			return Article{}, ErrExtractFailed
		}

		logger.Debug("article extracted", "url", url, "text_length", len(result.article.CleanedText))

		return Article{
			Title: result.article.Title,
			Text:  result.article.CleanedText,
			URL:   result.article.FinalURL,
		}, nil
	case <-timeoutCtx.Done():
		logger.Error("extraction timed out", "url", url, "timeout", ExtractionTimeout.String())
		sentry.CaptureMessage("Article extraction timed out")

		return Article{}, ErrExtractFailed
	}
}
