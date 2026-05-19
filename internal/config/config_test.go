package config

import (
	"os"
	"strings"
	"testing"
)

func TestLoadUsesFeatureDefaults(t *testing.T) {
	t.Setenv("LLM_FEATURE_CHAT_MODEL", "gemma4:e4b")

	cfg := Load()

	if cfg.LLM.Features.Chat.Model != "gemma4:e4b" {
		t.Fatalf("unexpected chat model: %q", cfg.LLM.Features.Chat.Model)
	}
	if cfg.LLM.Features.Summarize.Model != "gemma4:e4b" {
		t.Fatalf("expected summarize model to inherit chat model, got %q", cfg.LLM.Features.Summarize.Model)
	}
	if cfg.LLM.Features.ImageRecognition.Model != "gemma4:e4b" {
		t.Fatalf("expected image recognition model to inherit chat model, got %q", cfg.LLM.Features.ImageRecognition.Model)
	}

	if cfg.LLM.ToolLoopMaxIterations != 6 {
		t.Fatalf("expected tool loop max iterations default to be 6, got %d", cfg.LLM.ToolLoopMaxIterations)
	}
}

func TestLoadUsesFeatureDefaultsForEmptyOptionalModels(t *testing.T) {
	t.Setenv("LLM_FEATURE_CHAT_MODEL", "gemma4:e4b")
	t.Setenv("LLM_FEATURE_SUMMARIZE_MODEL", "")
	t.Setenv("LLM_FEATURE_IMAGE_RECOGNITION_MODEL", "")

	cfg := Load()

	if cfg.LLM.Features.Summarize.Model != "gemma4:e4b" {
		t.Fatalf("expected empty summarize model to inherit chat model, got %q", cfg.LLM.Features.Summarize.Model)
	}
	if cfg.LLM.Features.ImageRecognition.Model != "gemma4:e4b" {
		t.Fatalf("expected empty image recognition model to inherit chat model, got %q", cfg.LLM.Features.ImageRecognition.Model)
	}
}

func TestLoadUsesExplicitFeatureModels(t *testing.T) {
	t.Setenv("LLM_FEATURE_CHAT_MODEL", "chat-model")
	t.Setenv("LLM_FEATURE_SUMMARIZE_MODEL", "summary-model")
	t.Setenv("LLM_FEATURE_IMAGE_RECOGNITION_MODEL", "vision-model")
	t.Setenv("LLM_BACKEND_OPENAI_COMPAT_BASE_URL", "http://openai-compat.internal/v1")
	t.Setenv("LLM_BACKEND_OPENAI_COMPAT_API_TOKEN", "secret")
	t.Setenv("LLM_TOOL_LOOP_MAX_ITERATIONS", "9")

	cfg := Load()

	if cfg.LLM.Backends.OpenAICompat.BaseURL != "http://openai-compat.internal/v1" {
		t.Fatalf("unexpected openai-compatible base url: %q", cfg.LLM.Backends.OpenAICompat.BaseURL)
	}

	if cfg.LLM.Features.Chat.Model != "chat-model" {
		t.Fatalf("unexpected chat model: %q", cfg.LLM.Features.Chat.Model)
	}
	if cfg.LLM.Features.Summarize.Model != "summary-model" {
		t.Fatalf("unexpected summarize model: %q", cfg.LLM.Features.Summarize.Model)
	}
	if cfg.LLM.Features.ImageRecognition.Model != "vision-model" {
		t.Fatalf("unexpected image recognition model: %q", cfg.LLM.Features.ImageRecognition.Model)
	}

	if cfg.LLM.ToolLoopMaxIterations != 9 {
		t.Fatalf("unexpected tool loop max iterations: %d", cfg.LLM.ToolLoopMaxIterations)
	}
}

func TestDockerfileDoesNotBakeFeatureModelDefaults(t *testing.T) {
	data, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}

	for _, name := range []string{
		"LLM_FEATURE_CHAT_MODEL",
		"LLM_FEATURE_SUMMARIZE_MODEL",
		"LLM_FEATURE_IMAGE_RECOGNITION_MODEL",
	} {
		if strings.Contains(string(data), name+"=") {
			t.Fatalf("Dockerfile must not define %s default", name)
		}
	}
}

