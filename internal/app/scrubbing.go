package app

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/support/secretscrub"

	"github.com/getsentry/sentry-go"
	"golang.org/x/net/http/httpproxy"
)

// newSecretScrubber registers every secret the app is configured with, so a
// single scrubber can clean Sentry events, log records, and HTTP client
// errors. Every new secret-bearing configuration value (API tokens, keys,
// credential header values, URL-embedded passwords) must be added here — a
// secret missing from this registry can surface verbatim in error messages,
// logs, and Sentry issue titles.
func newSecretScrubber(cfg *config.Config) *secretscrub.Scrubber {
	var secrets []secretscrub.Secret

	// Scalar secrets also register their derived renderings — trimmed and
	// quoted — because those are the forms remote surfaces and error
	// diagnostics actually carry.
	for _, secret := range []secretscrub.Secret{
		{Name: "telegram-token", Value: cfg.Bot.Telegram.Token},
		{Name: "llm-api-token", Value: cfg.LLM.Backends.OpenAICompat.APIToken},
		{Name: "llm-image-api-token", Value: cfg.LLM.ImageRecognition.OpenAICompat.APIToken},
		{Name: "llm-image-minimax-api-key", Value: cfg.LLM.ImageRecognition.MiniMaxVLM.APIKey},
		{Name: "search-tavily-api-key", Value: cfg.Providers.Tavily.APIKey},
		{Name: "search-kagi-api-key", Value: cfg.Providers.Kagi.APIKey},
		{Name: "systemone-api-key", Value: cfg.SystemOne.APIKey},
		{Name: "sentry-dsn", Value: cfg.Sentry.DSN},
	} {
		secrets = append(secrets, secret)
		secrets = withDerivedForms(secrets, secret)
	}

	// The DSN's userinfo parts are credential components, not a username.
	// Register them individually: net/url re-renders the parsed URL in its
	// errors (dropping the fragment, say), so renderings exist that no
	// full-DSN string matches, while the components match wherever they
	// appear.
	secrets = withURLCredentials(secrets, "sentry-dsn", cfg.Sentry.DSN)

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
			headerName := "mcp-" + name + "-header-" + header

			// Every form the value can take on a remote surface gets
			// registered, each with its %q rendering alongside: diagnostics
			// quote whichever form they carry.
			forms := []secretscrub.Secret{
				{Name: headerName, Value: value},
			}

			// HTTP transmission trims surrounding whitespace, so providers
			// see and echo the trimmed form. This also covers bare
			// (scheme-less) header values, which credentialFromHeader does
			// not normalize.
			if trimmed := strings.TrimSpace(value); trimmed != "" && trimmed != value {
				forms = append(forms, secretscrub.Secret{Name: headerName + "-trimmed", Value: trimmed})
			}

			// Providers echo the bare credential in errors ("invalid api
			// key X"), not the full header value, so the credential part of
			// scheme-prefixed values is registered alongside the whole.
			if credential, ok := credentialFromHeader(value); ok {
				forms = append(forms, secretscrub.Secret{Name: headerName + "-credential", Value: credential})
			}

			for _, form := range forms {
				secrets = append(secrets, form)
				secrets = withQuoted(secrets, form.Name, form.Value)
			}
		}

		// Only the password part of an MCP endpoint URL is registered: a
		// generic username would mutilate unrelated text, and net/http
		// already masks passwords inside its own url.Error rendering.
		secrets = appendURLPassword(secrets, "mcp-"+name+"-url-password", server.URL)
	}

	// Outbound proxies follow the standard environment variables, resolved
	// with the same httpproxy.FromEnvironment semantics the HTTP clients
	// use (which covers the lowercase variable variants). Only the userinfo
	// password is registered — a generic username would mutilate unrelated
	// text — because proxy dial errors and network diagnostics can carry
	// the proxy address. NO_PROXY is a host list without credentials.
	proxyConfig := httpproxy.FromEnvironment()
	for name, proxy := range map[string]string{
		"http-proxy-url-password":  proxyConfig.HTTPProxy,
		"https-proxy-url-password": proxyConfig.HTTPSProxy,
	} {
		secrets = appendURLPassword(secrets, name, proxy)
	}

	return secretscrub.New(secrets)
}

