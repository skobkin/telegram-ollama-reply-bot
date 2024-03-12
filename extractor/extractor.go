package extractor

import (
	"errors"
	"github.com/advancedlogic/GoOse"
	"log/slog"
)

var (
	ErrExtractFailed = errors.New("extraction failed")
)

type Extractor struct {
	goose *goose.Goose
}

func NewExtractor() *Extractor {
	gooseExtractor := goose.New()

	return &Extractor{
		goose: &gooseExtractor,
	}
}

type Article struct {
	Title string
	Text  string
	Url   string
}

func (e *Extractor) GetArticleFromUrl(url string) (Article, error) {
	slog.Info("extractor: requested extraction from URL ", "url", url)

	article, err := e.goose.ExtractFromURL(url)

	if err != nil {
		slog.Error("extractor: failed extracting from URL", "url", url)

		return Article{}, ErrExtractFailed
	}

	slog.Debug("extractor: article extracted", "article", article)

	return Article{
		Title: article.Title,
		Text:  article.CleanedText,
		Url:   article.FinalURL,
	}, nil
}
