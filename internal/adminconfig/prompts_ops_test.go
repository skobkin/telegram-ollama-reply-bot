package adminconfig

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func testBuiltins() BuiltinTemplates {
	return BuiltinTemplates{
		PromptFeatureChat:             "builtin chat {{.ToolPolicy}}",
		PromptFeatureSummarize:        "builtin summarize",
		PromptFeatureImageRecognition: "builtin image",
	}
}

func TestBuiltinPromptReturnsBodyVerbatim(t *testing.T) {
	svc := NewService(nil, testBuiltins(), nil)

	body, ok := svc.BuiltinPrompt(PromptFeatureChat)
	if !ok || body != "builtin chat {{.ToolPolicy}}" {
		t.Fatalf("expected verbatim builtin, got %q ok=%t", body, ok)
	}

	if _, ok := svc.BuiltinPrompt(PromptFeature("nope")); ok {
		t.Fatalf("unknown feature must report missing builtin")
	}
}

func TestActivePromptPrecedence(t *testing.T) {
	svc := NewService(&stubStore{prompts: map[string]string{
		"chat:0":   "global body",
		"chat:42":  "chat body",
		"chat:-77": "",
	}}, testBuiltins(), nil)
	ctx := context.Background()

	body, source, err := svc.ActivePrompt(ctx, PromptFeatureChat, 42)
	if err != nil || body != "chat body" || source != PromptSourceChat {
		t.Fatalf("expected chat override, got %q %s err %v", body, source, err)
	}

	body, source, err = svc.ActivePrompt(ctx, PromptFeatureChat, 55)
	if err != nil || body != "global body" || source != PromptSourceGlobal {
		t.Fatalf("expected global fallback, got %q %s err %v", body, source, err)
	}

	body, source, err = svc.ActivePrompt(ctx, PromptFeatureSummarize, 42)
	if err != nil || body != "builtin summarize" || source != PromptSourceBuiltin {
		t.Fatalf("expected builtin fallback, got %q %s err %v", body, source, err)
	}

	if _, _, err = svc.ActivePrompt(ctx, PromptFeature("nope"), 42); err == nil {
		t.Fatalf("unknown feature must error")
	}
}

func TestPromptOverviewReportsSources(t *testing.T) {
	svc := NewService(&stubStore{prompts: map[string]string{
		"chat:0":  "global body",
		"chat:42": "chat body",
	}}, testBuiltins(), nil)

	overview, err := svc.PromptOverview(context.Background(), 42)
	if err != nil {
		t.Fatalf("prompt overview: %v", err)
	}

	byFeature := map[PromptFeature]PromptStatus{}
	for _, status := range overview {
		byFeature[status.Feature] = status
	}

	chat := byFeature[PromptFeatureChat]
	if !chat.HasChatStored || !chat.HasGlobalStored || chat.Effective != PromptSourceChat {
		t.Fatalf("unexpected chat status: %+v", chat)
	}

	summarize := byFeature[PromptFeatureSummarize]
	if summarize.HasChatStored || summarize.HasGlobalStored || summarize.Effective != PromptSourceBuiltin {
		t.Fatalf("unexpected summarize status: %+v", summarize)
	}
}

func TestPromptOverviewGlobalScope(t *testing.T) {
	svc := NewService(&stubStore{prompts: map[string]string{"chat:0": "global body"}}, testBuiltins(), nil)

	overview, err := svc.PromptOverview(context.Background(), 0)
	if err != nil {
		t.Fatalf("prompt overview: %v", err)
	}
	if len(overview) != 3 {
		t.Fatalf("expected three features, got %d", len(overview))
	}
	for _, status := range overview {
		if status.HasChatStored {
			t.Fatalf("global scope must never report chat-stored, got %+v", status)
		}
	}
}

func TestSetPromptValidatesBeforePersist(t *testing.T) {
	store := &stubStore{prompts: map[string]string{"chat:0": "old"}}
	validator := func(_ PromptFeature, body string) error {
		if strings.Contains(body, "BAD") {
			return errors.New("invalid template")
		}

		return nil
	}
	svc := NewService(store, testBuiltins(), validator)
	ctx := context.Background()

	if err := svc.SetPrompt(ctx, PromptFeatureChat, 0, "BAD {{.Bogus}}", 7); err == nil {
		t.Fatalf("invalid template must be rejected")
	}
	if got := store.prompts["chat:0"]; got != "old" {
		t.Fatalf("rejected template must not overwrite, got %q", got)
	}

	if err := svc.SetPrompt(ctx, PromptFeatureChat, 0, "new body", 7); err != nil {
		t.Fatalf("valid template must persist: %v", err)
	}
	if got := store.prompts["chat:0"]; got != "new body" {
		t.Fatalf("expected stored template, got %q", got)
	}

	if err := svc.SetPrompt(ctx, PromptFeature("nope"), 0, "x", 7); err == nil {
		t.Fatalf("unknown feature must be rejected")
	}
}

// TestSetPromptFallsBackToParseOnlyValidation proves a nil validator keeps
// parse-only semantics for low-level tests.
func TestSetPromptFallsBackToParseOnlyValidation(t *testing.T) {
	store := &stubStore{prompts: map[string]string{}}
	svc := NewService(store, testBuiltins(), nil)

	// Parses fine, references an unknown field: parse-only accepts it.
	if err := svc.SetPrompt(context.Background(), PromptFeatureChat, 0, "{{.Bogus}}", 7); err != nil {
		t.Fatalf("parse-only fallback must accept: %v", err)
	}

	if err := svc.SetPrompt(context.Background(), PromptFeatureChat, 0, "{{.Language", 7); err == nil {
		t.Fatalf("parse errors must still be rejected")
	}
}

func TestPromptResets(t *testing.T) {
	store := &stubStore{prompts: map[string]string{
		"chat:42": "chat body",
		"chat:0":  "global body",
	}}
	svc := NewService(store, testBuiltins(), nil)
	ctx := context.Background()

	if err := svc.ResetChatPrompt(ctx, PromptFeatureChat, 42); err != nil {
		t.Fatalf("reset chat prompt: %v", err)
	}
	if _, ok := store.prompts["chat:42"]; ok {
		t.Fatalf("chat row must be deleted")
	}
	if got := store.prompts["chat:0"]; got != "global body" {
		t.Fatalf("global row must be untouched by chat reset, got %q", got)
	}

	if err := svc.RestoreBuiltinGlobalPrompt(ctx, PromptFeatureChat); err != nil {
		t.Fatalf("restore builtin: %v", err)
	}
	if _, ok := store.prompts["chat:0"]; ok {
		t.Fatalf("global row must be deleted")
	}

	if err := svc.ResetChatPrompt(ctx, PromptFeature("nope"), 42); err == nil {
		t.Fatalf("unknown feature must be rejected")
	}
	if err := svc.RestoreBuiltinGlobalPrompt(ctx, PromptFeature("nope")); err == nil {
		t.Fatalf("unknown feature must be rejected")
	}
}
