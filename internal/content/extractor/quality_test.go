package extractor

import (
	"strings"
	"testing"
)

func TestContentScoreAcceptsShortCleanArticle(t *testing.T) {
	t.Parallel()

	// A legitimately short article must not be rejected because of its
	// length (issue #124 requirement).
	content := Content{
		Title: "Short note",
		Text:  strings.Repeat("This is a real sentence with actual content. ", 3), // ~135 chars
		HTML:  "<p>" + strings.Repeat("This is a real sentence with actual content. ", 3) + "</p>",
	}

	if !acceptable(content) {
		t.Fatalf("expected short clean article to be accepted, score %f", contentScore(content))
	}
}

func TestContentScoreRejectsEmptyContent(t *testing.T) {
	t.Parallel()

	if acceptable(Content{}) {
		t.Fatal("expected empty content to be rejected")
	}
	if acceptable(Content{Title: "Only a title"}) {
		t.Fatal("expected title-only content to be rejected")
	}
}

func TestContentScoreRejectsLinkFarm(t *testing.T) {
	t.Parallel()

	content := linkFarmContent()

	if acceptable(content) {
		t.Fatalf("expected link farm to be rejected, score %f", contentScore(content))
	}
}

func TestContentScoreLinkAllowanceKeepsLinkyButRealArticles(t *testing.T) {
	t.Parallel()

	// ~300 chars of prose with two links: well inside the allowance.
	content := Content{
		Title: "Guide",
		Text: strings.Repeat("A useful sentence that explains the concept in plain words. ", 5) +
			"Another sentence follows, and then the guide concludes with references.",
		HTML: `<p>` + strings.Repeat("A useful sentence that explains the concept in plain words. ", 5) +
			`</p><a href="/a">a</a> <a href="/b">b</a>`,
	}

	if !acceptable(content) {
		t.Fatalf("expected link-light article to be accepted, score %f", contentScore(content))
	}
}

func TestContentScoreLongerCleanBeatsShorterClean(t *testing.T) {
	t.Parallel()

	short := cleanContent()
	long := Content{
		Title: short.Title,
		Text:  short.Text + " " + strings.Repeat("Additional analysis extends the discussion with concrete numbers. ", 10),
		HTML:  short.HTML + "<p>" + strings.Repeat("Additional analysis extends the discussion with concrete numbers. ", 10) + "</p>",
	}

	if contentScore(long) <= contentScore(short) {
		t.Fatalf("expected longer clean article to score higher (%f <= %f)",
			contentScore(long), contentScore(short))
	}
}

func TestContentScoreTitleBonusApplies(t *testing.T) {
	t.Parallel()

	text := strings.Repeat("Meaningful prose that would pass the gate on its own anyway. ", 8)

	withTitle := Content{Title: "T", Text: text, HTML: "<p>" + text + "</p>"}
	without := Content{Text: text, HTML: "<p>" + text + "</p>"}

	if contentScore(withTitle)-contentScore(without) != qualityTitleBonus {
		t.Fatalf("expected title bonus of %f, got delta %f",
			qualityTitleBonus, contentScore(withTitle)-contentScore(without))
	}
}

func TestContentScoreSubLinearInLength(t *testing.T) {
	t.Parallel()

	// Doubling the text length must not double the length term, so length
	// alone cannot dominate the decision.
	mid := Content{Text: strings.Repeat("x", 1000), HTML: "<p>" + strings.Repeat("x", 1000) + "</p>"}
	long := Content{Text: strings.Repeat("x", 2000), HTML: "<p>" + strings.Repeat("x", 2000) + "</p>"}

	growth := contentScore(long) - contentScore(mid)
	total := contentScore(long)

	if growth > total/2 {
		t.Fatalf("expected sub-linear growth, growth %f of total %f", growth, total)
	}
}
