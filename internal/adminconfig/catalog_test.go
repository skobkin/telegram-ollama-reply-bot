package adminconfig

import (
	"context"
	"testing"
	"time"
)

func catalogFixture() []ChatCatalogEntry {
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	return []ChatCatalogEntry{
		{ChatID: -1001, ChatType: "supergroup", Title: "Alpha Group", Username: "alphagroup", DisplayName: "Alpha Group", LastSeenAt: base.Add(-1 * time.Hour)},
		{ChatID: -1002, ChatType: "group", Title: "beta chat", DisplayName: "beta chat", LastSeenAt: base.Add(-2 * time.Hour)},
		{ChatID: 500, ChatType: "private", Title: "", Username: "gamma_user", DisplayName: "Gamma", LastSeenAt: base.Add(-3 * time.Hour)},
		{ChatID: 600, ChatType: "private", Title: "", DisplayName: "Delta User", LastSeenAt: base.Add(-4 * time.Hour)},
	}
}

func TestQueryChatsFiltersByType(t *testing.T) {
	svc := NewService(&stubStore{chats: catalogFixture()}, nil, nil)
	ctx := context.Background()

	page, err := svc.QueryChats(ctx, ChatQuery{Filter: ChatFilterGroups})
	if err != nil {
		t.Fatalf("query chats: %v", err)
	}
	if page.Total != 2 || page.Entries[0].ChatID != -1001 {
		t.Fatalf("expected both groups newest-first, got %+v", page.Entries)
	}

	page, err = svc.QueryChats(ctx, ChatQuery{Filter: ChatFilterPrivate})
	if err != nil {
		t.Fatalf("query chats: %v", err)
	}
	if page.Total != 2 || page.Entries[0].ChatID != 500 {
		t.Fatalf("expected both private chats, got %+v", page.Entries)
	}

	page, err = svc.QueryChats(ctx, ChatQuery{Filter: ChatFilterAll})
	if err != nil {
		t.Fatalf("query chats: %v", err)
	}
	if page.Total != 4 {
		t.Fatalf("expected all four chats, got %d", page.Total)
	}
}

func TestQueryChatsSearch(t *testing.T) {
	svc := NewService(&stubStore{chats: catalogFixture()}, nil, nil)
	ctx := context.Background()

	// Case-insensitive title search.
	page, err := svc.QueryChats(ctx, ChatQuery{Search: "ALPHA"})
	if err != nil || page.Total != 1 || page.Entries[0].ChatID != -1001 {
		t.Fatalf("title search failed: %+v err %v", page, err)
	}

	// Username search.
	page, err = svc.QueryChats(ctx, ChatQuery{Search: "gamma"})
	if err != nil || page.Total != 1 || page.Entries[0].ChatID != 500 {
		t.Fatalf("username search failed: %+v err %v", page, err)
	}

	// The @username form the search prompt advertises must match the stored
	// bare username.
	page, err = svc.QueryChats(ctx, ChatQuery{Search: "@Gamma"})
	if err != nil || page.Total != 1 || page.Entries[0].ChatID != 500 {
		t.Fatalf("@username search failed: %+v err %v", page, err)
	}

	// Exact numeric ID match.
	page, err = svc.QueryChats(ctx, ChatQuery{Search: "-1002"})
	if err != nil || page.Total != 1 || page.Entries[0].ChatID != -1002 {
		t.Fatalf("numeric id search failed: %+v err %v", page, err)
	}

	// No match.
	page, err = svc.QueryChats(ctx, ChatQuery{Search: "zzz"})
	if err != nil || page.Total != 0 {
		t.Fatalf("expected empty result, got %+v err %v", page, err)
	}
}

func TestQueryChatsPagination(t *testing.T) {
	svc := NewService(&stubStore{chats: catalogFixture()}, nil, nil)
	ctx := context.Background()

	// Two per page: newest first, then older.
	page, err := svc.QueryChats(ctx, ChatQuery{PerPage: 2, Page: 1})
	if err != nil {
		t.Fatalf("query chats: %v", err)
	}
	if page.Total != 4 || page.Pages != 2 || len(page.Entries) != 2 || page.Entries[0].ChatID != -1001 {
		t.Fatalf("unexpected first page: %+v", page)
	}

	page, err = svc.QueryChats(ctx, ChatQuery{PerPage: 2, Page: 2})
	if err != nil {
		t.Fatalf("query chats: %v", err)
	}
	if len(page.Entries) != 2 || page.Entries[0].ChatID != 500 {
		t.Fatalf("unexpected second page: %+v", page)
	}

	// Out-of-range page clamps to the last page.
	page, err = svc.QueryChats(ctx, ChatQuery{PerPage: 2, Page: 99})
	if err != nil {
		t.Fatalf("query chats: %v", err)
	}
	if page.Page != 2 {
		t.Fatalf("expected clamp to last page, got %d", page.Page)
	}

	// Empty catalog still reports one page and page 1.
	empty := NewService(&stubStore{}, nil, nil)
	page, err = empty.QueryChats(ctx, ChatQuery{})
	if err != nil || page.Page != 1 || page.Pages != 1 || len(page.Entries) != 0 {
		t.Fatalf("unexpected empty result: %+v err %v", page, err)
	}
}

func TestQueryChatsExcludesIDs(t *testing.T) {
	svc := NewService(&stubStore{chats: catalogFixture()}, nil, nil)

	page, err := svc.QueryChats(context.Background(), ChatQuery{ExcludeIDs: []int64{-1001, 500}})
	if err != nil {
		t.Fatalf("query chats: %v", err)
	}
	if page.Total != 2 {
		t.Fatalf("expected excluded ids removed, got %d", page.Total)
	}
	for _, entry := range page.Entries {
		if entry.ChatID == -1001 || entry.ChatID == 500 {
			t.Fatalf("excluded chat leaked: %+v", entry)
		}
	}
}

func TestRecentChatsLimitAndExclude(t *testing.T) {
	svc := NewService(&stubStore{chats: catalogFixture()}, nil, nil)

	recent, err := svc.RecentChats(context.Background(), 2, []int64{-1001})
	if err != nil {
		t.Fatalf("recent chats: %v", err)
	}
	if len(recent) != 2 || recent[0].ChatID != -1002 || recent[1].ChatID != 500 {
		t.Fatalf("expected newest two skipping excluded, got %+v", recent)
	}

	if recent, err = svc.RecentChats(context.Background(), 0, nil); err != nil || recent != nil {
		t.Fatalf("zero limit must return nothing, got %+v err %v", recent, err)
	}
}

func TestRegisterChatUpserts(t *testing.T) {
	store := &stubStore{}
	svc := NewService(store, nil, nil)

	entry := ChatCatalogEntry{ChatID: 42, ChatType: "private", DisplayName: "New Chat"}
	if err := svc.RegisterChat(context.Background(), entry); err != nil {
		t.Fatalf("register chat: %v", err)
	}
	if len(store.chats) != 1 || store.chats[0].ChatID != 42 {
		t.Fatalf("expected upserted entry, got %+v", store.chats)
	}
}
