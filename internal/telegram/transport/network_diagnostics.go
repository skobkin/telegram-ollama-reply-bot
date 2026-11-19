package transport

import (
	"log/slog"
	"net/url"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/support/urlredact"

	"golang.org/x/net/http/httpproxy"
)

const telegramAPIURL = "https://api.telegram.org"

func LogNetworkRouting(logger *slog.Logger, cfg *config.Config) {
	proxyConfig := httpproxy.FromEnvironment()
	proxyFunc := proxyConfig.ProxyFunc()

	logger.Info(
		"network routing snapshot",
		"telegram_transport", "net/http",
		"http_proxy_effective", redactProxyValue(proxyConfig.HTTPProxy),
		"https_proxy_effective", redactProxyValue(proxyConfig.HTTPSProxy),
		"no_proxy_effective", redactNoProxyValue(proxyConfig.NoProxy),
		"telegram_api_url", telegramAPIURL,
		"telegram_proxy", resolveProxyForURL(telegramAPIURL, proxyFunc),
		"llm_backend", config.LLMBackendOpenAICompat,
		// Logged redacted for the same reason as the System One endpoint
		// below: backend endpoint URLs can carry credentials in userinfo.
		"llm_chat_base_url", redactEndpointURL(activeLLMBaseURL(cfg)),
		"llm_chat_proxy", resolveProxyForURL(activeLLMBaseURL(cfg), proxyFunc),
		// The System One endpoint rides the same environment-resolved proxy
		// policy as every other outbound connection (no System One-specific
		// routing exists); it is logged redacted because endpoint URLs can
		// carry credentials. An empty endpoint reads as unset.
		"systemone_base_url", redactEndpointURL(cfg.SystemOne.BaseURL),
		"systemone_proxy", resolveProxyForURL(cfg.SystemOne.BaseURL, proxyFunc),
	)
}

// redactEndpointURL renders an endpoint URL with embedded credentials
// redacted; an empty value reads as unset.
func redactEndpointURL(raw string) string {
	if raw == "" {
		return "unset"
	}

	return urlredact.RedactOne(raw)
}

func activeLLMBaseURL(cfg *config.Config) string {
	return cfg.LLM.Backends.OpenAICompat.BaseURL
}

func resolveProxyForURL(rawURL string, proxy func(*url.URL) (*url.URL, error)) string {
	if rawURL == "" {
		return "n/a"
	}

	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return "request_error:" + err.Error()
	}

	proxyURL, err := proxy(parsedURL)
	if err != nil {
		return "proxy_error:" + err.Error()
	}
	if proxyURL == nil {
		return "direct"
	}

	return redactURL(proxyURL)
}

func redactProxyValue(value string) string {
	if value == "" {
		return "unset"
	}

	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return "set"
	}

	return redactURL(parsed)
}

func redactNoProxyValue(value string) string {
	if value == "" {
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
