package app

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"telegram-ollama-reply-bot/internal/config"

	"github.com/getsentry/sentry-go"
)

// scrubTestConfig returns a config whose every secret field carries a fake
// fixture. The values are deliberately not credential-shaped to stay out of
// secret-scanning.
func scrubTestConfig() *config.Config {
	cfg := &config.Config{}

	cfg.Bot.Telegram.Token = "TELEGRAMFIXTURE"
	cfg.LLM.Backends.OpenAICompat.APIToken = "LLMFIXTURE"
	cfg.LLM.ImageRecognition.OpenAICompat.APIToken = "IMAGELLMFIXTURE"
	cfg.LLM.ImageRecognition.MiniMaxVLM.APIKey = "MINIMAXFIXTURE"
	cfg.Providers.Tavily.APIKey = "TAVILYFIXTURE"
	cfg.Providers.Kagi.APIKey = "KAGIFIXTURE"
	cfg.SystemOne.APIKey = "SYSTEMONEFIXTURE"
	cfg.Sentry.DSN = "SENTRYDSNFIXTURE"

	cfg.MCP.Servers = map[string]config.MCPServerConfig{
		"docs": { //nolint:gosec // fake fixture, not a credential
			URL:     "https://user:DOCSURLPASSWORD@mcp.example.test/sse",
			Headers: map[string]string{"Authorization": "DOCSHEADERFIXTURE"},
		},
		"plain": {
			URL: "https://mcp2.example.test/sse",
		},
	}

	return cfg
}

func TestNewSecretScrubberRegistersAllSecrets(t *testing.T) {
	t.Parallel()

	scrubber := newSecretScrubber(scrubTestConfig())

	for _, fixture := range []string{
		"TELEGRAMFIXTURE",
		"LLMFIXTURE",
		"IMAGELLMFIXTURE",
		"MINIMAXFIXTURE",
		"TAVILYFIXTURE",
		"KAGIFIXTURE",
		"SYSTEMONEFIXTURE",
		"SENTRYDSNFIXTURE",
		"DOCSURLPASSWORD",
		"DOCSHEADERFIXTURE",
	} {
		text := "payload with " + fixture + " inside"

		if !scrubber.Contains(text) {
			t.Fatalf("registered secret missing from scrubber: %s", fixture)
		}

		if got := scrubber.Scrub(text); got == text {
			t.Fatalf("registered secret was not scrubbed: %s", fixture)
		}
	}
}

func TestNewSecretScrubberEmptyConfigNoPanic(t *testing.T) {
	t.Parallel()

	scrubber := newSecretScrubber(&config.Config{})

	if got := scrubber.Scrub("anything"); got != "anything" {
		t.Fatalf("zero-value config produced a working scrubber: %q", got)
	}
}

func TestNewSecretScrubberIgnoresUsername(t *testing.T) {
	t.Parallel()

	scrubber := newSecretScrubber(scrubTestConfig())

	// The URL username is deliberately not registered: scrubbing a generic
	// username would mutilate unrelated text.
	if got := scrubber.Scrub("contact user at user@host"); got != "contact user at user@host" {
		t.Fatalf("username was scrubbed: %q", got)
	}
}

func TestSentryBeforeSendScrubsTelegoShapedException(t *testing.T) {
	t.Parallel()

	scrubber := newSecretScrubber(scrubTestConfig())
	hook := newSentryBeforeSend(scrubber)

	// The exact shape telego produces when a Telegram API call fails:
	// the token-bearing URL inside the exception value.
	event := &sentry.Event{
		Exception: []sentry.Exception{
			{
				Type:  "*url.Error",
				Value: `Post "https://api.telegram.org/botTELEGRAMFIXTURE/sendMessage": dial tcp: connection refused`,
			},
		},
	}

	got := hook(event, nil)
	if got != event {
		t.Fatal("hook dropped or replaced the event")
	}

	if got.Exception[0].Type != "*url.Error" {
		t.Fatalf("clean exception type was modified: %q", got.Exception[0].Type)
	}

	if containsAny(got.Exception[0].Value, "TELEGRAMFIXTURE") {
		t.Fatalf("token survived in exception value: %q", got.Exception[0].Value)
	}

	if !containsAny(got.Exception[0].Value, "[REDACTED:telegram-token]") {
		t.Fatalf("scrub marker missing from exception value: %q", got.Exception[0].Value)
	}
}

