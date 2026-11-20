package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"sync"
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

type payloadStruct struct {
	URL string
	N   int
}

type namedFixture string

type marshalFixture struct{}

func (marshalFixture) MarshalJSON() ([]byte, error) {
	return []byte(`{"tok":"LOGFIXTURE","n":7}`), nil
}

type panicError struct{}

func (panicError) Error() string { panic("boom LOGFIXTURE") }

func decodeEntry(t *testing.T, buf *bytes.Buffer, key string) any {
	t.Helper()

	entry := decodeLastLine(t, buf)

	value, ok := entry[key]
	if !ok {
		t.Fatalf("attribute %q missing from record: %v", key, entry)
	}

	return value
}

func TestScrubHandlerScrubsStructuredAttrs(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		log   func(logger *slog.Logger)
		check func(t *testing.T, value any)
	}{
		{
			name: "struct attr",
			log: func(logger *slog.Logger) {
				logger.Error("payload", "data", payloadStruct{URL: "https://host/botLOGFIXTURE/x", N: 3})
			},
			check: func(t *testing.T, value any) {
				object, ok := value.(map[string]any)
				if !ok {
					t.Fatalf("structured attr lost its object shape: %v", value)
				}

				if object["URL"] != "https://host/bot[REDACTED:fixture]/x" {
					t.Fatalf("struct field not scrubbed: %v", object["URL"])
				}

				if object["N"] != float64(3) {
					t.Fatalf("clean struct field altered: %v", object["N"])
				}
			},
		},
		{
			name: "map attr",
			log: func(logger *slog.Logger) {
				logger.Error("payload", "data", map[string]any{"nested": map[string]any{"tok": "LOGFIXTURE"}})
			},
			check: func(t *testing.T, value any) {
				nested, ok := value.(map[string]any)["nested"].(map[string]any)
				if !ok {
					t.Fatalf("nested map shape lost: %v", value)
				}

				if nested["tok"] != "[REDACTED:fixture]" {
					t.Fatalf("nested map value not scrubbed: %v", nested["tok"])
				}
			},
		},
		{
			name: "json.RawMessage attr",
			log: func(logger *slog.Logger) {
				logger.Error("payload", "data", json.RawMessage(`{"tok":"LOGFIXTURE"}`))
			},
			check: func(t *testing.T, value any) {
				object, ok := value.(map[string]any)
				if !ok {
					t.Fatalf("raw message lost its object shape: %v", value)
				}

				if object["tok"] != "[REDACTED:fixture]" {
					t.Fatalf("raw message content not scrubbed: %v", object["tok"])
				}
			},
		},
		{
			name: "custom marshaler attr",
			log: func(logger *slog.Logger) {
				logger.Error("payload", "data", marshalFixture{})
			},
			check: func(t *testing.T, value any) {
				object, ok := value.(map[string]any)
				if !ok {
					t.Fatalf("marshaler output lost its object shape: %v", value)
				}

				if object["tok"] != "[REDACTED:fixture]" || object["n"] != float64(7) {
					t.Fatalf("marshaler output not scrubbed correctly: %v", object)
				}
			},
		},
		{
			name: "named string attr",
			log: func(logger *slog.Logger) {
				logger.Error("payload", "data", namedFixture("LOGFIXTURE"))
			},
			check: func(t *testing.T, value any) {
				if value != "[REDACTED:fixture]" {
					t.Fatalf("named string not scrubbed: %v", value)
				}
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			logger, buf := scrubTestLogger(t, scrubTestScrubber())

			testCase.log(logger)

			testCase.check(t, decodeEntry(t, buf, "data"))
		})
	}
}

func TestScrubHandlerKeepsCleanStructuredAttrIdentical(t *testing.T) {
	t.Parallel()

	logger, buf := scrubTestLogger(t, scrubTestScrubber())

	logger.Error("payload", "data", payloadStruct{URL: "https://host/clean", N: 3})

	value, ok := decodeEntry(t, buf, "data").(map[string]any)
	if !ok {
		t.Fatalf("clean structured attr lost its object shape: %v", buf.String())
	}

	if value["URL"] != "https://host/clean" || value["N"] != float64(3) {
		t.Fatalf("clean structured attr was altered: %v", value)
	}
}

func TestScrubHandlerScrubsAttrKeysAndGroupNames(t *testing.T) {
	t.Parallel()

	logger, buf := scrubTestLogger(t, scrubTestScrubber())

	logger.Error("keys and groups", "key LOGFIXTURE", "value")

	entry := decodeLastLine(t, buf)
	if entry["key [REDACTED:fixture]"] != "value" {
		t.Fatalf("attribute key not scrubbed: %v", entry)
	}

	logger, buf = scrubTestLogger(t, scrubTestScrubber())
	logger.WithGroup("group LOGFIXTURE").Error("grouped", "x", 1)

	entry = decodeLastLine(t, buf)
	if _, ok := entry["group [REDACTED:fixture]"]; !ok {
		t.Fatalf("group name not scrubbed: %v", entry)
	}
}

