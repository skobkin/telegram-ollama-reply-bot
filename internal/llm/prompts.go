package llm

import (
	"bytes"
	"context"
	"fmt"
	"text/template"

	"telegram-ollama-reply-bot/internal/adminconfig"
)

type PromptScope struct {
	ChatID int64
}

type PromptRenderer interface {
	RenderChatSystemPrompt(ctx context.Context, scope PromptScope, model, compactContext, toolPolicy string) (string, error)
	RenderSummarizeSystemPrompt(ctx context.Context, scope PromptScope) (string, error)
	RenderImageRecognitionSystemPrompt(ctx context.Context, scope PromptScope) (string, error)
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
	imageRecognitionEnabled  bool
	maxSummaryLength         int
}

type AdminPromptRenderer struct {
	config                  *adminconfig.Service
	imageRecognitionEnabled bool
}

// ChatPromptData is the data contract of chat system prompts. The renderers
// and template validation share this single source of truth: every field
// here is a legitimate template placeholder for the chat feature.
type ChatPromptData struct {
	Language                string
	Model                   string
	Context                 string
	Gender                  string
	CharacterName           string
	ToneMode                string
	AllowTeasing            bool
	ImageRecognitionEnabled bool
	ToolPolicy              string
}

// SummarizePromptData is the data contract of summarize system prompts.
type SummarizePromptData struct {
	Language  string
	MaxLength int
}

// ImageRecognitionPromptData is the data contract of image recognition
// system prompts.
type ImageRecognitionPromptData struct {
	Language string
}

func NewStaticPromptRenderer(imageRecognitionEnabled bool) (*StaticPromptRenderer, error) {
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
		imageRecognitionEnabled:  imageRecognitionEnabled,
		maxSummaryLength:         DefaultMaxSummaryLength,
	}, nil
}

func NewAdminPromptRenderer(config *adminconfig.Service, imageRecognitionEnabled bool) *AdminPromptRenderer {
	return &AdminPromptRenderer{config: config, imageRecognitionEnabled: imageRecognitionEnabled}
}

func (p *StaticPromptRenderer) RenderChatSystemPrompt(_ context.Context, _ PromptScope, model, compactContext, toolPolicy string) (string, error) {
	return executeTemplate("chat", p.chatTemplate, ChatPromptData{
		Language:                p.language,
		Model:                   model,
		Context:                 compactContext,
		Gender:                  p.gender,
		CharacterName:           p.characterName,
		ToneMode:                p.toneMode,
		AllowTeasing:            p.allowTeasing,
		ImageRecognitionEnabled: p.imageRecognitionEnabled,
		ToolPolicy:              toolPolicy,
	})
}

func (p *StaticPromptRenderer) RenderSummarizeSystemPrompt(_ context.Context, _ PromptScope) (string, error) {
	return executeTemplate("summarize", p.summarizeTemplate, SummarizePromptData{
		Language:  p.language,
		MaxLength: p.maxSummaryLength,
	})
}

func (p *StaticPromptRenderer) RenderImageRecognitionSystemPrompt(_ context.Context, _ PromptScope) (string, error) {
	return executeTemplate("image_recognition", p.imageRecognitionTemplate, ImageRecognitionPromptData{
		Language: p.language,
	})
}

func (p *AdminPromptRenderer) RenderChatSystemPrompt(ctx context.Context, scope PromptScope, model, compactContext, toolPolicy string) (string, error) {
	resolved, err := p.config.ResolveChatConfig(ctx, scope.ChatID)
	if err != nil {
		return "", err
	}

	body, err := p.promptBody(ctx, adminconfig.PromptFeatureChat, scope.ChatID, DefaultChatPromptTemplate)
	if err != nil {
		return "", err
	}

	tmpl, err := template.New("chat").Parse(body)
	if err != nil {
		return "", err
	}

	return executeTemplate("chat", tmpl, ChatPromptData{
		Language:                resolved.Language,
		Model:                   model,
		Context:                 compactContext,
		Gender:                  resolved.Gender,
		CharacterName:           resolved.CharacterName,
		ToneMode:                resolved.ToneMode,
		AllowTeasing:            resolved.AllowTeasing,
		ImageRecognitionEnabled: p.imageRecognitionEnabled,
		ToolPolicy:              toolPolicy,
	})
}

func (p *AdminPromptRenderer) RenderSummarizeSystemPrompt(ctx context.Context, scope PromptScope) (string, error) {
	resolved, err := p.config.ResolveChatConfig(ctx, scope.ChatID)
	if err != nil {
		return "", err
	}

	body, err := p.promptBody(ctx, adminconfig.PromptFeatureSummarize, scope.ChatID, DefaultSummarizePromptTemplate)
	if err != nil {
		return "", err
	}

	tmpl, err := template.New("summarize").Parse(body)
	if err != nil {
		return "", err
	}

	return executeTemplate("summarize", tmpl, SummarizePromptData{
		Language:  resolved.Language,
		MaxLength: DefaultMaxSummaryLength,
	})
}

func (p *AdminPromptRenderer) RenderImageRecognitionSystemPrompt(ctx context.Context, scope PromptScope) (string, error) {
	resolved, err := p.config.ResolveChatConfig(ctx, scope.ChatID)
	if err != nil {
		return "", err
	}

	body, err := p.promptBody(ctx, adminconfig.PromptFeatureImageRecognition, scope.ChatID, DefaultImageRecognitionPromptTemplate)
	if err != nil {
		return "", err
	}

	tmpl, err := template.New("image_recognition").Parse(body)
	if err != nil {
		return "", err
	}

	return executeTemplate("image_recognition", tmpl, ImageRecognitionPromptData{
		Language: resolved.Language,
	})
}

func (p *AdminPromptRenderer) promptBody(ctx context.Context, feature adminconfig.PromptFeature, chatID int64, fallback string) (string, error) {
	body, ok, err := p.config.Store().GetPromptTemplate(ctx, feature, chatID)
	if err != nil {
		return "", err
	}
	if ok {
		return body, nil
	}
	if chatID != 0 {
		body, ok, err = p.config.Store().GetPromptTemplate(ctx, feature, 0)
		if err != nil {
			return "", err
		}
		if ok {
			return body, nil
		}
	}

	return fallback, nil
}

func executeTemplate(name string, tmpl *template.Template, data any) (string, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute template %s: %w", name, err)
	}

	return buf.String(), nil
}
