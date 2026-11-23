package msgdesc

import (
	"strings"
	"testing"
	"unicode/utf8"

	tg "github.com/mymmrac/telego"

	"telegram-ollama-reply-bot/internal/state"
)

func TestDescribe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		msg      *tg.Message
		wantKind state.AttachmentKind
		want     string
	}{
		{"sticker with emoji and set", &tg.Message{Sticker: &tg.Sticker{Emoji: "😀", SetName: "catpack"}}, state.AttachmentSticker, "😀 (catpack)"},
		{"sticker without set", &tg.Message{Sticker: &tg.Sticker{Emoji: "👍"}}, state.AttachmentSticker, "👍"},
		{"poll", &tg.Message{Poll: &tg.Poll{Question: "Dinner?", Type: "regular", Options: []tg.PollOption{{Text: "Pizza"}, {Text: "Sushi"}}}}, state.AttachmentPoll, `"Dinner?" — Pizza / Sushi`},
		{"quiz", &tg.Message{Poll: &tg.Poll{Question: "2+2?", Type: "quiz", Options: []tg.PollOption{{Text: "3"}, {Text: "4"}}, CorrectOptionIDs: []int{1}}}, state.AttachmentQuiz, `"2+2?" — 3 / 4`},
		{"dice", &tg.Message{Dice: &tg.Dice{Emoji: "🎲", Value: 4}}, state.AttachmentDice, "🎲 = 4"},
		{"animation wins over its document", &tg.Message{Animation: &tg.Animation{Duration: 3}, Document: &tg.Document{FileName: "x.mp4"}}, state.AttachmentAnimation, "0:03"},
		{"video note wins over video", &tg.Message{VideoNote: &tg.VideoNote{Duration: 65}, Video: &tg.Video{Duration: 65}}, state.AttachmentVideoNote, "1:05"},
		{"video", &tg.Message{Video: &tg.Video{Duration: 600}}, state.AttachmentVideo, "10:00"},
		{"voice", &tg.Message{Voice: &tg.Voice{Duration: 12}}, state.AttachmentVoice, "0:12"},
		{"voice without duration", &tg.Message{Voice: &tg.Voice{}}, state.AttachmentVoice, ""},
		{"audio", &tg.Message{Audio: &tg.Audio{Performer: "Daft Punk", Title: "Aerodynamic", Duration: 212}}, state.AttachmentAudio, "Daft Punk — Aerodynamic (3:32)"},
		{"audio title only", &tg.Message{Audio: &tg.Audio{Title: "Song"}}, state.AttachmentAudio, "Song"},
		{"document", &tg.Message{Document: &tg.Document{FileName: "report.pdf"}}, state.AttachmentDocument, "report.pdf"},
		{"location", &tg.Message{Location: &tg.Location{Latitude: 52.5200, Longitude: 13.4050}}, state.AttachmentLocation, ""},
		{"contact", &tg.Message{Contact: &tg.Contact{PhoneNumber: "+15551234567", FirstName: "Mallory"}}, state.AttachmentContact, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Describe(tt.msg)
			if got == nil {
				t.Fatal("expected an attachment")
			}
			if got.Kind != tt.wantKind || got.Label != tt.want {
				t.Fatalf("Describe() = {%s %q}, want {%s %q}", got.Kind, got.Label, tt.wantKind, tt.want)
			}
		})
	}
}

func TestDescribeReturnsNilForOrdinaryMessages(t *testing.T) {
	t.Parallel()

	for name, msg := range map[string]*tg.Message{
		"nil":     nil,
		"text":    {Text: "hello"},
		"caption": {Caption: "look"},
		"photo":   {Photo: []tg.PhotoSize{{FileID: "f"}}}, // keeps the dedicated image path
		"empty":   {},
	} {
		if got := Describe(msg); got != nil {
			t.Errorf("%s: expected nil, got %+v", name, got)
		}
	}
}

// Contacts and locations identify people and places; none of that may reach
// the retained description.
func TestDescribeNeverRetainsIdentifyingData(t *testing.T) {
	t.Parallel()

	contact := Describe(&tg.Message{Contact: &tg.Contact{PhoneNumber: "+15551234567", FirstName: "Mallory", LastName: "Smith", Vcard: "BEGIN:VCARD"}})
	location := Describe(&tg.Message{Location: &tg.Location{Latitude: 52.52, Longitude: 13.405}})

	for _, attachment := range []*state.Attachment{contact, location} {
		rendered := attachment.Placeholder()
		for _, secret := range []string{"1555", "Mallory", "Smith", "VCARD", "52.5", "13.4"} {
			if strings.Contains(rendered, secret) {
				t.Errorf("%q leaks %q", rendered, secret)
			}
		}
	}
}