func TestLoadUsesStateDefaults(t *testing.T) {
	t.Setenv("LLM_FEATURE_CHAT_MODEL", "gemma4:e4b")

	cfg := Load()

	if cfg.State.HistoryStreamsMax != 1024 {
		t.Fatalf("unexpected history streams max: %d", cfg.State.HistoryStreamsMax)
	}
	if cfg.State.ImageCacheTTL <= 0 {
		t.Fatalf("expected positive image cache ttl, got %s", cfg.State.ImageCacheTTL)
	}
}

func TestLoadUsesExplicitStateConfig(t *testing.T) {
	t.Setenv("LLM_FEATURE_CHAT_MODEL", "gemma4:e4b")
	t.Setenv("STATE_MAX_BYTES", "123456")
	t.Setenv("STATE_HISTORY_MAX_BYTES", "45678")
	t.Setenv("STATE_HISTORY_STREAMS_MAX", "32")
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
	if cfg.State.ImageCacheMaxBytes != 8192 {
		t.Fatalf("unexpected image cache max bytes: %d", cfg.State.ImageCacheMaxBytes)
	}
	if cfg.State.ImageCacheTTL.Minutes() != 45 {
		t.Fatalf("unexpected image cache ttl: %s", cfg.State.ImageCacheTTL)
	}
}

func TestLoadUsesPersistentStorePath(t *testing.T) {
	t.Setenv("LLM_FEATURE_CHAT_MODEL", "gemma4:e4b")
	t.Setenv("PERSISTENT_STORE_PATH", "/data/db.sqlite")

	cfg := Load()

	if cfg.Persistence.StorePath != "/data/db.sqlite" {
		t.Fatalf("unexpected persistent store path: %q", cfg.Persistence.StorePath)
	}
}

func TestLoadUsesDefaultPersistentStorePath(t *testing.T) {
	t.Setenv("LLM_FEATURE_CHAT_MODEL", "gemma4:e4b")

	cfg := Load()

	if cfg.Persistence.StorePath != "/data/db.sqlite" {
		t.Fatalf("unexpected default persistent store path: %q", cfg.Persistence.StorePath)
	}
}

func TestLoadDefaultsSearchBackendToNone(t *testing.T) {
	t.Setenv("LLM_FEATURE_CHAT_MODEL", "gemma4:e4b")

	cfg := Load()

	if cfg.Search.Backend != SearchBackendNone {
		t.Fatalf("unexpected search backend: %q", cfg.Search.Backend)
	}
	if len(cfg.Search.Chain) != 0 {
		t.Fatalf("expected empty search chain, got %v", cfg.Search.Chain)
	}
}

func TestLoadUsesProviderOrientedSearchConfig(t *testing.T) {
	t.Setenv("LLM_FEATURE_CHAT_MODEL", "gemma4:e4b")
	t.Setenv("SEARCH_BACKEND", SearchBackendChain)
	t.Setenv("SEARCH_BACKEND_CHAIN", " tavily , kagi ")
	t.Setenv("PROVIDER_TAVILY_API_KEY", "tavily-secret")
	t.Setenv("PROVIDER_KAGI_API_KEY", "kagi-secret")

	cfg := Load()

	if cfg.Search.Backend != SearchBackendChain {
		t.Fatalf("unexpected search backend: %q", cfg.Search.Backend)
	}
	if got, want := len(cfg.Search.Chain), 2; got != want {
		t.Fatalf("unexpected search chain length: got %d want %d", got, want)
	}
	if cfg.Search.Chain[0] != SearchBackendTavily || cfg.Search.Chain[1] != SearchBackendKagi {
		t.Fatalf("unexpected search chain: %v", cfg.Search.Chain)
	}
	if cfg.Providers.Tavily.APIKey != "tavily-secret" {
		t.Fatalf("unexpected tavily api key")
	}
	if cfg.Providers.Kagi.APIKey != "kagi-secret" {
		t.Fatalf("unexpected kagi api key")
	}
}
