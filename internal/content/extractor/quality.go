package extractor

import (
	"math"
	"strings"
	"unicode/utf8"
)

// Quality scoring decides whether an extraction result is worth accepting or
// whether the pipeline should fall through to the next backend. The score is
// deliberately simple and constant-tuned (not operator-configurable): the
// goal is to reject boilerplate-dominated junk, never to reject legitimately
// short articles — length alone is sub-linear and cannot sink a clean result.
const (
	// qualityAcceptThreshold is the score a result must reach to be accepted
	// immediately. Roughly: a clean 200-character article with a title.
	qualityAcceptThreshold = 6.0

	// qualityTitleBonus rewards results that carry a title: it is a cheap,
	// reliable signal of a real article extraction.
	qualityTitleBonus = 2.0

	// qualityLinkPenaltyPerK is the score removed per link-per-1000-chars
	// above qualityLinkAllowance. Navigation menus and link farms extract as
	// mostly links with little prose; real articles stay far below.
	qualityLinkPenaltyPerK = 4.0

	// qualityLinkAllowance is the links-per-1000-chars allowed before any
	// penalty applies, so link-rich but legitimate short texts are safe.
	qualityLinkAllowance = 5.0
)

// contentScore estimates extraction quality on a 0..∞ scale that penalties
// can push below zero:
//
//	text length:  2·ln(1+runes)         — sub-linear, saturating
//	title:        +2 when present
//	link density: −4 per link-per-1000-chars above 5
func contentScore(c Content) float64 {
	runes := utf8.RuneCountInString(c.Text)
	score := 2 * math.Log1p(float64(runes))

	if c.Title != "" {
		score += qualityTitleBonus
	}

	if c.HTML != "" && runes > 0 {
		hrefs := strings.Count(c.HTML, "href=")
		linksPerK := float64(hrefs) * 1000 / float64(runes)
		score -= qualityLinkPenaltyPerK * math.Max(0, linksPerK-qualityLinkAllowance)
	}

	return score
}

// acceptable reports whether a backend result can be delivered without
// trying further backends.
func acceptable(c Content) bool {
	return c.Text != "" && contentScore(c) >= qualityAcceptThreshold
}
