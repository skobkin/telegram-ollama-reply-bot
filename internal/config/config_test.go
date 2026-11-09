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

func TestLoadIgnoresPlainEnvVarsWithoutDelimiter(t *testing.T) {
	t.Setenv("LLM__FEATURES__CHAT__MODEL", "gemma4:e4b")
	// Plain scalar variables whose lowercased names collide with top-level
	// config keys must not affect the config tree at all: `LOG=debug` used to
	// replace the whole log.* subtree and abort the boot with an unmarshal
	// error.
	t.Setenv("LOG", "debug")
	t.Setenv("BOT", "x")
	t.Setenv("STATE", "something")
	t.Setenv("FOO__BAR", "ignored-but-harmless")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Logging.Level != "info" {
		t.Fatalf("expected logging level default to survive plain env vars, got %q", cfg.Logging.Level)
	}
	if cfg.LLM.Features.Chat.Model != "gemma4:e4b" {
		t.Fatalf("unexpected chat model: %q", cfg.LLM.Features.Chat.Model)
	}
}

func TestLoadUsesBotHistoryLimitDefaults(t *testing.T) {
	t.Setenv("LLM__FEATURES__CHAT__MODEL", "gemma4:e4b")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Bot.UncompressedHistoryLimit != 15 {
		t.Fatalf("expected uncompressed history limit default of 15, got %d", cfg.Bot.UncompressedHistoryLimit)
	}
	if cfg.Bot.HistorySummaryThreshold != 5 {
		t.Fatalf("expected history summary threshold default of 5, got %d", cfg.Bot.HistorySummaryThreshold)
	}
}

func TestLoadUsesDocumentedLLMPrefixedHistoryLimitOverrides(t *testing.T) {
	t.Setenv("LLM__FEATURES__CHAT__MODEL", "gemma4:e4b")
	t.Setenv("LLM__UNCOMPRESSED_HISTORY_LIMIT", "20")
	t.Setenv("LLM__HISTORY_SUMMARY_THRESHOLD", "3")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Bot.UncompressedHistoryLimit != 20 {
		t.Fatalf("expected uncompressed history limit override of 20, got %d", cfg.Bot.UncompressedHistoryLimit)
	}
	if cfg.Bot.HistorySummaryThreshold != 3 {
		t.Fatalf("expected history summary threshold override of 3, got %d", cfg.Bot.HistorySummaryThreshold)
	}
}

func TestLoadUsesBotPrefixedHistoryLimitOverrides(t *testing.T) {
	t.Setenv("LLM__FEATURES__CHAT__MODEL", "gemma4:e4b")
	t.Setenv("BOT__UNCOMPRESSED_HISTORY_LIMIT", "7")
	t.Setenv("BOT__HISTORY_SUMMARY_THRESHOLD", "2")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Bot.UncompressedHistoryLimit != 7 {
		t.Fatalf("expected uncompressed history limit override of 7, got %d", cfg.Bot.UncompressedHistoryLimit)
	}
	if cfg.Bot.HistorySummaryThreshold != 2 {
		t.Fatalf("expected history summary threshold override of 2, got %d", cfg.Bot.HistorySummaryThreshold)
	}
}