// bootstrapEnvSecrets builds the pre-Load registry subset straight from the
// process environment. A failed config.Load returns before newSecretScrubber
// can run, and its error reaches main's plain stderr, so bootstrap failures
// are scrubbed with the credential values Load would have registered: the
// documented secret-bearing variables, endpoint URL userinfo passwords, the
// MCP header variable family, and the proxy passwords. Names carry the
// "bootstrap-" prefix where they cannot mirror the post-Load registry ones.
func bootstrapEnvSecrets() []secretscrub.Secret {
	var secrets []secretscrub.Secret

	// The keys are environment variable names and the values registry
	// labels — configuration metadata, not credentials.
	for envName, registryName := range map[string]string{ //nolint:gosec // environment variable names and registry labels, not credentials
		"BOT__TELEGRAM__TOKEN":                             "telegram-token",
		"LLM__BACKENDS__OPENAI_COMPAT__API_TOKEN":          "llm-api-token",
		"LLM__IMAGE_RECOGNITION__OPENAI_COMPAT__API_TOKEN": "llm-image-api-token",
		"LLM__IMAGE_RECOGNITION__MINIMAX_VLM__API_KEY":     "llm-image-minimax-api-key",
		"PROVIDERS__TAVILY__API_KEY":                       "search-tavily-api-key",
		"PROVIDERS__KAGI__API_KEY":                         "search-kagi-api-key",
		"SYSTEMONE__API_KEY":                               "systemone-api-key",
		"SENTRY__DSN":                                      "sentry-dsn",
	} {
		value, ok := os.LookupEnv(envName)
		if !ok || value == "" {
			continue
		}

		secret := secretscrub.Secret{Name: registryName, Value: value}
		secrets = append(secrets, secret)
		secrets = withDerivedForms(secrets, secret)

		if registryName == "sentry-dsn" {
			secrets = withURLCredentials(secrets, registryName, value)
		}
	}

	for envName, registryName := range map[string]string{
		"LLM__BACKENDS__OPENAI_COMPAT__BASE_URL":          "bootstrap-llm-backend-url-password",
		"LLM__IMAGE_RECOGNITION__OPENAI_COMPAT__BASE_URL": "bootstrap-llm-image-backend-url-password",
		"LLM__IMAGE_RECOGNITION__MINIMAX_VLM__BASE_URL":   "bootstrap-llm-image-minimax-url-password",
		"SYSTEMONE__BASE_URL":                             "bootstrap-systemone-endpoint-url-password",
	} {
		if value, ok := os.LookupEnv(envName); ok {
			secrets = appendURLPassword(secrets, registryName, value)
		}
	}

	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		if value == "" || !isMCPHeaderEnvVar(name) {
			continue
		}

		secret := secretscrub.Secret{Name: bootstrapMCPHeaderName(name), Value: value}
		secrets = append(secrets, secret)
		secrets = withDerivedForms(secrets, secret)
	}

	proxyConfig := httpproxy.FromEnvironment()
	for name, proxy := range map[string]string{
		"http-proxy-url-password":  proxyConfig.HTTPProxy,
		"https-proxy-url-password": proxyConfig.HTTPSProxy,
	} {
		secrets = appendURLPassword(secrets, name, proxy)
	}

	return secrets
}

// isMCPHeaderEnvVar mirrors the config package's isMCPHeaderVar: the shape
// MCP__SERVERS__<SERVER>__HEADERS__<SEG>[__SEG...], case-insensitive.
func isMCPHeaderEnvVar(key string) bool {
	parts := strings.Split(key, "__")

	return len(parts) >= 5 &&
		strings.EqualFold(parts[0], "MCP") &&
		strings.EqualFold(parts[1], "SERVERS") &&
		strings.EqualFold(parts[3], "HEADERS")
}

// bootstrapMCPHeaderName turns an MCP header variable name into a stable
// registry label: MCP__SERVERS__DOCS__HEADERS__AUTH →
// "bootstrap-mcp-docs-header-auth".
func bootstrapMCPHeaderName(envName string) string {
	parts := strings.Split(envName, "__")
	labels := make([]string, 0, len(parts))

	for i, part := range parts[2:] {
		if i == 1 && strings.EqualFold(part, "HEADERS") {
			continue
		}

		labels = append(labels, strings.ToLower(part))
	}

	return "bootstrap-mcp-" + strings.Join(labels, "-")
}

