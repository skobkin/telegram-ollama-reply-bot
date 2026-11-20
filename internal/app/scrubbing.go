package app

import (
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

// scalarSecretSpec pairs one registered scalar credential with the
// environment variable the config loader reads it from and the accessor for
// the loaded value. One table drives both registries — the runtime one
// reads the loaded config, the bootstrap one (bootstrapEnvSecrets) reads
// the environment directly, before Load can fail — so a new credential
// cannot be added to one and forgotten in the other.
//
// The variable names are configuration metadata, not credentials.
type scalarSecretSpec struct { //nolint:gosec // environment variable names and registry labels, not credentials
	name    string
	envName string
	fromCfg func(*config.Config) string
}

var scalarSecretSpecs = []scalarSecretSpec{
	{name: "telegram-token", envName: "BOT__TELEGRAM__TOKEN", fromCfg: func(cfg *config.Config) string { return cfg.Bot.Telegram.Token }},
	{name: "llm-api-token", envName: "LLM__BACKENDS__OPENAI_COMPAT__API_TOKEN", fromCfg: func(cfg *config.Config) string { return cfg.LLM.Backends.OpenAICompat.APIToken }},
	{name: "llm-image-api-token", envName: "LLM__IMAGE_RECOGNITION__OPENAI_COMPAT__API_TOKEN", fromCfg: func(cfg *config.Config) string { return cfg.LLM.ImageRecognition.OpenAICompat.APIToken }},
	{name: "llm-image-minimax-api-key", envName: "LLM__IMAGE_RECOGNITION__MINIMAX_VLM__API_KEY", fromCfg: func(cfg *config.Config) string { return cfg.LLM.ImageRecognition.MiniMaxVLM.APIKey }},
	{name: "search-tavily-api-key", envName: "PROVIDERS__TAVILY__API_KEY", fromCfg: func(cfg *config.Config) string { return cfg.Providers.Tavily.APIKey }},
	{name: "search-kagi-api-key", envName: "PROVIDERS__KAGI__API_KEY", fromCfg: func(cfg *config.Config) string { return cfg.Providers.Kagi.APIKey }},
	{name: "systemone-api-key", envName: "SYSTEMONE__API_KEY", fromCfg: func(cfg *config.Config) string { return cfg.SystemOne.APIKey }},
	{name: "sentry-dsn", envName: "SENTRY__DSN", fromCfg: func(cfg *config.Config) string { return cfg.Sentry.DSN }},
}

// endpointSecretSpec pairs one credential-bearing endpoint URL with its
// environment variable and loaded accessor, like scalarSecretSpec. Only the
// userinfo password of these URLs registers (see appendURLPassword);
// startup diagnostics and validation errors quote them verbatim.
//
// The variable names are configuration metadata, not credentials.
type endpointSecretSpec struct { //nolint:gosec // environment variable names and registry labels, not credentials
	name    string
	envName string
	fromCfg func(*config.Config) string
}

var endpointSecretSpecs = []endpointSecretSpec{
	{name: "llm-backend-url-password", envName: "LLM__BACKENDS__OPENAI_COMPAT__BASE_URL", fromCfg: func(cfg *config.Config) string { return cfg.LLM.Backends.OpenAICompat.BaseURL }},
	{name: "llm-image-backend-url-password", envName: "LLM__IMAGE_RECOGNITION__OPENAI_COMPAT__BASE_URL", fromCfg: func(cfg *config.Config) string { return cfg.LLM.ImageRecognition.OpenAICompat.BaseURL }},
	{name: "llm-image-minimax-url-password", envName: "LLM__IMAGE_RECOGNITION__MINIMAX_VLM__BASE_URL", fromCfg: func(cfg *config.Config) string { return cfg.LLM.ImageRecognition.MiniMaxVLM.BaseURL }},
	{name: "systemone-endpoint-url-password", envName: "SYSTEMONE__BASE_URL", fromCfg: func(cfg *config.Config) string { return cfg.SystemOne.BaseURL }},
}

// newSecretScrubber registers every secret the app is configured with, so a
// single scrubber can clean Sentry events, log records, and HTTP client
// errors. Every new secret-bearing configuration value (API tokens, keys,
// credential header values, URL-embedded passwords) must be added to the
// spec tables above or the MCP/proxy sections here — a secret missing from
// this registry can surface verbatim in error messages, logs, and Sentry
// issue titles.
func newSecretScrubber(cfg *config.Config) *secretscrub.Scrubber {
	var secrets []secretscrub.Secret

	// Scalar secrets also register their derived renderings — trimmed and
	// quoted — because those are the forms remote surfaces and error
	// diagnostics actually carry.
	for _, spec := range scalarSecretSpecs {
		secret := secretscrub.Secret{Name: spec.name, Value: spec.fromCfg(cfg)}
		secrets = append(secrets, secret)
		secrets = withDerivedForms(secrets, secret)
	}

	// The DSN's userinfo parts are credential components, not a username.
	// Register them individually: net/url re-renders the parsed URL in its
	// errors (dropping the fragment, say), so renderings exist that no
	// full-DSN string matches, while the components match wherever they
	// appear.
	secrets = withURLCredentials(secrets, "sentry-dsn", cfg.Sentry.DSN)

	for _, spec := range endpointSecretSpecs {
		secrets = appendURLPassword(secrets, spec.name, spec.fromCfg(cfg))
	}

	for name, server := range cfg.MCP.Servers {
		for header, value := range server.Headers {
			secrets = appendHeaderCredentialForms(secrets, "mcp-"+name+"-header-"+header, value)
		}

		// Only the password part of an MCP endpoint URL is registered: a
		// generic username would mutilate unrelated text, and net/http
		// already masks passwords inside its own url.Error rendering.
		secrets = appendURLPassword(secrets, "mcp-"+name+"-url-password", server.URL)
	}

	// Outbound proxies follow the standard environment variables, resolved
	// with the same httpproxy.FromEnvironment semantics the HTTP clients
	// use (which covers the lowercase variable variants and the scheme-less
	// address forms parseProxy accepts). Only the userinfo password is
	// registered — a generic username would mutilate unrelated text —
	// because proxy dial errors and network diagnostics can carry the proxy
	// address. NO_PROXY is a host list without credentials.
	proxyConfig := httpproxy.FromEnvironment()
	for name, proxy := range map[string]string{
		"http-proxy-url-password":  proxyConfig.HTTPProxy,
		"https-proxy-url-password": proxyConfig.HTTPSProxy,
	} {
		secrets = appendProxyPassword(secrets, name, proxy)
	}

	return secretscrub.New(secrets)
}

// bootstrapEnvSecrets builds the pre-Load registry subset straight from the
// process environment. A failed config.Load returns before newSecretScrubber
// can run, and its error reaches main's plain stderr, so bootstrap failures
// are scrubbed with the credential values Load would have registered: the
// same spec tables drive both registries, the environment is matched the
// way the loader matches it (variable names lowercased before mapping), and
// the MCP endpoint-URL and request-header variable families are picked up
// with the same shapes and derived names the loader produces.
func bootstrapEnvSecrets() []secretscrub.Secret {
	env := bootstrapEnvValues()

	var secrets []secretscrub.Secret

	for _, spec := range scalarSecretSpecs {
		value := env[strings.ToLower(spec.envName)]
		if strings.TrimSpace(value) == "" {
			continue
		}

		secret := secretscrub.Secret{Name: spec.name, Value: value}
		secrets = append(secrets, secret)
		secrets = withDerivedForms(secrets, secret)

		if spec.name == "sentry-dsn" {
			secrets = withURLCredentials(secrets, spec.name, value)
		}
	}

	for _, spec := range endpointSecretSpecs {
		if value := env[strings.ToLower(spec.envName)]; strings.TrimSpace(value) != "" {
			secrets = appendURLPassword(secrets, spec.name, value)
		}
	}

	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		if strings.TrimSpace(value) == "" {
			continue
		}

		switch {
		case isMCPHeaderEnvVar(name):
			server, header, ok := mcpHeaderEnvNames(name)
			if !ok {
				continue
			}

			secrets = appendHeaderCredentialForms(secrets, "mcp-"+server+"-header-"+header, value)
		case isMCPServerURLEnvVar(name):
			secrets = appendURLPassword(secrets, "mcp-"+mcpServerEnvName(name)+"-url-password", value)
		}
	}

	proxyConfig := httpproxy.FromEnvironment()
	for name, proxy := range map[string]string{
		"http-proxy-url-password":  proxyConfig.HTTPProxy,
		"https-proxy-url-password": proxyConfig.HTTPSProxy,
	} {
		secrets = appendProxyPassword(secrets, name, proxy)
	}

	return secrets
}

