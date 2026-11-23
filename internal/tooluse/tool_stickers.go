package tooluse

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"telegram-ollama-reply-bot/internal/stickers"
)

// StickerCatalog is the read side of the operator-curated sticker catalog:
// the current immutable search index. The tools never see anything the
// administrator did not activate, and never mutate the catalog.
type StickerCatalog interface {
	Snapshot() *stickers.Index
}

// AttachStickerCatalog registers the sticker tools over a catalog. A nil
// catalog is a no-op: the tools stay absent from the registry.
func (r *Runtime) AttachStickerCatalog(catalog StickerCatalog) {
	if catalog == nil {
		return
	}
	r.stickers = catalog

	r.registry.Register(Definition{
		Name:             listStickersToolName,
		Description:      "Search the curated sticker catalog. Sticker descriptions are written mostly in English, so translate the user's intent (whatever language they use) into short English search terms for the query; tags may be in other languages, so a tag in the user's own language can also match. Results are ranked deterministically and carry a stable sticker id, a description, emoji and tags. Only ids returned here exist: never invent one. Send a result with send_sticker.",
		Parameters:       json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","maxLength":200,"description":"Optional free-text query matched against ids, descriptions, tags and emoji. Empty lists the catalog in a stable order."},"tags":{"type":"array","maxItems":8,"items":{"type":"string","minLength":1,"maxLength":32},"description":"Optional tags; every given tag must match."},"limit":{"type":"integer","minimum":1,"maximum":20,"description":"Maximum stickers to return. Defaults to 10."}},"additionalProperties":false}`),
		InvocationPolicy: InvocationPolicyDiscretionary,
		Visibility:       VisibilityStandard,
		ResultCharBudget: listStickersResultCharBudget,
		Handler:          r.listStickers,
		Resolve:          r.stickersPresentation,
	})
	r.registry.Register(Definition{
		Name:             sendStickerToolName,
		Description:      "Send one sticker from the curated catalog into the current conversation, by the id returned from list_stickers. Use it when a sticker suits the moment. The sticker goes out as its own message, before your text reply; it cannot be placed inside the text, and writing or describing a sticker in text (for example \"[Sticker: …]\") sends nothing. A turn gets a single send attempt, and a sticker can be the whole reply (follow it with say_nothing instead of extra text).",
		Parameters:       json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","minLength":3,"maxLength":48,"description":"Sticker id exactly as returned by list_stickers."}},"required":["id"],"additionalProperties":false}`),
		InvocationPolicy: InvocationPolicyDiscretionary,
		Visibility:       VisibilityStandard,
		ResultCharBudget: sendStickerResultCharBudget,
		SideEffecting:    true,
		Handler:          r.sendSticker,
		Resolve:          r.sendStickerPresentation,
	})
}

const listStickersToolName = "list_stickers"

// stickersPresentation hides the sticker tools while no sticker is active, so
// a model is never offered a catalog that cannot serve anything, and while a
// proactive turn is inside the sticker cooldown (browsing would be wasted).
func (r *Runtime) stickersPresentation(call CallContext, p Presentation) (Presentation, bool) {
	if r.stickers == nil || r.stickers.Snapshot().Len() == 0 || r.proactivelyCooling(call) {
		return p, false
	}

	return p, true
}

const (
	stickerFullDescriptionRunes    = 160
	stickerReducedDescriptionRunes = 48
	stickerFullTags                = 8
	stickerFullSetTitleRunes       = 64
)

func stickerArgError(code, message, hint string) toolResult {
	return ErrorResult(NewToolError(code, ErrorArguments, message, true, hint))
}

