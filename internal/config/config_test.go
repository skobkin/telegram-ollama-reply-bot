package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoadUsesFeatureDefaults(t *testing.T) {
	t.Setenv("LLM__FEATURES__CHAT__MODEL", "gemma4:e4b")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

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
	t.Setenv("LLM__FEATURES__CHAT__MODEL", "gemma4:e4b")
	t.Setenv("LLM__FEATURES__SUMMARIZE__MODEL", "")
	t.Setenv("LLM__FEATURES__IMAGE_RECOGNITION__MODEL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.LLM.Features.Summarize.Model != "gemma4:e4b" {
		t.Fatalf("expected empty summarize model to inherit chat model, got %q", cfg.LLM.Features.Summarize.Model)
	}
	if cfg.LLM.Features.ImageRecognition.Model != "gemma4:e4b" {
		t.Fatalf("expected empty image recognition model to inherit chat model, got %q", cfg.LLM.Features.ImageRecognition.Model)
	}
}

func TestLoadUsesExplicitFeatureModels(t *testing.T) {
	t.Setenv("LLM__FEATURES__CHAT__MODEL", "chat-model")
	t.Setenv("LLM__FEATURES__SUMMARIZE__MODEL", "summary-model")
	t.Setenv("LLM__FEATURES__IMAGE_RECOGNITION__MODEL", "vision-model")
	t.Setenv("LLM__BACKENDS__OPENAI_COMPAT__BASE_URL", "http://openai-compat.internal/v1")
	t.Setenv("LLM__BACKENDS__OPENAI_COMPAT__API_TOKEN", "secret")
	t.Setenv("LLM__TOOL_LOOP_MAX_ITERATIONS", "9")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

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
		"LLM__FEATURES__CHAT__MODEL",
		"LLM__FEATURES__SUMMARIZE__MODEL",
		"LLM__FEATURES__IMAGE_RECOGNITION__MODEL",
	} {
		if strings.Contains(string(data), name+"=") {
			t.Fatalf("Dockerfile must not define %s default", name)
		}
	}
}

func TestLoadUsesStateDefaults(t *testing.T) {
	t.Setenv("LLM__FEATURES__CHAT__MODEL", "gemma4:e4b")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.State.HistoryStreamsMax != 1024 {
		t.Fatalf("unexpected history streams max: %d", cfg.State.HistoryStreamsMax)
	}
	if cfg.State.ImageCacheTTL <= 0 {
		t.Fatalf("expected positive image cache ttl, got %s", cfg.State.ImageCacheTTL)
	}
}

func TestLoadUsesExplicitStateConfig(t *testing.T) {
	t.Setenv("LLM__FEATURES__CHAT__MODEL", "gemma4:e4b")
	t.Setenv("STATE__MAX_BYTES", "123456")
	t.Setenv("STATE__HISTORY_MAX_BYTES", "45678")
	t.Setenv("STATE__HISTORY_STREAMS_MAX", "32")
	t.Setenv("STATE__IMAGE_CACHE_MAX_BYTES", "8192")
	t.Setenv("STATE__IMAGE_CACHE_TTL", "45m")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

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
	t.Setenv("LLM__FEATURES__CHAT__MODEL", "gemma4:e4b")
	t.Setenv("PERSISTENT__STORE_PATH", "/data/db.sqlite")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Persistence.StorePath != "/data/db.sqlite" {
		t.Fatalf("unexpected persistent store path: %q", cfg.Persistence.StorePath)
	}
}

func TestLoadUsesDefaultPersistentStorePath(t *testing.T) {
	t.Setenv("LLM__FEATURES__CHAT__MODEL", "gemma4:e4b")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Persistence.StorePath != "/data/db.sqlite" {
		t.Fatalf("unexpected default persistent store path: %q", cfg.Persistence.StorePath)
	}
}

func TestLoadDefaultsSearchBackendToNone(t *testing.T) {
	t.Setenv("LLM__FEATURES__CHAT__MODEL", "gemma4:e4b")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Search.Backend != SearchBackendNone {
		t.Fatalf("unexpected search backend: %q", cfg.Search.Backend)
	}
	if len(cfg.Search.Chain) != 0 {
		t.Fatalf("expected empty search chain, got %v", cfg.Search.Chain)
	}
}