func TestMCPConfigValidateRejectsNegativeResultCharBudget(t *testing.T) {
	c := MCPConfig{
		Enabled: true,
		Servers: map[string]MCPServerConfig{
			"browser": {URL: "https://mcp.example.com", ResultCharBudget: -1},
		},
	}

	err := c.Validate()
	if err == nil {
		t.Fatalf("expected error for negative result char budget")
	}
	if !strings.Contains(err.Error(), "browser") || !strings.Contains(err.Error(), "result_char_budget") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadRejectsNegativeMCPResultCharBudget(t *testing.T) {
	t.Setenv("LLM__FEATURES__CHAT__MODEL", "gemma4:e4b")
	t.Setenv("MCP__ENABLED", "true")
	t.Setenv("MCP__SERVERS__BROWSER__URL", "https://mcp.example.com")
	t.Setenv("MCP__SERVERS__BROWSER__RESULT_CHAR_BUDGET", "-5")

	_, err := Load()
	if err == nil {
		t.Fatalf("expected load to fail on negative MCP result char budget")
	}
	if !strings.Contains(err.Error(), "browser") {
		t.Fatalf("expected error to mention the server name, got: %v", err)
	}
}

func TestLoadJoinsMCPHeaderSegmentsWithHyphens(t *testing.T) {
	t.Setenv("LLM__FEATURES__CHAT__MODEL", "gemma4:e4b")
	t.Setenv("MCP__ENABLED", "true")
	t.Setenv("MCP__SERVERS__BROWSER__URL", "https://mcp.example.com")
	t.Setenv("MCP__SERVERS__BROWSER__HEADERS__X__API__KEY", "api-key-value")
	t.Setenv("MCP__SERVERS__BROWSER__HEADERS__ACCEPT__LANGUAGE", "en-US")
	t.Setenv("MCP__SERVERS__BROWSER__HEADERS__X_CUSTOM", "underscore-value")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	browser, ok := cfg.MCP.Servers["browser"]
	if !ok {
		t.Fatalf("missing browser server")
	}

	// Multiple `__`-separated segments are joined with "-": HTTP header names
	// are case-insensitive and Go canonicalizes them on send.
	if got, want := browser.Headers["x-api-key"], "api-key-value"; got != want {
		t.Fatalf("unexpected x-api-key header: got %q want %q", got, want)
	}
	if got, want := browser.Headers["accept-language"], "en-US"; got != want {
		t.Fatalf("unexpected accept-language header: got %q want %q", got, want)
	}
	// A single segment keeps its literal underscores.
	if got, want := browser.Headers["x_custom"], "underscore-value"; got != want {
		t.Fatalf("unexpected x_custom header: got %q want %q", got, want)
	}
	if len(browser.Headers) != 3 {
		t.Fatalf("unexpected headers: %v", browser.Headers)
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
		Enabled: true,
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
		Enabled: true,
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
		Enabled: true,
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
		Enabled: true,
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
		Enabled: true,
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
		Enabled: true,
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

func TestMCPConfigValidateHTTPErrorRedactsQuery(t *testing.T) {
	c := MCPConfig{
		Enabled: true,
		Servers: map[string]MCPServerConfig{
			"probe": {URL: "http://mcp.example.invalid/mcp?api_key=secret-value"},
		},
	}

	err := c.Validate()
	if err == nil {
		t.Fatalf("expected error for http:// without insecure=true")
	}
	msg := err.Error()
	if strings.Contains(msg, "secret-value") {
		t.Fatalf("error message leaks query credentials: %v", err)
	}
	if !strings.Contains(msg, `mcp.servers.probe.url "http://mcp.example.invalid/mcp"`) {
		t.Fatalf("error message lost redacted endpoint: %v", err)
	}
	if !strings.Contains(msg, "set mcp.servers.probe.insecure=true to allow it") {
		t.Fatalf("error message lost insecure=true guidance: %v", err)
	}
}

func TestMCPConfigValidateInvalidURLErrorRedactsQuery(t *testing.T) {
	// A control character makes url.ParseRequestURI fail while the query
	// string is still present in the raw value; the error must not echo it.
	c := MCPConfig{
		Enabled: true,
		Servers: map[string]MCPServerConfig{
			"probe": {URL: "http://mcp.example.invalid/mcp\x7f?api_key=secret-value"},
		},
	}

	err := c.Validate()
	if err == nil {
		t.Fatalf("expected error for malformed URL")
	}
	msg := err.Error()
	if strings.Contains(msg, "secret-value") {
		t.Fatalf("error message leaks query credentials: %v", err)
	}
	if !strings.Contains(msg, "probe") || !strings.Contains(msg, "is not a valid URL") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMCPConfigValidateUnsupportedSchemeErrorRedactsQuery(t *testing.T) {
	c := MCPConfig{
		Enabled: true,
		Servers: map[string]MCPServerConfig{
			"probe": {URL: "ftp://mcp.example.invalid/mcp?api_key=secret-value"},
		},
	}

	err := c.Validate()
	if err == nil {
		t.Fatalf("expected error for unsupported scheme")
	}
	msg := err.Error()
	if strings.Contains(msg, "secret-value") {
		t.Fatalf("error message leaks query credentials: %v", err)
	}
	if !strings.Contains(msg, `has unsupported scheme "ftp"`) {
		t.Fatalf("error message lost scheme detail: %v", err)
	}
}

func TestMCPConfigValidateUnsupportedSchemeErrorRedactsUserinfo(t *testing.T) {
	// ftp:// has no query string, so before RedactOne it fell through
	// redaction entirely and the userinfo credentials were echoed verbatim
	// into stderr and container logs.
	c := MCPConfig{
		Enabled: true,
		Servers: map[string]MCPServerConfig{
			//nolint:gosec // test fixture: redaction must drop userinfo credentials
			"probe": {URL: "ftp://user:secretpass@host/mcp"},
		},
	}

	err := c.Validate()
	if err == nil {
		t.Fatalf("expected error for unsupported scheme")
	}
	msg := err.Error()
	if strings.Contains(msg, "secretpass") || strings.Contains(msg, "user:") {
		t.Fatalf("error message leaks userinfo credentials: %v", err)
	}
	if !strings.Contains(msg, `has unsupported scheme "ftp"`) {
		t.Fatalf("error message lost scheme detail: %v", err)
	}
}

func TestMCPConfigValidateInvalidURLErrorRedactsUserinfo(t *testing.T) {
	// An invalid percent-escape makes url.ParseRequestURI fail; the credential
	// would appear twice without redaction — in the echoed URL and in the
	// rebuilt *url.Error — so both are checked via the whole error string.
	c := MCPConfig{
		Enabled: true,
		Servers: map[string]MCPServerConfig{
			//nolint:gosec // test fixture: redaction must drop userinfo credentials
			"probe": {URL: "https://user:secretpass@host/%zz"},
		},
	}

	err := c.Validate()
	if err == nil {
		t.Fatalf("expected error for malformed URL")
	}
	msg := err.Error()
	if strings.Contains(msg, "secretpass") || strings.Contains(msg, "user:") {
		t.Fatalf("error message leaks userinfo credentials: %v", err)
	}
	if !strings.Contains(msg, "is not a valid URL") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMCPConfigValidateRejectsNegativeTimeout(t *testing.T) {
	c := MCPConfig{
		Enabled: true,
		Servers: map[string]MCPServerConfig{
			"slow": {URL: "https://mcp.example.com", Timeout: -1 * time.Second},
		},
	}

	err := c.Validate()
	if err == nil {
		t.Fatalf("expected error for negative timeout")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMCPConfigValidateAcceptsAnyPositiveTimeout(t *testing.T) {
	// Timeout is a per-tool-call timeout: any positive duration is valid.
	// Slow tools such as browser automation legitimately need 120s+.
	for _, timeout := range []time.Duration{
		500 * time.Millisecond,
		15 * time.Second,
		2 * time.Minute,
	} {
		t.Run(timeout.String(), func(t *testing.T) {
			c := MCPConfig{
				Enabled: true,
				Servers: map[string]MCPServerConfig{
					"slow": {URL: "https://mcp.example.com", Timeout: timeout},
				},
			}

			if err := c.Validate(); err != nil {
				t.Fatalf("unexpected error for timeout %s: %v", timeout, err)
			}
		})
	}
}

func TestLoadAcceptsLongMCPTimeout(t *testing.T) {
	t.Setenv("LLM__FEATURES__CHAT__MODEL", "gemma4:e4b")
	t.Setenv("MCP__ENABLED", "true")
	t.Setenv("MCP__SERVERS__BROWSER__URL", "https://mcp.example.com")
	t.Setenv("MCP__SERVERS__BROWSER__TIMEOUT", "150s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	browser, ok := cfg.MCP.Servers["browser"]
	if !ok {
		t.Fatalf("missing browser server")
	}
	if browser.Timeout != 150*time.Second {
		t.Fatalf("unexpected timeout: %s", browser.Timeout)
	}
}

func TestMCPConfigValidateRejectsUnknownInvocationPolicy(t *testing.T) {
	c := MCPConfig{
		Enabled: true,
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
				Enabled: true,
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
		Enabled: true,
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
				ResultCharBudget: 4200,
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

func TestInvocationPolicyFor(t *testing.T) {
	cases := map[string]string{
		"":                                   "",
		"  ":                                 "",
		"unknown":                            "",
		InvocationPolicyExplicitRequestOnly:  InvocationPolicyExplicitRequestOnly,
		"  " + InvocationPolicyDiscretionary: InvocationPolicyDiscretionary,
		InvocationPolicyDiscretionaryPaid:    InvocationPolicyDiscretionaryPaid,
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
	t.Setenv("MCP__SERVERS__BROWSER__RESULT_CHAR_BUDGET", "5000")
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
	if browser.ResultCharBudget != 5000 {
		t.Fatalf("unexpected result char budget: %d", browser.ResultCharBudget)
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
	t.Setenv("MCP__ENABLED", "true")
	t.Setenv("MCP__SERVERS__BROWSER__URL", "ftp://mcp.example.com")

	_, err := Load()
	if err == nil {
		t.Fatalf("expected load to fail on invalid MCP URL scheme")
	}
	if !strings.Contains(err.Error(), "unsupported scheme") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadSucceedsWithInvalidMCPServersWhenDisabled(t *testing.T) {
	// An operator who set MCP variables but forgot MCP__ENABLED=true gets a
	// booting bot: the manager short-circuits on Enabled, so the invalid
	// per-server values are never used.
	t.Setenv("LLM__FEATURES__CHAT__MODEL", "gemma4:e4b")
	t.Setenv("MCP__SERVERS__BROKEN__URL", "ftp://mcp.example.com")
	t.Setenv("MCP__SERVERS__BROKEN__RESULT_CHAR_BUDGET", "-5")
	t.Setenv("MCP__SERVERS__BROKEN__TIMEOUT", "-1s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected load to succeed with MCP disabled, got: %v", err)
	}
	if cfg.MCP.Enabled {
		t.Fatalf("expected mcp.enabled to default to false")
	}
}
