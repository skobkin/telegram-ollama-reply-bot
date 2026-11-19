package app

import (
	"net/url"
	"strings"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/support/secretscrub"

	"github.com/getsentry/sentry-go"
)

// newSecretScrubber registers every secret the app is configured with, so a
// single scrubber can clean Sentry events, log records, and HTTP client
// errors. Every new secret-bearing configuration value (API tokens, keys,
// credential header values, URL-embedded passwords) must be added here — a
// secret missing from this registry can surface verbatim in error messages,
// logs, and Sentry issue titles.
func newSecretScrubber(cfg *config.Config) *secretscrub.Scrubber {
	secrets := []secretscrub.Secret{
		{Name: "telegram-token", Value: cfg.Bot.Telegram.Token},
		{Name: "llm-api-token", Value: cfg.LLM.Backends.OpenAICompat.APIToken},
		{Name: "llm-image-api-token", Value: cfg.LLM.ImageRecognition.OpenAICompat.APIToken},
		{Name: "llm-image-minimax-api-key", Value: cfg.LLM.ImageRecognition.MiniMaxVLM.APIKey},
		{Name: "search-tavily-api-key", Value: cfg.Providers.Tavily.APIKey},
		{Name: "search-kagi-api-key", Value: cfg.Providers.Kagi.APIKey},
		{Name: "systemone-api-key", Value: cfg.SystemOne.APIKey},
		{Name: "sentry-dsn", Value: cfg.Sentry.DSN},
	}

	// Backend endpoint URLs may carry basic-auth userinfo; only the password
	// part is registered (see appendURLPassword). Startup diagnostics and
	// validation errors quote these URLs verbatim.
	for name, endpoint := range map[string]string{
		"llm-backend-url-password":        cfg.LLM.Backends.OpenAICompat.BaseURL,
		"llm-image-backend-url-password":  cfg.LLM.ImageRecognition.OpenAICompat.BaseURL,
		"llm-image-minimax-url-password":  cfg.LLM.ImageRecognition.MiniMaxVLM.BaseURL,
		"systemone-endpoint-url-password": cfg.SystemOne.BaseURL,
	} {
		secrets = appendURLPassword(secrets, name, endpoint)
	}

	for name, server := range cfg.MCP.Servers {
		for header, value := range server.Headers {
			secrets = append(secrets, secretscrub.Secret{
				Name:  "mcp-" + name + "-header-" + header,
				Value: value,
			})

			// Providers echo the bare credential in errors ("invalid api
			// key X"), not the full header value, so the credential part of
			// scheme-prefixed values is registered alongside the whole.
			if credential, ok := credentialFromHeader(value); ok {
				secrets = append(secrets, secretscrub.Secret{
					Name:  "mcp-" + name + "-header-" + header + "-credential",
					Value: credential,
				})
			}
		}

		// Only the password part of an MCP endpoint URL is registered: a
		// generic username would mutilate unrelated text, and net/http
		// already masks passwords inside its own url.Error rendering.
		secrets = appendURLPassword(secrets, "mcp-"+name+"-url-password", server.URL)
	}

	return secretscrub.New(secrets)
}

// credentialFromHeader extracts the credential part of a scheme-prefixed
// header value ("Bearer X", "Token X", …), which is what providers echo in
// their own error messages.
func credentialFromHeader(value string) (string, bool) {
	scheme, credential, found := strings.Cut(value, " ")
	if !found || credential == "" {
		return "", false
	}

	switch strings.ToLower(scheme) {
	case "bearer", "basic", "bot", "token":
		return credential, true
	default:
		return "", false
	}
}

// appendURLPassword appends the password embedded in an endpoint URL's
// userinfo, if any. The username is deliberately never registered: a
// generic username would mutilate unrelated text.
func appendURLPassword(secrets []secretscrub.Secret, name, rawURL string) []secretscrub.Secret {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.User == nil {
		return secrets
	}

	if password, ok := parsed.User.Password(); ok && password != "" {
		secrets = append(secrets, secretscrub.Secret{Name: name, Value: password})
	}

	return secrets
}

