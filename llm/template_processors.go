package llm

import (
	"bytes"
	"telegram-ollama-reply-bot/config"
	"text/template"
)

// TemplateProcessor handles template processing for LLM prompts
type TemplateProcessor struct {
	chatTemplate      *template.Template
	summarizeTemplate *template.Template
	language          string
	gender            string
	maxSummaryLength  int
}

// NewTemplateProcessor creates a new TemplateProcessor with initialized templates
func NewTemplateProcessor(prompts config.PromptConfig) (*TemplateProcessor, error) {
	chatTmpl, err := template.New("chat").Parse(prompts.ChatSystemPrompt)
	if err != nil {
		return nil, err
	}

	summarizeTmpl, err := template.New("summarize").Parse(prompts.SummarizePrompt)
	if err != nil {
		return nil, err
	}

	return &TemplateProcessor{
		chatTemplate:      chatTmpl,
		summarizeTemplate: summarizeTmpl,
		language:          prompts.Language,
		gender:            prompts.Gender,
		maxSummaryLength:  prompts.MaxSummaryLength,
	}, nil
}

// ProcessChatTemplate processes the chat system prompt template
func (p *TemplateProcessor) ProcessChatTemplate(model, context string) (string, error) {
	var buf bytes.Buffer
	err := p.chatTemplate.Execute(&buf, struct {
		Language string
		Model    string
		Context  string
		Gender   string
	}{
		Language: p.language,
		Model:    model,
		Context:  context,
		Gender:   p.gender,
	})
	if err != nil {
		return "", err
	}
	return buf.String(), nil
}

// ProcessSummarizeTemplate processes the summarize prompt template
func (p *TemplateProcessor) ProcessSummarizeTemplate() (string, error) {
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
