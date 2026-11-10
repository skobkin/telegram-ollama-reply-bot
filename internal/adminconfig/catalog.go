package adminconfig

import (
	"context"
	"strconv"
	"strings"
)

// ChatFilter selects a catalog slice by chat type.
type ChatFilter string

const (
	ChatFilterAll     ChatFilter = "all"
	ChatFilterGroups  ChatFilter = "groups"
	ChatFilterPrivate ChatFilter = "private"
)

// DefaultChatPageSize is the catalog page size used by the admin UI.
const DefaultChatPageSize = 8

// ChatQuery describes a catalog query: filtering, case-insensitive search
// over names/usernames or an exact numeric chat ID, and pagination.
type ChatQuery struct {
	Filter     ChatFilter
	Search     string
	Page       int // 1-based; values below 1 clamp to 1
	PerPage    int // values below 1 use DefaultChatPageSize
	ExcludeIDs []int64
}

// ChatPage is one page of catalog results.
type ChatPage struct {
	Entries []ChatCatalogEntry
	Total   int
	Page    int
	Pages   int
}

// QueryChats filters, searches, and paginates the chat catalog. Entries are
// ordered by last-seen (newest first), matching the store's default sort.
func (s *Service) QueryChats(ctx context.Context, query ChatQuery) (ChatPage, error) {
	perPage := query.PerPage
	if perPage <= 0 {
		perPage = DefaultChatPageSize
	}

	excluded := make(map[int64]bool, len(query.ExcludeIDs))
	for _, id := range query.ExcludeIDs {
		excluded[id] = true
	}

	// Stored usernames carry no leading '@' while the search prompt
	// advertises '@username' — strip it so the advertised form matches.
	search := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(query.Search), "@"))
	searchID := int64(0)
	if parsed, err := strconv.ParseInt(strings.TrimSpace(query.Search), 10, 64); err == nil {
		searchID = parsed
	}

	chats, err := s.store.ListChats(ctx)
	if err != nil {
		return ChatPage{}, err
	}

	filtered := make([]ChatCatalogEntry, 0, len(chats))
	for _, chat := range chats {
		if excluded[chat.ChatID] {
			continue
		}
		if !matchesChatFilter(chat, query.Filter) {
			continue
		}
		if search != "" && !matchesChatSearch(chat, search, searchID) {
			continue
		}
		filtered = append(filtered, chat)
	}

	total := len(filtered)
	pages := (total + perPage - 1) / perPage
	if pages == 0 {
		pages = 1
	}
	page := query.Page
	if page < 1 {
		page = 1
	}
	if page > pages {
		page = pages
	}

	start := (page - 1) * perPage
	end := start + perPage
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}

	return ChatPage{
		Entries: append([]ChatCatalogEntry(nil), filtered[start:end]...),
		Total:   total,
		Page:    page,
		Pages:   pages,
	}, nil
}

// RecentChats returns up to n most recently seen catalog entries, skipping
// the excluded IDs (used to hide admin DMs from dashboards).
func (s *Service) RecentChats(ctx context.Context, n int, exclude []int64) ([]ChatCatalogEntry, error) {
	if n <= 0 {
		return nil, nil
	}

	excluded := make(map[int64]bool, len(exclude))
	for _, id := range exclude {
		excluded[id] = true
	}

	chats, err := s.store.ListChats(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]ChatCatalogEntry, 0, n)
	for _, chat := range chats {
		if len(result) >= n {
			break
		}
		if excluded[chat.ChatID] {
			continue
		}
		result = append(result, chat)
	}

	return result, nil
}

// RegisterChat upserts a catalog entry after add-chat flows.
func (s *Service) RegisterChat(ctx context.Context, entry ChatCatalogEntry) error {
	return s.store.UpsertChatCatalog(ctx, entry)
}

func matchesChatFilter(chat ChatCatalogEntry, filter ChatFilter) bool {
	switch filter {
	case ChatFilterGroups:
		return chat.ChatType == "group" || chat.ChatType == "supergroup"
	case ChatFilterPrivate:
		return chat.ChatType == "private"
	default:
		return true
	}
}

func matchesChatSearch(chat ChatCatalogEntry, lowerSearch string, searchID int64) bool {
	if searchID != 0 && chat.ChatID == searchID {
		return true
	}

	return strings.Contains(strings.ToLower(chat.Title), lowerSearch) ||
		strings.Contains(strings.ToLower(chat.Username), lowerSearch) ||
		strings.Contains(strings.ToLower(chat.DisplayName), lowerSearch)
}
