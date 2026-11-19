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
	// RenderCapturedChatSystemPrompt renders the chat persona from a
	// captured template body and the defining settings of an operation
	// snapshot, instead of resolving both live: proactive operations freeze
	// their persona so the decision context and every generation iteration
	// of the resulting turn describe the same character.
	RenderCapturedChatSystemPrompt(ctx context.Context, persona CapturedPersona, chatID int64, model, compactContext, toolPolicy string) (string, error)
	RenderSummarizeSystemPrompt(ctx context.Context, scope PromptScope) (string, error)
	RenderImageRecognitionSystemPrompt(ctx context.Context, scope PromptScope) (string, error)
}

// CapturedPersona is the persona-defining part of an operation snapshot: the
// resolved chat prompt template body plus the character settings, captured
// once when the operation begins and rendered for both the decision context
// and the resulting action turn.
type CapturedPersona struct {
	TemplateBody  string
	Language      string
	Gender        string
	CharacterName string
}

type StaticPromptRenderer struct {
	chatTemplate             *template.Template
	summarizeTemplate        *template.Template
	imageRecognitionTemplate *template.Template
	language                 string
	gender                   string
	characterName            string
	imageRecognitionEnabled  bool
	maxSummaryLength         int
	botUsername              string
}

type AdminPromptRenderer struct {
	config                  *adminconfig.Service
	imageRecognitionEnabled bool
	botUsername             string
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
	ImageRecognitionEnabled bool
	ToolPolicy              string
	// BotUsername is the bot's own Telegram handle without the leading @,
	// from GetMe. Empty when unknown — templates must stay renderable
	// without it (guard with {{if .BotUsername}}).
	BotUsername string
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
		imageRecognitionEnabled:  imageRecognitionEnabled,
		maxSummaryLength:         DefaultMaxSummaryLength,
	}, nil
}

// NewAdminPromptRenderer builds the production prompt renderer. botUsername
// is the bot's own Telegram handle from GetMe (without the leading @) and is
// exposed to chat templates as {{.BotUsername}}; an empty value renders
// templates with the handle clause omitted.
func NewAdminPromptRenderer(config *adminconfig.Service, imageRecognitionEnabled bool, botUsername string) *AdminPromptRenderer {
	return &AdminPromptRenderer{config: config, imageRecognitionEnabled: imageRecognitionEnabled, botUsername: botUsername}
}

func (p *StaticPromptRenderer) RenderChatSystemPrompt(_ context.Context, _ PromptScope, model, compactContext, toolPolicy string) (string, error) {
	return executeTemplate("chat", p.chatTemplate, ChatPromptData{
		Language:                p.language,
		Model:                   model,
		Context:                 compactContext,
		Gender:                  p.gender,
		CharacterName:           p.characterName,
		ImageRecognitionEnabled: p.imageRecognitionEnabled,
		ToolPolicy:              toolPolicy,
		BotUsername:             p.botUsername,
	})
}

// RenderCapturedChatSystemPrompt renders the static renderer's own fixed
// template with the captured persona's settings: static mode has no stored
// template bodies to capture, so only the settings can differ per snapshot.
func (p *StaticPromptRenderer) RenderCapturedChatSystemPrompt(_ context.Context, persona CapturedPersona, _ int64, model, compactContext, toolPolicy string) (string, error) {
	return executeTemplate("chat", p.chatTemplate, ChatPromptData{
		Language:                persona.Language,
		Model:                   model,
		Context:                 compactContext,
		Gender:                  persona.Gender,
		CharacterName:           persona.CharacterName,
		ImageRecognitionEnabled: p.imageRecognitionEnabled,
		ToolPolicy:              toolPolicy,
		BotUsername:             p.botUsername,
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

	return p.renderCapturedChat(ctx, CapturedPersona{
		Language:      resolved.Language,
		Gender:        resolved.Gender,
		CharacterName: resolved.CharacterName,
	}, scope.ChatID, model, compactContext, toolPolicy)
}

// RenderChatSystemPromptResolved renders the chat persona from explicit
// settings values instead of a fresh chat-config resolve: decision contexts
// pass their operation snapshot's fields, so the persona cannot drift from
// the character the resulting action will use. Without a captured body the
// prompt body still resolves through the ordinary per-chat path — the same
// live-template behavior ordinary multi-iteration turns have.
func (p *AdminPromptRenderer) RenderChatSystemPromptResolved(ctx context.Context, chatID int64, language, gender, characterName, model, compactContext, toolPolicy string) (string, error) {
	return p.renderCapturedChat(ctx, CapturedPersona{
		Language:      language,
		Gender:        gender,
		CharacterName: characterName,
	}, chatID, model, compactContext, toolPolicy)
}

// RenderCapturedChatSystemPrompt renders the chat persona from the
// operation's captured template body and defining settings. Proactive
// operations capture both once at snapshot time, so the decision context
// and every generation iteration of the resulting turn describe exactly the
// character the snapshot froze — a template or settings change mid-operation
// cannot split the persona.
func (p *AdminPromptRenderer) RenderCapturedChatSystemPrompt(ctx context.Context, persona CapturedPersona, chatID int64, model, compactContext, toolPolicy string) (string, error) {
	return p.renderCapturedChat(ctx, persona, chatID, model, compactContext, toolPolicy)
}

// ChatPromptBody resolves the effective chat prompt template body: the
// per-chat override, else the global override, else the built-in default.
// Proactive operations capture it once at operation start so the decision
// and its action render the same persona even if the template changes while
// the decision request is in flight.
func (p *AdminPromptRenderer) ChatPromptBody(ctx context.Context, chatID int64) (string, error) {
	return p.promptBody(ctx, adminconfig.PromptFeatureChat, chatID, DefaultChatPromptTemplate)
}

// renderCapturedChat is the shared chat persona rendering: the captured
// body when present, the live-resolved body otherwise, executed with the
// given settings and the renderer's static context.
func (p *AdminPromptRenderer) renderCapturedChat(ctx context.Context, persona CapturedPersona, chatID int64, model, compactContext, toolPolicy string) (string, error) {
	body := persona.TemplateBody
	if body == "" {
		var err error
		if body, err = p.promptBody(ctx, adminconfig.PromptFeatureChat, chatID, DefaultChatPromptTemplate); err != nil {
			return "", err
		}
	}

	tmpl, err := template.New("chat").Parse(body)
	if err != nil {
		return "", err
	}

	return executeTemplate("chat", tmpl, ChatPromptData{
		Language:                persona.Language,
		Model:                   model,
		Context:                 compactContext,
		Gender:                  persona.Gender,
		CharacterName:           persona.CharacterName,
		ImageRecognitionEnabled: p.imageRecognitionEnabled,
		ToolPolicy:              toolPolicy,
		BotUsername:             p.botUsername,
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