// withDerivedForms registers derived renderings of a configured secret
// alongside the configured value — the forms remote surfaces actually carry:
//
//   - trimmed: HTTP header transmission strips surrounding whitespace, so
//     the trimmed form is what services receive and echo back;
//   - quoted: rendering a value through %q — what net/url's parse errors and
//     every *url.Error message do — escapes special characters, and neither
//     the raw nor a trimmed form matches that rendering. Quoted forms are
//     registered for the raw and the trimmed value alike, since diagnostics
//     quote whichever normalized form they carry.
func withDerivedForms(secrets []secretscrub.Secret, secret secretscrub.Secret) []secretscrub.Secret {
	trimmed := strings.TrimSpace(secret.Value)

	if trimmed != "" && trimmed != secret.Value {
		secrets = append(secrets, secretscrub.Secret{
			Name:  secret.Name + "-trimmed",
			Value: trimmed,
		})

		secrets = withQuoted(secrets, secret.Name+"-trimmed", trimmed)
	}

	return withQuoted(secrets, secret.Name, secret.Value)
}

// withURLCredentials registers the credential components embedded in a DSN's
// userinfo — the public key, and the legacy secret key when present — each
// with its %q rendering alongside. Both are authentication material rather
// than a generic username, so unlike endpoint URLs, the whole userinfo
// registers; the quoted renderings match diagnostics that escape the
// components while re-rendering the URL.
func withURLCredentials(secrets []secretscrub.Secret, name, rawURL string) []secretscrub.Secret {
	userinfo, ok := urlUserinfoPrefix(rawURL)
	if !ok {
		return secrets
	}

	publicKey, secretKey, _ := strings.Cut(userinfo, ":")

	for _, component := range []struct{ name, value string }{
		{name: name + "-public-key", value: publicKey},
		{name: name + "-secret-key", value: secretKey},
	} {
		if component.value == "" {
			continue
		}

		secrets = append(secrets, secretscrub.Secret{Name: component.name, Value: component.value})
		secrets = withQuoted(secrets, component.name, component.value)
	}

	return secrets
}

// withQuoted registers the %q rendering of a value alongside the value
// itself: rendering escapes special characters, and neither the raw nor a
// trimmed form matches that rendering (strconv.Quote's inner text is
// exactly it).
func withQuoted(secrets []secretscrub.Secret, name, value string) []secretscrub.Secret {
	quoted := strconv.Quote(value)
	if len(quoted) < 2 {
		return secrets
	}

	if inner := quoted[1 : len(quoted)-1]; inner != value {
		secrets = append(secrets, secretscrub.Secret{
			Name:  name + "-quoted",
			Value: inner,
		})
	}

	return secrets
}