func TestSentryBeforeSendScrubsAllStringSurfaces(t *testing.T) {
	t.Parallel()

	scrubber := newSecretScrubber(scrubTestConfig())
	hook := newSentryBeforeSend(scrubber)

	event := &sentry.Event{
		Message:     "failure LLMFIXTURE",
		Transaction: "task LLMFIXTURE",
		Fingerprint: []string{"fp", "LLMFIXTURE"},
		Tags:        map[string]string{"route": "LLMFIXTURE"},
		Extra: map[string]any{
			"detail": "LLMFIXTURE",
			"nested": map[string]any{"deep": []any{"LLMFIXTURE", 42}},
		},
		Contexts: map[string]sentry.Context{
			"runtime": {"version": "LLMFIXTURE"},
		},
		Breadcrumbs: []*sentry.Breadcrumb{
			{
				Message: "fetch LLMFIXTURE",
				Data:    map[string]any{"url": "https://host/botLLMFIXTURE/getMe"},
			},
		},
		Request: &sentry.Request{
			URL:         "https://host/botLLMFIXTURE/getMe",
			QueryString: "key=LLMFIXTURE",
			Cookies:     "sid=LLMFIXTURE",
			Data:        "body LLMFIXTURE",
			Headers:     map[string]string{"Authorization": "Bearer LLMFIXTURE"},
			Env:         map[string]string{"GATEWAY": "https://u:LLMFIXTURE@host"}, //nolint:gosec // fake fixture, not a credential
		},
		User: sentry.User{
			ID:       "u-LLMFIXTURE",
			Username: "llm-fixture",
			Data:     map[string]string{"token": "LLMFIXTURE"},
		},
		Threads: []sentry.Thread{
			{Name: "worker LLMFIXTURE"},
		},
		Exception: []sentry.Exception{
			{
				Type:  "*url.Error",
				Value: "Post https://host/botLLMFIXTURE/getMe: refused",
				Mechanism: &sentry.Mechanism{
					Description: "retried LLMFIXTURE",
					Data:        map[string]any{"attempt": "LLMFIXTURE"},
				},
			},
		},
	}

	hook(event, nil)

	// The event must never be dropped, and no surface may carry the fixture.
	for _, failure := range collectLeakFailures(event) {
		t.Error(failure)
	}

	if event.Extra["detail"] != "[REDACTED:llm-api-token]" {
		t.Fatalf("Extra value not scrubbed: %v", event.Extra["detail"])
	}

	if event.Request.Headers["Authorization"] != "Bearer [REDACTED:llm-api-token]" {
		t.Fatalf("request header not scrubbed: %q", event.Request.Headers["Authorization"])
	}

	if event.User.ID != "u-[REDACTED:llm-api-token]" {
		t.Fatalf("user ID not scrubbed: %q", event.User.ID)
	}

	// Numbers survive nested walks untouched.
	nested := event.Extra["nested"].(map[string]any)["deep"].([]any)
	if nested[1] != 42 {
		t.Fatalf("non-string nested value was altered: %v", nested[1])
	}
}

