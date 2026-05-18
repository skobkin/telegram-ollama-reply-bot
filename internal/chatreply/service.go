package chatreply

import (
	"context"
	"errors"

	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/tooluse"
)

type plainResponder interface {
	HandleChatMessage(ctx context.Context, scope llm.PromptScope, requestContext llm.ChatReplyContext) (string, *llm.TokenUsage, error)
}

type toolResponder interface {
	ReplyWithTools(ctx context.Context, req tooluse.ChatRequest) (string, *llm.TokenUsage, error)
}

type Service struct {
	plain plainResponder
	tools toolResponder
}

func New(plain plainResponder, tools toolResponder) *Service {
	if plain == nil {
		panic("plain responder is required")
	}

	return &Service{
		plain: plain,
		tools: tools,
	}
}

type Request struct {
	Scope          state.ConversationScope
	PromptScope    llm.PromptScope
	ReplyContext   llm.ChatReplyContext
	RequestMessage state.Message
}

func (s *Service) HandleChatMessage(ctx context.Context, req Request) (string, *llm.TokenUsage, error) {
	if s.tools == nil {
		return s.plain.HandleChatMessage(ctx, req.PromptScope, req.ReplyContext)
	}

	reply, usage, err := s.tools.ReplyWithTools(ctx, tooluse.ChatRequest{
		Scope:          req.Scope,
		PromptScope:    req.PromptScope,
		ReplyContext:   req.ReplyContext,
		RequestMessage: req.RequestMessage,
	})
	if err == nil {
		return reply, usage, nil
	}

	if !errors.Is(err, tooluse.ErrToolUseUnavailable) {
		return "", usage, err
	}

	return s.plain.HandleChatMessage(ctx, req.PromptScope, req.ReplyContext)
}