func TestPollLabelIsBoundedAndSafe(t *testing.T) {
	t.Parallel()

	options := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		options = append(options, strings.Repeat("o", 100))
	}
	got := Poll(strings.Repeat("q", 500)+"\n\x00evil", options, false)

	if utf8.RuneCountInString(got.Label) > maxLabelRunes {
		t.Fatalf("label exceeds %d runes: %d", maxLabelRunes, utf8.RuneCountInString(got.Label))
	}
	if strings.ContainsAny(got.Label, "\n\x00\r\t") {
		t.Fatalf("label must be single-line and free of control characters: %q", got.Label)
	}

	few := Poll("Pick", []string{"a", "b", "c", "d", "e", "f", "g", "h"}, false)
	if !strings.Contains(few.Label, "a / b / c / d / e / f") || !strings.Contains(few.Label, "(+2 more)") {
		t.Fatalf("only the first %d options are listed with a remainder note: %q", maxPollOptions, few.Label)
	}
}

func TestQuizLabelDoesNotRevealTheAnswer(t *testing.T) {
	t.Parallel()

	got := Poll("Capital of France?", []string{"Berlin", "Paris"}, true)
	if got.Kind != state.AttachmentQuiz {
		t.Fatalf("kind = %s", got.Kind)
	}
	// The builder has no input for the correct option at all: the answer
	// cannot leak. Both options must be listed without any marker.
	if strings.Contains(got.Label, "✓") || strings.Contains(strings.ToLower(got.Label), "correct") {
		t.Fatalf("quiz label must not mark an answer: %q", got.Label)
	}
}

func TestChartAndDiceLabels(t *testing.T) {
	t.Parallel()

	if got := Chart("line", "Monthly sales"); got.Kind != state.AttachmentChart || got.Label != "line: Monthly sales" {
		t.Fatalf("chart = %+v", got)
	}
	if got := Chart("pie", ""); got.Label != "pie" {
		t.Fatalf("untitled chart = %+v", got)
	}
	if got := Chart("bar", strings.Repeat("t", 200)); utf8.RuneCountInString(got.Label) > maxLabelRunes {
		t.Fatalf("chart label unbounded: %d runes", utf8.RuneCountInString(got.Label))
	}
	if got := Dice("🎯", 6); got.Label != "🎯 = 6" {
		t.Fatalf("dice = %+v", got)
	}
	if got := Dice("🎲", 0); got.Label != "🎲" {
		t.Fatalf("a dice with an unknown outcome must not claim zero: %+v", got)
	}
	if got := Sticker("😀", strings.Repeat("d", 300)); utf8.RuneCountInString(got.Label) > maxLabelRunes {
		t.Fatalf("sticker label unbounded: %d runes", utf8.RuneCountInString(got.Label))
	}
}

func TestGeneratedImageLabel(t *testing.T) {
	t.Parallel()

	if got := GeneratedImage("", "a cat in a hat"); got.Kind != state.AttachmentGeneratedImage || got.Label != "a cat in a hat" {
		t.Fatalf("prompt fallback = %+v", got)
	}
	if got := GeneratedImage("Meet Whiskers", "a cat in a hat"); got.Label != "Meet Whiskers" {
		t.Fatalf("a caption must win over the prompt: %+v", got)
	}
	if got := GeneratedImage("  \n ", "  sunset\nover  the sea "); got.Label != "sunset over the sea" {
		t.Fatalf("whitespace must be normalized and a blank caption ignored: %+v", got)
	}
	if got := GeneratedImage("", strings.Repeat("p", 500)); utf8.RuneCountInString(got.Label) > maxGeneratedImageRunes {
		t.Fatalf("prompt excerpt unbounded: %d runes", utf8.RuneCountInString(got.Label))
	}
	if got := GeneratedImage(strings.Repeat("c", 500), "p"); utf8.RuneCountInString(got.Label) > maxGeneratedImageRunes {
		t.Fatalf("caption excerpt unbounded: %d runes", utf8.RuneCountInString(got.Label))
	}
	if got := GeneratedImage("", ""); got.Label != "" || got.Placeholder() != "[Generated image]" {
		t.Fatalf("an empty description must still render a placeholder: %+v", got)
	}
}

func TestStickerLabelUsesCatalogDescription(t *testing.T) {
	t.Parallel()

	if got := Sticker("😀", "cat waving"); got.Placeholder() != "[Sticker: 😀 (cat waving)]" {
		t.Fatalf("sticker = %q", got.Placeholder())
	}
}

func TestCleanCollapsesWhitespaceAndControls(t *testing.T) {
	t.Parallel()

	for input, want := range map[string]string{
		"  a \n\t b\u0000c  ": "a b c",
		"":                    "",
		"\n\n":                "",
		"plain":               "plain",
	} {
		if got := clean(input); got != want {
			t.Errorf("clean(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestTruncateKeepsRunesIntact(t *testing.T) {
	t.Parallel()

	if got := truncate("héllo wörld", 5); got != "héll…" || !utf8.ValidString(got) {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncate("short", 10); got != "short" {
		t.Fatalf("truncate = %q", got)
	}
}
