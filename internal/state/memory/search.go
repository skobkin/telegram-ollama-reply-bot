package memory

import (
	"cmp"
	"slices"
	"strings"
	"unicode"

	"telegram-ollama-reply-bot/internal/state"

	"golang.org/x/text/unicode/norm"
)

type conversationSearchIndex struct {
	tokenToID map[string]int
	tokens    []string
	postings  map[int][]int
	messages  []searchMessageEntry
	bytes     int64
}

type searchMessageEntry struct {
	tokenIDs []int
	bytes    int64
}

type matchedKeyword struct {
	keyword string
	exact   bool
}

type searchCandidate struct {
	position int
	matches  map[string]matchedKeyword
}

func buildSearchIndex(messages []state.Message) *conversationSearchIndex {
	index := &conversationSearchIndex{
		tokenToID: make(map[string]int),
		tokens:    make([]string, 0),
		postings:  make(map[int][]int),
		messages:  make([]searchMessageEntry, 0, len(messages)),
	}

	for position, msg := range messages {
		index.appendMessage(position, msg)
	}

	return index
}

func (i *conversationSearchIndex) appendMessage(position int, msg state.Message) {
	if i == nil {
		return
	}

	tokenIDs := make([]int, 0)
	seen := make(map[int]struct{})
	for _, token := range normalizeSearchTokens(renderSearchMessageText(msg)) {
		tokenID := i.ensureTokenID(token)
		if _, ok := seen[tokenID]; ok {
			continue
		}
		seen[tokenID] = struct{}{}
		tokenIDs = append(tokenIDs, tokenID)
		i.postings[tokenID] = append(i.postings[tokenID], position)
		i.bytes += 8
	}

	entryBytes := int64(16 + len(tokenIDs)*8)
	i.messages = append(i.messages, searchMessageEntry{
		tokenIDs: tokenIDs,
		bytes:    entryBytes,
	})
	i.bytes += entryBytes
}

func (i *conversationSearchIndex) ensureTokenID(token string) int {
	if tokenID, ok := i.tokenToID[token]; ok {
		return tokenID
	}

	tokenID := len(i.tokens)
	i.tokenToID[token] = tokenID
	i.tokens = append(i.tokens, token)
	i.bytes += int64(len(token) + 48)

	return tokenID
}

func (s *ConversationStore) Search(scope state.ConversationScope, query state.HistorySearchQuery) []state.HistorySearchMatch {
	s.mu.RLock()
	defer s.mu.RUnlock()

	bucket := s.streams[scopeKey(scope)]
	if bucket == nil || len(bucket.messages) == 0 || bucket.searchIndex == nil {
		return nil
	}

	keywords := normalizeKeywords(query.Keywords)
	if len(keywords) == 0 {
		return nil
	}

	matchMode := query.MatchMode
	if matchMode != state.HistorySearchMatchModeAny {
		matchMode = state.HistorySearchMatchModeAll
	}

	candidates := make(map[int]*searchCandidate)
	for _, keyword := range keywords {
		matches := bucket.searchIndex.lookupKeyword(keyword)
		if len(matches) == 0 {
			if matchMode == state.HistorySearchMatchModeAll {
				return nil
			}

			continue
		}

		for _, match := range matches {
			candidate := candidates[match.position]
			if candidate == nil {
				candidate = &searchCandidate{
					position: match.position,
					matches:  make(map[string]matchedKeyword),
				}
				candidates[match.position] = candidate
			}

			existing, ok := candidate.matches[keyword]
			if !ok || (!existing.exact && match.exact) {
				candidate.matches[keyword] = matchedKeyword{
					keyword: match.keyword,
					exact:   match.exact,
				}
			}
		}
	}

	results := make([]state.HistorySearchMatch, 0, len(candidates))
	for _, candidate := range candidates {
		if matchMode == state.HistorySearchMatchModeAll && len(candidate.matches) != len(keywords) {
			continue
		}

		result := searchMatchFromCandidate(bucket.messages[candidate.position], keywords, candidate)
		results = append(results, result)
	}

	slices.SortFunc(results, func(left, right state.HistorySearchMatch) int {
		if left.Score != right.Score {
			return cmp.Compare(right.Score, left.Score)
		}
		if !left.Message.CreatedAt.Equal(right.Message.CreatedAt) {
			if left.Message.CreatedAt.Before(right.Message.CreatedAt) {
				return 1
			}

			return -1
		}

		return cmp.Compare(right.Message.MessageID, left.Message.MessageID)
	})

	limit := query.Limit
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}

	return results
}

