package bot

import (
	"context"
	"slices"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/imagegen"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/llmcontext"
	"telegram-ollama-reply-bot/internal/tooluse"
)

type turnTestImageGenerator struct{}

func (turnTestImageGenerator) Name() string           { return "stub" }
func (turnTestImageGenerator) Timeout() time.Duration { return time.Minute }
func (turnTestImageGenerator) Generate(context.Context, imagegen.Request) (imagegen.Image, error) {
	return imagegen.Image{}, nil
}

// The resolved chat config's image generation permission and model must reach
// the tool runtime for every turn: a disabled chat, or one with no model
// selected, never sees generate_image even though a backend is attached.
func TestConversationTurnPassesImageGenerationSettingsToToolRuntime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		model   string
		want    bool
	}{
		{"enabled with a model", true, "MiniMax-H3-Image", true},
		{"disabled chat", false, "MiniMax-H3-Image", false},
		{"enabled without a model", true, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProactiveTestBot(t, nil)
			p.bot.tools = tooluse.New(p.llm, p.history, nil, nil, apiActionSender{api: p.apiBot}, nil, p.bot.logger, tooluse.Config{
				MaxIterations: 6,
				BotIdentity:   tooluse.BotIdentity{UserID: testBotID.ID, Name: testBotID.FirstName},
			})
			p.bot.tools.AttachImageGenerator(turnTestImageGenerator{}, 0)
			p.llm.responses = []llm.Response{{Message: llm.TextMessage(llm.RoleAssistant, "done")}}

			toolStatsTurn(context.Background(), t, p, llmcontext.TriggerMention, adminconfig.ResolvedChatConfig{
				ImageGenerationEnabled: tc.enabled,
				ImageGenerationModel:   tc.model,
			})

			p.llm.mu.Lock()
			advertised := slices.Clone(p.llm.lastTools)
			p.llm.mu.Unlock()

			if len(advertised) == 0 {
				t.Fatal("fixture must advertise at least one tool")
			}
			if got := slices.Contains(advertised, "generate_image"); got != tc.want {
				t.Fatalf("generate_image advertised = %v, want %v (tools: %v)", got, tc.want, advertised)
			}
		})
	}
}
