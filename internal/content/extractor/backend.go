package extractor

import (
	"context"
	"fmt"
	"log/slog"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/content/fetch"
)

// Backend is one extraction engine: a CPU-only pass over an already-fetched
// page. Backends never perform network IO — the shared fetcher (with its SSRF
// policy) is the only network boundary.
type Backend interface {
	// Name identifies the backend in configuration and logs.
	Name() string
	// Extract parses the page and returns the raw content candidates.
	Extract(ctx context.Context, page *fetch.Page) (Content, error)
}

// Content is a backend's raw extraction result before pipeline selection.
// Text and HTML are two renderings of the same content: Text is what
// consumers historically received, HTML keeps structure (headings, lists,
// links, tables, code) and is the source for Markdown rendering.
type Content struct {
	Title string
	Text  string
	HTML  string

	// Empty is set by the producing backend when it determines, by its own
	// standard, that nothing extractable was found. Backends whose libraries
	// still render wrapper markup for empty results (so Text/HTML alone do
	// not prove emptiness) rely on this flag. The pipeline checks it first;
	// the structural nonEmpty() check below remains as defense for results
	// that leave the flag unset.
	Empty bool
}

// nonEmpty reports whether the content carries anything usable.
func (c Content) nonEmpty() bool {
	return c.Text != "" || c.HTML != ""
}

// NewBackends builds the extraction backends for the configured names, in
// order. Unknown names are an error (config validation normally catches this
// earlier; the check here keeps construction total).
func NewBackends(names []string, logger *slog.Logger) ([]Backend, error) {
	if logger == nil {
		logger = slog.Default().With("pkg", "content/extractor")
	}

	backends := make([]Backend, 0, len(names))
	for _, name := range names {
		backend, err := newBackend(name, logger.With("backend", name))
		if err != nil {
			return nil, err
		}
		backends = append(backends, backend)
	}

	return backends, nil
}

func newBackend(name string, logger *slog.Logger) (Backend, error) {
	switch name {
	case config.ExtractionBackendReadability:
		return NewReadabilityExtractor(logger), nil
	case config.ExtractionBackendTrafilatura:
		return NewTrafilaturaExtractor(logger), nil
	case config.ExtractionBackendDomDistiller:
		return NewDomDistillerExtractor(logger), nil
	default:
		return nil, fmt.Errorf("unknown extraction backend %q", name)
	}
}
