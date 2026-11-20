package secretscrub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
)

// sanitizeDepth bounds recursion into nested values: content below it fails
// closed to the truncation marker rather than surviving unredacted. Real
// diagnostic payloads sit far above it, and the cap also terminates
// self-referencing maps and slices.
const sanitizeDepth = 8

// ScrubValue returns a secret-free replacement for an arbitrary payload
// value — the shared sanitizer behind the slog attribute handler and the
// Sentry event walk.
//
// Scalars and strings cannot hide secret-bearing text behind a custom
// rendering: strings are scrubbed literally, and the numeric/boolean kinds
// pass through structurally (their rendering is deterministic, and
// round-tripping them through JSON would cost numeric precision). The JSON-
// shaped container types are copied and walked, keys included. Everything
// else — custom structs, SDK objects, exotic map types — goes through the
// serialization boundary of scrubOpaque: it is marshaled ONCE, decoded, and
// only the scrubbed decoded representation is retained.
//
// The original opaque value is therefore never returned, deliberately: a
// custom marshaler can render differently — and leak — on its next call,
// which is exactly the call downstream serialization would make. Inspecting
// the value and passing the original through on a clean rendering would let
// the leak happen after the inspection.
func (s *Scrubber) ScrubValue(value any) any {
	return s.scrubAny(value, sanitizeDepth)
}

// ScrubMap returns an owned copy of an arbitrary payload map with every key
// and value scrubbed (see ScrubValue). A nil map stays nil so optional
// payload fields keep their absent shape.
func (s *Scrubber) ScrubMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}

	out := make(map[string]any, len(values))

	for key, value := range values {
		out[s.Scrub(key)] = s.scrubAny(value, sanitizeDepth-1)
	}

	return out
}

// ErrorText renders an error the way diagnostic surfaces do (its Error()
// text) and scrubs the rendering. Rendering is panic-guarded: a typed-nil
// error renders as "<nil>" — matching the log handler's recover contract —
// and a panicking Error() is caught, scrubbed, and reported as a panic
// rendering instead of crashing the caller.
func (s *Scrubber) ErrorText(err error) (text string) {
	defer func() {
		if r := recover(); r != nil {
			if v := reflect.ValueOf(err); v.Kind() == reflect.Pointer && v.IsNil() {
				text = "<nil>"

				return
			}

			text = s.Scrub(fmt.Sprintf("!PANIC: %v", r))
		}
	}()

	return s.Scrub(err.Error())
}

func (s *Scrubber) scrubAny(value any, depth int) any {
	if depth <= 0 {
		return s.truncatedMarker
	}

	// Named string types arrive here alongside plain strings (a log
	// attribute of a named string type has KindAny, for example), and their
	// underlying text scrubs like any other string. json.Number is
	// string-kind too, but it is numeric literal text produced by the
	// boundary's own decode and passes through as-is, preserving numeric
	// precision.
	if reflected := reflect.ValueOf(value); reflected.Kind() == reflect.String {
		if _, isNumber := value.(json.Number); isNumber {
			return value
		}

		return s.Scrub(reflected.String())
	}

	switch typed := value.(type) {
	case nil:
		return nil
	case bool, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, float32, float64:
		return value
	case error:
		return s.ErrorText(typed)
	case map[string]any:
		if typed == nil {
			return nil
		}

		out := make(map[string]any, len(typed))

		for key, elem := range typed {
			out[s.Scrub(key)] = s.scrubAny(elem, depth-1)
		}

		return out
	case map[string]string:
		if typed == nil {
			return nil
		}

		out := make(map[string]string, len(typed))

		for key, elem := range typed {
			out[s.Scrub(key)] = s.Scrub(elem)
		}

		return out
	case []any:
		out := make([]any, len(typed))

		for i, elem := range typed {
			out[i] = s.scrubAny(elem, depth-1)
		}

		return out
	case []string:
		out := make([]string, len(typed))

		for i, elem := range typed {
			out[i] = s.Scrub(elem)
		}

		return out
	default:
		return s.scrubOpaque(value, depth)
	}
}

// scrubOpaque is the serialization boundary for composite values the typed
// walk cannot enter. The value is marshaled exactly once — the bytes a
// downstream serializer would produce — decoded, and rebuilt into plain JSON
// values whose strings and keys are scrubbed: decoding before scrubbing is
// what makes JSON-escaped renderings of a secret (HTML-sensitive characters
// arrive as < and friends) match the registry at all.
//
// Nothing opaque passes through, in either failure direction: a marshaler
// whose output carries no registered value is still replaced by its decoded
// rendering, because it can render differently on the next call, and a
// marshal failure or panic collapses to an omission marker or a scrubbed
// panic rendering — an error text from a refusing marshaler is exactly what
// a downstream handler would print, so the original is never handed on to
// produce it. encoding/json bounds the boundary itself (it errors on pointer
// cycles), so no unbounded reflective traversal is introduced.
func (s *Scrubber) scrubOpaque(value any, depth int) (out any) {
	defer func() {
		if r := recover(); r != nil {
			out = s.Scrub(fmt.Sprintf("!PANIC: %v", r))
		}
	}()

	encoded, err := json.Marshal(value)
	if err != nil {
		return s.unserializableMarker
	}

	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()

	if err := decoder.Decode(&decoded); err != nil {
		// Unreachable for json.Marshal output; fail closed rather than pass
		// anything uninspected.
		return s.Scrub(string(encoded))
	}

	return s.scrubAny(decoded, depth-1)
}
