// Package config loads, validates, and exposes the bot's runtime configuration.
//
// Configuration is sourced from environment variables via the Koanf env provider
// (double-underscore path delimiter, lowercased keys). Sensible defaults for
// optional fields are baked in so the bot can boot in a development environment
// without any env wiring. Per-feature model selection allows the summarize and
// image-recognition routes to fall back to the chat model when not set.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/v2"

	"telegram-ollama-reply-bot/internal/support/urlredact"
)

// Config represents the root configuration structure.
type Config struct {
	LLM         LLMConfig         `koanf:"llm"`
	Sentry      SentryConfig      `koanf:"sentry"`
	Bot         BotConfig         `koanf:"bot"`
	State       StateConfig       `koanf:"state"`
	Logging     LoggingConfig     `koanf:"log"`
	Persistence PersistenceConfig `koanf:"persistent"`
	Providers   ProvidersConfig   `koanf:"providers"`
	Search      SearchConfig      `koanf:"search"`
	MCP         MCPConfig         `koanf:"mcp"`
}

const (
	LLMBackendOpenAICompat = "openai_compat"

	SearchBackendNone   = "none"
	SearchBackendTavily = "tavily"
	SearchBackendKagi   = "kagi"
	SearchBackendChain  = "chain"
)

// LLMConfig contains configuration for the LLM service.
type LLMConfig struct {
	Backends                BackendConfig `koanf:"backends"`
	Features                FeatureConfig `koanf:"features"`
	ToolLoopMaxIterations   int           `koanf:"tool_loop_max_iterations"`
	ImageRecognitionEnabled bool          `koanf:"image_recognition_enabled"`
}

// SentryConfig contains configuration for Sentry error tracking.
type SentryConfig struct {
	DSN string `koanf:"dsn"`
}

// LoggingConfig contains configuration for structured logging.
type LoggingConfig struct {
	Level string `koanf:"level"`
}

// BotConfig contains configuration for bot settings.
type BotConfig struct {
	AdminIDs                 []int64        `koanf:"admin_ids"`
	Telegram                 TelegramConfig `koanf:"telegram"`
	UncompressedHistoryLimit int            `koanf:"uncompressed_history_limit"`
	HistorySummaryThreshold  int            `koanf:"history_summary_threshold"`
	ProcessingTimeout        time.Duration  `koanf:"processing_timeout"`
}

// TelegramConfig contains configuration for the Telegram bot transport.
type TelegramConfig struct {
	Token string `koanf:"token"`
}

// PersistenceConfig contains durable storage configuration.
type PersistenceConfig struct {
	StorePath string `koanf:"store_path"`
}

// ProvidersConfig contains credentials for external providers that may support multiple capabilities.
type ProvidersConfig struct {
	Tavily TavilyProviderConfig `koanf:"tavily"`
	Kagi   KagiProviderConfig   `koanf:"kagi"`
}

// TavilyProviderConfig contains Tavily credentials.
type TavilyProviderConfig struct {
	APIKey string `koanf:"api_key"`
}

// KagiProviderConfig contains Kagi credentials.
type KagiProviderConfig struct {
	APIKey string `koanf:"api_key"`
}

// SearchConfig contains search capability routing.
type SearchConfig struct {
	Backend string   `koanf:"backend"`
	Chain   []string `koanf:"chain"`
}

// MCPConfig contains configuration for Model Context Protocol (MCP) tool servers.
//
// Operators declare any number of MCP servers in `mcp.servers.<name>`. The bot
// connects to each over Streamable HTTP at startup, discovers its tools, and
// registers them on the existing tool registry. Each server's tools are scoped
// to the server: per-server `allowed_tools` and `restricted_tools` only filter
// tools advertised by that server, never the built-in tooluse tools.
type MCPConfig struct {
	Enabled bool                       `koanf:"enabled"`
	Servers map[string]MCPServerConfig `koanf:"servers"`
}

// Invocation policy values accepted by `MCPServerConfig.InvocationPolicy` and
// used by the bot when translating MCP tool annotations into the existing
// tooluse package policy constants.
const (
	InvocationPolicyExplicitRequestOnly = "explicit_request_only"
	InvocationPolicyDiscretionary       = "discretionary"
	InvocationPolicyDiscretionaryPaid   = "discretionary_paid"
)

