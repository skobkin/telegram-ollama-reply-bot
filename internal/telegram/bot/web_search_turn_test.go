package bot

import (
	"context"
	"slices"
	"testing"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/content/search"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/llmcontext"
	"telegram-ollama-reply-bot/internal/tooluse"
)

type permissionTestSearcher struct{}

func (permissionTestSearcher) Search(context.Context, search.Request) (search.Result, error) {
	return search.Result{}, nil
}

// The resolved chat config's web search permission must reach the tool
// runtime for every turn, so a disabled chat never sees search_web even
// though a backend is configured.
func TestConversationTurnPassesWebSearchPermissionToToolRuntime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
	}{
		{"enabled chat", true},
		{"disabled chat", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProactiveTestBot(t, nil)
			p.bot.tools = tooluse.New(p.llm, p.history, nil, permissionTestSearcher{}, apiActionSender{api: p.apiBot}, nil, p.bot.logger, tooluse.Config{
				MaxIterations: 6,
				BotIdentity:   tooluse.BotIdentity{UserID: testBotID.ID, Name: testBotID.FirstName},
			})
			p.llm.responses = []llm.Response{{Message: llm.TextMessage(llm.RoleAssistant, "done")}}

			toolStatsTurn(context.Background(), t, p, llmcontext.TriggerMention, adminconfig.ResolvedChatConfig{WebSearchEnabled: tc.enabled})

			p.llm.mu.Lock()
			advertised := slices.Clone(p.llm.lastTools)
			p.llm.mu.Unlock()

			if len(advertised) == 0 {
				t.Fatal("fixture must advertise at least one tool")
			}
			if got := slices.Contains(advertised, "search_web"); got != tc.enabled {
				t.Fatalf("search_web advertised = %v, want %v (tools: %v)", got, tc.enabled, advertised)
			}
		})
	}
}