func searchMatchFromCandidate(msg state.Message, keywords []string, candidate *searchCandidate) state.HistorySearchMatch {
	matchedKeywords := make([]string, 0, len(candidate.matches))
	exactCount := 0
	for _, keyword := range keywords {
		match, ok := candidate.matches[keyword]
		if !ok {
			continue
		}
		matchedKeywords = append(matchedKeywords, match.keyword)
		if match.exact {
			exactCount++
		}
	}

	matchKind := "mixed"
	switch {
	case len(matchedKeywords) == 0:
		matchKind = "fuzzy"
	case exactCount == len(matchedKeywords):
		matchKind = "exact"
	case exactCount == 0:
		matchKind = "fuzzy"
	}

	fuzzyCount := len(matchedKeywords) - exactCount

	return state.HistorySearchMatch{
		Message:         cloneMessage(msg),
		Score:           len(matchedKeywords)*100 + exactCount*20 + fuzzyCount*5,
		MatchKind:       matchKind,
		MatchedKeywords: matchedKeywords,
	}
}

func (i *conversationSearchIndex) lookupKeyword(keyword string) []matchedKeywordPosition {
	if i == nil || keyword == "" {
		return nil
	}

	result := make([]matchedKeywordPosition, 0)
	if tokenID, ok := i.tokenToID[keyword]; ok {
		result = append(result, positionsForToken(keyword, true, i.postings[tokenID])...)
	}

	maxDistance := keywordMaxDistance(keyword)
	if maxDistance <= 0 {
		return result
	}

	for tokenID, token := range i.tokens {
		if token == keyword {
			continue
		}
		if !isFuzzyMatch(keyword, token, maxDistance) {
			continue
		}
		result = append(result, positionsForToken(keyword, false, i.postings[tokenID])...)
	}

	return result
}

type matchedKeywordPosition struct {
	position int
	keyword  string
	exact    bool
}

func positionsForToken(keyword string, exact bool, positions []int) []matchedKeywordPosition {
	result := make([]matchedKeywordPosition, 0, len(positions))
	for _, position := range positions {
		result = append(result, matchedKeywordPosition{
			position: position,
			keyword:  keyword,
			exact:    exact,
		})
	}

	return result
}

func normalizeKeywords(keywords []string) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0, len(keywords))
	for _, keyword := range keywords {
		for _, token := range normalizeSearchTokens(keyword) {
			if _, ok := seen[token]; ok {
				continue
			}
			seen[token] = struct{}{}
			result = append(result, token)
		}
	}

	return result
}

func normalizeSearchTokens(text string) []string {
	normalized := strings.ToLower(norm.NFKC.String(text))
	if normalized == "" {
		return nil
	}

	result := make([]string, 0)
	var current []rune
	flush := func() {
		if len(current) == 0 {
			return
		}
		result = append(result, string(current))
		current = current[:0]
	}

	for _, r := range normalized {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current = append(current, r)

			continue
		}
		flush()
	}
	flush()

	return result
}

func renderSearchMessageText(message state.Message) string {
	var sb strings.Builder

	if message.ReplyTo != nil {
		sb.WriteString("> ")
		sb.WriteString(renderSearchMessageText(*message.ReplyTo))
		sb.WriteString("\n")
	}

	sb.WriteString(message.Name)
	if message.Username != "" {
		sb.WriteString(" (@")
		sb.WriteString(message.Username)
		sb.WriteString(")")
	}
	sb.WriteString(": ")

	if message.HasImage {
		if message.Image != "" {
			sb.WriteString("[Image: ")
			sb.WriteString(message.Image)
			sb.WriteString("] ")
		} else {
			sb.WriteString("[Image] ")
		}
	}

	sb.WriteString(message.Text)

	return sb.String()
}

func keywordMaxDistance(keyword string) int {
	length := len([]rune(keyword))
	switch {
	case length < 4:
		return 0
	case length < 8:
		return 1
	default:
		return 2
	}
}

func isFuzzyMatch(left, right string, maxDistance int) bool {
	if maxDistance <= 0 {
		return left == right
	}

	leftRunes := []rune(left)
	rightRunes := []rune(right)
	diff := len(leftRunes) - len(rightRunes)
	if diff < 0 {
		diff = -diff
	}
	if diff > maxDistance {
		return false
	}

	previous := make([]int, len(rightRunes)+1)
	for j := range previous {
		previous[j] = j
	}

	for i := 1; i <= len(leftRunes); i++ {
		current := make([]int, len(rightRunes)+1)
		current[0] = i
		rowMin := current[0]
		for j := 1; j <= len(rightRunes); j++ {
			cost := 0
			if leftRunes[i-1] != rightRunes[j-1] {
				cost = 1
			}

			insertCost := current[j-1] + 1
			deleteCost := previous[j] + 1
			replaceCost := previous[j-1] + cost
			current[j] = minInt(insertCost, deleteCost, replaceCost)
			if current[j] < rowMin {
				rowMin = current[j]
			}
		}
		if rowMin > maxDistance {
			return false
		}
		previous = current
	}

	return previous[len(rightRunes)] <= maxDistance
}

func minInt(values ...int) int {
	best := values[0]
	for _, value := range values[1:] {
		if value < best {
			best = value
		}
	}

	return best
}
