package extractor

import (
	"telegram-ollama-reply-bot/internal/config"
)

// Evaluator vote weights and thresholds are code-defined like the quality
// gate constants: each is grounded in the #127 benchmark (per-page F1 data
// in its comment 8184) and the #132 live probe, not operator-tuned.
const (
	// voteMediaWikiReadability promotes readability on MediaWiki pages: the
	// #127 benchmark measured the best F1 of the three backends there
	// (0.86/0.95 vs 0.79/0.92 for trafilatura).
	voteMediaWikiReadability = 1.0

	// voteMediaWikiDomDistiller demotes domdistiller on MediaWiki pages: it
	// drops reference-heavy sections and tables there (F1 0.62-0.72 in
	// #127, the weakest of the three).
	voteMediaWikiDomDistiller = -1.0

	// voteLongPageDomDistiller promotes domdistiller on very large pages: in
	// the #127 sample it was the only backend recovering long news articles
	// without truncation or junk (~91% recall), while readability truncated
	// to 45% and passed the gate before domdistiller ever ran (#131).
	voteLongPageDomDistiller = 1.0

	// voteLongPageTrafilatura demotes trafilatura on very large pages: worst
	// long-page recall in the #127 sample (13%) and the slowest backend
	// there (58.7 ms vs 27.6 ms for domdistiller on the 142 KB fixture).
	voteLongPageTrafilatura = -1.0

	// longPageBodyThresholdBytes is the fetched-body size from which a page
	// counts as "very large" for the long-page evaluator. 128 KiB sits below
	// the checked-in article_large.html fixture (142,267 bytes, fires) and
	// above text-rich long essays (~80 KB, does not fire). Body size is a
	// blunt proxy: markup-bloated short pages fire it too, and the only
	// consequence is a different — still gate-checked — backend winning.
	// Pages just below the cutoff keep the configured order, so the #131
	// truncation lock-in persists for long pages under 128 KiB; fixing that
	// needs a truncation signal on the extraction result itself, not a lower
	// byte cutoff.
	longPageBodyThresholdBytes = 128 << 10
)

// mediaWikiEvaluator favors readability on MediaWiki pages. On the default
// backend order readability already leads, so the up-vote mostly guards
// operator-reordered configurations; the domdistiller demotion keeps its
// weak wiki recall from winning when it is configured first. Host-based
// detection (a "wiki" substring) can false-positive on non-MediaWiki hosts;
// that only re-ranks the chain, and the quality gate still applies.
//
// Interaction with the long-page evaluator: on a very large page at a
// wiki-substring host that is not actually MediaWiki, the demotion cancels
// domdistiller's long-page promotion (net 0), so readability — with its
// truncation-prone long-page recall — outranks it. For a domdistiller-first
// configuration that recreates the #131 truncation lock-in on such hosts.
// Accepted: the demotion is benchmark-grounded for actual wiki pages, and
// the winning extraction is still gate-checked with rescue mode behind it.
type mediaWikiEvaluator struct{}

func (mediaWikiEvaluator) Name() string {
	return "mediawiki"
}

func (mediaWikiEvaluator) Vote(f Features) map[string]float64 {
	if !f.MediaWiki {
		return nil
	}

	return map[string]float64{
		config.ExtractionBackendReadability:  voteMediaWikiReadability,
		config.ExtractionBackendDomDistiller: voteMediaWikiDomDistiller,
	}
}

// longPageEvaluator favors domdistiller on very large pages, where the
// first-acceptable-wins order otherwise locks in a readability truncation
// that passes the quality gate (#131). Readability is deliberately not
// down-voted: if domdistiller then fails or returns empty, readability
// still runs next and can rescue the page.
type longPageEvaluator struct{}

func (longPageEvaluator) Name() string {
	return "long-page"
}

func (longPageEvaluator) Vote(f Features) map[string]float64 {
	if f.BodySize < longPageBodyThresholdBytes {
		return nil
	}

	return map[string]float64{
		config.ExtractionBackendDomDistiller: voteLongPageDomDistiller,
		config.ExtractionBackendTrafilatura:  voteLongPageTrafilatura,
	}
}

func defaultEvaluators() []Evaluator {
	return []Evaluator{
		mediaWikiEvaluator{},
		longPageEvaluator{},
	}
}