// collectLeakFailures walks every string surface of the event and reports
// those still carrying the fixture.
func collectLeakFailures(event *sentry.Event) []string {
	var failures []string

	check := func(surface string, value string) {
		if containsAny(value, "LLMFIXTURE") {
			failures = append(failures, surface+": "+value)
		}
	}

	check("message", event.Message)
	check("transaction", event.Transaction)

	for _, fp := range event.Fingerprint {
		check("fingerprint", fp)
	}

	for key, tag := range event.Tags {
		check("tag "+key, tag)
	}

	for _, failure := range collectMapLeakFailures(event.Extra) {
		failures = append(failures, "extra: "+failure)
	}

	for name, contextData := range event.Contexts {
		for key, value := range contextData {
			if s, ok := value.(string); ok && containsAny(s, "LLMFIXTURE") {
				failures = append(failures, "context "+name+"."+key+": "+s)
			}
		}
	}

	for _, crumb := range event.Breadcrumbs {
		check("breadcrumb message", crumb.Message)

		for _, failure := range collectMapLeakFailures(crumb.Data) {
			failures = append(failures, "breadcrumb data: "+failure)
		}
	}

	if event.Request != nil {
		check("request url", event.Request.URL)
		check("request query", event.Request.QueryString)
		check("request cookies", event.Request.Cookies)
		check("request data", event.Request.Data)

		for key, header := range event.Request.Headers {
			check("request header "+key, header)
		}

		for key, env := range event.Request.Env {
			check("request env "+key, env)
		}
	}

	check("user id", event.User.ID)
	check("user email", event.User.Email)
	check("user ip", event.User.IPAddress)
	check("user username", event.User.Username)
	check("user name", event.User.Name)

	for key, data := range event.User.Data {
		check("user data "+key, data)
	}

	for _, thread := range event.Threads {
		check("thread name", thread.Name)
	}

	for _, exc := range event.Exception {
		check("exception type", exc.Type)
		check("exception value", exc.Value)

		if exc.Mechanism != nil {
			check("mechanism description", exc.Mechanism.Description)

			for _, failure := range collectMapLeakFailures(exc.Mechanism.Data) {
				failures = append(failures, "mechanism data: "+failure)
			}
		}
	}

	return failures
}

func collectMapLeakFailures(values map[string]any) []string {
	var failures []string

	for key, value := range values {
		switch typed := value.(type) {
		case string:
			if containsAny(typed, "LLMFIXTURE") {
				failures = append(failures, key+": "+typed)
			}
		case map[string]any:
			for _, failure := range collectMapLeakFailures(typed) {
				failures = append(failures, key+"."+failure)
			}
		case []any:
			for i, elem := range typed {
				if s, ok := elem.(string); ok && containsAny(s, "LLMFIXTURE") {
					failures = append(failures, key+"["+string(rune('0'+i))+"]: "+s)
				}
			}
		}
	}

	return failures
}

func TestSentryBeforeSendPreservesSDKFields(t *testing.T) {
	t.Parallel()

	scrubber := newSecretScrubber(scrubTestConfig())
	hook := newSentryBeforeSend(scrubber)

	event := &sentry.Event{
		EventID:     "abc123",
		Release:     "v1.2.3",
		Environment: "production",
		ServerName:  "bot-host",
		Platform:    "go",
		Level:       sentry.LevelError,
	}

	event.Message = "clean message"

	hook(event, nil)

	if event.EventID != "abc123" || event.Release != "v1.2.3" || event.Environment != "production" ||
		event.ServerName != "bot-host" || event.Platform != "go" || event.Level != sentry.LevelError {
		t.Fatalf("SDK-controlled fields were modified: %+v", event)
	}

	if event.Message != "clean message" {
		t.Fatalf("clean message was modified: %q", event.Message)
	}
}