// bootstrapEnvValues flattens the environment keyed by lowercased variable
// name. The loader lowercases every variable name before mapping it onto
// the config tree (config.Load's env transform), so a credential set in any
// letter case must be found here the same way; an exact-case LookupEnv
// would silently miss the value. Blank values are kept — the lookup sites
// apply the loader's blank-means-unset rule themselves. The first
// occurrence of a name wins.
func bootstrapEnvValues() map[string]string {
	env := make(map[string]string)

	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		key := strings.ToLower(name)
		if _, seen := env[key]; !seen {
			env[key] = value
		}
	}

	return env
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

// isMCPServerURLEnvVar reports whether an environment variable name is an
// MCP endpoint URL: MCP__SERVERS__<SERVER>__URL, matched case-insensitively
// like every variable name the loader maps onto the tree.
func isMCPServerURLEnvVar(key string) bool {
	parts := strings.Split(key, "__")

	return len(parts) == 4 &&
		strings.EqualFold(parts[0], "MCP") &&
		strings.EqualFold(parts[1], "SERVERS") &&
		strings.EqualFold(parts[3], "URL")
}

// mcpServerEnvName lowercases the server segment of an
// MCP__SERVERS__<SERVER>__* variable, producing the server name the loader
// derives for the config map key — and the registry label mirrors.
func mcpServerEnvName(key string) string {
	parts := strings.Split(key, "__")

	return strings.ToLower(parts[2])
}