// InvocationPolicyFor returns the canonical string form of an invocation
// policy, trimming surrounding whitespace. Unknown values return "" so callers
// can distinguish "valid" from "unset / unrecognized".
func InvocationPolicyFor(s string) string {
	s = strings.TrimSpace(s)
	switch s {
	case InvocationPolicyExplicitRequestOnly, InvocationPolicyDiscretionary, InvocationPolicyDiscretionaryPaid:
		return s
	default:
		return ""
	}
}

// MCPServerConfig contains configuration for one MCP server.
//
// `URL` is required and must be HTTPS unless `Insecure` is true. `Headers` are
// sent on every request; their values are never logged. Because environment
// variable names cannot contain "-", spell a hyphenated header as multiple
// `__`-separated segments after `HEADERS` (`...HEADERS__X__API__KEY` yields
// the header name "x-api-key"); a single segment keeps its literal
// underscores (`...HEADERS__X_API_KEY` yields "x_api_key"). `Timeout` is a
// per-tool-call timeout: when unset (zero) the client uses its default of 15s,
// and slow tools such as browser automation may set it far above that.
// `InvocationPolicy` and `SideEffecting` are operator overrides for
// the per-tool defaults derived from the MCP tool annotations (`readOnlyHint`
// and `destructiveHint` respectively). `AllowedTools` is a whitelist; setting
// it when none of the named tools exist on the server is a startup error
// (catches typos early). `RestrictedTools` is a deny-list applied after
// `AllowedTools`. `Optional` makes a server best-effort: on connection failure
// the bot logs a warning and registers zero tools from that server.
type MCPServerConfig struct {
	URL              string            `koanf:"url"`
	Headers          map[string]string `koanf:"headers"`
	Timeout          time.Duration     `koanf:"timeout"`
	Insecure         bool              `koanf:"insecure"`
	InvocationPolicy string            `koanf:"invocation_policy"`
	SideEffecting    *bool             `koanf:"side_effecting"`
	ResultCharBudget int               `koanf:"result_char_budget"`
	AllowedTools     []string          `koanf:"allowed_tools"`
	RestrictedTools  []string          `koanf:"restricted_tools"`
	Optional         bool              `koanf:"optional"`
}

// StateConfig contains limits for the in-memory state store.
type StateConfig struct {
	MaxBytes           int64         `koanf:"max_bytes"`
	HistoryMaxBytes    int64         `koanf:"history_max_bytes"`
	HistoryStreamsMax  int           `koanf:"history_streams_max"`
	ImageCacheMaxBytes int64         `koanf:"image_cache_max_bytes"`
	ImageCacheTTL      time.Duration `koanf:"image_cache_ttl"`
}

// BackendConfig contains configuration for all supported LLM backends.
type BackendConfig struct {
	OpenAICompat OpenAICompatBackendConfig `koanf:"openai_compat"`
}

// OpenAICompatBackendConfig contains configuration for OpenAI-compatible backends.
type OpenAICompatBackendConfig struct {
	BaseURL  string `koanf:"base_url"`
	APIToken string `koanf:"api_token"`
}

// FeatureConfig contains per-feature model selection.
type FeatureConfig struct {
	Chat             FeatureRouteConfig `koanf:"chat"`
	Summarize        FeatureRouteConfig `koanf:"summarize"`
	ImageRecognition FeatureRouteConfig `koanf:"image_recognition"`
}

// FeatureRouteConfig contains model selection for one feature.
type FeatureRouteConfig struct {
	Model string `koanf:"model"`
}