func TestSentryBeforeSendDeepNestingAndCyclesNoPanic(t *testing.T) {
	t.Parallel()

	scrubber := newSecretScrubber(scrubTestConfig())
	hook := newSentryBeforeSend(scrubber)

	// A map that contains itself must terminate via the depth cap.
	cyclic := map[string]any{}
	cyclic["self"] = cyclic

	// Content beyond the depth cap is left untouched rather than panicking.
	deep := map[string]any{}
	cursor := deep
	for i := 0; i < 20; i++ {
		next := map[string]any{}
		cursor["next"] = next
		cursor = next
	}
	cursor["payload"] = "LLMFIXTURE"

	event := &sentry.Event{
		Extra: map[string]any{
			"cyclic": cyclic,
			"deep":   deep,
		},
	}

	hook(event, nil) // must not panic or hang

	// Content beyond the depth cap stays untouched (unscrubbed) rather than
	// being lost; the walk gives up before reaching it.
	cursorNext, ok := event.Extra["deep"].(map[string]any)
	if !ok {
		t.Fatal("deep extra map was replaced")
	}

	for i := 0; i < 20; i++ {
		next, ok := cursorNext["next"].(map[string]any)
		if !ok {
			t.Fatalf("deep chain broken at depth %d", i)

			return
		}

		cursorNext = next
	}

	if got := cursorNext["payload"]; got != "LLMFIXTURE" {
		t.Fatalf("content past the depth cap was altered: %v", got)
	}
}

func TestSentryBeforeSendIdempotent(t *testing.T) {
	t.Parallel()

	scrubber := newSecretScrubber(scrubTestConfig())
	hook := newSentryBeforeSend(scrubber)

	event := &sentry.Event{
		Message: "Post https://host/botLLMFIXTURE/getMe failed",
	}

	hook(event, nil)
	first := event.Message

	hook(event, nil)
	if event.Message != first {
		t.Fatalf("scrubbing is not idempotent: %q -> %q", first, event.Message)
	}
}

func TestSentryBeforeSendNilEvent(t *testing.T) {
	t.Parallel()

	hook := newSentryBeforeSend(newSecretScrubber(scrubTestConfig()))

	if got := hook(nil, nil); got != nil {
		t.Fatal("nil event must stay nil")
	}
}

func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if needle != "" && strings.Contains(text, needle) {
			return true
		}
	}

	return false
}

func TestNewSecretScrubberRegistersHeaderCredential(t *testing.T) {
	t.Parallel()

	cfg := scrubTestConfig()
	cfg.MCP.Servers = map[string]config.MCPServerConfig{
		"docs": {
			URL:     "https://mcp.example.test/sse",
			Headers: map[string]string{"Authorization": "Bearer BEARERCREDFIXTURE"}, //nolint:gosec // fake fixture, not a credential
		},
	}

	scrubber := newSecretScrubber(cfg)

	// The full header value and the bare credential both scrub.
	for _, fixture := range []string{"Bearer BEARERCREDFIXTURE", "invalid api key BEARERCREDFIXTURE"} {
		text := "provider said: " + fixture

		if got := scrubber.Scrub(text); got == text {
			t.Fatalf("credential literal survived: %q", text)
		}
	}
}

func TestNewSecretScrubberRegistersTrimmedHeaderValue(t *testing.T) {
	t.Parallel()

	cfg := scrubTestConfig()
	cfg.MCP.Servers = map[string]config.MCPServerConfig{
		"docs": { //nolint:gosec // fake fixture, not a credential
			URL: "https://mcp.example.test/sse",
			Headers: map[string]string{
				"X-API-Key": "  BAREKEYFIXTURE  ", //nolint:gosec // fake fixture, not a credential
			},
		},
	}

	scrubber := newSecretScrubber(cfg)

	// The padded configured value, the trimmed form providers actually see
	// over HTTP, and the bare key in an error echo all scrub.
	for _, fixture := range []string{
		"  BAREKEYFIXTURE  ",
		"BAREKEYFIXTURE",
		"invalid key BAREKEYFIXTURE",
	} {
		text := "upstream said: " + fixture

		if got := scrubber.Scrub(text); got == text {
			t.Fatalf("header form survived: %q", text)
		}
	}
}

