package adminconfig

import (
	"context"
	"testing"
)

func TestShouldAllowChatDeniesOrdinaryChatsUnderDefaultPolicy(t *testing.T) {
	// Fresh install: allowlist policy with an empty allowlist denies
	// everything except admin DMs. The old implicit "empty = allow everyone"
	// behavior is gone.
	svc := NewService(&stubStore{})

	allowed, err := svc.ShouldAllowChat(context.Background(), 100500, false)
	if err != nil {
		t.Fatalf("should allow chat: %v", err)
	}
	if allowed {
		t.Fatalf("empty allowlist must deny ordinary chats")
	}
}

func TestShouldAllowChatAlwaysAllowsAdminDM(t *testing.T) {
	svc := NewService(&stubStore{})

	allowed, err := svc.ShouldAllowChat(context.Background(), 7, true)
	if err != nil {
		t.Fatalf("should allow chat: %v", err)
	}
	if !allowed {
		t.Fatalf("admin DM must always be allowed")
	}
}

func TestShouldAllowChatAllowlistMembership(t *testing.T) {
	svc := NewService(&stubStore{policy: AccessPolicyAllowlist, whitelistHit: true})

	allowed, err := svc.ShouldAllowChat(context.Background(), 42, false)
	if err != nil {
		t.Fatalf("should allow chat: %v", err)
	}
	if !allowed {
		t.Fatalf("whitelisted chat must be allowed")
	}
}

func TestShouldAllowChatAllowAllPermitsEverything(t *testing.T) {
	svc := NewService(&stubStore{policy: AccessPolicyAllowAll, whitelistHit: false})

	allowed, err := svc.ShouldAllowChat(context.Background(), -100999, false)
	if err != nil {
		t.Fatalf("should allow chat: %v", err)
	}
	if !allowed {
		t.Fatalf("allow_all must permit ordinary chats regardless of membership")
	}
}

func TestAccessPolicyNormalizesUnknownStoredValue(t *testing.T) {
	// A manually corrupted value must fail closed (allowlist), not open.
	svc := NewService(&stubStore{policy: AccessPolicy("bogus")})

	policy, err := svc.AccessPolicy(context.Background())
	if err != nil {
		t.Fatalf("access policy: %v", err)
	}
	if policy != AccessPolicyAllowlist {
		t.Fatalf("unknown policy must normalize to allowlist, got %q", policy)
	}
}

func TestSetAccessPolicyValidatesBeforeStore(t *testing.T) {
	store := &stubStore{policy: AccessPolicyAllowlist}
	svc := NewService(store)

	if err := svc.SetAccessPolicy(context.Background(), AccessPolicy("bogus"), 1); err == nil {
		t.Fatalf("expected invalid policy to be rejected")
	}
	if store.policy != AccessPolicyAllowlist {
		t.Fatalf("rejected write must not change stored policy")
	}

	if err := svc.SetAccessPolicy(context.Background(), AccessPolicyAllowAll, 1); err != nil {
		t.Fatalf("set allow_all: %v", err)
	}
	if store.policy != AccessPolicyAllowAll {
		t.Fatalf("expected stored policy allow_all, got %q", store.policy)
	}
}

func TestSetAllowlistAddAndRemove(t *testing.T) {
	store := &stubStore{whitelist: []int64{5}, whitelistHit: true}
	svc := NewService(store)

	if err := svc.SetAllowlist(context.Background(), 7, true, 1); err != nil {
		t.Fatalf("add allowlist chat: %v", err)
	}
	if got := store.whitelist; len(got) != 2 {
		t.Fatalf("expected two whitelist chats, got %v", got)
	}

	if err := svc.SetAllowlist(context.Background(), 7, false, 1); err != nil {
		t.Fatalf("remove allowlist chat: %v", err)
	}
}

func TestAccessSummaryCountsEntries(t *testing.T) {
	svc := NewService(&stubStore{policy: AccessPolicyAllowAll, whitelist: []int64{1, 2, 3}})

	summary, err := svc.AccessSummary(context.Background())
	if err != nil {
		t.Fatalf("access summary: %v", err)
	}
	if summary.Policy != AccessPolicyAllowAll || summary.AllowlistCount != 3 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
}

func TestAllowlistJoinsCatalogDisplayData(t *testing.T) {
	svc := NewService(&stubStore{
		whitelist: []int64{-100123},
		chats: []ChatCatalogEntry{
			{ChatID: -100123, ChatType: "supergroup", Title: "Group Foo", Username: "groupfoo", DisplayName: "Group Foo"},
			{ChatID: 999, ChatType: "private", DisplayName: "Unrelated"},
		},
	})

	entries, err := svc.Allowlist(context.Background())
	if err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one allowlist entry, got %d", len(entries))
	}
	entry := entries[0]
	if entry.ChatID != -100123 || entry.Title != "Group Foo" || entry.Username != "groupfoo" || entry.ChatType != "supergroup" {
		t.Fatalf("unexpected joined entry: %+v", entry)
	}
}
