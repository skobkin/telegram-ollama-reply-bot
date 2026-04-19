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

	if cfg.LLM.ToolLoopMaxIterations != 6 {
		t.Fatalf("expected tool loop max iterations default to be 6, got %d", cfg.LLM.ToolLoopMaxIterations)
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
	t.Setenv("LLM_TOOL_LOOP_MAX_ITERATIONS", "9")

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

	if cfg.LLM.ToolLoopMaxIterations != 9 {
		t.Fatalf("unexpected tool loop max iterations: %d", cfg.LLM.ToolLoopMaxIterations)
	}
}

func TestLoadUsesStateDefaults(t *testing.T) {
	t.Setenv("LLM_FEATURE_CHAT_MODEL", "gemma3:27b")

	cfg := Load()

	if cfg.State.HistoryMessagesPerStream != 150 {
		t.Fatalf("unexpected history messages per stream: %d", cfg.State.HistoryMessagesPerStream)
	}
	if cfg.State.HistoryStreamsMax != 1024 {
		t.Fatalf("unexpected history streams max: %d", cfg.State.HistoryStreamsMax)
	}
	if cfg.State.ImageCacheTTL <= 0 {
		t.Fatalf("expected positive image cache ttl, got %s", cfg.State.ImageCacheTTL)
	}
}

func TestLoadUsesExplicitStateConfig(t *testing.T) {
	t.Setenv("LLM_FEATURE_CHAT_MODEL", "gemma3:27b")
	t.Setenv("STATE_MAX_BYTES", "123456")
	t.Setenv("STATE_HISTORY_MAX_BYTES", "45678")
	t.Setenv("STATE_HISTORY_STREAMS_MAX", "32")
	t.Setenv("STATE_HISTORY_MESSAGES_PER_STREAM", "24")
	t.Setenv("STATE_IMAGE_CACHE_MAX_BYTES", "8192")
	t.Setenv("STATE_IMAGE_CACHE_TTL", "45m")

	cfg := Load()

	if cfg.State.MaxBytes != 123456 {
		t.Fatalf("unexpected state max bytes: %d", cfg.State.MaxBytes)
	}
	if cfg.State.HistoryMaxBytes != 45678 {
		t.Fatalf("unexpected history max bytes: %d", cfg.State.HistoryMaxBytes)
	}
	if cfg.State.HistoryStreamsMax != 32 {
		t.Fatalf("unexpected history streams max: %d", cfg.State.HistoryStreamsMax)
	}
	if cfg.State.HistoryMessagesPerStream != 24 {
		t.Fatalf("unexpected history messages per stream: %d", cfg.State.HistoryMessagesPerStream)
	}
	if cfg.State.ImageCacheMaxBytes != 8192 {
		t.Fatalf("unexpected image cache max bytes: %d", cfg.State.ImageCacheMaxBytes)
	}
	if cfg.State.ImageCacheTTL.Minutes() != 45 {
		t.Fatalf("unexpected image cache ttl: %s", cfg.State.ImageCacheTTL)
	}
}

func TestLoadUsesPersistentStorePath(t *testing.T) {
	t.Setenv("LLM_FEATURE_CHAT_MODEL", "gemma3:27b")
	t.Setenv("PERSISTENT_STORE_PATH", "/data/db.sqlite")

	cfg := Load()

	if cfg.Persistence.StorePath != "/data/db.sqlite" {
		t.Fatalf("unexpected persistent store path: %q", cfg.Persistence.StorePath)
	}
}

func TestLoadUsesDefaultPersistentStorePath(t *testing.T) {
	t.Setenv("LLM_FEATURE_CHAT_MODEL", "gemma3:27b")

	cfg := Load()

	if cfg.Persistence.StorePath != "/data/db.sqlite" {
		t.Fatalf("unexpected default persistent store path: %q", cfg.Persistence.StorePath)
	}
}