func TestCredentialFromHeader(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value string
		want  string
		ok    bool
	}{
		{"bearer", "Bearer SECRET1", "SECRET1", true},
		{"bearer lowercase", "bearer SECRET1", "SECRET1", true},
		{"basic", "Basic dXNlcjpwYXNz", "dXNlcjpwYXNz", true},
		{"bot", "Bot SECRET2", "SECRET2", true},
		{"token", "token  SECRET3 ", "SECRET3", true},
		{"padded", " Bearer SECRET4 ", "SECRET4", true},
		{"unrecognized scheme", "Digest abc", "", false},
		{"no credential", "Bearer ", "", false},
		{"no space", "rawvalue", "", false},
		{"empty", "", "", false},
	}

	for _, testCase := range cases {
		got, ok := credentialFromHeader(testCase.value)
		if ok != testCase.ok || got != testCase.want {
			t.Fatalf("%s: got (%q, %v), want (%q, %v)", testCase.name, got, ok, testCase.want, testCase.ok)
		}
	}
}

func TestNewSecretScrubberRegistersTrimmedTokenForm(t *testing.T) {
	t.Parallel()

	cfg := scrubTestConfig()
	cfg.LLM.Backends.OpenAICompat.APIToken = "  PADDEDTOKENFIXTURE  "

	scrubber := newSecretScrubber(cfg)

	// Header transmission strips surrounding whitespace, so the backend
	// receives and echoes the trimmed token, not the padded configured one.
	for _, form := range []string{"PADDEDTOKENFIXTURE", "  PADDEDTOKENFIXTURE  "} {
		text := "backend echoed " + form

		if got := scrubber.Scrub(text); got == text {
			t.Fatalf("token form survived: %q", text)
		}
	}
}

func TestNewSecretScrubberRegistersQuotedDSNForm(t *testing.T) {
	t.Parallel()

	cfg := scrubTestConfig()
	cfg.Sentry.DSN = "SENTRYDSNFIXTURE\n" //nolint:gosec // fake fixture, not a credential

	scrubber := newSecretScrubber(cfg)

	// Sentry's initialization error renders the DSN through %q, which
	// escapes the trailing newline; that rendering must match too.
	rendered := strconv.Quote(cfg.Sentry.DSN)

	if got := scrubber.Scrub(rendered); strings.Contains(got, "SENTRYDSNFIXTURE") {
		t.Fatalf("quoted DSN rendering survived: %q", got)
	}
}

func TestNewSecretScrubberRegistersQuotedTrimmedForm(t *testing.T) {
	t.Parallel()

	cfg := scrubTestConfig()
	cfg.LLM.Backends.OpenAICompat.APIToken = " PADDED\"TOKENFIXTURE "

	scrubber := newSecretScrubber(cfg)

	// A provider that trims the key and then renders it through %q escapes
	// the quote; that rendering matches neither the padded raw registration
	// nor its own quoted form — only the quoted trimmed form.
	rendered := strconv.Quote(`PADDED"TOKENFIXTURE`)

	if got := scrubber.Scrub(rendered); strings.Contains(got, "PADDEDTOKENFIXTURE") {
		t.Fatalf("quoted trimmed token rendering survived: %q", got)
	}
}

func TestNewSecretScrubberRegistersQuotedHeaderForms(t *testing.T) {
	t.Parallel()

	cfg := scrubTestConfig()
	cfg.MCP.Servers = map[string]config.MCPServerConfig{
		"docs": { //nolint:gosec // fake fixture, not a credential
			URL: "https://mcp.example.test/sse",
			Headers: map[string]string{
				"Authorization": `Bearer QUOTED"HEADERFIXTURE`, //nolint:gosec // fake fixture, not a credential
			},
		},
	}

	scrubber := newSecretScrubber(cfg)

	// A diagnostic rendering the bare credential through %q escapes the
	// quote; the quoted form of the credential must match.
	if got := scrubber.Scrub(strconv.Quote(`QUOTED"HEADERFIXTURE`)); strings.Contains(got, "QUOTED\"HEADERFIXTURE") {
		t.Fatalf("quoted credential rendering survived: %q", got)
	}

	// So must the quoted form of the full header value.
	if got := scrubber.Scrub(strconv.Quote(`Bearer QUOTED"HEADERFIXTURE`)); strings.Contains(got, "QUOTED\"HEADERFIXTURE") {
		t.Fatalf("quoted full header rendering survived: %q", got)
	}
}