// Load reads configuration from environment variables, applies defaults,
// and returns a populated *Config. An error is returned when a value is
// present but cannot be parsed (e.g. an integer that fails to convert).
//
// Environment variables use a `__` path delimiter; the first path component
// is the top-level config field. Within a path component, snake_case is used.
// Example: `LLM__FEATURES__CHAT__MODEL`, `STATE__IMAGE_CACHE_TTL`,
// `BOT__ADMIN_IDS`, `LLM__BACKENDS__OPENAI_COMPAT__BASE_URL`.
//
// Empty values are treated as "unset": defaults remain in effect. A non-empty
// value that fails to parse is a hard error.
func Load() (*Config, error) {
	k := koanf.New(".")

	if err := applyDefaults(k); err != nil {
		return nil, fmt.Errorf("config: apply defaults: %w", err)
	}

	if err := k.Load(env.Provider("__", env.Opt{
		TransformFunc: func(key, value string) (string, any) {
			// The provider is created without a prefix, so it sees every
			// process environment variable. Only variables that contain the
			// `__` path delimiter can map onto the config tree; blanking the
			// key makes the provider drop the variable entirely. Without this
			// filter, a plain scalar variable whose lowercased name collides
			// with a top-level config key (e.g. LOG=debug replacing the
			// whole log.* subtree) breaks unmarshalling and refuses the boot.
			// Variables with an unknown prefix but a `__` delimiter (e.g.
			// FOO__BAR=x) land on unmapped keys and are ignored by the
			// struct unmarshal, so they are harmless.
			//
			// MCP header variables are dropped here unconditionally: they are
			// parsed directly from the environment after the unmarshal (see
			// applyMCPHeaderEnv). Routing them through the Koanf tree makes
			// two header names that are path-prefixes of each other (x.api as
			// a scalar vs x.api.key as a nested map) race in the merge, and
			// one of the two headers is silently lost depending on environ
			// order.
			if !strings.Contains(key, "__") {
				return "", nil
			}
			if isMCPHeaderVar(key) {
				return "", nil
			}
			if strings.TrimSpace(value) == "" {
				return "", nil
			}

			return strings.ToLower(key), value
		},
	}), nil); err != nil {
		return nil, fmt.Errorf("config: load env: %w", err)
	}

	if err := applyHistoryLimitAliases(k); err != nil {
		return nil, fmt.Errorf("config: apply history limit aliases: %w", err)
	}

	var cfg Config
	if err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{
		DecoderConfig: &mapstructure.DecoderConfig{
			WeaklyTypedInput: true,
			DecodeHook: mapstructure.ComposeDecodeHookFunc(
				csvInt64SliceHook(),
				csvStringSliceHook(),
				mapstructure.StringToTimeDurationHookFunc(),
				decimalIntHook(),
			),
		},
	}); err != nil {
		return nil, fmt.Errorf("config: unmarshal: %w", err)
	}

	applyMCPHeaderEnv(&cfg)

	// Endpoint URLs are trimmed at load time: validate() trims only for its
	// own checks, so a value set with surrounding spaces would pass
	// validation and then fail the connection with a misleading error.
	for name, server := range cfg.MCP.Servers {
		trimmed := strings.TrimSpace(server.URL)
		if trimmed == server.URL {
			continue
		}
		server.URL = trimmed
		cfg.MCP.Servers[name] = server
	}

	// Per-feature model fallback: summarize and image_recognition inherit the
	// chat model when not explicitly set. This keeps the existing operator
	// experience of "set CHAT and you're done" while still allowing overrides.
	if cfg.LLM.Features.Summarize.Model == "" {
		cfg.LLM.Features.Summarize.Model = cfg.LLM.Features.Chat.Model
	}
	if cfg.LLM.Features.ImageRecognition.Model == "" {
		cfg.LLM.Features.ImageRecognition.Model = cfg.LLM.Features.Chat.Model
	}

	if err := cfg.MCP.Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	return &cfg, nil
}

