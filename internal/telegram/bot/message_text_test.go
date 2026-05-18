package bot

import (
	"testing"

	tg "github.com/mymmrac/telego"
)

func TestExtractMessageTextUsesTextWhenPresent(t *testing.T) {
	message := tg.Message{
		Text: "Check https://example.com/a",
	}

	if got := extractMessageText(message); got != "Check https://example.com/a" {
		t.Fatalf("unexpected text: %q", got)
	}
}

func TestExtractMessageTextFallsBackToCaption(t *testing.T) {
	message := tg.Message{
		Caption: "Photo with https://example.com/caption",
	}

	if got := extractMessageText(message); got != "Photo with https://example.com/caption" {
		t.Fatalf("unexpected text: %q", got)
	}
}

func TestExtractMessageTextAppendsHiddenTextLinkURL(t *testing.T) {
	message := tg.Message{
		Text: "Read this article",
		Entities: []tg.MessageEntity{
			{Type: tg.EntityTypeTextLink, URL: "https://example.com/article"},
		},
	}

	want := "Read this article\nhttps://example.com/article"
	if got := extractMessageText(message); got != want {
		t.Fatalf("unexpected text: %q", got)
	}
}

func TestExtractMessageTextUsesCaptionEntitiesForHiddenLinks(t *testing.T) {
	message := tg.Message{
		Caption: "Attached source",
		CaptionEntities: []tg.MessageEntity{
			{Type: tg.EntityTypeTextLink, URL: "https://example.com/source"},
			{Type: tg.EntityTypeTextLink, URL: "https://example.com/source"},
		},
	}

	want := "Attached source\nhttps://example.com/source"
	if got := extractMessageText(message); got != want {
		t.Fatalf("unexpected text: %q", got)
	}
}
