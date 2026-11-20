package secretscrub

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// boundaryScrubber registers one fixture for the serialization-boundary
// tests.
func boundaryScrubber() *Scrubber {
	return New([]Secret{{Name: "fixture", Value: "SANITIZEFIXTURE"}})
}

// statefulMarshaler renders clean JSON on its first marshaling and
// secret-bearing JSON afterwards — the shape of a marshaler that behaves
// differently between the scrubber's inspection and a downstream serializer's
// rendering.
type statefulMarshaler struct{ calls int }

func (m *statefulMarshaler) MarshalJSON() ([]byte, error) {
	m.calls++

	if m.calls == 1 {
		return []byte(`{"note":"clean"}`), nil
	}

	return []byte(`{"note":"SANITIZEFIXTURE"}`), nil
}

func TestScrubValueStatefulMarshalerNeverPassesThrough(t *testing.T) {
	t.Parallel()

	scrubber := boundaryScrubber()
	value := &statefulMarshaler{}

	got := scrubber.ScrubValue(value)

	// The returned value must be the rebuilt rendering of the FIRST
	// serialization, not the original object: re-marshaling it — what a
	// downstream serializer would do — must not invoke the marshaler again,
	// whose later output carries the secret.
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal rebuilt value: %v", err)
	}

	if value.calls != 1 {
		t.Fatalf("original marshaler object passed downstream (marshaled %d times)", value.calls)
	}

	if strings.Contains(string(encoded), "SANITIZEFIXTURE") {
		t.Fatalf("stateful marshaler leaked on re-rendering: %s", encoded)
	}

	if !strings.Contains(string(encoded), "clean") {
		t.Fatalf("rebuilt value lost its content: %s", encoded)
	}
}

// erroringMarshaler refuses to marshal with a secret-bearing error — the
// text a downstream handler would print for the failed marshal.
type erroringMarshaler struct{}

func (erroringMarshaler) MarshalJSON() ([]byte, error) {
	return nil, errors.New("cannot serialize SANITIZEFIXTURE")
}

func TestScrubValueMarshalErrorFailsClosed(t *testing.T) {
	t.Parallel()

	scrubber := boundaryScrubber()

	got := scrubber.ScrubValue(erroringMarshaler{})

	text, ok := got.(string)
	if !ok || text != scrubber.UnserializableMarker() {
		t.Fatalf("marshal failure did not collapse to the omission marker: %v", got)
	}
}

type panickingMarshaler struct{}

func (panickingMarshaler) MarshalJSON() ([]byte, error) {
	panic("marshal exploded SANITIZEFIXTURE")
}

func TestScrubValuePanickingMarshaler(t *testing.T) {
	t.Parallel()

	scrubber := boundaryScrubber()

	got := scrubber.ScrubValue(panickingMarshaler{})

	text, ok := got.(string)
	if !ok || !strings.Contains(text, "!PANIC:") {
		t.Fatalf("panicking marshaler lost its panic rendering: %v", got)
	}

	if scrubber.Contains(text) {
		t.Fatalf("panic rendering carried the secret: %q", text)
	}
}

// escapedMarshaler renders a secret whose characters encoding/json escapes
// (& arrives as the six-character HTML-escape sequence), so the secret never
// appears verbatim in the encoded form — only decoding the representation
// finds it.
type escapedMarshaler struct{}

func (escapedMarshaler) MarshalJSON() ([]byte, error) {
	return []byte(`{"tok":"ESC&FIXTURE"}`), nil
}

func TestScrubValueDecodesBeforeScrubbing(t *testing.T) {
	t.Parallel()

	scrubber := New([]Secret{{Name: "escaped", Value: "ESC&FIXTURE"}})

	got := scrubber.ScrubValue(escapedMarshaler{})

	object, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("escaped secret lost its object shape: %v", got)
	}

	if object["tok"] != "[REDACTED:escaped]" {
		t.Fatalf("JSON-escaped secret not scrubbed: %v", object["tok"])
	}
}

// precisionStruct carries an integer beyond float64's exact range: a decode
// without exact numbers would corrupt it on the way through the boundary.
type precisionStruct struct {
	Big int64 `json:"big"`
}

func TestScrubValuePreservesNumericPrecision(t *testing.T) {
	t.Parallel()

	// No registered secrets: the boundary must not corrupt clean content.
	scrubber := New(nil)

	got := scrubber.ScrubValue(precisionStruct{Big: 9007199254740993})

	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal rebuilt value: %v", err)
	}

	if !strings.Contains(string(encoded), "9007199254740993") {
		t.Fatalf("numeric precision lost through the boundary: %s", encoded)
	}

	if strings.Contains(string(encoded), "9007199254740992") {
		t.Fatalf("number was rounded through float64: %s", encoded)
	}
}

func TestScrubValueScalarsPassThrough(t *testing.T) {
	t.Parallel()

	scrubber := boundaryScrubber()

	if got := scrubber.ScrubValue(nil); got != nil {
		t.Fatalf("nil altered: %v", got)
	}

	for _, scalar := range []any{true, 42, int8(7), uint16(9), 1.5} {
		if got := scrubber.ScrubValue(scalar); got != scalar {
			t.Fatalf("scalar %v altered: %v", scalar, got)
		}
	}

	if got := scrubber.ScrubValue("plain SANITIZEFIXTURE").(string); scrubber.Contains(got) {
		t.Fatalf("string not scrubbed: %q", got)
	}

	if got := scrubber.ScrubValue(errors.New("failed for SANITIZEFIXTURE")).(string); scrubber.Contains(got) {
		t.Fatalf("error not scrubbed: %q", got)
	}
}

func TestScrubValueWalksMapKeysAndErrorValues(t *testing.T) {
	t.Parallel()

	scrubber := boundaryScrubber()

	got := scrubber.ScrubMap(map[string]any{
		"key SANITIZEFIXTURE": "value SANITIZEFIXTURE",
		"err":                 errors.New("chain SANITIZEFIXTURE"),
		"count":               3,
	})

	if got["key [REDACTED:fixture]"] != "value [REDACTED:fixture]" {
		t.Fatalf("map key or value not scrubbed: %v", got)
	}

	errText, ok := got["err"].(string)
	if !ok || !strings.Contains(errText, "[REDACTED:fixture]") {
		t.Fatalf("error value not scrubbed: %v", got["err"])
	}

	// Structural scalars keep their Go type through the walk.
	if count, ok := got["count"].(int); !ok || count != 3 {
		t.Fatalf("scalar value altered: %v", got["count"])
	}
}

func TestScrubMapNilStaysNil(t *testing.T) {
	t.Parallel()

	if got := boundaryScrubber().ScrubMap(nil); got != nil {
		t.Fatalf("nil map was rebuilt: %v", got)
	}
}

func TestScrubValueDepthFailClosed(t *testing.T) {
	t.Parallel()

	scrubber := boundaryScrubber()

	var cursor any = "SANITIZEFIXTURE"
	for i := 0; i < 20; i++ {
		cursor = []any{cursor}
	}

	got := fmt.Sprint(scrubber.ScrubValue(cursor))

	if scrubber.Contains(got) {
		t.Fatalf("content past the depth cap survived: %s", got)
	}

	if !strings.Contains(got, scrubber.TruncatedMarker()) {
		t.Fatalf("truncated subtree lost its marker: %s", got)
	}
}