func TestNewSecretScrubberRegistersDSNComponents(t *testing.T) {
	t.Parallel()

	cfg := scrubTestConfig()
	cfg.Sentry.DSN = "https://DSNKEYFIXTURE:DSNSECRETSELFIXTURE@o.example.test/123" //nolint:gosec // fake fixture, not a credential

	scrubber := newSecretScrubber(cfg)

	// Both credential components of the DSN match on their own.
	for _, component := range []string{"DSNKEYFIXTURE", "DSNSECRETSELFIXTURE"} {
		text := "credential echo: " + component

		if got := scrubber.Scrub(text); got == text {
			t.Fatalf("DSN component survived: %q", component)
		}
	}

	// net/url re-renders the parsed URL in its errors, dropping the
	// fragment, so a rendering can omit DSN parts no full-DSN string
	// matches; the component still matches there.
	rendered := `invalid url: parse "https://DSNKEYFIXTURE@host/%zz": invalid URL escape "%zz"`
	if got := scrubber.Scrub(rendered); strings.Contains(got, "DSNKEYFIXTURE") {
		t.Fatalf("DSN public key survived in a partial rendering: %q", got)
	}

	// A quote-bearing key registers its quoted rendering too: the parse
	// error escapes the quote while re-rendering the URL.
	cfg.Sentry.DSN = `https://PUBLIC"KEY_FIXTURE@host/%zz#fragment` //nolint:gosec // fake fixture, not a credential
	scrubber = newSecretScrubber(cfg)

	rendered = `parse "https://PUBLIC\"KEY_FIXTURE@host/%zz": invalid URL escape "%zz"`
	if got := scrubber.Scrub(rendered); strings.Contains(got, "KEY_FIXTURE") {
		t.Fatalf("quoted DSN key rendering survived: %q", got)
	}
}

func TestNewSecretScrubberRegistersQuotedDecodedURLPassword(t *testing.T) {
	t.Parallel()

	cfg := scrubTestConfig()
	cfg.LLM.Backends.OpenAICompat.BaseURL = "https://ops:PASSWORD%22TAIL@llm.internal/v1"

	scrubber := newSecretScrubber(cfg)

	// The decoded password carries a real quote; a diagnostic quoting the
	// decoded form through %q must match the quoted decoded registration,
	// not just the raw one (which here contains no escapable characters).
	rendered := strconv.Quote(`PASSWORD"TAIL`)

	if got := scrubber.Scrub(rendered); strings.Contains(got, `PASSWORD\"TAIL`) {
		t.Fatalf("quoted decoded password rendering survived: %q", got)
	}
}

func TestNewSecretScrubberRegistersEndpointURLPasswords(t *testing.T) {
	t.Parallel()

	cfg := scrubTestConfig()
	cfg.LLM.Backends.OpenAICompat.BaseURL = "https://ops:LLMURLPASSFIXTURE@llm.internal/v1"
	cfg.SystemOne.BaseURL = "https://ops:S1URLPASSFIXTURE@decision.internal/v1"

	scrubber := newSecretScrubber(cfg)

	for _, fixture := range []string{"LLMURLPASSFIXTURE", "S1URLPASSFIXTURE"} {
		text := "dial https://ops:" + fixture + "@host failed"

		if got := scrubber.Scrub(text); got == text {
			t.Fatalf("endpoint URL password survived: %q", fixture)
		}
	}
}