// applyDefaults seeds the Koanf instance with the values that the operator
// would otherwise have to set explicitly. Env values loaded later override
// these defaults.
func applyDefaults(k *koanf.Koanf) error {
	defaults := map[string]any{
		"llm.tool_loop_max_iterations":   6,
		"llm.image_recognition_enabled":  true,
		"bot.uncompressed_history_limit": 15,
		"bot.history_summary_threshold":  5,
		"bot.processing_timeout":         "30s",
		"state.max_bytes":                int64(256 << 20),
		"state.history_max_bytes":        int64(160 << 20),
		"state.history_streams_max":      1024,
		"state.image_cache_max_bytes":    int64(64 << 20),
		"state.image_cache_ttl":          "24h",
		"log.level":                      "info",
		"persistent.store_path":          "/data/db.sqlite",
		"search.backend":                 SearchBackendNone,
	}
	for key, value := range defaults {
		if err := k.Set(key, value); err != nil {
			return fmt.Errorf("set %q: %w", key, err)
		}
	}

	return nil
}

// historyLimitAliases maps the documented environment variable paths of the
// history limits onto the BotConfig paths where the fields actually live. The
// limits used to be part of the LLM section before the Koanf migration, so
// their documented names (LLM__UNCOMPRESSED_HISTORY_LIMIT,
// LLM__HISTORY_SUMMARY_THRESHOLD) carry the llm.* prefix. When such a variable
// is present in the environment, its value is copied onto the bot.* path.
// Because the alias is applied last, a documented LLM__* variable wins if the
// same limit is also provided through its canonical BOT__* name.
func applyHistoryLimitAliases(k *koanf.Koanf) error {
	aliases := [][2]string{
		{"llm.uncompressed_history_limit", "bot.uncompressed_history_limit"},
		{"llm.history_summary_threshold", "bot.history_summary_threshold"},
	}

	for _, alias := range aliases {
		// The llm.* path only exists when the environment provided it: the
		// defaults for these fields are seeded under bot.* directly.
		value := k.Get(alias[0])
		if value == nil {
			continue
		}
		if err := k.Set(alias[1], value); err != nil {
			return fmt.Errorf("set %q from %q: %w", alias[1], alias[0], err)
		}
	}

	return nil
}

// isMCPHeaderVar reports whether an environment variable name is an MCP
// request header: MCP__SERVERS__<SERVER>__HEADERS__<SEG>[__SEG...]. The shape
// is checked before lowercasing, so the segment comparison is case-insensitive.
func isMCPHeaderVar(key string) bool {
	parts := strings.Split(key, "__")

	return len(parts) >= 5 &&
		strings.EqualFold(parts[0], "mcp") &&
		strings.EqualFold(parts[1], "servers") &&
		strings.EqualFold(parts[3], "headers")
}

// applyMCPHeaderEnv merges MCP request headers from the environment directly
// into the loaded config, bypassing Koanf. Environment variable names cannot
// contain "-", so a hyphenated header is spelled with multiple `__`-separated
// segments: MCP__SERVERS__S__HEADERS__X__API__KEY yields the header name
// "x-api-key". A single segment keeps its literal underscores:
// HEADERS__X_API_KEY yields "x_api_key". Keys are lowercased, matching the
// env provider's transform.
//
// Headers are parsed here because Koanf models a header name as a nested
// path under mcp.servers.<name>.headers, and a merge of two names that are
// path-prefixes of each other (x.api scalar vs x.api.key map) silently drops
// one of the two depending on environ order. Headers for a server name with
// no entry in cfg.MCP.Servers are silently skipped, mirroring how the struct
// unmarshal ignores unmapped keys. Blank values are treated as unset, like
// every other variable.
func applyMCPHeaderEnv(cfg *Config) {
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || !isMCPHeaderVar(key) {
			continue
		}
		if strings.TrimSpace(value) == "" {
			continue
		}

		parts := strings.Split(key, "__")
		server := strings.ToLower(parts[2])
		serverCfg, ok := cfg.MCP.Servers[server]
		if !ok {
			continue
		}

		headerName := strings.ToLower(strings.Join(parts[4:], "-"))
		if serverCfg.Headers == nil {
			serverCfg.Headers = make(map[string]string)
		}
		serverCfg.Headers[headerName] = value
		cfg.MCP.Servers[server] = serverCfg
	}
}