func TestScrubHandlerScrubsPanickingErrorPanicValue(t *testing.T) {
	t.Parallel()

	logger, buf := scrubTestLogger(t, scrubTestScrubber())

	// slog renders a panicking Error() as "!PANIC: <panic value>"; the panic
	// value itself can carry secret material and must be scrubbed like any
	// other rendering.
	logger.Error("panicking", "err", panicError{})

	entry := decodeLastLine(t, buf)
	text, _ := entry["err"].(string)

	if !strings.Contains(text, "!PANIC:") {
		t.Fatalf("panic rendering lost: %v", entry["err"])
	}

	if strings.Contains(text, "LOGFIXTURE") {
		t.Fatalf("panic value leaked the secret: %v", entry["err"])
	}

	if strings.Contains(buf.String(), "LOGFIXTURE") {
		t.Fatal("fixture survived anywhere in the output")
	}
}

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

func TestScrubHandlerTypedNilErrorAttr(t *testing.T) {
	t.Parallel()

	logger, buf := scrubTestLogger(t, scrubTestScrubber())

	// A typed-nil error must render the way plain slog does — "<nil>" —
	// instead of panicking where the delegate handler would not.
	logger.Error("typed nil", "err", (*url.Error)(nil))

	entry := decodeLastLine(t, buf)
	if entry["err"] != "<nil>" {
		t.Fatalf("typed-nil error not handled like slog: %v", entry["err"])
	}

	// A real error still scrubs through the same path.
	logger.Error("real error", "err", errors.New("failed for LOGFIXTURE"))

	entry = decodeLastLine(t, buf)
	if !strings.Contains(entry["err"].(string), "[REDACTED:fixture]") {
		t.Fatalf("real error attribute not scrubbed: %v", entry["err"])
	}
}

func TestScrubHandlerPreservesAttributeAttachmentGroup(t *testing.T) {
	t.Parallel()

	logger, buf := scrubTestLogger(t, scrubTestScrubber())

	// slog group semantics are positional: attributes attached before
	// WithGroup stay outside the group; only later ones land inside.
	grouped := logger.With("request_id", "r-1").WithGroup("details")
	grouped.Error("ordering", "secret", "LOGFIXTURE")

	entry := decodeLastLine(t, buf)
	if entry["request_id"] != "r-1" {
		t.Fatalf("pre-group attribute was moved under the group: %v", entry)
	}

	details, ok := entry["details"].(map[string]any)
	if !ok {
		t.Fatalf("group attribute missing: %v", entry)
	}

	if details["secret"] != "[REDACTED:fixture]" {
		t.Fatalf("in-group attribute not scrubbed: %v", details["secret"])
	}
}

func TestScrubHandlerDoesNotMutateCallerGroupAttr(t *testing.T) {
	t.Parallel()

	logger, buf := scrubTestLogger(t, scrubTestScrubber())

	// slog.Value.Group() exposes the caller's underlying slice; scrubbing
	// must copy it, or this attribute would change under the caller and
	// concurrent log calls would race on it.
	reusedGroup := slog.Group("details", "secret", "LOGFIXTURE")

	logger.LogAttrs(context.Background(), slog.LevelError, "with group attr", reusedGroup)

	if got := reusedGroup.Value.Group()[0].Value.String(); got != "LOGFIXTURE" {
		t.Fatalf("caller's group attribute was mutated: %q", got)
	}

	entry := decodeLastLine(t, buf)
	details, ok := entry["details"].(map[string]any)
	if !ok {
		t.Fatalf("group attribute missing: %v", entry)
	}

	if details["secret"] != "[REDACTED:fixture]" {
		t.Fatalf("group attribute not scrubbed: %v", details["secret"])
	}

	// Logging the same attribute from several goroutines exercises the race
	// the in-place scrubbing used to cause; -race watches this section.
	loggers := make([]*slog.Logger, 4)
	buffers := make([]*bytes.Buffer, 4)

	for i := range loggers {
		loggers[i], buffers[i] = scrubTestLogger(t, scrubTestScrubber())
	}

	var group sync.WaitGroup

	for i := range loggers {
		group.Add(1)

		go func(idx int) {
			defer group.Done()
			loggers[idx].LogAttrs(context.Background(), slog.LevelError, "concurrent", reusedGroup)

			if strings.Contains(buffers[idx].String(), "LOGFIXTURE") {
				t.Errorf("fixture survived concurrent scrubbing")
			}
		}(i)
	}

	group.Wait()

	if got := reusedGroup.Value.Group()[0].Value.String(); got != "LOGFIXTURE" {
		t.Fatalf("caller's group attribute was mutated concurrently: %q", got)
	}
}