func TestLoadUsesProviderOrientedSearchConfig(t *testing.T) {
	t.Setenv("LLM__FEATURES__CHAT__MODEL", "gemma4:e4b")
	t.Setenv("SEARCH__BACKEND", SearchBackendChain)
	t.Setenv("SEARCH__CHAIN", " tavily , kagi ")
	t.Setenv("PROVIDERS__TAVILY__API_KEY", "tavily-secret")
	t.Setenv("PROVIDERS__KAGI__API_KEY", "kagi-secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

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

func TestLoadImageRecognitionEnabledDefaultsToTrue(t *testing.T) {
	t.Setenv("LLM__FEATURES__CHAT__MODEL", "gemma4:e4b")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !cfg.LLM.ImageRecognitionEnabled {
		t.Fatalf("expected image recognition to be enabled by default")
	}
}

func TestLoadImageRecognitionEnabledCanBeDisabled(t *testing.T) {
	t.Setenv("LLM__FEATURES__CHAT__MODEL", "gemma4:e4b")
	t.Setenv("LLM__IMAGE_RECOGNITION_ENABLED", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.LLM.ImageRecognitionEnabled {
		t.Fatalf("expected image recognition to be disabled")
	}
}

func TestMCPConfigValidateAcceptsHTTPSServer(t *testing.T) {
	c := MCPConfig{
		Enabled: true,
		Servers: map[string]MCPServerConfig{
			"browser": {
				URL: "https://mcp.example.com",
			},
		},
	}

	if err := c.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMCPConfigValidateRejectsEmptyServerName(t *testing.T) {
	c := MCPConfig{
		Servers: map[string]MCPServerConfig{
			"": {URL: "https://mcp.example.com"},
		},
	}

	err := c.Validate()
	if err == nil {
		t.Fatalf("expected error for empty server name")
	}
	if !strings.Contains(err.Error(), "server name must not be empty") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMCPConfigValidateRejectsEmptyURL(t *testing.T) {
	c := MCPConfig{
		Servers: map[string]MCPServerConfig{
			"browser": {},
		},
	}

	err := c.Validate()
	if err == nil {
		t.Fatalf("expected error for empty URL")
	}
	if !strings.Contains(err.Error(), `mcp.servers.browser.url is required`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMCPConfigValidateRejectsHTTPWithoutInsecure(t *testing.T) {
	c := MCPConfig{
		Servers: map[string]MCPServerConfig{
			"local": {URL: "http://localhost:8765/mcp"},
		},
	}

	err := c.Validate()
	if err == nil {
		t.Fatalf("expected error for http:// without insecure=true")
	}
	if !strings.Contains(err.Error(), "insecure=true") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMCPConfigValidateAllowsHTTPWithInsecure(t *testing.T) {
	c := MCPConfig{
		Servers: map[string]MCPServerConfig{
			"local": {URL: "http://localhost:8765/mcp", Insecure: true},
		},
	}

	if err := c.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMCPConfigValidateRejectsUnsupportedScheme(t *testing.T) {
	c := MCPConfig{
		Servers: map[string]MCPServerConfig{
			"weird": {URL: "ftp://mcp.example.com"},
		},
	}

	err := c.Validate()
	if err == nil {
		t.Fatalf("expected error for unsupported scheme")
	}
	if !strings.Contains(err.Error(), "unsupported scheme") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMCPConfigValidateRejectsInvalidURL(t *testing.T) {
	c := MCPConfig{
		Servers: map[string]MCPServerConfig{
			"bad": {URL: "ht!tp://broken"},
		},
	}

	err := c.Validate()
	if err == nil {
		t.Fatalf("expected error for malformed URL")
	}
	if !strings.Contains(err.Error(), "is not a valid URL") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMCPConfigValidateRejectsOutOfRangeTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
	}{
		{"too short", 500 * time.Millisecond},
		{"too long", 2 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := MCPConfig{
				Servers: map[string]MCPServerConfig{
					"slow": {URL: "https://mcp.example.com", Timeout: tc.timeout},
				},
			}

			err := c.Validate()
			if err == nil {
				t.Fatalf("expected error for timeout %s", tc.timeout)
			}
			if !strings.Contains(err.Error(), "out of range") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestMCPConfigValidateRejectsUnknownInvocationPolicy(t *testing.T) {
	c := MCPConfig{
		Servers: map[string]MCPServerConfig{
			"browser": {URL: "https://mcp.example.com", InvocationPolicy: "sometimes"},
		},
	}

	err := c.Validate()
	if err == nil {
		t.Fatalf("expected error for unknown invocation policy")
	}
	if !strings.Contains(err.Error(), "invocation_policy") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMCPConfigValidateAcceptsKnownInvocationPolicies(t *testing.T) {
	for _, policy := range []string{
		InvocationPolicyExplicitRequestOnly,
		InvocationPolicyDiscretionary,
		InvocationPolicyDiscretionaryPaid,
		"  " + InvocationPolicyDiscretionary + "  ",
	} {
		t.Run(policy, func(t *testing.T) {
			c := MCPConfig{
				Servers: map[string]MCPServerConfig{
					"browser": {URL: "https://mcp.example.com", InvocationPolicy: policy},
				},
			}

			if err := c.Validate(); err != nil {
				t.Fatalf("unexpected error for policy %q: %v", policy, err)
			}
		})
	}
}

func TestMCPConfigValidateRejectsEmptyHeaderName(t *testing.T) {
	c := MCPConfig{
		Servers: map[string]MCPServerConfig{
			"browser": {
				URL:     "https://mcp.example.com",
				Headers: map[string]string{"": "value"},
			},
		},
	}

	err := c.Validate()
	if err == nil {
		t.Fatalf("expected error for empty header name")
	}
	if !strings.Contains(err.Error(), "empty header name") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMCPConfigValidateHappyPath(t *testing.T) {
	sideEffecting := false
	c := MCPConfig{
		Enabled: true,
		Servers: map[string]MCPServerConfig{
			"browser": {
				URL:              "https://mcp.example.com",
				Headers:          map[string]string{"Authorization": "Bearer secret"},
				Timeout:          15 * time.Second,
				InvocationPolicy: InvocationPolicyDiscretionary,
				SideEffecting:    &sideEffecting,
				AllowedTools:     []string{"fetch", " Summarize ", "fetch"},
				RestrictedTools:  []string{"delete_history"},
				Optional:         true,
			},
			"local": {
				URL:      "http://localhost:8765/mcp",
				Insecure: true,
			},
		},
	}

	if err := c.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInvocationPolicyFor(t *testing.T) {	cases := map[string]string{
		"":                                       "",
		"  ":                                     "",
		"unknown":                                "",
		InvocationPolicyExplicitRequestOnly:     InvocationPolicyExplicitRequestOnly,
		"  " + InvocationPolicyDiscretionary:     InvocationPolicyDiscretionary,
		InvocationPolicyDiscretionaryPaid:        InvocationPolicyDiscretionaryPaid,
	}

	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			if got := InvocationPolicyFor(in); got != want {
				t.Fatalf("InvocationPolicyFor(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

func TestLoadUsesMCPConfigFromEnv(t *testing.T) {
	t.Setenv("LLM__FEATURES__CHAT__MODEL", "gemma4:e4b")
	t.Setenv("MCP__ENABLED", "true")
	t.Setenv("MCP__SERVERS__BROWSER__URL", "https://mcp.example.com")
	t.Setenv("MCP__SERVERS__BROWSER__TIMEOUT", "20s")
	t.Setenv("MCP__SERVERS__BROWSER__INVOCATION_POLICY", InvocationPolicyDiscretionary)
	t.Setenv("MCP__SERVERS__BROWSER__ALLOWED_TOOLS", "fetch, summarize , fetch")
	t.Setenv("MCP__SERVERS__BROWSER__HEADERS__AUTHORIZATION", "Bearer secret")
	t.Setenv("MCP__SERVERS__LOCAL__URL", "http://localhost:8765/mcp")
	t.Setenv("MCP__SERVERS__LOCAL__INSECURE", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !cfg.MCP.Enabled {
		t.Fatalf("expected mcp.enabled=true")
	}

	browser, ok := cfg.MCP.Servers["browser"]
	if !ok {
		t.Fatalf("missing browser server")
	}
	if browser.URL != "https://mcp.example.com" {
		t.Fatalf("unexpected browser url: %q", browser.URL)
	}
	if browser.Timeout != 20*time.Second {
		t.Fatalf("unexpected timeout: %s", browser.Timeout)
	}
	if browser.InvocationPolicy != InvocationPolicyDiscretionary {
		t.Fatalf("unexpected invocation policy: %q", browser.InvocationPolicy)
	}
	if got, want := browser.Headers["authorization"], "Bearer secret"; got != want {
		t.Fatalf("unexpected header: %q", got)
	}

	local, ok := cfg.MCP.Servers["local"]
	if !ok {
		t.Fatalf("missing local server")
	}
	if !local.Insecure {
		t.Fatalf("expected local server to be insecure")
	}
}

func TestLoadRejectsInvalidMCPFromEnv(t *testing.T) {
	t.Setenv("LLM__FEATURES__CHAT__MODEL", "gemma4:e4b")
	t.Setenv("MCP__SERVERS__BROWSER__URL", "ftp://mcp.example.com")

	_, err := Load()
	if err == nil {
		t.Fatalf("expected load to fail on invalid MCP URL scheme")
	}
	if !strings.Contains(err.Error(), "unsupported scheme") {
		t.Fatalf("unexpected error: %v", err)
	}
}
