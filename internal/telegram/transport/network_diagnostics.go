package transport

import (
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	"telegram-ollama-reply-bot/internal/config"
)

const telegramAPIURL = "https://api.telegram.org"

func LogNetworkRouting(logger *slog.Logger, cfg *config.Config) {
	logger.Debug(
		"network routing snapshot",
		"telegram_transport", "net/http",
		"telegram_api_url", telegramAPIURL,
		"telegram_proxy", resolveProxyForURL(telegramAPIURL, http.ProxyFromEnvironment),
		"llm_chat_backend", cfg.LLM.Features.Chat.Backend,
		"llm_chat_base_url", activeLLMBaseURL(cfg),
		"llm_chat_proxy", resolveProxyForURL(activeLLMBaseURL(cfg), http.ProxyFromEnvironment),
		"http_proxy", redactProxyEnv("HTTP_PROXY"),
		"https_proxy", redactProxyEnv("HTTPS_PROXY"),
		"all_proxy", redactProxyEnv("ALL_PROXY"),
		"no_proxy", redactNoProxyEnv("NO_PROXY"),
		"http_proxy_lower", redactProxyEnv("http_proxy"),
		"https_proxy_lower", redactProxyEnv("https_proxy"),
		"all_proxy_lower", redactProxyEnv("all_proxy"),
		"no_proxy_lower", redactNoProxyEnv("no_proxy"),
	)
}

func activeLLMBaseURL(cfg *config.Config) string {
	switch cfg.LLM.Features.Chat.Backend {
	case config.LLMBackendOllama:
		return cfg.LLM.Backends.Ollama.BaseURL
	case config.LLMBackendOpenAICompat:
		return cfg.LLM.Backends.OpenAICompat.BaseURL
	default:
		return ""
	}
}

func resolveProxyForURL(rawURL string, proxy func(*http.Request) (*url.URL, error)) string {
	if rawURL == "" {
		return "n/a"
	}

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "request_error:" + err.Error()
	}

	proxyURL, err := proxy(req)
	if err != nil {
		return "proxy_error:" + err.Error()
	}
	if proxyURL == nil {
		return "direct"
	}

	return redactURL(proxyURL)
}

func redactProxyEnv(key string) string {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return "unset"
	}

	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return "set"
	}

	return redactURL(parsed)
}

func redactNoProxyEnv(key string) string {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return "unset"
	}

	return value
}

func redactURL(raw *url.URL) string {
	if raw == nil {
		return "unset"
	}

	safe := *raw
	if safe.User != nil {
		username := safe.User.Username()
		if username == "" {
			safe.User = url.User("redacted")
		} else {
			safe.User = url.UserPassword(username, "redacted")
		}
	}

	return safe.String()
}