// statefulFixture renders clean JSON on its first marshaling and
// secret-bearing JSON afterwards, mimicking a marshaler whose output differs
// between the scrub boundary's inspection and the delegate handler's
// rendering.
type statefulFixture struct{ calls int }

func (f *statefulFixture) MarshalJSON() ([]byte, error) {
	f.calls++

	if f.calls == 1 {
		return []byte(`{"note":"clean"}`), nil
	}

	return []byte(`{"note":"LOGFIXTURE"}`), nil
}

// erroringFixture refuses marshaling with a secret-bearing error — the text
// the delegate handler would print for the failed marshal when the original
// value is passed through.
type erroringFixture struct{}

func (erroringFixture) MarshalJSON() ([]byte, error) {
	return nil, errors.New("cannot serialize LOGFIXTURE")
}

// escapedFixture renders the secret behind encoding/json's HTML escaping
// (& is emitted as a six-character escape), so the secret never appears
// verbatim in the encoded form.
type escapedFixture struct{}

func (escapedFixture) MarshalJSON() ([]byte, error) {
	return []byte(`{"tok":"ESC&FIXTURE"}`), nil
}

func TestScrubHandlerStatefulMarshalerNeverPassesThrough(t *testing.T) {
	t.Parallel()

	logger, buf := scrubTestLogger(t, scrubTestScrubber())

	logger.Error("stateful payload", "data", &statefulFixture{})

	// The assertion is against the final serialized record: the emitted JSON
	// must come from the boundary's first serialization only, never from a
	// second render of the original object.
	if strings.Contains(buf.String(), "LOGFIXTURE") {
		t.Fatalf("stateful marshaler leaked into the emitted record: %s", buf.String())
	}

	entry := decodeLastLine(t, buf)
	data, ok := entry["data"].(map[string]any)
	if !ok || data["note"] != "clean" {
		t.Fatalf("rebuilt attribute lost its content: %v", entry["data"])
	}
}

func TestScrubHandlerMarshalErrorFailsClosed(t *testing.T) {
	t.Parallel()

	logger, buf := scrubTestLogger(t, scrubTestScrubber())

	logger.Error("failing payload", "data", erroringFixture{})

	// A marshaling failure must never hand the original value to the
	// delegate handler, whose own !ERROR rendering would echo the marshal
	// error — secret-bearing here.
	if strings.Contains(buf.String(), "LOGFIXTURE") || strings.Contains(buf.String(), "!ERROR") {
		t.Fatalf("marshal failure leaked into the emitted record: %s", buf.String())
	}

	entry := decodeLastLine(t, buf)
	if entry["data"] != scrubTestScrubber().UnserializableMarker() {
		t.Fatalf("marshal failure not replaced by the omission marker: %v", entry["data"])
	}
}

func TestScrubHandlerDecodesEscapedSecrets(t *testing.T) {
	t.Parallel()

	scrubber := secretscrub.New([]secretscrub.Secret{{Name: "escaped", Value: "ESC&FIXTURE"}})
	logger, buf := scrubTestLogger(t, scrubber)

	logger.Error("escaped payload", "data", escapedFixture{})

	// The emitted record must carry neither the raw fixture nor its
	// HTML-escaped rendering — a text-level search of the encoded bytes
	// would miss the latter.
	for _, form := range []string{"ESC&FIXTURE", "ESC\\u0026FIXTURE"} {
		if strings.Contains(buf.String(), form) {
			t.Fatalf("escaped secret rendering survived: %s", buf.String())
		}
	}

	entry := decodeLastLine(t, buf)
	data, ok := entry["data"].(map[string]any)
	if !ok || data["tok"] != "[REDACTED:escaped]" {
		t.Fatalf("escaped secret not scrubbed: %v", entry["data"])
	}
}

func TestScrubHandlerPreservesNumericPrecision(t *testing.T) {
	t.Parallel()

	logger, buf := scrubTestLogger(t, secretscrub.New(nil))

	// An opaque struct is rebuilt through the boundary; a decode without
	// exact numbers would round 9007199254740993 to ...92.
	logger.Error("precision", "data", struct {
		Big int64 `json:"big"`
	}{Big: 9007199254740993})

	if !strings.Contains(buf.String(), "9007199254740993") {
		t.Fatalf("numeric precision lost through the boundary: %s", buf.String())
	}

	if strings.Contains(buf.String(), "9007199254740992") {
		t.Fatalf("number was rounded through float64: %s", buf.String())
	}
}