func TestNewSecretScrubberRegistersMalformedEndpointURLPassword(t *testing.T) {
	t.Parallel()

	cfg := scrubTestConfig()

	// The invalid escape in the path makes url.Parse fail, but the credential
	// in the userinfo must still be registered: startup diagnostics quote the
	// raw URL verbatim.
	cfg.LLM.Backends.OpenAICompat.BaseURL = "https://ops:MALFORMEDURLPWFIXTURE@llm.internal/%zz"

	scrubber := newSecretScrubber(cfg)

	text := `parse "https://ops:MALFORMEDURLPWFIXTURE@llm.internal/%zz": invalid URL escape "%zz"` //nolint:gosec // fake fixture, not a credential
	if got := scrubber.Scrub(text); got == text {
		t.Fatalf("malformed-endpoint URL password survived: %q", got)
	}
}

func TestNewSecretScrubberRegistersEncodedURLPassword(t *testing.T) {
	t.Parallel()

	cfg := scrubTestConfig()
	cfg.SystemOne.BaseURL = "https://ops:ENCODEDURLPW%40SSFIXTURE@decision.internal/v1"

	scrubber := newSecretScrubber(cfg)

	// The percent-encoded form appears inside URLs; the decoded form is what
	// a server echoes back after unescaping its credentials. Both register.
	for _, fixture := range []string{"ENCODEDURLPW%40SSFIXTURE", "ENCODEDURLPW@SSFIXTURE"} {
		text := "dial failed with " + fixture

		if got := scrubber.Scrub(text); got == text {
			t.Fatalf("encoded URL password form survived: %q", fixture)
		}
	}
}

func TestURLUserinfoPrefix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		rawURL string
		want   string
		ok     bool
	}{
		{"user and password", "https://ops:pw@host/path", "ops:pw", true},
		{"user only", "https://ops@host", "ops", true},
		{"stops at path", "https://ops:pw@host/deep/path?q=1#f", "ops:pw", true},
		{"stops at query", "https://ops:pw@host?q=1", "ops:pw", true},
		{"stops at fragment", "https://ops:pw@host#frag", "ops:pw", true},
		{"malformed remainder", "https://ops:pw@host/%zz", "ops:pw", true},
		{"no userinfo", "https://host/path", "", false},
		{"port only", "https://host:8080/x", "", false},
		{"no scheme", "host/path", "", false},
		{"empty userinfo", "https://@host", "", false},
	}

	for _, testCase := range cases {
		got, ok := urlUserinfoPrefix(testCase.rawURL)
		if ok != testCase.ok || got != testCase.want {
			t.Fatalf("%s: got (%q, %v), want (%q, %v)", testCase.name, got, ok, testCase.want, testCase.ok)
		}
	}
}

func TestNewSecretScrubberRegistersQuotedURLPassword(t *testing.T) {
	t.Parallel()

	cfg := scrubTestConfig()
	cfg.LLM.Backends.OpenAICompat.BaseURL = `https://ops:QUOTEPW"SSFIXTURE@llm.internal/%zz`

	scrubber := newSecretScrubber(cfg)

	// The credential carries a character that %q — the rendering net/url uses
	// for its own parse errors — escapes, so the escaped form must be
	// registered too.
	_, parseErr := url.Parse(cfg.LLM.Backends.OpenAICompat.BaseURL)
	if parseErr == nil {
		t.Fatal("malformed fixture URL parsed cleanly")
	}

	diagnostic := parseErr.Error()
	if !strings.Contains(diagnostic, `QUOTEPW\"SSFIXTURE`) {
		t.Fatalf("expected the quoted rendering in the diagnostic: %q", diagnostic)
	}

	if got := scrubber.Scrub(diagnostic); strings.Contains(got, "QUOTEPW") {
		t.Fatalf("quoted password rendering survived: %q", got)
	}
}

