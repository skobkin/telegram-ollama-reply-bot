package llm

import (
	"bytes"
	"context"
	"fmt"
	"text/template"
)

type PromptScope struct {
	ChatID int64
}

type PromptRenderer interface {
	RenderChatPrompt(ctx context.Context, scope PromptScope, model, compactContext string) (string, error)
	RenderSummarizePrompt(ctx context.Context, scope PromptScope) (string, error)
	RenderImageRecognitionPrompt(ctx context.Context, scope PromptScope) (string, error)
}

type StaticPromptRenderer struct {
	chatTemplate             *template.Template
	summarizeTemplate        *template.Template
	imageRecognitionTemplate *template.Template
	language                 string
	gender                   string
	characterName            string
	toneMode                 string
	allowTeasing             bool
	maxSummaryLength         int
}

func NewStaticPromptRenderer() (*StaticPromptRenderer, error) {
	chatTmpl, err := template.New("chat").Parse(DefaultChatPromptTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse chat template: %w", err)
	}

	summarizeTmpl, err := template.New("summarize").Parse(DefaultSummarizePromptTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse summarize template: %w", err)
	}

	imageTmpl, err := template.New("image_recognition").Parse(DefaultImageRecognitionPromptTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse image recognition template: %w", err)
	}

	return &StaticPromptRenderer{
		chatTemplate:             chatTmpl,
		summarizeTemplate:        summarizeTmpl,
		imageRecognitionTemplate: imageTmpl,
		language:                 DefaultLanguage,
		gender:                   DefaultGender,
		characterName:            DefaultCharacterName,
		toneMode:                 DefaultToneMode,
		allowTeasing:             DefaultAllowTeasing,
		maxSummaryLength:         DefaultMaxSummaryLength,
	}, nil
}

func ValidatePromptTemplate(name, body string) error {
	if _, err := template.New(name).Parse(body); err != nil {
		return fmt.Errorf("parse template %s: %w", name, err)
	}

	return nil
}

func (p *StaticPromptRenderer) RenderChatPrompt(_ context.Context, _ PromptScope, model, compactContext string) (string, error) {
	var buf bytes.Buffer
	err := p.chatTemplate.Execute(&buf, struct {
		Language      string
		Model         string
		Context       string
		Gender        string
		CharacterName string
		ToneMode      string
		AllowTeasing  bool
	}{
		Language:      p.language,
		Model:         model,
		Context:       compactContext,
		Gender:        p.gender,
		CharacterName: p.characterName,
		ToneMode:      p.toneMode,
		AllowTeasing:  p.allowTeasing,
	})
	if err != nil {
		return "", err
	}

	return buf.String(), nil
}

func (p *StaticPromptRenderer) RenderSummarizePrompt(_ context.Context, _ PromptScope) (string, error) {
	var buf bytes.Buffer
	err := p.summarizeTemplate.Execute(&buf, struct {
		Language  string
		MaxLength int
	}{
		Language:  p.language,
		MaxLength: p.maxSummaryLength,
	})
	if err != nil {
		return "", err
	}

	return buf.String(), nil
}

func (p *StaticPromptRenderer) RenderImageRecognitionPrompt(_ context.Context, _ PromptScope) (string, error) {
	var buf bytes.Buffer
	err := p.imageRecognitionTemplate.Execute(&buf, struct {
		Language string
	}{
		Language: p.language,
	})
	if err != nil {
		return "", err
	}

	return buf.String(), nil
}
