package logging

import (
	"context"
	"log/slog"

	"telegram-ollama-reply-bot/internal/support/secretscrub"
)

// scrubHandler scrubs registered secrets from every record before it reaches
// the delegate handler. It is the app-wide log backstop: the per-site
// sanitizers (safeerr wrapping, URL redaction, the bot logger's token
// scrubbing) cover known shapes, while this handler catches whatever still
// carries secret material at write time.
//
// slog handlers own their preformatted state (WithAttrs/WithGroup), so this
// wrapper buffers the scrubbed With/WithGroup timeline itself and rebuilds
// the record at Handle time; the delegate only ever sees plain Handle calls
// with fully scrubbed records. Rebuilding keeps the time, level, and caller
// PC of the original record.
//
// The timeline matters because slog group semantics are positional: a
// WithGroup opens its group for every attribute attached AFTER it, while
// attributes attached before stay outside. A flat attribute list plus an
// open-group path would wrongly move pre-group attributes under the group.
type scrubHandler struct {
	scrubber *secretscrub.Scrubber
	next     slog.Handler
	entries  []scrubEntry
}

// scrubEntry is one step of the With/WithGroup timeline: either an
// already-scrubbed attribute or a group opened from that point on.
type scrubEntry struct {
	group string    // non-empty: a group opened here
	attr  slog.Attr // valid when group is empty
}

func newScrubHandler(scrubber *secretscrub.Scrubber, next slog.Handler) slog.Handler {
	return &scrubHandler{scrubber: scrubber, next: next}
}

func (h *scrubHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *scrubHandler) Handle(ctx context.Context, record slog.Record) error {
	var rebuilt slog.Record
	rebuilt.Time = record.Time
	rebuilt.Level = record.Level
	rebuilt.PC = record.PC
	rebuilt.Message = h.scrubber.Scrub(record.Message)

	callAttrs := make([]slog.Attr, 0, 4)

	record.Attrs(func(attr slog.Attr) bool {
		callAttrs = append(callAttrs, h.scrubAttr(attr))

		return true
	})

	rebuilt.AddAttrs(h.rebuild(callAttrs)...)

	return h.next.Handle(ctx, rebuilt)
}

// rebuild folds the buffered timeline plus the per-call attributes into the
// attribute tree slog would have produced. An open group with no attributes
// is never materialized, matching slog's empty-group omission.
func (h *scrubHandler) rebuild(callAttrs []slog.Attr) []slog.Attr {
	type frame struct {
		name  string
		attrs []slog.Attr
	}

	// Frames map one-to-one onto the timeline's group markers; the root
	// frame holds everything attached before the first group.
	frames := []frame{{}}

	for _, entry := range h.entries {
		if entry.group != "" {
			frames = append(frames, frame{name: entry.group})

			continue
		}

		last := len(frames) - 1
		frames[last].attrs = append(frames[last].attrs, entry.attr)
	}

	// Per-call attributes attach after everything buffered, so they land in
	// the innermost open group.
	last := len(frames) - 1
	frames[last].attrs = append(frames[last].attrs, callAttrs...)

	out := frames[last].attrs

	for i := len(frames) - 1; i >= 1; i-- {
		parent := make([]slog.Attr, 0, len(frames[i-1].attrs)+1)
		parent = append(parent, frames[i-1].attrs...)
		parent = append(parent, groupAttr(frames[i].name, out))
		out = parent
	}

	return out
}

// groupAttr builds a group attribute from a slice of attributes, adapting
// slog.Group's variadic any signature.
func groupAttr(name string, attrs []slog.Attr) slog.Attr {
	args := make([]any, len(attrs))
	for i, attr := range attrs {
		args[i] = attr
	}

	return slog.Group(name, args...)
}

func (h *scrubHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	entries := make([]scrubEntry, 0, len(h.entries)+len(attrs))
	entries = append(entries, h.entries...)

	for _, attr := range attrs {
		entries = append(entries, scrubEntry{attr: h.scrubAttr(attr)})
	}

	return &scrubHandler{scrubber: h.scrubber, next: h.next, entries: entries}
}

func (h *scrubHandler) WithGroup(name string) slog.Handler {
	name = h.scrubber.Scrub(name)
	if name == "" {
		return h
	}

	entries := make([]scrubEntry, 0, len(h.entries)+1)
	entries = append(entries, h.entries...)
	entries = append(entries, scrubEntry{group: name})

	return &scrubHandler{scrubber: h.scrubber, next: h.next, entries: entries}
}

// scrubAttr scrubs one attribute: its key (call sites have been known to
// embed secret material in keys, and the JSON handler emits keys verbatim),
// then its value. String values are scrubbed literally; group values
// recursively; every KindAny value — structs, maps, slices, named string
// types, errors, custom JSON marshalers — goes through the shared
// serialization boundary of Scrubber.ScrubValue, so nothing the JSON handler
// could render reaches it uninspected, and no inspected value is passed
// through as the original object.
//
// Deferred LogValuer attributes are resolved here: the downstream handler
// would resolve them itself, past this scrub. Error attributes are rendered
// through their Error() text by the shared boundary's error path — what the
// delegate handler would print anyway — with the panic guard slog applies
// (a typed-nil renders as "<nil>", a panicking Error() as a scrubbed panic
// rendering).
func (h *scrubHandler) scrubAttr(attr slog.Attr) slog.Attr {
	attr.Key = h.scrubber.Scrub(attr.Key)
	attr.Value = attr.Value.Resolve()

	switch attr.Value.Kind() {
	case slog.KindString:
		attr.Value = slog.StringValue(h.scrubber.Scrub(attr.Value.String()))
	case slog.KindGroup:
		// Group() exposes the caller's underlying slice, so the scrubbed
		// children go into a fresh slice — mutating in place would change
		// the caller's attribute and race when the same attribute is logged
		// concurrently.
		source := attr.Value.Group()
		attrs := make([]slog.Attr, len(source))
		copy(attrs, source)

		for i := range attrs {
			attrs[i] = h.scrubAttr(attrs[i])
		}

		attr.Value = slog.GroupValue(attrs...)
	case slog.KindAny:
		attr.Value = slog.AnyValue(h.scrubber.ScrubValue(attr.Value.Any()))
	}

	return attr
}
