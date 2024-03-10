package extractor

import (
	"errors"
	"github.com/advancedlogic/GoOse"
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
	article, err := e.goose.ExtractFromURL(url)

	if err != nil {
		return Article{}, ErrExtractFailed
	}

	return Article{
		Title: article.Title,
		Text:  article.CleanedText,
		Url:   article.FinalURL,
	}, nil
}
