package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/support/secretscrub"
)

func scrubTestScrubber() *secretscrub.Scrubber {
	return secretscrub.New([]secretscrub.Secret{
		{Name: "fixture", Value: "LOGFIXTURE"},
	})
}

// scrubTestLogger builds a logger writing JSON into buf through a scrub
// handler, mirroring the production buildHandler wrapping.
func scrubTestLogger(t *testing.T, scrubber *secretscrub.Scrubber) (*slog.Logger, *bytes.Buffer) {
	t.Helper()

	var buf bytes.Buffer

	handler, err := buildHandler(Options{Level: "debug", Writer: &buf}, scrubber)
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}

	return slog.New(handler), &buf
}

// decodeLastLine parses the last complete JSON line of the buffer.
func decodeLastLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) == 0 {
		t.Fatal("no log output")
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &entry); err != nil {
		t.Fatalf("decode log line %q: %v", lines[len(lines)-1], err)
	}

	return entry
}

func TestScrubHandlerMessage(t *testing.T) {
	t.Parallel()

	logger, buf := scrubTestLogger(t, scrubTestScrubber())

	logger.Error("request to /botLOGFIXTURE/getMe failed")

	entry := decodeLastLine(t, buf)
	if entry["msg"] != "request to /bot[REDACTED:fixture]/getMe failed" {
		t.Fatalf("message not scrubbed: %v", entry["msg"])
	}
}

func TestScrubHandlerStringAttrAndErrorAttr(t *testing.T) {
	t.Parallel()

	logger, buf := scrubTestLogger(t, scrubTestScrubber())

	leaking := errors.New(`Post "https://api.telegram.org/botLOGFIXTURE/sendMessage": refused`)
	logger.Error("llm call failed", "url", "https://host/botLOGFIXTURE/getMe", "error", leaking)

	entry := decodeLastLine(t, buf)
	if entry["url"] != "https://host/bot[REDACTED:fixture]/getMe" {
		t.Fatalf("string attribute not scrubbed: %v", entry["url"])
	}

	if !strings.Contains(entry["error"].(string), "[REDACTED:fixture]") {
		t.Fatalf("error attribute not scrubbed: %v", entry["error"])
	}

	if strings.Contains(buf.String(), "LOGFIXTURE") {
		t.Fatal("fixture survived anywhere in the output")
	}
}

func TestScrubHandlerKeepsCleanRecordUnchanged(t *testing.T) {
	t.Parallel()

	logger, buf := scrubTestLogger(t, scrubTestScrubber())

	stamp := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	logger.Info("plain operation", "count", 3, "at", stamp)

	entry := decodeLastLine(t, buf)
	if entry["msg"] != "plain operation" || entry["count"] != float64(3) {
		t.Fatalf("clean record was altered: %v", entry)
	}

	if entry["at"] == nil {
		t.Fatal("time attribute was dropped")
	}
}

func TestScrubHandlerGroupAttrs(t *testing.T) {
	t.Parallel()

	logger, buf := scrubTestLogger(t, scrubTestScrubber())

	grouped := logger.WithGroup("details").With("token", "LOGFIXTURE")
	grouped.Error("with group", "secret", "LOGFIXTURE")

	entry := decodeLastLine(t, buf)
	details, ok := entry["details"].(map[string]any)
	if !ok {
		t.Fatalf("group attribute missing: %v", entry)
	}

	if details["token"] != "[REDACTED:fixture]" {
		t.Fatalf("preformatted group attribute not scrubbed: %v", details["token"])
	}

	if details["secret"] != "[REDACTED:fixture]" {
		t.Fatalf("per-call group attribute not scrubbed: %v", details["secret"])
	}
}

func TestScrubHandlerEmptyGroupNotMaterialized(t *testing.T) {
	t.Parallel()

	logger, buf := scrubTestLogger(t, scrubTestScrubber())

	logger.WithGroup("empty").Error("no attrs under the group")

	entry := decodeLastLine(t, buf)
	if _, exists := entry["empty"]; exists {
		t.Fatalf("empty group was materialized: %v", entry)
	}
}

func TestScrubHandlerWithAttrsBeforeGroup(t *testing.T) {
	t.Parallel()

	logger, buf := scrubTestLogger(t, scrubTestScrubber())

	logger.With("pkg", "llm", "key", "LOGFIXTURE").Error("plain message")

	entry := decodeLastLine(t, buf)
	if entry["pkg"] != "llm" {
		t.Fatalf("clean preformatted attribute lost: %v", entry["pkg"])
	}

	if entry["key"] != "[REDACTED:fixture]" {
		t.Fatalf("preformatted attribute not scrubbed: %v", entry["key"])
	}
}

func TestScrubHandlerNilScrubberPassthrough(t *testing.T) {
	t.Parallel()

	logger, buf := scrubTestLogger(t, nil)

	logger.Error("nothing to scrub here")

	entry := decodeLastLine(t, buf)
	if entry["msg"] != "nothing to scrub here" {
		t.Fatalf("nil scrubber altered output: %v", entry["msg"])
	}
}

func TestScrubHandlerPreservesLevelAndCallerPC(t *testing.T) {
	t.Parallel()

	logger, buf := scrubTestLogger(t, scrubTestScrubber())

	logger.Warn("guarded message LOGFIXTURE")

	entry := decodeLastLine(t, buf)
	if entry["level"] != "WARN" {
		t.Fatalf("level not preserved: %v", entry["level"])
	}

	if _, hasTime := entry["time"]; !hasTime {
		t.Fatal("timestamp not preserved")
	}
}

// Compile-time interface conformance.
var _ slog.Handler = (*scrubHandler)(nil)

type deferredSecret struct{}

func (deferredSecret) LogValue() slog.Value { return slog.StringValue("carry LOGFIXTURE through") }

func TestScrubHandlerResolvesLogValuer(t *testing.T) {
	t.Parallel()

	logger, buf := scrubTestLogger(t, scrubTestScrubber())

	logger.Error("deferred attribute", "lazy", deferredSecret{})

	entry := decodeLastLine(t, buf)
	if containsFixture(entry["lazy"]) {
		t.Fatalf("deferred LogValuer attribute was not scrubbed: %v", entry["lazy"])
	}
}

func containsFixture(value any) bool {
	s, ok := value.(string)

	return ok && strings.Contains(s, "LOGFIXTURE")
}
