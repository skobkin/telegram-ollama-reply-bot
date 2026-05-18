package chatreply

import (
	"context"
	"errors"
	"testing"

	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/tooluse"
)

type stubPlainResponder struct {
	reply string
	usage *llm.TokenUsage
	err   error
	calls int
}

func (s *stubPlainResponder) HandleChatMessage(context.Context, llm.PromptScope, llm.ChatReplyContext) (string, *llm.TokenUsage, error) {
	s.calls++

	return s.reply, s.usage, s.err
}

type stubToolResponder struct {
	reply string
	usage *llm.TokenUsage
	err   error
	calls int
}

func (s *stubToolResponder) ReplyWithTools(context.Context, tooluse.ChatRequest) (string, *llm.TokenUsage, error) {
	s.calls++

	return s.reply, s.usage, s.err
}

func TestServiceUsesToolResponderWhenAvailable(t *testing.T) {
	plain := &stubPlainResponder{reply: "plain"}
	tools := &stubToolResponder{reply: "tools", usage: &llm.TokenUsage{TotalTokens: 7}}
	service := New(plain, tools)

	reply, usage, err := service.HandleChatMessage(context.Background(), Request{
		Scope:        state.ConversationScope{ChatID: 1},
		PromptScope:  llm.PromptScope{ChatID: 1},
		ReplyContext: llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "hi")},
	})
	if err != nil {
		t.Fatalf("HandleChatMessage() error = %v", err)
	}
	if reply != "tools" {
		t.Fatalf("unexpected reply: %q", reply)
	}
	if usage == nil || usage.TotalTokens != 7 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
	if plain.calls != 0 {
		t.Fatalf("plain responder should not be called, got %d", plain.calls)
	}
}

func TestServiceFallsBackToPlainChatWhenToolUseUnavailable(t *testing.T) {
	plain := &stubPlainResponder{reply: "plain", usage: &llm.TokenUsage{TotalTokens: 3}}
	tools := &stubToolResponder{err: errors.Join(tooluse.ErrToolUseUnavailable, errors.New("boom"))}
	service := New(plain, tools)

	reply, usage, err := service.HandleChatMessage(context.Background(), Request{
		Scope:        state.ConversationScope{ChatID: 1},
		PromptScope:  llm.PromptScope{ChatID: 1},
		ReplyContext: llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "hi")},
	})
	if err != nil {
		t.Fatalf("HandleChatMessage() error = %v", err)
	}
	if reply != "plain" {
		t.Fatalf("unexpected reply: %q", reply)
	}
	if usage == nil || usage.TotalTokens != 3 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
	if plain.calls != 1 {
		t.Fatalf("expected plain fallback call, got %d", plain.calls)
	}
}

func TestServiceReturnsToolErrorAfterLoopStarts(t *testing.T) {
	plain := &stubPlainResponder{reply: "plain"}
	tools := &stubToolResponder{usage: &llm.TokenUsage{TotalTokens: 2}, err: tooluse.ErrToolLoopLimitReached}
	service := New(plain, tools)

	_, usage, err := service.HandleChatMessage(context.Background(), Request{
		Scope:        state.ConversationScope{ChatID: 1},
		PromptScope:  llm.PromptScope{ChatID: 1},
		ReplyContext: llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "hi")},
	})
	if !errors.Is(err, tooluse.ErrToolLoopLimitReached) {
		t.Fatalf("expected ErrToolLoopLimitReached, got %v", err)
	}
	if usage == nil || usage.TotalTokens != 2 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
	if plain.calls != 0 {
		t.Fatalf("plain responder should not be used, got %d calls", plain.calls)
	}
}