// RouteForFeature returns the per-feature model route for the given feature name.
// The feature names match the values used by the LLM service (e.g. "chat",
// "summarize", "image_recognition").
func (c LLMConfig) RouteForFeature(feature string) (FeatureRouteConfig, error) {
	switch feature {
	case "chat":
		return c.Features.Chat, nil
	case "summarize":
		return c.Features.Summarize, nil
	case "image_recognition":
		return c.Features.ImageRecognition, nil
	default:
		return FeatureRouteConfig{}, fmt.Errorf("unknown feature %q", feature)
	}
}

// Validate checks the MCP configuration for semantic problems. Servers are
// only validated when MCP is enabled: the manager short-circuits on Enabled
// and never connects, so an operator who set MCP variables but forgot
// MCP__ENABLED=true gets a booting bot instead of a refused one. The checks
// are:
//
//   - `URL` is a valid HTTP or HTTPS URL; HTTPS is required unless `Insecure` is true.
//   - `Timeout`, if set, is positive. It is a per-tool-call timeout and has no
//     upper limit; zero means "use the package default".
//   - `InvocationPolicy`, if non-empty, matches a known policy value.
//   - `ResultCharBudget`, if set, is positive. Zero (or absent) means "use the
//     default budget"; there is no upper cap.
//   - `SideEffecting` nil is allowed (the bot derives it from `destructiveHint`).
//   - `Headers` keys are non-empty and must not name a protocol-managed
//     header (values are never validated; they are secrets and must not be
//     logged at startup).
func (c MCPConfig) Validate() error {
	if !c.Enabled {
		return nil
	}

	for name, server := range c.Servers {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("mcp.servers: server name must not be empty")
		}
		if err := server.validate(name); err != nil {
			return err
		}
	}

	return nil
}

// redactMCPEndpoint reduces a configured MCP endpoint to scheme://host/path
// before it is echoed into a validation error, which ends up in stderr and
// container logs. MCP endpoints commonly carry credentials (as query
// parameters or userinfo), and the scheme may be one the validation below
// rejects, so no validation path may quote the endpoint verbatim; the
// redaction accepts any scheme, not just http(s).
func redactMCPEndpoint(rawURL string) string {
	return urlredact.RedactOne(rawURL)
}

// mcpProtocolHeaders lists the header names the MCP streamable-HTTP transport
// sets itself on every request: Accept, Content-Type, Mcp-Session-Id, and
// MCP-Protocol-Version. The MCP client applies operator headers with
// Header.Set (replace semantics, deliberate: no duplicate headers), so an
// operator entry with one of these names replaces the SDK-set value and
// breaks the protocol handshake at boot with a confusing server error. They
// are refused at load time instead. HTTP header names are case-insensitive,
// so the check is too.
var mcpProtocolHeaders = map[string]struct{}{
	"accept":               {},
	"content-type":         {},
	"mcp-session-id":       {},
	"mcp-protocol-version": {},
}