// stickerListItem is one result row. Presentation fields are optional and
// dropped tier by tier under budget pressure; id is never altered.
type stickerListItem struct {
	ID          string   `json:"id"`
	Emoji       string   `json:"emoji,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Set         string   `json:"set,omitempty"`
}

func (r *Runtime) listStickers(_ context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var payload struct {
		Query string   `json:"query"`
		Tags  []string `json:"tags"`
		Limit *int     `json:"limit"`
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if !decodeToolJSON(args, &payload) {
		return toolResult{}, invalidArguments("Provide an optional query (string), tags (array of strings) and limit (integer).")
	}

	query := strings.TrimSpace(payload.Query)
	if utf8.RuneCountInString(query) > stickers.MaxSearchQueryRunes {
		return stickerArgError("invalid_query", fmt.Sprintf("query must be at most %d characters.", stickers.MaxSearchQueryRunes), "Use a shorter query."), nil
	}
	if len(payload.Tags) > stickers.MaxSearchTags {
		return stickerArgError("invalid_tags", fmt.Sprintf("tags must contain at most %d entries.", stickers.MaxSearchTags), "Use fewer tags."), nil
	}
	tags := make([]string, 0, len(payload.Tags))
	for _, tag := range payload.Tags {
		tag = strings.TrimSpace(tag)
		if tag == "" || utf8.RuneCountInString(tag) > stickers.MaxTagRunes || strings.ContainsFunc(tag, unicode.IsControl) {
			return stickerArgError("invalid_tags", fmt.Sprintf("each tag must be 1 to %d characters without control characters.", stickers.MaxTagRunes), "Use short plain-text tags."), nil
		}
		tags = append(tags, tag)
	}
	limit := stickers.DefaultSearchLimit
	if payload.Limit != nil {
		limit = *payload.Limit
		if limit < 1 || limit > stickers.MaxSearchLimit {
			return stickerArgError("invalid_limit", fmt.Sprintf("limit must be between 1 and %d.", stickers.MaxSearchLimit), "Request between 1 and 20 stickers."), nil
		}
	}

	var index *stickers.Index
	if r.stickers != nil {
		index = r.stickers.Snapshot()
	}
	hits, total := index.Search(query, tags, limit)

	result := fitStickerList(hits, total, listStickersResultCharBudget)
	data := result.Data.(map[string]any)
	logToolResult(callCtx, result, "returned", data["returned"], "total_matches", total)

	return result, nil
}

// fitStickerList builds the list result so it serializes within budget
// WITHOUT any runtime reduction: an oversized annotation must neither fail
// serialization nor push every useful match out. Hits are placed in rank
// order, each at the richest presentation tier that fits (full, reduced,
// id-only), measured with the runtime's own serializer — so JSON escaping and
// the metadata counters are counted exactly. The first match always fits: an
// id is at most 48 ASCII characters. The id itself is never cut.
func fitStickerList(hits []stickers.Hit, total, budget int) toolResult {
	items := make([]stickerListItem, 0, len(hits))
	reduced := 0

	for i, hit := range hits {
		placed := false
		for tier, item := range stickerTiers(hit) {
			candidate := make([]stickerListItem, len(items), len(items)+1)
			copy(candidate, items)
			candidate = append(candidate, item)
			omitted := len(hits) - (i + 1)
			reducedNow := reduced
			if tier > 0 {
				reducedNow++
			}
			if _, preparation := prepareResult(stickerListResult(candidate, total, omitted, reducedNow), budget); preparation == ResultPreparationComplete {
				items, reduced, placed = candidate, reducedNow, true

				break
			}
		}
		if !placed {
			break
		}
	}

	return stickerListResult(items, total, len(hits)-len(items), reduced)
}

func stickerListResult(items []stickerListItem, total, omitted, reduced int) toolResult {
	data := map[string]any{
		"stickers":      items,
		"total_matches": total,
		"returned":      len(items),
	}
	truncated := omitted > 0 || reduced > 0
	if omitted > 0 {
		data["omitted"] = omitted
	}
	if reduced > 0 {
		data["presentation_reduced"] = reduced
	}

	summary := fmt.Sprintf("Found %d matching sticker(s)", total)
	if len(items) < total {
		summary = fmt.Sprintf("Showing %d of %d matching sticker(s)", len(items), total)
	}

	return toolResult{Status: "ok", Summary: summary, Data: data, Truncated: truncated}
}

// stickerTiers returns one hit's presentations, richest first.
func stickerTiers(hit stickers.Hit) []stickerListItem {
	full := stickerListItem{
		ID: hit.ID, Emoji: hit.Emoji,
		Description: ellipsize(hit.Description, stickerFullDescriptionRunes),
		Set:         ellipsize(hit.SetTitle, stickerFullSetTitleRunes),
	}
	for i, tag := range hit.Tags {
		if i == stickerFullTags {
			break
		}
		full.Tags = append(full.Tags, tag)
	}
	reduced := stickerListItem{ID: hit.ID, Emoji: hit.Emoji, Description: ellipsize(hit.Description, stickerReducedDescriptionRunes)}
	minimal := stickerListItem{ID: hit.ID, Emoji: hit.Emoji}

	return []stickerListItem{full, reduced, minimal}
}

// ellipsize shortens s to at most limit runes, marking the cut.
func ellipsize(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)

	return string(runes[:limit-1]) + "…"
}
