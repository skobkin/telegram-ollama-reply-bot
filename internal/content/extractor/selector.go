package extractor

import (
	"bytes"
	"cmp"
	nurl "net/url"
	"slices"
	"strings"

	"telegram-ollama-reply-bot/internal/content/fetch"
)

// Features are cheap signals about a fetched page, computed once per
// request before any backend runs. Deliberately substring/size-based:
// every backend re-parses the body anyway, so the selector must not add
// a competing full parse.
type Features struct {
	// URL is the parsed final URL; nil when unparseable.
	URL *nurl.URL

	// Host is the lowercased hostname with a leading "www." stripped.
	Host string

	// BodySize is len(page.Body) in bytes.
	BodySize int

	// MediaWiki reports MediaWiki markers in the body or a wiki host.
	MediaWiki bool
}

// MediaWiki body markers. Package-level []byte so the scan never converts
// the (potentially multi-megabyte) body to a string.
var (
	mwContentTextNeedle = []byte(`id="mw-content-text"`)
	mediaWikiNeedle     = []byte("mediawiki")
)

func extractFeatures(page *fetch.Page) Features {
	pageURL := parsePageURL(page)

	host := ""
	if pageURL != nil {
		host = strings.TrimPrefix(strings.ToLower(pageURL.Hostname()), "www.")
	}

	return Features{
		URL:      pageURL,
		Host:     host,
		BodySize: len(page.Body),
		MediaWiki: bytes.Contains(page.Body, mwContentTextNeedle) ||
			bytes.Contains(page.Body, mediaWikiNeedle) ||
			strings.Contains(host, "wiki"),
	}
}

// Evaluator votes backend weight deltas from page features. Implementations
// must be deterministic, side-effect free, and cheap. Returning nil (or an
// empty map) is neutral: the configured order is preserved untouched.
type Evaluator interface {
	Name() string

	// Vote returns weight deltas keyed by backend name. Votes for backends
	// outside the configured set are ignored by the selector.
	Vote(f Features) map[string]float64
}

// Ordering is the selector's output for one page: the backends in the order
// they should run, the accumulated votes (per configured backend name; never
// nil), and the evaluators that cast at least one counted vote.
type Ordering struct {
	Backends []Backend
	Votes    map[string]float64
	Fired    []string
}

func (o Ordering) names() []string {
	names := make([]string, 0, len(o.Backends))
	for _, b := range o.Backends {
		names = append(names, b.Name())
	}

	return names
}

// selector ranks the configured backends per page by accumulating evaluator
// votes. It is stateless and safe for concurrent use.
type selector struct {
	evaluators []Evaluator
}

func newSelector() *selector {
	return &selector{evaluators: defaultEvaluators()}
}

// order ranks backends by descending total vote. The sort is stable, so
// ties and zero-vote backends keep the configured relative order: with no
// evaluator firing, the returned order equals the input. The input slice is
// never mutated (the pipeline is shared across concurrent requests).
func (s *selector) order(f Features, backends []Backend) Ordering {
	votes := make(map[string]float64, len(backends))
	var fired []string

	if len(backends) >= 2 {
		registered := make(map[string]struct{}, len(backends))
		for _, b := range backends {
			registered[b.Name()] = struct{}{}
		}

		for _, e := range s.evaluators {
			cast := false

			for name, weight := range e.Vote(f) {
				if _, ok := registered[name]; !ok {
					continue
				}

				votes[name] += weight
				cast = true
			}

			if cast {
				fired = append(fired, e.Name())
			}
		}
	}

	order := slices.Clone(backends)
	slices.SortStableFunc(order, func(a, b Backend) int {
		return cmp.Compare(votes[b.Name()], votes[a.Name()])
	})

	return Ordering{Backends: order, Votes: votes, Fired: fired}
}