// credentialFromHeader extracts the credential part of a scheme-prefixed
// header value ("Bearer X", "Token X", …), which is what providers echo in
// their own error messages. Whitespace around the value is trimmed first:
// HTTP header transmission trims it, so the trimmed form is what providers
// echo back.
func credentialFromHeader(value string) (string, bool) {
	scheme, credential, found := strings.Cut(strings.TrimSpace(value), " ")
	if !found {
		return "", false
	}

	credential = strings.TrimSpace(credential)
	if credential == "" {
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

	// Every form the password can take on a diagnostic surface is
	// registered, each with its %q rendering alongside: net/url renders URLs
	// through %q in parse errors and in every *url.Error message, escaping
	// whatever form it carries.
	forms := []secretscrub.Secret{
		{Name: name, Value: password},
	}

	// The decoded form is what a server echoes back after unescaping its
	// credentials.
	if decoded, err := url.PathUnescape(password); err == nil && decoded != password {
		forms = append(forms, secretscrub.Secret{Name: name + "-decoded", Value: decoded})
	}

	for _, form := range forms {
		secrets = append(secrets, form)
		secrets = withQuoted(secrets, form.Name, form.Value)
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

// maxScrubDepth bounds recursion into nested map/slice values; the cap also
// terminates self-referencing maps. Real payloads sit far above it, and
// content beyond it fails closed (see Scrubber.TruncatedMarker) rather than
// surviving unredacted.
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
		owned.Category = scrubber.Scrub(crumb.Category)
		owned.Type = scrubber.Scrub(crumb.Type)
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
	// The scope user is copied into the event shallowly, so the Data map is
	// shared with the scope and must be replaced, not scrubbed in place.
	if event.User.Data != nil {
		userData := make(map[string]string, len(event.User.Data))

		for key, value := range event.User.Data {
			userData[scrubber.Scrub(key)] = scrubber.Scrub(value)
		}

		event.User.Data = userData
	}

	for i := range event.Threads {
		event.Threads[i].Name = scrubber.Scrub(event.Threads[i].Name)
	}

	scrubStringMap(scrubber, event.Tags)
	event.Extra = copyScrubAnyMap(scrubber, event.Extra, maxScrubDepth)

	// The contexts map is rebuilt so context names — caller-chosen strings
	// that could embed secret material — are scrubbed alongside their data.
	if len(event.Contexts) > 0 {
		contexts := make(map[string]sentry.Context, len(event.Contexts))

		for name, contextData := range event.Contexts {
			contexts[scrubber.Scrub(name)] = copyScrubAnyMap(scrubber, contextData, maxScrubDepth)
		}

		event.Contexts = contexts
	}
}

// scrubStringMap scrubs keys and values in place. The maps it sees (tags,
// request headers, request env) are event-owned (built fresh by the scope
// merge), so in-place mutation is safe here. Keys are caller-chosen strings
// that can embed secret material, so they are scrubbed too.
func scrubStringMap(scrubber *secretscrub.Scrubber, values map[string]string) {
	updates := make([][2]string, 0, len(values))

	for key, value := range values {
		updates = append(updates, [2]string{scrubber.Scrub(key), scrubber.Scrub(value)})
		delete(values, key)
	}

	for _, pair := range updates {
		values[pair[0]] = pair[1]
	}
}

// copyScrubAnyMap returns an event-owned copy of an arbitrary payload map
// with every key and text value scrubbed, replacing the shared original.
// Subtrees beyond the depth cap collapse to the scrubber's truncation
// marker.
func copyScrubAnyMap(scrubber *secretscrub.Scrubber, values map[string]any, depth int) map[string]any {
	if values == nil {
		return nil
	}

	if depth <= 0 {
		return map[string]any{"omitted": scrubber.TruncatedMarker()}
	}

	out := make(map[string]any, len(values))

	for key, value := range values {
		out[scrubber.Scrub(key)] = copyScrubAny(scrubber, value, depth-1)
	}

	return out
}

// copyScrubAny scrubs one arbitrary payload value into owned storage:
// strings are immutable and scrub directly; maps and slices are copied and
// walked (keys included). Content-free scalars serialize to their exact JSON
// rendering and pass through unchanged. Everything else — custom structs,
// SDK objects, exotic map types — goes through the serialization boundary
// of scrubOpaque, so no value the SDK could render reaches serialization
// uninspected.
func copyScrubAny(scrubber *secretscrub.Scrubber, value any, depth int) any {
	if depth <= 0 {
		return scrubber.TruncatedMarker()
	}

	switch typed := value.(type) {
	case string:
		return scrubber.Scrub(typed)
	case map[string]any:
		return copyScrubAnyMap(scrubber, typed, depth)
	case map[string]string:
		out := make(map[string]string, len(typed))

		for key, elem := range typed {
			out[scrubber.Scrub(key)] = scrubber.Scrub(elem)
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
	case bool, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, float32, float64:
		return value
	default:
		return scrubOpaque(scrubber, value)
	}
}

// scrubOpaque scrubs composite values the typed walk cannot enter: the SDK
// serializes them through encoding/json at send time, so their JSON is
// produced here — the exact bytes the payload would carry — scrubbed, and
// rebuilt into equivalent JSON values. Unserializable values collapse to the
// scrubber's omission marker. encoding/json bounds the walk itself (it
// errors on pointer cycles), so no unbounded reflective traversal is
// introduced.
func scrubOpaque(scrubber *secretscrub.Scrubber, value any) (out any) {
	defer func() {
		if r := recover(); r != nil {
			out = scrubber.UnserializableMarker()
		}
	}()

	encoded, err := json.Marshal(value)
	if err != nil {
		return scrubber.UnserializableMarker()
	}

	text := string(encoded)
	scrubbed := scrubber.Scrub(text)

	if scrubbed == text {
		return value
	}

	var rebuilt any
	if json.Unmarshal([]byte(scrubbed), &rebuilt) != nil {
		return scrubbed
	}

	return rebuilt
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