func (s MCPServerConfig) validate(name string) error {
	rawURL := strings.TrimSpace(s.URL)
	if rawURL == "" {
		return fmt.Errorf("mcp.servers.%s.url is required", name)
	}

	parsed, err := url.ParseRequestURI(rawURL)
	if err != nil {
		// A *url.Error quotes the raw URL it failed on, so rebuild it around
		// the redacted endpoint: the query (often credentials) must stay out
		// of startup diagnostics while the cause stays wrapped for callers.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = &url.Error{Op: uerr.Op, URL: redactMCPEndpoint(rawURL), Err: uerr.Err}
		}

		return fmt.Errorf("mcp.servers.%s.url %q is not a valid URL: %w", name, redactMCPEndpoint(rawURL), err)
	}

	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "https":
		// always allowed
	case "http":
		if !s.Insecure {
			return fmt.Errorf("mcp.servers.%s.url %q uses http://; set mcp.servers.%s.insecure=true to allow it", name, redactMCPEndpoint(rawURL), name)
		}
	default:
		return fmt.Errorf("mcp.servers.%s.url %q has unsupported scheme %q (use https or http with insecure=true)", name, redactMCPEndpoint(rawURL), parsed.Scheme)
	}

	// Timeout is a per-tool-call timeout: when set it must simply be positive.
	// There is no upper clamp, because slow tools such as browser automation
	// legitimately need 120s or more. Zero means "use the client default".
	if s.Timeout < 0 {
		return fmt.Errorf("mcp.servers.%s.timeout must be positive when set (omit it to use the default), got %s", name, s.Timeout)
	}

	if s.InvocationPolicy != "" {
		switch InvocationPolicyFor(s.InvocationPolicy) {
		case InvocationPolicyExplicitRequestOnly, InvocationPolicyDiscretionary, InvocationPolicyDiscretionaryPaid:
			// ok
		default:
			return fmt.Errorf("mcp.servers.%s.invocation_policy %q is not a known value (use explicit_request_only, discretionary, or discretionary_paid)", name, s.InvocationPolicy)
		}
	}

	// Zero means "unset": the per-tool budget falls back to the default.
	if s.ResultCharBudget < 0 {
		return fmt.Errorf("mcp.servers.%s.result_char_budget %d must be positive when set (omit it to use the default budget)", name, s.ResultCharBudget)
	}

	for headerName := range s.Headers {
		if strings.TrimSpace(headerName) == "" {
			return fmt.Errorf("mcp.servers.%s.headers contains an empty header name", name)
		}
		if _, managed := mcpProtocolHeaders[strings.ToLower(headerName)]; managed {
			// The name is echoed deliberately; the value is a secret and
			// must never appear in startup diagnostics.
			return fmt.Errorf("mcp.servers.%s.headers: %q is managed by the MCP protocol and cannot be overridden", name, headerName)
		}
	}

	return nil
}

// decimalIntHook decodes string values into integer fields with
// strconv.ParseInt in base 10. With WeaklyTypedInput, mapstructure would
// otherwise parse numeric strings via ParseInt(s, 0, ...), so a leading zero
// meant octal: "010" silently became 8 and "08" refused the boot. The
// pre-Koanf loader used strconv.Atoi, i.e. base 10. A value that fails to
// parse is returned as an error, matching the documented contract that a
// present-but-unparseable env value is a hard startup error instead of a
// silent fallback to the default. The hook runs after the duration hook, so
// duration strings never reach it, and its type assertion skips non-string
// values that earlier hooks already converted.
func decimalIntHook() mapstructure.DecodeHookFunc {
	return func(f, t reflect.Type, data any) (any, error) {
		if f.Kind() != reflect.String || (t.Kind() != reflect.Int && t.Kind() != reflect.Int64) {
			return data, nil
		}
		raw, ok := data.(string)
		if !ok {
			return data, nil
		}
		n, err := strconv.ParseInt(raw, 10, t.Bits())
		if err != nil {
			return nil, fmt.Errorf("parse %q as int: %w", raw, err)
		}

		return n, nil
	}
}

// csvInt64SliceHook parses a comma-separated string of decimal integers into a
// []int64. Empty entries are skipped; an unparseable entry is a hard error
// (operators benefit from knowing the value is broken rather than silently
// losing a permission).
func csvInt64SliceHook() mapstructure.DecodeHookFunc {
	int64SliceType := reflect.TypeOf([]int64(nil))

	return func(f, t reflect.Type, data any) (any, error) {
		if f.Kind() != reflect.String || t != int64SliceType {
			return data, nil
		}
		raw, ok := data.(string)
		if !ok {
			return data, nil
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return []int64{}, nil
		}
		parts := strings.Split(raw, ",")
		out := make([]int64, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			n, err := strconv.ParseInt(part, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse %q as int64: %w", part, err)
			}
			out = append(out, n)
		}

		return out, nil
	}
}

// csvStringSliceHook splits a comma-separated string into a []string. Empty
// entries are skipped. The default mapstructure.StringToSliceHookFunc does
// not trim whitespace or skip empties, so we override it for this type.
func csvStringSliceHook() mapstructure.DecodeHookFunc {
	stringSliceType := reflect.TypeOf([]string(nil))

	return func(f, t reflect.Type, data any) (any, error) {
		if f.Kind() != reflect.String || t != stringSliceType {
			return data, nil
		}
		raw, ok := data.(string)
		if !ok {
			return data, nil
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return []string{}, nil
		}
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}

		return out, nil
	}
}
