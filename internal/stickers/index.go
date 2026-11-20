package stickers

import (
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
)

// Hit is one search result. It carries presentation data only: never a
// file_id or file_unique_id.
type Hit struct {
	ID          string
	Description string
	Emoji       string
	Tags        []string
	SetTitle    string
}

// Index is an immutable, bounded search/send index over the ACTIVE entries
// of a catalog State. Readers hold a snapshot; updates build a new Index and
// swap it in atomically, so a reader never sees a partial import.
type Index struct {
	items []indexed
	byID  map[string]int
}

type indexed struct {
	entry     Entry
	setTitle  string
	idTokens  map[string]struct{}
	descWords map[string]struct{}
	tagsFold  map[string]struct{}
	emoji     string
}

// BuildIndex indexes the active entries of s (enabled set, available,
// enabled, with an id and a description) in a stable order: set name,
// position, id.
func BuildIndex(s State) *Index {
	fold := cases.Fold()
	titles := make(map[string]string, len(s.Sets))
	enabled := make(map[string]bool, len(s.Sets))
	for _, set := range s.Sets {
		titles[set.Name], enabled[set.Name] = set.Title, set.Enabled
	}

	idx := &Index{byID: map[string]int{}}
	for _, e := range s.Entries {
		if !e.Active(enabled[e.SetName]) {
			continue
		}
		item := indexed{
			entry:     e,
			setTitle:  titles[e.SetName],
			idTokens:  tokenSet(fold.String(e.SemanticID)),
			descWords: tokenSet(fold.String(e.Description)),
			tagsFold:  make(map[string]struct{}, len(e.Tags)),
			emoji:     normalizeEmoji(e.Emoji),
		}
		for _, tag := range e.Tags {
			item.tagsFold[fold.String(tag)] = struct{}{}
		}
		idx.items = append(idx.items, item)
	}
	sort.SliceStable(idx.items, func(i, j int) bool {
		a, b := idx.items[i].entry, idx.items[j].entry
		if a.SetName != b.SetName {
			return a.SetName < b.SetName
		}
		if a.Position != b.Position {
			return a.Position < b.Position
		}

		return a.SemanticID < b.SemanticID
	})
	for i, item := range idx.items {
		idx.byID[item.entry.SemanticID] = i
	}

	return idx
}

// Len reports the number of active entries. A nil Index is empty.
func (x *Index) Len() int {
	if x == nil {
		return 0
	}

	return len(x.items)
}

// Resolve returns the active entry with the given semantic id.
func (x *Index) Resolve(id string) (Entry, bool) {
	if x == nil {
		return Entry{}, false
	}
	i, ok := x.byID[id]
	if !ok {
		return Entry{}, false
	}

	return x.items[i].entry, true
}

// Ranking policy (deterministic, no external service). Query tokens are the
// case-folded letter/digit runs of the query; an entry scores per token:
//
//	exact id (whole query)  100
//	exact tag               10
//	emoji equals a field    8   (whitespace-separated query fields)
//	id token                6
//	description word        3
//
// With a non-empty query only entries scoring > 0 match. The tags filter is a
// hard AND: every filter tag must equal one of the entry's tags
// (case-insensitive). Ties break by set name, position, then id. An empty
// query with no tags matches everything, in that stable order.
const (
	scoreExactID = 100
	scoreTag     = 10
	scoreEmoji   = 8
	scoreIDToken = 6
	scoreWord    = 3
)

// Search returns up to limit hits and the total number of matches. limit is
// clamped to [1, MaxSearchLimit].
func (x *Index) Search(query string, tags []string, limit int) ([]Hit, int) {
	if x == nil || len(x.items) == 0 {
		return nil, 0
	}
	if limit < 1 {
		limit = DefaultSearchLimit
	}
	if limit > MaxSearchLimit {
		limit = MaxSearchLimit
	}

	fold := cases.Fold()
	foldedQuery := strings.TrimSpace(fold.String(query))
	tokens := uniqueTokens(foldedQuery)
	fields := strings.Fields(query)
	filter := make([]string, 0, len(tags))
	for _, tag := range tags {
		if t := strings.TrimSpace(fold.String(tag)); t != "" {
			filter = append(filter, t)
		}
	}
	hasQuery := foldedQuery != ""

	type scored struct {
		pos, score int
	}
	var matches []scored
	for i, item := range x.items {
		if !hasAllTags(item, filter) {
			continue
		}
		score := 0
		if hasQuery {
			score = item.score(foldedQuery, tokens, fields)
			if score == 0 {
				continue
			}
		}
		matches = append(matches, scored{i, score})
	}
	// items are already in stable order, so a stable sort by score keeps
	// the set/position/id tie-break.
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].score > matches[j].score })

	total := len(matches)
	if len(matches) > limit {
		matches = matches[:limit]
	}
	hits := make([]Hit, 0, len(matches))
	for _, m := range matches {
		item := x.items[m.pos]
		hits = append(hits, Hit{
			ID: item.entry.SemanticID, Description: item.entry.Description, Emoji: item.entry.Emoji,
			Tags: append([]string(nil), item.entry.Tags...), SetTitle: item.setTitle,
		})
	}

	return hits, total
}

func (it indexed) score(foldedQuery string, tokens, fields []string) int {
	score := 0
	if foldedQuery == strings.ToLower(it.entry.SemanticID) {
		score += scoreExactID
	}
	for _, token := range tokens {
		if _, ok := it.tagsFold[token]; ok {
			score += scoreTag
		}
		if _, ok := it.idTokens[token]; ok {
			score += scoreIDToken
		}
		if _, ok := it.descWords[token]; ok {
			score += scoreWord
		}
	}
	if it.emoji != "" {
		for _, field := range fields {
			if normalizeEmoji(field) == it.emoji {
				score += scoreEmoji

				break
			}
		}
	}

	return score
}

func hasAllTags(item indexed, filter []string) bool {
	for _, tag := range filter {
		if _, ok := item.tagsFold[tag]; !ok {
			return false
		}
	}

	return true
}

func tokenList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func uniqueTokens(s string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, token := range tokenList(s) {
		if _, dup := seen[token]; !dup {
			seen[token] = struct{}{}
			out = append(out, token)
		}
	}

	return out
}

func tokenSet(s string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, token := range tokenList(s) {
		out[token] = struct{}{}
	}

	return out
}

// normalizeEmoji drops variation selectors so "❤" and "❤️" compare equal.
func normalizeEmoji(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '️' || r == '︎' {
			return -1
		}

		return r
	}, strings.TrimSpace(s))
}
