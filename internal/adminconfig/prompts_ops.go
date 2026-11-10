package adminconfig

import (
	"context"
	"fmt"
)

// PromptSource reports where an effective prompt body comes from.
type PromptSource string

const (
	// PromptSourceChat is a chat-scoped stored template.
	PromptSourceChat PromptSource = "chat"
	// PromptSourceGlobal is a globally stored template.
	PromptSourceGlobal PromptSource = "global"
	// PromptSourceBuiltin is the compiled-in template, used when nothing is
	// stored for the scope (or after a restore-to-builtin reset).
	PromptSourceBuiltin PromptSource = "builtin"
)

// PromptStatus summarizes one prompt feature for the prompts screens.
type PromptStatus struct {
	Feature         PromptFeature
	HasChatStored   bool
	HasGlobalStored bool
	Effective       PromptSource
}

// BuiltinPrompt returns the built-in template body for a feature verbatim
// (placeholders intact), or false when no builtin is registered.
func (s *Service) BuiltinPrompt(feature PromptFeature) (string, bool) {
	body, ok := s.builtins[feature]

	return body, ok
}

// PromptOverview reports the prompt status of every feature for a scope.
// chatID == 0 inspects the global scope only. Resolution precedence mirrors
// the runtime renderer: chat → global → builtin.
func (s *Service) PromptOverview(ctx context.Context, chatID int64) ([]PromptStatus, error) {
	overview := make([]PromptStatus, 0, len(s.PromptFeatures()))
	for _, feature := range s.PromptFeatures() {
		status := PromptStatus{Feature: feature, Effective: PromptSourceBuiltin}

		_, hasGlobal, err := s.store.GetPromptTemplate(ctx, feature, 0)
		if err != nil {
			return nil, err
		}
		status.HasGlobalStored = hasGlobal
		if hasGlobal {
			status.Effective = PromptSourceGlobal
		}

		if chatID != 0 {
			_, hasChat, err := s.store.GetPromptTemplate(ctx, feature, chatID)
			if err != nil {
				return nil, err
			}
			status.HasChatStored = hasChat
			if hasChat {
				status.Effective = PromptSourceChat
			}
		}

		overview = append(overview, status)
	}

	return overview, nil
}

// ActivePrompt resolves the effective prompt body for a feature and scope
// with its source (chat → global → builtin).
func (s *Service) ActivePrompt(ctx context.Context, feature PromptFeature, chatID int64) (string, PromptSource, error) {
	if !IsPromptFeature(feature) {
		return "", "", fmt.Errorf("unknown prompt feature %q", feature)
	}

	if chatID != 0 {
		body, ok, err := s.store.GetPromptTemplate(ctx, feature, chatID)
		if err != nil {
			return "", "", err
		}
		if ok {
			return body, PromptSourceChat, nil
		}
	}

	body, ok, err := s.store.GetPromptTemplate(ctx, feature, 0)
	if err != nil {
		return "", "", err
	}
	if ok {
		return body, PromptSourceGlobal, nil
	}

	builtin, hasBuiltin := s.BuiltinPrompt(feature)
	if !hasBuiltin {
		return "", "", fmt.Errorf("no builtin prompt registered for %q", feature)
	}

	return builtin, PromptSourceBuiltin, nil
}

// SetPrompt validates and stores a prompt body for a scope. Validation runs
// before persistence; an invalid body never overwrites the stored one.
func (s *Service) SetPrompt(ctx context.Context, feature PromptFeature, chatID int64, body string, actorID int64) error {
	if !IsPromptFeature(feature) {
		return fmt.Errorf("unknown prompt feature %q", feature)
	}

	if err := s.validatePromptBody(feature, body); err != nil {
		return err
	}

	return s.store.SetPromptTemplate(ctx, feature, chatID, body, actorID)
}

// ResetChatPrompt removes a chat-scoped template so the chat falls back to
// the global one.
func (s *Service) ResetChatPrompt(ctx context.Context, feature PromptFeature, chatID int64) error {
	if !IsPromptFeature(feature) {
		return fmt.Errorf("unknown prompt feature %q", feature)
	}

	return s.store.ClearPromptTemplate(ctx, feature, chatID)
}

// RestoreBuiltinGlobalPrompt removes the globally stored template so the
// runtime falls back to the compiled-in builtin.
func (s *Service) RestoreBuiltinGlobalPrompt(ctx context.Context, feature PromptFeature) error {
	if !IsPromptFeature(feature) {
		return fmt.Errorf("unknown prompt feature %q", feature)
	}

	return s.store.ClearPromptTemplate(ctx, feature, 0)
}

func (s *Service) validatePromptBody(feature PromptFeature, body string) error {
	if s.promptValidator != nil {
		return s.promptValidator(feature, body)
	}

	return ValidatePrompt(feature, body)
}
