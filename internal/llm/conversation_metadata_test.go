package llm

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/config"
)

func TestMetadataAppendedWithCustomAndCapturedPersonas(t *testing.T) {
	store := &promptStoreStub{prompts: map[string]string{"chat:0": "CUSTOM PERSONA without context placeholders"}}
	renderer := NewAdminPromptRenderer(adminconfig.NewService(store, nil, nil), false, "")
	service := &Service{cfg: config.LLMConfig{Features: config.FeatureConfig{Chat: config.FeatureRouteConfig{Model: "model"}}}, prompts: renderer, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, captured := range []*CapturedPersona{nil, {TemplateBody: "CAPTURED PERSONA without context placeholders"}} {
		reply := ChatReplyContext{Persona: captured, EarlierSummary: "SUMMARY", History: []Message{TextMessage(RoleUser, "history")}, UserMessage: TextMessage(RoleUser, "trigger"), RetainedHistory: &ConversationMetadata{RetainedMessages: 8}}
		for i := 0; i < 2; i++ {
			request, err := service.BuildChatToolRequest(context.Background(), PromptScope{}, reply, "pinned-model", nil, "policy", []Message{TextMessage(RoleTool, "tool result")}, false)
			if err != nil {
				t.Fatal(err)
			}
			system := request.Messages[0].Text()
			for _, want := range []string{"PERSONA without context placeholders", "retained_messages=8", "retained_bounds=unavailable", "conversation_messages_in_context=2", "not complete Telegram history", "SUMMARY"} {
				if !strings.Contains(system, want) {
					t.Fatalf("missing %q: %s", want, system)
				}
			}
			if strings.Count(system, "Conversation metadata") != 1 || request.Model != "pinned-model" {
				t.Fatal(system, request.Model)
			}
		}
	}
}

func TestMetadataClockAndHistoryAvailability(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.FixedZone("Test/Zone", 4*3600))
	for _, tc := range []struct {
		metadata *ConversationMetadata
		want     string
	}{
		{nil, "retained_messages=unavailable; retained_bounds=unavailable"},
		{&ConversationMetadata{}, "retained_messages=0; retained_bounds=empty"},
		{&ConversationMetadata{RetainedMessages: 1, OldestMessageAt: now, NewestMessageAt: now}, "retained_bounds=2026-11-20T06:00:00Z..2026-11-20T06:00:00Z"},
	} {
		text := renderConversationMetadata(ChatReplyContext{RetainedHistory: tc.metadata}, now)
		if !strings.Contains(text, tc.want) || !strings.Contains(text, "now=2026-11-20T10:00:00+04:00") || !strings.Contains(text, "timezone=Test/Zone (UTC+04:00)") || strings.Contains(text, "0001-") {
			t.Fatal(text)
		}
	}
}