func TestScrubFatalError(t *testing.T) {
	t.Parallel()

	scrubber := newSecretScrubber(scrubTestConfig())

	// A fatal error carrying a registered secret is scrubbed; the message is
	// what main prints, so the marker must be present.
	err := scrubFatalError(scrubber, errors.New("connect https://host/botTELEGRAMFIXTURE/getMe failed"))
	if containsAny(err.Error(), "TELEGRAMFIXTURE") {
		t.Fatalf("fatal error kept the secret: %v", err)
	}

	if !containsAny(err.Error(), "[REDACTED:telegram-token]") {
		t.Fatalf("fatal error lost the marker: %v", err)
	}

	// Nil error and nil scrubber pass through.
	if scrubFatalError(scrubber, nil) != nil {
		t.Fatal("nil fatal error was replaced")
	}

	clean := errors.New("plain failure")
	if scrubFatalError(nil, clean) != clean { //nolint:errorlint // identity comparison is the assertion
		t.Fatal("nil scrubber rebuilt the error")
	}
}

func TestSentryBeforeSendDoesNotMutateSharedBreadcrumb(t *testing.T) {
	t.Parallel()

	scrubber := newSecretScrubber(scrubTestConfig())
	hook := newSentryBeforeSend(scrubber)

	// Scope breadcrumbs are shared by pointer between concurrent events;
	// scrubbing two events at once must leave the original untouched.
	crumb := &sentry.Breadcrumb{
		Message: "fetch https://host/botLLMFIXTURE/getMe",
		Data:    map[string]any{"url": "https://host/botLLMFIXTURE/getMe"},
	}

	events := []*sentry.Event{
		{Breadcrumbs: []*sentry.Breadcrumb{crumb}},
		{Breadcrumbs: []*sentry.Breadcrumb{crumb}},
		{Breadcrumbs: []*sentry.Breadcrumb{crumb}},
		{Breadcrumbs: []*sentry.Breadcrumb{crumb}},
	}

	var group sync.WaitGroup

	for _, event := range events {
		group.Add(1)

		go func(ev *sentry.Event) {
			defer group.Done()
			hook(ev, nil)
		}(event)
	}

	group.Wait()

	if crumb.Message != "fetch https://host/botLLMFIXTURE/getMe" {
		t.Fatalf("shared breadcrumb was mutated: %q", crumb.Message)
	}

	if crumb.Data["url"] != "https://host/botLLMFIXTURE/getMe" {
		t.Fatalf("shared breadcrumb data was mutated: %v", crumb.Data["url"])
	}

	for _, event := range events {
		if !containsAny(event.Breadcrumbs[0].Message, "[REDACTED:llm-api-token]") {
			t.Fatalf("event-owned breadcrumb was not scrubbed: %q", event.Breadcrumbs[0].Message)
		}

		if !containsAny(event.Breadcrumbs[0].Data["url"].(string), "[REDACTED:llm-api-token]") {
			t.Fatalf("event-owned breadcrumb data was not scrubbed: %v", event.Breadcrumbs[0].Data["url"])
		}
	}
}

func TestSentryBeforeSendDoesNotMutateSharedUserData(t *testing.T) {
	t.Parallel()

	scrubber := newSecretScrubber(scrubTestConfig())
	hook := newSentryBeforeSend(scrubber)

	// The scope user is copied into events shallowly, so its Data map is
	// shared by every capture; scrubbing must replace it on the event, not
	// rewrite the scope-owned map in place.
	shared := map[string]string{"token": "LLMFIXTURE"}

	events := make([]*sentry.Event, 4)
	for i := range events {
		events[i] = &sentry.Event{User: sentry.User{ID: "u-1", Data: shared}}
	}

	var group sync.WaitGroup

	for _, event := range events {
		group.Add(1)

		go func(ev *sentry.Event) {
			defer group.Done()
			hook(ev, nil)
		}(event)
	}

	group.Wait()

	if shared["token"] != "LLMFIXTURE" {
		t.Fatalf("scope-owned user data was mutated: %v", shared["token"])
	}

	for i, event := range events {
		if got := event.User.Data["token"]; got != "[REDACTED:llm-api-token]" {
			t.Fatalf("event %d: user data not scrubbed: %q", i, got)
		}
	}
}
