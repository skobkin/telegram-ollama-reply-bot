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
// wrapper buffers the scrubbed preformatted attributes and the open group
// path itself and rebuilds the record at Handle time; the delegate only ever
// sees plain Handle calls with fully scrubbed records. Rebuilding keeps the
// time, level, and caller PC of the original record.
type scrubHandler struct {
	scrubber *secretscrub.Scrubber
	next     slog.Handler
	attrs    []slog.Attr // preformatted, already scrubbed
	groups   []string    // open group path
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

	attrs := make([]slog.Attr, 0, len(h.attrs))
	attrs = append(attrs, h.attrs...)

	record.Attrs(func(attr slog.Attr) bool {
		attrs = append(attrs, h.scrubAttr(attr))

		return true
	})

	// slog applies the open group path to both preformatted and per-call
	// attributes, so the fold covers everything added above. An open group
	// with no attributes is never materialized, matching slog's empty-group
	// omission.
	for i := len(h.groups) - 1; i >= 0; i-- {
		attrs = []slog.Attr{groupAttr(h.groups[i], attrs)}
	}

	rebuilt.AddAttrs(attrs...)

	return h.next.Handle(ctx, rebuilt)
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
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)

	for _, attr := range attrs {
		merged = append(merged, h.scrubAttr(attr))
	}

	return &scrubHandler{scrubber: h.scrubber, next: h.next, attrs: merged, groups: h.groups}
}

func (h *scrubHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}

	groups := make([]string, 0, len(h.groups)+1)
	groups = append(groups, h.groups...)
	groups = append(groups, name)

	return &scrubHandler{scrubber: h.scrubber, next: h.next, attrs: h.attrs, groups: groups}
}

// scrubAttr scrubs one attribute. String values are scrubbed literally;
// group values recursively; error values are replaced by their scrubbed
// rendering — identical to what slog would print, but without retaining the
// original error object whose text could leak through later rendering.
//
// Deferred LogValuer attributes are resolved here: the downstream handler
// would resolve them itself, past this scrub. Values of other kinds
// (numbers, times, arbitrary structs and composites) cannot be inspected
// without reflection and pass through unchanged — attributes that embed
// secrets inside composites must not be logged at levels where that
// matters (see AGENTS.md).
func (h *scrubHandler) scrubAttr(attr slog.Attr) slog.Attr {
	attr.Value = attr.Value.Resolve()

	switch attr.Value.Kind() {
	case slog.KindString:
		attr.Value = slog.StringValue(h.scrubber.Scrub(attr.Value.String()))
	case slog.KindGroup:
		attrs := attr.Value.Group()

		for i := range attrs {
			attrs[i] = h.scrubAttr(attrs[i])
		}

		attr.Value = slog.GroupValue(attrs...)
	case slog.KindAny:
		if err, ok := attr.Value.Any().(error); ok {
			attr.Value = slog.StringValue(h.scrubber.Scrub(err.Error()))
		}
	}

	return attr
}
