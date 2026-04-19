package config

import "testing"

func TestLoadUsesFeatureDefaults(t *testing.T) {
	t.Setenv("LLM_FEATURE_CHAT_MODEL", "gemma3:27b")

	cfg := Load()

	if cfg.LLM.Features.Chat.Backend != LLMBackendOpenAICompat {
		t.Fatalf("unexpected chat backend: %q", cfg.LLM.Features.Chat.Backend)
	}

	if cfg.LLM.Features.Summarize.Backend != cfg.LLM.Features.Chat.Backend {
		t.Fatalf("expected summarize backend to inherit chat backend, got %q", cfg.LLM.Features.Summarize.Backend)
	}

	if cfg.LLM.Features.Summarize.Model != "gemma3:27b" {
		t.Fatalf("expected summarize model to inherit chat model, got %q", cfg.LLM.Features.Summarize.Model)
	}

	if cfg.LLM.Features.ToolUse.Model != "gemma3:27b" {
		t.Fatalf("expected tool use model to inherit chat model, got %q", cfg.LLM.Features.ToolUse.Model)
	}
}

func TestLoadUsesExplicitFeatureRoutes(t *testing.T) {
	t.Setenv("LLM_FEATURE_CHAT_BACKEND", LLMBackendOllama)
	t.Setenv("LLM_FEATURE_CHAT_MODEL", "gemma3:12b")
	t.Setenv("LLM_FEATURE_SUMMARIZE_BACKEND", LLMBackendOpenAICompat)
	t.Setenv("LLM_FEATURE_SUMMARIZE_MODEL", "gpt-4.1-mini")
	t.Setenv("LLM_BACKEND_OLLAMA_BASE_URL", "http://ollama.internal:11434")
	t.Setenv("LLM_BACKEND_OPENAI_COMPAT_BASE_URL", "http://openai-compat.internal/v1")
	t.Setenv("LLM_BACKEND_OPENAI_COMPAT_API_TOKEN", "secret")

	cfg := Load()

	if cfg.LLM.Backends.Ollama.BaseURL != "http://ollama.internal:11434" {
		t.Fatalf("unexpected ollama base url: %q", cfg.LLM.Backends.Ollama.BaseURL)
	}

	if cfg.LLM.Backends.OpenAICompat.BaseURL != "http://openai-compat.internal/v1" {
		t.Fatalf("unexpected openai-compatible base url: %q", cfg.LLM.Backends.OpenAICompat.BaseURL)
	}

	if cfg.LLM.Features.Chat.Backend != LLMBackendOllama {
		t.Fatalf("unexpected chat backend: %q", cfg.LLM.Features.Chat.Backend)
	}

	if cfg.LLM.Features.Summarize.Backend != LLMBackendOpenAICompat {
		t.Fatalf("unexpected summarize backend: %q", cfg.LLM.Features.Summarize.Backend)
	}

	if cfg.LLM.Features.Summarize.Model != "gpt-4.1-mini" {
		t.Fatalf("unexpected summarize model: %q", cfg.LLM.Features.Summarize.Model)
	}
}