// newSentryBeforeSend returns a BeforeSend hook that scrubs every registered
// secret from all string-bearing surfaces of an error event, in place, and
// always returns the event (returning nil would drop it).
//
// Error chains are already flattened into Event.Exception entries before
// BeforeSend runs (sentry-go CaptureException → EventFromException →
// processEvent), so scrubbing each exception's Type and Value covers the
// full chain text. Stacktraces, SDK metadata, and module lists are skipped
// deliberately: they carry this binary's own source and build information,
// which never embed environment-derived secret values. Transactions and
// check-ins never reach BeforeSend (separate hooks), and the app attaches no
// log or metric payloads through it.
func newSentryBeforeSend(scrubber *secretscrub.Scrubber) func(*sentry.Event, *sentry.EventHint) *sentry.Event {
	return func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
		if event == nil {
			return nil
		}

		scrubEvent(scrubber, event)

		return event
	}
}

// maxScrubDepth bounds recursion into nested map/slice values before
// leaving deeper content untouched; real payloads sit far above this, and
// the cap also terminates self-referencing maps.
const maxScrubDepth = 8

func scrubEvent(scrubber *secretscrub.Scrubber, event *sentry.Event) {
	if scrubber == nil {
		return
	}

	event.Message = scrubber.Scrub(event.Message)
	event.Transaction = scrubber.Scrub(event.Transaction)

	for i, part := range event.Fingerprint {
		event.Fingerprint[i] = scrubber.Scrub(part)
	}

	for i := range event.Exception {
		event.Exception[i].Type = scrubber.Scrub(event.Exception[i].Type)
		event.Exception[i].Value = scrubber.Scrub(event.Exception[i].Value)

		if mechanism := event.Exception[i].Mechanism; mechanism != nil {
			mechanism.Description = scrubber.Scrub(mechanism.Description)
			scrubAnyMap(scrubber, mechanism.Data, maxScrubDepth)
		}
	}

	for _, crumb := range event.Breadcrumbs {
		if crumb == nil {
			continue
		}

		crumb.Message = scrubber.Scrub(crumb.Message)
		scrubAnyMap(scrubber, crumb.Data, maxScrubDepth)
	}

	if event.Request != nil {
		event.Request.URL = scrubber.Scrub(event.Request.URL)
		event.Request.Data = scrubber.Scrub(event.Request.Data)
		event.Request.QueryString = scrubber.Scrub(event.Request.QueryString)
		event.Request.Cookies = scrubber.Scrub(event.Request.Cookies)
		scrubStringMap(scrubber, event.Request.Headers)
		scrubStringMap(scrubber, event.Request.Env)
	}

	event.User.ID = scrubber.Scrub(event.User.ID)
	event.User.Email = scrubber.Scrub(event.User.Email)
	event.User.IPAddress = scrubber.Scrub(event.User.IPAddress)
	event.User.Username = scrubber.Scrub(event.User.Username)
	event.User.Name = scrubber.Scrub(event.User.Name)
	scrubStringMap(scrubber, event.User.Data)

	for i := range event.Threads {
		event.Threads[i].Name = scrubber.Scrub(event.Threads[i].Name)
	}

	scrubStringMap(scrubber, event.Tags)
	scrubAnyMap(scrubber, event.Extra, maxScrubDepth)

	for _, contextData := range event.Contexts {
		scrubAnyMap(scrubber, contextData, maxScrubDepth)
	}
}

// scrubStringMap scrubs values in place; keys are tag/header names, not
// secret-bearing content.
func scrubStringMap(scrubber *secretscrub.Scrubber, values map[string]string) {
	for key, value := range values {
		values[key] = scrubber.Scrub(value)
	}
}

func scrubAnyMap(scrubber *secretscrub.Scrubber, values map[string]any, depth int) {
	if depth <= 0 {
		return
	}

	for key, value := range values {
		values[key] = scrubAny(scrubber, value, depth)
	}
}

// scrubAny scrubs one arbitrary payload value: strings directly, maps and
// slices by recursive in-place walk. Everything else (numbers, bools, SDK
// objects) cannot carry secret text and is passed through.
func scrubAny(scrubber *secretscrub.Scrubber, value any, depth int) any {
	if depth <= 0 {
		return value
	}

	switch typed := value.(type) {
	case string:
		return scrubber.Scrub(typed)
	case map[string]any:
		scrubAnyMap(scrubber, typed, depth-1)

		return typed
	case map[string]string:
		scrubStringMap(scrubber, typed)

		return typed
	case []any:
		for i, elem := range typed {
			typed[i] = scrubAny(scrubber, elem, depth-1)
		}

		return typed
	case []string:
		for i, elem := range typed {
			typed[i] = scrubber.Scrub(elem)
		}

		return typed
	default:
		return value
	}
}
