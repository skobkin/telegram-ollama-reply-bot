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

	// qualityLinkPenaltyPerK is the score removed per unit of
	// sqrt(links-per-1000-chars above qualityLinkAllowance). The square root
	// saturates the penalty: navigation menus and link farms extract at tens
	// to thousands of links-per-1000-chars and still lose decisively, while
	// reference-heavy articles keep their length score (#132). The saturating
	// form is strictly weaker than the old linear penalty at every density:
	// text-heavy boilerplate in the ~6–20 links/k band now passes the gate
	// too, and at the top of the reference range (24 links/k) only a very
	// long extraction (~36k+ runes with a title) still clears it.
	qualityLinkPenaltyPerK = 4.0

	// qualityLinkAllowance is the links-per-1000-chars allowed before any
	// penalty applies, so link-rich but legitimate short texts are safe.
	// Plain prose stays well below it; reference-heavy long-form pages
	// legitimately extract above it, which the saturating penalty tolerates.
	qualityLinkAllowance = 6.0
)

// contentScore estimates extraction quality on a 0..∞ scale that penalties
// can push below zero:
//
//	text length:  2·ln(1+runes)         — sub-linear, saturating
//	title:        +2 when present
//	link density: −4·√(links-per-1000-chars above 6)
//
// The link-density term needs hrefs to count, so a result whose backend
// could not render HTML (structure is optional in every backend) is gated
// on length and title alone; this only loosens the gate on the
// render-failure path and is accepted.
//
// The link penalty grows with the square root of the density excess instead
// of linearly (#132): a linear penalty crushes the logarithmic length term on
// link-dense reference sites — every clean backend extracted Wikipedia
// articles at 12–24 links/k and failed the gate, falling to rescue mode. The
// square-root penalty keeps the probed pages accepted (measured: all 15 probe
// extractions pass with margin) while boilerplate-dominated junk at densities
// an order of magnitude higher is still rejected — by narrower absolute
// margins than under the linear penalty.
func contentScore(c Content) float64 {
	runes := utf8.RuneCountInString(c.Text)
	score := 2 * math.Log1p(float64(runes))

	if c.Title != "" {
		score += qualityTitleBonus
	}

	if c.HTML != "" && runes > 0 {
		hrefs := strings.Count(c.HTML, "href=")
		linksPerK := float64(hrefs) * 1000 / float64(runes)
		score -= qualityLinkPenaltyPerK * math.Sqrt(math.Max(0, linksPerK-qualityLinkAllowance))
	}

	return score
}

// acceptable reports whether a backend result can be delivered without
// trying further backends. score must be contentScore(c); it is passed in
// separately so the pipeline can reuse the score it already computed for
// logging and the rescue comparison instead of rescanning Text and HTML.
func acceptable(c Content, score float64) bool {
	return c.Text != "" && score >= qualityAcceptThreshold
}