// mcpHeaderEnvNames derives the server and header name parts of an
// MCP__SERVERS__<SERVER>__HEADERS__<SEG>[__SEG...] variable exactly the way
// the loader does (config.applyMCPHeaderEnv): the header name is the
// remaining segments joined with "-" and lowercased, so the bootstrap
// registry labels match the post-Load registry ones.
func mcpHeaderEnvNames(key string) (server, header string, ok bool) {
	parts := strings.Split(key, "__")
	if len(parts) < 5 {
		return "", "", false
	}

	return strings.ToLower(parts[2]), strings.ToLower(strings.Join(parts[4:], "-")), true
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

// appendHeaderCredentialForms registers every form an MCP header value can
// take on a remote surface, each with its %q rendering alongside:
//
//   - the raw configured value;
//   - the trimmed form: HTTP header transmission strips surrounding
//     whitespace, so providers receive and echo the trimmed form (this also
//     covers bare scheme-less values, which credentialFromHeader does not
//     normalize);
//   - the bare credential of scheme-prefixed values ("Bearer X", "Token X",
//     …): providers echo the bare credential in errors ("invalid api key
//     X"), not the full header value.
//
// The same helper serves both registries, so the two header paths cannot
// drift.
func appendHeaderCredentialForms(secrets []secretscrub.Secret, name, value string) []secretscrub.Secret {
	forms := []secretscrub.Secret{{Name: name, Value: value}}

	if trimmed := strings.TrimSpace(value); trimmed != "" && trimmed != value {
		forms = append(forms, secretscrub.Secret{Name: name + "-trimmed", Value: trimmed})
	}

	if credential, ok := credentialFromHeader(value); ok {
		forms = append(forms, secretscrub.Secret{Name: name + "-credential", Value: credential})
	}

	for _, form := range forms {
		secrets = append(secrets, form)
		secrets = withQuoted(secrets, form.Name, form.Value)
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

// appendProxyPassword registers the userinfo password of a proxy address.
// The address is normalized exactly the way x/net/http/httpproxy's
// parseProxy accepts one: a value that does not parse to a scheme and host
// is retried with an "http://" prefix, so the scheme-less authenticated
// form ("user:pass@host:8080") registers too. The normalization is
// best-effort — parseProxy's failure message quotes a rejected address
// verbatim, credentials included, so extraction is attempted regardless.
// The same helper serves both registries: the post-Load one and the
// bootstrap one.
func appendProxyPassword(secrets []secretscrub.Secret, name, address string) []secretscrub.Secret {
	if parsed, err := url.Parse(address); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return appendURLPassword(secrets, name, address)
	}

	return appendURLPassword(secrets, name, "http://"+address)
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

// Arbitrary payload maps (mechanism data, breadcrumb data, extra, contexts)
// are scrubbed through the shared serialization boundary of
// secretscrub.Scrubber.ScrubMap, which copies them into event-owned storage
// with depth-capped, fail-closed traversal.

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
			mechanism.Data = scrubber.ScrubMap(mechanism.Data)
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
		owned.Data = scrubber.ScrubMap(crumb.Data)
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
	event.Extra = scrubber.ScrubMap(event.Extra)

	// The contexts map is rebuilt so context names — caller-chosen strings
	// that could embed secret material — are scrubbed alongside their data.
	if len(event.Contexts) > 0 {
		contexts := make(map[string]sentry.Context, len(event.Contexts))

		for name, contextData := range event.Contexts {
			contexts[scrubber.Scrub(name)] = scrubber.ScrubMap(contextData)
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

// scrubFatalError scrubs the error returned by Run, which main prints to
// plain stderr — the one diagnostic surface no other scrubbing layer covers.
// A nil scrubber or nil error passes through unchanged.
func scrubFatalError(scrubber *secretscrub.Scrubber, err error) error {
	if scrubber == nil || err == nil {
		return err
	}

	return errors.New(scrubber.Scrub(err.Error()))
}
