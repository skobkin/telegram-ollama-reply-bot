package app

import (
	"errors"
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
//
// Extraction is manual rather than url.Parse-based: a URL with userinfo but
// a malformed remainder (an invalid escape in the path, say) must still
// yield its credential — startup diagnostics quote such URLs verbatim. Both
// the raw (percent-encoded) and the decoded password forms are registered
// when they differ.
func appendURLPassword(secrets []secretscrub.Secret, name, rawURL string) []secretscrub.Secret {
	userinfo, ok := urlUserinfoPrefix(rawURL)
	if !ok {
		return secrets
	}

	_, password, hasPassword := strings.Cut(userinfo, ":")
	if !hasPassword || password == "" {
		return secrets
	}

	secrets = append(secrets, secretscrub.Secret{Name: name, Value: password})

	if decoded, err := url.PathUnescape(password); err == nil && decoded != password {
		secrets = append(secrets, secretscrub.Secret{Name: name + "-decoded", Value: decoded})
	}

	return secrets
}

// urlUserinfoPrefix extracts the userinfo part ("user:password") of a URL
// string, tolerating a malformed remainder after the authority.
func urlUserinfoPrefix(rawURL string) (string, bool) {
	separator := strings.Index(rawURL, "://")
	if separator < 0 {
		return "", false
	}

	authority := rawURL[separator+3:]
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		authority = authority[:end]
	}

	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return "", false
	}

	userinfo := authority[:at]
	if userinfo == "" {
		return "", false
	}

	return userinfo, true
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

	// Scope-sourced payloads (breadcrumbs, fingerprint, extra, contexts) are
	// shared by reference between concurrent events, so every scrubbed copy
	// is event-owned instead of mutating shared memory in place.
	if len(event.Fingerprint) > 0 {
		fingerprint := make([]string, len(event.Fingerprint))

		for i, part := range event.Fingerprint {
			fingerprint[i] = scrubber.Scrub(part)
		}

		event.Fingerprint = fingerprint
	}

	for i := range event.Exception {
		event.Exception[i].Type = scrubber.Scrub(event.Exception[i].Type)
		event.Exception[i].Value = scrubber.Scrub(event.Exception[i].Value)

		if mechanism := event.Exception[i].Mechanism; mechanism != nil {
			mechanism.Description = scrubber.Scrub(mechanism.Description)
			mechanism.Data = copyScrubAnyMap(scrubber, mechanism.Data, maxScrubDepth)
		}
	}

	for i, crumb := range event.Breadcrumbs {
		if crumb == nil {
			continue
		}

		owned := *crumb
		owned.Message = scrubber.Scrub(crumb.Message)
		owned.Data = copyScrubAnyMap(scrubber, crumb.Data, maxScrubDepth)
		event.Breadcrumbs[i] = &owned
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
	event.Extra = copyScrubAnyMap(scrubber, event.Extra, maxScrubDepth)

	for name, contextData := range event.Contexts {
		event.Contexts[name] = copyScrubAnyMap(scrubber, contextData, maxScrubDepth)
	}
}

// scrubStringMap scrubs values in place; keys are tag/header names, not
// secret-bearing content. String maps hold immutable values and are only
// ever event-owned (built fresh by the scope merge), so in-place mutation
// is safe here.
func scrubStringMap(scrubber *secretscrub.Scrubber, values map[string]string) {
	for key, value := range values {
		values[key] = scrubber.Scrub(value)
	}
}

// copyScrubAnyMap returns an event-owned copy of an arbitrary payload map
// with every text value scrubbed, replacing the shared original.
func copyScrubAnyMap(scrubber *secretscrub.Scrubber, values map[string]any, depth int) map[string]any {
	if values == nil {
		return nil
	}

	if depth <= 0 {
		return values
	}

	out := make(map[string]any, len(values))

	for key, value := range values {
		out[key] = copyScrubAny(scrubber, value, depth-1)
	}

	return out
}

// copyScrubAny scrubs one arbitrary payload value into owned storage:
// strings are immutable and scrub directly; maps and slices are copied and
// walked. Everything else (numbers, bools, SDK objects) cannot carry secret
// text and is passed through.
func copyScrubAny(scrubber *secretscrub.Scrubber, value any, depth int) any {
	if depth <= 0 {
		return value
	}

	switch typed := value.(type) {
	case string:
		return scrubber.Scrub(typed)
	case map[string]any:
		return copyScrubAnyMap(scrubber, typed, depth)
	case map[string]string:
		out := make(map[string]string, len(typed))

		for key, elem := range typed {
			out[key] = scrubber.Scrub(elem)
		}

		return out
	case []any:
		out := make([]any, len(typed))

		for i, elem := range typed {
			out[i] = copyScrubAny(scrubber, elem, depth-1)
		}

		return out
	case []string:
		out := make([]string, len(typed))

		for i, elem := range typed {
			out[i] = scrubber.Scrub(elem)
		}

		return out
	default:
		return value
	}
}

// scrubFatalError scrubs the error returned by Run, which main prints to
// plain stderr — the one diagnostic surface no other scrubbing layer covers.
// A nil scrubber or nil error passes through unchanged.
func scrubFatalError(scrubber *secretscrub.Scrubber, err error) error {
	if scrubber == nil || err == nil {
		return err
	}

	return errors.New(scrubber.Scrub(err.Error()))
}
